// User bootstrap tests (plan step 1.3c). The in-memory store tests cover
// the idempotency contract and claim fallbacks without any infrastructure;
// the postgres:18 testcontainer test (run via `make backend-test`, which
// mounts the host docker.sock — Makefile DOCKER_GO_TC, D-019) proves the
// real pgUserStore: repeat logins yield exactly one row with one wrapped
// DEK, and a concurrent first-login race yields the same.
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"finance-tracker/internal/authn"
	"finance-tracker/internal/crypto"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jws"
	"github.com/lestrrat-go/jwx/v3/jwt"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// memStore is an in-memory UserStore implementing the contract: first
// login wins, repeat logins return the stored row.
type memStore struct {
	mu   sync.Mutex
	rows map[string]UserRow
}

func newMemStore() *memStore { return &memStore{rows: map[string]UserRow{}} }

func (m *memStore) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rows)
}

func (m *memStore) get(sub string) (UserRow, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[sub]
	return r, ok
}

func (m *memStore) UpsertUser(_ context.Context, sub, email, displayName string, wrappedDEK []byte) (UserRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if row, ok := m.rows[sub]; ok {
		return row, nil // stored row wins
	}
	row := UserRow{ID: "id-" + sub, Sub: sub, Email: email, DisplayName: displayName, WrappedDEK: wrappedDEK}
	m.rows[sub] = row
	return row, nil
}

func testKEK(t *testing.T) []byte {
	t.Helper()
	kek := make([]byte, crypto.KEKSize)
	if _, err := rand.Read(kek); err != nil {
		t.Fatalf("KEK random: %v", err)
	}
	return kek
}

func mintKey(t *testing.T) jwk.Key {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	key, err := jwk.Import(priv)
	if err != nil {
		t.Fatalf("import key: %v", err)
	}
	return key
}

func liveAuth(t *testing.T, key jwk.Key) *authn.Verifier {
	t.Helper()
	set := jwk.NewSet()
	if err := set.AddKey(key); err != nil {
		t.Fatalf("add key: %v", err)
	}
	v, err := authn.NewWithKeyFunc(
		authn.Config{Issuer: "http://issuer.test", Audience: "test-aud"},
		func(context.Context) (any, error) { return set, nil },
	)
	if err != nil {
		t.Fatalf("build verifier: %v", err)
	}
	return v
}

// mintIdentityToken builds an RS256-signed JWT with the standard claims
// plus the OIDC profile claims; empty arguments omit the claim entirely.
func mintIdentityToken(t *testing.T, key jwk.Key, sub, email, name, username string) string {
	t.Helper()
	now := time.Now()
	b := jwt.NewBuilder().
		Issuer("http://issuer.test").
		Subject(sub).
		Audience([]string{"test-aud"}).
		IssuedAt(now).
		Expiration(now.Add(15 * time.Minute))
	if email != "" {
		b = b.Claim("email", email)
	}
	if name != "" {
		b = b.Claim("name", name)
	}
	if username != "" {
		b = b.Claim("preferred_username", username)
	}
	tok, err := b.Build()
	if err != nil {
		t.Fatalf("build token: %v", err)
	}
	payload, err := jwt.NewSerializer().Serialize(tok)
	if err != nil {
		t.Fatalf("serialize token: %v", err)
	}
	signed, err := jws.Sign(payload, jws.WithKey(jwa.RS256(), key))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return string(signed)
}

func TestNewBootstrapFailFast(t *testing.T) {
	if _, err := NewBootstrap(nil, testKEK(t)); err == nil || err.Error() != "server: user store is required" {
		t.Fatalf("nil store: want %q, got %v", "server: user store is required", err)
	}
	short := make([]byte, 16)
	if _, err := NewBootstrap(newMemStore(), short); err == nil {
		t.Fatal("short KEK: want error, got nil")
	}
}

func TestEnsureUserCreatesWrappedDEK(t *testing.T) {
	kek := testKEK(t)
	store := newMemStore()
	b, err := NewBootstrap(store, kek)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	row, err := b.EnsureUser(context.Background(), authn.Identity{
		Sub: "user-1", Email: "a@localhost", Name: "A", Username: "a",
	})
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if row.ID == "" || row.Sub != "user-1" || row.Email != "a@localhost" || row.DisplayName != "A" {
		t.Fatalf("unexpected row: %+v", row)
	}
	if len(row.WrappedDEK) < crypto.PackMinSize {
		t.Fatalf("wrapped DEK too short: %d bytes", len(row.WrappedDEK))
	}
	dek, err := crypto.UnwrapDEK(kek, row.WrappedDEK)
	if err != nil {
		t.Fatalf("unwrap: %v", err)
	}
	if len(dek) != crypto.DEKSize {
		t.Fatalf("DEK size: want %d, got %d", crypto.DEKSize, len(dek))
	}
	if store.count() != 1 {
		t.Fatalf("store rows: want 1, got %d", store.count())
	}
}

func TestEnsureUserRepeatLoginKeepsFirstRow(t *testing.T) {
	kek := testKEK(t)
	store := newMemStore()
	b, err := NewBootstrap(store, kek)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	first, err := b.EnsureUser(context.Background(), authn.Identity{
		Sub: "user-1", Email: "a@localhost", Name: "A", Username: "a",
	})
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	// Repeat login with CHANGED profile claims: the stored row must win.
	again, err := b.EnsureUser(context.Background(), authn.Identity{
		Sub: "user-1", Email: "new@localhost", Name: "Changed", Username: "z",
	})
	if err != nil {
		t.Fatalf("repeat ensure: %v", err)
	}
	if store.count() != 1 {
		t.Fatalf("rows: want 1, got %d", store.count())
	}
	if again.ID != first.ID {
		t.Fatalf("ID changed: %q -> %q", first.ID, again.ID)
	}
	if !bytes.Equal(again.WrappedDEK, first.WrappedDEK) {
		t.Fatal("wrapped DEK changed on repeat login")
	}
	if again.Email != "a@localhost" || again.DisplayName != "A" {
		t.Fatalf("stored claims overwritten: %+v", again)
	}
}

func TestEnsureUserClaimFallbacks(t *testing.T) {
	kek := testKEK(t)
	b, err := NewBootstrap(newMemStore(), kek)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	// Distinct subs: the store is keyed by sub, so reusing one would let
	// the first case's stored row win for the later ones.
	cases := []struct {
		id          authn.Identity
		wantEmail   string
		wantDisplay string
	}{
		{authn.Identity{Sub: "s0", Email: "e@x", Name: "N", Username: "u"}, "e@x", "N"},
		{authn.Identity{Sub: "s1", Name: "N", Username: "u"}, "s1", "N"},
		{authn.Identity{Sub: "s2", Username: "u"}, "s2", "u"},
		{authn.Identity{Sub: "s3"}, "s3", "s3"},
	}
	for i, c := range cases {
		row, err := b.EnsureUser(context.Background(), c.id)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if row.Email != c.wantEmail || row.DisplayName != c.wantDisplay {
			t.Fatalf("case %d: want (%q, %q), got (%q, %q)", i, c.wantEmail, c.wantDisplay, row.Email, row.DisplayName)
		}
	}
	if _, err := b.EnsureUser(context.Background(), authn.Identity{}); err == nil {
		t.Fatal("empty sub: want error, got nil")
	}
}

// migratedPool starts a disposable postgres:18 container, applies all
// migrations with goose, and returns a connected pgxpool. Mirrors
// migrate_test.migratedDB (same readiness rationale, D-019); the pool is
// closed and the container stopped when the test ends.
func migratedPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	ctx := context.Background()

	ctr, err := postgres.Run(ctx, "postgres:18", postgres.BasicWaitStrategies())
	if err != nil {
		t.Fatalf("testcontainers postgres: %v", err)
	}
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = ctr.Stop(sctx, nil)
	})

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := pingUntilReady(ctx, db, 60*time.Second); err != nil {
		t.Fatalf("postgres readiness: %v", err)
	}

	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("goose dialect: %v", err)
	}
	if err := goose.UpContext(ctx, db, "../../migrations"); err != nil {
		t.Fatalf("goose up: %v", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgx pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("pool ping: %v", err)
	}
	return pool, ctx
}

// pingUntilReady polls db.PingContext until it succeeds or the budget runs
// out (see migrate_test.pingUntilReady for why the built-in wait alone is
// not trusted).
func pingUntilReady(ctx context.Context, db *sql.DB, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	var lastErr error
	for {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		lastErr = db.PingContext(pctx)
		cancel()
		if lastErr == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("after %v: %w", budget, lastErr)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func countUsers(t *testing.T, pool *pgxpool.Pool, ctx context.Context, sub string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM users WHERE auth0_sub = $1", sub).Scan(&n); err != nil {
		t.Fatalf("count users: %v", err)
	}
	return n
}

func TestPGUserStoreIdempotentAndRaceSafe(t *testing.T) {
	pool, ctx := migratedPool(t)
	kek := testKEK(t)
	b, err := NewBootstrap(NewPGUserStore(pool), kek)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	// Sequential: a second login must not create a second row or replace
	// the stored wrapped DEK.
	first, err := b.EnsureUser(ctx, authn.Identity{Sub: "pg-sub", Email: "pg@localhost", Name: "PG"})
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	again, err := b.EnsureUser(ctx, authn.Identity{Sub: "pg-sub", Email: "pg@localhost", Name: "PG"})
	if err != nil {
		t.Fatalf("repeat ensure: %v", err)
	}
	if again.ID != first.ID || !bytes.Equal(again.WrappedDEK, first.WrappedDEK) {
		t.Fatalf("repeat login changed the row: %+v vs %+v", first, again)
	}
	if n := countUsers(t, pool, ctx, "pg-sub"); n != 1 {
		t.Fatalf("rows for pg-sub: want 1, got %d", n)
	}

	// Concurrent: 8 goroutines race the first login for a fresh sub; the
	// PK/UNIQUE constraint lets exactly one INSERT win and all observers
	// must agree on the stored row.
	const sub = "pg-race"
	const n = 8
	results := make([]UserRow, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = b.EnsureUser(ctx, authn.Identity{Sub: sub, Email: "race@localhost", Name: "Race"})
		}(i)
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
	}
	for i := 1; i < n; i++ {
		if results[i].ID != results[0].ID || !bytes.Equal(results[i].WrappedDEK, results[0].WrappedDEK) {
			t.Fatalf("race produced divergent rows: %+v vs %+v", results[0], results[i])
		}
	}
	if got := countUsers(t, pool, ctx, sub); got != 1 {
		t.Fatalf("rows for %s: want 1, got %d", sub, got)
	}
}

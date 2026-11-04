// Shared test environment for the repo package (plan step 2.5): one
// postgres:18 testcontainer with every goose migration applied, plus
// users rows seeded directly: a fresh per-user DEK wrapped under a test
// KEK, the same wrapped_dek the 1.3c bootstrap path stores. (3.1 made
// the server package depend on repo, so a repo test importing server
// would be an import cycle — the seed INSERTs that row instead.) The
// stores under test then receive exactly what the API hands them: the
// caller's unwrapped DEK — the repos never see the KEK.
//
// migratedPool/pingUntilReady/testKEK are copies of the server package's
// test helpers: test files are package-private, and a shared internal
// testutil package would be a bigger change than the cap allows (D-019).
package repo

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"finance-tracker/internal/crypto"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// testUserRow shadows the one field of server.UserRow the repo tests
// use. server is no longer importable from repo tests: 3.1 made server
// depend on repo, which would be an import cycle in the test.
type testUserRow struct {
	ID string
}

// testUser is one seeded users row plus its unwrapped DEK.
type testUser struct {
	row testUserRow
	dek []byte
}

type testEnv struct {
	pool  *pgxpool.Pool
	ctx   context.Context
	users map[string]testUser // keyed by sub
}

// newTestEnv starts one migrated database and seeds one users row per
// sub, storing a fresh per-user DEK wrapped under a test KEK — the same
// row the 1.3c bootstrap path writes. Every test in the package that
// needs a database uses a single env (the container is the expensive
// part, ~7-15 s).
func newTestEnv(t *testing.T, subs ...string) *testEnv {
	t.Helper()
	pool, ctx := migratedPool(t)
	kek := testKEK(t)

	env := &testEnv{pool: pool, ctx: ctx, users: map[string]testUser{}}
	for _, sub := range subs {
		dek, err := crypto.NewDEK()
		if err != nil {
			t.Fatalf("generate DEK for %s: %v", sub, err)
		}
		wrapped, err := crypto.WrapDEK(kek, dek)
		if err != nil {
			t.Fatalf("wrap DEK for %s: %v", sub, err)
		}
		var id string
		err = pool.QueryRow(ctx, `
			INSERT INTO users (auth0_sub, email, display_name, wrapped_dek)
			VALUES ($1, $2, $3, $4)
			RETURNING id`, sub, sub+"@localhost", sub, wrapped).Scan(&id)
		if err != nil {
			t.Fatalf("seed user %s: %v", sub, err)
		}
		env.users[sub] = testUser{row: testUserRow{ID: id}, dek: dek}
	}
	return env
}

func (e *testEnv) user(t *testing.T, sub string) testUser {
	t.Helper()
	u, ok := e.users[sub]
	if !ok {
		t.Fatalf("no test user %q (newTestEnv was not asked for it)", sub)
	}
	return u
}

func testKEK(t *testing.T) []byte {
	t.Helper()
	kek := make([]byte, crypto.KEKSize)
	if _, err := rand.Read(kek); err != nil {
		t.Fatalf("KEK random: %v", err)
	}
	return kek
}

// migratedPool starts a disposable postgres:18 container, applies all
// migrations with goose, and returns a connected pgxpool. Copy of
// server.bootstrap_test.migratedPool (same readiness rationale, D-019);
// the migrations path is relative to THIS package: ../../migrations.
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
// out (copy of the server package's helper: the container's built-in wait
// alone is not trusted — postgres can accept connections before it is
// ready to serve them).
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

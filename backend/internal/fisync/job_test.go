// Job tests (plan 9.2): RunOnce against a testcontainers postgres:18
// with all migrations, following the categorize/runner_test.go env
// pattern (that one is unexported, so this is a local copy — D-019:
// no shared testutil). Seeds one users row per sub (fresh DEK wrapped
// under a test KEK), one active institution, and one credential (two
// sealed packs); then: tick 1 creates the "Fake Bank Account" account
// + 2 txs + stamps last_synced_at, tick 2 (dedup) adds ZERO rows and
// restamps, a no-adapter credential writes the pinned notification,
// and a failing adapter writes the pinned notification without
// advancing last_synced_at.
package fisync

import (
	"context"
	"crypto/rand"
	"database/sql"
	"testing"
	"time"

	"finance-tracker/internal/crypto"
	"finance-tracker/internal/repo"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

type jobUser struct {
	id  string
	dek []byte
}

type jobEnv struct {
	pool  *pgxpool.Pool
	ctx   context.Context
	kek   []byte
	users map[string]jobUser // keyed by sub
}

// newJobEnv starts one migrated postgres:18 container and seeds one
// users row per sub (fresh DEK wrapped under a test KEK — the same
// row the 1.3c bootstrap path writes). Copy of the categorize package's
// newRunnerEnv (D-019: no shared testutil).
func newJobEnv(t *testing.T, subs ...string) *jobEnv {
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
	deadline := time.Now().Add(60 * time.Second)
	for {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		perr := db.PingContext(pctx)
		cancel()
		if perr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("postgres readiness: %v", perr)
		}
		time.Sleep(500 * time.Millisecond)
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
	kek := make([]byte, crypto.KEKSize)
	if _, err := rand.Read(kek); err != nil {
		t.Fatalf("KEK random: %v", err)
	}
	env := &jobEnv{pool: pool, ctx: ctx, kek: kek, users: map[string]jobUser{}}
	for _, sub := range subs {
		dek, err := crypto.NewDEK()
		if err != nil {
			t.Fatalf("generate DEK: %v", err)
		}
		wrapped, err := crypto.WrapDEK(kek, dek)
		if err != nil {
			t.Fatalf("wrap DEK: %v", err)
		}
		var id string
		err = pool.QueryRow(ctx, `
			INSERT INTO users (auth0_sub, email, display_name, wrapped_dek)
			VALUES ($1, $2, $3, $4)
			RETURNING id`, sub, sub+"@localhost", sub, wrapped).Scan(&id)
		if err != nil {
			t.Fatalf("seed user %s: %v", sub, err)
		}
		env.users[sub] = jobUser{id: id, dek: dek}
	}
	return env
}

func (e *jobEnv) user(t *testing.T, sub string) jobUser {
	t.Helper()
	u, ok := e.users[sub]
	if !ok {
		t.Fatalf("no test user %q (newJobEnv was not asked for it)", sub)
	}
	return u
}

// dekForUser resolves the DEK the way cmd/worker's existing closure
// does: users.wrapped_dek unwrapped under the test KEK.
func (e *jobEnv) dekForUser() func(context.Context, string) ([]byte, error) {
	return func(ctx context.Context, userID string) ([]byte, error) {
		var wrapped []byte
		if err := e.pool.QueryRow(ctx,
			`SELECT wrapped_dek FROM users WHERE id = $1`, userID).Scan(&wrapped); err != nil {
			return nil, err
		}
		return crypto.UnwrapDEK(e.kek, wrapped)
	}
}

func TestFISyncJob(t *testing.T) {
	env := newJobEnv(t, "fisync-a", "fisync-b")
	ctx := env.ctx
	a, b := env.user(t, "fisync-a"), env.user(t, "fisync-b")

	inst := repo.NewInstitutionStore(env.pool)
	creds := repo.NewFICredentialStore(env.pool)
	accts := repo.NewAccountStore(env.pool)
	txs := repo.NewTransactionStore(env.pool)
	notifs := repo.NewNotificationStore(env.pool)

	fakeBank, err := inst.Create(ctx, repo.InstitutionIn{Code: "fake-bank", Name: "Fake Bank"})
	if err != nil {
		t.Fatalf("create institution: %v", err)
	}
	credA, err := creds.Create(ctx, a.id, repo.FICredentialIn{
		InstitutionID: fakeBank.ID,
		Token:         "job-access-token",
		RefreshToken:  "job-refresh-token",
	}, a.dek)
	if err != nil {
		t.Fatalf("create credential: %v", err)
	}
	job := NewJob(creds, inst, accts, txs, notifs,
		NewRegistry(NewFakeAdapter(false)), env.dekForUser())
	if _, err := creds.Get(ctx, a.id, credA.ID, a.dek); err != nil {
		t.Fatalf("credential round trip: %v", err)
	}

	now1 := time.Date(2026, 11, 8, 9, 5, 0, 0, time.UTC)

	t.Run("tick 1 creates the account, 2 txs, stamps last_synced_at", func(t *testing.T) {
		if err := job.RunOnce(ctx, now1); err != nil {
			t.Fatalf("run once: %v", err)
		}
		gotAccts, err := accts.List(ctx, a.id, a.dek)
		if err != nil {
			t.Fatalf("list accounts: %v", err)
		}
		if len(gotAccts) != 1 || gotAccts[0].Name != "Fake Bank Account" ||
			gotAccts[0].Type != "bank" || gotAccts[0].Currency != "USD" {
			t.Fatalf("accounts: %+v, want one bank/USD 'Fake Bank Account'", gotAccts)
		}
		gotTxs, err := txs.List(ctx, a.id, repo.TransactionFilter{}, a.dek)
		if err != nil {
			t.Fatalf("list txs: %v", err)
		}
		if len(gotTxs) != 2 {
			t.Fatalf("txs: got %d, want 2", len(gotTxs))
		}
		// List orders date_posted DESC: idx0 = 2026-01-12 payroll,
		// idx1 = 2026-01-10 subscription.
		want := []struct {
			date   time.Time
			amount string
			desc   string
		}{
			{time.Date(2026, 1, 12, 0, 0, 0, 0, time.UTC), "1500.00", "FAKE PAYROLL"},
			{time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC), "-42.13", "FAKE SUBSCRIPTION"},
		}
		for i, w := range want {
			g := gotTxs[i]
			if g.AccountID != gotAccts[0].ID {
				t.Fatalf("tx %d on account %s, want the fake account %s", i, g.AccountID, gotAccts[0].ID)
			}
			if !g.DatePosted.Equal(w.date) || g.Amount != w.amount || g.Description != w.desc {
				t.Fatalf("tx %d: got %+v, want date %v amount %q desc %q", i, g, w.date, w.amount, w.desc)
			}
		}
		gotCreds, err := creds.List(ctx, a.id, a.dek)
		if err != nil {
			t.Fatalf("list credentials: %v", err)
		}
		if len(gotCreds) != 1 || gotCreds[0].LastSyncedAt == nil || !gotCreds[0].LastSyncedAt.Equal(now1) {
			t.Fatalf("last_synced_at = %+v, want %v", gotCreds, now1)
		}
	})

	now2 := now1.Add(time.Hour)
	t.Run("tick 2 dedups to zero new rows, restamps last_synced_at", func(t *testing.T) {
		if err := job.RunOnce(ctx, now2); err != nil {
			t.Fatalf("run once: %v", err)
		}
		gotAccts, err := accts.List(ctx, a.id, a.dek)
		if err != nil || len(gotAccts) != 1 {
			t.Fatalf("accounts after tick 2: %+v (err %v), want still 1", gotAccts, err)
		}
		gotTxs, err := txs.List(ctx, a.id, repo.TransactionFilter{}, a.dek)
		if err != nil || len(gotTxs) != 2 {
			t.Fatalf("txs after tick 2: got %d (err %v), want still 2 (dedup)", len(gotTxs), err)
		}
		gotCreds, err := creds.List(ctx, a.id, a.dek)
		if err != nil {
			t.Fatalf("list credentials: %v", err)
		}
		if len(gotCreds) != 1 || gotCreds[0].LastSyncedAt == nil || !gotCreds[0].LastSyncedAt.Equal(now2) {
			t.Fatalf("last_synced_at after tick 2 = %+v, want %v", gotCreds, now2)
		}
	})

	t.Run("no-adapter credential writes the pinned notification", func(t *testing.T) {
		none, err := inst.Create(ctx, repo.InstitutionIn{Code: "no-adapter", Name: "No Adapter Bank"})
		if err != nil {
			t.Fatalf("create institution: %v", err)
		}
		if _, err := creds.Create(ctx, b.id, repo.FICredentialIn{
			InstitutionID: none.ID, Token: "b-token", RefreshToken: "b-refresh",
		}, b.dek); err != nil {
			t.Fatalf("create b credential: %v", err)
		}
		// A per-credential failure NEVER fails RunOnce.
		if err := job.RunOnce(ctx, now2.Add(time.Hour)); err != nil {
			t.Fatalf("run once: %v (per-credential failures must not fail RunOnce)", err)
		}
		got, err := notifs.List(ctx, b.id, repo.NotificationFilter{Limit: 100})
		if err != nil {
			t.Fatalf("list notifications: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("notifications: got %d, want 1", len(got))
		}
		n := got[0]
		if n.Kind != "error" ||
			n.Title != "no adapter registered for institution no-adapter" ||
			n.Body != "institution code has no registered adapter" {
			t.Fatalf("notification: kind %q title %q body %q, want the pinned no-adapter values", n.Kind, n.Title, n.Body)
		}
		// The row stopped before account mapping: user b got no account.
		gotAccts, err := accts.List(ctx, b.id, b.dek)
		if err != nil || len(gotAccts) != 0 {
			t.Fatalf("b accounts: %+v (err %v), want none", gotAccts, err)
		}
	})

	t.Run("failing adapter notifies and does not advance last_synced_at", func(t *testing.T) {
		failJob := NewJob(creds, inst, accts, txs, notifs,
			NewRegistry(NewFakeAdapter(true)), env.dekForUser())
		// The previous subtest's successful pass already stamped user a
		// at now2+1h (11:05); the failing tick uses a LATER now (12:05)
		// and must not advance the stamp.
		nowFail := now2.Add(2 * time.Hour)
		if err := failJob.RunOnce(ctx, nowFail); err != nil {
			t.Fatalf("run once: %v (per-credential failures must not fail RunOnce)", err)
		}
		got, err := notifs.List(ctx, a.id, repo.NotificationFilter{Limit: 100})
		if err != nil {
			t.Fatalf("list notifications: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("a notifications: got %d, want 1", len(got))
		}
		n := got[0]
		if n.Kind != "error" ||
			n.Title != "FI sync failed for fake-bank" ||
			n.Body != "fake adapter: simulated failure" {
			t.Fatalf("notification: kind %q title %q body %q, want the pinned failure values", n.Kind, n.Title, n.Body)
		}
		gotCreds, err := creds.List(ctx, a.id, a.dek)
		if err != nil || len(gotCreds) != 1 {
			t.Fatalf("list credentials: %+v (err %v)", gotCreds, err)
		}
		// Stamped at now2+1h by the earlier successful pass; the
		// failing tick (now2+2h) must not advance it.
		stamp := now2.Add(time.Hour)
		if gotCreds[0].LastSyncedAt == nil || !gotCreds[0].LastSyncedAt.Equal(stamp) {
			t.Fatalf("last_synced_at advanced to %v, want still %v (failing tick does not stamp)",
				gotCreds[0].LastSyncedAt, stamp)
		}
		gotTxs, err := txs.List(ctx, a.id, repo.TransactionFilter{}, a.dek)
		if err != nil || len(gotTxs) != 2 {
			t.Fatalf("txs after failing tick: got %d (err %v), want still 2", len(gotTxs), err)
		}
	})
}

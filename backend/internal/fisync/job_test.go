// Job tests (plan 9.2): RunOnce against a testcontainers postgres:18
// with all migrations, following the categorize/runner_test.go env
// pattern (that one is unexported, so this is a local copy — D-019:
// no shared testutil). Seeds one users row per sub (fresh DEK wrapped
// under a test KEK), one active institution, and one credential (two
// sealed packs); then: tick 1 creates the "Fake Bank Account" account
// + 2 txs + stamps last_synced_at, tick 2 (empty overlap) adds ZERO
// rows and restamps, a no-adapter credential writes the pinned
// notification,
// and a failing adapter writes the pinned notification without
// advancing last_synced_at. TestFISyncJobWatermarkAndBatches pins the
// watermark contract against a scripted adapter (first pass = full
// backfill, later passes re-fetch only the 48h overlap, per-account
// batch insert, empty overlap stamps without touching accounts);
// TestFISyncJobBatchFailureBlocksStamp pins that a failed batch
// notifies and blocks the stamp.
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
	// The fake's pinned dates (January) sit far outside tick 2's
	// watermark (now1-48h): fresh is empty, so the pass takes the
	// no-probe/no-upsert short-circuit and just restamps.
	t.Run("tick 2 empty overlap adds zero rows, restamps last_synced_at", func(t *testing.T) {
		if err := job.RunOnce(ctx, now2); err != nil {
			t.Fatalf("run once: %v", err)
		}
		gotAccts, err := accts.List(ctx, a.id, a.dek)
		if err != nil || len(gotAccts) != 1 {
			t.Fatalf("accounts after tick 2: %+v (err %v), want still 1", gotAccts, err)
		}
		gotTxs, err := txs.List(ctx, a.id, repo.TransactionFilter{}, a.dek)
		if err != nil || len(gotTxs) != 2 {
			t.Fatalf("txs after tick 2: got %d (err %v), want still 2 (empty overlap)", len(gotTxs), err)
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

// scriptedTx is one line of a scripted adapter's script: the tx plus
// the earliest tick now at which the adapter serves it (late-arrival
// simulation: a tx "appears" on the statement from visibleAt on).
type scriptedTx struct {
	tx        IncomingTransaction
	visibleAt time.Time
}

// scriptedAdapter is a test-only InstitutionAdapter: fake.go's fake is
// pinned (the worker dev mode, the e2e, and the docs rely on its exact
// output), so the watermark and batch tests script their own tx sets.
// Sync returns a fresh copy of every line whose visibleAt has passed —
// deterministic across ticks, independent of the credential.
type scriptedAdapter struct {
	code  string
	lines []scriptedTx
}

func (s *scriptedAdapter) Code() string { return s.code }

func (s *scriptedAdapter) Sync(ctx context.Context, cred TokenCredentials, now time.Time) ([]IncomingTransaction, error) {
	out := make([]IncomingTransaction, 0, len(s.lines))
	for _, line := range s.lines {
		if !now.Before(line.visibleAt) {
			out = append(out, line.tx)
		}
	}
	return out, nil
}

// TestFISyncJobWatermarkAndBatches pins the watermark contract with a
// scripted adapter: tick 1 (nil stamp) is a full backfill; tick 2
// re-fetches the 48h overlap and dedups to zero new rows; tick 3's
// overlap catches a late-arriving tx on a SECOND account (one
// UpsertBatch per touched account); tick 4's overlap re-fetches the
// already-stored txs (dedup to zero new rows) while the watermark
// filters a newly visible tx whose posting is outside it — a label
// that appears only in filtered-out txs must not create an account —
// and the pass still stamps. (The empty-overlap short-circuit is pinned
// by TestFISyncJob's tick 2, whose fake dates are months old.)
func TestFISyncJobWatermarkAndBatches(t *testing.T) {
	env := newJobEnv(t, "fisync-w")
	ctx := env.ctx
	w := env.user(t, "fisync-w")

	inst := repo.NewInstitutionStore(env.pool)
	creds := repo.NewFICredentialStore(env.pool)
	accts := repo.NewAccountStore(env.pool)
	txs := repo.NewTransactionStore(env.pool)
	notifs := repo.NewNotificationStore(env.pool)

	bank, err := inst.Create(ctx, repo.InstitutionIn{Code: "scripted-bank", Name: "Scripted Bank"})
	if err != nil {
		t.Fatalf("create institution: %v", err)
	}
	if _, err := creds.Create(ctx, w.id, repo.FICredentialIn{
		InstitutionID: bank.ID, Token: "w-token", RefreshToken: "w-refresh",
	}, w.dek); err != nil {
		t.Fatalf("create credential: %v", err)
	}

	now1 := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	now2 := now1.Add(time.Hour)
	now3 := now1.Add(2 * time.Hour)
	now4 := now3.Add(100 * time.Hour)
	checking, savings, vault := "Scripted Checking", "Scripted Savings", "Scripted Vault"
	// old: posted 100h before tick 1 — outside every overlap window of
	// the test (tick 2's watermark is now1-48h, tick 4's now3-48h).
	// recent: posted 2h before tick 1 — inside every later tick's
	// overlap, re-fetched and deduped to zero new rows each time.
	// late: appears on the statement between tick 2 and tick 3, posted
	// inside tick 3's overlap — the overlap's reason for existing.
	// hidden: appears at tick 4 on a label never seen before, but is
	// posted OUTSIDE tick 4's overlap — it must not create the account.
	adapter := &scriptedAdapter{code: "scripted-bank", lines: []scriptedTx{
		{tx: IncomingTransaction{
			ExternalID: "scr-1", AccountName: checking,
			DatePosted: now1.Add(-100 * time.Hour), Amount: "-100.50", Description: "OLD MERCHANT",
		}, visibleAt: now1},
		{tx: IncomingTransaction{
			ExternalID: "scr-2", AccountName: checking,
			DatePosted: now1.Add(-2 * time.Hour), Amount: "250.00", Description: "RECENT MERCHANT",
		}, visibleAt: now1},
		{tx: IncomingTransaction{
			ExternalID: "scr-3", AccountName: savings,
			DatePosted: now1.Add(90 * time.Minute), Amount: "75.25", Description: "LATE MERCHANT",
		}, visibleAt: now1.Add(90 * time.Minute)},
		{tx: IncomingTransaction{
			ExternalID: "scr-4", AccountName: vault,
			DatePosted: now1.Add(-100 * time.Hour), Amount: "10.00", Description: "HIDDEN MERCHANT",
		}, visibleAt: now4},
	}}
	job := NewJob(creds, inst, accts, txs, notifs,
		NewRegistry(adapter), env.dekForUser())

	// date_posted is a `date` column: the adapter's time-of-day is
	// discarded on insert, so stored rows read back as midnight UTC.
	// recent and late share a posting date (both on tick 1's day).
	recentDate := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	oldDate := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)

	wantAccounts := func(t *testing.T, names ...string) {
		t.Helper()
		gotAccts, err := accts.List(ctx, w.id, w.dek)
		if err != nil {
			t.Fatalf("list accounts: %v", err)
		}
		if len(gotAccts) != len(names) {
			t.Fatalf("accounts: %+v, want %d (one per %v)", gotAccts, len(names), names)
		}
		seen := map[string]bool{}
		for _, a := range gotAccts {
			seen[a.Name] = true
		}
		for _, n := range names {
			if !seen[n] {
				t.Fatalf("accounts %v, want %q among them", seen, n)
			}
		}
	}
	stampOf := func(t *testing.T) *time.Time {
		t.Helper()
		gotCreds, err := creds.List(ctx, w.id, w.dek)
		if err != nil || len(gotCreds) != 1 {
			t.Fatalf("list credentials: %+v (err %v)", gotCreds, err)
		}
		return gotCreds[0].LastSyncedAt
	}
	if s := stampOf(t); s != nil {
		t.Fatalf("fresh credential has last_synced_at = %v, want nil", s)
	}

	t.Run("tick 1 (nil stamp) is a full backfill and stamps", func(t *testing.T) {
		if err := job.RunOnce(ctx, now1); err != nil {
			t.Fatalf("run once: %v", err)
		}
		gotTxs, err := txs.List(ctx, w.id, repo.TransactionFilter{}, w.dek)
		if err != nil {
			t.Fatalf("list txs: %v", err)
		}
		// late is not on the statement yet (appears between tick 2 and
		// 3): the backfill lands old + recent, both on checking.
		if len(gotTxs) != 2 {
			t.Fatalf("txs: got %d, want 2 (late not yet visible)", len(gotTxs))
		}
		// List orders date_posted DESC: idx0 = recent, idx1 = old.
		want := []struct {
			date   time.Time
			amount string
			desc   string
		}{
			{recentDate, "250.00", "RECENT MERCHANT"},
			{oldDate, "-100.50", "OLD MERCHANT"},
		}
		for i, wrow := range want {
			g := gotTxs[i]
			if !g.DatePosted.Equal(wrow.date) || g.Amount != wrow.amount || g.Description != wrow.desc {
				t.Fatalf("tx %d: got date %v amount %q desc %q, want %+v", i, g.DatePosted, g.Amount, g.Description, wrow)
			}
		}
		wantAccounts(t, checking)
		s := stampOf(t)
		if s == nil || !s.Equal(now1) {
			t.Fatalf("last_synced_at = %v, want %v", s, now1)
		}
	})

	t.Run("tick 2 re-fetches the overlap and dedups to zero new rows", func(t *testing.T) {
		if err := job.RunOnce(ctx, now2); err != nil {
			t.Fatalf("run once: %v", err)
		}
		// watermark = now1-48h: old (now1-100h) is filtered out, recent
		// (now1-2h) is re-fetched and dedups; late still not visible.
		gotTxs, err := txs.List(ctx, w.id, repo.TransactionFilter{}, w.dek)
		if err != nil || len(gotTxs) != 2 {
			t.Fatalf("txs after tick 2: got %d (err %v), want still 2 (overlap dedup)", len(gotTxs), err)
		}
		wantAccounts(t, checking)
		s := stampOf(t)
		if s == nil || !s.Equal(now2) {
			t.Fatalf("last_synced_at = %v, want %v", s, now2)
		}
	})

	t.Run("tick 3 overlap catches the late tx on a second account", func(t *testing.T) {
		if err := job.RunOnce(ctx, now3); err != nil {
			t.Fatalf("run once: %v", err)
		}
		// watermark = now2-48h: recent re-fetches and dedups; late
		// (posted now1+90m, now visible) is inside the overlap and lands
		// on the second account.
		gotTxs, err := txs.List(ctx, w.id, repo.TransactionFilter{}, w.dek)
		if err != nil || len(gotTxs) != 3 {
			t.Fatalf("txs after tick 3: got %d (err %v), want 3 (late landed)", len(gotTxs), err)
		}
		var lateFound bool
		for _, g := range gotTxs {
			if g.Description == "LATE MERCHANT" && g.Amount == "75.25" &&
				g.DatePosted.Equal(recentDate) {
				lateFound = true
			}
		}
		if !lateFound {
			t.Fatalf("txs after tick 3: %+v, want the LATE MERCHANT row", gotTxs)
		}
		wantAccounts(t, checking, savings)
		s := stampOf(t)
		if s == nil || !s.Equal(now3) {
			t.Fatalf("last_synced_at = %v, want %v", s, now3)
		}
	})

	t.Run("tick 4 watermark-filtered label does not create an account", func(t *testing.T) {
		if err := job.RunOnce(ctx, now4); err != nil {
			t.Fatalf("run once: %v", err)
		}
		// watermark = now3-48h (now1-46h): old and hidden are filtered
		// out — so the hidden tx's vault label is never even mapped —
		// while recent and late sit inside the overlap and re-fetch to
		// zero new rows (dedup). The pass still stamps.
		gotTxs, err := txs.List(ctx, w.id, repo.TransactionFilter{}, w.dek)
		if err != nil || len(gotTxs) != 3 {
			t.Fatalf("txs after tick 4: got %d (err %v), want still 3", len(gotTxs), err)
		}
		wantAccounts(t, checking, savings)
		s := stampOf(t)
		if s == nil || !s.Equal(now4) {
			t.Fatalf("last_synced_at = %v, want %v (empty overlap still stamps)", s, now4)
		}
		got, err := notifs.List(ctx, w.id, repo.NotificationFilter{Limit: 100})
		if err != nil {
			t.Fatalf("list notifications: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("notifications: got %+v, want none (the happy path never notifies)", got)
		}
	})
}

// TestFISyncJobBatchFailureBlocksStamp pins the batch-failure contract:
// a failed UpsertBatch for an account notifies and BLOCKS the stamp
// (last_synced_at stays nil), while the account mapping that ran
// before the batch already created the account. The follow-up pass
// with a good adapter sees the stamp still nil, so it is a full
// backfill again — the good tx lands and the pass stamps.
func TestFISyncJobBatchFailureBlocksStamp(t *testing.T) {
	env := newJobEnv(t, "fisync-fb")
	ctx := env.ctx
	fb := env.user(t, "fisync-fb")

	inst := repo.NewInstitutionStore(env.pool)
	creds := repo.NewFICredentialStore(env.pool)
	accts := repo.NewAccountStore(env.pool)
	txs := repo.NewTransactionStore(env.pool)
	notifs := repo.NewNotificationStore(env.pool)

	bank, err := inst.Create(ctx, repo.InstitutionIn{Code: "scripted-bad-bank", Name: "Scripted Bad Bank"})
	if err != nil {
		t.Fatalf("create institution: %v", err)
	}
	if _, err := creds.Create(ctx, fb.id, repo.FICredentialIn{
		InstitutionID: bank.ID, Token: "fb-token", RefreshToken: "fb-refresh",
	}, fb.dek); err != nil {
		t.Fatalf("create credential: %v", err)
	}

	nowF := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	// The bad amount fails canonicalAmount inside UpsertBatch's
	// validateBatchRow — a deterministic batch failure without a DB
	// fault.
	bad := &scriptedAdapter{code: "scripted-bad-bank", lines: []scriptedTx{
		{tx: IncomingTransaction{
			ExternalID: "bad-1", AccountName: "Bad Checking",
			DatePosted: nowF.Add(-time.Hour), Amount: "not-a-number", Description: "BAD MERCHANT",
		}, visibleAt: nowF},
	}}
	job := NewJob(creds, inst, accts, txs, notifs,
		NewRegistry(bad), env.dekForUser())

	t.Run("failed batch notifies and blocks the stamp", func(t *testing.T) {
		// A per-credential failure NEVER fails RunOnce.
		if err := job.RunOnce(ctx, nowF); err != nil {
			t.Fatalf("run once: %v (per-credential failures must not fail RunOnce)", err)
		}
		// The bad row was mapped (the account exists) but never
		// inserted — the batch failed before the chunk loop.
		gotAccts, err := accts.List(ctx, fb.id, fb.dek)
		if err != nil || len(gotAccts) != 1 || gotAccts[0].Name != "Bad Checking" {
			t.Fatalf("accounts: %+v (err %v), want the mapped 'Bad Checking'", gotAccts, err)
		}
		gotTxs, err := txs.List(ctx, fb.id, repo.TransactionFilter{}, fb.dek)
		if err != nil || len(gotTxs) != 0 {
			t.Fatalf("txs: got %d (err %v), want 0 (the batch failed)", len(gotTxs), err)
		}
		got, err := notifs.List(ctx, fb.id, repo.NotificationFilter{Limit: 100})
		if err != nil || len(got) != 1 {
			t.Fatalf("notifications: got %+v (err %v), want 1", got, err)
		}
		n := got[0]
		if n.Kind != "error" ||
			n.Title != "FI sync failed for scripted-bad-bank" ||
			n.Body != "upsert: amount \"not-a-number\" is not a valid decimal" {
			t.Fatalf("notification: kind %q title %q body %q, want the pinned batch-failure values", n.Kind, n.Title, n.Body)
		}
		gotCreds, err := creds.List(ctx, fb.id, fb.dek)
		if err != nil || len(gotCreds) != 1 {
			t.Fatalf("list credentials: %+v (err %v)", gotCreds, err)
		}
		if gotCreds[0].LastSyncedAt != nil {
			t.Fatalf("last_synced_at = %v, want nil (a failed batch must not stamp)", gotCreds[0].LastSyncedAt)
		}
	})

	t.Run("good follow-up pass is a full backfill again and stamps", func(t *testing.T) {
		good := &scriptedAdapter{code: "scripted-bad-bank", lines: []scriptedTx{
			{tx: IncomingTransaction{
				ExternalID: "bad-2", AccountName: "Bad Checking",
				DatePosted: nowF.Add(-time.Hour), Amount: "10.00", Description: "GOOD MERCHANT",
			}, visibleAt: nowF},
		}}
		goodJob := NewJob(creds, inst, accts, txs, notifs,
			NewRegistry(good), env.dekForUser())
		nowF2 := nowF.Add(time.Hour)
		if err := goodJob.RunOnce(ctx, nowF2); err != nil {
			t.Fatalf("run once: %v", err)
		}
		// Stamp still nil before this pass: no watermark, full
		// backfill. The account is REUSED (not re-created).
		gotTxs, err := txs.List(ctx, fb.id, repo.TransactionFilter{}, fb.dek)
		if err != nil || len(gotTxs) != 1 {
			t.Fatalf("txs after good pass: got %d (err %v), want 1", len(gotTxs), err)
		}
		if gotTxs[0].Amount != "10.00" || gotTxs[0].Description != "GOOD MERCHANT" {
			t.Fatalf("tx after good pass: got amount %q desc %q, want the GOOD MERCHANT row",
				gotTxs[0].Amount, gotTxs[0].Description)
		}
		gotAccts, err := accts.List(ctx, fb.id, fb.dek)
		if err != nil || len(gotAccts) != 1 || gotAccts[0].Name != "Bad Checking" {
			t.Fatalf("accounts after good pass: %+v (err %v), want the reused 'Bad Checking'", gotAccts, err)
		}
		gotCreds, err := creds.List(ctx, fb.id, fb.dek)
		if err != nil || len(gotCreds) != 1 {
			t.Fatalf("list credentials: %+v (err %v)", gotCreds, err)
		}
		if gotCreds[0].LastSyncedAt == nil || !gotCreds[0].LastSyncedAt.Equal(nowF2) {
			t.Fatalf("last_synced_at = %v, want %v", gotCreds[0].LastSyncedAt, nowF2)
		}
	})
}

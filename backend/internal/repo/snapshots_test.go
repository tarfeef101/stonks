// Snapshot CRUD + equity series tests (round 51c1): one postgres:18
// testcontainer per test (newTestEnv) covering CreateSnapshot
// (canonicalization, note sealing at rest, (account, as_of) conflict,
// invalid balances), ListSnapshots (as_of DESC), Get/Update/Delete
// (tri-state note patch, as_of immutability, per-user scoping), and
// EquitySeries (snapshot-anchored, opening-based, no-snapshot, edge
// cases).
package repo

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// countSnapshots counts account_snapshots rows for an account (raw,
// unscoped — test-only sanity check that rejected writes landed now).
func countSnapshots(t *testing.T, pool *pgxpool.Pool, ctx context.Context, accountID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM account_snapshots WHERE account_id = $1`,
		accountID).Scan(&n); err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	return n
}

func TestSnapshotCRUD(t *testing.T) {
	env := newTestEnv(t, "sn-a", "sn-b")
	snap := NewSnapshotStore(env.pool)
	acct := NewAccountStore(env.pool)
	a := env.user(t, "sn-a")
	b := env.user(t, "sn-b")
	ctx := env.ctx

	acc, err := acct.Create(ctx, a.row.ID, AccountIn{Name: "RRSP", Type: "rrsp", Currency: "USD", OpeningBalance: strPtr("1000.00")}, a.dek)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	s0, err := snap.CreateSnapshot(ctx, a.row.ID, SnapshotIn{
		AccountID: acc.ID, AsOf: dateOf("2024-06-01"), Balance: "900.5", Note: "mid-year check",
	}, a.dek)
	if err != nil {
		t.Fatalf("create s0: %v", err)
	}
	if s0.Balance != "900.50" || s0.Note != "mid-year check" {
		t.Fatalf("s0: balance %q, note %q", s0.Balance, s0.Note)
	}
	if s0.UserID != a.row.ID || s0.AccountID != acc.ID || !s0.AsOf.Equal(dateOf("2024-06-01")) {
		t.Fatalf("s0 ids/dates: %+v", s0)
	}
	if s0.CreatedAt.IsZero() || s0.UpdatedAt.IsZero() {
		t.Fatalf("s0 timestamps: %+v", s0)
	}

	s1, err := snap.CreateSnapshot(ctx, a.row.ID, SnapshotIn{
		AccountID: acc.ID, AsOf: dateOf("2025-01-01"), Balance: "1500", Note: "",
	}, a.dek)
	if err != nil {
		t.Fatalf("create s1: %v", err)
	}
	if s1.Balance != "1500.00" || s1.Note != "" {
		t.Fatalf("s1: balance %q, note %q", s1.Balance, s1.Note)
	}

	t.Run("list is as_of DESC", func(t *testing.T) {
		got, err := snap.ListSnapshots(ctx, a.row.ID, acc.ID, a.dek)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("list: want 2, got %d", len(got))
		}
		if !got[0].AsOf.Equal(dateOf("2025-01-01")) || !got[1].AsOf.Equal(dateOf("2024-06-01")) {
			t.Fatalf("list order: %+v", got)
		}
		if got[0].Balance != "1500.00" || got[1].Balance != "900.50" {
			t.Fatalf("list balances: %q / %q", got[0].Balance, got[1].Balance)
		}
	})

	t.Run("get round-trips", func(t *testing.T) {
		got, err := snap.GetSnapshot(ctx, a.row.ID, s1.ID, a.dek)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.ID != s1.ID || got.Balance != "1500.00" || got.Note != "" ||
			!got.AsOf.Equal(dateOf("2025-01-01")) {
			t.Fatalf("get: %+v", got)
		}
	})

	t.Run("note sealed at rest", func(t *testing.T) {
		var raw *[]byte
		if err := env.pool.QueryRow(ctx,
			`SELECT note_encrypted FROM account_snapshots WHERE id = $1`, s0.ID).Scan(&raw); err != nil {
			t.Fatalf("raw s0 note: %v", err)
		}
		if raw == nil || len(*raw) == 0 {
			t.Fatal("s0 note_encrypted must be a sealed pack, got NULL/empty")
		}
		if bytes.Contains(*raw, []byte("mid-year check")) {
			t.Fatal("s0 note is stored in the clear")
		}
		var raw1 *[]byte
		if err := env.pool.QueryRow(ctx,
			`SELECT note_encrypted FROM account_snapshots WHERE id = $1`, s1.ID).Scan(&raw1); err != nil {
			t.Fatalf("raw s1 note: %v", err)
		}
		if raw1 != nil {
			t.Fatalf("s1 note_encrypted must be NULL for empty note, got %v", raw1)
		}
	})

	t.Run("update balance and clear note", func(t *testing.T) {
		updated, err := snap.UpdateSnapshot(ctx, a.row.ID, s1.ID,
			SnapshotPatch{Balance: strPtr("1600.25"), Note: strPtr("")}, a.dek)
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if updated.Balance != "1600.25" || updated.Note != "" {
			t.Fatalf("updated: balance %q, note %q", updated.Balance, updated.Note)
		}
		if !updated.AsOf.Equal(dateOf("2025-01-01")) {
			t.Fatalf("as_of must be immutable, got %v", updated.AsOf)
		}
		got, err := snap.GetSnapshot(ctx, a.row.ID, s1.ID, a.dek)
		if err != nil {
			t.Fatalf("get after update: %v", err)
		}
		if got.Balance != "1600.25" || got.Note != "" {
			t.Fatalf("persisted: balance %q, note %q", got.Balance, got.Note)
		}
		// s0 untouched.
		s0got, err := snap.GetSnapshot(ctx, a.row.ID, s0.ID, a.dek)
		if err != nil {
			t.Fatalf("get s0: %v", err)
		}
		if s0got.Balance != "900.50" || s0got.Note != "mid-year check" {
			t.Fatalf("s0 changed: balance %q, note %q", s0got.Balance, s0got.Note)
		}
	})

	t.Run("duplicate (account, as_of) conflicts", func(t *testing.T) {
		_, err := snap.CreateSnapshot(ctx, a.row.ID, SnapshotIn{
			AccountID: acc.ID, AsOf: dateOf("2025-01-01"), Balance: "1",
		}, a.dek)
		if !errors.Is(err, ErrSnapshotConflict) {
			t.Fatalf("duplicate day: want ErrSnapshotConflict, got %v", err)
		}
		if n := countSnapshots(t, env.pool, ctx, acc.ID); n != 2 {
			t.Fatalf("rows after conflict: %d", n)
		}
	})

	t.Run("invalid balances rejected", func(t *testing.T) {
		for _, bal := range []string{"-5", "1.234"} {
			_, err := snap.CreateSnapshot(ctx, a.row.ID, SnapshotIn{
				AccountID: acc.ID, AsOf: dateOf("2025-06-01"), Balance: bal,
			}, a.dek)
			if err == nil {
				t.Errorf("balance %q: want error, got none", bal)
			}
		}
		if n := countSnapshots(t, env.pool, ctx, acc.ID); n != 2 {
			t.Fatalf("rows after invalid writes: %d", n)
		}
	})

	t.Run("scoped per user", func(t *testing.T) {
		accB, err := acct.Create(ctx, b.row.ID, AccountIn{Name: "TFSA", Type: "tfsa", Currency: "CAD"}, b.dek)
		if err != nil {
			t.Fatalf("create b account: %v", err)
		}
		sb, err := snap.CreateSnapshot(ctx, b.row.ID, SnapshotIn{
			AccountID: accB.ID, AsOf: dateOf("2025-02-01"), Balance: "42",
		}, b.dek)
		if err != nil {
			t.Fatalf("create b snapshot: %v", err)
		}
		if sb.Balance != "42.00" {
			t.Fatalf("b balance: %q", sb.Balance)
		}
		if _, err := snap.GetSnapshot(ctx, a.row.ID, sb.ID, a.dek); !errors.Is(err, ErrSnapshotNotFound) {
			t.Errorf("a get b snapshot: want ErrSnapshotNotFound, got %v", err)
		}
		if _, err := snap.UpdateSnapshot(ctx, a.row.ID, sb.ID,
			SnapshotPatch{Balance: strPtr("43")}, a.dek); !errors.Is(err, ErrSnapshotNotFound) {
			t.Errorf("a update b snapshot: want ErrSnapshotNotFound, got %v", err)
		}
		if err := snap.DeleteSnapshot(ctx, a.row.ID, sb.ID); !errors.Is(err, ErrSnapshotNotFound) {
			t.Errorf("a delete b snapshot: want ErrSnapshotNotFound, got %v", err)
		}
		if _, err := snap.CreateSnapshot(ctx, a.row.ID, SnapshotIn{
			AccountID: accB.ID, AsOf: dateOf("2025-02-02"), Balance: "1",
		}, a.dek); !errors.Is(err, ErrAccountNotFound) {
			t.Errorf("a create on b account: want ErrAccountNotFound, got %v", err)
		}
		// b still reads its own snapshot, untouched by a's attempts.
		got, err := snap.GetSnapshot(ctx, b.row.ID, sb.ID, b.dek)
		if err != nil {
			t.Fatalf("b get own snapshot: %v", err)
		}
		if got.Balance != "42.00" {
			t.Fatalf("b snapshot changed: %q", got.Balance)
		}
	})

	t.Run("delete", func(t *testing.T) {
		if err := snap.DeleteSnapshot(ctx, a.row.ID, s0.ID); err != nil {
			t.Fatalf("delete s0: %v", err)
		}
		if _, err := snap.GetSnapshot(ctx, a.row.ID, s0.ID, a.dek); !errors.Is(err, ErrSnapshotNotFound) {
			t.Errorf("get after delete: want ErrSnapshotNotFound, got %v", err)
		}
		if err := snap.DeleteSnapshot(ctx, a.row.ID, s0.ID); !errors.Is(err, ErrSnapshotNotFound) {
			t.Errorf("second delete: want ErrSnapshotNotFound, got %v", err)
		}
	})
}

// wantEquity is one expected EquitySeries point: day in "2006-01-02"
// form, balance and net in cents.
type wantEquity struct {
	day     string
	balance int64
	net     int64
}

// checkEquity compares a whole series value-wise, point by point.
func checkEquity(t *testing.T, got []EquityPoint, want []wantEquity) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("points: want %d, got %d (%+v)", len(want), len(got), got)
	}
	for i := range want {
		if !got[i].AsOf.Equal(dateOf(want[i].day)) {
			t.Errorf("point %d day: want %s, got %v", i, want[i].day, got[i].AsOf)
		}
		if got[i].Balance != want[i].balance {
			t.Errorf("point %d (%s) balance: want %d, got %d", i, want[i].day, want[i].balance, got[i].Balance)
		}
		if got[i].NetContributions != want[i].net {
			t.Errorf("point %d (%s) net: want %d, got %d", i, want[i].day, want[i].net, got[i].NetContributions)
		}
	}
}

// seedEquityRow is one (day, amount) transaction seed for the equity
// tests.
type seedEquityRow struct {
	day, amt string
}

func TestEquitySeriesSnapshotAnchored(t *testing.T) {
	env := newTestEnv(t, "eq-a")
	snap := NewSnapshotStore(env.pool)
	acct := NewAccountStore(env.pool)
	tx := NewTransactionStore(env.pool)
	a := env.user(t, "eq-a")
	ctx := env.ctx

	acc, err := acct.Create(ctx, a.row.ID, AccountIn{Name: "RRSP", Type: "rrsp", Currency: "USD", OpeningBalance: strPtr("1000.00")}, a.dek)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	for _, r := range []seedEquityRow{
		{"2025-02-01", "-200.00"}, {"2025-03-01", "300.00"}, {"2025-05-01", "50.00"},
	} {
		if _, err := tx.Create(ctx, a.row.ID, TransactionIn{
			AccountID: acc.ID, DatePosted: dateOf(r.day), Amount: r.amt, Description: "seed",
		}, a.dek); err != nil {
			t.Fatalf("seed tx %s: %v", r.day, err)
		}
	}
	for _, r := range []seedEquityRow{
		{"2025-01-01", "1500.00"}, {"2025-04-01", "1700.00"},
	} {
		if _, err := snap.CreateSnapshot(ctx, a.row.ID, SnapshotIn{
			AccountID: acc.ID, AsOf: dateOf(r.day), Balance: r.amt,
		}, a.dek); err != nil {
			t.Fatalf("seed snapshot %s: %v", r.day, err)
		}
	}

	// The 2025-01-01 snapshot (1500.00) is the anchor until the
	// 2025-04-01 one (1700.00) supersedes it; net is the lifetime
	// signed sum through each day.
	t.Run("full range", func(t *testing.T) {
		got, err := snap.EquitySeries(ctx, a.row.ID, acc.ID, dateOf("2025-01-01"), dateOf("2025-05-01"))
		if err != nil {
			t.Fatalf("series: %v", err)
		}
		checkEquity(t, got, []wantEquity{
			{"2025-01-01", 150000, 0},
			{"2025-02-01", 130000, -20000},
			{"2025-03-01", 160000, 10000},
			{"2025-04-01", 170000, 10000},
			{"2025-05-01", 175000, 15000},
		})
	})

	t.Run("range mid-stream", func(t *testing.T) {
		got, err := snap.EquitySeries(ctx, a.row.ID, acc.ID, dateOf("2025-03-01"), dateOf("2025-05-01"))
		if err != nil {
			t.Fatalf("series: %v", err)
		}
		// Pre-range transactions still count in net; the 2025-01-01
		// snapshot (before the range) still anchors 03-01.
		checkEquity(t, got, []wantEquity{
			{"2025-03-01", 160000, 10000},
			{"2025-04-01", 170000, 10000},
			{"2025-05-01", 175000, 15000},
		})
	})
}

func TestEquitySeriesNoSnapshots(t *testing.T) {
	env := newTestEnv(t, "eq-b")
	snap := NewSnapshotStore(env.pool)
	acct := NewAccountStore(env.pool)
	tx := NewTransactionStore(env.pool)
	a := env.user(t, "eq-b")
	ctx := env.ctx

	acc, err := acct.Create(ctx, a.row.ID, AccountIn{Name: "Chequing", Type: "bank", Currency: "USD", OpeningBalance: strPtr("1000.00")}, a.dek)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	for _, r := range []seedEquityRow{
		{"2024-06-01", "50.00"}, {"2025-01-15", "30.00"},
	} {
		if _, err := tx.Create(ctx, a.row.ID, TransactionIn{
			AccountID: acc.ID, DatePosted: dateOf(r.day), Amount: r.amt, Description: "seed",
		}, a.dek); err != nil {
			t.Fatalf("seed tx %s: %v", r.day, err)
		}
	}

	got, err := snap.EquitySeries(ctx, a.row.ID, acc.ID, dateOf("2025-01-01"), dateOf("2025-02-01"))
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	// No snapshots: opening + lifetime net. The 2024-06-01 tx is
	// pre-range but still in net; the 2025-02-01 range end has no
	// activity, so it is not a point.
	checkEquity(t, got, []wantEquity{
		{"2025-01-01", 105000, 5000},
		{"2025-01-15", 108000, 8000},
	})
}

func TestEquitySeriesEdges(t *testing.T) {
	env := newTestEnv(t, "ee-a", "ee-b")
	snap := NewSnapshotStore(env.pool)
	acct := NewAccountStore(env.pool)
	a := env.user(t, "ee-a")
	b := env.user(t, "ee-b")
	ctx := env.ctx

	accA, err := acct.Create(ctx, a.row.ID, AccountIn{Name: "RRSP", Type: "rrsp", Currency: "USD", OpeningBalance: strPtr("1000.00")}, a.dek)
	if err != nil {
		t.Fatalf("create a account: %v", err)
	}
	accB, err := acct.Create(ctx, b.row.ID, AccountIn{Name: "TFSA", Type: "tfsa", Currency: "CAD"}, b.dek)
	if err != nil {
		t.Fatalf("create b account: %v", err)
	}

	t.Run("empty account is a single point", func(t *testing.T) {
		got, err := snap.EquitySeries(ctx, a.row.ID, accA.ID, dateOf("2025-01-01"), dateOf("2025-03-01"))
		if err != nil {
			t.Fatalf("series: %v", err)
		}
		checkEquity(t, got, []wantEquity{
			{"2025-01-01", 100000, 0},
		})
	})

	t.Run("foreign and missing accounts", func(t *testing.T) {
		_, err := snap.EquitySeries(ctx, a.row.ID, accB.ID, dateOf("2025-01-01"), dateOf("2025-03-01"))
		if !errors.Is(err, ErrAccountNotFound) {
			t.Errorf("foreign account: want ErrAccountNotFound, got %v", err)
		}
		_, err = snap.EquitySeries(ctx, a.row.ID, "00000000-0000-4000-8000-000000000000", dateOf("2025-01-01"), dateOf("2025-03-01"))
		if !errors.Is(err, ErrAccountNotFound) {
			t.Errorf("missing account: want ErrAccountNotFound, got %v", err)
		}
	})
}

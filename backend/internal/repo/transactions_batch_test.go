// Batch upsert tests (T-P1-3a): the shared postgres:18 testcontainer
// (newTestEnv) plus the import-shape contract the 4.6 runner relies
// on — re-import dedup (all skipped, the first writer's description
// kept), within-file duplicates (first occurrence wins), a 1001-row
// import that crosses the 500-row chunk boundary (its duplicate is
// caught by the next chunk's DB probe, not the within-chunk seen
// map), and the guard rails: a foreign account, a mixed-account
// batch, fields the batch INSERT cannot carry, and the empty batch.
package repo

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// importIn builds one import-shaped row the way the 4.6 runner does:
// date_executed = date_posted, and only AccountID / dates / Amount /
// Description set (CategoryID nil — imports have no category).
func importIn(accountID string, d time.Time, amount, description string) TransactionIn {
	return TransactionIn{
		AccountID:    accountID,
		DateExecuted: &d,
		DatePosted:   d,
		Amount:       amount,
		Description:  description,
	}
}

func findTxByDate(t *testing.T, txs []Transaction, d time.Time) Transaction {
	t.Helper()
	for _, tx := range txs {
		if tx.DatePosted.Equal(d) {
			return tx
		}
	}
	t.Fatalf("no row posted %s", d)
	return Transaction{}
}

func countTx(t *testing.T, pool *pgxpool.Pool, ctx context.Context, userID, accountID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM transactions WHERE user_id = $1 AND account_id = $2`,
		userID, accountID).Scan(&n); err != nil {
		t.Fatalf("count transactions: %v", err)
	}
	return n
}

func TestTransactionStoreUpsertBatch(t *testing.T) {
	env := newTestEnv(t, "batch-a", "batch-b")
	tx := NewTransactionStore(env.pool)
	acct := NewAccountStore(env.pool)
	if tx == nil || acct == nil {
		t.Fatal("non-nil pool must give non-nil stores")
	}
	a := env.user(t, "batch-a")
	b := env.user(t, "batch-b")
	ctx := env.ctx

	aAcct, err := acct.Create(ctx, a.row.ID, AccountIn{Name: "Batch Bank", Type: "bank", Currency: "USD"}, a.dek)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	// A second account of a (the mixed-account guard rail) and one of
	// b (the foreign-account probe).
	aAcct2, err := acct.Create(ctx, a.row.ID, AccountIn{Name: "Batch Bank 2", Type: "bank", Currency: "USD"}, a.dek)
	if err != nil {
		t.Fatalf("create second account: %v", err)
	}
	bAcct, err := acct.Create(ctx, b.row.ID, AccountIn{Name: "Batch Bank B", Type: "bank", Currency: "USD"}, b.dek)
	if err != nil {
		t.Fatalf("create b account: %v", err)
	}

	t.Run("re-import dedups everything", func(t *testing.T) {
		base := []TransactionIn{
			importIn(aAcct.ID, dateOf("2026-03-01"), "10.0", "  Coffee   Shop "),
			importIn(aAcct.ID, dateOf("2026-03-02"), "25", "Grocery Store"),
			importIn(aAcct.ID, dateOf("2026-03-03"), "-5.25", "Refund"),
		}
		landed, err := tx.UpsertBatch(ctx, a.row.ID, base, a.dek)
		if err != nil {
			t.Fatalf("first batch: %v", err)
		}
		if !reflect.DeepEqual(landed, []bool{true, true, true}) {
			t.Fatalf("first batch perRow = %v, want all true", landed)
		}

		// Same identities, re-spelled (case/spacing/amount spelling):
		// all skipped, nothing new.
		again := []TransactionIn{
			importIn(aAcct.ID, dateOf("2026-03-01"), "10.00", "coffee shop"),
			importIn(aAcct.ID, dateOf("2026-03-02"), "25.00", "grocery  store"),
			importIn(aAcct.ID, dateOf("2026-03-03"), "-5.25", "refund"),
		}
		landed, err = tx.UpsertBatch(ctx, a.row.ID, again, a.dek)
		if err != nil {
			t.Fatalf("re-import batch: %v", err)
		}
		if !reflect.DeepEqual(landed, []bool{false, false, false}) {
			t.Fatalf("re-import perRow = %v, want all false", landed)
		}

		// The first writer's raw descriptions survive the re-import.
		got, err := tx.List(ctx, a.row.ID, TransactionFilter{AccountIDs: []string{aAcct.ID}}, a.dek)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("rows = %d, want 3", len(got))
		}
		for _, want := range []struct {
			d    time.Time
			desc string
		}{
			{dateOf("2026-03-01"), "  Coffee   Shop "},
			{dateOf("2026-03-02"), "Grocery Store"},
			{dateOf("2026-03-03"), "Refund"},
		} {
			if got := findTxByDate(t, got, want.d); got.Description != want.desc {
				t.Fatalf("row %s description = %q, want first writer's %q", want.d, got.Description, want.desc)
			}
		}
	})

	t.Run("within-file duplicates first occurrence wins", func(t *testing.T) {
		ins := []TransactionIn{
			importIn(aAcct.ID, dateOf("2026-04-01"), "25.00", "Grocery Store"),
			importIn(aAcct.ID, dateOf("2026-04-02"), "3.5", "Coffee"),
			// Same identity as row 0 (canonical amount, normalized
			// description), different spelling: the first occurrence
			// wins, in input order.
			importIn(aAcct.ID, dateOf("2026-04-01"), "25", "  grocery   STORE "),
		}
		landed, err := tx.UpsertBatch(ctx, a.row.ID, ins, a.dek)
		if err != nil {
			t.Fatalf("batch: %v", err)
		}
		if !reflect.DeepEqual(landed, []bool{true, true, false}) {
			t.Fatalf("perRow = %v, want [true true false]", landed)
		}
		got, err := tx.List(ctx, a.row.ID, TransactionFilter{AccountIDs: []string{aAcct.ID}}, a.dek)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if tx := findTxByDate(t, got, dateOf("2026-04-01")); tx.Description != "Grocery Store" {
			t.Fatalf("duplicate kept description %q, want first writer's %q", tx.Description, "Grocery Store")
		}
	})

	t.Run("1001-row import crosses the chunk boundary", func(t *testing.T) {
		base := dateOf("2026-05-01")
		const n = 1001
		ins := make([]TransactionIn, n)
		for i := 0; i < n; i++ {
			ins[i] = importIn(aAcct.ID, base.AddDate(0, 0, i), fmt.Sprintf("%d.00", i+1), fmt.Sprintf("Row %04d", i))
		}
		// Row 500 re-identifies row 0. It crosses the 500-row chunk
		// boundary, so only the chunk-2 DB probe can catch it (the
		// within-chunk seen map sees the two rows in different chunks).
		ins[500] = importIn(aAcct.ID, base, "1.00", "row 0000")

		landed, err := tx.UpsertBatch(ctx, a.row.ID, ins, a.dek)
		if err != nil {
			t.Fatalf("batch: %v", err)
		}
		if len(landed) != n {
			t.Fatalf("perRow = %d entries, want %d", len(landed), n)
		}
		inserted, skipped := 0, 0
		for i, l := range landed {
			if l {
				inserted++
				continue
			}
			skipped++
			if i != 500 {
				t.Fatalf("perRow[%d] = false, want only row 500 skipped", i)
			}
		}
		if inserted != 1000 || skipped != 1 {
			t.Fatalf("inserted/skipped = %d/%d, want 1000/1", inserted, skipped)
		}

		// offMin/offMax over the LANDED rows only (row 0 landed as the
		// first occurrence, row 500 did not): the window is row 0's
		// date .. row 1000's date.
		offMin, offMax := ins[0].DatePosted, ins[0].DatePosted
		for i, l := range landed {
			if !l {
				continue
			}
			if ins[i].DatePosted.Before(offMin) {
				offMin = ins[i].DatePosted
			}
			if ins[i].DatePosted.After(offMax) {
				offMax = ins[i].DatePosted
			}
		}
		if !offMin.Equal(base) || !offMax.Equal(base.AddDate(0, 0, 1000)) {
			t.Fatalf("offMin/offMax = %s/%s, want %s/%s", offMin, offMax, base, base.AddDate(0, 0, 1000))
		}

		// Re-importing the whole file is a full skip: 0 inserted /
		// 1001 skipped, including the row-500 re-identity.
		landed, err = tx.UpsertBatch(ctx, a.row.ID, ins, a.dek)
		if err != nil {
			t.Fatalf("re-import batch: %v", err)
		}
		for i, l := range landed {
			if l {
				t.Fatalf("re-import perRow[%d] = true, want all false", i)
			}
		}
		// Subtests 1+2 inserted 5 rows into aAcct before this one.
		if n := countTx(t, env.pool, ctx, a.row.ID, aAcct.ID); n != 3+2+1000 {
			t.Fatalf("total rows in aAcct = %d, want %d", n, 3+2+1000)
		}
	})

	t.Run("guard rails", func(t *testing.T) {
		// A foreign account (b's, probed as a's) fails the single
		// account probe exactly like the per-row Upsert does.
		_, err := tx.UpsertBatch(ctx, a.row.ID, []TransactionIn{
			importIn(bAcct.ID, dateOf("2026-06-01"), "1.00", "x"),
		}, a.dek)
		if !errors.Is(err, ErrAccountNotFound) {
			t.Fatalf("foreign account err = %v, want ErrAccountNotFound", err)
		}

		// A mixed batch is a caller bug the single probe cannot cover.
		_, err = tx.UpsertBatch(ctx, a.row.ID, []TransactionIn{
			importIn(aAcct.ID, dateOf("2026-06-01"), "1.00", "x"),
			importIn(aAcct2.ID, dateOf("2026-06-02"), "2.00", "y"),
		}, a.dek)
		if err == nil || !strings.Contains(err.Error(), "share one account_id") {
			t.Fatalf("mixed accounts err = %v, want the single-account error", err)
		}

		// Fields the batch INSERT cannot carry are rejected rather
		// than silently dropped (the import runner never sets them).
		withCategory := importIn(aAcct.ID, dateOf("2026-06-03"), "1.00", "x")
		cid := "00000000-0000-0000-0000-000000000000"
		withCategory.CategoryID = &cid
		if _, err := tx.UpsertBatch(ctx, a.row.ID, []TransactionIn{withCategory}, a.dek); err == nil ||
			!strings.Contains(err.Error(), "category_id") {
			t.Fatalf("category err = %v, want the category rejection", err)
		}
		withNotes := importIn(aAcct.ID, dateOf("2026-06-03"), "1.00", "x")
		notes := "n"
		withNotes.Notes = &notes
		if _, err := tx.UpsertBatch(ctx, a.row.ID, []TransactionIn{withNotes}, a.dek); err == nil ||
			!strings.Contains(err.Error(), "notes") {
			t.Fatalf("notes err = %v, want the notes rejection", err)
		}
		withRaw := importIn(aAcct.ID, dateOf("2026-06-03"), "1.00", "x")
		raw := "r"
		withRaw.Raw = &raw
		if _, err := tx.UpsertBatch(ctx, a.row.ID, []TransactionIn{withRaw}, a.dek); err == nil ||
			!strings.Contains(err.Error(), "raw") {
			t.Fatalf("raw err = %v, want the raw rejection", err)
		}
		withReview := importIn(aAcct.ID, dateOf("2026-06-03"), "1.00", "x")
		withReview.ReviewPending = true
		if _, err := tx.UpsertBatch(ctx, a.row.ID, []TransactionIn{withReview}, a.dek); err == nil ||
			!strings.Contains(err.Error(), "review_pending") {
			t.Fatalf("review_pending err = %v, want the review_pending rejection", err)
		}

		// The shared invariants still apply: a bad amount and a
		// date_executed after date_posted fail exactly like Upsert.
		badAmount := importIn(aAcct.ID, dateOf("2026-06-04"), "", "x")
		if _, err := tx.UpsertBatch(ctx, a.row.ID, []TransactionIn{badAmount}, a.dek); err == nil ||
			!strings.Contains(err.Error(), "amount") {
			t.Fatalf("amount err = %v, want the amount error", err)
		}
		badDate := importIn(aAcct.ID, dateOf("2026-06-05"), "1.00", "x")
		exec := dateOf("2026-06-06")
		badDate.DateExecuted = &exec
		if _, err := tx.UpsertBatch(ctx, a.row.ID, []TransactionIn{badDate}, a.dek); !errors.Is(err, ErrDateRule) {
			t.Fatalf("date rule err = %v, want ErrDateRule", err)
		}

		// An empty batch is a no-op: no error, no rows, and (the
		// runner never calls it with zero rows — but if it did) no
		// account probe either.
		landed, err := tx.UpsertBatch(ctx, a.row.ID, nil, a.dek)
		if err != nil || len(landed) != 0 {
			t.Fatalf("empty batch: perRow=%v err=%v, want empty/nil", landed, err)
		}
	})
}

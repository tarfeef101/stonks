// Summary route tests (plan step 3.2): GET /api/v1/summary against the
// in-memory stores from transactions_test.go. The interesting surface
// is the group union (aggregation groups + active-budget target keys,
// incl. a target-only category and a dangling target key), the per-
// category target summation across multiple active budgets, the sort
// order (name asc, nulls last, id tiebreak), the totals (transaction
// groups only, int64 cents), and the pure period-bounds arithmetic.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"finance-tracker/internal/repo"
)

// memBudgetStore is the in-memory BudgetStore the summary tests run
// against: rows for one owner (the repo scopes by user id) and a list
// error for the 500 path.
type memBudgetStore struct {
	userID  string
	rows    []repo.Budget
	listErr error
}

func (m *memBudgetStore) List(_ context.Context, userID string, _ []byte) ([]repo.Budget, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	if userID != m.userID {
		return []repo.Budget{}, nil
	}
	out := make([]repo.Budget, len(m.rows))
	copy(out, m.rows)
	return out, nil
}

// TestSummary covers the month rollup: the group union (tx groups, a
// target-only category, a dangling target key, uncategorized), target
// summation across the two active month budgets (inactive and other-
// period budgets excluded), the sort order, the totals, and the
// computed from/to bounds. Transactions are seeded at the period
// midpoint so no month length or clock position flips the result.
func TestSummary(t *testing.T) {
	c1, c2, c3, c4 := nextTestID(), nextTestID(), nextTestID(), nextTestID()
	user := "id-sum-a"
	store := &memTxStore{}
	budgets := &memBudgetStore{
		userID: user,
		rows: []repo.Budget{
			{ID: nextTestID(), Name: "active month", Period: "month", Active: true,
				Targets: json.RawMessage(`{"` + c1 + `":100,"` + c2 + `":5.55,"dangling":7,"` + c4 + `":3}`)},
			{ID: nextTestID(), Name: "second month", Period: "month", Active: true,
				Targets: json.RawMessage(`{"` + c1 + `":25}`)},
			{ID: nextTestID(), Name: "inactive month", Period: "month", Active: false,
				Targets: json.RawMessage(`{"` + c1 + `":50}`)},
			{ID: nextTestID(), Name: "active quarter", Period: "quarter", Active: true,
				Targets: json.RawMessage(`{"` + c1 + `":50}`)},
		},
	}
	cats := &memCategoryStore{rows: []memCategoryRow{
		{userID: user, id: c1, name: "Groceries", active: true},
		{userID: user, id: c2, name: "Coffee", active: true},
		{userID: user, id: c3, name: "Zero", active: true},
		{userID: user, id: c4, name: "TargetOnly", active: true},
	}}
	h, key := newTxServer(t, store, cats, budgets)
	tok := mintIdentityToken(t, key, "sum-a", "a@localhost", "A", "a")

	from, _ := periodBounds("month", time.Now())
	mid := dateOnly(from.AddDate(0, 0, 14)) // the 15th: inside every month
	acc := nextTestID()
	seedTx(t, store, user, acc, c1, mid, "-50.25", "groceries big", "")
	seedTx(t, store, user, acc, c1, mid, "-0.75", "groceries small", "")
	seedTx(t, store, user, acc, c1, mid, "10", "refund", "")
	seedTx(t, store, user, acc, c2, mid, "-3.5", "latte", "")
	seedTx(t, store, user, acc, "", mid, "-1.23", "misc spend", "")
	seedTx(t, store, user, acc, "", mid, "5", "misc income", "")
	seedTx(t, store, user, acc, c3, mid, "2.50", "in", "")
	seedTx(t, store, user, acc, c3, mid, "-2.50", "out", "")

	code, out := doReq(t, h, http.MethodGet, "/api/v1/summary", "", authHdr(tok))
	if code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", code, out)
	}
	var resp summaryOut
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("decode: %v (body %s)", err, out)
	}

	if resp.Period != "month" {
		t.Errorf("period = %q, want month", resp.Period)
	}
	wantFrom, wantTo := periodBounds("month", time.Now())
	if resp.From != dateOnly(wantFrom) || resp.To != dateOnly(wantTo) {
		t.Errorf("from/to = %s..%s, want %s..%s", resp.From, resp.To, dateOnly(wantFrom), dateOnly(wantTo))
	}

	// Union order: named groups by name asc (Coffee, Groceries,
	// TargetOnly, Zero), then the unnamed by id asc with the null id
	// (uncategorized) last.
	want := []summaryGroup{
		{CategoryID: &c2, CategoryName: strPtr("Coffee"), Spend: "3.50", Income: "0.00", Net: "-3.50", Target: strPtr("5.55")},
		{CategoryID: &c1, CategoryName: strPtr("Groceries"), Spend: "51.00", Income: "10.00", Net: "-41.00", Target: strPtr("125.00")},
		{CategoryID: &c4, CategoryName: strPtr("TargetOnly"), Spend: "0.00", Income: "0.00", Net: "0.00", Target: strPtr("3.00")},
		{CategoryID: &c3, CategoryName: strPtr("Zero"), Spend: "2.50", Income: "2.50", Net: "0.00"},
		{CategoryID: strPtr("dangling"), Spend: "0.00", Income: "0.00", Net: "0.00", Target: strPtr("7.00")},
		{Spend: "1.23", Income: "5.00", Net: "3.77"},
	}
	if !reflect.DeepEqual(resp.Groups, want) {
		got, _ := json.Marshal(resp.Groups)
		wantJSON, _ := json.Marshal(want)
		t.Errorf("groups = %s, want %s", got, wantJSON)
	}

	// Totals cover the transaction groups only (the target-only groups
	// contribute nothing).
	wantTotals := summaryTotals{Spend: "58.23", Income: "17.50", Net: "-40.73"}
	if resp.Totals != wantTotals {
		t.Errorf("totals = %+v, want %+v", resp.Totals, wantTotals)
	}
}

// TestSummaryPeriods covers the quarter and year bounds with an empty
// period (no tx, no budgets): zeroed totals and an empty group list.
func TestSummaryPeriods(t *testing.T) {
	user := "id-sum-p"
	store := &memTxStore{}
	budgets := &memBudgetStore{userID: user}
	cats := &memCategoryStore{}
	h, key := newTxServer(t, store, cats, budgets)
	tok := mintIdentityToken(t, key, "sum-p", "p@localhost", "P", "p")
	hdr := authHdr(tok)

	for _, period := range []string{"quarter", "year"} {
		t.Run(period, func(t *testing.T) {
			code, out := doReq(t, h, http.MethodGet, "/api/v1/summary?period="+period+"&group_by=category", "", hdr)
			if code != http.StatusOK {
				t.Fatalf("status = %d (body %s)", code, out)
			}
			var resp summaryOut
			if err := json.Unmarshal([]byte(out), &resp); err != nil {
				t.Fatalf("decode: %v (body %s)", err, out)
			}
			if resp.Period != period {
				t.Errorf("period = %q, want %q", resp.Period, period)
			}
			wantFrom, wantTo := periodBounds(period, time.Now())
			if resp.From != dateOnly(wantFrom) || resp.To != dateOnly(wantTo) {
				t.Errorf("from/to = %s..%s, want %s..%s", resp.From, resp.To, dateOnly(wantFrom), dateOnly(wantTo))
			}
			if len(resp.Groups) != 0 {
				t.Errorf("groups = %+v, want none", resp.Groups)
			}
			if resp.Totals != (summaryTotals{Spend: "0.00", Income: "0.00", Net: "0.00"}) {
				t.Errorf("totals = %+v, want all 0.00", resp.Totals)
			}
		})
	}
}

// TestSummaryValidation covers the query validation: the period and
// group_by vocabulary (400) and the default period (month).
func TestSummaryValidation(t *testing.T) {
	store := &memTxStore{}
	budgets := &memBudgetStore{}
	cats := &memCategoryStore{}
	h, key := newTxServer(t, store, cats, budgets)
	tok := mintIdentityToken(t, key, "sum-v", "v@localhost", "V", "v")
	hdr := authHdr(tok)

	for qs, want := range map[string]string{
		"?period=week":       "period must be one of month, quarter, year",
		"?period=Month":      "period must be one of month, quarter, year",
		"?group_by=account":  "group_by must be category",
		"?group_by=Category": "group_by must be category",
	} {
		t.Run(qs, func(t *testing.T) {
			code, out := doReq(t, h, http.MethodGet, "/api/v1/summary"+qs, "", hdr)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", code, out)
			}
			if c, m := apiErr(t, out); c != codeInvalidRequest || m != want {
				t.Errorf("error = %s/%q, want %s/%q", c, m, codeInvalidRequest, want)
			}
		})
	}

	// No period param defaults to month.
	code, out := doReq(t, h, http.MethodGet, "/api/v1/summary", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("default: status = %d (body %s)", code, out)
	}
	var resp summaryOut
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Period != "month" {
		t.Errorf("default period = %q, want month", resp.Period)
	}
}

// TestSummaryErrors covers the store-failure mapping: a nil store is a
// 503, list failures are 500s with their op-specific messages, and an
// undecodable active budget targets blob is a 500.
func TestSummaryErrors(t *testing.T) {
	valid := func(tx TransactionStore, cats CategoryStore, budgets BudgetStore) (http.Handler, map[string]string) {
		t.Helper()
		h, key := newTxServer(t, tx, cats, budgets)
		tok := mintIdentityToken(t, key, "sum-e", "e@localhost", "E", "e")
		return h, authHdr(tok)
	}

	t.Run("nil store is 503", func(t *testing.T) {
		h, key := newTxServer(t, &memTxStore{}, nil, &memBudgetStore{})
		tok := mintIdentityToken(t, key, "sum-e", "e@localhost", "E", "e")
		code, out := doReq(t, h, http.MethodGet, "/api/v1/summary", "", authHdr(tok))
		if code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeInternal || m != "database not configured" {
			t.Errorf("error = %s/%q", c, m)
		}
	})

	t.Run("budget list failure", func(t *testing.T) {
		budgets := &memBudgetStore{listErr: errors.New("boom")}
		h, hdr := valid(&memTxStore{}, &memCategoryStore{}, budgets)
		code, out := doReq(t, h, http.MethodGet, "/api/v1/summary", "", hdr)
		if code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeInternal || m != "budget operation failed" {
			t.Errorf("error = %s/%q", c, m)
		}
	})

	t.Run("category list failure", func(t *testing.T) {
		cats := &memCategoryStore{listErr: errors.New("boom")}
		h, hdr := valid(&memTxStore{}, cats, &memBudgetStore{})
		code, out := doReq(t, h, http.MethodGet, "/api/v1/summary", "", hdr)
		if code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeInternal || m != "category operation failed" {
			t.Errorf("error = %s/%q", c, m)
		}
	})

	t.Run("undecodable targets blob", func(t *testing.T) {
		budgets := &memBudgetStore{
			userID: "id-sum-e",
			rows: []repo.Budget{
				{ID: nextTestID(), Name: "bad blob", Period: "month", Active: true,
					Targets: json.RawMessage(`[1,2]`)},
			},
		}
		h, hdr := valid(&memTxStore{}, &memCategoryStore{}, budgets)
		code, out := doReq(t, h, http.MethodGet, "/api/v1/summary", "", hdr)
		if code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeInternal || m != "budget targets could not be decoded" {
			t.Errorf("error = %s/%q", c, m)
		}
	})
}

// TestPeriodBounds pins the period arithmetic on fixed dates (no clock
// at all): month ends, the quarter start month for every month of the
// year (the 1 + 3*((m-1)/3) block starts), quarter edges, and the year.
func TestPeriodBounds(t *testing.T) {
	pick := func(s string) time.Time {
		d, err := time.Parse("2006-01-02", s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		return d
	}
	day := func(tm time.Time) string { return tm.Format("2006-01-02") }

	cases := []struct {
		period string
		now    string
		from   string
		to     string
	}{
		{"month", "2026-01-15", "2026-01-01", "2026-01-31"},
		{"month", "2026-02-10", "2026-02-01", "2026-02-28"},
		{"month", "2026-02-28", "2026-02-01", "2026-02-28"},
		{"month", "2026-12-31", "2026-12-01", "2026-12-31"},
		{"quarter", "2026-01-01", "2026-01-01", "2026-03-31"},
		{"quarter", "2026-03-31", "2026-01-01", "2026-03-31"},
		{"quarter", "2026-04-01", "2026-04-01", "2026-06-30"},
		{"quarter", "2026-05-20", "2026-04-01", "2026-06-30"},
		{"quarter", "2026-07-15", "2026-07-01", "2026-09-30"},
		{"quarter", "2026-10-01", "2026-10-01", "2026-12-31"},
		{"quarter", "2026-12-31", "2026-10-01", "2026-12-31"},
		{"year", "2026-01-01", "2026-01-01", "2026-12-31"},
		{"year", "2026-07-04", "2026-01-01", "2026-12-31"},
		{"year", "2026-12-31", "2026-01-01", "2026-12-31"},
	}
	for _, tc := range cases {
		from, to := periodBounds(tc.period, pick(tc.now))
		if day(from) != tc.from || day(to) != tc.to {
			t.Errorf("periodBounds(%s, %s) = %s..%s, want %s..%s", tc.period, tc.now, day(from), day(to), tc.from, tc.to)
		}
	}

	// The quarter's start month for every month of the year.
	wantStarts := []time.Month{1, 1, 1, 4, 4, 4, 7, 7, 7, 10, 10, 10}
	for m := time.Month(1); m <= 12; m++ {
		from, _ := periodBounds("quarter", time.Date(2026, m, 15, 12, 0, 0, 0, time.UTC))
		if from.Month() != wantStarts[m-1] {
			t.Errorf("quarter start for month %d = %s, want %s", m, from.Month(), wantStarts[m-1])
		}
	}
}

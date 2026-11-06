// Summary route tests (plan step 3.2): GET /api/v1/summary against the
// in-memory stores from transactions_test.go. The interesting surface
// is the group union (aggregation groups + active-budget target keys,
// incl. a target-only category and a dangling target key), the per-
// category target summation across multiple active budgets, the sort
// order (name asc, nulls last, id tiebreak), the totals (transaction
// groups only, int64 cents), and the pure period-bounds arithmetic.
package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"finance-tracker/internal/repo"
)

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
	budgets := &memBudgetStore{rows: []memBudgetRow{
		{userID: user, b: repo.Budget{ID: nextTestID(), Name: "active month", Period: "month", Active: true,
			Targets: json.RawMessage(`{"` + c1 + `":100,"` + c2 + `":5.55,"dangling":7,"` + c4 + `":3}`)}},
		{userID: user, b: repo.Budget{ID: nextTestID(), Name: "second month", Period: "month", Active: true,
			Targets: json.RawMessage(`{"` + c1 + `":25}`)}},
		{userID: user, b: repo.Budget{ID: nextTestID(), Name: "inactive month", Period: "month", Active: false,
			Targets: json.RawMessage(`{"` + c1 + `":50}`)}},
		{userID: user, b: repo.Budget{ID: nextTestID(), Name: "active quarter", Period: "quarter", Active: true,
			Targets: json.RawMessage(`{"` + c1 + `":50}`)}},
	}}
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

	// Byte-compat (6.2): without trend=N the response must not carry a
	// trend key at all (the nil slice omitempty).
	if resp.Trend != nil {
		t.Errorf("trend = %+v, want nil without the param", resp.Trend)
	}
	if strings.Contains(out, "trend") {
		t.Errorf("body carries a trend key without the param: %s", out)
	}
}

// TestSummaryPeriods covers the quarter and year bounds with an empty
// period (no tx, no budgets): zeroed totals and an empty group list.
func TestSummaryPeriods(t *testing.T) {
	store := &memTxStore{}
	budgets := &memBudgetStore{}
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
		"?period=week":         "period must be one of month, quarter, year",
		"?period=Month":        "period must be one of month, quarter, year",
		"?group_by=account":    "group_by must be category",
		"?group_by=Category":   "group_by must be category",
		"?trend=abc":           "trend must be an integer",
		"?trend=0":             "trend must be between 1 and 24",
		"?trend=25":            "trend must be between 1 and 24",
		"?trend=-1":            "trend must be between 1 and 24",
		"?account_id=notauuid": "account_id must be a valid UUID",
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

// TestSummaryTrend covers the 6.2 trend parameter: month trend=3 with
// fixtures at the 15th of each of the last three months (inside every
// month), expecting the windows oldest first, the last point's bounds
// equal to the top-level from/to, the per-point totals, and the
// recorded store calls carrying the request's account filter. A
// quarter trend=2 pins the window shape across the shift.
func TestSummaryTrend(t *testing.T) {
	c1 := nextTestID()
	user := "id-sum-t"
	store := &memTxStore{}
	cats := &memCategoryStore{rows: []memCategoryRow{{userID: user, id: c1, name: "Groceries", active: true}}}
	h, key := newTxServer(t, store, cats, &memBudgetStore{})
	tok := mintIdentityToken(t, key, "sum-t", "t@localhost", "T", "t")
	hdr := authHdr(tok)

	now := time.Now()
	accA, accB := nextTestID(), nextTestID()
	// The 15th of the month k months before now.
	mid := func(k int) string {
		s := now.AddDate(0, -k, 0)
		y, m, _ := s.Date()
		return dateOnly(time.Date(y, m, 15, 12, 0, 0, 0, time.UTC))
	}
	seedTx(t, store, user, accA, c1, mid(0), "-10.00", "cur spend", "")
	seedTx(t, store, user, accB, c1, mid(0), "+4.00", "cur income", "")
	seedTx(t, store, user, accA, c1, mid(1), "-7.00", "prev spend", "")
	seedTx(t, store, user, accA, c1, mid(2), "-1.50", "older spend", "")

	code, out := doReq(t, h, http.MethodGet, "/api/v1/summary?period=month&trend=3", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", code, out)
	}
	var resp summaryOut
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("decode: %v (body %s)", err, out)
	}
	wantFrom, wantTo := periodBounds("month", time.Now())
	if resp.From != dateOnly(wantFrom) || resp.To != dateOnly(wantTo) {
		t.Fatalf("top-level from/to = %s..%s, want %s..%s", resp.From, resp.To, dateOnly(wantFrom), dateOnly(wantTo))
	}
	if len(resp.Trend) != 3 {
		t.Fatalf("trend len = %d, want 3 (%s)", len(resp.Trend), out)
	}
	// Oldest first: k=2, k=1, k=0. Each window is the calendar month
	// containing now pulled back by k months.
	for i, k := range []int{2, 1, 0} {
		pt := resp.Trend[i]
		pf, ptEnd := periodBounds("month", trendShift("month", now, k))
		if pt.From != dateOnly(pf) || pt.To != dateOnly(ptEnd) {
			t.Errorf("trend[%d] from/to = %s..%s, want %s..%s", i, pt.From, pt.To, dateOnly(pf), dateOnly(ptEnd))
		}
	}
	wantPts := []trendPoint{
		{Spend: "1.50", Income: "0.00", Net: "-1.50"},
		{Spend: "7.00", Income: "0.00", Net: "-7.00"},
		{Spend: "10.00", Income: "4.00", Net: "-6.00"},
	}
	for i := range wantPts {
		if resp.Trend[i].Spend != wantPts[i].Spend || resp.Trend[i].Income != wantPts[i].Income || resp.Trend[i].Net != wantPts[i].Net {
			t.Errorf("trend[%d] amounts = %s/%s/%s, want %s/%s/%s", i,
				resp.Trend[i].Spend, resp.Trend[i].Income, resp.Trend[i].Net,
				wantPts[i].Spend, wantPts[i].Income, wantPts[i].Net)
		}
	}
	// The k=0 point is exactly the top-level window.
	last := resp.Trend[2]
	if last.From != resp.From || last.To != resp.To {
		t.Errorf("last trend point = %s..%s, want the top-level %s..%s", last.From, last.To, resp.From, resp.To)
	}
	if resp.Totals != (summaryTotals{Spend: "10.00", Income: "4.00", Net: "-6.00"}) {
		t.Errorf("totals = %+v, want spend 10.00 / income 4.00 / net -6.00", resp.Totals)
	}
	// The store saw four calls, in request order (the top-level window,
	// then the trend points oldest first: k=2, k=1, k=0). The handler
	// recomputes the k=0 window rather than reusing the top-level
	// result, so the last call's bounds equal the top-level's.
	if len(store.aggregateCalls) != 4 {
		t.Fatalf("aggregate calls = %d, want 4: %+v", len(store.aggregateCalls), store.aggregateCalls)
	}
	top := store.aggregateCalls[0]
	if dateOnly(top.from) != resp.From || dateOnly(top.to) != resp.To {
		t.Errorf("first call bounds = %s..%s, want %s..%s", dateOnly(top.from), dateOnly(top.to), resp.From, resp.To)
	}
	for i, k := range []int{2, 1, 0} {
		c := store.aggregateCalls[1+i]
		pf, ptEnd := periodBounds("month", trendShift("month", now, k))
		if dateOnly(c.from) != dateOnly(pf) || dateOnly(c.to) != dateOnly(ptEnd) {
			t.Errorf("call %d (k=%d) bounds = %s..%s, want %s..%s", 1+i, k,
				dateOnly(c.from), dateOnly(c.to), dateOnly(pf), dateOnly(ptEnd))
		}
	}

	// With an account filter the totals narrow and every call carries
	// the id (top-level and each trend point).
	code, out = doReq(t, h, http.MethodGet, "/api/v1/summary?period=month&trend=2&account_id="+accA, "", hdr)
	if code != http.StatusOK {
		t.Fatalf("filtered: status = %d (body %s)", code, out)
	}
	var filtered summaryOut
	if err := json.Unmarshal([]byte(out), &filtered); err != nil {
		t.Fatalf("decode: %v (body %s)", err, out)
	}
	if filtered.Totals != (summaryTotals{Spend: "10.00", Income: "0.00", Net: "-10.00"}) {
		t.Errorf("filtered totals = %+v, want spend 10.00 / income 0.00 / net -10.00", filtered.Totals)
	}
	if len(filtered.Trend) != 2 {
		t.Fatalf("filtered trend len = %d, want 2 (%s)", len(filtered.Trend), out)
	}
	if filtered.Trend[0].Spend != "7.00" || filtered.Trend[1].Spend != "10.00" {
		t.Errorf("filtered trend spend = %s, %s, want 7.00, 10.00", filtered.Trend[0].Spend, filtered.Trend[1].Spend)
	}
	// The second request adds three calls (top-level plus the two trend
	// points), so the store saw seven in total; every call from index 4
	// on carries the account filter.
	if len(store.aggregateCalls) != 7 {
		t.Fatalf("aggregate calls = %d, want 7: %+v", len(store.aggregateCalls), store.aggregateCalls)
	}
	for i := 4; i < len(store.aggregateCalls); i++ {
		c := store.aggregateCalls[i]
		if len(c.accountIDs) != 1 || c.accountIDs[0] != accA {
			t.Errorf("call %d accountIDs = %+v, want [%s]", i, c.accountIDs, accA)
		}
	}

	t.Run("quarter bounds", func(t *testing.T) {
		h2, key2 := newTxServer(t, &memTxStore{}, &memCategoryStore{}, &memBudgetStore{})
		tok2 := mintIdentityToken(t, key2, "sum-tq", "tq@localhost", "TQ", "tq")
		code, out := doReq(t, h2, http.MethodGet, "/api/v1/summary?period=quarter&trend=2", "", authHdr(tok2))
		if code != http.StatusOK {
			t.Fatalf("status = %d (body %s)", code, out)
		}
		var resp summaryOut
		if err := json.Unmarshal([]byte(out), &resp); err != nil {
			t.Fatalf("decode: %v (body %s)", err, out)
		}
		if len(resp.Trend) != 2 {
			t.Fatalf("trend len = %d, want 2 (%s)", len(resp.Trend), out)
		}
		// Independent previous-quarter window: the 3-month block ending
		// the quarter before the current one (year rollover handled).
		n := time.Now()
		y, m, _ := n.Date()
		p := 1 + 3*((int(m)-1)/3) - 3
		if p < 1 {
			p += 12
			y--
		}
		prevFrom := time.Date(y, time.Month(p), 1, 0, 0, 0, 0, time.UTC)
		prevTo := prevFrom.AddDate(0, 3, -1)
		if resp.Trend[0].From != dateOnly(prevFrom) || resp.Trend[0].To != dateOnly(prevTo) {
			t.Errorf("previous quarter window = %s..%s, want %s..%s", resp.Trend[0].From, resp.Trend[0].To, dateOnly(prevFrom), dateOnly(prevTo))
		}
		if resp.Trend[1].From != resp.From || resp.Trend[1].To != resp.To {
			t.Errorf("current quarter window = %s..%s, want the top-level %s..%s", resp.Trend[1].From, resp.Trend[1].To, resp.From, resp.To)
		}
	})
}

// TestSummaryTrendGroups covers the 6.3 per-window breakdown on the
// trend points: each point's groups equal that window's aggregation
// rows (names joined from the category list, sorted with the shared
// comparator — name asc, nulls last, id tiebreak — so the order can
// differ from the store's own id-asc output), an empty window
// marshals "groups":[] (never null, never absent), and the trend
// array carries no target key anywhere.
func TestSummaryTrendGroups(t *testing.T) {
	c1 := nextTestID()
	c2 := nextTestID()
	c3 := nextTestID()
	user := "id-sum-tg"
	store := &memTxStore{}
	cats := &memCategoryStore{rows: []memCategoryRow{
		{userID: user, id: c1, name: "Beta", active: true},
		{userID: user, id: c2, name: "Alpha", active: true},
		{userID: user, id: c3, name: "Gamma", active: true},
	}}
	h, key := newTxServer(t, store, cats, &memBudgetStore{})
	tok := mintIdentityToken(t, key, "sum-tg", "tg@localhost", "TG", "tg")
	hdr := authHdr(tok)

	now := time.Now()
	acc := nextTestID()
	// mid(k): the 15th of the month k months before now (inside every
	// calendar month, so no month length or clock position flips the
	// windows).
	mid := func(k int) string {
		s := now.AddDate(0, -k, 0)
		y, m, _ := s.Date()
		return dateOnly(time.Date(y, m, 15, 12, 0, 0, 0, time.UTC))
	}
	// Seed only the current (k=0) and previous (k=1) windows, so the
	// k=2 window of a trend=3 request is empty. The two seeded
	// windows' rows differ, and the fake's own output order (id asc,
	// "" first) differs from the handler's (name asc, nulls last):
	// the previous window holds c2 ("Alpha") plus an uncategorized
	// row, the current window holds c1 ("Beta") plus c2.
	seedTx(t, store, user, acc, c2, mid(1), "-5.00", "prev alpha", "")
	seedTx(t, store, user, acc, "", mid(1), "-1.00", "prev uncat", "")
	seedTx(t, store, user, acc, c1, mid(0), "-2.00", "cur beta", "")
	seedTx(t, store, user, acc, c2, mid(0), "-3.00", "cur alpha", "")

	code, out := doReq(t, h, http.MethodGet, "/api/v1/summary?period=month&trend=3", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", code, out)
	}
	var resp summaryOut
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("decode: %v (body %s)", err, out)
	}
	if len(resp.Trend) != 3 {
		t.Fatalf("trend len = %d, want 3 (%s)", len(resp.Trend), out)
	}
	// Point 0 = the empty k=2 window: a non-nil empty groups list.
	if !reflect.DeepEqual(resp.Trend[0].Groups, []trendGroup{}) {
		t.Errorf("trend[0] groups = %+v, want an empty slice", resp.Trend[0].Groups)
	}
	// Point 1 = the previous window: "Alpha" first (name asc), the
	// uncategorized row (null name) last — NOT the fake's id-asc order
	// ("" first).
	want1 := []trendGroup{
		{CategoryID: &c2, CategoryName: strPtr("Alpha"), Spend: "5.00", Income: "0.00", Net: "-5.00"},
		{Spend: "1.00", Income: "0.00", Net: "-1.00"},
	}
	if !reflect.DeepEqual(resp.Trend[1].Groups, want1) {
		t.Errorf("trend[1] groups = %+v, want %+v", resp.Trend[1].Groups, want1)
	}
	// Point 2 = the current window: name asc (Alpha, Beta).
	want2 := []trendGroup{
		{CategoryID: &c2, CategoryName: strPtr("Alpha"), Spend: "3.00", Income: "0.00", Net: "-3.00"},
		{CategoryID: &c1, CategoryName: strPtr("Beta"), Spend: "2.00", Income: "0.00", Net: "-2.00"},
	}
	if !reflect.DeepEqual(resp.Trend[2].Groups, want2) {
		t.Errorf("trend[2] groups = %+v, want %+v", resp.Trend[2].Groups, want2)
	}
	// The current window's trend groups sort exactly like the
	// top-level groups for the same rows (both hold Alpha, Beta).
	nameSeq := func(gs []trendGroup) []string {
		s := make([]string, 0, len(gs))
		for _, g := range gs {
			s = append(s, *g.CategoryName)
		}
		return s
	}
	topSeq := make([]string, 0, len(resp.Groups))
	for _, g := range resp.Groups {
		topSeq = append(topSeq, *g.CategoryName)
	}
	if !reflect.DeepEqual(nameSeq(resp.Trend[2].Groups), topSeq) {
		t.Errorf("trend[2] order %v, want the top-level order %v", nameSeq(resp.Trend[2].Groups), topSeq)
	}
	// Raw-body pins: the empty window marshals [] (never null and
	// never absent), and the trend array (the last marshalled key of
	// the response) carries no target key anywhere.
	if !strings.Contains(out, `"groups":[]`) {
		t.Errorf("body lacks \"groups\":[] for the empty window: %s", out)
	}
	if strings.Contains(out, `"groups":null`) {
		t.Errorf("body carries a null groups: %s", out)
	}
	ti := strings.Index(out, `"trend":`)
	if ti == -1 {
		t.Fatalf("body lacks the trend key: %s", out)
	}
	trendBody := out[ti+len(`"trend":`):]
	if strings.Contains(trendBody, `"target"`) {
		t.Errorf("trend array carries a target key: %s", trendBody)
	}
}

// TestSummaryAccountFilter covers the 6.2 account filter: a valid id
// reaches the store and narrows the totals, an empty value is skipped
// (the unfiltered 200), and an unknown id is a 200 with zeroed totals
// and no groups.
func TestSummaryAccountFilter(t *testing.T) {
	c1 := nextTestID()
	user := "id-sum-af"
	store := &memTxStore{}
	cats := &memCategoryStore{rows: []memCategoryRow{{userID: user, id: c1, name: "Groceries", active: true}}}
	h, key := newTxServer(t, store, cats, &memBudgetStore{})
	tok := mintIdentityToken(t, key, "sum-af", "af@localhost", "AF", "af")
	hdr := authHdr(tok)

	from, _ := periodBounds("month", time.Now())
	mid := dateOnly(from.AddDate(0, 0, 14)) // the 15th: inside every month
	accA, accB := nextTestID(), nextTestID()
	seedTx(t, store, user, accA, c1, mid, "-10.00", "a spend", "")
	seedTx(t, store, user, accB, c1, mid, "+6.00", "b income", "")

	// Valid id: the store saw it, and the totals carry only that
	// account's rows.
	code, out := doReq(t, h, http.MethodGet, "/api/v1/summary?account_id="+accA, "", hdr)
	if code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", code, out)
	}
	var resp summaryOut
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("decode: %v (body %s)", err, out)
	}
	if resp.Totals != (summaryTotals{Spend: "10.00", Income: "0.00", Net: "-10.00"}) {
		t.Errorf("totals = %+v, want spend 10.00 / income 0.00 / net -10.00", resp.Totals)
	}
	if len(store.aggregateCalls) != 1 || len(store.aggregateCalls[0].accountIDs) != 1 || store.aggregateCalls[0].accountIDs[0] != accA {
		t.Fatalf("aggregate calls = %+v, want one call with [%s]", store.aggregateCalls, accA)
	}

	// Empty value: skipped, so the unfiltered totals come back.
	code, out = doReq(t, h, http.MethodGet, "/api/v1/summary?account_id=", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("empty id: status = %d (body %s)", code, out)
	}
	var unfiltered summaryOut
	if err := json.Unmarshal([]byte(out), &unfiltered); err != nil {
		t.Fatalf("decode: %v (body %s)", err, out)
	}
	if unfiltered.Totals != (summaryTotals{Spend: "10.00", Income: "6.00", Net: "-4.00"}) {
		t.Errorf("unfiltered totals = %+v, want spend 10.00 / income 6.00 / net -4.00", unfiltered.Totals)
	}
	if len(store.aggregateCalls) != 2 || store.aggregateCalls[1].accountIDs != nil {
		t.Errorf("calls = %+v, want the second call unfiltered", store.aggregateCalls)
	}

	// Unknown (well-formed) id: 200 with zeroed totals and no groups.
	unknown := nextTestID()
	code, out = doReq(t, h, http.MethodGet, "/api/v1/summary?account_id="+unknown, "", hdr)
	if code != http.StatusOK {
		t.Fatalf("unknown id: status = %d (body %s)", code, out)
	}
	var zeros summaryOut
	if err := json.Unmarshal([]byte(out), &zeros); err != nil {
		t.Fatalf("decode: %v (body %s)", err, out)
	}
	if zeros.Totals != (summaryTotals{Spend: "0.00", Income: "0.00", Net: "0.00"}) {
		t.Errorf("zero totals = %+v, want all 0.00", zeros.Totals)
	}
	if len(zeros.Groups) != 0 {
		t.Errorf("groups = %+v, want none", zeros.Groups)
	}
	if len(store.aggregateCalls) != 3 || len(store.aggregateCalls[2].accountIDs) != 1 || store.aggregateCalls[2].accountIDs[0] != unknown {
		t.Errorf("calls = %+v, want the third call with [%s]", store.aggregateCalls, unknown)
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
		budgets := &memBudgetStore{rows: []memBudgetRow{{
			userID: "id-sum-e",
			b: repo.Budget{ID: nextTestID(), Name: "bad blob", Period: "month", Active: true,
				Targets: json.RawMessage(`[1,2]`)},
		}}}
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

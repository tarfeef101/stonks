// Matching endpoint handler tests (round 25). The engine semantics are
// pinned in internal/mortgage/matching_test.go; these pin the wire: the
// house error order (503 before auth, 404 before the store), the exact
// key order, the canonical 2-decimal strings, and the drift scenario end
// to end (grid payments that drift from 1326.18 to 1463.00 must surface
// exactly one rate-change suggestion and nothing else).
package server

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"finance-tracker/internal/mortgage"
)

// wantMatchingKeys is the exact top-level key order of the matching body.
var wantMatchingKeys = []string{
	"scheduled_payment", "planned_payoff_date", "planned_payoff_index",
	"actual_payoff_date", "last_matched_date", "actual_balance",
	"planned_balance", "periods_matched", "periods_missing",
	"extras", "shorts", "suggestions", "flags", "periods",
}

// wantPeriodKeys is the exact per-row key order of the period table.
var wantPeriodKeys = []string{
	"date", "payment", "extra", "short", "balance", "planned_balance",
}

// matchMortgageBody is the matching test loan's create body: 485000.00 @
// 4.39 percent, 25 yr amortization, semi-monthly, start 2026-01-15, no
// recurring extra — the engine's specA fixture (base payment 1326.18,
// planned payoff 2050-08-22 at index 600).
func matchMortgageBody() string {
	return `{"account_id":"` + mortAcctID + `","principal":"485000.00","annual_rate":"4.39","amortization_years":25,"start_date":"2026-01-15","payment_frequency":"semi_monthly","extra_payment_amount":"0.00","extra_payment_frequency":"monthly","extra_prepayment_type":"principal"}`
}

// matchEngineSpec mirrors matchMortgageBody in engine terms; the tests
// use it to derive the plan grid the transactions are seeded on.
func matchEngineSpec() mortgage.Spec {
	return mortgage.Spec{
		Principal:             485000,
		AnnualRate:            4.39,
		AmortizationYears:     25,
		StartDate:             dateOf("2026-01-15"),
		PaymentFrequency:      "semi_monthly",
		ExtraPaymentFrequency: "monthly",
		ExtraPrepaymentType:   "principal",
	}
}

// createMatchMortgage creates the matching test loan through the POST
// and returns its id.
func createMatchMortgage(t *testing.T, h http.Handler, hdr map[string]string) string {
	t.Helper()
	code, out := doReq(t, h, http.MethodPost, "/api/v1/mortgages", matchMortgageBody(), hdr)
	if code != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201 (body %s)", code, out)
	}
	return idFrom(t, out)
}

// TestMortgageMatchingErrors covers the house error order: a missing
// token is 401 with the authn library's default body (not the envelope)
// and answers BEFORE the store check; a nil mortgage store and a nil
// transaction store are both 503 "database not configured"; a malformed
// id is 404 without touching the store; an unknown well-formed id is
// 404.
func TestMortgageMatchingErrors(t *testing.T) {
	// No token: 401 from the authn middleware's default error handler
	// (body {"error":"invalid_token"}, not the business envelope).
	h0, key0 := newMortgageServer(t, newMortgageAccounts(), nil)
	code, out := doReq(t, h0, http.MethodGet, "/api/v1/mortgages/"+checkingID+"/matching", "", map[string]string{})
	if code != http.StatusUnauthorized {
		t.Fatalf("no-token: status = %d, want 401 (body %s)", code, out)
	}
	if strings.TrimSpace(out) != `{"error":"invalid_token"}` {
		t.Fatalf("no-token body = %s, want {\"error\":\"invalid_token\"} (the authn library's default 401 body)", out)
	}

	// Nil mortgage store: 503 "database not configured" (the
	// notConfigured path; the auth middleware still runs first).
	hdr0 := authHdr(mintIdentityToken(t, key0, "mort-a", "mort-a@localhost", "mort-a", "mort-a"))
	code, out = doReq(t, h0, http.MethodGet, "/api/v1/mortgages/"+checkingID+"/matching", "", hdr0)
	expectAPIError(t, code, out, http.StatusServiceUnavailable, "internal", "database not configured")

	// Nil transaction store: 503 even with a mortgage store configured.
	h1, key1 := newMortgageServer(t, newMortgageAccounts(), &memMortgageStore{})
	hdr1 := authHdr(mintIdentityToken(t, key1, "mort-a", "mort-a@localhost", "mort-a", "mort-a"))
	code, out = doReq(t, h1, http.MethodGet, "/api/v1/mortgages/"+checkingID+"/matching", "", hdr1)
	expectAPIError(t, code, out, http.StatusServiceUnavailable, "internal", "database not configured")

	// The 404 paths need a configured server (both stores wired).
	store := &memMortgageStore{}
	h, key := newMortgageServer(t, newMortgageAccounts(), store, &memTxStore{})
	hdr := authHdr(mintIdentityToken(t, key, "mort-a", "mort-a@localhost", "mort-a", "mort-a"))

	// Malformed id: 404 before the store is touched.
	code, out = doReq(t, h, http.MethodGet, "/api/v1/mortgages/not-a-uuid/matching", "", hdr)
	expectAPIError(t, code, out, http.StatusNotFound, "not_found", "mortgage not found")
	if store.getCalls != 0 {
		t.Fatalf("store get calls = %d, want 0 (malformed id never reaches the store)", store.getCalls)
	}

	// Unknown well-formed id: 404.
	code, out = doReq(t, h, http.MethodGet, "/api/v1/mortgages/99999999-9999-9999-9999-999999999999/matching", "", hdr)
	expectAPIError(t, code, out, http.StatusNotFound, "not_found", "mortgage not found")
	if store.getCalls != 1 {
		t.Fatalf("store get calls = %d, want 1", store.getCalls)
	}
}

// TestMortgageMatchingNoTransactions pins the empty case: a loan whose
// linked account has no debits answers the full shape with zeroed rows,
// the no_transactions flag, the principal as the planned balance, and
// every slice as [] (never null).
func TestMortgageMatchingNoTransactions(t *testing.T) {
	store := &memMortgageStore{}
	txs := &memTxStore{}
	h, key := newMortgageServer(t, newMortgageAccounts(), store, txs)
	hdr := authHdr(mintIdentityToken(t, key, "mort-a", "mort-a@localhost", "mort-a", "mort-a"))
	id := createMatchMortgage(t, h, hdr)

	code, out := doReq(t, h, http.MethodGet, "/api/v1/mortgages/"+id+"/matching", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("get: status = %d, want 200 (body %s)", code, out)
	}
	if !reflect.DeepEqual(jsonKeyOrder(t, out), wantMatchingKeys) {
		t.Fatalf("body keys = %v, want exact order %v", jsonKeyOrder(t, out), wantMatchingKeys)
	}
	var body matchingOut
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if body.ScheduledPayment != "1326.18" {
		t.Errorf("scheduled_payment = %q, want %q", body.ScheduledPayment, "1326.18")
	}
	if body.PlannedPayoffDate != "2050-08-22" || body.PlannedPayoffIndex != 600 {
		t.Errorf("planned payoff = %s/%d, want 2050-08-22/600", body.PlannedPayoffDate, body.PlannedPayoffIndex)
	}
	if body.ActualPayoffDate != nil || body.LastMatchedDate != nil {
		t.Errorf("nullable dates = %v/%v, want both null", body.ActualPayoffDate, body.LastMatchedDate)
	}
	if body.ActualBalance != "485000.00" || body.PlannedBalance != "485000.00" {
		t.Errorf("balances = %s/%s, want 485000.00/485000.00", body.ActualBalance, body.PlannedBalance)
	}
	if body.PeriodsMatched != 0 || body.PeriodsMissing != 0 {
		t.Errorf("matched/missing = %d/%d, want 0/0", body.PeriodsMatched, body.PeriodsMissing)
	}
	if len(body.Extras) != 0 || len(body.Shorts) != 0 || len(body.Suggestions) != 0 || len(body.Periods) != 0 {
		t.Errorf("slices = extras %d, shorts %d, suggestions %d, periods %d; want all empty",
			len(body.Extras), len(body.Shorts), len(body.Suggestions), len(body.Periods))
	}
	if !reflect.DeepEqual(body.Flags, []string{"no_transactions"}) {
		t.Errorf("flags = %v, want [no_transactions]", body.Flags)
	}
	// Slices are [] on the wire, never null.
	for _, k := range []string{"extras", "shorts", "suggestions", "periods"} {
		if !strings.Contains(out, `"`+k+`":[]`) {
			t.Errorf("empty %s must serialize as []: %s", k, out)
		}
	}
}

// TestMortgageMatchingDrift pins the drift scenario end to end: 20 grid
// payments at the scheduled 1326.18 that drift to 1463.00 from the 11th
// period (2026-06-14) surface exactly one rate-change suggestion (implied
// 5.40 percent, effective on the run's first period) and no TOP-LEVEL
// extras, shorts, or flags (the run's per-period deviations live in the
// period table's extra column, not the extras list).
func TestMortgageMatchingDrift(t *testing.T) {
	store := &memMortgageStore{}
	txs := &memTxStore{}
	h, key := newMortgageServer(t, newMortgageAccounts(), store, txs)
	hdr := authHdr(mintIdentityToken(t, key, "mort-a", "mort-a@localhost", "mort-a", "mort-a"))
	id := createMatchMortgage(t, h, hdr)

	sched, err := mortgage.Generate(matchEngineSpec())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for i := 0; i < 20; i++ {
		amt := "-1326.18"
		if i >= 10 {
			amt = "-1463.00"
		}
		seedTx(t, txs, "id-mort-a", mortAcctID, "", sched.Payments[i].Date.Format("2006-01-02"), amt, "mortgage payment", "")
	}

	code, out := doReq(t, h, http.MethodGet, "/api/v1/mortgages/"+id+"/matching", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("get: status = %d, want 200 (body %s)", code, out)
	}
	if !reflect.DeepEqual(jsonKeyOrder(t, out), wantMatchingKeys) {
		t.Fatalf("body keys = %v, want exact order %v", jsonKeyOrder(t, out), wantMatchingKeys)
	}
	// Period-table row key order (the first row verbatim below pins the
	// nulls too; this pins the order).
	rowsRaw := rawJSONValueAt(t, out, "periods")
	first := firstObjectInArray(t, rowsRaw)
	if !reflect.DeepEqual(jsonKeyOrder(t, first), wantPeriodKeys) {
		t.Fatalf("period row keys = %v, want exact order %v", jsonKeyOrder(t, first), wantPeriodKeys)
	}
	var body matchingOut
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}

	if body.ScheduledPayment != "1326.18" {
		t.Errorf("scheduled_payment = %q, want %q", body.ScheduledPayment, "1326.18")
	}
	if body.PlannedPayoffDate != "2050-08-22" || body.PlannedPayoffIndex != 600 {
		t.Errorf("planned payoff = %s/%d, want 2050-08-22/600", body.PlannedPayoffDate, body.PlannedPayoffIndex)
	}
	if body.ActualPayoffDate != nil {
		t.Errorf("actual_payoff_date = %v, want null (not paid off)", body.ActualPayoffDate)
	}
	if body.LastMatchedDate == nil || *body.LastMatchedDate != "2026-10-27" {
		t.Errorf("last_matched_date = %v, want 2026-10-27", body.LastMatchedDate)
	}
	if body.ActualBalance != "474508.09" {
		t.Errorf("actual_balance = %q, want %q", body.ActualBalance, "474508.09")
	}
	if body.PlannedBalance != "475887.46" {
		t.Errorf("planned_balance = %q, want %q", body.PlannedBalance, "475887.46")
	}
	if body.PeriodsMatched != 20 || body.PeriodsMissing != 0 {
		t.Errorf("matched/missing = %d/%d, want 20/0", body.PeriodsMatched, body.PeriodsMissing)
	}
	if len(body.Extras) != 0 || len(body.Shorts) != 0 || len(body.Flags) != 0 {
		t.Errorf("extras/shorts/flags = %v/%v/%v, want all empty (the drift is a suggestion, not amounts)",
			body.Extras, body.Shorts, body.Flags)
	}
	if len(body.Suggestions) != 1 {
		t.Fatalf("suggestions = %v, want exactly 1", body.Suggestions)
	}
	s := body.Suggestions[0]
	if s.EffectiveDate != "2026-06-14" || s.ImpliedAnnualRate != "5.40" || s.ObservedPayment != "1463.00" || s.ObservedPeriods != 10 {
		t.Errorf("suggestion = %+v, want 2026-06-14 / 5.40 / 1463.00 / 10", s)
	}

	// Period table: 20 rows; the first row verbatim (null extra/short);
	// then every drift row shows its per-period +136.82 in the extra
	// column (the table reports the raw deviations; the suggestion
	// column conveys the rate interpretation) and the last row ends at
	// the actual balance.
	if len(body.Periods) != 20 {
		t.Fatalf("len(periods) = %d, want 20", len(body.Periods))
	}
	wantRow0 := `{"date":"2026-01-15","payment":"1326.18","extra":null,"short":null,"balance":"484552.16","planned_balance":"484552.16"}`
	if first != wantRow0 {
		t.Errorf("periods[0] = %s, want verbatim %s", first, wantRow0)
	}
	for i := 10; i < 20; i++ {
		row := body.Periods[i]
		if row.Payment == nil || *row.Payment != "1463.00" || row.Extra == nil || *row.Extra != "136.82" || row.Short != nil {
			t.Errorf("periods[%d] = %+v, want 1463.00 with extra 136.82 and no short", i, row)
		}
	}
	if d := body.Periods[10]; d.Date != "2026-06-14" || d.Balance != "479892.13" || d.PlannedBalance != "480028.94" {
		t.Errorf("periods[10] = %+v, want 2026-06-14 / 479892.13 / 480028.94", d)
	}
	lastRow := body.Periods[19]
	if lastRow.Date != "2026-10-27" || lastRow.Payment == nil || *lastRow.Payment != "1463.00" || lastRow.Balance != "474508.09" {
		t.Errorf("periods[19] = %+v, want 2026-10-27 at 1463.00 ending at 474508.09", lastRow)
	}
}

// TestMortgageMatchingExtrasAndShorts pins the non-sustained deviation
// shape: one off-grid debit (outside the 72h window) and one isolated
// underpayment surface as extras/shorts entries ({date, amount} with
// positive magnitudes) and NO suggestion (a single period is not a run).
func TestMortgageMatchingExtrasAndShorts(t *testing.T) {
	store := &memMortgageStore{}
	txs := &memTxStore{}
	h, key := newMortgageServer(t, newMortgageAccounts(), store, txs)
	hdr := authHdr(mintIdentityToken(t, key, "mort-a", "mort-a@localhost", "mort-a", "mort-a"))
	id := createMatchMortgage(t, h, hdr)

	sched, err := mortgage.Generate(matchEngineSpec())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	// Five grid payments; the 4th (2026-03-01) is 26.18 short.
	amts := []string{"-1326.18", "-1326.18", "-1326.18", "-1300.00", "-1326.18"}
	for i, amt := range amts {
		seedTx(t, txs, "id-mort-a", mortAcctID, "", sched.Payments[i].Date.Format("2006-01-02"), amt, "mortgage payment", "")
	}
	// One off-grid debit: 2026-01-22 is 7 days from the 01-15 plan date
	// and 8 days from 01-30 — outside the 72h matching window.
	seedTx(t, txs, "id-mort-a", mortAcctID, "", "2026-01-22", "-100.00", "lump sum", "")

	code, out := doReq(t, h, http.MethodGet, "/api/v1/mortgages/"+id+"/matching", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("get: status = %d, want 200 (body %s)", code, out)
	}
	var body matchingOut
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if body.PeriodsMatched != 5 || body.PeriodsMissing != 0 {
		t.Errorf("matched/missing = %d/%d, want 5/0", body.PeriodsMatched, body.PeriodsMissing)
	}
	if body.LastMatchedDate == nil || *body.LastMatchedDate != "2026-03-16" {
		t.Errorf("last_matched_date = %v, want 2026-03-16", body.LastMatchedDate)
	}
	if len(body.Suggestions) != 0 || len(body.Flags) != 0 || len(body.Periods) != 5 {
		t.Errorf("suggestions/flags/periods = %d/%d/%d, want 0/0/5", len(body.Suggestions), len(body.Flags), len(body.Periods))
	}
	if len(body.Extras) != 1 {
		t.Fatalf("extras = %v, want exactly 1", body.Extras)
	}
	wantExtra := `{"date":"2026-01-22","amount":"100.00"}`
	if got := firstObjectInArray(t, rawJSONValueAt(t, out, "extras")); got != wantExtra {
		t.Errorf("extras[0] = %s, want verbatim %s", got, wantExtra)
	}
	if len(body.Shorts) != 1 {
		t.Fatalf("shorts = %v, want exactly 1", body.Shorts)
	}
	wantShort := `{"date":"2026-03-01","amount":"26.18"}`
	if got := firstObjectInArray(t, rawJSONValueAt(t, out, "shorts")); got != wantShort {
		t.Errorf("shorts[0] = %s, want verbatim %s", got, wantShort)
	}
}

// TestMortgageMatchingPagination pins the >100-row paging loop: 101 grid
// payments (one page of 100 plus one) all match with no flags, and the
// last matched date is the 101st plan date.
func TestMortgageMatchingPagination(t *testing.T) {
	store := &memMortgageStore{}
	txs := &memTxStore{}
	h, key := newMortgageServer(t, newMortgageAccounts(), store, txs)
	hdr := authHdr(mintIdentityToken(t, key, "mort-a", "mort-a@localhost", "mort-a", "mort-a"))
	id := createMatchMortgage(t, h, hdr)

	sched, err := mortgage.Generate(matchEngineSpec())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for i := 0; i < 101; i++ {
		seedTx(t, txs, "id-mort-a", mortAcctID, "", sched.Payments[i].Date.Format("2006-01-02"), "-1326.18", "mortgage payment", "")
	}

	code, out := doReq(t, h, http.MethodGet, "/api/v1/mortgages/"+id+"/matching", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("get: status = %d, want 200 (body %s)", code, out)
	}
	var body matchingOut
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if body.PeriodsMatched != 101 || body.PeriodsMissing != 0 || len(body.Periods) != 101 {
		t.Errorf("matched/missing/rows = %d/%d/%d, want 101/0/101 (the 100-row page must page again)",
			body.PeriodsMatched, body.PeriodsMissing, len(body.Periods))
	}
	if len(body.Flags) != 0 || len(body.Extras) != 0 || len(body.Shorts) != 0 || len(body.Suggestions) != 0 {
		t.Errorf("flags/extras/shorts/suggestions = %v/%v/%v/%v, want all empty",
			body.Flags, body.Extras, body.Shorts, body.Suggestions)
	}
	wantLast := sched.Payments[100].Date.Format("2006-01-02")
	if body.LastMatchedDate == nil || *body.LastMatchedDate != wantLast {
		t.Errorf("last_matched_date = %v, want %s", body.LastMatchedDate, wantLast)
	}
}

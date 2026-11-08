// Route tests for the 7.3 amortization endpoints:
// GET /api/v1/mortgages/{id}/amortization and
// POST /api/v1/mortgages/{id}/amortization/what-if. House httptest idiom
// exactly as server/mortgages_test.go: the shared memMortgageStore
// (per-op call counters prove the handler answered before reaching the
// store), the 401 unauth and 503 nil-store paths, the
// malformed-id-never-reaches-store rule, and jsonKeyOrder pins for the
// wire field order (the DTO field order is part of the contract). The
// reference loan is the 7.2 engine's REF constants
// (backend/internal/mortgage/mortgage_test.go TestReferenceClosedForm);
// the derived first-payment values are pinned per the 7.3 handoff.
package server

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// REF constants (reference loan: 500000.00 @ 4.99 percent, 25 yr,
// monthly, start 2026-01-15, extra 0.00 monthly, term) -- the parent's
// independent simulation, grep-verified against the 7.2 engine test
// before launch.
const (
	refPayoffDate    = "2050-12-15"
	refPayoffIndex   = 300
	refBasePayment   = "2905.18"
	refTotalInterest = "371553.77"
)

// refMortgageBody is the REF loan's create body (the 7.1 reference
// constants, copied verbatim from the e2e_71 script).
func refMortgageBody() string {
	return `{"account_id":"` + mortAcctID + `","principal":"500000.00","annual_rate":"4.99","amortization_years":25,"start_date":"2026-01-15","payment_frequency":"monthly","extra_payment_amount":"0.00","extra_payment_frequency":"monthly","extra_prepayment_type":"term"}`
}

// createRefMortgage creates the REF loan through the 7.1 POST and
// returns its id.
func createRefMortgage(t *testing.T, h http.Handler, hdr map[string]string) string {
	t.Helper()
	code, out := doReq(t, h, http.MethodPost, "/api/v1/mortgages", refMortgageBody(), hdr)
	if code != http.StatusCreated {
		t.Fatalf("create ref: status = %d, want 201 (body %s)", code, out)
	}
	return idFrom(t, out)
}

// wantScheduleKeys / wantPaymentKeys / wantWhatIfKeys are the pinned
// wire field orders (the DTO struct order is the contract).
var (
	wantScheduleKeys = []string{
		"base_payment", "payoff_date", "payoff_index", "total_paid",
		"total_interest", "payments",
	}
	wantPaymentKeys = []string{
		"date", "payment", "interest", "principal", "extra", "balance",
	}
	wantWhatIfKeys = []string{
		"payoff_date", "total_interest", "months_saved", "interest_saved", "schedule",
	}
)

// rawJSONValueAt returns the raw text of the top-level value for key in
// body (a brace/bracket scan, so key order inside nested objects is
// preserved -- a map decode would lose it).
func rawJSONValueAt(t *testing.T, body, key string) string {
	t.Helper()
	needle := `"` + key + `":`
	i := strings.Index(body, needle)
	if i < 0 {
		t.Fatalf("key %q not found in body: %s", key, body)
	}
	j := i + len(needle)
	for j < len(body) && body[j] == ' ' {
		j++
	}
	if j >= len(body) {
		t.Fatalf("no value after %q: %s", key, body)
	}
	open := body[j]
	if open != '{' && open != '[' {
		t.Fatalf("value for %q does not start with %c: %s", key, open, body)
	}
	depth := 0
	for k := j; k < len(body); k++ {
		switch body[k] {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return body[j : k+1]
			}
		}
	}
	t.Fatalf("unbalanced value for %q: %s", key, body)
	return ""
}

// firstObjectInArray returns the raw text of the first {...} element of
// a JSON array body (brace scan; key order inside is preserved).
func firstObjectInArray(t *testing.T, arr string) string {
	t.Helper()
	i := strings.IndexByte(arr, '{')
	if i < 0 {
		t.Fatalf("no object in array: %s", arr)
	}
	depth := 0
	for k := i; k < len(arr); k++ {
		switch arr[k] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return arr[i : k+1]
			}
		}
	}
	t.Fatalf("unbalanced object in array: %s", arr)
	return ""
}

// TestMortgageAmortizationGet covers the REF loan end to end: 200; the
// exact key order; the REF pins; the derived first-payment values
// (mutually consistent with the engine's round2); the final payment's
// "0.00" balance and payoff date.
func TestMortgageAmortizationGet(t *testing.T) {
	store := &memMortgageStore{}
	h, key := newMortgageServer(t, newMortgageAccounts(), store)
	hdr := authHdr(mintIdentityToken(t, key, "mort-a", "mort-a@localhost", "mort-a", "mort-a"))
	id := createRefMortgage(t, h, hdr)

	code, out := doReq(t, h, http.MethodGet, "/api/v1/mortgages/"+id+"/amortization", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("get: status = %d, want 200 (body %s)", code, out)
	}
	if !reflect.DeepEqual(jsonKeyOrder(t, out), wantScheduleKeys) {
		t.Fatalf("body keys = %v, want exact order %v", jsonKeyOrder(t, out), wantScheduleKeys)
	}
	var sch scheduleOut
	if err := json.Unmarshal([]byte(out), &sch); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if sch.BasePayment != refBasePayment {
		t.Errorf("base_payment = %q, want %q", sch.BasePayment, refBasePayment)
	}
	if sch.PayoffDate != refPayoffDate {
		t.Errorf("payoff_date = %q, want %q", sch.PayoffDate, refPayoffDate)
	}
	if sch.PayoffIndex != refPayoffIndex {
		t.Errorf("payoff_index = %d, want %d", sch.PayoffIndex, refPayoffIndex)
	}
	if sch.TotalInterest != refTotalInterest {
		t.Errorf("total_interest = %q, want %q", sch.TotalInterest, refTotalInterest)
	}
	if len(sch.Payments) != refPayoffIndex {
		t.Fatalf("len(payments) = %d, want %d (the engine's len == payoff_index invariant)", len(sch.Payments), refPayoffIndex)
	}
	// payments[0] verbatim (the derived first-payment values, pinned).
	wantFirst := `{"date":"2026-01-15","payment":"2905.18","interest":"2057.88","principal":"847.30","extra":"0.00","balance":"499152.70"}`
	first := firstObjectInArray(t, rawJSONValueAt(t, out, "payments"))
	if first != wantFirst {
		t.Fatalf("payments[0] = %s, want verbatim %s", first, wantFirst)
	}
	last := sch.Payments[len(sch.Payments)-1]
	if last.Balance != "0.00" {
		t.Errorf("final payment balance = %q, want \"0.00\"", last.Balance)
	}
	if last.Date != refPayoffDate {
		t.Errorf("final payment date = %q, want %q", last.Date, refPayoffDate)
	}
}

// TestMortgageAmortizationGetErrors covers the house error order: a
// malformed id answers 404 WITHOUT reaching the store; an unknown
// well-formed id and another user's row are both 404 (no existence
// disclosure); no token is 401; a nil store is 503.
func TestMortgageAmortizationGetErrors(t *testing.T) {
	store := &memMortgageStore{}
	h, key := newMortgageServer(t, newMortgageAccounts(), store)
	hdr := authHdr(mintIdentityToken(t, key, "mort-a", "mort-a@localhost", "mort-a", "mort-a"))
	hdrB := authHdr(mintIdentityToken(t, key, "mort-b", "mort-b@localhost", "mort-b", "mort-b"))

	// Malformed id: 404 before the store is touched.
	code, out := doReq(t, h, http.MethodGet, "/api/v1/mortgages/not-a-uuid/amortization", "", hdr)
	expectAPIError(t, code, out, http.StatusNotFound, "not_found", "mortgage not found")
	if store.getCalls != 0 {
		t.Fatalf("store get calls = %d, want 0 (malformed id never reaches the store)", store.getCalls)
	}

	// Unknown well-formed id: 404.
	code, out = doReq(t, h, http.MethodGet, "/api/v1/mortgages/99999999-9999-9999-9999-999999999999/amortization", "", hdr)
	expectAPIError(t, code, out, http.StatusNotFound, "not_found", "mortgage not found")

	// A row for user A; user B sees the same 404 (no disclosure).
	id1 := createRefMortgage(t, h, hdr)
	code, out = doReq(t, h, http.MethodGet, "/api/v1/mortgages/"+id1+"/amortization", "", hdrB)
	expectAPIError(t, code, out, http.StatusNotFound, "not_found", "mortgage not found")

	// No token: 401 from the authn middleware, which answers with the
	// auth0 go-jwt-middleware default error handler (RFC 6750: body
	// {"error":"invalid_token"}, a bare WWW-Authenticate challenge) —
	// NOT the business envelope; requireUser's 401 is defensive and is
	// never reached for an unauthenticated request.
	code, out = doReq(t, h, http.MethodGet, "/api/v1/mortgages/"+id1+"/amortization", "", map[string]string{})
	if code != http.StatusUnauthorized {
		t.Fatalf("no-token: status = %d, want 401 (body %s)", code, out)
	}
	if strings.TrimSpace(out) != `{"error":"invalid_token"}` {
		t.Fatalf("no-token body = %s, want {\"error\":\"invalid_token\"} (the authn library's default 401 body)", out)
	}

	// Nil store: 503 "database not configured" (the notConfigured path).
	h503, key503 := newMortgageServer(t, newMortgageAccounts(), nil)
	hdr503 := authHdr(mintIdentityToken(t, key503, "mort-a", "mort-a@localhost", "mort-a", "mort-a"))
	code, out = doReq(t, h503, http.MethodGet, "/api/v1/mortgages/"+id1+"/amortization", "", hdr503)
	expectAPIError(t, code, out, http.StatusServiceUnavailable, "internal", "database not configured")
}

// TestMortgageWhatIfEmpty covers the empty-body rule: POST {} is 400
// "empty what-if: provide at least one field"; the id check runs FIRST
// (malformed id + empty body -> 404, not 400); a JSON null field is
// absent (a pointer nils on null), so all-null is also empty.
func TestMortgageWhatIfEmpty(t *testing.T) {
	store := &memMortgageStore{}
	h, key := newMortgageServer(t, newMortgageAccounts(), store)
	hdr := authHdr(mintIdentityToken(t, key, "mort-a", "mort-a@localhost", "mort-a", "mort-a"))
	id := createRefMortgage(t, h, hdr)

	// Malformed id + empty body: the id check wins (404, not 400).
	code, out := doReq(t, h, http.MethodPost, "/api/v1/mortgages/not-a-uuid/amortization/what-if", `{}`, hdr)
	expectAPIError(t, code, out, http.StatusNotFound, "not_found", "mortgage not found")

	// {}: 400, the exact message.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/mortgages/"+id+"/amortization/what-if", `{}`, hdr)
	expectAPIError(t, code, out, http.StatusBadRequest, "invalid_request", "empty what-if: provide at least one field")

	// All fields present as JSON null: nil pointers = absent = empty.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/mortgages/"+id+"/amortization/what-if",
		`{"annual_rate":null,"extra_payment_amount":null,"extra_payment_frequency":null,"one_time_prepayment":null}`, hdr)
	expectAPIError(t, code, out, http.StatusBadRequest, "invalid_request", "empty what-if: provide at least one field")
}

// TestMortgageWhatIfRateUp covers a pure rate change: the regular
// payment is recomputed for the full original term, so the payoff is
// UNCHANGED (months_saved 0, same payoff date and index) but the
// interest_saved goes NEGATIVE (a higher rate costs more) and the
// scenario's base payment is higher than the REF base payment.
func TestMortgageWhatIfRateUp(t *testing.T) {
	store := &memMortgageStore{}
	h, key := newMortgageServer(t, newMortgageAccounts(), store)
	hdr := authHdr(mintIdentityToken(t, key, "mort-a", "mort-a@localhost", "mort-a", "mort-a"))
	id := createRefMortgage(t, h, hdr)

	code, out := doReq(t, h, http.MethodPost, "/api/v1/mortgages/"+id+"/amortization/what-if",
		`{"annual_rate":"5.99"}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("what-if: status = %d, want 200 (body %s)", code, out)
	}
	var res whatIfOut
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if res.MonthsSaved != 0 {
		t.Errorf("months_saved = %d, want 0 (a rate change keeps the term)", res.MonthsSaved)
	}
	if res.PayoffDate != refPayoffDate {
		t.Errorf("payoff_date = %q, want %q (unchanged by a rate change)", res.PayoffDate, refPayoffDate)
	}
	if res.Schedule.PayoffIndex != refPayoffIndex {
		t.Errorf("schedule.payoff_index = %d, want %d", res.Schedule.PayoffIndex, refPayoffIndex)
	}
	saved, err := strconv.ParseFloat(res.InterestSaved, 64)
	if err != nil {
		t.Fatalf("interest_saved %q is not a number: %v", res.InterestSaved, err)
	}
	if saved >= 0 {
		t.Errorf("interest_saved = %q, want a negative value (a rate increase costs interest)", res.InterestSaved)
	}
	base, err := strconv.ParseFloat(res.Schedule.BasePayment, 64)
	if err != nil {
		t.Fatalf("schedule.base_payment %q is not a number: %v", res.Schedule.BasePayment, err)
	}
	ref, _ := strconv.ParseFloat(refBasePayment, 64)
	if base <= ref {
		t.Errorf("schedule.base_payment = %q, want > %q (recomputed at the higher rate)", res.Schedule.BasePayment, refBasePayment)
	}
}

// TestMortgageWhatIfOneTime covers a one-time prepayment applied mid
// term: it shortens the schedule (months_saved > 0, an EARLIER payoff
// date in string order) and saves interest (interest_saved > 0).
func TestMortgageWhatIfOneTime(t *testing.T) {
	store := &memMortgageStore{}
	h, key := newMortgageServer(t, newMortgageAccounts(), store)
	hdr := authHdr(mintIdentityToken(t, key, "mort-a", "mort-a@localhost", "mort-a", "mort-a"))
	id := createRefMortgage(t, h, hdr)

	code, out := doReq(t, h, http.MethodPost, "/api/v1/mortgages/"+id+"/amortization/what-if",
		`{"one_time_prepayment":{"amount":"5000.00","date":"2026-12-15"}}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("what-if: status = %d, want 200 (body %s)", code, out)
	}
	var res whatIfOut
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if res.MonthsSaved <= 0 {
		t.Errorf("months_saved = %d, want > 0 (a prepayment shortens the term)", res.MonthsSaved)
	}
	if res.PayoffDate >= refPayoffDate {
		t.Errorf("payoff_date = %q, want earlier than %q", res.PayoffDate, refPayoffDate)
	}
	saved, err := strconv.ParseFloat(res.InterestSaved, 64)
	if err != nil {
		t.Fatalf("interest_saved %q is not a number: %v", res.InterestSaved, err)
	}
	if saved <= 0 {
		t.Errorf("interest_saved = %q, want > 0 (a prepayment saves interest)", res.InterestSaved)
	}
}

// TestMortgageWhatIfRecurringExtra covers a recurring extra (the base
// spec has none): it shortens the schedule and saves interest.
func TestMortgageWhatIfRecurringExtra(t *testing.T) {
	store := &memMortgageStore{}
	h, key := newMortgageServer(t, newMortgageAccounts(), store)
	hdr := authHdr(mintIdentityToken(t, key, "mort-a", "mort-a@localhost", "mort-a", "mort-a"))
	id := createRefMortgage(t, h, hdr)

	code, out := doReq(t, h, http.MethodPost, "/api/v1/mortgages/"+id+"/amortization/what-if",
		`{"extra_payment_amount":"500.00","extra_payment_frequency":"monthly"}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("what-if: status = %d, want 200 (body %s)", code, out)
	}
	var res whatIfOut
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if res.MonthsSaved <= 0 {
		t.Errorf("months_saved = %d, want > 0 (a recurring extra shortens the term)", res.MonthsSaved)
	}
	saved, err := strconv.ParseFloat(res.InterestSaved, 64)
	if err != nil {
		t.Fatalf("interest_saved %q is not a number: %v", res.InterestSaved, err)
	}
	if saved <= 0 {
		t.Errorf("interest_saved = %q, want > 0", res.InterestSaved)
	}
}

// TestMortgageWhatIfPrepaymentAfterPayoff covers the one engine runtime
// error with a 400 meaning: a valid future date that lands after the
// payoff is rejected with the engine's EXACT text.
func TestMortgageWhatIfPrepaymentAfterPayoff(t *testing.T) {
	store := &memMortgageStore{}
	h, key := newMortgageServer(t, newMortgageAccounts(), store)
	hdr := authHdr(mintIdentityToken(t, key, "mort-a", "mort-a@localhost", "mort-a", "mort-a"))
	id := createRefMortgage(t, h, hdr)

	code, out := doReq(t, h, http.MethodPost, "/api/v1/mortgages/"+id+"/amortization/what-if",
		`{"one_time_prepayment":{"amount":"500.00","date":"2060-01-01"}}`, hdr)
	expectAPIError(t, code, out, http.StatusBadRequest, "invalid_request",
		"one-time prepayment date is after the payoff date")
}

// TestMortgageWhatIfValidation covers each present-field message EXACTLY
// as pinned (absent fields raise nothing), plus the "; "-join when
// several fields fail at once (wire order: annual_rate first, then
// one_time_prepayment).
func TestMortgageWhatIfValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"rate_not_a_number", `{"annual_rate":"abc"}`, "annual_rate must be a number"},
		{"rate_zero", `{"annual_rate":"0"}`, "annual_rate must be greater than 0 and at most 100"},
		{"rate_above_100", `{"annual_rate":"100.01"}`, "annual_rate must be greater than 0 and at most 100"},
		{"extra_too_many_decimals", `{"extra_payment_amount":"1.234"}`, `amount "1.234" has more than 2 decimal places`},
		{"extra_negative", `{"extra_payment_amount":"-5.00"}`, "extra_payment_amount must not be negative"},
		{"frequency_bad", `{"extra_payment_frequency":"weekly"}`, "extra_payment_frequency must be monthly, quarterly, or yearly"},
		{"prepayment_missing_date", `{"one_time_prepayment":{"amount":"500.00"}}`, "one_time_prepayment.date is required"},
		{"prepayment_amount_zero", `{"one_time_prepayment":{"amount":"0.00","date":"2026-12-15"}}`, "one_time_prepayment.amount must be greater than zero"},
		{"prepayment_bad_date", `{"one_time_prepayment":{"amount":"500.00","date":"2026-13-01"}}`, "one_time_prepayment.date must be a date in YYYY-MM-DD form"},
		{"combined_rate_and_date", `{"annual_rate":"abc","one_time_prepayment":{"amount":"500.00","date":"nope"}}`,
			"annual_rate must be a number; one_time_prepayment.date must be a date in YYYY-MM-DD form"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &memMortgageStore{}
			h, key := newMortgageServer(t, newMortgageAccounts(), store)
			hdr := authHdr(mintIdentityToken(t, key, "mort-a", "mort-a@localhost", "mort-a", "mort-a"))
			id := createRefMortgage(t, h, hdr)
			code, out := doReq(t, h, http.MethodPost, "/api/v1/mortgages/"+id+"/amortization/what-if", tc.body, hdr)
			expectAPIError(t, code, out, http.StatusBadRequest, "invalid_request", tc.want)
		})
	}
}

// TestMortgageAmortizationKeyOrder pins the wire field order of all
// three response DTOs (scheduleOut on the GET, paymentOut inside its
// payments array, whatIfOut on the POST, and the nested scheduleOut
// inside the what-if body).
func TestMortgageAmortizationKeyOrder(t *testing.T) {
	store := &memMortgageStore{}
	h, key := newMortgageServer(t, newMortgageAccounts(), store)
	hdr := authHdr(mintIdentityToken(t, key, "mort-a", "mort-a@localhost", "mort-a", "mort-a"))
	id := createRefMortgage(t, h, hdr)

	// GET: scheduleOut top level + payments[0] object.
	code, out := doReq(t, h, http.MethodGet, "/api/v1/mortgages/"+id+"/amortization", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("get: status = %d, want 200 (body %s)", code, out)
	}
	if !reflect.DeepEqual(jsonKeyOrder(t, out), wantScheduleKeys) {
		t.Fatalf("schedule keys = %v, want exact order %v", jsonKeyOrder(t, out), wantScheduleKeys)
	}
	first := firstObjectInArray(t, rawJSONValueAt(t, out, "payments"))
	if !reflect.DeepEqual(jsonKeyOrder(t, first), wantPaymentKeys) {
		t.Fatalf("payment keys = %v, want exact order %v", jsonKeyOrder(t, first), wantPaymentKeys)
	}

	// POST: whatIfOut top level + the nested schedule object.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/mortgages/"+id+"/amortization/what-if",
		`{"annual_rate":"5.99"}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("what-if: status = %d, want 200 (body %s)", code, out)
	}
	if !reflect.DeepEqual(jsonKeyOrder(t, out), wantWhatIfKeys) {
		t.Fatalf("what-if keys = %v, want exact order %v", jsonKeyOrder(t, out), wantWhatIfKeys)
	}
	nested := rawJSONValueAt(t, out, "schedule")
	if !reflect.DeepEqual(jsonKeyOrder(t, nested), wantScheduleKeys) {
		t.Fatalf("nested schedule keys = %v, want exact order %v", jsonKeyOrder(t, nested), wantScheduleKeys)
	}
}

// Route tests for the mortgage endpoints (plan step 7.1):
// GET/POST /api/v1/mortgages, GET/PATCH/DELETE /api/v1/mortgages/{id}.
// All in-memory: memMortgageStore mirrors the repo.MortgageStore
// semantics the handlers depend on (user-id scoping — a foreign or
// unknown id is ErrMortgageNotFound, no existence disclosure; Create
// assigns the id and stores the spec verbatim; Update applies the
// non-nil patch fields; Delete removes the row) and injects per-operation
// errors for the mapping table plus per-op call counters so a test can
// prove the handler answered BEFORE touching the store. Accounts come
// from the shared memAccountStore (accounts_test.go). The shared helpers
// (authHdr, apiErr, idFrom, nextTestID, doReq) live in accounts_test.go
// and settings_test.go.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"finance-tracker/internal/repo"

	"github.com/lestrrat-go/jwx/v3/jwk"
)

// Fixed well-formed account ids for the fixtures (shape-checked, so the
// account existence/type path is exercised, not the UUID-shape 400).
const (
	mortAcctID = "11111111-1111-1111-1111-111111111111"
	checkingID = "22222222-2222-2222-2222-222222222222"
)

// newMortgageAccounts is the shared account fixture: user "mort-a" owns
// a mortgage account and a checking account (the wrong-type case).
func newMortgageAccounts() *memAccountStore {
	return &memAccountStore{rows: []memAccountRow{
		// userID is the users row id ("id-" + sub, what requireUser passes
		// to the stores), not the sub itself (imports_test.go precedent).
		{userID: "id-mort-a", id: mortAcctID, name: "Home", typ: "mortgage", currency: "USD"},
		{userID: "id-mort-a", id: checkingID, name: "Checking", typ: "checking", currency: "USD"},
	}}
}

// memMortgageRow is one fake-store row.
type memMortgageRow struct {
	userID string
	m      repo.Mortgage
}

// memMortgageStore mirrors the repo.MortgageStore semantics the handlers
// depend on. The *Err fields inject errors for the handler's error
// mapping (real failures need a DB); the *Calls counters let a test
// assert the handler answered before reaching the store.
type memMortgageStore struct {
	rows []memMortgageRow

	listErr   error
	getErr    error
	createErr error
	updateErr error
	deleteErr error

	listCalls   int
	getCalls    int
	createCalls int
	updateCalls int
	deleteCalls int
}

func (m *memMortgageStore) List(_ context.Context, userID string, _ []byte) ([]repo.Mortgage, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	m.listCalls++
	out := []repo.Mortgage{}
	for _, r := range m.rows {
		if r.userID == userID {
			out = append(out, r.m)
		}
	}
	return out, nil // insertion order = the repo's ORDER BY created_at, id
}

func (m *memMortgageStore) Get(_ context.Context, userID, id string, _ []byte) (repo.Mortgage, error) {
	if m.getErr != nil {
		return repo.Mortgage{}, m.getErr
	}
	m.getCalls++
	for _, r := range m.rows {
		if r.userID == userID && r.m.ID == id {
			return r.m, nil
		}
	}
	return repo.Mortgage{}, repo.ErrMortgageNotFound
}

func (m *memMortgageStore) Create(_ context.Context, userID string, in repo.MortgageIn, _ []byte) (repo.Mortgage, error) {
	if m.createErr != nil {
		return repo.Mortgage{}, m.createErr
	}
	m.createCalls++
	mg := repo.Mortgage{
		ID:        nextTestID(),
		UserID:    userID,
		AccountID: in.AccountID,
		Spec: repo.MortgageSpec{
			Principal:             in.Principal,
			AnnualRate:            in.AnnualRate,
			AmortizationYears:     in.AmortizationYears,
			StartDate:             in.StartDate,
			PaymentFrequency:      in.PaymentFrequency,
			ExtraPaymentAmount:    in.ExtraPaymentAmount,
			ExtraPaymentFrequency: in.ExtraPaymentFrequency,
			ExtraPrepaymentType:   in.ExtraPrepaymentType,
			TermYears:             in.TermYears,
			RateType:              in.RateType,
		},
	}
	m.rows = append(m.rows, memMortgageRow{userID: userID, m: mg})
	return mg, nil
}

func (m *memMortgageStore) Update(_ context.Context, userID, id string, p repo.MortgagePatch, _ []byte) (repo.Mortgage, error) {
	if m.updateErr != nil {
		return repo.Mortgage{}, m.updateErr
	}
	m.updateCalls++
	for i, r := range m.rows {
		if r.userID != userID || r.m.ID != id {
			continue
		}
		if p.Principal != nil {
			r.m.Spec.Principal = *p.Principal
		}
		if p.AnnualRate != nil {
			r.m.Spec.AnnualRate = *p.AnnualRate
		}
		if p.AmortizationYears != nil {
			r.m.Spec.AmortizationYears = *p.AmortizationYears
		}
		if p.StartDate != nil {
			r.m.Spec.StartDate = *p.StartDate
		}
		if p.PaymentFrequency != nil {
			r.m.Spec.PaymentFrequency = *p.PaymentFrequency
		}
		if p.ExtraPaymentAmount != nil {
			r.m.Spec.ExtraPaymentAmount = *p.ExtraPaymentAmount
		}
		if p.ExtraPaymentFrequency != nil {
			r.m.Spec.ExtraPaymentFrequency = *p.ExtraPaymentFrequency
		}
		if p.ExtraPrepaymentType != nil {
			r.m.Spec.ExtraPrepaymentType = *p.ExtraPrepaymentType
		}
		if p.TermYears != nil {
			r.m.Spec.TermYears = *p.TermYears
		}
		if p.RateType != nil {
			r.m.Spec.RateType = *p.RateType
		}
		if p.RateEvents != nil {
			r.m.Spec.RateEvents = p.RateEvents
		}
		if p.PaymentClassifications != nil {
			r.m.Spec.PaymentClassifications = p.PaymentClassifications
		}
		m.rows[i].m = r.m
		return r.m, nil
	}
	return repo.Mortgage{}, repo.ErrMortgageNotFound
}

func (m *memMortgageStore) Delete(_ context.Context, userID, id string) error {
	if m.deleteErr != nil {
		return m.deleteErr
	}
	m.deleteCalls++
	for i, r := range m.rows {
		if r.userID == userID && r.m.ID == id {
			m.rows = append(m.rows[:i], m.rows[i+1:]...)
			return nil
		}
	}
	return repo.ErrMortgageNotFound
}

// newMortgageServer builds a full router with a live token verifier, an
// in-memory bootstrap, the given account store, and the given mortgage
// store (nil allowed — that is the 503 path). The optional transaction
// store (round 25 matching) is nil when omitted — that is its 503 path.
// The key is returned so tests can mint tokens for several users under
// the same verifier.
func newMortgageServer(t *testing.T, accounts AccountStore, store MortgageStore, txs ...*memTxStore) (http.Handler, jwk.Key) {
	t.Helper()
	key := mintKey(t)
	b, err := NewBootstrap(newMemStore(), testKEK(t), nil)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	var txStore TransactionStore
	if len(txs) > 0 {
		txStore = txs[0]
	}
	return New(fakeReady{}, BuildInfo{}, liveAuth(t, key), b, nil, accounts, nil, txStore, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, store, nil, false, nil, nil, nil), key
}

// baseMortgageBody is the full 9-field create body with every value
// valid (the canonical-form assertions in the CRUD test key off it).
func baseMortgageBody() string {
	return `{"account_id":"` + mortAcctID + `","principal":"450000","annual_rate":"4.99","amortization_years":30,"start_date":"2020-06-01","payment_frequency":"monthly","extra_payment_amount":"250","extra_payment_frequency":"monthly","extra_prepayment_type":"term"}`
}

// withoutField drops one field from a JSON object body (the
// all-9-required subtests key off the base body).
func withoutField(t *testing.T, body, field string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("unmarshal base body: %v", err)
	}
	delete(m, field)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("remarshal: %v", err)
	}
	return string(out)
}

// withField sets one field in a JSON object body (the per-field
// validation subtests key off the base body).
func withField(t *testing.T, body, field string, value any) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("unmarshal base body: %v", err)
	}
	m[field] = value
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("remarshal: %v", err)
	}
	return string(out)
}

// wantMortgageKeys is the pinned wire field order (the DTO field order
// is part of the contract; a map decode would lose it).
var wantMortgageKeys = []string{
	"id", "account_id", "principal", "annual_rate", "amortization_years",
	"start_date", "payment_frequency", "extra_payment_amount",
	"extra_payment_frequency", "extra_prepayment_type",
	"term_years", "rate_type", "rate_events",
}

// jsonKeyOrder returns the top-level keys of a JSON object body in wire
// order (a json.Decoder token walk, not a map decode).
func jsonKeyOrder(t *testing.T, body string) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(body))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		t.Fatalf("body is not a JSON object: %v (%s)", err, body)
	}
	var keys []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			t.Fatalf("read key: %v", err)
		}
		key, _ := k.(string)
		keys = append(keys, key)
		var sink any
		if err := dec.Decode(&sink); err != nil {
			t.Fatalf("read value: %v", err)
		}
	}
	return keys
}

// expectAPIError asserts the response is the pinned error envelope.
func expectAPIError(t *testing.T, code int, out string, wantStatus int, wantCode, wantMsg string) {
	t.Helper()
	if code != wantStatus {
		t.Fatalf("status = %d, want %d (body %s)", code, wantStatus, out)
	}
	ec, em := apiErr(t, out)
	if ec != wantCode || em != wantMsg {
		t.Fatalf("envelope = %q %q, want %q %q", ec, em, wantCode, wantMsg)
	}
}

// TestMortgagesCRUD covers the happy path end to end: fresh list (body
// exactly []), create (201, the EXACT wire field order, canonical
// 2-decimal amounts, no bookkeeping timestamps), list in create order,
// get, patch (single field), delete (204, empty body, 404s afterwards).
func TestMortgagesCRUD(t *testing.T) {
	store := &memMortgageStore{}
	h, key := newMortgageServer(t, newMortgageAccounts(), store)
	hdr := authHdr(mintIdentityToken(t, key, "mort-a", "mort-a@localhost", "mort-a", "mort-a"))

	// Fresh user: the list body is exactly [] (not null).
	code, out := doReq(t, h, http.MethodGet, "/api/v1/mortgages", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("list: status = %d, want 200 (body %s)", code, out)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("fresh list body = %q, want exactly []", out)
	}

	// Create: 201 with the EXACT wire field order and canonical values.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/mortgages", baseMortgageBody(), hdr)
	if code != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201 (body %s)", code, out)
	}
	keys := jsonKeyOrder(t, out)
	if !reflect.DeepEqual(keys, wantMortgageKeys) {
		t.Fatalf("create body keys = %v, want exact order %v", keys, wantMortgageKeys)
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(out), &row); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if row["id"] == "" {
		t.Error("body has no id")
	}
	if row["account_id"] != mortAcctID {
		t.Errorf("account_id = %v, want %s", row["account_id"], mortAcctID)
	}
	if row["principal"] != "450000.00" {
		t.Errorf("principal = %v, want canonical \"450000.00\"", row["principal"])
	}
	if row["annual_rate"] != "4.99" {
		t.Errorf("annual_rate = %v, want percent string \"4.99\"", row["annual_rate"])
	}
	if row["amortization_years"] != float64(30) {
		t.Errorf("amortization_years = %v, want integer 30", row["amortization_years"])
	}
	if row["start_date"] != "2020-06-01" {
		t.Errorf("start_date = %v, want \"2020-06-01\"", row["start_date"])
	}
	if row["payment_frequency"] != "monthly" {
		t.Errorf("payment_frequency = %v, want \"monthly\"", row["payment_frequency"])
	}
	if row["extra_payment_amount"] != "250.00" {
		t.Errorf("extra_payment_amount = %v, want canonical \"250.00\"", row["extra_payment_amount"])
	}
	if row["extra_payment_frequency"] != "monthly" {
		t.Errorf("extra_payment_frequency = %v, want \"monthly\"", row["extra_payment_frequency"])
	}
	if row["extra_prepayment_type"] != "term" {
		t.Errorf("extra_prepayment_type = %v, want \"term\"", row["extra_prepayment_type"])
	}
	if row["term_years"] != float64(0) {
		t.Errorf("term_years = %v, want 0 (the create default)", row["term_years"])
	}
	if row["rate_type"] != "fixed" {
		t.Errorf("rate_type = %v, want \"fixed\" (the create default)", row["rate_type"])
	}
	if !strings.Contains(out, `"rate_events":[]`) {
		t.Errorf("create body = %s, want rate_events exactly [] (no events at create)", out)
	}
	if _, ok := row["created_at"]; ok {
		t.Error("body carries created_at (bookkeeping stays server-side)")
	}
	if _, ok := row["updated_at"]; ok {
		t.Error("body carries updated_at (bookkeeping stays server-side)")
	}
	id1 := idFrom(t, out)

	// Second create (different values): create order.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/mortgages",
		withField(t, withField(t, withField(t, withField(t, withField(t, baseMortgageBody(), "principal", "200000"), "amortization_years", 15), "payment_frequency", "semi_monthly"), "term_years", 5), "rate_type", "variable"), hdr)
	if code != http.StatusCreated {
		t.Fatalf("create 2: status = %d, want 201 (body %s)", code, out)
	}
	id2 := idFrom(t, out)

	// List: create order.
	code, out = doReq(t, h, http.MethodGet, "/api/v1/mortgages", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("list: status = %d, want 200 (body %s)", code, out)
	}
	var list []mortgageOut
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if len(list) != 2 {
		t.Fatalf("list length = %d, want 2 (%s)", len(list), out)
	}
	if list[0].ID != id1 || list[1].ID != id2 {
		t.Errorf("list = [%s, %s], want [%s, %s] in create order", list[0].ID, list[1].ID, id1, id2)
	}
	if list[1].Principal != "200000.00" || list[1].AmortizationYears != 15 || list[1].PaymentFrequency != "semi_monthly" ||
		list[1].TermYears != 5 || list[1].RateType != "variable" {
		t.Errorf("second row = %+v, want the patched-in values", list[1])
	}
	ids := []string{list[0].ID, list[1].ID}
	if ids[0] != id1 || ids[1] != id2 {
		t.Errorf("list ids = %v, want [%s %s]", ids, id1, id2)
	}

	// Get by id.
	code, out = doReq(t, h, http.MethodGet, "/api/v1/mortgages/"+id1, "", hdr)
	if code != http.StatusOK {
		t.Fatalf("get: status = %d, want 200 (body %s)", code, out)
	}
	if idFrom(t, out) != id1 {
		t.Errorf("get id = %s, want %s", idFrom(t, out), id1)
	}

	// Patch: a single field, the rest untouched.
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/mortgages/"+id1, `{"annual_rate":"5.25"}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("patch: status = %d, want 200 (body %s)", code, out)
	}
	row = nil
	if err := json.Unmarshal([]byte(out), &row); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if row["annual_rate"] != "5.25" {
		t.Errorf("patched annual_rate = %v, want \"5.25\"", row["annual_rate"])
	}
	if row["principal"] != "450000.00" || row["amortization_years"] != float64(30) {
		t.Errorf("patch left other fields alone: principal = %v, years = %v", row["principal"], row["amortization_years"])
	}
	if !reflect.DeepEqual(jsonKeyOrder(t, out), wantMortgageKeys) {
		t.Errorf("patch body keys = %v, want the same exact order", jsonKeyOrder(t, out))
	}

	// Delete: 204, empty body; the row is gone afterwards.
	code, out = doReq(t, h, http.MethodDelete, "/api/v1/mortgages/"+id1, "", hdr)
	if code != http.StatusNoContent {
		t.Fatalf("delete: status = %d, want 204 (body %s)", code, out)
	}
	if out != "" {
		t.Errorf("delete body = %q, want empty", out)
	}
	code, out = doReq(t, h, http.MethodGet, "/api/v1/mortgages/"+id1, "", hdr)
	expectAPIError(t, code, out, http.StatusNotFound, "not_found", "mortgage not found")

	// List: only the second row remains.
	code, out = doReq(t, h, http.MethodGet, "/api/v1/mortgages", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("list after delete: status = %d, want 200 (body %s)", code, out)
	}
	list = nil
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if len(list) != 1 || list[0].ID != id2 {
		t.Errorf("list after delete = %+v, want only %s", list, id2)
	}
}

// TestMortgagesCreateIgnoresEvents pins the round-23 create rule: rate
// events are never settable on a create (they are recorded over time —
// via the PATCH full-array replacement, or the round-25 matcher — not
// invented at create), so a client-sent rate_events is silently ignored
// and the created row is served with exactly [].
func TestMortgagesCreateIgnoresEvents(t *testing.T) {
	store := &memMortgageStore{}
	h, key := newMortgageServer(t, newMortgageAccounts(), store)
	hdr := authHdr(mintIdentityToken(t, key, "mort-a", "mort-a@localhost", "mort-a", "mort-a"))
	events := []map[string]any{
		{"effective_date": "2020-07-01", "annual_rate": "5.50", "rate_type": "fixed", "action": "re_amortize"},
	}
	code, out := doReq(t, h, http.MethodPost, "/api/v1/mortgages",
		withField(t, baseMortgageBody(), "rate_events", events), hdr)
	if code != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201 (body %s)", code, out)
	}
	if !strings.Contains(out, `"rate_events":[]`) {
		t.Fatalf("create body = %s, want rate_events exactly [] (client-sent events are ignored on create)", out)
	}
	if store.createCalls != 1 {
		t.Errorf("store create calls = %d, want 1", store.createCalls)
	}
}

// TestMortgagesCreateRequired covers the POST all-9-required rule: each
// missing field is exactly "<name> is required".
func TestMortgagesCreateRequired(t *testing.T) {
	fields := []string{
		"account_id", "principal", "annual_rate", "amortization_years",
		"start_date", "payment_frequency", "extra_payment_amount",
		"extra_payment_frequency", "extra_prepayment_type",
	}
	for _, field := range fields {
		t.Run(field, func(t *testing.T) {
			store := &memMortgageStore{}
			h, key := newMortgageServer(t, newMortgageAccounts(), store)
			hdr := authHdr(mintIdentityToken(t, key, "mort-a", "mort-a@localhost", "mort-a", "mort-a"))
			code, out := doReq(t, h, http.MethodPost, "/api/v1/mortgages", withoutField(t, baseMortgageBody(), field), hdr)
			expectAPIError(t, code, out, http.StatusBadRequest, "invalid_request", field+" is required")
			if store.createCalls != 0 {
				t.Errorf("store create calls = %d, want 0 (400 before any store call)", store.createCalls)
			}
		})
	}
}

// TestMortgagesCreateValidation covers each per-field message EXACTLY as
// pinned (a shape-valid but absent account still 404s — see
// TestMortgagesCreateAccount), plus the "; "-join when several fields
// fail at once (account_id problem first, then the spec fields in wire
// order).
func TestMortgagesCreateValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"account_id_bad_shape", withField(t, baseMortgageBody(), "account_id", "not-a-uuid"), "account_id must be a valid UUID"},
		{"principal_bad_decimal", withField(t, baseMortgageBody(), "principal", "abc"), `amount "abc" is not a valid decimal`},
		{"principal_zero", withField(t, baseMortgageBody(), "principal", "0"), "principal must be greater than zero"},
		{"principal_negative", withField(t, baseMortgageBody(), "principal", "-100"), "principal must be greater than zero"},
		{"annual_rate_not_a_number", withField(t, baseMortgageBody(), "annual_rate", "abc"), "annual_rate must be a number"},
		{"annual_rate_zero", withField(t, baseMortgageBody(), "annual_rate", "0"), "annual_rate must be greater than 0 and at most 100"},
		{"annual_rate_above_100", withField(t, baseMortgageBody(), "annual_rate", "100.01"), "annual_rate must be greater than 0 and at most 100"},
		{"annual_rate_nan", withField(t, baseMortgageBody(), "annual_rate", "nan"), "annual_rate must be greater than 0 and at most 100"},
		{"years_below_1", withField(t, baseMortgageBody(), "amortization_years", 0), "amortization_years must be between 1 and 40"},
		{"years_above_40", withField(t, baseMortgageBody(), "amortization_years", 41), "amortization_years must be between 1 and 40"},
		{"start_date_bad_form", withField(t, baseMortgageBody(), "start_date", "2020/06/01"), "start_date must be a date in YYYY-MM-DD form"},
		{"start_date_future", withField(t, baseMortgageBody(), "start_date", "2999-01-01"), "start_date must not be in the future"},
		{"payment_frequency_bad", withField(t, baseMortgageBody(), "payment_frequency", "weekly"), "payment_frequency must be monthly, semi_monthly, or biweekly"},
		{"extra_amount_negative", withField(t, baseMortgageBody(), "extra_payment_amount", "-1"), "extra_payment_amount must not be negative"},
		{"extra_frequency_bad", withField(t, baseMortgageBody(), "extra_payment_frequency", "biweekly"), "extra_payment_frequency must be monthly, quarterly, or yearly"},
		{"prepayment_type_bad", withField(t, baseMortgageBody(), "extra_prepayment_type", "interest"), "extra_prepayment_type must be term or principal"},
		{"term_years_above_40", withField(t, baseMortgageBody(), "term_years", 41), "term_years must be between 0 and 40"},
		{"term_years_negative", withField(t, baseMortgageBody(), "term_years", -1), "term_years must be between 0 and 40"},
		{"rate_type_bad", withField(t, baseMortgageBody(), "rate_type", "arm"), "rate_type must be fixed or variable"},
		{"two_problems_joined", withField(t, withField(t, baseMortgageBody(), "principal", "0"), "annual_rate", "abc"),
			"principal must be greater than zero; annual_rate must be a number"},
		{"account_missing_plus_bad_principal", withField(t, withoutField(t, baseMortgageBody(), "account_id"), "principal", "abc"),
			"account_id is required; amount \"abc\" is not a valid decimal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &memMortgageStore{}
			h, key := newMortgageServer(t, newMortgageAccounts(), store)
			hdr := authHdr(mintIdentityToken(t, key, "mort-a", "mort-a@localhost", "mort-a", "mort-a"))
			code, out := doReq(t, h, http.MethodPost, "/api/v1/mortgages", tc.body, hdr)
			expectAPIError(t, code, out, http.StatusBadRequest, "invalid_request", tc.want)
			if store.createCalls != 0 {
				t.Errorf("store create calls = %d, want 0 (400 before any store call)", store.createCalls)
			}
		})
	}
}

// TestMortgagesCreateAccount covers the account existence/type checks,
// which run AFTER shape validation and only when account_id was
// shape-valid: an unknown well-formed account is 404 "account not found"
// (the imports.go sentinel message); a well-formed account of another
// type is 400 "account is not a mortgage account".
func TestMortgagesCreateAccount(t *testing.T) {
	t.Run("unknown_account_404", func(t *testing.T) {
		store := &memMortgageStore{}
		h, key := newMortgageServer(t, newMortgageAccounts(), store)
		hdr := authHdr(mintIdentityToken(t, key, "mort-a", "mort-a@localhost", "mort-a", "mort-a"))
		body := withField(t, baseMortgageBody(), "account_id", "99999999-9999-9999-9999-999999999999")
		code, out := doReq(t, h, http.MethodPost, "/api/v1/mortgages", body, hdr)
		expectAPIError(t, code, out, http.StatusNotFound, "not_found", "account not found")
		if store.createCalls != 0 {
			t.Errorf("store create calls = %d, want 0 (404 before any store call)", store.createCalls)
		}
	})
	t.Run("wrong_type_400", func(t *testing.T) {
		store := &memMortgageStore{}
		h, key := newMortgageServer(t, newMortgageAccounts(), store)
		hdr := authHdr(mintIdentityToken(t, key, "mort-a", "mort-a@localhost", "mort-a", "mort-a"))
		body := withField(t, baseMortgageBody(), "account_id", checkingID)
		code, out := doReq(t, h, http.MethodPost, "/api/v1/mortgages", body, hdr)
		expectAPIError(t, code, out, http.StatusBadRequest, "invalid_request", "account is not a mortgage account")
		if store.createCalls != 0 {
			t.Errorf("store create calls = %d, want 0 (400 before any store call)", store.createCalls)
		}
	})
}

// TestMortgagesGet covers the id rules: a malformed id answers 404
// WITHOUT reaching the store; a well-formed id the caller does not own
// (another user's row) and an unknown well-formed id are both 404
// (existence is not disclosed).
func TestMortgagesGet(t *testing.T) {
	store := &memMortgageStore{}
	h, key := newMortgageServer(t, newMortgageAccounts(), store)
	hdr := authHdr(mintIdentityToken(t, key, "mort-a", "mort-a@localhost", "mort-a", "mort-a"))
	hdrB := authHdr(mintIdentityToken(t, key, "mort-b", "mort-b@localhost", "mort-b", "mort-b"))

	// Malformed id: 404 before the store is touched.
	code, out := doReq(t, h, http.MethodGet, "/api/v1/mortgages/not-a-uuid", "", hdr)
	expectAPIError(t, code, out, http.StatusNotFound, "not_found", "mortgage not found")
	if store.getCalls != 0 {
		t.Fatalf("store get calls = %d, want 0 (malformed id never reaches the store)", store.getCalls)
	}

	// A row for user A; user B must see the same 404 (no disclosure).
	code, out = doReq(t, h, http.MethodPost, "/api/v1/mortgages", baseMortgageBody(), hdr)
	if code != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201 (body %s)", code, out)
	}
	id1 := idFrom(t, out)
	code, out = doReq(t, h, http.MethodGet, "/api/v1/mortgages/"+id1, "", hdrB)
	expectAPIError(t, code, out, http.StatusNotFound, "not_found", "mortgage not found")

	// An unknown well-formed id: 404 as well.
	code, out = doReq(t, h, http.MethodGet, "/api/v1/mortgages/99999999-9999-9999-9999-999999999999", "", hdr)
	expectAPIError(t, code, out, http.StatusNotFound, "not_found", "mortgage not found")
}

// TestMortgagesPatch covers the PATCH body rules: empty patch is 400
// (no store call); the present fields are validated with the same
// per-field rules as create (absent fields raise nothing); account_id
// is NOT in the patch DTO and a client-sent one is silently ignored.
func TestMortgagesPatch(t *testing.T) {
	store := &memMortgageStore{}
	h, key := newMortgageServer(t, newMortgageAccounts(), store)
	hdr := authHdr(mintIdentityToken(t, key, "mort-a", "mort-a@localhost", "mort-a", "mort-a"))

	code, out := doReq(t, h, http.MethodPost, "/api/v1/mortgages", baseMortgageBody(), hdr)
	if code != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201 (body %s)", code, out)
	}
	id1 := idFrom(t, out)

	// Empty patch: 400 before the store.
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/mortgages/"+id1, `{}`, hdr)
	expectAPIError(t, code, out, http.StatusBadRequest, "invalid_request", "empty patch: provide at least one field")
	if store.updateCalls != 0 {
		t.Fatalf("store update calls = %d, want 0 (empty patch never reaches the store)", store.updateCalls)
	}

	// Present-field validation only: one bad present field, the absent
	// fields (which would fail create's required rules) stay silent.
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/mortgages/"+id1, `{"annual_rate":"200"}`, hdr)
	expectAPIError(t, code, out, http.StatusBadRequest, "invalid_request", "annual_rate must be greater than 0 and at most 100")
	if store.updateCalls != 0 {
		t.Fatalf("store update calls = %d, want 0 (400 before any store call)", store.updateCalls)
	}

	// Several bad present fields: "; "-joined in wire order.
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/mortgages/"+id1,
		`{"principal":"0","payment_frequency":"weekly"}`, hdr)
	expectAPIError(t, code, out, http.StatusBadRequest, "invalid_request",
		"principal must be greater than zero; payment_frequency must be monthly, semi_monthly, or biweekly")

	// account_id is silently ignored: the patch applies, the account stays.
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/mortgages/"+id1,
		`{"account_id":"`+checkingID+`","amortization_years":25}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("patch: status = %d, want 200 (body %s)", code, out)
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(out), &row); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if row["account_id"] != mortAcctID {
		t.Errorf("account_id = %v, want %s (account_id is immutable, patch DTO has no such field)", row["account_id"], mortAcctID)
	}
	if row["amortization_years"] != float64(25) {
		t.Errorf("amortization_years = %v, want 25", row["amortization_years"])
	}

	// Round 23: a non-nil rate_events FULLY replaces the stored events,
	// canonicalizing each annual_rate to the 2-decimal percent string;
	// the other spec fields stay verbatim.
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/mortgages/"+id1,
		`{"rate_events":[{"effective_date":"2020-07-01","annual_rate":"5","rate_type":"fixed","action":"re_amortize"}]}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("patch events: status = %d, want 200 (body %s)", code, out)
	}
	row = nil
	if err := json.Unmarshal([]byte(out), &row); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	evs, ok := row["rate_events"].([]any)
	if !ok || len(evs) != 1 {
		t.Fatalf("rate_events = %v, want exactly one event", row["rate_events"])
	}
	ev := evs[0].(map[string]any)
	if ev["effective_date"] != "2020-07-01" || ev["annual_rate"] != "5.00" || ev["rate_type"] != "fixed" || ev["action"] != "re_amortize" {
		t.Errorf("event = %v, want the canonical 5.00 and the verbatim fields", ev)
	}
	if row["annual_rate"] != "4.99" || row["term_years"] != float64(0) {
		t.Errorf("event patch left other fields alone: annual_rate = %v, term_years = %v", row["annual_rate"], row["term_years"])
	}

	// term_years / rate_type are ordinary patch fields.
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/mortgages/"+id1,
		`{"term_years":5,"rate_type":"variable"}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("patch term: status = %d, want 200 (body %s)", code, out)
	}
	row = nil
	if err := json.Unmarshal([]byte(out), &row); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if row["term_years"] != float64(5) || row["rate_type"] != "variable" {
		t.Errorf("term_years/rate_type = %v/%v, want 5/variable", row["term_years"], row["rate_type"])
	}

	// An empty array is a valid full-array replacement: it clears.
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/mortgages/"+id1, `{"rate_events":[]}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("patch clear: status = %d, want 200 (body %s)", code, out)
	}
	if !strings.Contains(out, `"rate_events":[]`) {
		t.Errorf("cleared body = %s, want rate_events exactly []", out)
	}

	// null = absent: with nothing else present, an empty patch.
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/mortgages/"+id1, `{"rate_events":null}`, hdr)
	expectAPIError(t, code, out, http.StatusBadRequest, "invalid_request", "empty patch: provide at least one field")

	// Per-event 400s, EXACTLY as pinned. The stored start date
	// (2020-06-01) anchors the after-start check; a patch start_date
	// overrides it when present.
	eventCases := []struct {
		name string
		body string
		want string
	}{
		{"bad_date_form", `{"rate_events":[{"effective_date":"2020/07/01","annual_rate":"5","rate_type":"fixed","action":"re_amortize"}]}`,
			"rate event effective_date must be a date in YYYY-MM-DD form"},
		{"before_stored_start", `{"rate_events":[{"effective_date":"2020-05-01","annual_rate":"5","rate_type":"fixed","action":"re_amortize"}]}`,
			"rate event effective_date must be after start_date"},
		{"rate_not_a_number", `{"rate_events":[{"effective_date":"2020-07-01","annual_rate":"abc","rate_type":"fixed","action":"re_amortize"}]}`,
			"rate event annual_rate must be a number"},
		{"rate_zero", `{"rate_events":[{"effective_date":"2020-07-01","annual_rate":"0","rate_type":"fixed","action":"re_amortize"}]}`,
			"rate event annual_rate must be greater than 0 and at most 100"},
		{"rate_above_100", `{"rate_events":[{"effective_date":"2020-07-01","annual_rate":"100.01","rate_type":"fixed","action":"re_amortize"}]}`,
			"rate event annual_rate must be greater than 0 and at most 100"},
		{"rate_type_bad", `{"rate_events":[{"effective_date":"2020-07-01","annual_rate":"5","rate_type":"arm","action":"re_amortize"}]}`,
			"rate event rate_type must be fixed or variable"},
		{"action_bad", `{"rate_events":[{"effective_date":"2020-07-01","annual_rate":"5","rate_type":"fixed","action":"swap"}]}`,
			"rate event action must be re_amortize or keep_payment"},
		{"not_ascending", `{"rate_events":[{"effective_date":"2020-08-01","annual_rate":"5","rate_type":"fixed","action":"re_amortize"},{"effective_date":"2020-07-01","annual_rate":"5","rate_type":"fixed","action":"re_amortize"}]}`,
			"rate event effective_dates must be strictly ascending"},
		{"patch_start_overrides_stored", `{"start_date":"2021-01-01","rate_events":[{"effective_date":"2020-12-01","annual_rate":"5","rate_type":"fixed","action":"re_amortize"}]}`,
			"rate event effective_date must be after start_date"},
	}
	for _, tc := range eventCases {
		t.Run(tc.name, func(t *testing.T) {
			before := store.updateCalls
			code, out := doReq(t, h, http.MethodPatch, "/api/v1/mortgages/"+id1, tc.body, hdr)
			expectAPIError(t, code, out, http.StatusBadRequest, "invalid_request", tc.want)
			if store.updateCalls != before {
				t.Errorf("store update calls = %d, want %d (400 before any store update)", store.updateCalls, before)
			}
		})
	}

	// The row is fetched BEFORE body validation: an unknown well-formed
	// id 404s even with a malformed body (no existence disclosure).
	code, out = doReq(t, h, http.MethodPatch,
		"/api/v1/mortgages/99999999-9999-9999-9999-999999999999", `{"annual_rate":"abc"}`, hdr)
	expectAPIError(t, code, out, http.StatusNotFound, "not_found", "mortgage not found")

	// Malformed id: 404 before the store.
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/mortgages/not-a-uuid", `{"annual_rate":"5"}`, hdr)
	expectAPIError(t, code, out, http.StatusNotFound, "not_found", "mortgage not found")
}

// TestMortgagesDelete covers DELETE: 204 (empty body) on success, 404
// for an unknown well-formed id, and 404 for a malformed id without
// reaching the store.
func TestMortgagesDelete(t *testing.T) {
	store := &memMortgageStore{}
	h, key := newMortgageServer(t, newMortgageAccounts(), store)
	hdr := authHdr(mintIdentityToken(t, key, "mort-a", "mort-a@localhost", "mort-a", "mort-a"))

	// Malformed id: 404 before the store.
	code, out := doReq(t, h, http.MethodDelete, "/api/v1/mortgages/not-a-uuid", "", hdr)
	expectAPIError(t, code, out, http.StatusNotFound, "not_found", "mortgage not found")
	if store.deleteCalls != 0 {
		t.Fatalf("store delete calls = %d, want 0 (malformed id never reaches the store)", store.deleteCalls)
	}

	// Unknown well-formed id: 404.
	code, out = doReq(t, h, http.MethodDelete, "/api/v1/mortgages/99999999-9999-9999-9999-999999999999", "", hdr)
	expectAPIError(t, code, out, http.StatusNotFound, "not_found", "mortgage not found")

	// Success: 204, empty body; gone afterwards.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/mortgages", baseMortgageBody(), hdr)
	if code != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201 (body %s)", code, out)
	}
	id1 := idFrom(t, out)
	code, out = doReq(t, h, http.MethodDelete, "/api/v1/mortgages/"+id1, "", hdr)
	if code != http.StatusNoContent {
		t.Fatalf("delete: status = %d, want 204 (body %s)", code, out)
	}
	if out != "" {
		t.Errorf("delete body = %q, want empty", out)
	}
	code, out = doReq(t, h, http.MethodDelete, "/api/v1/mortgages/"+id1, "", hdr)
	expectAPIError(t, code, out, http.StatusNotFound, "not_found", "mortgage not found")
}

// TestMortgagesNilStore covers the 503 path (no database configured:
// main.go leaves the store nil).
func TestMortgagesNilStore(t *testing.T) {
	h, key := newMortgageServer(t, newMortgageAccounts(), nil)
	hdr := authHdr(mintIdentityToken(t, key, "mort-a", "mort-a@localhost", "mort-a", "mort-a"))
	code, out := doReq(t, h, http.MethodGet, "/api/v1/mortgages", "", hdr)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("list: status = %d, want 503 (body %s)", code, out)
	}
}

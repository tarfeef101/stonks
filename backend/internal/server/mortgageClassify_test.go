// Classify endpoint handler tests (round 32): the user's manual
// one-off-vs-rate-change answer for one aberrant payment the matching
// report flags. Pins the house error order (503 before auth, 404 before
// the store, body validation before the Get), the joined validation
// messages, the lazily-created "mortgage_aberrant" notifications
// (exact bodies, the deterministic jsonb ref, dedup on revisit, and the
// mark-read on classify), and both classification paths end to end: a
// one_off stores the answer without touching the schedule, a
// rate_change upserts a rate event so the re-amortized plan explains
// the higher payments and the suggestion disappears, and a rejected
// records the answer with no rate event (the suggestion stays in the
// report, annotated, until it is re-classified in place).
package server

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"finance-tracker/internal/mortgage"
	"finance-tracker/internal/repo"

	"github.com/lestrrat-go/jwx/v3/jwk"
)

// newClassifyServer builds a full router with a live token verifier, an
// in-memory bootstrap, and the given account, mortgage, transaction, and
// notification stores (any of them nil — that is its 503 path).
func newClassifyServer(t *testing.T, accounts AccountStore, store MortgageStore, txs TransactionStore, notifs NotificationStore) (http.Handler, jwk.Key) {
	t.Helper()
	key := mintKey(t)
	b, err := NewBootstrap(newMemStore(), testKEK(t), nil)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return New(fakeReady{}, BuildInfo{}, liveAuth(t, key), b, nil, accounts, nil, txs, nil, notifs, nil, nil, nil, nil, nil, nil, nil, nil, nil, store, nil, false, nil, nil, nil), key
}

// storedMortgage returns the in-memory store's row for the id (the
// tests are in the same package, so the spec is directly inspectable).
func storedMortgage(t *testing.T, store *memMortgageStore, id string) repo.Mortgage {
	t.Helper()
	for _, r := range store.rows {
		if r.m.ID == id {
			return r.m
		}
	}
	t.Fatalf("mortgage %s not found in the store", id)
	return repo.Mortgage{}
}

// seedExtrasAndShorts replays the matching ExtrasAndShorts scenario on
// the linked account: five grid payments (the 4th, 2026-03-01, is
// 26.18 short) plus one off-grid debit of 100.00 on 2026-01-22.
func seedExtrasAndShorts(t *testing.T, txs *memTxStore) {
	t.Helper()
	sched, err := mortgage.Generate(matchEngineSpec())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	amts := []string{"-1326.18", "-1326.18", "-1326.18", "-1300.00", "-1326.18"}
	for i, amt := range amts {
		seedTx(t, txs, "id-mort-a", mortAcctID, "", sched.Payments[i].Date.Format("2006-01-02"), amt, "mortgage payment", "")
	}
	seedTx(t, txs, "id-mort-a", mortAcctID, "", "2026-01-22", "-100.00", "lump sum", "")
}

// TestMortgageClassifyErrors pins the house error order: a missing
// token is 401 with the authn library's default body; a nil mortgage
// store and a nil transaction store are both 503; a malformed id is
// 404 before the store; a non-JSON body is 400 before the store; and
// body validation precedes the Get (a bad body against an UNKNOWN id is
// 400, not 404, with every violation joined in order).
func TestMortgageClassifyErrors(t *testing.T) {
	// No token: 401 from the authn middleware's default error handler
	// (body {"error":"invalid_token"}, not the business envelope).
	h0, _ := newClassifyServer(t, newMortgageAccounts(), &memMortgageStore{}, &memTxStore{}, &memNotificationStore{})
	code, out := doReq(t, h0, http.MethodPost, "/api/v1/mortgages/"+checkingID+"/classifications", "", map[string]string{})
	if code != http.StatusUnauthorized {
		t.Fatalf("no-token: status = %d, want 401 (body %s)", code, out)
	}
	if strings.TrimSpace(out) != `{"error":"invalid_token"}` {
		t.Fatalf("no-token body = %s, want {\"error\":\"invalid_token\"} (the authn library's default 401 body)", out)
	}

	// Nil mortgage store: 503 "database not configured".
	h1, key1 := newClassifyServer(t, newMortgageAccounts(), nil, &memTxStore{}, &memNotificationStore{})
	hdr1 := authHdr(mintIdentityToken(t, key1, "mort-a", "mort-a@localhost", "mort-a", "mort-a"))
	code, out = doReq(t, h1, http.MethodPost, "/api/v1/mortgages/"+checkingID+"/classifications", "", hdr1)
	expectAPIError(t, code, out, http.StatusServiceUnavailable, "internal", "database not configured")

	// Nil transaction store: 503 even with a mortgage store configured.
	h2, key2 := newClassifyServer(t, newMortgageAccounts(), &memMortgageStore{}, nil, &memNotificationStore{})
	hdr2 := authHdr(mintIdentityToken(t, key2, "mort-a", "mort-a@localhost", "mort-a", "mort-a"))
	code, out = doReq(t, h2, http.MethodPost, "/api/v1/mortgages/"+checkingID+"/classifications", "", hdr2)
	expectAPIError(t, code, out, http.StatusServiceUnavailable, "internal", "database not configured")

	// The 404/400 paths need a configured server.
	store := &memMortgageStore{}
	h, key := newClassifyServer(t, newMortgageAccounts(), store, &memTxStore{}, &memNotificationStore{})
	hdr := authHdr(mintIdentityToken(t, key, "mort-a", "mort-a@localhost", "mort-a", "mort-a"))

	// Malformed id: 404 before the store is touched.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/mortgages/not-a-uuid/classifications", `{}`, hdr)
	expectAPIError(t, code, out, http.StatusNotFound, "not_found", "mortgage not found")
	if store.getCalls != 0 {
		t.Fatalf("store get calls = %d, want 0 (malformed id never reaches the store)", store.getCalls)
	}

	// Unknown well-formed id with a VALID body: 404 from the Get.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/mortgages/99999999-9999-9999-9999-999999999999/classifications",
		`{"date":"2026-01-22","kind":"extra","classification":"one_off"}`, hdr)
	expectAPIError(t, code, out, http.StatusNotFound, "not_found", "mortgage not found")

	// Non-JSON body: 400 before the store is touched.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/mortgages/"+nextTestID()+"/classifications", `not json`, hdr)
	if code != http.StatusBadRequest {
		t.Fatalf("non-JSON: status = %d, want 400 (body %s)", code, out)
	}
	if c, m := apiErr(t, out); c != codeInvalidRequest || !strings.HasPrefix(m, "body must be a JSON object: ") {
		t.Errorf("error = %s/%q, want %s/%q…", c, m, codeInvalidRequest, "body must be a JSON object: ")
	}
	if store.getCalls != 1 {
		t.Fatalf("store get calls = %d, want 1 (the non-JSON body answered before the Get)", store.getCalls)
	}

	// Validation precedes the Get: a bad body against an UNKNOWN id is
	// 400 (not 404), with every violation joined in order.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/mortgages/99999999-9999-9999-9999-999999999999/classifications",
		`{"date":"2026-13-45","kind":"lump","classification":"rate_change"}`, hdr)
	expectAPIError(t, code, out, http.StatusBadRequest, "invalid_request",
		"date must be YYYY-MM-DD; kind must be extra, short or suggestion; annual_rate is required for a rate change; rate_type must be fixed or variable; action must be re_amortize or keep_payment")
}

// TestMortgageClassifyOneOff pins the one-off path end to end: the
// first matching GET lazily creates one "mortgage_aberrant"
// notification per unclassified item (exact bodies, deterministic jsonb
// ref, title == body), a revisit dedupes, and the classify answer is
// stored in the spec with its notification marked read (the short's
// stays waiting). Re-classifying the same (kind, date) replaces in
// place.
func TestMortgageClassifyOneOff(t *testing.T) {
	store := &memMortgageStore{}
	txs := &memTxStore{}
	notifs := &memNotificationStore{}
	h, key := newClassifyServer(t, newMortgageAccounts(), store, txs, notifs)
	hdr := authHdr(mintIdentityToken(t, key, "mort-a", "mort-a@localhost", "mort-a", "mort-a"))
	id := createMatchMortgage(t, h, hdr)
	seedExtrasAndShorts(t, txs)

	// First matching GET: both items carry classification null, and
	// the two notifications are created lazily.
	code, out := doReq(t, h, http.MethodGet, "/api/v1/mortgages/"+id+"/matching", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("get: status = %d, want 200 (body %s)", code, out)
	}
	if got := firstObjectInArray(t, rawJSONValueAt(t, out, "extras")); got != `{"date":"2026-01-22","amount":"100.00","classification":null}` {
		t.Errorf("extras[0] = %s, want verbatim {…,\"classification\":null}", got)
	}
	if got := firstObjectInArray(t, rawJSONValueAt(t, out, "shorts")); got != `{"date":"2026-03-01","amount":"26.18","classification":null}` {
		t.Errorf("shorts[0] = %s, want verbatim {…,\"classification\":null}", got)
	}
	if len(notifs.rows) != 2 {
		t.Fatalf("notifications = %d rows, want 2 (one per unclassified item)", len(notifs.rows))
	}
	const (
		extraBody = "Mortgage extra of 100.00 on 2026-01-22 needs classification"
		shortBody = "Mortgage shortfall of 26.18 on 2026-03-01 needs classification"
	)
	byBody := map[string]repo.Notification{}
	for _, r := range notifs.rows {
		if r.userID != "id-mort-a" {
			t.Errorf("row %s scoped to user %q, want id-mort-a", r.n.ID, r.userID)
		}
		if r.n.Kind != "mortgage_aberrant" {
			t.Errorf("row %s kind = %q, want mortgage_aberrant", r.n.ID, r.n.Kind)
		}
		if r.n.Title != r.n.Body {
			t.Errorf("row %s title %q != body %q (the single-sentence convention)", r.n.ID, r.n.Title, r.n.Body)
		}
		if r.n.ReadAt != nil {
			t.Errorf("row %s already read", r.n.ID)
		}
		byBody[r.n.Body] = r.n
	}
	if n, ok := byBody[extraBody]; !ok || !jsonbEqual(n.Ref, []byte(`{"date":"2026-01-22","kind":"extra","mortgage_id":"`+id+`"}`)) {
		t.Errorf("extra notification missing or ref = %v, want the (kind, date) pair scoped to the mortgage", byBody[extraBody].Ref)
	}
	if n, ok := byBody[shortBody]; !ok || !jsonbEqual(n.Ref, []byte(`{"date":"2026-03-01","kind":"short","mortgage_id":"`+id+`"}`)) {
		t.Errorf("short notification missing or ref = %v, want the (kind, date) pair scoped to the mortgage", byBody[shortBody].Ref)
	}

	// A second GET dedupes on (kind, ref) in any read state: still 2.
	code, out = doReq(t, h, http.MethodGet, "/api/v1/mortgages/"+id+"/matching", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("revisit: status = %d, want 200 (body %s)", code, out)
	}
	if len(notifs.rows) != 2 {
		t.Fatalf("revisit created %d rows, want 2 (the (kind, ref) dedup must hold)", len(notifs.rows))
	}

	// Classify the extra as a one-off payment against principal.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/mortgages/"+id+"/classifications",
		`{"date":"2026-01-22","kind":"extra","classification":"one_off"}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("classify: status = %d, want 200 (body %s)", code, out)
	}
	if got := rawJSONValueAt(t, out, "rate_events"); got != `[]` {
		t.Errorf("rate_events = %s, want [] (a one-off writes no rate event)", got)
	}
	var resp mortgageOut
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if resp.ID != id || resp.AnnualRate != "4.39" {
		t.Errorf("response = %s/%s, want %s/4.39 (the base rate is untouched)", resp.ID, resp.AnnualRate, id)
	}

	// The answer lives inside the DEK-encrypted spec (inspectable in
	// the same package).
	m := storedMortgage(t, store, id)
	wantCls := []repo.PaymentClassification{{Date: "2026-01-22", Kind: "extra", Classification: "one_off"}}
	if !reflect.DeepEqual(m.Spec.PaymentClassifications, wantCls) {
		t.Errorf("spec.payment_classifications = %#v, want %#v (rate fields nil for a one-off)", m.Spec.PaymentClassifications, wantCls)
	}
	if len(m.Spec.RateEvents) != 0 {
		t.Errorf("spec.rate_events = %#v, want none", m.Spec.RateEvents)
	}

	// The extra's notification is read; the short's is still waiting.
	for _, r := range notifs.rows {
		if r.n.Body == extraBody && r.n.ReadAt == nil {
			t.Errorf("the extra's notification was not marked read")
		}
		if r.n.Body == shortBody && r.n.ReadAt != nil {
			t.Errorf("the short's notification was marked read early")
		}
	}

	// The report now carries the answer; the item is still reported
	// (a one-off does not change the schedule) and no notification is
	// duplicated on the revisit.
	code, out = doReq(t, h, http.MethodGet, "/api/v1/mortgages/"+id+"/matching", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("get after classify: status = %d, want 200 (body %s)", code, out)
	}
	var body matchingOut
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if len(body.Extras) != 1 || body.Extras[0].Classification == nil || *body.Extras[0].Classification != "one_off" {
		t.Errorf("extras = %+v, want the one extra classified one_off", body.Extras)
	}
	if len(body.Shorts) != 1 || body.Shorts[0].Classification != nil {
		t.Errorf("shorts = %+v, want the one short still unclassified", body.Shorts)
	}
	if len(notifs.rows) != 2 {
		t.Errorf("rows after classify + revisit = %d, want 2 (no duplicates)", len(notifs.rows))
	}

	// Re-classifying the same (kind, date) REPLACES in place: one
	// classification row, now the rate change, plus the rate event.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/mortgages/"+id+"/classifications",
		`{"date":"2026-01-22","kind":"extra","classification":"rate_change","annual_rate":"5.40","rate_type":"fixed","action":"re_amortize"}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("re-classify: status = %d, want 200 (body %s)", code, out)
	}
	m = storedMortgage(t, store, id)
	rate, rateType, action := "5.40", "fixed", "re_amortize"
	wantRe := []repo.PaymentClassification{{
		Date: "2026-01-22", Kind: "extra", Classification: "rate_change",
		AnnualRate: &rate, RateType: &rateType, Action: &action,
	}}
	if !reflect.DeepEqual(m.Spec.PaymentClassifications, wantRe) {
		t.Errorf("spec.payment_classifications = %#v, want %#v (the in-place replace, not a second row)", m.Spec.PaymentClassifications, wantRe)
	}
	wantEvents := []repo.RateEvent{{EffectiveDate: "2026-01-22", AnnualRate: "5.40", RateType: "fixed", Action: "re_amortize"}}
	if !reflect.DeepEqual(m.Spec.RateEvents, wantEvents) {
		t.Errorf("spec.rate_events = %#v, want %#v", m.Spec.RateEvents, wantEvents)
	}
}

// TestMortgageClassifyRateChange pins the rate-change path end to end
// on the drift scenario: the single suggestion surfaces with a null
// classification and its notification, the answer upserts a rate event
// (served in the 200 response, base rate untouched), the notification
// is marked read, and the re-amortized plan now explains the higher
// payments — the suggestion is gone and all 20 periods match.
func TestMortgageClassifyRateChange(t *testing.T) {
	store := &memMortgageStore{}
	txs := &memTxStore{}
	notifs := &memNotificationStore{}
	h, key := newClassifyServer(t, newMortgageAccounts(), store, txs, notifs)
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

	// The drift surfaces exactly one suggestion (classification null)
	// and its notification.
	code, out := doReq(t, h, http.MethodGet, "/api/v1/mortgages/"+id+"/matching", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("get: status = %d, want 200 (body %s)", code, out)
	}
	wantSuggestion := `{"effective_date":"2026-06-14","implied_annual_rate":"5.40","observed_payment":"1463.00","observed_periods":10,"classification":null}`
	if got := firstObjectInArray(t, rawJSONValueAt(t, out, "suggestions")); got != wantSuggestion {
		t.Errorf("suggestions[0] = %s, want verbatim %s", got, wantSuggestion)
	}
	if len(notifs.rows) != 1 {
		t.Fatalf("notifications = %d rows, want 1 (the suggestion)", len(notifs.rows))
	}
	n := notifs.rows[0].n
	if n.Kind != "mortgage_aberrant" || n.Title != n.Body || n.ReadAt != nil {
		t.Errorf("notification = %+v, want an unread mortgage_aberrant row with title == body", n)
	}
	if n.Body != "A sustained payment change since 2026-06-14 needs classification" {
		t.Errorf("body = %q, want the pinned suggestion body", n.Body)
	}
	if !jsonbEqual(n.Ref, []byte(`{"date":"2026-06-14","kind":"suggestion","mortgage_id":"`+id+`"}`)) {
		t.Errorf("ref = %s, want the (kind, date) pair scoped to the mortgage", n.Ref)
	}

	// Answer: it is a rate change to 5.40, re-amortized.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/mortgages/"+id+"/classifications",
		`{"date":"2026-06-14","kind":"suggestion","classification":"rate_change","annual_rate":"5.40","rate_type":"fixed","action":"re_amortize"}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("classify: status = %d, want 200 (body %s)", code, out)
	}
	// The 200 response carries the upserted event (the PATCH
	// convention); the base rate is untouched.
	wantEvents := `[{"effective_date":"2026-06-14","annual_rate":"5.40","rate_type":"fixed","action":"re_amortize"}]`
	if got := rawJSONValueAt(t, out, "rate_events"); got != wantEvents {
		t.Errorf("rate_events = %s, want %s", got, wantEvents)
	}
	var resp mortgageOut
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if resp.AnnualRate != "4.39" {
		t.Errorf("base annual_rate = %q, want 4.39 (only the event changes)", resp.AnnualRate)
	}

	// The spec now carries both the classification and the event.
	m := storedMortgage(t, store, id)
	rate, rateType, action := "5.40", "fixed", "re_amortize"
	wantCls := []repo.PaymentClassification{{
		Date: "2026-06-14", Kind: "suggestion", Classification: "rate_change",
		AnnualRate: &rate, RateType: &rateType, Action: &action,
	}}
	if !reflect.DeepEqual(m.Spec.PaymentClassifications, wantCls) {
		t.Errorf("spec.payment_classifications = %#v, want %#v", m.Spec.PaymentClassifications, wantCls)
	}
	wantEvent := []repo.RateEvent{{EffectiveDate: "2026-06-14", AnnualRate: "5.40", RateType: "fixed", Action: "re_amortize"}}
	if !reflect.DeepEqual(m.Spec.RateEvents, wantEvent) {
		t.Errorf("spec.rate_events = %#v, want %#v", m.Spec.RateEvents, wantEvent)
	}

	// The notification is read.
	if notifs.rows[0].n.ReadAt == nil {
		t.Error("the suggestion's notification was not marked read")
	}

	// The re-amortized plan now EXPLAINS the higher payments: no
	// suggestion, nothing else flagged, all 20 periods matched — and
	// the revisit creates no new notification.
	code, out = doReq(t, h, http.MethodGet, "/api/v1/mortgages/"+id+"/matching", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("get after classify: status = %d, want 200 (body %s)", code, out)
	}
	var body matchingOut
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if len(body.Suggestions) != 0 {
		t.Errorf("suggestions = %+v, want none (the re-amortized plan matches the payments)", body.Suggestions)
	}
	if len(body.Extras) != 0 || len(body.Shorts) != 0 || len(body.Flags) != 0 {
		t.Errorf("extras/shorts/flags = %v/%v/%v, want all empty", body.Extras, body.Shorts, body.Flags)
	}
	if body.PeriodsMatched != 20 || body.PeriodsMissing != 0 {
		t.Errorf("matched/missing = %d/%d, want 20/0", body.PeriodsMatched, body.PeriodsMissing)
	}
	if len(notifs.rows) != 1 {
		t.Errorf("rows after classify + revisit = %d, want 1 (no duplicates)", len(notifs.rows))
	}
}

// TestMortgageClassifyRejected pins the third path: "rejected" is a
// durable answer — it stores the classification (ignoring the rate
// fields), creates no rate event, marks the notification read, leaves
// the suggestion in the report (the unchanged plan still drifts, so the
// matcher re-derives it, annotated with the stored answer) without
// creating a duplicate notification, and a later re-classification to
// rate_change replaces the answer in place so the plan then explains
// the payments.
func TestMortgageClassifyRejected(t *testing.T) {
	store := &memMortgageStore{}
	txs := &memTxStore{}
	notifs := &memNotificationStore{}
	h, key := newClassifyServer(t, newMortgageAccounts(), store, txs, notifs)
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

	// The drift surfaces exactly one suggestion and its notification.
	code, out := doReq(t, h, http.MethodGet, "/api/v1/mortgages/"+id+"/matching", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("get: status = %d, want 200 (body %s)", code, out)
	}
	if len(notifs.rows) != 1 {
		t.Fatalf("notifications = %d rows, want 1 (the suggestion)", len(notifs.rows))
	}

	// Answer: not a rate change — reject it (the rate fields must be
	// ignored).
	code, out = doReq(t, h, http.MethodPost, "/api/v1/mortgages/"+id+"/classifications",
		`{"date":"2026-06-14","kind":"suggestion","classification":"rejected","annual_rate":"5.40","rate_type":"fixed","action":"re_amortize"}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("classify: status = %d, want 200 (body %s)", code, out)
	}
	if got := rawJSONValueAt(t, out, "rate_events"); got != "[]" {
		t.Errorf("rate_events = %s, want [] (a rejection writes no rate event)", got)
	}
	m := storedMortgage(t, store, id)
	wantCls := []repo.PaymentClassification{{Date: "2026-06-14", Kind: "suggestion", Classification: "rejected"}}
	if !reflect.DeepEqual(m.Spec.PaymentClassifications, wantCls) {
		t.Errorf("spec.payment_classifications = %#v, want %#v (the rate fields are ignored and stored as nulls)",
			m.Spec.PaymentClassifications, wantCls)
	}
	if len(m.Spec.RateEvents) != 0 {
		t.Errorf("spec.rate_events = %#v, want none (a rejection writes no rate event)", m.Spec.RateEvents)
	}
	if notifs.rows[0].n.ReadAt == nil {
		t.Error("the suggestion's notification was not marked read")
	}

	// The plan is unchanged, so the matcher re-derives the suggestion —
	// annotated with the stored answer — and the revisit creates no new
	// notification.
	code, out = doReq(t, h, http.MethodGet, "/api/v1/mortgages/"+id+"/matching", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("get after classify: status = %d, want 200 (body %s)", code, out)
	}
	wantSuggestion := `{"effective_date":"2026-06-14","implied_annual_rate":"5.40","observed_payment":"1463.00","observed_periods":10,"classification":"rejected"}`
	if got := firstObjectInArray(t, rawJSONValueAt(t, out, "suggestions")); got != wantSuggestion {
		t.Errorf("suggestions[0] = %s, want verbatim %s", got, wantSuggestion)
	}
	if len(notifs.rows) != 1 {
		t.Errorf("rows after classify + revisit = %d, want 1 (no duplicates)", len(notifs.rows))
	}

	// A later re-classification replaces the answer in place, and the
	// re-amortized plan then explains the higher payments.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/mortgages/"+id+"/classifications",
		`{"date":"2026-06-14","kind":"suggestion","classification":"rate_change","annual_rate":"5.40","rate_type":"fixed","action":"re_amortize"}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("re-classify: status = %d, want 200 (body %s)", code, out)
	}
	m = storedMortgage(t, store, id)
	rate, rateType, action := "5.40", "fixed", "re_amortize"
	wantCls = []repo.PaymentClassification{{
		Date: "2026-06-14", Kind: "suggestion", Classification: "rate_change",
		AnnualRate: &rate, RateType: &rateType, Action: &action,
	}}
	if !reflect.DeepEqual(m.Spec.PaymentClassifications, wantCls) {
		t.Errorf("spec.payment_classifications = %#v, want %#v (replaced in place, no duplicate row)",
			m.Spec.PaymentClassifications, wantCls)
	}
	wantEvent := []repo.RateEvent{{EffectiveDate: "2026-06-14", AnnualRate: "5.40", RateType: "fixed", Action: "re_amortize"}}
	if !reflect.DeepEqual(m.Spec.RateEvents, wantEvent) {
		t.Errorf("spec.rate_events = %#v, want %#v", m.Spec.RateEvents, wantEvent)
	}
	code, out = doReq(t, h, http.MethodGet, "/api/v1/mortgages/"+id+"/matching", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("get after re-classify: status = %d, want 200 (body %s)", code, out)
	}
	var body matchingOut
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if len(body.Suggestions) != 0 {
		t.Errorf("suggestions = %+v, want none (the re-amortized plan matches the payments)", body.Suggestions)
	}
	if body.PeriodsMatched != 20 || body.PeriodsMissing != 0 {
		t.Errorf("matched/missing = %d/%d, want 20/0", body.PeriodsMatched, body.PeriodsMissing)
	}
}

// TestMortgageClassifyValidation pins the per-field 400s (exact
// messages) and the "pair not in the current report" guard, and proves
// a one_off ignores its rate fields (accepted, stored as nulls, no
// rate event written).
func TestMortgageClassifyValidation(t *testing.T) {
	store := &memMortgageStore{}
	txs := &memTxStore{}
	h, key := newClassifyServer(t, newMortgageAccounts(), store, txs, &memNotificationStore{})
	hdr := authHdr(mintIdentityToken(t, key, "mort-a", "mort-a@localhost", "mort-a", "mort-a"))
	id := createMatchMortgage(t, h, hdr)

	sched, err := mortgage.Generate(matchEngineSpec())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	seedTx(t, txs, "id-mort-a", mortAcctID, "", sched.Payments[0].Date.Format("2006-01-02"), "-1326.18", "mortgage payment", "")
	seedTx(t, txs, "id-mort-a", mortAcctID, "", "2026-01-22", "-100.00", "lump sum", "")

	rateChange := func(rate, rateType, action string) string {
		return `{"date":"2026-01-22","kind":"extra","classification":"rate_change","annual_rate":"` + rate + `","rate_type":"` + rateType + `","action":"` + action + `"}`
	}
	cases := []struct {
		name string
		body string
		msg  string
	}{
		{"bad date", `{"date":"2026-13-45","kind":"extra","classification":"one_off"}`,
			"date must be YYYY-MM-DD"},
		{"bad kind", `{"date":"2026-01-22","kind":"lump","classification":"one_off"}`,
			"kind must be extra, short or suggestion"},
		{"bad classification", `{"date":"2026-01-22","kind":"extra","classification":"both"}`,
			"classification must be one_off, rate_change or rejected"},
		{"missing rate", `{"date":"2026-01-22","kind":"extra","classification":"rate_change","rate_type":"fixed","action":"re_amortize"}`,
			"annual_rate is required for a rate change"},
		{"zero rate", rateChange("0", "fixed", "re_amortize"),
			"annual_rate must be a positive amount"},
		{"canonical zero rate", rateChange("0.00", "fixed", "re_amortize"),
			"annual_rate must be a positive amount"},
		{"non-numeric rate", rateChange("abc", "fixed", "re_amortize"),
			"annual_rate must be a positive amount"},
		{"negative rate", rateChange("-1", "fixed", "re_amortize"),
			"annual_rate must be a positive amount"},
		{"bad rate type", rateChange("5.40", "adjustable", "re_amortize"),
			"rate_type must be fixed or variable"},
		{"bad action", rateChange("5.40", "fixed", "refinance"),
			"action must be re_amortize or keep_payment"},
		{"pair not in the report", `{"date":"2027-01-01","kind":"extra","classification":"one_off"}`,
			"no extra on 2027-01-01 in the current matching report"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out := doReq(t, h, http.MethodPost, "/api/v1/mortgages/"+id+"/classifications", tc.body, hdr)
			expectAPIError(t, code, out, http.StatusBadRequest, "invalid_request", tc.msg)
		})
	}

	t.Run("one_off ignores the rate fields", func(t *testing.T) {
		body := rateChange("5.40", "fixed", "re_amortize")
		body = strings.Replace(body, `"classification":"rate_change"`, `"classification":"one_off"`, 1)
		code, out := doReq(t, h, http.MethodPost, "/api/v1/mortgages/"+id+"/classifications", body, hdr)
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", code, out)
		}
		m := storedMortgage(t, store, id)
		wantCls := []repo.PaymentClassification{{Date: "2026-01-22", Kind: "extra", Classification: "one_off"}}
		if !reflect.DeepEqual(m.Spec.PaymentClassifications, wantCls) {
			t.Errorf("spec.payment_classifications = %#v, want %#v (the rate fields are ignored and stored as nulls)",
				m.Spec.PaymentClassifications, wantCls)
		}
		if len(m.Spec.RateEvents) != 0 {
			t.Errorf("spec.rate_events = %#v, want none (a one-off writes no rate event)", m.Spec.RateEvents)
		}
	})
}

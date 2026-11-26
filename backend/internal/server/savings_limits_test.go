package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"finance-tracker/internal/repo"
)

// --- stateful plan store fake (52a2) -----------------------------------------

// limitsFixedTime is the deterministic timestamp statePlans stamps onto
// created rows.
var limitsFixedTime = time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

// statePlans is a stateful in-memory PlanStore: upserts key on
// (plan_type, year), deletes are by id, lists come back in insertion
// order with optional plan_type ("" = any) and year (0 = any) filters.
type statePlans struct {
	mu    sync.Mutex
	lo    []repo.LimitOverride
	ra    []repo.RoomAdjustment
	loSeq int
	raSeq int
}

func (s *statePlans) ListLimitOverrides(ctx context.Context, userID, planType string, year int) ([]repo.LimitOverride, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []repo.LimitOverride{}
	for _, lo := range s.lo {
		if planType != "" && lo.PlanType != planType {
			continue
		}
		if year != 0 && lo.Year != year {
			continue
		}
		out = append(out, lo)
	}
	return out, nil
}

func (s *statePlans) UpsertLimitOverride(ctx context.Context, userID, planType string, year int, annualLimit string) (repo.LimitOverride, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.lo {
		if s.lo[i].PlanType == planType && s.lo[i].Year == year {
			s.lo[i].AnnualLimit = annualLimit
			s.lo[i].UpdatedAt = limitsFixedTime
			return s.lo[i], nil
		}
	}
	s.loSeq++
	lo := repo.LimitOverride{
		ID:          "lo" + strconv.Itoa(s.loSeq),
		PlanType:    planType,
		Year:        year,
		AnnualLimit: annualLimit,
		CreatedAt:   limitsFixedTime,
		UpdatedAt:   limitsFixedTime,
	}
	s.lo = append(s.lo, lo)
	return lo, nil
}

func (s *statePlans) DeleteLimitOverride(ctx context.Context, userID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.lo {
		if s.lo[i].ID == id {
			s.lo = append(s.lo[:i], s.lo[i+1:]...)
			return nil
		}
	}
	return repo.ErrLimitOverrideNotFound
}

func (s *statePlans) ListRoomAdjustments(ctx context.Context, userID, planType string, dek []byte) ([]repo.RoomAdjustment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []repo.RoomAdjustment{}
	for _, a := range s.ra {
		if planType != "" && a.PlanType != planType {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

func (s *statePlans) CreateRoomAdjustment(ctx context.Context, userID, planType string, year int, amount, note string, dek []byte) (repo.RoomAdjustment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.raSeq++
	a := repo.RoomAdjustment{
		ID:        "ra" + strconv.Itoa(s.raSeq),
		PlanType:  planType,
		Year:      year,
		Amount:    amount,
		Note:      note,
		CreatedAt: limitsFixedTime,
	}
	s.ra = append(s.ra, a)
	return a, nil
}

func (s *statePlans) DeleteRoomAdjustment(ctx context.Context, userID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.ra {
		if s.ra[i].ID == id {
			s.ra = append(s.ra[:i], s.ra[i+1:]...)
			return nil
		}
	}
	return repo.ErrAdjustmentNotFound
}

// --- harness -------------------------------------------------------------------

// newLimitsTest wires the 52a2 limit/adjustment routes onto a stateful
// plan store fake with a real (in-memory) bootstrap so requireUser's
// EnsureUser/UnwrapDEK path runs unmodified; every other store is nil
// because those routes only touch requireUser + h.plans.
func newLimitsTest(t *testing.T, p *statePlans) (http.Handler, string) {
	t.Helper()
	key := mintKey(t)
	b, err := NewBootstrap(newMemStore(), testKEK(t), nil)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	h := New(
		fakeReady{}, BuildInfo{}, liveAuth(t, key), b,
		nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		false,
		nil, nil, nil, nil, nil, nil,
		p, PublicConfig{})
	token := mintIdentityToken(t, key, "limits-sub", "limits@localhost", "Limits", "limitsuser")
	return h, token
}

// --- response shapes (wire mirrors with string timestamps) ---------------------

type limitOverrideWire struct {
	ID          string `json:"id"`
	PlanType    string `json:"plan_type"`
	Year        int    `json:"year"`
	AnnualLimit string `json:"annual_limit"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

type roomAdjustmentWire struct {
	ID        string `json:"id"`
	PlanType  string `json:"plan_type"`
	Year      int    `json:"year"`
	Amount    string `json:"amount"`
	Note      string `json:"note"`
	CreatedAt string `json:"created_at"`
}

func decodeLimitOne(t *testing.T, body string) limitOverrideWire {
	t.Helper()
	var w limitOverrideWire
	if err := json.Unmarshal([]byte(body), &w); err != nil {
		t.Fatalf("decoding limit override body %s: %v", body, err)
	}
	return w
}

// decodeLimits unmarshals a limits list body and fails on JSON null
// (the API must answer [], never null).
func decodeLimits(t *testing.T, body string) []limitOverrideWire {
	t.Helper()
	var out []limitOverrideWire
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decoding limits body %s: %v", body, err)
	}
	if out == nil {
		t.Fatalf("limits body %s decodes to null, want []", body)
	}
	return out
}

func decodeAdjustmentOne(t *testing.T, body string) roomAdjustmentWire {
	t.Helper()
	var w roomAdjustmentWire
	if err := json.Unmarshal([]byte(body), &w); err != nil {
		t.Fatalf("decoding room adjustment body %s: %v", body, err)
	}
	return w
}

// decodeAdjustments unmarshals an adjustments list body and fails on
// JSON null (the API must answer [], never null).
func decodeAdjustments(t *testing.T, body string) []roomAdjustmentWire {
	t.Helper()
	var out []roomAdjustmentWire
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decoding adjustments body %s: %v", body, err)
	}
	if out == nil {
		t.Fatalf("adjustments body %s decodes to null, want []", body)
	}
	return out
}

func wantRFC3339(t *testing.T, name, s string) {
	t.Helper()
	if _, err := time.Parse(time.RFC3339, s); err != nil {
		t.Errorf("%s = %q does not parse as RFC3339: %v", name, s, err)
	}
}

// --- tests -----------------------------------------------------------------------

func TestSavingsLimits(t *testing.T) {
	h, token := newLimitsTest(t, &statePlans{})
	const list = "/api/v1/savings/limits"

	// 1. Create a TFSA override.
	code, body := doRaw(t, h, http.MethodPut, list, `{"plan_type":"tfsa","year":2026,"annual_limit":"7000.00"}`, token)
	if code != http.StatusOK {
		t.Fatalf("upsert status = %d, want 200 (body %s)", code, body)
	}
	w := decodeLimitOne(t, body)
	if w.ID != "lo1" || w.PlanType != "tfsa" || w.Year != 2026 || w.AnnualLimit != "7000.00" {
		t.Fatalf("upsert = %+v, want id lo1/tfsa/2026/7000.00", w)
	}
	wantRFC3339(t, "created_at", w.CreatedAt)
	wantRFC3339(t, "updated_at", w.UpdatedAt)

	// 2. Upsert the same (plan_type, year): same id, new limit.
	code, body = doRaw(t, h, http.MethodPut, list, `{"plan_type":"tfsa","year":2026,"annual_limit":"7500.00"}`, token)
	if code != http.StatusOK {
		t.Fatalf("upsert status = %d, want 200 (body %s)", code, body)
	}
	w = decodeLimitOne(t, body)
	if w.ID != "lo1" || w.AnnualLimit != "7500.00" {
		t.Fatalf("upsert = %+v, want same id lo1 with annual_limit 7500.00", w)
	}

	// 3. Two more plans.
	code, body = doRaw(t, h, http.MethodPut, list, `{"plan_type":"rrsp","year":2026,"annual_limit":"33810.00"}`, token)
	if code != http.StatusOK || decodeLimitOne(t, body).ID != "lo2" {
		t.Fatalf("rrsp upsert status = %d body %s, want 200 id lo2", code, body)
	}
	code, body = doRaw(t, h, http.MethodPut, list, `{"plan_type":"fhsa","year":2026,"annual_limit":"8000.00"}`, token)
	if code != http.StatusOK || decodeLimitOne(t, body).ID != "lo3" {
		t.Fatalf("fhsa upsert status = %d body %s, want 200 id lo3", code, body)
	}

	// 4. Plan types without an annual limit are rejected.
	for _, pt := range []string{"rrif", "resp", "rdsp", "gic"} {
		code, body = doRaw(t, h, http.MethodPut, list, `{"plan_type":"`+pt+`","year":2026,"annual_limit":"5000.00"}`, token)
		if code != http.StatusBadRequest {
			t.Fatalf("upsert %s status = %d, want 400 (body %s)", pt, code, body)
		}
		wantErrEnvelope(t, body, codeInvalidRequest)
	}

	// 5. Bad year / amount / empty body are rejected.
	for _, b := range []string{
		`{"plan_type":"tfsa","year":1899,"annual_limit":"7000.00"}`,
		`{"plan_type":"tfsa","year":2101,"annual_limit":"7000.00"}`,
		`{"plan_type":"tfsa","year":2026,"annual_limit":"abc"}`,
		`{"plan_type":"tfsa","year":2026,"annual_limit":"-1.00"}`,
		`{}`,
	} {
		code, body = doRaw(t, h, http.MethodPut, list, b, token)
		if code != http.StatusBadRequest {
			t.Fatalf("upsert %s status = %d, want 400 (body %s)", b, code, body)
		}
		wantErrEnvelope(t, body, codeInvalidRequest)
	}

	// 6. The list now holds the three overrides.
	code, body = doRaw(t, h, http.MethodGet, list, "", token)
	if code != http.StatusOK {
		t.Fatalf("list status = %d, want 200 (body %s)", code, body)
	}
	if got := decodeLimits(t, body); len(got) != 3 {
		t.Fatalf("limits = %d entries, want 3 (body %s)", len(got), body)
	}

	// 7. Filters.
	code, body = doRaw(t, h, http.MethodGet, list+"?plan_type=tfsa", "", token)
	if code != http.StatusOK {
		t.Fatalf("tfsa list status = %d, want 200 (body %s)", code, body)
	}
	got := decodeLimits(t, body)
	if len(got) != 1 || got[0].AnnualLimit != "7500.00" {
		t.Fatalf("tfsa list = %+v, want 1 entry with annual_limit 7500.00", got)
	}

	code, body = doRaw(t, h, http.MethodGet, list+"?plan_type=tfsa&year=2026", "", token)
	if code != http.StatusOK || len(decodeLimits(t, body)) != 1 {
		t.Fatalf("tfsa/2026 list status = %d body %s, want 1 entry", code, body)
	}
	code, body = doRaw(t, h, http.MethodGet, list+"?plan_type=rrsp", "", token)
	if code != http.StatusOK || len(decodeLimits(t, body)) != 1 {
		t.Fatalf("rrsp list status = %d body %s, want 1 entry", code, body)
	}
	code, body = doRaw(t, h, http.MethodGet, list+"?plan_type=rrif", "", token)
	if code != http.StatusOK {
		t.Fatalf("rrif list status = %d, want 200 (body %s)", code, body)
	}
	if got := decodeLimits(t, body); len(got) != 0 {
		t.Fatalf("rrif list = %+v, want empty (non-null) array", got)
	}

	for _, q := range []string{"?plan_type=xyz", "?year=abc"} {
		code, body = doRaw(t, h, http.MethodGet, list+q, "", token)
		if code != http.StatusBadRequest {
			t.Fatalf("list %s status = %d, want 400 (body %s)", q, code, body)
		}
		wantErrEnvelope(t, body, codeInvalidRequest)
	}

	// 8. Delete.
	code, body = doRaw(t, h, http.MethodDelete, list+"/lo1", "", token)
	if code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204 (body %s)", code, body)
	}
	code, body = doRaw(t, h, http.MethodGet, list+"?plan_type=tfsa", "", token)
	if code != http.StatusOK {
		t.Fatalf("list status = %d, want 200 (body %s)", code, body)
	}
	if got := decodeLimits(t, body); len(got) != 0 {
		t.Fatalf("tfsa list after delete = %+v, want empty (non-null) array", got)
	}
	code, body = doRaw(t, h, http.MethodDelete, list+"/lo1", "", token)
	if code != http.StatusNotFound {
		t.Fatalf("re-delete status = %d, want 404 (body %s)", code, body)
	}
	wantErrEnvelope(t, body, codeNotFound)
}

func TestSavingsAdjustments(t *testing.T) {
	h, token := newLimitsTest(t, &statePlans{})
	const list = "/api/v1/savings/adjustments"

	// 1. Create a negative adjustment (room reclaimed).
	code, body := doRaw(t, h, http.MethodPost, list, `{"plan_type":"tfsa","year":2025,"amount":"-2000.00","note":"room reclaimed from 2024"}`, token)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (body %s)", code, body)
	}
	w := decodeAdjustmentOne(t, body)
	if w.ID != "ra1" || w.PlanType != "tfsa" || w.Year != 2025 || w.Amount != "-2000.00" || w.Note != "room reclaimed from 2024" {
		t.Fatalf("create = %+v, want id ra1/tfsa/2025/-2000.00 with the note", w)
	}
	wantRFC3339(t, "created_at", w.CreatedAt)

	// 2. A second adjustment with an empty note.
	code, body = doRaw(t, h, http.MethodPost, list, `{"plan_type":"rrsp","year":2026,"amount":"1000.00","note":""}`, token)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (body %s)", code, body)
	}
	w = decodeAdjustmentOne(t, body)
	if w.ID != "ra2" || w.Amount != "1000.00" || w.Note != "" {
		t.Fatalf("create = %+v, want id ra2/1000.00/empty note", w)
	}

	// 3. Validation.
	longNote := strings.Repeat("x", 501)
	for _, b := range []string{
		`{"plan_type":"gic","year":2026,"amount":"1.00","note":""}`,
		`{"plan_type":"tfsa","year":1899,"amount":"1.00","note":""}`,
		`{"plan_type":"tfsa","year":2026,"amount":"0.00","note":""}`,
		`{"plan_type":"tfsa","year":2026,"amount":"abc","note":""}`,
		`{"plan_type":"tfsa","year":2026,"amount":"1.00","note":"` + longNote + `"}`,
	} {
		code, body = doRaw(t, h, http.MethodPost, list, b, token)
		if code != http.StatusBadRequest {
			t.Fatalf("create %s... status = %d, want 400 (body %s)", b[:32], code, body)
		}
		wantErrEnvelope(t, body, codeInvalidRequest)
	}

	// 4. The list holds both adjustments.
	code, body = doRaw(t, h, http.MethodGet, list, "", token)
	if code != http.StatusOK {
		t.Fatalf("list status = %d, want 200 (body %s)", code, body)
	}
	if got := decodeAdjustments(t, body); len(got) != 2 {
		t.Fatalf("adjustments = %d entries, want 2 (body %s)", len(got), body)
	}
	code, body = doRaw(t, h, http.MethodGet, list+"?plan_type=rrsp", "", token)
	if code != http.StatusOK || len(decodeAdjustments(t, body)) != 1 {
		t.Fatalf("rrsp list status = %d body %s, want 1 entry", code, body)
	}
	code, body = doRaw(t, h, http.MethodGet, list+"?plan_type=rrsp&year=2025", "", token)
	if code != http.StatusOK {
		t.Fatalf("rrsp/2025 list status = %d, want 200 (body %s)", code, body)
	}
	if got := decodeAdjustments(t, body); len(got) != 0 {
		t.Fatalf("rrsp/2025 list = %+v, want empty (non-null) array", got)
	}
	code, body = doRaw(t, h, http.MethodGet, list+"?plan_type=rrif", "", token)
	if code != http.StatusOK {
		t.Fatalf("rrif list status = %d, want 200 (body %s)", code, body)
	}
	if got := decodeAdjustments(t, body); len(got) != 0 {
		t.Fatalf("rrif list = %+v, want empty (non-null) array", got)
	}

	// 5. Delete.
	code, body = doRaw(t, h, http.MethodDelete, list+"/ra1", "", token)
	if code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204 (body %s)", code, body)
	}
	code, body = doRaw(t, h, http.MethodGet, list, "", token)
	if code != http.StatusOK {
		t.Fatalf("list status = %d, want 200 (body %s)", code, body)
	}
	if got := decodeAdjustments(t, body); len(got) != 1 {
		t.Fatalf("adjustments after delete = %d entries, want 1 (body %s)", len(got), body)
	}
	code, body = doRaw(t, h, http.MethodDelete, list+"/ra1", "", token)
	if code != http.StatusNotFound {
		t.Fatalf("re-delete status = %d, want 404 (body %s)", code, body)
	}
	wantErrEnvelope(t, body, codeNotFound)
}

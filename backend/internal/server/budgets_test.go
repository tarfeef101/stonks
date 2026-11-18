// Route tests for the budget endpoints (plan step 3.3): GET/POST
// /api/v1/budgets and PATCH/DELETE /api/v1/budgets/{id}. All in-memory:
// memBudgetStore mirrors the repo.BudgetStore semantics the handlers
// depend on (user-id scoping, every target key must be the caller's own
// category, names trimmed on write, active defaults to true, immutable
// period, no-op patches keep updated_at) and injects per-operation
// errors for the mapping table. The shared helpers (authHdr, apiErr,
// idFrom, nextTestID, setOne, doReq) live in accounts_test.go,
// transactions_test.go, and settings_test.go.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"finance-tracker/internal/repo"

	"github.com/lestrrat-go/jwx/v3/jwk"
)

// memBudgetRow is one fake-store row.
type memBudgetRow struct {
	userID string
	b      repo.Budget
}

// memBudgetStore mirrors the repo.BudgetStore semantics the handlers
// depend on: rows are scoped by user id (a foreign id is treated as
// absent — existence is not disclosed); every target key must be the
// caller's own category (repo.ErrCategoryNotFound otherwise); names are
// trimmed on write; active defaults to true on create; a PATCH that
// changes nothing (trimmed name, active, and targets all unchanged) is
// a no-op that leaves updated_at untouched; rows come back in insertion
// order, standing in for the repo's created_at, id order.
type memBudgetStore struct {
	rows []memBudgetRow

	// ownCategory holds each user's category ids, standing in for the
	// user_categories rows the repo's ownership check queries.
	ownCategory map[string]map[string]bool

	listErr   error
	createErr error
	updateErr error
	deleteErr error
}

func (m *memBudgetStore) markCategory(userID, id string) {
	m.ownCategory[userID] = setOne(m.ownCategory[userID], id)
}

// checkTargets enforces the ownership rule the repo applies on create
// AND update (the handlers 400 the shape violations before any store
// call, so the shape is assumed valid here).
func (m *memBudgetStore) checkTargets(userID string, targets json.RawMessage) error {
	var obj map[string]any
	if err := json.Unmarshal(targets, &obj); err != nil {
		return repo.ErrCategoryNotFound
	}
	own := m.ownCategory[userID]
	for k := range obj {
		if !own[k] {
			return repo.ErrCategoryNotFound
		}
	}
	return nil
}

func (m *memBudgetStore) List(_ context.Context, userID string, _ []byte) ([]repo.Budget, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	out := []repo.Budget{}
	for _, r := range m.rows {
		if r.userID == userID {
			out = append(out, r.b)
		}
	}
	return out, nil
}

// ActiveTargets stands in for the repo's SQL-side period/active filter
// (the summary handler no longer filters in Go); like List it honors
// listErr and the rows' insertion order.
func (m *memBudgetStore) ActiveTargets(_ context.Context, userID, period string, _ []byte) ([]repo.ActiveBudget, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	out := []repo.ActiveBudget{}
	for _, r := range m.rows {
		if r.userID == userID && r.b.Active && r.b.Period == period {
			out = append(out, repo.ActiveBudget{ID: r.b.ID, Targets: r.b.Targets})
		}
	}
	return out, nil
}

func (m *memBudgetStore) Create(_ context.Context, userID string, in repo.BudgetIn, _ []byte) (repo.Budget, error) {
	if m.createErr != nil {
		return repo.Budget{}, m.createErr
	}
	if err := m.checkTargets(userID, in.Targets); err != nil {
		return repo.Budget{}, err
	}
	active := true
	if in.Active != nil {
		active = *in.Active
	}
	now := time.Now().UTC()
	b := repo.Budget{
		ID: nextTestID(), Name: strings.TrimSpace(in.Name), Period: in.Period,
		Active: active, Targets: in.Targets, CreatedAt: now, UpdatedAt: now,
	}
	m.rows = append(m.rows, memBudgetRow{userID: userID, b: b})
	return b, nil
}

func (m *memBudgetStore) Update(_ context.Context, userID, id string, p repo.BudgetPatch, _ []byte) (repo.Budget, error) {
	if m.updateErr != nil {
		return repo.Budget{}, m.updateErr
	}
	idx := -1
	for i, r := range m.rows {
		if r.userID == userID && r.b.ID == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		return repo.Budget{}, repo.ErrBudgetNotFound
	}
	cur := m.rows[idx].b
	nameChanged := p.Name != nil && strings.TrimSpace(*p.Name) != cur.Name
	activeChanged := p.Active != nil && *p.Active != cur.Active
	targetsChanged := false
	if p.Targets != nil {
		// The repo re-checks ownership before the no-op decision, so an
		// unowned key 404s even when the bytes are unchanged.
		if err := m.checkTargets(userID, *p.Targets); err != nil {
			return repo.Budget{}, err
		}
		targetsChanged = !bytes.Equal(*p.Targets, cur.Targets)
	}
	if !nameChanged && !activeChanged && !targetsChanged {
		// No-op patch: the row comes back without an UPDATE.
		return cur, nil
	}
	if nameChanged {
		cur.Name = strings.TrimSpace(*p.Name)
	}
	if activeChanged {
		cur.Active = *p.Active
	}
	if targetsChanged {
		cur.Targets = *p.Targets
	}
	cur.UpdatedAt = time.Now().UTC()
	m.rows[idx].b = cur
	return cur, nil
}

func (m *memBudgetStore) Delete(_ context.Context, userID, id string) error {
	if m.deleteErr != nil {
		return m.deleteErr
	}
	for i, r := range m.rows {
		if r.userID == userID && r.b.ID == id {
			m.rows = append(m.rows[:i], m.rows[i+1:]...)
			return nil
		}
	}
	return repo.ErrBudgetNotFound
}

// newBudgetServer builds a full router with a live token verifier, an
// in-memory bootstrap, and the given budget store (nil allowed — that
// is the 503 path). The key is returned so tests can mint tokens for
// several users under the same verifier.
func newBudgetServer(t *testing.T, store BudgetStore) (http.Handler, jwk.Key) {
	t.Helper()
	key := mintKey(t)
	b, err := NewBootstrap(newMemStore(), testKEK(t), nil)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return New(fakeReady{}, BuildInfo{}, liveAuth(t, key), b, nil, nil, nil, nil, store, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, false, nil, nil, nil, nil), key
}

// TestBudgetsCRUD covers the happy path end to end: create (201, trimmed
// name, default active, no bookkeeping timestamps, targets passed
// through), list (creation order), patch (name/active/targets applied,
// period in the body ignored, no-op keeps updated_at), delete (204,
// empty body, gone from the list).
func TestBudgetsCRUD(t *testing.T) {
	c1, c2 := nextTestID(), nextTestID()
	store := &memBudgetStore{ownCategory: map[string]map[string]bool{}}
	store.markCategory("id-bud-a", c1)
	store.markCategory("id-bud-a", c2)
	h, key := newBudgetServer(t, store)
	hdr := authHdr(mintIdentityToken(t, key, "bud-a", "a@localhost", "A", "a"))

	// active omitted = default true; the name is trimmed on write.
	code, out := doReq(t, h, http.MethodPost, "/api/v1/budgets",
		`{"name":"  groceries  ","period":"month","targets":{"`+c1+`":100,"`+c2+`":5.55}}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201 (body %s)", code, out)
	}
	id1 := idFrom(t, out)
	var row map[string]any
	if err := json.Unmarshal([]byte(out), &row); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if row["name"] != "groceries" {
		t.Errorf("name = %v, want trimmed \"groceries\"", row["name"])
	}
	if row["period"] != "month" || row["active"] != true {
		t.Errorf("period/active = %v/%v, want month/true (the default)", row["period"], row["active"])
	}
	if _, ok := row["created_at"]; ok {
		t.Error("body carries created_at (bookkeeping stays server-side)")
	}
	if _, ok := row["updated_at"]; ok {
		t.Error("body carries updated_at (bookkeeping stays server-side)")
	}
	tg, ok := row["targets"].(map[string]any)
	if !ok {
		t.Fatalf("targets is not an object (body %s)", out)
	}
	if len(tg) != 2 || tg[c1] != float64(100) || tg[c2] != 5.55 {
		t.Errorf("targets = %v, want %s:100 and %s:5.55", tg, c1, c2)
	}

	code, out = doReq(t, h, http.MethodPost, "/api/v1/budgets",
		`{"name":"payroll","period":"quarter","active":false,"targets":{"`+c1+`":7}}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201 (body %s)", code, out)
	}
	id2 := idFrom(t, out)
	row = nil
	if err := json.Unmarshal([]byte(out), &row); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if row["active"] != false {
		t.Errorf("active = %v, want false", row["active"])
	}

	code, out = doReq(t, h, http.MethodGet, "/api/v1/budgets", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("list: status = %d, want 200 (body %s)", code, out)
	}
	var rows []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if len(rows) != 2 || rows[0].ID != id1 || rows[1].ID != id2 {
		t.Fatalf("list = %+v, want [%s, %s] in creation order", rows, id1, id2)
	}
	if rows[0].Name != "groceries" {
		t.Errorf("first name = %q, want \"groceries\"", rows[0].Name)
	}

	// Patch: name applied, period in the body ignored (immutable).
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/budgets/"+id1, `{"name":"Groceries","period":"year"}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("patch: status = %d, want 200 (body %s)", code, out)
	}
	row = nil
	if err := json.Unmarshal([]byte(out), &row); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if row["name"] != "Groceries" {
		t.Errorf("name = %v, want \"Groceries\"", row["name"])
	}
	if row["period"] != "month" {
		t.Errorf("period = %v, want immutable \"month\" (ignored in the PATCH body)", row["period"])
	}

	// No-op patch (name unchanged after trim): updated_at stays put.
	before := store.rows[0].b.UpdatedAt
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/budgets/"+id1, `{"name":"Groceries"}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("no-op patch: status = %d, want 200 (body %s)", code, out)
	}
	if after := store.rows[0].b.UpdatedAt; !after.Equal(before) {
		t.Errorf("no-op patch moved updated_at (%s -> %s)", before.Format(time.RFC3339Nano), after.Format(time.RFC3339Nano))
	}

	// Patch targets and active.
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/budgets/"+id1, `{"targets":{"`+c2+`":9}}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("patch targets: status = %d, want 200 (body %s)", code, out)
	}
	row = nil
	if err := json.Unmarshal([]byte(out), &row); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	tg, ok = row["targets"].(map[string]any)
	if !ok || len(tg) != 1 || tg[c2] != 9.0 {
		t.Errorf("targets = %v, want only %s:9", row["targets"], c2)
	}
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/budgets/"+id2, `{"active":true}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("patch active: status = %d, want 200 (body %s)", code, out)
	}
	row = nil
	if err := json.Unmarshal([]byte(out), &row); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if row["active"] != true {
		t.Errorf("active = %v, want true", row["active"])
	}

	// Delete: 204 with an empty body, gone from the list.
	code, out = doReq(t, h, http.MethodDelete, "/api/v1/budgets/"+id2, "", hdr)
	if code != http.StatusNoContent {
		t.Fatalf("delete: status = %d, want 204 (body %s)", code, out)
	}
	if out != "" {
		t.Errorf("delete body = %q, want empty", out)
	}
	code, out = doReq(t, h, http.MethodGet, "/api/v1/budgets", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("list after delete: status = %d, want 200 (body %s)", code, out)
	}
	rows = nil
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if len(rows) != 1 || rows[0].ID != id1 {
		t.Errorf("list after delete = %+v, want only %s", rows, id1)
	}
}

// TestBudgetsCreateValidation pins every create 400 (the repo's exact
// messages, all problems joined with "; " in name; period; targets
// order) and the non-object-body decode failure.
func TestBudgetsCreateValidation(t *testing.T) {
	c1 := nextTestID()
	store := &memBudgetStore{ownCategory: map[string]map[string]bool{}}
	store.markCategory("id-bud-v", c1)
	h, key := newBudgetServer(t, store)
	hdr := authHdr(mintIdentityToken(t, key, "bud-v", "v@localhost", "V", "v"))

	cases := []struct {
		name string
		body string
		want string
	}{
		{"empty name", `{"name":"   ","period":"month","targets":{"` + c1 + `":1}}`, "name must not be empty"},
		{"long name", `{"name":"` + strings.Repeat("a", 256) + `","period":"month","targets":{"` + c1 + `":1}}`, "name must be at most 255 characters"},
		{"bad period", `{"name":"x","period":"week","targets":{"` + c1 + `":1}}`, "period must be one of month, quarter, year"},
		{"omitted targets", `{"name":"x","period":"month"}`, "targets must be a non-empty JSON document"},
		{"null targets", `{"name":"x","period":"month","targets":null}`, "targets must name at least one category"},
		{"empty object targets", `{"name":"x","period":"month","targets":{}}`, "targets must name at least one category"},
		{"non-object targets", `{"name":"x","period":"month","targets":[1]}`, "targets must be a JSON object"},
		{"malformed key", `{"name":"x","period":"month","targets":{"nope":1}}`, `target key "nope" is not a category id`},
		{"non-number value", `{"name":"x","period":"month","targets":{"` + c1 + `":"big"}}`, "target for " + c1 + " must be a number"},
		{"negative value", `{"name":"x","period":"month","targets":{"` + c1 + `":-1}}`, "target for " + c1 + " must be between 0 and 999999999999.99"},
		{"all problems joined", `{"name":"  ","period":"week","targets":{}}`,
			"name must not be empty; period must be one of month, quarter, year; targets must name at least one category"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out := doReq(t, h, http.MethodPost, "/api/v1/budgets", tc.body, hdr)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", code, out)
			}
			if c, m := apiErr(t, out); c != codeInvalidRequest || m != tc.want {
				t.Errorf("error = %s/%q, want %s/%q", c, m, codeInvalidRequest, tc.want)
			}
		})
	}
	// A non-object body fails at decode, before field validation.
	for _, body := range []string{`not json`, `[]`} {
		t.Run("non-object body "+body, func(t *testing.T) {
			code, out := doReq(t, h, http.MethodPost, "/api/v1/budgets", body, hdr)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", code, out)
			}
			if c, m := apiErr(t, out); c != codeInvalidRequest || !strings.HasPrefix(m, "body must be a JSON object: ") {
				t.Errorf("error = %s/%q, want %s/%q…", c, m, codeInvalidRequest, "body must be a JSON object: ")
			}
		})
	}
	if len(store.rows) != 0 {
		t.Errorf("validation 400s left %d rows behind", len(store.rows))
	}
}

// TestBudgetsPatchValidation pins every patch 400: absent and null
// fields collapse into the empty patch, and the set fields get the same
// per-field messages as create.
func TestBudgetsPatchValidation(t *testing.T) {
	c1 := nextTestID()
	store := &memBudgetStore{ownCategory: map[string]map[string]bool{}}
	store.markCategory("id-bud-p", c1)
	h, key := newBudgetServer(t, store)
	hdr := authHdr(mintIdentityToken(t, key, "bud-p", "p@localhost", "P", "p"))
	code, out := doReq(t, h, http.MethodPost, "/api/v1/budgets",
		`{"name":"base","period":"year","targets":{"`+c1+`":1}}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("seed: status = %d, want 201 (body %s)", code, out)
	}
	id := idFrom(t, out)

	cases := []struct {
		name string
		body string
		want string
	}{
		{"empty object", `{}`, "empty patch: provide at least one field"},
		{"null name", `{"name":null}`, "empty patch: provide at least one field"},
		{"null active", `{"active":null}`, "empty patch: provide at least one field"},
		{"null targets", `{"targets":null}`, "empty patch: provide at least one field"},
		{"empty name", `{"name":"  "}`, "name must not be empty"},
		{"long name", `{"name":"` + strings.Repeat("a", 256) + `"}`, "name must be at most 255 characters"},
		{"empty object targets", `{"targets":{}}`, "targets must name at least one category"},
		{"non-number value", `{"targets":{"` + c1 + `":"big"}}`, "target for " + c1 + " must be a number"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out := doReq(t, h, http.MethodPatch, "/api/v1/budgets/"+id, tc.body, hdr)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", code, out)
			}
			if c, m := apiErr(t, out); c != codeInvalidRequest || m != tc.want {
				t.Errorf("error = %s/%q, want %s/%q", c, m, codeInvalidRequest, tc.want)
			}
		})
	}
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/budgets/"+id, `not json`, hdr)
	if code != http.StatusBadRequest {
		t.Fatalf("non-object body: status = %d, want 400 (body %s)", code, out)
	}
	if c, m := apiErr(t, out); c != codeInvalidRequest || !strings.HasPrefix(m, "body must be a JSON object: ") {
		t.Errorf("error = %s/%q, want %s/%q…", c, m, codeInvalidRequest, "body must be a JSON object: ")
	}
}

// TestBudgetsErrors covers the 404s (malformed path id answered before
// the store is reached; unknown well-formed id) and the per-operation
// 500 mapping.
func TestBudgetsErrors(t *testing.T) {
	c1 := nextTestID()

	t.Run("malformed path id never reaches the store", func(t *testing.T) {
		// Injected errors would surface as 500s if the store were
		// called; the shape check must answer 404 first.
		store := &memBudgetStore{updateErr: errors.New("boom"), deleteErr: errors.New("boom")}
		h, key := newBudgetServer(t, store)
		hdr := authHdr(mintIdentityToken(t, key, "bud-e", "e@localhost", "E", "e"))
		for _, method := range []string{http.MethodPatch, http.MethodDelete} {
			body := ""
			if method == http.MethodPatch {
				body = `{"name":"x"}`
			}
			code, out := doReq(t, h, method, "/api/v1/budgets/nope", body, hdr)
			if code != http.StatusNotFound {
				t.Fatalf("%s: status = %d, want 404 (body %s)", method, code, out)
			}
			if c, m := apiErr(t, out); c != codeNotFound || m != "budget not found" {
				t.Errorf("%s: error = %s/%q, want %s/budget not found", method, c, m, codeNotFound)
			}
		}
	})

	t.Run("unknown well-formed id is 404", func(t *testing.T) {
		store := &memBudgetStore{ownCategory: map[string]map[string]bool{}}
		store.markCategory("id-bud-u", c1)
		h, key := newBudgetServer(t, store)
		hdr := authHdr(mintIdentityToken(t, key, "bud-u", "u@localhost", "U", "u"))
		id := nextTestID()
		code, out := doReq(t, h, http.MethodPatch, "/api/v1/budgets/"+id, `{"name":"x"}`, hdr)
		if code != http.StatusNotFound {
			t.Fatalf("patch: status = %d, want 404 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeNotFound || m != "budget not found" {
			t.Errorf("patch: error = %s/%q", c, m)
		}
		code, out = doReq(t, h, http.MethodDelete, "/api/v1/budgets/"+id, "", hdr)
		if code != http.StatusNotFound {
			t.Fatalf("delete: status = %d, want 404 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeNotFound || m != "budget not found" {
			t.Errorf("delete: error = %s/%q", c, m)
		}
	})

	t.Run("store errors are 500", func(t *testing.T) {
		boom := errors.New("boom")
		cases := []struct {
			name   string
			store  *memBudgetStore
			method string
			path   string
			body   string
		}{
			{"list", &memBudgetStore{listErr: boom}, http.MethodGet, "/api/v1/budgets", ""},
			{"create", &memBudgetStore{createErr: boom}, http.MethodPost, "/api/v1/budgets",
				`{"name":"x","period":"month","targets":{"` + c1 + `":1}}`},
			{"update", &memBudgetStore{updateErr: boom}, http.MethodPatch, "/api/v1/budgets/" + nextTestID(), `{"name":"x"}`},
			{"delete", &memBudgetStore{deleteErr: boom}, http.MethodDelete, "/api/v1/budgets/" + nextTestID(), ""},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				h, key := newBudgetServer(t, tc.store)
				hdr := authHdr(mintIdentityToken(t, key, "bud-500", "s@localhost", "S", "s"))
				code, out := doReq(t, h, tc.method, tc.path, tc.body, hdr)
				if code != http.StatusInternalServerError {
					t.Fatalf("status = %d, want 500 (body %s)", code, out)
				}
				if c, m := apiErr(t, out); c != codeInternal || m != "budget operation failed" {
					t.Errorf("error = %s/%q, want %s/budget operation failed", c, m, codeInternal)
				}
			})
		}
	})
}

// TestBudgetsCrossUser proves the user-id scoping: user B sees an empty
// list and gets 404s (not 403s) on user A's row, which stays untouched.
func TestBudgetsCrossUser(t *testing.T) {
	c1 := nextTestID()
	store := &memBudgetStore{ownCategory: map[string]map[string]bool{}}
	store.markCategory("id-bud-a", c1)
	h, key := newBudgetServer(t, store)
	hdrA := authHdr(mintIdentityToken(t, key, "bud-a", "a@localhost", "A", "a"))
	hdrB := authHdr(mintIdentityToken(t, key, "bud-b", "b@localhost", "B", "b"))

	code, out := doReq(t, h, http.MethodPost, "/api/v1/budgets",
		`{"name":"a-budget","period":"month","targets":{"`+c1+`":1}}`, hdrA)
	if code != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201 (body %s)", code, out)
	}
	id := idFrom(t, out)

	code, out = doReq(t, h, http.MethodGet, "/api/v1/budgets", "", hdrB)
	if code != http.StatusOK {
		t.Fatalf("list: status = %d, want 200 (body %s)", code, out)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if len(rows) != 0 {
		t.Errorf("B's list = %s, want empty", out)
	}
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/budgets/"+id, `{"name":"stolen"}`, hdrB)
	if code != http.StatusNotFound {
		t.Fatalf("patch: status = %d, want 404 (body %s)", code, out)
	}
	if c, m := apiErr(t, out); c != codeNotFound || m != "budget not found" {
		t.Errorf("patch: error = %s/%q", c, m)
	}
	code, out = doReq(t, h, http.MethodDelete, "/api/v1/budgets/"+id, "", hdrB)
	if code != http.StatusNotFound {
		t.Fatalf("delete: status = %d, want 404 (body %s)", code, out)
	}
	if c, m := apiErr(t, out); c != codeNotFound || m != "budget not found" {
		t.Errorf("delete: error = %s/%q", c, m)
	}
	if len(store.rows) != 1 || store.rows[0].b.Name != "a-budget" {
		t.Errorf("store = %+v, want A's row untouched", store.rows)
	}
}

// TestBudgetsForeignTargets proves the target ownership rule at the
// store boundary: a key owned by another user — or by nobody — is a
// 404 "category not found" on both create and update.
func TestBudgetsForeignTargets(t *testing.T) {
	cA := nextTestID()    // owned by A
	cB := nextTestID()    // owned by B
	ghost := nextTestID() // owned by nobody
	store := &memBudgetStore{ownCategory: map[string]map[string]bool{}}
	store.markCategory("id-bud-a", cA)
	store.markCategory("id-bud-b", cB)
	h, key := newBudgetServer(t, store)
	hdrA := authHdr(mintIdentityToken(t, key, "bud-a", "a@localhost", "A", "a"))

	t.Run("create with a foreign key is 404", func(t *testing.T) {
		code, out := doReq(t, h, http.MethodPost, "/api/v1/budgets",
			`{"name":"x","period":"month","targets":{"`+cB+`":1}}`, hdrA)
		if code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeNotFound || m != "category not found" {
			t.Errorf("error = %s/%q, want %s/category not found", c, m, codeNotFound)
		}
	})

	t.Run("create with an unknown key is 404", func(t *testing.T) {
		code, out := doReq(t, h, http.MethodPost, "/api/v1/budgets",
			`{"name":"x","period":"month","targets":{"`+ghost+`":1}}`, hdrA)
		if code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeNotFound || m != "category not found" {
			t.Errorf("error = %s/%q, want %s/category not found", c, m, codeNotFound)
		}
	})

	t.Run("update with a foreign key is 404", func(t *testing.T) {
		code, out := doReq(t, h, http.MethodPost, "/api/v1/budgets",
			`{"name":"ok","period":"month","targets":{"`+cA+`":1}}`, hdrA)
		if code != http.StatusCreated {
			t.Fatalf("seed: status = %d, want 201 (body %s)", code, out)
		}
		id := idFrom(t, out)
		code, out = doReq(t, h, http.MethodPatch, "/api/v1/budgets/"+id,
			`{"targets":{"`+cA+`":1,"`+cB+`":2}}`, hdrA)
		if code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeNotFound || m != "category not found" {
			t.Errorf("error = %s/%q, want %s/category not found", c, m, codeNotFound)
		}
	})
}

// TestBudgetsAuth covers the auth and 503 edges: missing/garbage tokens
// are 401 (status only — the body is the authn package's business), and
// a valid token against a nil store is a 503 on every route.
func TestBudgetsAuth(t *testing.T) {
	t.Run("missing and garbage tokens are 401", func(t *testing.T) {
		// testAuth verifies against a nil key set: every token fails.
		h := New(fakeReady{}, BuildInfo{}, testAuth(), nil, nil, nil, nil, nil, &memBudgetStore{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, false, nil, nil, nil, nil)
		if code, _ := doReq(t, h, http.MethodGet, "/api/v1/budgets", "", nil); code != http.StatusUnauthorized {
			t.Errorf("no token: status = %d, want 401", code)
		}
		if code, _ := doReq(t, h, http.MethodPost, "/api/v1/budgets", `{"name":"x"}`,
			map[string]string{"Authorization": "Bearer not.a.jwt"}); code != http.StatusUnauthorized {
			t.Errorf("garbage token: status = %d, want 401", code)
		}
	})

	t.Run("nil store is 503 with a valid token", func(t *testing.T) {
		h, key := newBudgetServer(t, nil)
		tok := mintIdentityToken(t, key, "bud-nc", "nc@localhost", "N", "nc")
		hdr := authHdr(tok)
		cases := []struct {
			method string
			path   string
		}{
			{http.MethodGet, "/api/v1/budgets"},
			{http.MethodPost, "/api/v1/budgets"},
			{http.MethodPatch, "/api/v1/budgets/" + nextTestID()},
			{http.MethodDelete, "/api/v1/budgets/" + nextTestID()},
		}
		for _, tc := range cases {
			code, out := doReq(t, h, tc.method, tc.path, "", hdr)
			if code != http.StatusServiceUnavailable {
				t.Errorf("%s %s: status = %d, want 503 (body %s)", tc.method, tc.path, code, out)
				continue
			}
			if c, m := apiErr(t, out); c != codeInternal || m != "database not configured" {
				t.Errorf("%s %s: envelope = %s/%q, want internal/database not configured", tc.method, tc.path, c, m)
			}
		}
	})
}

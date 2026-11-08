// Route tests for the dashboard card endpoints (plan step 6.5):
// GET/POST /api/v1/dashboard-cards, GET/PATCH/DELETE
// /api/v1/dashboard-cards/{id}, and PUT /api/v1/dashboard-cards/order.
// All in-memory: memCardStore mirrors the repo.DashboardCardStore
// semantics the handlers depend on (user-id scoping, names trimmed on
// write, sort_order = the caller's MAX+1 on create, no-op patches keep
// updated_at, SetOrder rewrites sort_order = index+1 with an ownership
// re-check) and injects per-operation errors for the mapping table. The
// shared helpers (authHdr, apiErr, idFrom, nextTestID, doReq) live in
// accounts_test.go and settings_test.go.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"finance-tracker/internal/repo"

	"github.com/lestrrat-go/jwx/v3/jwk"
)

// memCardRow is one fake-store row.
type memCardRow struct {
	userID string
	c      repo.DashboardCard
}

// memCardStore mirrors the repo.DashboardCardStore semantics the
// handlers depend on: rows are scoped by user id (a foreign id is
// treated as absent — existence is not disclosed); names are trimmed on
// write; a new row's sort_order is the caller's current MAX+1 (gaps
// from deletes are not re-packed); a PATCH that changes nothing
// (trimmed name, explore_state) is a no-op that leaves updated_at
// untouched; List comes back ordered by sort_order; SetOrder rewrites
// sort_order = index+1 and re-checks ownership first (a foreign id is
// ErrDashboardCardNotFound and the whole rewrite is rolled back).
type memCardStore struct {
	rows []memCardRow

	listErr     error
	getErr      error
	createErr   error
	updateErr   error
	deleteErr   error
	setOrderErr error
}

func (m *memCardStore) List(_ context.Context, userID string, _ []byte) ([]repo.DashboardCard, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	out := []repo.DashboardCard{}
	for _, r := range m.rows {
		if r.userID == userID {
			out = append(out, r.c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].SortOrder < out[j].SortOrder })
	return out, nil
}

func (m *memCardStore) Get(_ context.Context, userID, id string, _ []byte) (repo.DashboardCard, error) {
	if m.getErr != nil {
		return repo.DashboardCard{}, m.getErr
	}
	for _, r := range m.rows {
		if r.userID == userID && r.c.ID == id {
			return r.c, nil
		}
	}
	return repo.DashboardCard{}, repo.ErrDashboardCardNotFound
}

func (m *memCardStore) Create(_ context.Context, userID string, in repo.DashboardCardIn, _ []byte) (repo.DashboardCard, error) {
	if m.createErr != nil {
		return repo.DashboardCard{}, m.createErr
	}
	// sort_order = COALESCE(MAX(sort_order), 0) + 1 over the caller's
	// rows (the repo's single-INSERT rule, gaps included).
	maxSort := 0
	for _, r := range m.rows {
		if r.userID == userID && r.c.SortOrder > maxSort {
			maxSort = r.c.SortOrder
		}
	}
	now := time.Now().UTC()
	c := repo.DashboardCard{
		ID: nextTestID(), Name: strings.TrimSpace(in.Name),
		ExploreState: in.ExploreState, SortOrder: maxSort + 1,
		CreatedAt: now, UpdatedAt: now,
	}
	m.rows = append(m.rows, memCardRow{userID: userID, c: c})
	return c, nil
}

func (m *memCardStore) Update(_ context.Context, userID, id string, p repo.DashboardCardPatch, _ []byte) (repo.DashboardCard, error) {
	if m.updateErr != nil {
		return repo.DashboardCard{}, m.updateErr
	}
	idx := -1
	for i, r := range m.rows {
		if r.userID == userID && r.c.ID == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		return repo.DashboardCard{}, repo.ErrDashboardCardNotFound
	}
	cur := m.rows[idx].c
	nameChanged := p.Name != nil && strings.TrimSpace(*p.Name) != cur.Name
	stateChanged := p.ExploreState != nil && *p.ExploreState != cur.ExploreState
	if !nameChanged && !stateChanged {
		// No-op patch: the row comes back without an UPDATE.
		return cur, nil
	}
	if nameChanged {
		cur.Name = strings.TrimSpace(*p.Name)
	}
	if stateChanged {
		cur.ExploreState = *p.ExploreState
	}
	cur.UpdatedAt = time.Now().UTC()
	m.rows[idx].c = cur
	return cur, nil
}

func (m *memCardStore) Delete(_ context.Context, userID, id string) error {
	if m.deleteErr != nil {
		return m.deleteErr
	}
	for i, r := range m.rows {
		if r.userID == userID && r.c.ID == id {
			m.rows = append(m.rows[:i], m.rows[i+1:]...)
			return nil
		}
	}
	return repo.ErrDashboardCardNotFound
}

func (m *memCardStore) SetOrder(_ context.Context, userID string, ids []string) error {
	if m.setOrderErr != nil {
		return m.setOrderErr
	}
	// Ownership re-check before any write (the repo does the same in its
	// transaction): a foreign id fails the whole rewrite.
	idx := make([]int, 0, len(ids))
	for _, id := range ids {
		found := -1
		for i, r := range m.rows {
			if r.userID == userID && r.c.ID == id {
				found = i
				break
			}
		}
		if found == -1 {
			return repo.ErrDashboardCardNotFound
		}
		idx = append(idx, found)
	}
	for i, rowIdx := range idx {
		m.rows[rowIdx].c.SortOrder = i + 1
	}
	return nil
}

// newCardServer builds a full router with a live token verifier, an
// in-memory bootstrap, and the given card store (nil allowed — that is
// the 503 path). The key is returned so tests can mint tokens for
// several users under the same verifier.
func newCardServer(t *testing.T, store CardStore) (http.Handler, jwk.Key) {
	t.Helper()
	key := mintKey(t)
	b, err := NewBootstrap(newMemStore(), testKEK(t))
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return New(fakeReady{}, BuildInfo{}, liveAuth(t, key), b, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, store, nil), key
}

// TestCardsCRUD covers the happy path end to end: create (201, trimmed
// name, explore_state passthrough, sort_order 1/2, no bookkeeping
// timestamps), fresh list (body exactly []), list (sort_order order),
// get, patch (rename keeps state + sort_order; state keeps the name;
// no-op keeps updated_at), delete (204, empty body, 404s afterwards).
func TestCardsCRUD(t *testing.T) {
	store := &memCardStore{}
	h, key := newCardServer(t, store)
	hdr := authHdr(mintIdentityToken(t, key, "card-a", "a@localhost", "A", "a"))

	// Fresh user: the list body is exactly [] (not null).
	code, out := doReq(t, h, http.MethodGet, "/api/v1/dashboard-cards", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("list: status = %d, want 200 (body %s)", code, out)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("fresh list body = %q, want exactly []", out)
	}

	// The name is trimmed on write; explore_state passes through
	// untouched (the frontend's canonical string); sort_order starts at 1.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/dashboard-cards",
		`{"name":"  Fall spend  ","explore_state":"from=2026-09-01&to=2026-11-30&chart=bar"}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201 (body %s)", code, out)
	}
	id1 := idFrom(t, out)
	var row map[string]any
	if err := json.Unmarshal([]byte(out), &row); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if row["name"] != "Fall spend" {
		t.Errorf("name = %v, want trimmed \"Fall spend\"", row["name"])
	}
	if row["explore_state"] != "from=2026-09-01&to=2026-11-30&chart=bar" {
		t.Errorf("explore_state = %v, want the canonical string untouched", row["explore_state"])
	}
	if row["sort_order"] != float64(1) {
		t.Errorf("sort_order = %v, want 1", row["sort_order"])
	}
	if _, ok := row["created_at"]; ok {
		t.Error("body carries created_at (bookkeeping stays server-side)")
	}
	if _, ok := row["updated_at"]; ok {
		t.Error("body carries updated_at (bookkeeping stays server-side)")
	}

	// explore_state omitted = "" (the all-time default view); sort_order 2.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/dashboard-cards", `{"name":"All time"}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201 (body %s)", code, out)
	}
	id2 := idFrom(t, out)
	row = nil
	if err := json.Unmarshal([]byte(out), &row); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if row["explore_state"] != "" {
		t.Errorf("explore_state = %v, want \"\" for the omitted field", row["explore_state"])
	}
	if row["sort_order"] != float64(2) {
		t.Errorf("sort_order = %v, want 2", row["sort_order"])
	}

	// List: sort_order order.
	code, out = doReq(t, h, http.MethodGet, "/api/v1/dashboard-cards", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("list: status = %d, want 200 (body %s)", code, out)
	}
	var rows []struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		ExploreState string `json:"explore_state"`
		SortOrder    int    `json:"sort_order"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if len(rows) != 2 || rows[0].ID != id1 || rows[1].ID != id2 {
		t.Fatalf("list = %+v, want [%s, %s] in sort order", rows, id1, id2)
	}
	if rows[0].SortOrder != 1 || rows[1].SortOrder != 2 {
		t.Errorf("sort orders = %d/%d, want 1/2", rows[0].SortOrder, rows[1].SortOrder)
	}

	// Get by id.
	code, out = doReq(t, h, http.MethodGet, "/api/v1/dashboard-cards/"+id1, "", hdr)
	if code != http.StatusOK {
		t.Fatalf("get: status = %d, want 200 (body %s)", code, out)
	}
	row = nil
	if err := json.Unmarshal([]byte(out), &row); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if row["name"] != "Fall spend" {
		t.Errorf("name = %v, want \"Fall spend\"", row["name"])
	}

	// Patch: rename keeps explore_state and sort_order.
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/dashboard-cards/"+id2, `{"name":"All time (renamed)"}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("patch: status = %d, want 200 (body %s)", code, out)
	}
	row = nil
	if err := json.Unmarshal([]byte(out), &row); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if row["name"] != "All time (renamed)" {
		t.Errorf("name = %v, want \"All time (renamed)\"", row["name"])
	}
	if row["explore_state"] != "" || row["sort_order"] != float64(2) {
		t.Errorf("state/sort = %v/%v, want unchanged", row["explore_state"], row["sort_order"])
	}

	// Patch: new state keeps the name.
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/dashboard-cards/"+id1, `{"explore_state":"q=coffee&chart=pie"}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("patch: status = %d, want 200 (body %s)", code, out)
	}
	row = nil
	if err := json.Unmarshal([]byte(out), &row); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if row["explore_state"] != "q=coffee&chart=pie" || row["name"] != "Fall spend" {
		t.Errorf("state/name = %v/%v, want q=coffee&chart=pie/Fall spend", row["explore_state"], row["name"])
	}

	// No-op patch (name unchanged after trim): updated_at stays put.
	before := store.rows[0].c.UpdatedAt
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/dashboard-cards/"+id1, `{"name":"  Fall spend  "}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("no-op patch: status = %d, want 200 (body %s)", code, out)
	}
	if after := store.rows[0].c.UpdatedAt; !after.Equal(before) {
		t.Errorf("no-op patch moved updated_at (%s -> %s)", before.Format(time.RFC3339Nano), after.Format(time.RFC3339Nano))
	}

	// Delete: 204 with an empty body; get and re-delete are 404s.
	code, out = doReq(t, h, http.MethodDelete, "/api/v1/dashboard-cards/"+id1, "", hdr)
	if code != http.StatusNoContent {
		t.Fatalf("delete: status = %d, want 204 (body %s)", code, out)
	}
	if out != "" {
		t.Errorf("delete body = %q, want empty", out)
	}
	code, out = doReq(t, h, http.MethodGet, "/api/v1/dashboard-cards/"+id1, "", hdr)
	if code != http.StatusNotFound {
		t.Fatalf("get after delete: status = %d, want 404 (body %s)", code, out)
	}
	if c, m := apiErr(t, out); c != codeNotFound || m != "dashboard card not found" {
		t.Errorf("get after delete: error = %s/%q", c, m)
	}
	code, out = doReq(t, h, http.MethodDelete, "/api/v1/dashboard-cards/"+id1, "", hdr)
	if code != http.StatusNotFound {
		t.Fatalf("re-delete: status = %d, want 404 (body %s)", code, out)
	}
}

// TestCardsCreateValidation pins every create 400 (the exact messages,
// all problems joined with "; ") and the decode failures (a non-string
// explore_state or a non-object body dies in decodeBody with "body
// must be a JSON object: ...").
func TestCardsCreateValidation(t *testing.T) {
	store := &memCardStore{}
	h, key := newCardServer(t, store)
	hdr := authHdr(mintIdentityToken(t, key, "card-v", "v@localhost", "V", "v"))

	cases := []struct {
		name string
		body string
		want string
	}{
		{"omitted name", `{}`, "name must not be empty"},
		{"empty name", `{"name":"   "}`, "name must not be empty"},
		{"long name", `{"name":"` + strings.Repeat("a", 256) + `"}`, "name must be at most 255 characters"},
		{"long state", `{"name":"x","explore_state":"` + strings.Repeat("a", 1025) + `"}`, "explore_state must be at most 1024 characters"},
		{"all problems joined", `{"name":"  ","explore_state":"` + strings.Repeat("a", 1025) + `"}`,
			"name must not be empty; explore_state must be at most 1024 characters"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out := doReq(t, h, http.MethodPost, "/api/v1/dashboard-cards", tc.body, hdr)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", code, out)
			}
			if c, m := apiErr(t, out); c != codeInvalidRequest || m != tc.want {
				t.Errorf("error = %s/%q, want %s/%q", c, m, codeInvalidRequest, tc.want)
			}
		})
	}
	// A non-string explore_state (and a non-object body) fails at
	// decode, before field validation.
	for _, body := range []string{`{"name":"x","explore_state":123}`, `not json`, `[]`} {
		t.Run("decode failure "+body[:min(16, len(body))], func(t *testing.T) {
			code, out := doReq(t, h, http.MethodPost, "/api/v1/dashboard-cards", body, hdr)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", code, out)
			}
			if c, m := apiErr(t, out); c != codeInvalidRequest || !strings.HasPrefix(m, "body must be a JSON object: ") {
				t.Errorf("error = %s/%q, want %s/%q…", c, m, codeInvalidRequest, "body must be a JSON object: ")
			}
		})
	}
	// The 255/1024 boundaries are accepted.
	code, out := doReq(t, h, http.MethodPost, "/api/v1/dashboard-cards",
		`{"name":"`+strings.Repeat("a", 255)+`","explore_state":"`+strings.Repeat("a", 1024)+`"}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("boundary create: status = %d, want 201 (body %s)", code, out)
	}
	if len(store.rows) != 1 {
		t.Errorf("validation 400s left %d rows behind (want only the boundary row)", len(store.rows))
	}
}

// TestCardsPatchValidation pins every patch 400: absent and null fields
// collapse into the empty patch (unknown fields are ignored), and the
// set fields get the same per-field messages as create.
func TestCardsPatchValidation(t *testing.T) {
	store := &memCardStore{}
	h, key := newCardServer(t, store)
	hdr := authHdr(mintIdentityToken(t, key, "card-p", "p@localhost", "P", "p"))
	code, out := doReq(t, h, http.MethodPost, "/api/v1/dashboard-cards", `{"name":"base"}`, hdr)
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
		{"null state", `{"explore_state":null}`, "empty patch: provide at least one field"},
		{"unknown fields only", `{"bogus":1}`, "empty patch: provide at least one field"},
		{"empty name", `{"name":"  "}`, "name must not be empty"},
		{"empty-string name", `{"name":""}`, "name must not be empty"},
		{"long name", `{"name":"` + strings.Repeat("a", 256) + `"}`, "name must be at most 255 characters"},
		{"long state", `{"explore_state":"` + strings.Repeat("a", 1025) + `"}`, "explore_state must be at most 1024 characters"},
		{"all problems joined", `{"name":"  ","explore_state":"` + strings.Repeat("a", 1025) + `"}`,
			"name must not be empty; explore_state must be at most 1024 characters"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out := doReq(t, h, http.MethodPatch, "/api/v1/dashboard-cards/"+id, tc.body, hdr)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", code, out)
			}
			if c, m := apiErr(t, out); c != codeInvalidRequest || m != tc.want {
				t.Errorf("error = %s/%q, want %s/%q", c, m, codeInvalidRequest, tc.want)
			}
		})
	}
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/dashboard-cards/"+id, `not json`, hdr)
	if code != http.StatusBadRequest {
		t.Fatalf("non-object body: status = %d, want 400 (body %s)", code, out)
	}
	if c, m := apiErr(t, out); c != codeInvalidRequest || !strings.HasPrefix(m, "body must be a JSON object: ") {
		t.Errorf("error = %s/%q, want %s/%q…", c, m, codeInvalidRequest, "body must be a JSON object: ")
	}
}

// TestCardsErrors covers the 404s (malformed path id answered before
// the store is reached; unknown well-formed id) and the per-operation
// 500 mapping.
func TestCardsErrors(t *testing.T) {
	boom := errors.New("boom")

	t.Run("malformed path id never reaches the store", func(t *testing.T) {
		// Injected errors would surface as 500s if the store were
		// called; the shape check must answer 404 first.
		store := &memCardStore{getErr: boom, updateErr: boom, deleteErr: boom}
		h, key := newCardServer(t, store)
		hdr := authHdr(mintIdentityToken(t, key, "card-e", "e@localhost", "E", "e"))
		cases := []struct {
			method string
			body   string
		}{
			{http.MethodGet, ""},
			{http.MethodPatch, `{"name":"x"}`},
			{http.MethodDelete, ""},
		}
		for _, tc := range cases {
			code, out := doReq(t, h, tc.method, "/api/v1/dashboard-cards/nope", tc.body, hdr)
			if code != http.StatusNotFound {
				t.Fatalf("%s: status = %d, want 404 (body %s)", tc.method, code, out)
			}
			if c, m := apiErr(t, out); c != codeNotFound || m != "dashboard card not found" {
				t.Errorf("%s: error = %s/%q, want %s/dashboard card not found", tc.method, c, m, codeNotFound)
			}
		}
	})

	t.Run("unknown well-formed id is 404", func(t *testing.T) {
		store := &memCardStore{}
		h, key := newCardServer(t, store)
		hdr := authHdr(mintIdentityToken(t, key, "card-u", "u@localhost", "U", "u"))
		id := nextTestID()
		code, out := doReq(t, h, http.MethodGet, "/api/v1/dashboard-cards/"+id, "", hdr)
		if code != http.StatusNotFound {
			t.Fatalf("get: status = %d, want 404 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeNotFound || m != "dashboard card not found" {
			t.Errorf("get: error = %s/%q", c, m)
		}
		code, out = doReq(t, h, http.MethodPatch, "/api/v1/dashboard-cards/"+id, `{"name":"x"}`, hdr)
		if code != http.StatusNotFound {
			t.Fatalf("patch: status = %d, want 404 (body %s)", code, out)
		}
		code, out = doReq(t, h, http.MethodDelete, "/api/v1/dashboard-cards/"+id, "", hdr)
		if code != http.StatusNotFound {
			t.Fatalf("delete: status = %d, want 404 (body %s)", code, out)
		}
	})

	t.Run("store errors are 500", func(t *testing.T) {
		cases := []struct {
			name   string
			store  *memCardStore
			method string
			path   string
			body   string
		}{
			{"list", &memCardStore{listErr: boom}, http.MethodGet, "/api/v1/dashboard-cards", ""},
			{"get", &memCardStore{getErr: boom}, http.MethodGet, "/api/v1/dashboard-cards/" + nextTestID(), ""},
			{"create", &memCardStore{createErr: boom}, http.MethodPost, "/api/v1/dashboard-cards", `{"name":"x"}`},
			{"update", &memCardStore{updateErr: boom}, http.MethodPatch, "/api/v1/dashboard-cards/" + nextTestID(), `{"name":"x"}`},
			{"delete", &memCardStore{deleteErr: boom}, http.MethodDelete, "/api/v1/dashboard-cards/" + nextTestID(), ""},
			// The empty store + "[]" passes every check, so SetOrder is
			// reached and its error maps to 500.
			{"setorder", &memCardStore{setOrderErr: boom}, http.MethodPut, "/api/v1/dashboard-cards/order", `[]`},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				h, key := newCardServer(t, tc.store)
				hdr := authHdr(mintIdentityToken(t, key, "card-500", "s@localhost", "S", "s"))
				code, out := doReq(t, h, tc.method, tc.path, tc.body, hdr)
				if code != http.StatusInternalServerError {
					t.Fatalf("status = %d, want 500 (body %s)", code, out)
				}
				if c, m := apiErr(t, out); c != codeInternal || m != "dashboard card operation failed" {
					t.Errorf("error = %s/%q, want %s/dashboard card operation failed", c, m, codeInternal)
				}
			})
		}
	})
}

// TestCardsOrder covers PUT /api/v1/dashboard-cards/order: the pinned
// check order (array shape; id shape; ownership; count + duplicates;
// apply), the 204 empty-body responses, the store rewrite, and the
// create-after-delete sort_order rule over the REMAINING rows.
func TestCardsOrder(t *testing.T) {
	t.Run("empty array against zero cards is 204", func(t *testing.T) {
		h, key := newCardServer(t, &memCardStore{})
		hdr := authHdr(mintIdentityToken(t, key, "card-o0", "z@localhost", "Z", "z"))
		code, out := doReq(t, h, http.MethodPut, "/api/v1/dashboard-cards/order", `[]`, hdr)
		if code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204 (body %s)", code, out)
		}
		if out != "" {
			t.Errorf("body = %q, want empty", out)
		}
	})

	store := &memCardStore{}
	h, key := newCardServer(t, store)
	hdr := authHdr(mintIdentityToken(t, key, "card-o", "o@localhost", "O", "o"))
	code, out := doReq(t, h, http.MethodPost, "/api/v1/dashboard-cards", `{"name":"Fall spend"}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("seed: status = %d, want 201 (body %s)", code, out)
	}
	id1 := idFrom(t, out)
	code, out = doReq(t, h, http.MethodPost, "/api/v1/dashboard-cards", `{"name":"All time"}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("seed: status = %d, want 201 (body %s)", code, out)
	}
	id2 := idFrom(t, out)
	fake := nextTestID() // well-formed uuid, no such card

	t.Run("reorder applies and the list re-sorts", func(t *testing.T) {
		code, out := doReq(t, h, http.MethodPut, "/api/v1/dashboard-cards/order",
			`["`+id2+`","`+id1+`"]`, hdr)
		if code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204 (body %s)", code, out)
		}
		if out != "" {
			t.Errorf("body = %q, want empty", out)
		}
		code, out = doReq(t, h, http.MethodGet, "/api/v1/dashboard-cards", "", hdr)
		if code != http.StatusOK {
			t.Fatalf("list: status = %d, want 200 (body %s)", code, out)
		}
		var rows []struct {
			Name      string `json:"name"`
			SortOrder int    `json:"sort_order"`
		}
		if err := json.Unmarshal([]byte(out), &rows); err != nil {
			t.Fatalf("body is not JSON: %v (%s)", err, out)
		}
		if len(rows) != 2 || rows[0].Name != "All time" || rows[0].SortOrder != 1 ||
			rows[1].Name != "Fall spend" || rows[1].SortOrder != 2 {
			t.Fatalf("list = %+v, want All time(1), Fall spend(2)", rows)
		}
	})

	orderCases := []struct {
		name   string
		body   string
		status int
		c      string
		want   string
		prefix bool
	}{
		{"not json", "nope", http.StatusBadRequest, codeInvalidRequest, "body must be a JSON array: ", true},
		{"non-array elements", `[123]`, http.StatusBadRequest, codeInvalidRequest, "body must be a JSON array: ", true},
		{"object", `{"id":"x"}`, http.StatusBadRequest, codeInvalidRequest, "body must be a JSON array: ", true},
		{"malformed id", `["nope"]`, http.StatusBadRequest, codeInvalidRequest, "order must contain only card ids", false},
		// The id-shape check runs BEFORE ownership, even when the first
		// element is a real-but-foreign-looking unknown.
		{"shape before ownership", `["` + fake + `","nope"]`, http.StatusBadRequest, codeInvalidRequest, "order must contain only card ids", false},
		{"foreign id is 404", `["` + id1 + `","` + fake + `"]`, http.StatusNotFound, codeNotFound, "dashboard card not found", false},
		// Ownership beats the count check: one foreign id is a 404 even
		// though the list is also short.
		{"foreign id beats count mismatch", `["` + fake + `"]`, http.StatusNotFound, codeNotFound, "dashboard card not found", false},
		{"missing id", `["` + id1 + `"]`, http.StatusBadRequest, codeInvalidRequest, "order must list every card exactly once", false},
		{"duplicate id", `["` + id1 + `","` + id1 + `"]`, http.StatusBadRequest, codeInvalidRequest, "order must list every card exactly once", false},
		{"empty array with cards", `[]`, http.StatusBadRequest, codeInvalidRequest, "order must list every card exactly once", false},
	}
	for _, tc := range orderCases {
		t.Run(tc.name, func(t *testing.T) {
			code, out := doReq(t, h, http.MethodPut, "/api/v1/dashboard-cards/order", tc.body, hdr)
			if code != tc.status {
				t.Fatalf("status = %d, want %d (body %s)", code, tc.status, out)
			}
			c, m := apiErr(t, out)
			if c != tc.c {
				t.Errorf("code = %q, want %q", c, tc.c)
				return
			}
			if tc.prefix {
				if !strings.HasPrefix(m, tc.want) {
					t.Errorf("message = %q, want prefix %q", m, tc.want)
				}
			} else if m != tc.want {
				t.Errorf("message = %q, want %q", m, tc.want)
			}
		})
	}

	// The failed orders left the store exactly as the reorder left it
	// (rows keep their creation positions; id2 holds 1, id1 holds 2).
	if store.rows[0].c.SortOrder != 2 || store.rows[1].c.SortOrder != 1 {
		t.Errorf("store sort orders = %d/%d, want 2/1 (failed orders are no-ops)",
			store.rows[0].c.SortOrder, store.rows[1].c.SortOrder)
	}

	t.Run("create after delete takes max+1 over remaining rows", func(t *testing.T) {
		// Delete Fall spend (sort_order 2); All time holds 1.
		code, out := doReq(t, h, http.MethodDelete, "/api/v1/dashboard-cards/"+id1, "", hdr)
		if code != http.StatusNoContent {
			t.Fatalf("delete: status = %d, want 204 (body %s)", code, out)
		}
		code, out = doReq(t, h, http.MethodPost, "/api/v1/dashboard-cards", `{"name":"temp"}`, hdr)
		if code != http.StatusCreated {
			t.Fatalf("create: status = %d, want 201 (body %s)", code, out)
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(out), &row); err != nil {
			t.Fatalf("body is not JSON: %v (%s)", err, out)
		}
		if row["sort_order"] != float64(2) {
			t.Errorf("sort_order = %v, want 2 (max over the remaining rows)", row["sort_order"])
		}
	})
}

// TestCardsCrossUser proves the user-id scoping: user B sees an empty
// list and gets 404s (not 403s) on user A's row — including a PUT
// /order naming A's card — and the row stays untouched.
func TestCardsCrossUser(t *testing.T) {
	store := &memCardStore{}
	h, key := newCardServer(t, store)
	hdrA := authHdr(mintIdentityToken(t, key, "card-a", "a@localhost", "A", "a"))
	hdrB := authHdr(mintIdentityToken(t, key, "card-b", "b@localhost", "B", "b"))

	code, out := doReq(t, h, http.MethodPost, "/api/v1/dashboard-cards", `{"name":"a-card"}`, hdrA)
	if code != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201 (body %s)", code, out)
	}
	id := idFrom(t, out)

	code, out = doReq(t, h, http.MethodGet, "/api/v1/dashboard-cards", "", hdrB)
	if code != http.StatusOK {
		t.Fatalf("list: status = %d, want 200 (body %s)", code, out)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("B's list = %q, want exactly []", out)
	}
	code, out = doReq(t, h, http.MethodGet, "/api/v1/dashboard-cards/"+id, "", hdrB)
	if code != http.StatusNotFound {
		t.Fatalf("get: status = %d, want 404 (body %s)", code, out)
	}
	if c, m := apiErr(t, out); c != codeNotFound || m != "dashboard card not found" {
		t.Errorf("get: error = %s/%q", c, m)
	}
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/dashboard-cards/"+id, `{"name":"stolen"}`, hdrB)
	if code != http.StatusNotFound {
		t.Fatalf("patch: status = %d, want 404 (body %s)", code, out)
	}
	code, out = doReq(t, h, http.MethodDelete, "/api/v1/dashboard-cards/"+id, "", hdrB)
	if code != http.StatusNotFound {
		t.Fatalf("delete: status = %d, want 404 (body %s)", code, out)
	}
	code, out = doReq(t, h, http.MethodPut, "/api/v1/dashboard-cards/order", `["`+id+`"]`, hdrB)
	if code != http.StatusNotFound {
		t.Fatalf("order: status = %d, want 404 (body %s)", code, out)
	}
	if c, m := apiErr(t, out); c != codeNotFound || m != "dashboard card not found" {
		t.Errorf("order: error = %s/%q", c, m)
	}
	if len(store.rows) != 1 || store.rows[0].c.Name != "a-card" {
		t.Errorf("store = %+v, want A's row untouched", store.rows)
	}
}

// TestCardsAuth covers the auth and 503 edges: missing/garbage tokens
// are 401 (status only — the body is the authn package's business), and
// a valid token against a nil store is a 503 on every route.
func TestCardsAuth(t *testing.T) {
	t.Run("missing and garbage tokens are 401", func(t *testing.T) {
		// testAuth verifies against a nil key set: every token fails.
		h := New(fakeReady{}, BuildInfo{}, testAuth(), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, &memCardStore{}, nil)
		if code, _ := doReq(t, h, http.MethodGet, "/api/v1/dashboard-cards", "", nil); code != http.StatusUnauthorized {
			t.Errorf("no token: status = %d, want 401", code)
		}
		if code, _ := doReq(t, h, http.MethodPut, "/api/v1/dashboard-cards/order", `[]`,
			map[string]string{"Authorization": "Bearer not.a.jwt"}); code != http.StatusUnauthorized {
			t.Errorf("garbage token: status = %d, want 401", code)
		}
	})

	t.Run("nil store is 503 with a valid token", func(t *testing.T) {
		h, key := newCardServer(t, nil)
		tok := mintIdentityToken(t, key, "card-nc", "nc@localhost", "N", "nc")
		hdr := authHdr(tok)
		cases := []struct {
			method string
			path   string
			body   string
		}{
			{http.MethodGet, "/api/v1/dashboard-cards", ""},
			{http.MethodPost, "/api/v1/dashboard-cards", `{"name":"x"}`},
			{http.MethodGet, "/api/v1/dashboard-cards/" + nextTestID(), ""},
			{http.MethodPatch, "/api/v1/dashboard-cards/" + nextTestID(), `{"name":"x"}`},
			{http.MethodDelete, "/api/v1/dashboard-cards/" + nextTestID(), ""},
			{http.MethodPut, "/api/v1/dashboard-cards/order", `[]`},
		}
		for _, tc := range cases {
			code, out := doReq(t, h, tc.method, tc.path, tc.body, hdr)
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

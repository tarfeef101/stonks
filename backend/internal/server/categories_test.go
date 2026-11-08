// Route tests for the category endpoints (plan step 3.1). All in-memory:
// memCategoryStore mirrors the repo.CategoryStore semantics the handlers
// depend on (user-id scoping, per-user case-sensitive uniqueness on the
// trimmed name, parent must be the caller's own row and is re-checked
// only when it actually changes, inner-nil parent demotes to root,
// active/sort_order defaults) and injects per-operation errors for the
// mapping table. The shared helpers (newCrudServer, authHdr, apiErr,
// idFrom, nextTestID) live in accounts_test.go.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"finance-tracker/internal/repo"
)

// memCategoryRow is one fake-store row. name is stored trimmed (the repo
// trims on write) and compared case-sensitively (the repo's name hash is
// exact-case); parent "" = root (the repo's NULL parent_id).
type memCategoryRow struct {
	userID string
	id     string
	name   string
	parent string
	active bool
	sort   int
}

func toRepoCategory(r memCategoryRow) repo.Category {
	return repo.Category{
		ID:        r.id,
		Name:      r.name,
		ParentID:  r.parent,
		Active:    r.active,
		SortOrder: r.sort,
	}
}

// memCategoryStore is the in-memory CategoryStore the route tests run
// against. It mirrors the repo semantics the handlers rely on — scoping
// on user id, per-user case-sensitive uniqueness of the trimmed name,
// the parent check (same-user, only when the parent changes, never for
// a demotion to root) — and can inject a per-operation error to exercise
// the handler's error mapping (real failures need a DB).
type memCategoryStore struct {
	rows      []memCategoryRow
	listErr   error
	createErr error
	updateErr error
	deleteErr error
}

func (m *memCategoryStore) exists(userID, id string) bool {
	for _, r := range m.rows {
		if r.userID == userID && r.id == id {
			return true
		}
	}
	return false
}

func (m *memCategoryStore) List(_ context.Context, userID string, _ []byte) ([]repo.Category, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	out := make([]repo.Category, 0, len(m.rows))
	for _, r := range m.rows { // insertion order stands in for created_at
		if r.userID == userID {
			out = append(out, toRepoCategory(r))
		}
	}
	// The repo orders by (sort_order, created_at): a stable sort keeps
	// insertion order among equal sort orders.
	slices.SortStableFunc(out, func(a, b repo.Category) int {
		if a.SortOrder == b.SortOrder {
			return 0
		}
		if a.SortOrder < b.SortOrder {
			return -1
		}
		return 1
	})
	return out, nil
}

func (m *memCategoryStore) Get(_ context.Context, userID, id string, _ []byte) (repo.Category, error) {
	for _, r := range m.rows {
		if r.userID == userID && r.id == id {
			return toRepoCategory(r), nil
		}
	}
	return repo.Category{}, repo.ErrCategoryNotFound
}

func (m *memCategoryStore) Create(_ context.Context, userID string, in repo.CategoryIn, _ []byte) (repo.Category, error) {
	if m.createErr != nil {
		return repo.Category{}, m.createErr
	}
	name := strings.TrimSpace(in.Name)
	for _, r := range m.rows {
		if r.userID == userID && r.name == name {
			return repo.Category{}, repo.ErrNameTaken
		}
	}
	parent := ""
	if in.ParentID != nil {
		parent = *in.ParentID
	}
	if parent != "" && !m.exists(userID, parent) {
		// A missing or other user's parent is ErrParentNotFound (repo).
		return repo.Category{}, repo.ErrParentNotFound
	}
	row := memCategoryRow{
		userID: userID, id: nextTestID(), name: name,
		parent: parent, active: true, sort: 0, // repo defaults
	}
	m.rows = append(m.rows, row)
	return toRepoCategory(row), nil
}

func (m *memCategoryStore) Update(_ context.Context, userID, id string, p repo.CategoryPatch, _ []byte) (repo.Category, error) {
	if m.updateErr != nil {
		return repo.Category{}, m.updateErr
	}
	idx := -1
	for i, r := range m.rows {
		if r.userID == userID && r.id == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return repo.Category{}, repo.ErrCategoryNotFound
	}
	r := m.rows[idx]
	if p.Name != nil {
		name := strings.TrimSpace(*p.Name)
		for _, o := range m.rows {
			if o.userID == userID && o.id != id && o.name == name {
				return repo.Category{}, repo.ErrNameTaken
			}
		}
		r.name = name
	}
	if p.ParentID != nil {
		want := ""
		if inner := *p.ParentID; inner != nil {
			want = *inner
		}
		if want != r.parent { // the repo re-checks only on a real change
			if want != "" && !m.exists(userID, want) {
				return repo.Category{}, repo.ErrParentNotFound
			}
			r.parent = want // "" = demote to root (no check, repo)
		}
	}
	if p.Active != nil {
		r.active = *p.Active
	}
	m.rows[idx] = r
	return toRepoCategory(r), nil
}

func (m *memCategoryStore) Delete(_ context.Context, userID, id string) error {
	if m.deleteErr != nil {
		return m.deleteErr
	}
	for i, r := range m.rows {
		if r.userID == userID && r.id == id {
			m.rows = append(m.rows[:i], m.rows[i+1:]...)
			return nil
		}
	}
	return repo.ErrCategoryNotFound
}

func TestCategoriesCRUD(t *testing.T) {
	store := &memCategoryStore{}
	h, key := newCrudServer(t, nil, store)
	token := mintIdentityToken(t, key, "cat-sub", "cat@localhost", "Cat", "cat")

	// An empty list is [] (a JSON array, not null). writeJSON's encoder
	// appends a trailing newline, so compare trimmed.
	code, body := doReq(t, h, http.MethodGet, "/api/v1/categories", "", authHdr(token))
	if code != http.StatusOK {
		t.Fatalf("GET categories status = %d, want 200 (body %s)", code, body)
	}
	if strings.TrimSpace(body) != "[]" {
		t.Fatalf("empty list = %s, want []", body)
	}

	// POST trims the name and answers 201 with the repo defaults:
	// active true, sort_order 0, root (parent_id is JSON null).
	code, body = doReq(t, h, http.MethodPost, "/api/v1/categories",
		`{"name":"  Groceries  "}`, authHdr(token))
	if code != http.StatusCreated {
		t.Fatalf("POST categories status = %d, want 201 (body %s)", code, body)
	}
	var created map[string]any
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("decode created category: %v (body %s)", err, body)
	}
	if created["name"] != "Groceries" {
		t.Errorf("created name = %v, want trimmed \"Groceries\"", created["name"])
	}
	if created["parent_id"] != nil {
		t.Errorf("created parent_id = %v, want null (root)", created["parent_id"])
	}
	if created["active"] != true {
		t.Errorf("created active = %v, want true", created["active"])
	}
	if created["sort_order"] != float64(0) {
		t.Errorf("created sort_order = %v, want 0", created["sort_order"])
	}
	id := idFrom(t, body)
	if !uuidShapeOK(id) {
		t.Fatalf("created id %q is not UUID-shaped", id)
	}

	// A child under the new root.
	code, body = doReq(t, h, http.MethodPost, "/api/v1/categories",
		fmt.Sprintf(`{"name":"Organic","parent_id":"%s"}`, id), authHdr(token))
	if code != http.StatusCreated {
		t.Fatalf("POST child status = %d, want 201 (body %s)", code, body)
	}
	childID := idFrom(t, body)
	if childID == id {
		t.Fatalf("child id equals the parent id")
	}

	// A duplicate (trimmed, exact-case) is 409 conflict.
	code, body = doReq(t, h, http.MethodPost, "/api/v1/categories",
		`{"name":"  Groceries "}`, authHdr(token))
	if code != http.StatusConflict {
		t.Fatalf("duplicate POST status = %d, want 409 (body %s)", code, body)
	}
	c, msg := apiErr(t, body)
	if c != "conflict" || msg != "a category with this name already exists" {
		t.Errorf("envelope = %s/%s, want conflict/a category with this name already exists", c, msg)
	}

	// The list reflects both rows; equal sort_order keeps insertion order.
	code, body = doReq(t, h, http.MethodGet, "/api/v1/categories", "", authHdr(token))
	if code != http.StatusOK {
		t.Fatalf("GET categories status = %d, want 200 (body %s)", code, body)
	}
	var list []map[string]any
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode list: %v (body %s)", err, body)
	}
	if len(list) != 2 || list[0]["id"] != id || list[1]["id"] != childID {
		t.Fatalf("list = %s, want the two created rows in insertion order", body)
	}

	// PATCH renames and toggles active off; the child is untouched.
	code, body = doReq(t, h, http.MethodPatch, "/api/v1/categories/"+id,
		`{"name":"Food","active":false}`, authHdr(token))
	if code != http.StatusOK {
		t.Fatalf("PATCH categories status = %d, want 200 (body %s)", code, body)
	}
	var patched map[string]any
	if err := json.Unmarshal([]byte(body), &patched); err != nil {
		t.Fatalf("decode patched: %v (body %s)", err, body)
	}
	if patched["name"] != "Food" || patched["active"] != false || patched["parent_id"] != nil {
		t.Errorf("patched = %s (parent must stay null)", body)
	}
	code, body = doReq(t, h, http.MethodGet, "/api/v1/categories", "", authHdr(token))
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode list: %v (body %s)", err, body)
	}
	if list[1]["name"] != "Organic" || list[1]["active"] != true {
		t.Errorf("child was touched by the parent's patch: %s", body)
	}

	// DELETE answers 204 with an empty body.
	code, body = doReq(t, h, http.MethodDelete, "/api/v1/categories/"+childID, "", authHdr(token))
	if code != http.StatusNoContent {
		t.Fatalf("DELETE categories status = %d, want 204", code)
	}
	if body != "" {
		t.Errorf("DELETE body = %q, want empty", body)
	}

	// Only the root survives.
	code, body = doReq(t, h, http.MethodGet, "/api/v1/categories", "", authHdr(token))
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode list: %v (body %s)", err, body)
	}
	if code != http.StatusOK || len(list) != 1 || list[0]["id"] != id {
		t.Fatalf("GET after delete = %d %s, want the one root row", code, body)
	}
}

func TestCategoryTriStatePatch(t *testing.T) {
	store := &memCategoryStore{}
	h, key := newCrudServer(t, nil, store)
	token := mintIdentityToken(t, key, "ctri-sub", "ctri@localhost", "CTri", "ctri")

	code, body := doReq(t, h, http.MethodPost, "/api/v1/categories",
		`{"name":"Parent"}`, authHdr(token))
	if code != http.StatusCreated {
		t.Fatalf("POST parent status = %d, want 201 (body %s)", code, body)
	}
	parentID := idFrom(t, body)
	code, body = doReq(t, h, http.MethodPost, "/api/v1/categories",
		`{"name":"Child"}`, authHdr(token))
	if code != http.StatusCreated {
		t.Fatalf("POST child status = %d, want 201 (body %s)", code, body)
	}
	childID := idFrom(t, body)

	patch := func(t *testing.T, payload string) map[string]any {
		code, body := doReq(t, h, http.MethodPatch, "/api/v1/categories/"+childID, payload, authHdr(token))
		if code != http.StatusOK {
			t.Fatalf("PATCH %s status = %d (body %s)", payload, code, body)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode patched: %v (body %s)", err, body)
		}
		return out
	}

	t.Run("absent leaves the parent untouched", func(t *testing.T) {
		out := patch(t, `{"active":false}`)
		if out["parent_id"] != nil {
			t.Errorf("parent_id = %v, want untouched (null root)", out["parent_id"])
		}
		if out["active"] != false {
			t.Errorf("active = %v, want false", out["active"])
		}
	})

	t.Run("string sets the parent", func(t *testing.T) {
		out := patch(t, fmt.Sprintf(`{"parent_id":"%s"}`, parentID))
		if out["parent_id"] != parentID {
			t.Errorf("parent_id = %v, want %s", out["parent_id"], parentID)
		}
	})

	t.Run("null demotes to root", func(t *testing.T) {
		out := patch(t, `{"parent_id":null}`)
		if out["parent_id"] != nil {
			t.Errorf("parent_id = %v, want null (root)", out["parent_id"])
		}
	})
}

func TestCategoryValidation(t *testing.T) {
	store := &memCategoryStore{}
	h, key := newCrudServer(t, nil, store)
	token := mintIdentityToken(t, key, "cval-sub", "cval@localhost", "CVal", "cval")

	longName := strings.Repeat("n", 256)
	patchID := nextTestID()

	cases := []struct {
		name   string
		method string
		path   string
		body   string
		want   string // substring of the message
	}{
		{"empty name", http.MethodPost, "/api/v1/categories",
			`{"name":"   "}`, "name must not be empty"},
		{"long name", http.MethodPost, "/api/v1/categories",
			`{"name":"` + longName + `"}`, "name must be at most 255 characters"},
		{"bad parent id", http.MethodPost, "/api/v1/categories",
			`{"name":"A","parent_id":"nope"}`, "parent_id must be a valid UUID"},
		{"parent wrong type", http.MethodPost, "/api/v1/categories",
			`{"name":"A","parent_id":42}`, "body must be a JSON object"},
		{"non-JSON body", http.MethodPost, "/api/v1/categories",
			`not json`, "body must be a JSON object"},
		{"array body", http.MethodPost, "/api/v1/categories",
			`[]`, "body must be a JSON object"},
		{"patch empty name", http.MethodPatch, "/api/v1/categories/" + patchID,
			`{"name":""}`, "name must not be empty"},
		{"patch long name", http.MethodPatch, "/api/v1/categories/" + patchID,
			`{"name":"` + longName + `"}`, "name must be at most 255 characters"},
		{"patch bad parent id", http.MethodPatch, "/api/v1/categories/" + patchID,
			`{"parent_id":"nope"}`, "parent_id must be a valid UUID"},
		{"patch parent wrong type", http.MethodPatch, "/api/v1/categories/" + patchID,
			`{"parent_id":42}`, "body must be a JSON object"},
		{"patch active wrong type", http.MethodPatch, "/api/v1/categories/" + patchID,
			`{"active":"yes"}`, "body must be a JSON object"},
		{"empty patch", http.MethodPatch, "/api/v1/categories/" + patchID,
			`{}`, "empty patch: provide at least one field"},
		{"unknown-only patch", http.MethodPatch, "/api/v1/categories/" + patchID,
			`{"bogus":1}`, "empty patch: provide at least one field"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := doReq(t, h, tc.method, tc.path, tc.body, authHdr(token))
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", code, body)
			}
			c, msg := apiErr(t, body)
			if c != "invalid_request" {
				t.Errorf("code = %s, want invalid_request", c)
			}
			if !strings.Contains(msg, tc.want) {
				t.Errorf("message %q does not contain %q", msg, tc.want)
			}
		})
	}

	// A POST with {} reports exactly the one problem categories have:
	// the missing name (there is no type/currency to complain about).
	code, body := doReq(t, h, http.MethodPost, "/api/v1/categories", `{}`, authHdr(token))
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", code, body)
	}
	_, msg := apiErr(t, body)
	if msg != "name must not be empty" {
		t.Errorf("message = %q, want exactly \"name must not be empty\"", msg)
	}
}

func TestCategoriesMalformedID(t *testing.T) {
	// If a malformed id ever reached the store, updateErr/deleteErr would
	// make the handler answer 500; a 404 proves the shape check runs first.
	store := &memCategoryStore{updateErr: errors.New("boom"), deleteErr: errors.New("boom")}
	h, key := newCrudServer(t, nil, store)
	token := mintIdentityToken(t, key, "cmal-sub", "cmal@localhost", "CMal", "cmal")

	for _, id := range []string{"not-a-uuid", "123", strings.Repeat("0", 35)} {
		for _, method := range []string{http.MethodPatch, http.MethodDelete} {
			body := ""
			if method == http.MethodPatch {
				body = `{"name":"X"}`
			}
			code, out := doReq(t, h, method, "/api/v1/categories/"+id, body, authHdr(token))
			if code != http.StatusNotFound {
				t.Fatalf("%s /%s status = %d, want 404 (body %s)", method, id, code, out)
			}
			c, msg := apiErr(t, out)
			if c != "not_found" || msg != "category not found" {
				t.Errorf("%s /%s envelope = %s/%s, want not_found/category not found", method, id, c, msg)
			}
		}
	}
}

func TestCategoryErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		method string
		err    error
		status int
		code   string
		msg    string
	}{
		{"create name taken", http.MethodPost, repo.ErrNameTaken,
			http.StatusConflict, "conflict", "a category with this name already exists"},
		{"create parent not found", http.MethodPost, repo.ErrParentNotFound,
			http.StatusBadRequest, "invalid_request", "parent category not found"},
		{"create other", http.MethodPost, errors.New("boom"),
			http.StatusInternalServerError, "internal", "category operation failed"},
		{"update not found", http.MethodPatch, repo.ErrCategoryNotFound,
			http.StatusNotFound, "not_found", "category not found"},
		{"update parent not found", http.MethodPatch, repo.ErrParentNotFound,
			http.StatusBadRequest, "invalid_request", "parent category not found"},
		{"update other", http.MethodPatch, errors.New("boom"),
			http.StatusInternalServerError, "internal", "category operation failed"},
		{"delete not found", http.MethodDelete, repo.ErrCategoryNotFound,
			http.StatusNotFound, "not_found", "category not found"},
		{"delete other", http.MethodDelete, errors.New("boom"),
			http.StatusInternalServerError, "internal", "category operation failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &memCategoryStore{}
			switch tc.method {
			case http.MethodPost:
				store.createErr = tc.err
			case http.MethodPatch:
				store.updateErr = tc.err
			case http.MethodDelete:
				store.deleteErr = tc.err
			}
			h, key := newCrudServer(t, nil, store)
			token := mintIdentityToken(t, key, "cmap-sub", "cmap@localhost", "CMap", "cmap")
			var (
				path string
				body string
			)
			switch tc.method {
			case http.MethodPost:
				path = "/api/v1/categories"
				body = `{"name":"X"}`
			case http.MethodPatch:
				path = "/api/v1/categories/" + nextTestID()
				body = `{"name":"X"}`
			case http.MethodDelete:
				path = "/api/v1/categories/" + nextTestID()
			}
			code, out := doReq(t, h, tc.method, path, body, authHdr(token))
			if code != tc.status {
				t.Fatalf("status = %d, want %d (body %s)", code, tc.status, out)
			}
			c, msg := apiErr(t, out)
			if c != tc.code || msg != tc.msg {
				t.Errorf("envelope = %s/%s, want %s/%s", c, msg, tc.code, tc.msg)
			}
		})
	}
}

func TestCategoriesAuthAndConfig(t *testing.T) {
	t.Run("no token", func(t *testing.T) {
		// prefixAuth answers 401 before any route or handler runs; the
		// body is the verifier's own, not the envelope, so status only.
		h, _ := newCrudServer(t, nil, &memCategoryStore{})
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete} {
			path := "/api/v1/categories"
			if method == http.MethodPatch || method == http.MethodDelete {
				path = "/api/v1/categories/" + nextTestID()
			}
			code, _ := doReq(t, h, method, path, "", nil)
			if code != http.StatusUnauthorized {
				t.Errorf("%s without token: status = %d, want 401", method, code)
			}
		}
	})
	t.Run("nil store", func(t *testing.T) {
		h, key := newCrudServer(t, nil, nil)
		token := mintIdentityToken(t, key, "cnc-sub", "cnc@localhost", "C", "cnc")
		code, body := doReq(t, h, http.MethodGet, "/api/v1/categories", "", authHdr(token))
		if code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 (body %s)", code, body)
		}
		c, msg := apiErr(t, body)
		if c != "internal" || msg != "database not configured" {
			t.Errorf("envelope = %s/%s, want internal/database not configured", c, msg)
		}
	})
	t.Run("nil bootstrap", func(t *testing.T) {
		key := mintKey(t)
		h := New(fakeReady{}, BuildInfo{}, liveAuth(t, key), nil, nil, nil, &memCategoryStore{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, false)
		token := mintIdentityToken(t, key, "cnb-sub", "cnb@localhost", "C", "cnb")
		code, body := doReq(t, h, http.MethodGet, "/api/v1/categories", "", authHdr(token))
		if code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 (body %s)", code, body)
		}
		c, msg := apiErr(t, body)
		if c != "internal" || msg != "database not configured" {
			t.Errorf("envelope = %s/%s, want internal/database not configured", c, msg)
		}
	})
}

func TestCategoriesPerUserScoping(t *testing.T) {
	store := &memCategoryStore{}
	h, key := newCrudServer(t, nil, store)
	tokenA := mintIdentityToken(t, key, "cscope-a", "ca@localhost", "CA", "ca")
	tokenB := mintIdentityToken(t, key, "cscope-b", "cb@localhost", "CB", "cb")

	// A creates a root and a child under it.
	code, body := doReq(t, h, http.MethodPost, "/api/v1/categories",
		`{"name":"Groceries"}`, authHdr(tokenA))
	if code != http.StatusCreated {
		t.Fatalf("POST root (a) status = %d, want 201 (body %s)", code, body)
	}
	rootID := idFrom(t, body)
	code, body = doReq(t, h, http.MethodPost, "/api/v1/categories",
		fmt.Sprintf(`{"name":"Organic","parent_id":"%s"}`, rootID), authHdr(tokenA))
	if code != http.StatusCreated {
		t.Fatalf("POST child (a) status = %d, want 201 (body %s)", code, body)
	}
	childID := idFrom(t, body)

	// B's list is empty: A's rows are invisible.
	code, body = doReq(t, h, http.MethodGet, "/api/v1/categories", "", authHdr(tokenB))
	if code != http.StatusOK || strings.TrimSpace(body) != "[]" {
		t.Fatalf("GET (b) = %d %s, want 200 []", code, body)
	}

	// B cannot patch or delete A's rows; existence is not disclosed.
	code, body = doReq(t, h, http.MethodPatch, "/api/v1/categories/"+rootID,
		`{"name":"Hacked"}`, authHdr(tokenB))
	if code != http.StatusNotFound {
		t.Fatalf("PATCH (b, a's id) status = %d, want 404 (body %s)", code, body)
	}
	if c, _ := apiErr(t, body); c != "not_found" {
		t.Errorf("PATCH (b) code = %s, want not_found", c)
	}
	code, _ = doReq(t, h, http.MethodDelete, "/api/v1/categories/"+childID, "", authHdr(tokenB))
	if code != http.StatusNotFound {
		t.Fatalf("DELETE (b, a's id) status = %d, want 404", code)
	}

	// A's rows are untouched.
	code, body = doReq(t, h, http.MethodGet, "/api/v1/categories", "", authHdr(tokenA))
	if code != http.StatusOK || !strings.Contains(body, `"Groceries"`) || !strings.Contains(body, `"Organic"`) {
		t.Fatalf("GET (a) = %d %s, want both untouched rows", code, body)
	}

	// A cross-user parent is rejected: B cannot hang a category off
	// A's root (the repo scopes the parent check to the caller).
	code, body = doReq(t, h, http.MethodPost, "/api/v1/categories",
		fmt.Sprintf(`{"name":"Organic","parent_id":"%s"}`, rootID), authHdr(tokenB))
	if code != http.StatusBadRequest {
		t.Fatalf("POST (b, a's parent) status = %d, want 400 (body %s)", code, body)
	}
	c, msg := apiErr(t, body)
	if c != "invalid_request" || msg != "parent category not found" {
		t.Errorf("envelope = %s/%s, want invalid_request/parent category not found", c, msg)
	}

	// The same name is reusable by B (uniqueness is per user).
	code, body = doReq(t, h, http.MethodPost, "/api/v1/categories",
		`{"name":"Groceries"}`, authHdr(tokenB))
	if code != http.StatusCreated {
		t.Fatalf("POST (b, same name) status = %d, want 201 (body %s)", code, body)
	}
}

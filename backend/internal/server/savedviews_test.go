// Route tests for the saved Explore view endpoints (round 16,
// migration 0015). All in-memory: memSavedViewStore mirrors the
// repo.SavedViewStore semantics the handlers depend on (user-id
// scoping, name trim + the 255-rune rule, the no-op update rule) and
// injects per-operation errors for the mapping table; the crypto,
// ordering and isolation rules stay with the repo testcontainer test.
// The shared helpers (doReq, authHdr, apiErr, idFrom, nextTestID) come
// from settings_test.go / accounts_test.go.
package server

import (
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

// memSavedViewRow is one fake saved-view row (name stored trimmed).
type memSavedViewRow struct {
	userID string
	v      repo.SavedView
}

// memSavedViewStore is an in-memory SavedViewStore.
type memSavedViewStore struct {
	rows []memSavedViewRow
	// Per-operation error injection for the 500 mapping table.
	failList, failCreate, failUpdate, failDelete error
}

func newMemSavedViewStore() *memSavedViewStore {
	return &memSavedViewStore{}
}

func (s *memSavedViewStore) List(_ context.Context, userID string, _ []byte) ([]repo.SavedView, error) {
	if s.failList != nil {
		return nil, s.failList
	}
	out := make([]repo.SavedView, 0, 4)
	for _, r := range s.rows {
		if r.userID == userID {
			out = append(out, r.v)
		}
	}
	return out, nil
}

func (s *memSavedViewStore) Create(_ context.Context, userID string, in repo.SavedViewIn, _ []byte) (repo.SavedView, error) {
	if s.failCreate != nil {
		return repo.SavedView{}, s.failCreate
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || len([]rune(name)) > 255 {
		return repo.SavedView{}, errors.New("repo: invalid saved view name")
	}
	now := time.Now()
	v := repo.SavedView{
		ID:        nextTestID(),
		Name:      name,
		State:     in.State,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.rows = append(s.rows, memSavedViewRow{userID: userID, v: v})
	return v, nil
}

func (s *memSavedViewStore) Update(_ context.Context, userID, id string, p repo.SavedViewPatch, _ []byte) (repo.SavedView, error) {
	if s.failUpdate != nil {
		return repo.SavedView{}, s.failUpdate
	}
	i := -1
	for j, r := range s.rows {
		if r.userID == userID && r.v.ID == id {
			i = j
			break
		}
	}
	if i < 0 {
		return repo.SavedView{}, repo.ErrSavedViewNotFound
	}
	cur := s.rows[i].v
	changed := false
	if p.Name != nil {
		name := strings.TrimSpace(*p.Name)
		if name == "" || len([]rune(name)) > 255 {
			return repo.SavedView{}, errors.New("repo: invalid saved view name")
		}
		if name != cur.Name {
			cur.Name = name
			changed = true
		}
	}
	if p.State != nil && *p.State != cur.State {
		cur.State = *p.State
		changed = true
	}
	if !changed {
		// No-op patch: the row comes back without touching updated_at.
		return cur, nil
	}
	cur.UpdatedAt = time.Now()
	s.rows[i].v = cur
	return cur, nil
}

func (s *memSavedViewStore) Delete(_ context.Context, userID, id string) error {
	if s.failDelete != nil {
		return s.failDelete
	}
	for i, r := range s.rows {
		if r.userID == userID && r.v.ID == id {
			s.rows = append(s.rows[:i], s.rows[i+1:]...)
			return nil
		}
	}
	return repo.ErrSavedViewNotFound
}

// newSVServer builds the router wired to the given saved-view store
// (nil allowed — that is the 503 path). The key is returned so tests
// can mint tokens for several users under the same verifier.
func newSVServer(t *testing.T, sv SavedViewStore) (http.Handler, jwk.Key) {
	t.Helper()
	key := mintKey(t)
	b, err := NewBootstrap(newMemStore(), testKEK(t), nil)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return New(
		fakeReady{}, BuildInfo{}, liveAuth(t, key), b,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		nil, nil, sv, nil, nil, false, nil, nil, nil, nil, nil, nil, nil, PublicConfig{},
	), key
}

// svJSON decodes a response body into a map, failing the test if it is
// not a JSON object.
func svJSON(t *testing.T, out string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	return m
}

// TestSavedViewsCRUD walks the saved-view routes: fresh empty list,
// create (name trimmed, absent state stored as ""), list in creation
// order with the states round-tripping, rename, a no-op rename that
// must not move updated_at, a state update that keeps the name, an
// empty patch, a malformed id, delete (204, empty body) and the 404 on
// a second delete.
func TestSavedViewsCRUD(t *testing.T) {
	sv := newMemSavedViewStore()
	h, key := newSVServer(t, sv)
	tok := mintIdentityToken(t, key, "u-a", "u-a@example.com", "User A", "ua")
	hdr := authHdr(tok)

	// Fresh list is exactly [].
	code, out := doReq(t, h, http.MethodGet, "/api/v1/saved-views", "", hdr)
	if code != http.StatusOK || strings.TrimSpace(out) != "[]" {
		t.Fatalf("fresh list = %d %q, want 200 []", code, out)
	}

	// Create: the name is trimmed on write, an absent state is the
	// default view (""), and the DTO carries no timestamps.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/saved-views", `{"name":"  Groceries now  "}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201 (body %s)", code, out)
	}
	row := svJSON(t, out)
	if row["name"] != "Groceries now" || row["state"] != "" {
		t.Fatalf("create row = %v, want trimmed name and empty state", row)
	}
	if _, ok := row["created_at"]; ok {
		t.Errorf("saved view DTO leaks created_at: %v", row)
	}
	id := idFrom(t, out)

	// Create with a state: the canonical explore_state string
	// round-trips verbatim.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/saved-views",
		`{"name":"Savings bar","state":"chart=bar&group=month"}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("create 2: status = %d (body %s)", code, out)
	}
	if r2 := svJSON(t, out); r2["name"] != "Savings bar" || r2["state"] != "chart=bar&group=month" {
		t.Fatalf("create 2 row = %v", r2)
	}
	id2 := idFrom(t, out)

	// List is in creation order and carries the states.
	code, out = doReq(t, h, http.MethodGet, "/api/v1/saved-views", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("list: status = %d (body %s)", code, out)
	}
	type svRow struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		State string `json:"state"`
	}
	var rows []svRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("list body is not JSON: %v (%s)", err, out)
	}
	if len(rows) != 2 || rows[0].ID != id || rows[1].ID != id2 {
		t.Fatalf("list = %+v, want creation order [%s, %s]", rows, id, id2)
	}
	if rows[0].Name != "Groceries now" || rows[0].State != "" ||
		rows[1].Name != "Savings bar" || rows[1].State != "chart=bar&group=month" {
		t.Fatalf("list rows = %+v, want names + states to round-trip", rows)
	}

	// Rename: 200, the state is untouched.
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/saved-views/"+id, `{"name":"Groceries (renamed)"}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("patch: status = %d (body %s)", code, out)
	}
	row = svJSON(t, out)
	if row["name"] != "Groceries (renamed)" || row["state"] != "" {
		t.Fatalf("patch row = %v, want the rename with the state kept", row)
	}

	// A no-op rename (trimmed-equal) must not move updated_at.
	before := sv.rows[0].v.UpdatedAt
	time.Sleep(5 * time.Millisecond)
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/saved-views/"+id, `{"name":"  Groceries (renamed)  "}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("no-op patch: status = %d (body %s)", code, out)
	}
	if after := sv.rows[0].v.UpdatedAt; !after.Equal(before) {
		t.Errorf("no-op rename moved updated_at: %s -> %s", before, after)
	}

	// State update: the name is untouched.
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/saved-views/"+id2, `{"state":"q=coffee&from=2026-01-01"}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("patch state: status = %d (body %s)", code, out)
	}
	if r2 := svJSON(t, out); r2["name"] != "Savings bar" || r2["state"] != "q=coffee&from=2026-01-01" {
		t.Fatalf("patch state row = %v", r2)
	}

	// A patch with neither field set is 400.
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/saved-views/"+id, `{}`, hdr)
	if code != http.StatusBadRequest {
		t.Fatalf("empty patch: status = %d, want 400 (body %s)", code, out)
	}
	if c, m := apiErr(t, out); c != "invalid_request" || m != "empty patch: provide at least one field" {
		t.Fatalf("empty patch envelope = %s %q", c, m)
	}

	// A malformed id never reaches the store: 404, not 500.
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/saved-views/not-a-uuid", `{"name":"x"}`, hdr)
	if code != http.StatusNotFound {
		t.Fatalf("patch malformed id: status = %d, want 404 (body %s)", code, out)
	}
	if c, m := apiErr(t, out); c != "not_found" || m != "saved view not found" {
		t.Fatalf("malformed-id envelope = %s %q, want not_found/saved view not found", c, m)
	}
	code, out = doReq(t, h, http.MethodDelete, "/api/v1/saved-views/not-a-uuid", "", hdr)
	if code != http.StatusNotFound {
		t.Fatalf("delete malformed id: status = %d, want 404 (body %s)", code, out)
	}

	// Delete: 204 with an empty body; re-delete: 404.
	code, out = doReq(t, h, http.MethodDelete, "/api/v1/saved-views/"+id, "", hdr)
	if code != http.StatusNoContent || out != "" {
		t.Fatalf("delete = %d %q, want 204 empty", code, out)
	}
	code, out = doReq(t, h, http.MethodDelete, "/api/v1/saved-views/"+id, "", hdr)
	if code != http.StatusNotFound {
		t.Fatalf("re-delete: status = %d, want 404 (body %s)", code, out)
	}
	if c, m := apiErr(t, out); c != "not_found" || m != "saved view not found" {
		t.Fatalf("re-delete envelope = %s %q, want not_found/saved view not found", c, m)
	}

	// The other view survived the delete of the first.
	code, out = doReq(t, h, http.MethodGet, "/api/v1/saved-views", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("final list: %d (%s)", code, out)
	}
	var rest []svRow
	if err := json.Unmarshal([]byte(out), &rest); err != nil {
		t.Fatalf("final list body is not JSON: %v (%s)", err, out)
	}
	if len(rest) != 1 || rest[0].ID != id2 {
		t.Fatalf("final list = %+v, want only view 2", rest)
	}
}

// TestSavedViewsValidation: the 400s — a blank or absent name, a name
// over 255 runes (255 itself passes), a state over 1024 runes (1024
// itself passes), and a malformed JSON body.
func TestSavedViewsValidation(t *testing.T) {
	sv := newMemSavedViewStore()
	h, key := newSVServer(t, sv)
	tok := mintIdentityToken(t, key, "u-a", "u-a@example.com", "User A", "ua")
	hdr := authHdr(tok)

	// 256-rune name is 400.
	code, out := doReq(t, h, http.MethodPost, "/api/v1/saved-views",
		`{"name":"`+strings.Repeat("n", 256)+`"}`, hdr)
	if code != http.StatusBadRequest {
		t.Fatalf("long name: status = %d, want 400 (body %s)", code, out)
	}
	if c, m := apiErr(t, out); c != "invalid_request" || m != "name must be at most 255 characters" {
		t.Fatalf("long name envelope = %s %q", c, m)
	}

	// The boundaries pass: a 255-rune name and a 1024-rune state.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/saved-views",
		`{"name":"`+strings.Repeat("n", 255)+`","state":"`+strings.Repeat("s", 1024)+`"}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("boundary create: status = %d, want 201 (body %s)", code, out)
	}
	bid := idFrom(t, out)
	if r := svJSON(t, out); len([]rune(r["name"].(string))) != 255 || len([]rune(r["state"].(string))) != 1024 {
		t.Fatalf("boundary row = %v, want 255/1024 runes", r)
	}

	// A 1025-rune state is 400.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/saved-views",
		`{"name":"Over","state":"`+strings.Repeat("s", 1025)+`"}`, hdr)
	if code != http.StatusBadRequest {
		t.Fatalf("long state: status = %d, want 400 (body %s)", code, out)
	}
	if c, m := apiErr(t, out); c != "invalid_request" || m != "state must be at most 1024 characters" {
		t.Fatalf("long state envelope = %s %q", c, m)
	}

	// Blank and absent names are 400.
	for _, body := range []string{`{"name":"   "}`, `{"state":"chart=bar"}`} {
		code, out = doReq(t, h, http.MethodPost, "/api/v1/saved-views", body, hdr)
		if code != http.StatusBadRequest {
			t.Fatalf("blank/absent name (%s): status = %d, want 400 (body %s)", body, code, out)
		}
		if c, m := apiErr(t, out); c != "invalid_request" || m != "name must not be empty" {
			t.Fatalf("blank/absent name envelope = %s %q", c, m)
		}
	}

	// A malformed body is 400.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/saved-views", `{oops`, hdr)
	if code != http.StatusBadRequest {
		t.Fatalf("malformed body: status = %d, want 400 (body %s)", code, out)
	}

	// PATCH: the same name rule on a well-formed id.
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/saved-views/"+bid, `{"name":"  "}`, hdr)
	if code != http.StatusBadRequest {
		t.Fatalf("patch blank name: status = %d, want 400 (body %s)", code, out)
	}

	// Nothing but the boundary row reached the store.
	if len(sv.rows) != 1 || sv.rows[0].v.ID != bid {
		t.Fatalf("store rows = %+v, want only the boundary row", sv.rows)
	}
}

// TestSavedViewsIsolation: every route is scoped to the caller — user
// b neither lists, renames nor deletes user a's views, and the 404s do
// not disclose existence.
func TestSavedViewsIsolation(t *testing.T) {
	sv := newMemSavedViewStore()
	h, key := newSVServer(t, sv)
	mine := authHdr(mintIdentityToken(t, key, "u-a", "u-a@example.com", "User A", "ua"))
	theirs := authHdr(mintIdentityToken(t, key, "u-b", "u-b@example.com", "User B", "ub"))

	code, out := doReq(t, h, http.MethodPost, "/api/v1/saved-views", `{"name":"A only"}`, mine)
	if code != http.StatusCreated {
		t.Fatalf("create: %d (%s)", code, out)
	}
	id := idFrom(t, out)

	// b's list is empty and b cannot patch or delete a's row.
	code, out = doReq(t, h, http.MethodGet, "/api/v1/saved-views", "", theirs)
	if code != http.StatusOK || strings.TrimSpace(out) != "[]" {
		t.Fatalf("b's list = %d %q, want 200 []", code, out)
	}
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/saved-views/"+id, `{"name":"hijack"}`, theirs)
	if code != http.StatusNotFound {
		t.Fatalf("b's patch: status = %d, want 404 (body %s)", code, out)
	}
	code, out = doReq(t, h, http.MethodDelete, "/api/v1/saved-views/"+id, "", theirs)
	if code != http.StatusNotFound {
		t.Fatalf("b's delete: status = %d, want 404 (body %s)", code, out)
	}

	// a's row is intact through all of it.
	code, out = doReq(t, h, http.MethodGet, "/api/v1/saved-views", "", mine)
	if code != http.StatusOK || !strings.Contains(out, "A only") {
		t.Fatalf("a's row must be intact: %d %q", code, out)
	}
}

// TestSavedViewsNilStore: with no database the four routes answer 503.
func TestSavedViewsNilStore(t *testing.T) {
	h, key := newSVServer(t, nil)
	tok := mintIdentityToken(t, key, "u-a", "u-a@example.com", "User A", "ua")
	hdr := authHdr(tok)
	for _, req := range [][2]string{
		{http.MethodGet, "/api/v1/saved-views"},
		{http.MethodPost, "/api/v1/saved-views"},
		{http.MethodPatch, "/api/v1/saved-views/" + nextTestID()},
		{http.MethodDelete, "/api/v1/saved-views/" + nextTestID()},
	} {
		code, _ := doReq(t, h, req[0], req[1], "", hdr)
		if code != http.StatusServiceUnavailable {
			t.Errorf("%s %s (nil store): status = %d, want 503", req[0], req[1], code)
		}
	}
}

// TestSavedViewsStoreErrors: injected store failures map to 500 (the
// details stay in the server log, not the body) and clear again.
func TestSavedViewsStoreErrors(t *testing.T) {
	sv := newMemSavedViewStore()
	h, key := newSVServer(t, sv)
	tok := mintIdentityToken(t, key, "u-a", "u-a@example.com", "User A", "ua")
	hdr := authHdr(tok)

	sv.failCreate = errors.New("boom")
	code, out := doReq(t, h, http.MethodPost, "/api/v1/saved-views", `{"name":"X"}`, hdr)
	if code != http.StatusInternalServerError {
		t.Fatalf("create (injected): status = %d, want 500 (body %s)", code, out)
	}
	if c, m := apiErr(t, out); c != "internal" || m != "saved view operation failed" {
		t.Fatalf("create envelope = %s %q", c, m)
	}
	sv.failCreate = nil

	code, out = doReq(t, h, http.MethodPost, "/api/v1/saved-views", `{"name":"X"}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("create after reset: %d (%s)", code, out)
	}
	id := idFrom(t, out)

	sv.failUpdate = errors.New("boom")
	code, out = doReq(t, h, http.MethodPatch, "/api/v1/saved-views/"+id, `{"name":"Y"}`, hdr)
	if code != http.StatusInternalServerError {
		t.Fatalf("update (injected): status = %d, want 500 (body %s)", code, out)
	}
	sv.failUpdate = nil

	sv.failDelete = errors.New("boom")
	code, out = doReq(t, h, http.MethodDelete, "/api/v1/saved-views/"+id, "", hdr)
	if code != http.StatusInternalServerError {
		t.Fatalf("delete (injected): status = %d, want 500 (body %s)", code, out)
	}
	sv.failDelete = nil

	sv.failList = errors.New("boom")
	code, out = doReq(t, h, http.MethodGet, "/api/v1/saved-views", "", hdr)
	if code != http.StatusInternalServerError {
		t.Fatalf("list (injected): status = %d, want 500 (body %s)", code, out)
	}
	sv.failList = nil

	// All four work again once the injections are cleared.
	code, out = doReq(t, h, http.MethodGet, "/api/v1/saved-views", "", hdr)
	if code != http.StatusOK || !strings.Contains(out, "X") {
		t.Fatalf("list after reset: %d %q", code, out)
	}
}

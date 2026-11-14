// Route tests for the notification endpoints (plan step 3.3):
// GET /api/v1/notifications (unread/limit/offset query params),
// POST /api/v1/notifications/{id}/read (idempotent, 200 with the
// updated row) and GET /api/v1/notifications/unread-count. All
// in-memory: memNotificationStore mirrors the repo.NotificationStore
// semantics the handlers depend on (limit 50/100 clamp, offset >= 0,
// created_at DESC then id DESC order, unread = read_at null,
// first-read-wins read_at, user-id scoping) and injects per-operation
// errors for the mapping table. The app-side notification writers do
// not exist until step 8.x, so rows are seeded directly. The shared
// helpers (authHdr, apiErr, idFrom, nextTestID, doReq) live in
// accounts_test.go and settings_test.go.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"testing"
	"time"

	"finance-tracker/internal/repo"

	"github.com/lestrrat-go/jwx/v3/jwk"
)

// memNotificationRow is one fake-store row.
type memNotificationRow struct {
	userID string
	n      repo.Notification
}

// memNotificationStore mirrors the repo.NotificationStore semantics the
// handlers depend on: a limit <= 0 becomes 50 and a limit > 100 becomes
// 100 (the repo's unexported consts), a negative offset becomes 0,
// unread = read_at is null (?unread=false returns everything), rows
// come back created_at DESC then id DESC, MarkRead stamps read_at once
// (first-read-wins), and every operation is scoped by user id.
type memNotificationStore struct {
	rows []memNotificationRow

	listErr   error
	readErr   error
	countErr  error
	createErr error
	findErr   error
}

// addNotif seeds one row directly and returns the minted id; read rows
// get a read_at a second after created_at.
func addNotif(m *memNotificationStore, userID, kind, title, body string, ref json.RawMessage, createdAt time.Time, read bool) string {
	id := nextTestID()
	n := repo.Notification{ID: id, Kind: kind, Title: title, Body: body, Ref: ref, CreatedAt: createdAt}
	if read {
		ra := createdAt.Add(time.Second)
		n.ReadAt = &ra
	}
	m.rows = append(m.rows, memNotificationRow{userID: userID, n: n})
	return id
}

func (m *memNotificationStore) List(_ context.Context, userID string, f repo.NotificationFilter) ([]repo.Notification, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 50 // repo defaultNotificationLimit (unexported)
	}
	if limit > 100 {
		limit = 100 // repo maxNotificationLimit (unexported)
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	var out []repo.Notification
	for _, r := range m.rows {
		if r.userID != userID {
			continue
		}
		if f.Unread && r.n.ReadAt != nil {
			continue
		}
		out = append(out, r.n)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	if f.Offset >= len(out) {
		return []repo.Notification{}, nil
	}
	if f.Offset+limit < len(out) {
		out = out[f.Offset : f.Offset+limit]
	} else {
		out = out[f.Offset:]
	}
	return out, nil
}

func (m *memNotificationStore) MarkRead(_ context.Context, userID, id string) (repo.Notification, error) {
	if m.readErr != nil {
		return repo.Notification{}, m.readErr
	}
	for i, r := range m.rows {
		if r.userID != userID || r.n.ID != id {
			continue
		}
		if r.n.ReadAt == nil {
			now := time.Now().UTC()
			r.n.ReadAt = &now
			m.rows[i] = r
		}
		return r.n, nil
	}
	return repo.Notification{}, repo.ErrNotificationNotFound
}

func (m *memNotificationStore) CountUnread(_ context.Context, userID string) (int, error) {
	if m.countErr != nil {
		return 0, m.countErr
	}
	n := 0
	for _, r := range m.rows {
		if r.userID == userID && r.n.ReadAt == nil {
			n++
		}
	}
	return n, nil
}

// Create appends the row (minting an id) and returns it, mirroring the
// repo's Create returning the stored row.
func (m *memNotificationStore) Create(_ context.Context, userID string, in repo.NotificationIn) (repo.Notification, error) {
	if m.createErr != nil {
		return repo.Notification{}, m.createErr
	}
	n := repo.Notification{
		ID:        nextTestID(),
		Kind:      in.Kind,
		Title:     in.Title,
		Body:      in.Body,
		Ref:       in.Ref,
		CreatedAt: time.Now().UTC(),
	}
	m.rows = append(m.rows, memNotificationRow{userID: userID, n: n})
	return n, nil
}

// FindByKindRef returns the user's newest (kind, jsonb-equal ref) row,
// any read state; ErrNotificationNotFound when there is none.
func (m *memNotificationStore) FindByKindRef(_ context.Context, userID, kind string, ref []byte) (repo.Notification, error) {
	if m.findErr != nil {
		return repo.Notification{}, m.findErr
	}
	best := -1
	for i := range m.rows {
		r := &m.rows[i]
		if r.userID != userID || r.n.Kind != kind || !jsonbEqual(r.n.Ref, ref) {
			continue
		}
		if best < 0 || r.n.CreatedAt.After(m.rows[best].n.CreatedAt) ||
			(r.n.CreatedAt.Equal(m.rows[best].n.CreatedAt) && r.n.ID > m.rows[best].n.ID) {
			best = i
		}
	}
	if best < 0 {
		return repo.Notification{}, repo.ErrNotificationNotFound
	}
	return m.rows[best].n, nil
}

// jsonbEqual mirrors jsonb equality: the documents are equal when they
// unmarshal to the same value (key order irrelevant); a nil/empty
// document matches only a nil/empty document.
func jsonbEqual(a, b []byte) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	var va, vb any
	if err := json.Unmarshal(a, &va); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &vb); err != nil {
		return false
	}
	ca, _ := json.Marshal(va) // map keys marshal in sorted order
	cb, _ := json.Marshal(vb)
	return string(ca) == string(cb)
}

// newNotificationsServer builds a full router with a live token
// verifier, an in-memory bootstrap, and the given notification store
// (nil allowed — that is the 503 path). The key is returned so tests
// can mint tokens for several users under the same verifier.
func newNotificationsServer(t *testing.T, store NotificationStore) (http.Handler, jwk.Key) {
	t.Helper()
	key := mintKey(t)
	b, err := NewBootstrap(newMemStore(), testKEK(t), nil)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return New(fakeReady{}, BuildInfo{}, liveAuth(t, key), b, nil, nil, nil, nil, nil, store, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, false, nil, nil, nil), key
}

// listIDs decodes a list body and returns its ids in order (sameIDs, the
// element-wise comparison, is shared from transactions_test.go).
func listIDs(t *testing.T, out string) []string {
	t.Helper()
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	got := make([]string, 0, len(rows))
	for _, r := range rows {
		id, _ := r["id"].(string)
		got = append(got, id)
	}
	return got
}

// TestNotificationsList covers the order (created_at DESC, id DESC tie
// break), the wire shape (no created_at; ref and read_at present as
// null while unread), and every query filter.
func TestNotificationsList(t *testing.T) {
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	budgetID := nextTestID()
	store := &memNotificationStore{}
	// Minted in this order; n3 and n4 share a created_at (the tie the
	// id DESC break must resolve, later-minted first).
	n1 := addNotif(store, "id-not-a", "budget.alert", "t1", "b1", nil, base, false)
	n2 := addNotif(store, "id-not-a", "budget.alert", "t2", "b2",
		json.RawMessage(`{"budget_id":"`+budgetID+`"}`), base.Add(time.Minute), false)
	n5 := addNotif(store, "id-not-a", "budget.alert", "t5", "b5", nil, base.Add(2*time.Minute), true)
	n3 := addNotif(store, "id-not-a", "budget.alert", "t3", "b3", nil, base.Add(3*time.Minute), false)
	n4 := addNotif(store, "id-not-a", "budget.alert", "t4", "b4", nil, base.Add(3*time.Minute), false)
	h, key := newNotificationsServer(t, store)
	hdr := authHdr(mintIdentityToken(t, key, "not-a", "a@localhost", "A", "a"))

	code, out := doReq(t, h, http.MethodGet, "/api/v1/notifications", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", code, out)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	got := make([]string, 0, len(rows))
	for _, r := range rows {
		id, _ := r["id"].(string)
		got = append(got, id)
	}
	if !sameIDs(got, n4, n3, n5, n2, n1) {
		t.Fatalf("order = %v, want [%s, %s, %s, %s, %s] (created_at DESC, id DESC tie break)", got, n4, n3, n5, n2, n1)
	}
	byID := map[string]map[string]any{}
	for _, r := range rows {
		if id, ok := r["id"].(string); ok {
			byID[id] = r
		}
	}

	// Wire shape: no bookkeeping timestamps anywhere.
	for id, r := range byID {
		if _, ok := r["created_at"]; ok {
			t.Errorf("row %s carries created_at (bookkeeping stays server-side)", id)
		}
	}
	// Unread row with no ref: both keys present with null values.
	row1 := byID[n1]
	if _, ok := row1["ref"]; !ok {
		t.Error("unread row is missing the ref key (it should be present, null)")
	}
	if row1["ref"] != nil {
		t.Errorf("row %s ref = %v, want null", n1, row1["ref"])
	}
	if _, ok := row1["read_at"]; !ok {
		t.Error("unread row is missing the read_at key (it should be present, null)")
	}
	if row1["read_at"] != nil {
		t.Errorf("row %s read_at = %v, want null", n1, row1["read_at"])
	}
	// Row with a ref: it decodes as a JSON object, passed through.
	ref, ok := byID[n2]["ref"].(map[string]any)
	if !ok {
		t.Fatalf("row %s ref is not an object: %v", n2, byID[n2]["ref"])
	}
	if ref["budget_id"] != budgetID {
		t.Errorf("ref.budget_id = %v, want %s", ref["budget_id"], budgetID)
	}
	// Read row: read_at is an RFC3339 string.
	s, ok := byID[n5]["read_at"].(string)
	if !ok {
		t.Fatalf("read row read_at = %v, want a string", byID[n5]["read_at"])
	}
	if _, err := time.Parse(time.RFC3339, s); err != nil {
		t.Errorf("read_at %q is not RFC3339: %v", s, err)
	}

	cases := []struct {
		name  string
		query string
		want  []string
	}{
		{"unread only", "?unread=true", []string{n4, n3, n2, n1}},
		{"unread=false returns everything", "?unread=false", []string{n4, n3, n5, n2, n1}},
		{"limit and offset", "?limit=2&offset=1", []string{n3, n5}},
		{"offset past the end", "?limit=2&offset=100", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out := doReq(t, h, http.MethodGet, "/api/v1/notifications"+tc.query, "", hdr)
			if code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", code, out)
			}
			got := listIDs(t, out)
			if !sameIDs(got, tc.want...) {
				t.Fatalf("order = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestNotificationsClamps pins the repo limit behavior the handlers
// rely on: 50 default, 100 cap, and the newest row first at every page
// size.
func TestNotificationsClamps(t *testing.T) {
	base := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	store := &memNotificationStore{}
	var newest string
	for i := 0; i < 120; i++ {
		newest = addNotif(store, "id-cl-a", "kind", "t", "b", nil,
			base.Add(time.Duration(i)*time.Second), false)
	}
	h, key := newNotificationsServer(t, store)
	hdr := authHdr(mintIdentityToken(t, key, "cl-a", "a@localhost", "A", "a"))

	cases := []struct {
		name  string
		query string
		want  int
	}{
		{"default limit", "", 50},
		{"explicit limit", "?limit=20", 20},
		{"zero limit falls back to the default", "?limit=0", 50},
		{"limit above the cap clamps", "?limit=500", 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out := doReq(t, h, http.MethodGet, "/api/v1/notifications"+tc.query, "", hdr)
			if code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %.200s…)", code, out)
			}
			got := listIDs(t, out)
			if len(got) != tc.want {
				t.Fatalf("page = %d rows, want %d", len(got), tc.want)
			}
			if got[0] != newest {
				t.Errorf("first row = %s, want the newest %s", got[0], newest)
			}
		})
	}
}

// TestNotificationsBadParams pins every list 400 (the exact messages)
// and the empty-string-param convention (treated as absent, so the
// store is reached — the injected error proves it).
func TestNotificationsBadParams(t *testing.T) {
	// The injected listErr would surface as a 500 if the store were
	// reached; a 400 proves the query was rejected before it.
	store := &memNotificationStore{listErr: errors.New("boom")}
	h, key := newNotificationsServer(t, store)
	hdr := authHdr(mintIdentityToken(t, key, "bad-a", "a@localhost", "A", "a"))

	cases := []struct {
		name  string
		query string
		msg   string
	}{
		{"non-boolean unread", "?unread=maybe", "unread must be a boolean"},
		{"non-integer limit", "?limit=abc", "limit must be an integer >= 0"},
		{"negative limit", "?limit=-1", "limit must be an integer >= 0"},
		{"non-integer offset", "?offset=abc", "offset must be an integer >= 0"},
		{"negative offset", "?offset=-5", "offset must be an integer >= 0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out := doReq(t, h, http.MethodGet, "/api/v1/notifications"+tc.query, "", hdr)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", code, out)
			}
			if c, m := apiErr(t, out); c != codeInvalidRequest || m != tc.msg {
				t.Errorf("error = %s/%q, want %s/%q", c, m, codeInvalidRequest, tc.msg)
			}
		})
	}
	t.Run("empty param is absent and reaches the store", func(t *testing.T) {
		code, out := doReq(t, h, http.MethodGet, "/api/v1/notifications?limit=", "", hdr)
		if code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeInternal || m != "notification operation failed" {
			t.Errorf("error = %s/%q, want %s/notification operation failed", c, m, codeInternal)
		}
	})
}

// TestNotificationsMarkRead covers the 200-with-the-row response and
// the first-read-wins idempotency (a second POST returns the original
// read_at, byte for byte).
func TestNotificationsMarkRead(t *testing.T) {
	base := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	budgetID := nextTestID()
	store := &memNotificationStore{}
	id := addNotif(store, "id-mr-a", "budget.alert", "Budget alert", "Body text",
		json.RawMessage(`{"budget_id":"`+budgetID+`"}`), base, false)
	h, key := newNotificationsServer(t, store)
	hdr := authHdr(mintIdentityToken(t, key, "mr-a", "a@localhost", "A", "a"))

	code, out := doReq(t, h, http.MethodPost, "/api/v1/notifications/"+id+"/read", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", code, out)
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(out), &row); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if row["id"] != id {
		t.Errorf("id = %v, want %s", row["id"], id)
	}
	ref, ok := row["ref"].(map[string]any)
	if !ok || ref["budget_id"] != budgetID {
		t.Errorf("ref = %v, want {budget_id: %s}", row["ref"], budgetID)
	}
	first, ok := row["read_at"].(string)
	if !ok {
		t.Fatalf("read_at = %v, want a string", row["read_at"])
	}
	if _, err := time.Parse(time.RFC3339, first); err != nil {
		t.Errorf("read_at %q is not RFC3339: %v", first, err)
	}

	// Second POST: 200 again, and the ORIGINAL read_at.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/notifications/"+id+"/read", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("repeat: status = %d, want 200 (body %s)", code, out)
	}
	var again map[string]any
	if err := json.Unmarshal([]byte(out), &again); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if again["read_at"] != first {
		t.Errorf("repeat read_at = %v, want the original %v (first-read-wins)", again["read_at"], first)
	}
}

// TestNotificationsErrors covers the 404s (malformed path id answered
// before the store is reached; unknown well-formed id) and the
// per-operation 500 mapping.
func TestNotificationsErrors(t *testing.T) {
	boom := errors.New("boom")

	t.Run("malformed path id never reaches the store", func(t *testing.T) {
		store := &memNotificationStore{readErr: boom}
		h, key := newNotificationsServer(t, store)
		hdr := authHdr(mintIdentityToken(t, key, "nt-e", "e@localhost", "E", "e"))
		code, out := doReq(t, h, http.MethodPost, "/api/v1/notifications/nope/read", "", hdr)
		if code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeNotFound || m != "notification not found" {
			t.Errorf("error = %s/%q, want %s/notification not found", c, m, codeNotFound)
		}
	})

	t.Run("unknown well-formed id is 404", func(t *testing.T) {
		store := &memNotificationStore{}
		h, key := newNotificationsServer(t, store)
		hdr := authHdr(mintIdentityToken(t, key, "nt-u", "u@localhost", "U", "u"))
		code, out := doReq(t, h, http.MethodPost, "/api/v1/notifications/"+nextTestID()+"/read", "", hdr)
		if code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeNotFound || m != "notification not found" {
			t.Errorf("error = %s/%q", c, m)
		}
	})

	t.Run("store errors are 500", func(t *testing.T) {
		cases := []struct {
			name   string
			store  *memNotificationStore
			method string
			path   string
		}{
			{"list", &memNotificationStore{listErr: boom}, http.MethodGet, "/api/v1/notifications"},
			{"mark-read", &memNotificationStore{readErr: boom}, http.MethodPost, "/api/v1/notifications/" + nextTestID() + "/read"},
			{"count-unread", &memNotificationStore{countErr: boom}, http.MethodGet, "/api/v1/notifications/unread-count"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				h, key := newNotificationsServer(t, tc.store)
				hdr := authHdr(mintIdentityToken(t, key, "nt-500", "s@localhost", "S", "s"))
				code, out := doReq(t, h, tc.method, tc.path, "", hdr)
				if code != http.StatusInternalServerError {
					t.Fatalf("status = %d, want 500 (body %s)", code, out)
				}
				if c, m := apiErr(t, out); c != codeInternal || m != "notification operation failed" {
					t.Errorf("error = %s/%q, want %s/notification operation failed", c, m, codeInternal)
				}
			})
		}
	})
}

// TestNotificationsCrossUser proves the user-id scoping: B's list and
// count only see B's rows, and B marking A's row is a 404 that leaves
// A's row untouched.
func TestNotificationsCrossUser(t *testing.T) {
	base := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	store := &memNotificationStore{}
	aid := addNotif(store, "id-nu-a", "kind", "t", "b", nil, base, false)
	bid := addNotif(store, "id-nu-b", "kind", "t", "b", nil, base.Add(time.Minute), false)
	h, key := newNotificationsServer(t, store)
	hdrB := authHdr(mintIdentityToken(t, key, "nu-b", "b@localhost", "B", "b"))

	code, out := doReq(t, h, http.MethodGet, "/api/v1/notifications", "", hdrB)
	if code != http.StatusOK {
		t.Fatalf("B list: status = %d, want 200 (body %s)", code, out)
	}
	got := listIDs(t, out)
	if !sameIDs(got, bid) {
		t.Fatalf("B's list = %v, want only %s", got, bid)
	}

	code, out = doReq(t, h, http.MethodGet, "/api/v1/notifications/unread-count", "", hdrB)
	if code != http.StatusOK {
		t.Fatalf("B count: status = %d, want 200 (body %s)", code, out)
	}
	var count struct {
		Unread int `json:"unread"`
	}
	if err := json.Unmarshal([]byte(out), &count); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if count.Unread != 1 {
		t.Errorf("B unread = %d, want 1 (A's row is not B's)", count.Unread)
	}

	code, out = doReq(t, h, http.MethodPost, "/api/v1/notifications/"+aid+"/read", "", hdrB)
	if code != http.StatusNotFound {
		t.Fatalf("B marks A's row: status = %d, want 404 (body %s)", code, out)
	}
	if c, m := apiErr(t, out); c != codeNotFound || m != "notification not found" {
		t.Errorf("error = %s/%q", c, m)
	}
	if store.rows[0].n.ReadAt != nil {
		t.Errorf("A's row was read by B (read_at %s)", store.rows[0].n.ReadAt.Format(time.RFC3339Nano))
	}
}

// TestNotificationsUnreadCount covers the badge source: the count of
// the caller's unread rows, and its drop after a mark-read.
func TestNotificationsUnreadCount(t *testing.T) {
	base := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	store := &memNotificationStore{}
	u1 := addNotif(store, "id-uc-a", "kind", "t", "b", nil, base, false)
	addNotif(store, "id-uc-a", "kind", "t", "b", nil, base.Add(time.Minute), false)
	addNotif(store, "id-uc-a", "kind", "t", "b", nil, base.Add(2*time.Minute), true)
	h, key := newNotificationsServer(t, store)
	hdr := authHdr(mintIdentityToken(t, key, "uc-a", "a@localhost", "A", "a"))

	code, out := doReq(t, h, http.MethodGet, "/api/v1/notifications/unread-count", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", code, out)
	}
	var count struct {
		Unread int `json:"unread"`
	}
	if err := json.Unmarshal([]byte(out), &count); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if count.Unread != 2 {
		t.Fatalf("unread = %d, want 2 (one row is already read)", count.Unread)
	}

	code, out = doReq(t, h, http.MethodPost, "/api/v1/notifications/"+u1+"/read", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("mark-read: status = %d, want 200 (body %s)", code, out)
	}
	code, out = doReq(t, h, http.MethodGet, "/api/v1/notifications/unread-count", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("count after read: status = %d, want 200 (body %s)", code, out)
	}
	if err := json.Unmarshal([]byte(out), &count); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if count.Unread != 1 {
		t.Errorf("unread after read = %d, want 1", count.Unread)
	}
}

// TestNotificationsAuth covers the auth and 503 edges: missing/garbage
// tokens are 401 (status only — the body is the authn package's
// business), and a valid token against a nil store is a 503 on every
// route.
func TestNotificationsAuth(t *testing.T) {
	t.Run("missing and garbage tokens are 401", func(t *testing.T) {
		// testAuth verifies against a nil key set: every token fails.
		h := New(fakeReady{}, BuildInfo{}, testAuth(), nil, nil, nil, nil, nil, nil, &memNotificationStore{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, false, nil, nil, nil)
		if code, _ := doReq(t, h, http.MethodGet, "/api/v1/notifications", "", nil); code != http.StatusUnauthorized {
			t.Errorf("no token: status = %d, want 401", code)
		}
		if code, _ := doReq(t, h, http.MethodPost, "/api/v1/notifications/"+nextTestID()+"/read", "",
			map[string]string{"Authorization": "Bearer not.a.jwt"}); code != http.StatusUnauthorized {
			t.Errorf("garbage token: status = %d, want 401", code)
		}
		if code, _ := doReq(t, h, http.MethodGet, "/api/v1/notifications/unread-count", "", nil); code != http.StatusUnauthorized {
			t.Errorf("no token (count): status = %d, want 401", code)
		}
	})

	t.Run("nil store is 503 with a valid token", func(t *testing.T) {
		h, key := newNotificationsServer(t, nil)
		tok := mintIdentityToken(t, key, "nt-nc", "nc@localhost", "N", "nc")
		hdr := authHdr(tok)
		cases := []struct {
			method string
			path   string
		}{
			{http.MethodGet, "/api/v1/notifications"},
			{http.MethodPost, "/api/v1/notifications/" + nextTestID() + "/read"},
			{http.MethodGet, "/api/v1/notifications/unread-count"},
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

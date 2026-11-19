// Offset route tests (migration 0012): the seven endpoints against an
// in-memory OffsetStore with injected errors, covering the 503 (no
// store), 401 (no token), the scan parameter validation (400s), the
// sentinel-to-status mapping (404/409/400), and the wire shape of the
// success responses. The matcher and group logic itself lives in the
// repo tests (offsets_test.go).
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"finance-tracker/internal/repo"

	"github.com/lestrrat-go/jwx/v3/jwk"
)

// memOffsetStore is an in-memory OffsetStore: each method's fields are
// its canned reply plus an injected error, so a subtest pins one
// handler response without a database. The last* fields capture what
// the handler passed through from the request.
type memOffsetStore struct {
	candidates  []repo.OffsetCandidate
	findErr     error
	insN        int
	insErr      error
	sugs        []repo.OffsetSuggestion
	listErr     error
	acceptID    string
	acceptErr   error
	lastAccept  string
	dismissErr  error
	lastDismiss string
	groupID     string
	setErr      error
	setIDs      []string
	groups      []repo.OffsetGroup
	groupsErr   error
	clearErr    error
	lastClear   string
}

func (s *memOffsetStore) FindOffsetCandidates(ctx context.Context, userID string, dek []byte, from, to *time.Time, windowDays int) ([]repo.OffsetCandidate, error) {
	return s.candidates, s.findErr
}

func (s *memOffsetStore) InsertOffsetSuggestions(ctx context.Context, userID string, cands []repo.OffsetCandidate) (int, error) {
	return s.insN, s.insErr
}

func (s *memOffsetStore) ListOffsetSuggestions(ctx context.Context, userID string, dek []byte, limit, offset int) ([]repo.OffsetSuggestion, error) {
	return s.sugs, s.listErr
}

func (s *memOffsetStore) AcceptOffsetSuggestion(ctx context.Context, userID, id string, dek []byte) (string, error) {
	s.lastAccept = id
	return s.acceptID, s.acceptErr
}

func (s *memOffsetStore) DismissOffsetSuggestion(ctx context.Context, userID, id string) error {
	s.lastDismiss = id
	return s.dismissErr
}

func (s *memOffsetStore) SetOffsetGroup(ctx context.Context, userID string, txIDs []string, dek []byte) (string, error) {
	s.setIDs = txIDs
	return s.groupID, s.setErr
}

func (s *memOffsetStore) ClearOffsetGroup(ctx context.Context, userID, id string) error {
	s.lastClear = id
	return s.clearErr
}

func (s *memOffsetStore) ListOffsetGroups(ctx context.Context, userID string, dek []byte, limit, offset int) ([]repo.OffsetGroup, error) {
	return s.groups, s.groupsErr
}

// newOffsetServer wires New with the live token verifier, the
// in-memory bootstrap, and only the offset store (the other stores
// stay nil — this file never touches their routes). The key is
// returned so tests can mint tokens under the same verifier.
func newOffsetServer(t *testing.T, store OffsetStore) (http.Handler, jwk.Key) {
	t.Helper()
	key := mintKey(t)
	b, err := NewBootstrap(newMemStore(), testKEK(t), nil)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return New(fakeReady{}, BuildInfo{}, liveAuth(t, key), b, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, false, nil, nil, store, nil, nil, nil, nil), key
}

// offsetTx is a canned repo.Transaction leg with only the fields the
// wire shape carries (the account id is a fixed shape-only value).
func offsetTx(id, date, amount, desc string) repo.Transaction {
	tm, err := time.Parse("2006-01-02", date)
	if err != nil {
		panic(err)
	}
	return repo.Transaction{
		ID:          id,
		AccountID:   "00000000-0000-0000-0000-000000000acc",
		DatePosted:  tm,
		Amount:      amount,
		Description: desc,
	}
}

func TestOffsetRoutes(t *testing.T) {
	sub := nextTestID()

	t.Run("unconfigured store answers 503", func(t *testing.T) {
		// The group's requireUser runs before the handler's nil-store
		// check, so the request needs a valid token to reach the 503.
		h, key := newOffsetServer(t, nil)
		tok := mintIdentityToken(t, key, sub, sub+"@localhost", sub, sub)
		for _, tc := range []struct {
			method, path string
		}{
			{"POST", "/api/v1/offsets/scan"},
			{"GET", "/api/v1/offsets/suggestions"},
			{"POST", "/api/v1/offsets/suggestions/" + nextTestID() + "/accept"},
			{"POST", "/api/v1/offsets/suggestions/" + nextTestID() + "/dismiss"},
			{"POST", "/api/v1/offsets/groups"},
			{"GET", "/api/v1/offsets/groups"},
			{"DELETE", "/api/v1/offsets/groups/" + nextTestID()},
		} {
			code, _ := doReq(t, h, tc.method, tc.path, "", authHdr(tok))
			if code != http.StatusServiceUnavailable {
				t.Errorf("%s %s = %d, want 503", tc.method, tc.path, code)
			}
		}
	})

	t.Run("missing token answers 401", func(t *testing.T) {
		h, _ := newOffsetServer(t, &memOffsetStore{})
		for _, tc := range []struct {
			method, path string
		}{
			{"POST", "/api/v1/offsets/scan"},
			{"GET", "/api/v1/offsets/suggestions"},
			{"POST", "/api/v1/offsets/suggestions/" + nextTestID() + "/accept"},
			{"POST", "/api/v1/offsets/suggestions/" + nextTestID() + "/dismiss"},
			{"POST", "/api/v1/offsets/groups"},
			{"GET", "/api/v1/offsets/groups"},
			{"DELETE", "/api/v1/offsets/groups/" + nextTestID()},
		} {
			code, body := doReq(t, h, tc.method, tc.path, "", nil)
			if code != http.StatusUnauthorized {
				t.Errorf("%s %s = %d, want 401 (body %s)", tc.method, tc.path, code, body)
			}
		}
	})

	t.Run("scan rejects bad window params", func(t *testing.T) {
		h, key := newOffsetServer(t, &memOffsetStore{})
		tok := mintIdentityToken(t, key, sub, sub+"@localhost", sub, sub)
		for _, q := range []string{
			"?from=11/01/2026",
			"?to=2026-13-45",
			"?window_days=0",
			"?window_days=61",
			"?window_days=abc",
		} {
			code, body := doReq(t, h, "POST", "/api/v1/offsets/scan"+q, "", authHdr(tok))
			if code != http.StatusBadRequest {
				t.Errorf("scan %s = %d, want 400 (body %s)", q, code, body)
			}
		}
	})

	t.Run("scan proposes the found pairs", func(t *testing.T) {
		store := &memOffsetStore{
			candidates: []repo.OffsetCandidate{{
				A: offsetTx(nextTestID(), "2026-11-01", "-100.00", "cc pay"),
				B: offsetTx(nextTestID(), "2026-11-03", "100.00", "cc pay posted"),
			}},
			insN: 1,
		}
		h, key := newOffsetServer(t, store)
		tok := mintIdentityToken(t, key, sub, sub+"@localhost", sub, sub)
		code, body := doReq(t, h, "POST", "/api/v1/offsets/scan", "", authHdr(tok))
		if code != http.StatusOK {
			t.Fatalf("scan = %d, want 200 (body %s)", code, body)
		}
		var out struct {
			Suggested int `json:"suggested"`
		}
		// writeJSON appends a trailing newline, so pin the shape by
		// decoding rather than an exact string.
		if err := json.Unmarshal([]byte(body), &out); err != nil || out.Suggested != 1 {
			t.Fatalf("scan body %q, want suggested=1", body)
		}
	})

	t.Run("suggestions list carries both legs opened", func(t *testing.T) {
		legA, legB := offsetTx(nextTestID(), "2026-11-01", "-100.00", "cc pay"),
			offsetTx(nextTestID(), "2026-11-03", "100.00", "cc pay posted")
		store := &memOffsetStore{sugs: []repo.OffsetSuggestion{{
			ID:        nextTestID(),
			A:         legA,
			B:         legB,
			CreatedAt: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC),
		}}}
		h, key := newOffsetServer(t, store)
		tok := mintIdentityToken(t, key, sub, sub+"@localhost", sub, sub)
		code, body := doReq(t, h, "GET", "/api/v1/offsets/suggestions", "", authHdr(tok))
		if code != http.StatusOK {
			t.Fatalf("list = %d, want 200 (body %s)", code, body)
		}
		var got []struct {
			ID        string `json:"id"`
			CreatedAt string `json:"created_at"`
			A         struct {
				ID          string `json:"id"`
				DatePosted  string `json:"date_posted"`
				Amount      string `json:"amount"`
				Description string `json:"description"`
			} `json:"a"`
			B struct {
				ID          string `json:"id"`
				DatePosted  string `json:"date_posted"`
				Amount      string `json:"amount"`
				Description string `json:"description"`
			} `json:"b"`
		}
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("decode: %v (body %s)", err, body)
		}
		if len(got) != 1 || got[0].A.ID != legA.ID || got[0].B.ID != legB.ID ||
			got[0].A.DatePosted != "2026-11-01" || got[0].B.Amount != "100.00" ||
			got[0].A.Description != "cc pay" ||
			got[0].CreatedAt != "2026-11-03T12:00:00Z" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("accept: 404s, then the group id", func(t *testing.T) {
		sid := nextTestID()
		store := &memOffsetStore{acceptID: nextTestID()}
		h, key := newOffsetServer(t, store)
		tok := mintIdentityToken(t, key, sub, sub+"@localhost", sub, sub)

		// A malformed id is rejected before the store is touched.
		code, body := doReq(t, h, "POST", "/api/v1/offsets/suggestions/not-a-uuid/accept", "", authHdr(tok))
		if code != http.StatusNotFound || store.lastAccept != "" {
			t.Fatalf("malformed accept = %d %q, want 404 without a store call", code, body)
		}

		// A resolved (or foreign) suggestion maps to 404 too.
		store.acceptErr = repo.ErrOffsetSuggestionNotFound
		code, body = doReq(t, h, "POST", "/api/v1/offsets/suggestions/"+sid+"/accept", "", authHdr(tok))
		if code != http.StatusNotFound || store.lastAccept != sid {
			t.Fatalf("resolved accept = %d %q, want 404 with the id passed through", code, body)
		}

		store.acceptErr = nil
		code, body = doReq(t, h, "POST", "/api/v1/offsets/suggestions/"+sid+"/accept", "", authHdr(tok))
		if code != http.StatusOK {
			t.Fatalf("accept = %d, want 200 (body %s)", code, body)
		}
		var out struct {
			GroupID string `json:"group_id"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil || out.GroupID != store.acceptID {
			t.Fatalf("accept body %q, want group_id %s", body, store.acceptID)
		}
	})

	t.Run("dismiss answers 204, unknown suggestion 404", func(t *testing.T) {
		sid := nextTestID()
		store := &memOffsetStore{}
		h, key := newOffsetServer(t, store)
		tok := mintIdentityToken(t, key, sub, sub+"@localhost", sub, sub)

		code, body := doReq(t, h, "POST", "/api/v1/offsets/suggestions/"+sid+"/dismiss", "", authHdr(tok))
		if code != http.StatusNoContent || store.lastDismiss != sid {
			t.Fatalf("dismiss = %d %q, want 204 with the id passed through", code, body)
		}

		store.dismissErr = repo.ErrOffsetSuggestionNotFound
		code, body = doReq(t, h, "POST", "/api/v1/offsets/suggestions/"+sid+"/dismiss", "", authHdr(tok))
		if code != http.StatusNotFound {
			t.Fatalf("resolved dismiss = %d %q, want 404", code, body)
		}
	})

	t.Run("create group: body validation and sentinel mapping", func(t *testing.T) {
		id1, id2 := nextTestID(), nextTestID()
		validBody := fmt.Sprintf(`{"transaction_ids":[%q,%q]}`, id1, id2)
		store := &memOffsetStore{groupID: nextTestID()}
		h, key := newOffsetServer(t, store)
		tok := mintIdentityToken(t, key, sub, sub+"@localhost", sub, sub)

		for name, body := range map[string]string{
			"empty body":    "",
			"non-uuid id":   `{"transaction_ids":["nope"]}`,
			"single id":     fmt.Sprintf(`{"transaction_ids":[%q]}`, id1),
			"duplicate ids": fmt.Sprintf(`{"transaction_ids":[%q,%q]}`, id1, id1),
		} {
			code, got := doReq(t, h, "POST", "/api/v1/offsets/groups", body, authHdr(tok))
			if code != http.StatusBadRequest {
				t.Errorf("create (%s) = %d, want 400 (body %s)", name, code, got)
			}
		}

		store.setErr = repo.ErrOffsetNotBalanced
		code, got := doReq(t, h, "POST", "/api/v1/offsets/groups", validBody, authHdr(tok))
		if code != http.StatusBadRequest {
			t.Errorf("unbalanced create = %d, want 400 (body %s)", code, got)
		}

		store.setErr = repo.ErrOffsetAlreadyGrouped
		code, got = doReq(t, h, "POST", "/api/v1/offsets/groups", validBody, authHdr(tok))
		if code != http.StatusConflict {
			t.Errorf("grouped create = %d, want 409 (body %s)", code, got)
		}

		store.setErr = nil
		code, got = doReq(t, h, "POST", "/api/v1/offsets/groups", validBody, authHdr(tok))
		if code != http.StatusOK || len(store.setIDs) != 2 || store.setIDs[0] != id1 || store.setIDs[1] != id2 {
			t.Fatalf("create = %d %q, want 200 with the ids passed through", code, got)
		}
		var out struct {
			GroupID string `json:"group_id"`
		}
		if err := json.Unmarshal([]byte(got), &out); err != nil || out.GroupID != store.groupID {
			t.Fatalf("create body %q, want group_id %s", got, store.groupID)
		}
	})

	t.Run("groups list and clear", func(t *testing.T) {
		gid := nextTestID()
		store := &memOffsetStore{groups: []repo.OffsetGroup{{
			ID:      gid,
			Members: []repo.Transaction{offsetTx(nextTestID(), "2026-11-01", "-100.00", "cc pay")},
		}}}
		h, key := newOffsetServer(t, store)
		tok := mintIdentityToken(t, key, sub, sub+"@localhost", sub, sub)

		code, body := doReq(t, h, "GET", "/api/v1/offsets/groups", "", authHdr(tok))
		if code != http.StatusOK {
			t.Fatalf("list groups = %d, want 200 (body %s)", code, body)
		}
		var got []struct {
			ID      string `json:"id"`
			Members []struct {
				ID     string `json:"id"`
				Amount string `json:"amount"`
			} `json:"members"`
		}
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("decode groups: %v (body %s)", err, body)
		}
		if len(got) != 1 || got[0].ID != gid || len(got[0].Members) != 1 || got[0].Members[0].Amount != "-100.00" {
			t.Fatalf("groups = %+v, want one group %s with one member", got, gid)
		}

		// Malformed id, unknown group, then the clean dissolve.
		code, body = doReq(t, h, "DELETE", "/api/v1/offsets/groups/not-a-uuid", "", authHdr(tok))
		if code != http.StatusNotFound || store.lastClear != "" {
			t.Fatalf("malformed clear = %d %q, want 404 without a store call", code, body)
		}
		store.clearErr = repo.ErrOffsetGroupNotFound
		code, body = doReq(t, h, "DELETE", "/api/v1/offsets/groups/"+gid, "", authHdr(tok))
		if code != http.StatusNotFound {
			t.Fatalf("unknown clear = %d %q, want 404", code, body)
		}
		store.clearErr = nil
		code, body = doReq(t, h, "DELETE", "/api/v1/offsets/groups/"+gid, "", authHdr(tok))
		if code != http.StatusNoContent || store.lastClear != gid {
			t.Fatalf("clear = %d %q, want 204 with the id passed through", code, body)
		}
	})
}

// review_test.go is the route-level tests for the review queue (plan
// step 5.5): GET /api/v1/review, the per-row accept, the accept-all
// bulk action, and the single-row reprompt. All in-memory:
// memReviewStore mirrors the repo.TransactionStore review semantics the
// handlers depend on (caller scoping, the review-pending predicate,
// category ownership, the repo's probe ordering) and injects
// per-operation errors for the mapping table.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"finance-tracker/internal/categorize"
	"finance-tracker/internal/llm"
	"finance-tracker/internal/repo"

	"github.com/lestrrat-go/jwx/v3/jwk"
)

// memReviewRow is one review-queue row: the owner's user id plus the
// transaction in its current state.
type memReviewRow struct {
	userID string
	tx     repo.Transaction
}

// pendingTx builds a review-pending transaction with a deterministic
// shape (the date is parsed as UTC, the way the API renders it back).
func pendingTx(userID, id, accountID, date, amount, desc string) memReviewRow {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		panic(err)
	}
	return memReviewRow{userID: userID, tx: repo.Transaction{
		ID: id, AccountID: accountID, DatePosted: t, Amount: amount,
		Description: desc, ReviewPending: true,
	}}
}

// memReviewStore is an in-memory ReviewStore fake. The queue predicate
// is the production one (review_pending AND no category yet);
// ownership is enforced on both the row and the category;
// AcceptReview/GetReviewPending answer the category probe before the
// row lookup, the repo's ordering.
type memReviewStore struct {
	rows         []memReviewRow
	cats         map[string]map[string]bool // user id -> owned category ids
	listErr      error
	getErr       error
	acceptErr    error
	acceptAllErr error
}

func (s *memReviewStore) addCat(userID, catID string) {
	if s.cats == nil {
		s.cats = map[string]map[string]bool{}
	}
	if s.cats[userID] == nil {
		s.cats[userID] = map[string]bool{}
	}
	s.cats[userID][catID] = true
}

func (s *memReviewStore) ownsCat(userID, catID string) bool {
	return s.cats[userID][catID]
}

// pending finds (userID, id) while it is still review pending.
func (s *memReviewStore) pending(userID, id string) (repo.Transaction, bool) {
	for i := range s.rows {
		r := &s.rows[i]
		if r.userID == userID && r.tx.ID == id && r.tx.ReviewPending && r.tx.CategoryID == "" {
			return r.tx, true
		}
	}
	return repo.Transaction{}, false
}

func (s *memReviewStore) ReviewPending(_ context.Context, userID string, _ []byte) ([]repo.Transaction, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := make([]repo.Transaction, 0)
	for i := range s.rows {
		r := &s.rows[i]
		if r.userID == userID && r.tx.ReviewPending && r.tx.CategoryID == "" {
			out = append(out, r.tx)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *memReviewStore) GetReviewPending(_ context.Context, userID, id string, _ []byte) (repo.Transaction, error) {
	if s.getErr != nil {
		return repo.Transaction{}, s.getErr
	}
	tx, ok := s.pending(userID, id)
	if !ok {
		return repo.Transaction{}, repo.ErrTransactionNotFound
	}
	return tx, nil
}

func (s *memReviewStore) AcceptReview(_ context.Context, userID, txnID, categoryID string) error {
	if s.acceptErr != nil {
		return s.acceptErr
	}
	if !s.ownsCat(userID, categoryID) {
		return repo.ErrCategoryNotFound
	}
	for i := range s.rows {
		r := &s.rows[i]
		if r.userID == userID && r.tx.ID == txnID && r.tx.ReviewPending && r.tx.CategoryID == "" {
			r.tx.CategoryID = categoryID
			r.tx.ReviewPending = false
			return nil
		}
	}
	return repo.ErrTransactionNotFound
}

func (s *memReviewStore) AcceptAll(_ context.Context, userID, categoryID string) (int, error) {
	if s.acceptAllErr != nil {
		return 0, s.acceptAllErr
	}
	if !s.ownsCat(userID, categoryID) {
		return 0, repo.ErrCategoryNotFound
	}
	n := 0
	for i := range s.rows {
		r := &s.rows[i]
		if r.userID == userID && r.tx.ReviewPending && r.tx.CategoryID == "" {
			r.tx.CategoryID = categoryID
			r.tx.ReviewPending = false
			n++
		}
	}
	return n, nil
}

// stubCategorizer is an llm.Categorizer that returns a canned JSON
// payload and records every request it is sent. Pointer receiver: the
// Reprompter's LLM factory hands out the same instance the test
// asserts on, so the recorded requests must accumulate on it.
type stubCategorizer struct {
	resp json.RawMessage
	err  error
	reqs []llm.Request
}

func (c *stubCategorizer) Categorize(_ context.Context, req llm.Request) (*llm.Response, error) {
	c.reqs = append(c.reqs, req)
	if c.err != nil {
		return nil, c.err
	}
	return &llm.Response{JSON: c.resp}, nil
}

// fakeCreds resolves a fixed credential or error. The zero value is
// the env-fallback case: like the repo's Resolve, "no stored
// credential" is ErrCredentialNotFound (Reprompt falls back to the
// env only on that error, never on an empty credential).
type fakeCreds struct {
	cred repo.ResolvedCredential
	err  error
}

func (f fakeCreds) Resolve(_ context.Context, _ string, _ []byte) (repo.ResolvedCredential, error) {
	if f.err != nil {
		return repo.ResolvedCredential{}, f.err
	}
	if f.cred == (repo.ResolvedCredential{}) {
		return repo.ResolvedCredential{}, repo.ErrCredentialNotFound
	}
	return f.cred, nil
}

// fakeCats lists fixed categories (or a fixed error).
type fakeCats struct {
	cats []repo.Category
	err  error
}

func (f fakeCats) List(_ context.Context, _ string, _ []byte) ([]repo.Category, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.cats, nil
}

// llmArgs captures the (baseURL, apiKey, model) the reprompt passed to
// the LLM factory, so tests can pin the credential-resolution outcome.
type llmArgs struct {
	baseURL string
	apiKey  string
	model   string
}

// catBody is the JSON body {"category_id": id} the accept routes take.
func catBody(catID string) string {
	return fmt.Sprintf(`{"category_id":%q}`, catID)
}

// newReviewServer wires New with the live token verifier, the in-memory
// bootstrap, and only the review store and reprompter (the other stores
// stay nil — this file never touches their routes). The key is returned
// so tests can mint tokens for several users under the same verifier.
func newReviewServer(t *testing.T, store ReviewStore, rep *Reprompter) (http.Handler, jwk.Key) {
	t.Helper()
	key := mintKey(t)
	b, err := NewBootstrap(newMemStore(), testKEK(t), nil)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return New(fakeReady{}, BuildInfo{}, liveAuth(t, key), b, nil, nil, nil, nil, nil, nil, nil, nil, nil, store, rep, nil, nil, nil, nil, nil, nil, false, nil, nil, nil), key
}

// reviewHdr is the Authorization header for a fresh identity token on
// sub (profile claims filled from the sub, the way the other route
// tests do).
func reviewHdr(t *testing.T, key jwk.Key, sub string) map[string]string {
	t.Helper()
	return authHdr(mintIdentityToken(t, key, sub, sub+"@localhost", sub, sub))
}

func TestReviewList(t *testing.T) {
	const sub = "alice"
	userID := "id-" + sub
	acct := nextTestID()
	idA, idB, idC, idD := nextTestID(), nextTestID(), nextTestID(), nextTestID()
	store := &memReviewStore{}
	// Two pending rows seeded OUT of id order, one already categorised
	// (auto — must not appear), one belonging to another user (must not
	// appear).
	store.rows = []memReviewRow{
		pendingTx(userID, idB, acct, "2026-11-02", "12.50", "beta"),
		pendingTx(userID, idA, acct, "2026-11-01", "10.00", "alpha"),
		{userID: userID, tx: repo.Transaction{
			ID: idC, AccountID: acct,
			DatePosted: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
			Amount:     "5.00", Description: "categorised", CategoryID: nextTestID(),
		}},
		pendingTx("id-bob", idD, acct, "2026-11-03", "1.00", "theirs"),
	}
	h, key := newReviewServer(t, store, nil)

	code, body := doReq(t, h, http.MethodGet, "/api/v1/review", "", reviewHdr(t, key, sub))
	if code != http.StatusOK {
		t.Fatalf("GET /review status = %d, want 200 (body %s)", code, body)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		t.Fatalf("decode list: %v (body %s)", err, body)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (body %s)", len(rows), body)
	}
	wantKeys := map[string]bool{"id": true, "account_id": true, "date_posted": true, "amount": true, "description": true}
	for i, row := range rows {
		if len(row) != len(wantKeys) {
			t.Fatalf("row %d = %v, want exactly keys %v", i, row, wantKeys)
		}
		for k := range row {
			if !wantKeys[k] {
				t.Fatalf("row %d has unexpected key %q", i, k)
			}
		}
	}
	if rows[0]["id"] != idA || rows[1]["id"] != idB {
		t.Fatalf("ordering = %v %v, want id order %s then %s", rows[0]["id"], rows[1]["id"], idA, idB)
	}
	if rows[0]["description"] != "alpha" || rows[0]["account_id"] != acct ||
		rows[0]["date_posted"] != "2026-11-01" || rows[0]["amount"] != "10.00" {
		t.Fatalf("row 0 = %v", rows[0])
	}

	// The other user sees only their own row.
	code, body = doReq(t, h, http.MethodGet, "/api/v1/review", "", reviewHdr(t, key, "bob"))
	if code != http.StatusOK {
		t.Fatalf("GET /review (bob) status = %d, want 200 (body %s)", code, body)
	}
	if !strings.Contains(body, "theirs") || strings.Contains(body, "alpha") {
		t.Fatalf("bob's list = %s", body)
	}

	t.Run("nil store", func(t *testing.T) {
		h2, key2 := newReviewServer(t, nil, nil)
		code, body := doReq(t, h2, http.MethodGet, "/api/v1/review", "", reviewHdr(t, key2, sub))
		if code != http.StatusServiceUnavailable {
			t.Fatalf("nil store status = %d, want 503 (body %s)", code, body)
		}
		if c, m := apiErr(t, body); c != "internal" || m != "database not configured" {
			t.Fatalf("err = %s/%s", c, m)
		}
	})

	t.Run("store error", func(t *testing.T) {
		store.listErr = errors.New("boom")
		code, body := doReq(t, h, http.MethodGet, "/api/v1/review", "", reviewHdr(t, key, sub))
		if code != http.StatusInternalServerError {
			t.Fatalf("store error status = %d, want 500 (body %s)", code, body)
		}
		if c, m := apiErr(t, body); c != "internal" || m != "review operation failed" {
			t.Fatalf("err = %s/%s", c, m)
		}
	})
}

func TestReviewAccept(t *testing.T) {
	const sub = "alice"
	userID := "id-" + sub
	catA, catB := nextTestID(), nextTestID()
	acct := nextTestID()

	newCase := func(t *testing.T) (*memReviewStore, http.Handler, jwk.Key) {
		t.Helper()
		store := &memReviewStore{}
		store.addCat(userID, catA)
		store.addCat("id-bob", catB)
		h, key := newReviewServer(t, store, nil)
		return store, h, key
	}

	t.Run("accept one row", func(t *testing.T) {
		store, h, key := newCase(t)
		tx := pendingTx(userID, nextTestID(), acct, "2026-11-10", "42.50", "ZKX 9911 REF")
		store.rows = append(store.rows, tx)

		code, body := doReq(t, h, http.MethodPost, "/api/v1/review/"+tx.tx.ID+"/accept", catBody(catA), reviewHdr(t, key, sub))
		if code != http.StatusOK {
			t.Fatalf("accept status = %d, want 200 (body %s)", code, body)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode row: %v (body %s)", err, body)
		}
		// The full updated row (the PATCH house pattern): every column
		// of the transactions list, category set, review cleared.
		if out["id"] != tx.tx.ID || out["account_id"] != acct || out["date_posted"] != "2026-11-10" ||
			out["amount"] != "42.50" || out["description"] != "ZKX 9911 REF" ||
			out["category_id"] != catA || out["review_pending"] != false {
			t.Fatalf("row = %v", out)
		}
		if out["date_executed"] != nil || out["notes"] != nil || out["raw"] != nil {
			t.Fatalf("unset columns must be null: %v", out)
		}
		if store.rows[0].tx.CategoryID != catA || store.rows[0].tx.ReviewPending {
			t.Fatalf("store row = %+v", store.rows[0].tx)
		}

		// The row left the queue.
		code, body = doReq(t, h, http.MethodGet, "/api/v1/review", "", reviewHdr(t, key, sub))
		if code != http.StatusOK || strings.TrimSpace(body) != "[]" {
			t.Fatalf("queue after accept = %q (status %d)", body, code)
		}
		// Re-accepting the resolved row is a 404 (it is no longer
		// review pending).
		code, body = doReq(t, h, http.MethodPost, "/api/v1/review/"+tx.tx.ID+"/accept", catBody(catA), reviewHdr(t, key, sub))
		if code != http.StatusNotFound {
			t.Fatalf("re-accept status = %d, want 404 (body %s)", code, body)
		}
		if c, m := apiErr(t, body); c != "not_found" || m != "transaction not found" {
			t.Fatalf("err = %s/%s", c, m)
		}
	})

	t.Run("not mine is 404", func(t *testing.T) {
		store, h, key := newCase(t)
		tx := pendingTx(userID, nextTestID(), acct, "2026-11-11", "7.00", "not yours")
		store.rows = append(store.rows, tx)
		code, body := doReq(t, h, http.MethodPost, "/api/v1/review/"+tx.tx.ID+"/accept", catBody(catB), reviewHdr(t, key, "bob"))
		if code != http.StatusNotFound {
			t.Fatalf("foreign accept status = %d, want 404 (body %s)", code, body)
		}
		if c, m := apiErr(t, body); c != "not_found" || m != "transaction not found" {
			t.Fatalf("err = %s/%s", c, m)
		}
		// The row stays pending for its owner.
		code, body = doReq(t, h, http.MethodGet, "/api/v1/review", "", reviewHdr(t, key, sub))
		if code != http.StatusOK || !strings.Contains(body, "not yours") {
			t.Fatalf("row must stay pending (status %d body %s)", code, body)
		}
	})

	t.Run("validation", func(t *testing.T) {
		cases := []struct {
			name string
			body string
			msg  string
		}{
			{"missing category_id", `{}`, "category_id must be a valid UUID"},
			{"malformed category_id", `{"category_id":"nope"}`, "category_id must be a valid UUID"},
			{"not json", `not json`, "body must be a JSON object"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				store, h, key := newCase(t)
				store.rows = append(store.rows, pendingTx(userID, nextTestID(), acct, "2026-11-12", "1.00", "validate"))
				code, body := doReq(t, h, http.MethodPost, "/api/v1/review/"+store.rows[0].tx.ID+"/accept", tc.body, reviewHdr(t, key, sub))
				if code != http.StatusBadRequest {
					t.Fatalf("%s: status = %d, want 400 (body %s)", tc.name, code, body)
				}
				if c, m := apiErr(t, body); c != "invalid_request" || !strings.HasPrefix(m, tc.msg) {
					t.Fatalf("%s: err = %s/%s", tc.name, c, m)
				}
			})
		}
	})

	t.Run("foreign category is 400", func(t *testing.T) {
		store, h, key := newCase(t)
		tx := pendingTx(userID, nextTestID(), acct, "2026-11-13", "2.00", "foreign cat")
		store.rows = append(store.rows, tx)
		code, body := doReq(t, h, http.MethodPost, "/api/v1/review/"+tx.tx.ID+"/accept", catBody(catB), reviewHdr(t, key, sub))
		if code != http.StatusBadRequest {
			t.Fatalf("foreign category status = %d, want 400 (body %s)", code, body)
		}
		if c, m := apiErr(t, body); c != "invalid_request" || m != "category_id is not one of your categories" {
			t.Fatalf("err = %s/%s", c, m)
		}
		// The row is untouched: the category probe fails first.
		code, body = doReq(t, h, http.MethodGet, "/api/v1/review", "", reviewHdr(t, key, sub))
		if code != http.StatusOK || !strings.Contains(body, "foreign cat") {
			t.Fatalf("row must stay pending (status %d body %s)", code, body)
		}
	})

	t.Run("malformed id is 404", func(t *testing.T) {
		_, h, key := newCase(t)
		code, body := doReq(t, h, http.MethodPost, "/api/v1/review/not-a-uuid/accept", catBody(catA), reviewHdr(t, key, sub))
		if code != http.StatusNotFound {
			t.Fatalf("malformed id status = %d, want 404 (body %s)", code, body)
		}
		if c, m := apiErr(t, body); c != "not_found" || m != "transaction not found" {
			t.Fatalf("err = %s/%s", c, m)
		}
	})

	t.Run("store error", func(t *testing.T) {
		store, h, key := newCase(t)
		tx := pendingTx(userID, nextTestID(), acct, "2026-11-14", "3.00", "boom")
		store.rows = append(store.rows, tx)
		store.acceptErr = errors.New("boom")
		code, body := doReq(t, h, http.MethodPost, "/api/v1/review/"+tx.tx.ID+"/accept", catBody(catA), reviewHdr(t, key, sub))
		if code != http.StatusInternalServerError {
			t.Fatalf("store error status = %d, want 500 (body %s)", code, body)
		}
		if c, m := apiErr(t, body); c != "internal" || m != "review operation failed" {
			t.Fatalf("err = %s/%s", c, m)
		}
	})

	t.Run("nil store", func(t *testing.T) {
		h, key := newReviewServer(t, nil, nil)
		code, body := doReq(t, h, http.MethodPost, "/api/v1/review/"+nextTestID()+"/accept", catBody(catA), reviewHdr(t, key, sub))
		if code != http.StatusServiceUnavailable {
			t.Fatalf("nil store status = %d, want 503 (body %s)", code, body)
		}
		if c, m := apiErr(t, body); c != "internal" || m != "database not configured" {
			t.Fatalf("err = %s/%s", c, m)
		}
	})
}

func TestReviewAcceptAll(t *testing.T) {
	const sub = "alice"
	userID := "id-" + sub
	otherID := "id-bob"
	catA, catB := nextTestID(), nextTestID()
	acct := nextTestID()

	newCase := func(t *testing.T) (*memReviewStore, http.Handler, jwk.Key) {
		t.Helper()
		store := &memReviewStore{}
		store.addCat(userID, catA)
		store.addCat(otherID, catB)
		store.rows = []memReviewRow{
			pendingTx(userID, nextTestID(), acct, "2026-11-01", "1.00", "mine one"),
			pendingTx(userID, nextTestID(), acct, "2026-11-02", "2.00", "mine two"),
			pendingTx(userID, nextTestID(), acct, "2026-11-03", "3.00", "mine three"),
			pendingTx(otherID, nextTestID(), acct, "2026-11-04", "4.00", "theirs"),
		}
		h, key := newReviewServer(t, store, nil)
		return store, h, key
	}

	t.Run("applies to the caller only", func(t *testing.T) {
		_, h, key := newCase(t)
		code, body := doReq(t, h, http.MethodPost, "/api/v1/review/accept-all", catBody(catA), reviewHdr(t, key, sub))
		if code != http.StatusOK {
			t.Fatalf("accept-all status = %d, want 200 (body %s)", code, body)
		}
		var out struct {
			Applied int `json:"applied"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode: %v (body %s)", err, body)
		}
		if out.Applied != 3 {
			t.Fatalf("applied = %d, want 3 (body %s)", out.Applied, body)
		}
		code, body = doReq(t, h, http.MethodGet, "/api/v1/review", "", reviewHdr(t, key, sub))
		if code != http.StatusOK || strings.TrimSpace(body) != "[]" {
			t.Fatalf("caller queue = %q (status %d)", body, code)
		}
		// The other user's row is untouched.
		code, body = doReq(t, h, http.MethodGet, "/api/v1/review", "", reviewHdr(t, key, "bob"))
		if code != http.StatusOK || !strings.Contains(body, "theirs") {
			t.Fatalf("other user's row must be untouched (status %d body %s)", code, body)
		}
		// An empty queue applies to nothing.
		code, body = doReq(t, h, http.MethodPost, "/api/v1/review/accept-all", catBody(catA), reviewHdr(t, key, sub))
		if code != http.StatusOK || !strings.Contains(body, `"applied":0`) {
			t.Fatalf("second accept-all = %q (status %d)", body, code)
		}
	})

	t.Run("validation", func(t *testing.T) {
		cases := []struct {
			name string
			body string
			msg  string
		}{
			{"missing category_id", `{}`, "category_id must be a valid UUID"},
			{"malformed category_id", `{"category_id":"nope"}`, "category_id must be a valid UUID"},
			{"not json", `not json`, "body must be a JSON object"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, h, key := newCase(t)
				code, body := doReq(t, h, http.MethodPost, "/api/v1/review/accept-all", tc.body, reviewHdr(t, key, sub))
				if code != http.StatusBadRequest {
					t.Fatalf("%s: status = %d, want 400 (body %s)", tc.name, code, body)
				}
				if c, m := apiErr(t, body); c != "invalid_request" || !strings.HasPrefix(m, tc.msg) {
					t.Fatalf("%s: err = %s/%s", tc.name, c, m)
				}
			})
		}

		t.Run("foreign category", func(t *testing.T) {
			_, h, key := newCase(t)
			code, body := doReq(t, h, http.MethodPost, "/api/v1/review/accept-all", catBody(catB), reviewHdr(t, key, sub))
			if code != http.StatusBadRequest {
				t.Fatalf("foreign category status = %d, want 400 (body %s)", code, body)
			}
			if c, m := apiErr(t, body); c != "invalid_request" || m != "category_id is not one of your categories" {
				t.Fatalf("err = %s/%s", c, m)
			}
		})
	})

	t.Run("store error", func(t *testing.T) {
		store, h, key := newCase(t)
		store.acceptAllErr = errors.New("boom")
		code, body := doReq(t, h, http.MethodPost, "/api/v1/review/accept-all", catBody(catA), reviewHdr(t, key, sub))
		if code != http.StatusInternalServerError {
			t.Fatalf("store error status = %d, want 500 (body %s)", code, body)
		}
		if c, m := apiErr(t, body); c != "internal" || m != "review operation failed" {
			t.Fatalf("err = %s/%s", c, m)
		}
	})

	t.Run("nil store", func(t *testing.T) {
		h, key := newReviewServer(t, nil, nil)
		code, body := doReq(t, h, http.MethodPost, "/api/v1/review/accept-all", catBody(catA), reviewHdr(t, key, sub))
		if code != http.StatusServiceUnavailable {
			t.Fatalf("nil store status = %d, want 503 (body %s)", code, body)
		}
		if c, m := apiErr(t, body); c != "internal" || m != "database not configured" {
			t.Fatalf("err = %s/%s", c, m)
		}
	})
}

func TestReviewReprompt(t *testing.T) {
	const sub = "alice"
	userID := "id-" + sub
	catID := nextTestID()
	acct := nextTestID()
	groceries := repo.Category{ID: catID, Name: "Groceries"}

	// newCase wires a Reprompter over one pending row; the LLM factory
	// records the resolved (baseURL, apiKey, model) and serves stub.
	newCase := func(t *testing.T, stub *stubCategorizer, creds CredentialResolver, cats []repo.Category, catsErr error) (*memReviewStore, http.Handler, string, jwk.Key, *llmArgs) {
		t.Helper()
		store := &memReviewStore{}
		store.addCat(userID, catID)
		row := pendingTx(userID, nextTestID(), acct, "2026-11-30", "12.34", "ZKX 9911 REF")
		store.rows = append(store.rows, row)
		args := &llmArgs{}
		rep := &Reprompter{
			Creds: creds,
			Cats:  fakeCats{cats: cats, err: catsErr},
			Txns:  store,
			Env:   categorize.LLMEnv{BaseURL: "http://env-llama/v1", APIKey: "env-key", Model: "env-model"},
			LLM: func(baseURL, apiKey, model string) (llm.Categorizer, error) {
				args.baseURL, args.apiKey, args.model = baseURL, apiKey, model
				return stub, nil
			},
		}
		h, key := newReviewServer(t, store, rep)
		return store, h, row.tx.ID, key, args
	}

	// suggestion is one canned model answer for the single item, the
	// shape categorize.Run parses (a JSON array of i-keyed entries).
	suggestion := func(t *testing.T, confidence float64) json.RawMessage {
		t.Helper()
		b, err := json.Marshal([]map[string]any{
			{"i": 1, "category": "GROCERIES", "confidence": confidence, "rationale": "clear enough"},
		})
		if err != nil {
			t.Fatalf("marshal canned suggestion: %v", err)
		}
		return b
	}

	t.Run("high confidence applies", func(t *testing.T) {
		stub := &stubCategorizer{resp: suggestion(t, 0.9)}
		store, h, id, key, args := newCase(t, stub, fakeCreds{}, []repo.Category{groceries}, nil)
		code, body := doReq(t, h, http.MethodPost, "/api/v1/review/"+id+"/reprompt", "", reviewHdr(t, key, sub))
		if code != http.StatusOK {
			t.Fatalf("reprompt status = %d, want 200 (body %s)", code, body)
		}
		var out struct {
			Category   string  `json:"category"`
			Confidence float64 `json:"confidence"`
			Rationale  string  `json:"rationale"`
			Applied    bool    `json:"applied"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode: %v (body %s)", err, body)
		}
		// The model answered "GROCERIES"; Run normalises it to the
		// canonical listed name.
		if out.Category != "Groceries" || out.Confidence != 0.9 || out.Rationale != "clear enough" || !out.Applied {
			t.Fatalf("out = %+v (body %s)", out, body)
		}
		// No stored credential: the env values are used verbatim.
		if args.baseURL != "http://env-llama/v1" || args.apiKey != "env-key" || args.model != "env-model" {
			t.Fatalf("resolved llm args = %+v", args)
		}
		// One provider call, with the row's description in the prompt.
		if len(stub.reqs) != 1 || !strings.Contains(stub.reqs[0].User, "ZKX 9911 REF") {
			t.Fatalf("llm requests = %+v", stub.reqs)
		}
		// Applied: the row left the queue and is categorised in the store.
		code, body = doReq(t, h, http.MethodGet, "/api/v1/review", "", reviewHdr(t, key, sub))
		if code != http.StatusOK || strings.TrimSpace(body) != "[]" {
			t.Fatalf("queue after apply = %q (status %d)", body, code)
		}
		if store.rows[0].tx.CategoryID != catID || store.rows[0].tx.ReviewPending {
			t.Fatalf("store row = %+v", store.rows[0].tx)
		}
	})

	t.Run("stored credential wins", func(t *testing.T) {
		cred := repo.ResolvedCredential{Provider: "openai", Endpoint: "https://api.example.com/v1", APIKey: "sk-test"}
		stub := &stubCategorizer{resp: suggestion(t, 0.8)}
		_, h, id, key, args := newCase(t, stub, fakeCreds{cred: cred}, []repo.Category{groceries}, nil)
		code, body := doReq(t, h, http.MethodPost, "/api/v1/review/"+id+"/reprompt", "", reviewHdr(t, key, sub))
		if code != http.StatusOK {
			t.Fatalf("reprompt status = %d, want 200 (body %s)", code, body)
		}
		var out struct {
			Applied bool `json:"applied"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil || !out.Applied {
			t.Fatalf("out = %+v (body %s)", out, body)
		}
		// The stored credential supplies endpoint and key; the model
		// still comes from the env (NormalizeEnv leaves it set).
		if args.baseURL != "https://api.example.com/v1" || args.apiKey != "sk-test" || args.model != "env-model" {
			t.Fatalf("resolved llm args = %+v", args)
		}
	})

	t.Run("low confidence stays pending", func(t *testing.T) {
		stub := &stubCategorizer{resp: suggestion(t, 0.5)}
		store, h, id, key, _ := newCase(t, stub, fakeCreds{}, []repo.Category{groceries}, nil)
		code, body := doReq(t, h, http.MethodPost, "/api/v1/review/"+id+"/reprompt", "", reviewHdr(t, key, sub))
		if code != http.StatusOK {
			t.Fatalf("reprompt status = %d, want 200 (body %s)", code, body)
		}
		var out struct {
			Category   string  `json:"category"`
			Confidence float64 `json:"confidence"`
			Applied    bool    `json:"applied"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode: %v (body %s)", err, body)
		}
		if out.Applied || out.Category != "Groceries" || out.Confidence != 0.5 {
			t.Fatalf("out = %+v (body %s)", out, body)
		}
		// The suggestion is reported but the row is untouched.
		code, body = doReq(t, h, http.MethodGet, "/api/v1/review", "", reviewHdr(t, key, sub))
		if code != http.StatusOK || !strings.Contains(body, id) {
			t.Fatalf("row must stay pending (status %d body %s)", code, body)
		}
		if store.rows[0].tx.CategoryID != "" || !store.rows[0].tx.ReviewPending {
			t.Fatalf("store row = %+v", store.rows[0].tx)
		}
	})

	t.Run("provider error", func(t *testing.T) {
		stub := &stubCategorizer{err: errors.New("llm down")}
		_, h, id, key, _ := newCase(t, stub, fakeCreds{}, []repo.Category{groceries}, nil)
		code, body := doReq(t, h, http.MethodPost, "/api/v1/review/"+id+"/reprompt", "", reviewHdr(t, key, sub))
		if code != http.StatusInternalServerError {
			t.Fatalf("provider error status = %d, want 500 (body %s)", code, body)
		}
		if c, m := apiErr(t, body); c != "internal" || m != "reprompt failed" {
			t.Fatalf("err = %s/%s", c, m)
		}
	})

	t.Run("no categories", func(t *testing.T) {
		stub := &stubCategorizer{resp: suggestion(t, 0.9)}
		_, h, id, key, args := newCase(t, stub, fakeCreds{}, nil, nil)
		code, body := doReq(t, h, http.MethodPost, "/api/v1/review/"+id+"/reprompt", "", reviewHdr(t, key, sub))
		if code != http.StatusInternalServerError {
			t.Fatalf("no categories status = %d, want 500 (body %s)", code, body)
		}
		if c, m := apiErr(t, body); c != "internal" || m != "reprompt failed" {
			t.Fatalf("err = %s/%s", c, m)
		}
		// Fail fast before any provider call.
		if args.baseURL != "" || len(stub.reqs) != 0 {
			t.Fatalf("provider must not be called (args %+v, reqs %+v)", args, stub.reqs)
		}
	})

	t.Run("concurrent accept", func(t *testing.T) {
		stub := &stubCategorizer{resp: suggestion(t, 0.9)}
		store, h, id, key, _ := newCase(t, stub, fakeCreds{}, []repo.Category{groceries}, nil)
		// The row was resolved in the meantime: AcceptReview reports it
		// as gone, which is NOT an error.
		store.acceptErr = repo.ErrTransactionNotFound
		code, body := doReq(t, h, http.MethodPost, "/api/v1/review/"+id+"/reprompt", "", reviewHdr(t, key, sub))
		if code != http.StatusOK {
			t.Fatalf("concurrent accept status = %d, want 200 (body %s)", code, body)
		}
		var out struct {
			Category string `json:"category"`
			Applied  bool   `json:"applied"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode: %v (body %s)", err, body)
		}
		if out.Applied || out.Category != "Groceries" {
			t.Fatalf("out = %+v (body %s)", out, body)
		}
	})

	t.Run("not found", func(t *testing.T) {
		_, h, id, key, _ := newCase(t, &stubCategorizer{}, fakeCreds{}, []repo.Category{groceries}, nil)

		code, body := doReq(t, h, http.MethodPost, "/api/v1/review/not-a-uuid/reprompt", "", reviewHdr(t, key, sub))
		if code != http.StatusNotFound {
			t.Fatalf("malformed id status = %d, want 404 (body %s)", code, body)
		}
		if c, m := apiErr(t, body); c != "not_found" || m != "transaction not found" {
			t.Fatalf("err = %s/%s", c, m)
		}

		// Another user's row: the same 404 (existence is not disclosed).
		code, body = doReq(t, h, http.MethodPost, "/api/v1/review/"+id+"/reprompt", "", reviewHdr(t, key, "bob"))
		if code != http.StatusNotFound {
			t.Fatalf("other user's row status = %d, want 404 (body %s)", code, body)
		}
		if c, m := apiErr(t, body); c != "not_found" || m != "transaction not found" {
			t.Fatalf("err = %s/%s", c, m)
		}
	})

	t.Run("nil wiring", func(t *testing.T) {
		h, key := newReviewServer(t, nil, &Reprompter{})
		code, body := doReq(t, h, http.MethodPost, "/api/v1/review/"+nextTestID()+"/reprompt", "", reviewHdr(t, key, sub))
		if code != http.StatusServiceUnavailable {
			t.Fatalf("nil store status = %d, want 503 (body %s)", code, body)
		}
		if c, m := apiErr(t, body); c != "internal" || m != "database not configured" {
			t.Fatalf("err = %s/%s", c, m)
		}

		h, key = newReviewServer(t, &memReviewStore{}, nil)
		code, body = doReq(t, h, http.MethodPost, "/api/v1/review/"+nextTestID()+"/reprompt", "", reviewHdr(t, key, sub))
		if code != http.StatusServiceUnavailable {
			t.Fatalf("nil reprompter status = %d, want 503 (body %s)", code, body)
		}
		if c, m := apiErr(t, body); c != "internal" || m != "database not configured" {
			t.Fatalf("err = %s/%s", c, m)
		}
	})
}

// TestReviewAuth: every review route is caller-scoped, so the auth
// middleware (401 unauthorized) precedes all handler logic.
func TestReviewAuth(t *testing.T) {
	id := nextTestID()
	routes := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/api/v1/review", ""},
		{http.MethodPost, "/api/v1/review/accept-all", `{"category_id":"` + id + `"}`},
		{http.MethodPost, "/api/v1/review/" + id + "/accept", `{"category_id":"` + id + `"}`},
		{http.MethodPost, "/api/v1/review/" + id + "/reprompt", ""},
	}
	for _, r := range routes {
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			h, _ := newReviewServer(t, &memReviewStore{}, nil)
			// Missing token: 401 from the auth middleware (its default
			// error handler, not the API envelope).
			if code, _ := doReq(t, h, r.method, r.path, r.body, nil); code != http.StatusUnauthorized {
				t.Errorf("no token: status = %d, want 401", code)
			}
			// Garbage token: the same 401.
			garbage := map[string]string{"Authorization": "Bearer not.a.jwt"}
			if code, _ := doReq(t, h, r.method, r.path, r.body, garbage); code != http.StatusUnauthorized {
				t.Errorf("garbage token: status = %d, want 401", code)
			}
		})
	}
}

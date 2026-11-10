// The step 5.4 endpoints against a real postgres:18 pool and the real
// jobs store (the 13th New argument — a concrete *repo.CategoriseJobStore,
// so the in-memory fakes cannot stand in here): POST enqueues a pending
// manual job (202, the row as JSON), a second POST while one is active
// is a 409, GET returns the caller's job (200), and a well-formed id the
// caller does not own — or a malformed id — is a 404 with the same
// message (existence is not disclosed). No token is a 401 (the verifier
// answers before the handler runs); a nil store is a 503.
package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"finance-tracker/internal/authn"
	"finance-tracker/internal/repo"
)

func TestCategoriseEndpoints(t *testing.T) {
	pool, ctx := migratedPool(t)
	defer pool.Close()

	key := mintKey(t)
	b, err := NewBootstrap(NewPGUserStore(pool), testKEK(t), nil)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	for _, sub := range []string{"cat-core", "cat-other"} {
		if _, err := b.EnsureUser(ctx, authn.Identity{Sub: sub, Email: sub + "@localhost", Name: sub}); err != nil {
			t.Fatalf("ensure %s: %v", sub, err)
		}
	}
	jobs := repo.NewCategoriseJobStore(pool)
	h := New(fakeReady{}, BuildInfo{}, liveAuth(t, key), b, nil,
		nil, nil, nil, nil, nil, nil, nil, jobs, nil, nil, nil, nil, nil, nil, false, nil, nil, nil)
	mine := authHdr(mintIdentityToken(t, key, "cat-core", "cat-core@localhost", "cat-core", "cat-core"))
	theirs := authHdr(mintIdentityToken(t, key, "cat-other", "cat-other@localhost", "cat-other", "cat-other"))

	var jobID, userID string

	t.Run("no token", func(t *testing.T) {
		for _, req := range [][2]string{
			{http.MethodPost, "/api/v1/categorise"},
			{http.MethodGet, "/api/v1/categorise/not-a-uuid"},
		} {
			code, _ := doReq(t, h, req[0], req[1], "", nil)
			if code != http.StatusUnauthorized {
				t.Errorf("%s %s without token: status = %d, want 401", req[0], req[1], code)
			}
		}
	})

	t.Run("nil store", func(t *testing.T) {
		hNil := New(fakeReady{}, BuildInfo{}, liveAuth(t, key), b, nil,
			nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, false, nil, nil, nil)
		code, body := doReq(t, hNil, http.MethodPost, "/api/v1/categorise", "", mine)
		if code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 (body %s)", code, body)
		}
	})

	t.Run("post enqueues manual job", func(t *testing.T) {
		code, body := doReq(t, h, http.MethodPost, "/api/v1/categorise", "", mine)
		if code != http.StatusAccepted {
			t.Fatalf("post: status = %d, want 202 (body %s)", code, body)
		}
		var job struct {
			ID          string  `json:"id"`
			UserID      string  `json:"user_id"`
			Source      string  `json:"source"`
			ImportJobID *string `json:"import_job_id"`
			Status      string  `json:"status"`
		}
		if err := json.Unmarshal([]byte(body), &job); err != nil {
			t.Fatalf("decode job: %v (body %s)", err, body)
		}
		if job.ID == "" || job.UserID == "" || job.Source != "manual" || job.ImportJobID != nil || job.Status != "pending" {
			t.Fatalf("job = %+v, want manual/pending with no import_job_id", job)
		}
		jobID, userID = job.ID, job.UserID

		// The same row, read back through the store.
		got, err := jobs.Get(ctx, userID, jobID)
		if err != nil {
			t.Fatalf("store get: %v", err)
		}
		if got.Source != "manual" || got.Status != "pending" || got.UserID != userID {
			t.Fatalf("store row = %+v, want the created manual/pending job", got)
		}
	})

	t.Run("post conflict while active", func(t *testing.T) {
		code, body := doReq(t, h, http.MethodPost, "/api/v1/categorise", "", mine)
		wantAPIError(t, code, body, http.StatusConflict, codeConflict,
			"a categorisation job is already in progress")
	})

	t.Run("get own job", func(t *testing.T) {
		code, body := doReq(t, h, http.MethodGet, "/api/v1/categorise/"+jobID, "", mine)
		if code != http.StatusOK {
			t.Fatalf("get: status = %d, want 200 (body %s)", code, body)
		}
		if id := idFrom(t, body); id != jobID {
			t.Fatalf("id = %s, want %s", id, jobID)
		}
	})

	t.Run("get other user job", func(t *testing.T) {
		code, body := doReq(t, h, http.MethodGet, "/api/v1/categorise/"+jobID, "", theirs)
		wantAPIError(t, code, body, http.StatusNotFound, codeNotFound,
			"categorise job not found")
	})

	t.Run("get malformed id", func(t *testing.T) {
		code, body := doReq(t, h, http.MethodGet, "/api/v1/categorise/not-a-uuid", "", mine)
		wantAPIError(t, code, body, http.StatusNotFound, codeNotFound,
			"categorise job not found")
	})
}

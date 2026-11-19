// The step 4.6 runner: thin gates against the in-memory stores (401 /
// 404 / 503 / 409 + the envelope), a full memory-backed run flow
// (insert, re-import dedup, overlapping file, row-level errors), and
// the CORE TestImportRun against a real Postgres pool (honest dedup:
// the 0002 unique index does the collision work).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"finance-tracker/internal/authn"
	"finance-tracker/internal/repo"

	"github.com/lestrrat-go/jwx/v3/jwk"
)

// newImportRunServer builds a full router with a live token verifier,
// an in-memory bootstrap, and the given import + transaction stores
// (nil allowed — that is the 503 path). The key is returned so tests
// can mint tokens for several users under the same verifier.
func newImportRunServer(t *testing.T, imp ImportStore, txs TransactionStore) (http.Handler, jwk.Key) {
	t.Helper()
	key := mintKey(t)
	b, err := NewBootstrap(newMemStore(), testKEK(t), nil)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return New(fakeReady{}, BuildInfo{}, liveAuth(t, key), b, nil, nil, nil, txs, nil, nil, nil, imp, nil, nil, nil, nil, nil, nil, nil, nil, nil, false, nil, nil, nil, nil, nil, nil, nil), key
}

// setJobStatus overwrites a seeded job's status directly (the only
// app-side writers for the non-mapping states are this endpoint's own,
// so the gate test seeds the states by hand).
func setJobStatus(store *memImportStore, jobID, status string) {
	for i, j := range store.rows {
		if j.ID == jobID {
			store.rows[i].Status = status
			return
		}
	}
}

// StartRun / FinishRun mirror the repo semantics the handler relies
// on: per-user scoping (a foreign job is ErrImportJobNotFound), the
// status vocabulary check on finish, and the re-read of the updated
// row. (The mem struct cannot grow error-injection fields — it lives
// in imports_test.go — so the happy paths are what the runner flow
// exercises.)
func (m *memImportStore) StartRun(_ context.Context, userID, id string, _ []byte) (repo.ImportJob, error) {
	for i, j := range m.rows {
		if j.ID != id || m.accounts[j.AccountID] != userID {
			continue
		}
		m.rows[i].Status = "importing"
		m.rows[i].UpdatedAt = time.Now().UTC()
		return m.rows[i], nil
	}
	return repo.ImportJob{}, repo.ErrImportJobNotFound
}

func (m *memImportStore) FinishRun(_ context.Context, userID, id, status string, result *json.RawMessage, runErr *string, _ []byte) (repo.ImportJob, error) {
	for i, j := range m.rows {
		if j.ID != id || m.accounts[j.AccountID] != userID {
			continue
		}
		ok := false
		for _, s := range repo.ImportJobStatuses {
			if s == status {
				ok = true
				break
			}
		}
		if !ok {
			return repo.ImportJob{}, fmt.Errorf("repo: import job status %q is not a pipeline state", status)
		}
		m.rows[i].Status = status
		m.rows[i].Result = result
		m.rows[i].Error = runErr
		m.rows[i].UpdatedAt = time.Now().UTC()
		return m.rows[i], nil
	}
	return repo.ImportJob{}, repo.ErrImportJobNotFound
}

// Upsert mirrors the repo's dedup-aware insert: a row whose identity
// (account, date, canonical amount, normalized description) already
// exists is returned with inserted=false (the runner counts it
// skipped); anything else is created exactly like Create.
func (m *memTxStore) Upsert(ctx context.Context, userID string, in repo.TransactionIn, dek []byte) (repo.Transaction, bool, error) {
	tx, err := m.Create(ctx, userID, in, dek)
	if err == nil {
		return tx, true, nil
	}
	if !errors.Is(err, repo.ErrDedupConflict) {
		return repo.Transaction{}, false, err
	}
	amount, err := repo.CanonicalAmount(in.Amount)
	if err != nil {
		return repo.Transaction{}, false, err
	}
	hash := memDedupHash(in.AccountID, in.DatePosted, amount, in.Description)
	for _, r := range m.rows {
		if r.userID == userID && r.tx.DedupHash == hash {
			return r.tx, false, nil
		}
	}
	return repo.Transaction{}, false, err
}

// UpsertBatch is the runner-facing batch form. The in-memory store has
// no chunking or multi-row path, so it delegates to the per-row Upsert
// in input order — the same dedup semantics the repo batch has (first
// occurrence wins, colliding rows come back false).
func (m *memTxStore) UpsertBatch(ctx context.Context, userID string, ins []repo.TransactionIn, dek []byte) ([]bool, error) {
	perRow := make([]bool, len(ins))
	for i, in := range ins {
		_, inserted, err := m.Upsert(ctx, userID, in, dek)
		if err != nil {
			return nil, err
		}
		perRow[i] = inserted
	}
	return perRow, nil
}

// runErrRow / runResult decode the job row's result jsonb — the same
// shape the runner marshals (inserted, skipped, per-row errors).
type runErrRow struct {
	Row     int    `json:"row"`
	Field   string `json:"field"`
	Message string `json:"message"`
}

type runResult struct {
	Inserted int         `json:"inserted"`
	Skipped  int         `json:"skipped"`
	Errors   []runErrRow `json:"errors"`
}

// wantRunOutcome posts the run and pins the 200 response: the raw job
// row with status "done", a nil error, and the exact
// inserted/skipped/errors payload.
func wantRunOutcome(t *testing.T, h http.Handler, hdr map[string]string, jobID string, wantInserted, wantSkipped int, wantErrs []runErrRow) {
	t.Helper()
	status, body := doReq(t, h, http.MethodPost, "/api/v1/imports/"+jobID+"/run", "", hdr)
	if status != http.StatusOK {
		t.Fatalf("run %s: status = %d, want 200 (body %s)", jobID, status, body)
	}
	var out struct {
		Status string          `json:"status"`
		Result json.RawMessage `json:"result"`
		Error  *string         `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("run %s: decode row: %v (body %s)", jobID, err, body)
	}
	if out.Status != "done" {
		t.Fatalf("run %s: status = %q, want done (error %v)", jobID, out.Status, out.Error)
	}
	if out.Error != nil {
		t.Fatalf("run %s: error = %q, want nil", jobID, *out.Error)
	}
	var res runResult
	if err := json.Unmarshal(out.Result, &res); err != nil {
		t.Fatalf("run %s: decode result %s: %v", jobID, out.Result, err)
	}
	if res.Inserted != wantInserted || res.Skipped != wantSkipped {
		t.Fatalf("run %s: inserted/skipped = %d/%d, want %d/%d", jobID, res.Inserted, res.Skipped, wantInserted, wantSkipped)
	}
	if len(res.Errors) != len(wantErrs) {
		t.Fatalf("run %s: errors = %+v, want %+v", jobID, res.Errors, wantErrs)
	}
	for i := range wantErrs {
		if res.Errors[i] != wantErrs[i] {
			t.Fatalf("run %s: errors[%d] = %+v, want %+v", jobID, i, res.Errors[i], wantErrs[i])
		}
	}
}

// TestImportRunGates pins the run endpoint's preconditions against the
// in-memory stores: 401 (no or unverified token, status only — the body
// is the authn package's business), 404 (malformed or unknown id), 503
// (either store missing, before auth), and 409 (every non-mapping
// status).
func TestImportRunGates(t *testing.T) {
	const sub = "run-gates"
	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	accountID := nextTestID()
	store := &memImportStore{accounts: map[string]string{accountID: "id-" + sub}}
	jobID := seedJobFile(store, accountID, "stmt.csv", createdAt, plainCSV)
	setJobStatus(store, jobID, "mapping")

	t.Run("missing and garbage tokens are 401", func(t *testing.T) {
		h := New(fakeReady{}, BuildInfo{}, testAuth(), nil, nil, nil, nil, &memTxStore{}, nil, nil, nil, store, nil, nil, nil, nil, nil, nil, nil, nil, nil, false, nil, nil, nil, nil, nil, nil, nil)
		if code, _ := doReq(t, h, http.MethodPost, "/api/v1/imports/"+jobID+"/run", "", nil); code != http.StatusUnauthorized {
			t.Errorf("no token: status = %d, want 401", code)
		}
		if code, _ := doReq(t, h, http.MethodPost, "/api/v1/imports/"+jobID+"/run", "",
			map[string]string{"Authorization": "Bearer not.a.jwt"}); code != http.StatusUnauthorized {
			t.Errorf("garbage token: status = %d, want 401", code)
		}
	})

	h, key := newImportRunServer(t, store, &memTxStore{})
	hdr := authHdr(mintIdentityToken(t, key, sub, sub+"@localhost", sub, sub))

	t.Run("malformed and unknown ids are 404", func(t *testing.T) {
		status, body := doReq(t, h, http.MethodPost, "/api/v1/imports/not-a-uuid/run", "", hdr)
		wantAPIError(t, status, body, http.StatusNotFound, codeNotFound, "import job not found")
		status, body = doReq(t, h, http.MethodPost, "/api/v1/imports/"+nextTestID()+"/run", "", hdr)
		wantAPIError(t, status, body, http.StatusNotFound, codeNotFound, "import job not found")
	})

	// Each 503 server mints its own verification key, so each gets a
	// token under ITS key (the prefix middleware answers 401 for a
	// foreign signature before the handler's 503 could fire).
	t.Run("missing stores are 503", func(t *testing.T) {
		hNoImp, keyNoImp := newImportRunServer(t, nil, &memTxStore{})
		hdrNoImp := authHdr(mintIdentityToken(t, keyNoImp, sub, sub+"@localhost", sub, sub))
		status, body := doReq(t, hNoImp, http.MethodPost, "/api/v1/imports/"+jobID+"/run", "", hdrNoImp)
		wantAPIError(t, status, body, http.StatusServiceUnavailable, codeInternal, "database not configured")
		hNoTxs, keyNoTxs := newImportRunServer(t, store, nil)
		hdrNoTxs := authHdr(mintIdentityToken(t, keyNoTxs, sub, sub+"@localhost", sub, sub))
		status, body = doReq(t, hNoTxs, http.MethodPost, "/api/v1/imports/"+jobID+"/run", "", hdrNoTxs)
		wantAPIError(t, status, body, http.StatusServiceUnavailable, codeInternal, "database not configured")
	})

	for _, st := range []string{"uploaded", "importing", "done", "failed"} {
		t.Run("409 when the status is "+st, func(t *testing.T) {
			id := seedJobFile(store, accountID, "stmt.csv", createdAt, plainCSV)
			setJobStatus(store, id, st)
			status, body := doReq(t, h, http.MethodPost, "/api/v1/imports/"+id+"/run", "", hdr)
			wantAPIError(t, status, body, http.StatusConflict, codeConflict, "import job must be in the mapping state to run")
		})
	}
}

// TestImportRunMemory walks one job through the full memory-backed
// runner: a clean import (3 rows), the terminal re-run guard (409), a
// same-file re-import (all rows skipped by the dedup identity), and a
// file with two bad rows (the errors are reported per row, the good
// row is still inserted, and the run still ends "done").
func TestImportRunMemory(t *testing.T) {
	const sub = "run-mem"
	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	accountID := nextTestID()
	store := &memImportStore{accounts: map[string]string{accountID: "id-" + sub}}
	txs := &memTxStore{}
	txs.markAccount("id-"+sub, accountID)

	h, key := newImportRunServer(t, store, txs)
	hdr := authHdr(mintIdentityToken(t, key, sub, sub+"@localhost", sub, sub))
	mapping := json.RawMessage(`{"kind":"csv","map":{"date_posted":{"index":0},"amount":{"index":2},"description":{"index":1}}}`)

	// seedMapped uploads one job (file + name) and stores the mapping
	// document, leaving the job in the "mapping" state the run wants.
	seedMapped := func(file []byte) string {
		id := seedJobFile(store, accountID, "stmt.csv", createdAt, file)
		if _, err := store.SetMapping(context.Background(), "id-"+sub, id, mapping, nil); err != nil {
			t.Fatalf("set mapping: %v", err)
		}
		return id
	}

	job1 := seedMapped(plainCSV)
	wantRunOutcome(t, h, hdr, job1, 3, 0, nil)

	// A done job is terminal: re-running it is a 409.
	status, body := doReq(t, h, http.MethodPost, "/api/v1/imports/"+job1+"/run", "", hdr)
	wantAPIError(t, status, body, http.StatusConflict, codeConflict, "import job must be in the mapping state to run")

	// The same file in a fresh job: every row already exists.
	job2 := seedMapped(plainCSV)
	wantRunOutcome(t, h, hdr, job2, 0, 3, nil)

	// Two bad rows: the date row and the amount row are reported, the
	// good row is inserted, and the run still finishes "done".
	badCSV := []byte("Date,Details,Amount\n" +
		"2026-02-01,GOOD ROW,-10.00\n" +
		"NOT A DATE,BAD DATE ROW,-20.00\n" +
		"2026-02-02,BAD AMOUNT ROW,abc\n")
	job3 := seedMapped(badCSV)
	wantRunOutcome(t, h, hdr, job3, 1, 0, []runErrRow{
		{Row: 1, Field: "date_posted", Message: `not a valid date: "NOT A DATE"`},
		{Row: 2, Field: "amount", Message: `amount "abc" is not a valid decimal`},
	})
	if len(txs.rows) != 4 { // 3 (job1) + 0 (job2) + 1 (job3)
		t.Fatalf("tx rows = %d, want 4", len(txs.rows))
	}
}

// TestImportRun is the CORE matrix against a real postgres:18 pool and
// the real stores: an honest dedup (the 0002 unique index does the
// collision work), the terminal re-run guard, an overlapping file,
// row-level errors, and a failed run (empty OFX -> status "failed", the
// raw row carries the error text, re-run guard again).
func TestImportRun(t *testing.T) {
	pool, ctx := migratedPool(t)
	defer pool.Close()

	const sub = "run-core"
	key := mintKey(t)
	b, err := NewBootstrap(NewPGUserStore(pool), testKEK(t), nil)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := b.EnsureUser(ctx, authn.Identity{Sub: sub, Email: sub + "@localhost", Name: sub}); err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	// The real jobs store is wired in so the run's plan-5.4 chain
	// (inserts -> pending categorise job) is exercised for real.
	h := New(fakeReady{}, BuildInfo{}, liveAuth(t, key), b, nil,
		repo.NewAccountStore(pool), nil, repo.NewTransactionStore(pool), nil, nil, nil, repo.NewImportJobStore(pool), repo.NewCategoriseJobStore(pool), nil, nil, nil, nil, nil, nil, nil, nil, false, nil, nil, nil, nil, nil, nil, nil)
	hdr := authHdr(mintIdentityToken(t, key, sub, sub+"@localhost", sub, sub))

	// The 5.4 import chain: a run that inserts transactions enqueues
	// one pending categorise job (source "import", linked to the import
	// job); an all-deduped or failed run enqueues nothing.
	var userID string
	if err := pool.QueryRow(ctx, "SELECT id FROM users WHERE auth0_sub = $1", sub).Scan(&userID); err != nil {
		t.Fatalf("user id: %v", err)
	}
	type cjRow struct{ source, importID, status string }
	jobRows := func() []cjRow {
		t.Helper()
		rows, err := pool.Query(ctx,
			"SELECT source, import_job_id, status FROM categorise_jobs WHERE user_id = $1 ORDER BY ctid", userID)
		if err != nil {
			t.Fatalf("query categorise jobs: %v", err)
		}
		defer rows.Close()
		var out []cjRow
		for rows.Next() {
			var r cjRow
			if err := rows.Scan(&r.source, &r.importID, &r.status); err != nil {
				t.Fatalf("scan categorise job: %v", err)
			}
			out = append(out, r)
		}
		return out
	}

	// The runner's Upsert probes the account's existence, so the
	// account must be created through the API first.
	code, body := doReq(t, h, http.MethodPost, "/api/v1/accounts",
		`{"name":"Checking","type":"bank","currency":"USD"}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("create account: status = %d, want 201 (body %s)", code, body)
	}
	acctID := idFrom(t, body)

	// newJob uploads the file; setMapping stores the raw mapping
	// document, which moves the job to "mapping".
	newJob := func(fileName string, file []byte) string {
		code, body := doMultipart(t, h, hdr, map[string]string{"account_id": acctID}, fileName, file)
		if code != http.StatusCreated {
			t.Fatalf("upload %s: status = %d, want 201 (body %s)", fileName, code, body)
		}
		return idFrom(t, body)
	}
	setMapping := func(jobID, doc string) {
		code, body := doReq(t, h, http.MethodPost, "/api/v1/imports/"+jobID+"/mapping", doc, hdr)
		if code != http.StatusOK {
			t.Fatalf("set mapping %s: status = %d, want 200 (body %s)", jobID, code, body)
		}
	}
	csvMapping := `{"kind":"csv","map":{"date_posted":{"index":0},"amount":{"index":2},"description":{"index":1}}}`

	t.Run("re-import same file", func(t *testing.T) {
		job1 := newJob("plain.csv", plainCSV)
		setMapping(job1, csvMapping)
		wantRunOutcome(t, h, hdr, job1, 3, 0, nil)

		// A done job is terminal.
		code, body := doReq(t, h, http.MethodPost, "/api/v1/imports/"+job1+"/run", "", hdr)
		wantAPIError(t, code, body, http.StatusConflict, codeConflict, "import job must be in the mapping state to run")

		// A fresh job, same file: every row collides on the 0002
		// unique index.
		job2 := newJob("plain-again.csv", plainCSV)
		setMapping(job2, csvMapping)
		wantRunOutcome(t, h, hdr, job2, 0, 3, nil)

		// job1 inserted 3 rows -> exactly one pending import job,
		// linked to job1; job2 inserted 0 -> it added no job.
		r := jobRows()
		if len(r) != 1 {
			t.Fatalf("categorise jobs after job1+job2 = %d, want 1: %+v", len(r), r)
		}
		if r[0].source != "import" || r[0].importID != job1 || r[0].status != "pending" {
			t.Fatalf("job row = %+v, want {import %s pending}", r[0], job1)
		}
	})

	t.Run("overlapping file", func(t *testing.T) {
		over := []byte("Date,Details,Amount\n" +
			"2026-01-05,COFFEE ROASTERY,-55.55\n" +
			"2026-01-12,GROCERY STORE,-120.30\n" +
			"2026-01-25,NEW ROW,-30.00\n")
		job := newJob("overlap.csv", over)
		setMapping(job, csvMapping)
		wantRunOutcome(t, h, hdr, job, 1, 2, nil)

		r := jobRows()
		if len(r) != 2 || r[1].importID != job || r[1].status != "pending" {
			t.Fatalf("jobs after overlap run = %+v, want one more pending job for %s", r, job)
		}
	})

	t.Run("row-level errors", func(t *testing.T) {
		badCSV := []byte("Date,Details,Amount\n" +
			"2026-02-01,GOOD ROW,-10.00\n" +
			"NOT A DATE,BAD DATE ROW,-20.00\n" +
			"2026-02-02,BAD AMOUNT ROW,abc\n")
		job := newJob("bad.csv", badCSV)
		setMapping(job, csvMapping)
		wantRunOutcome(t, h, hdr, job, 1, 0, []runErrRow{
			{Row: 1, Field: "date_posted", Message: `not a valid date: "NOT A DATE"`},
			{Row: 2, Field: "amount", Message: `amount "abc" is not a valid decimal`},
		})

		// The single good row was inserted -> one more job.
		if r := jobRows(); len(r) != 3 {
			t.Fatalf("jobs after bad.csv run = %d, want 3: %+v", len(r), r)
		}
	})

	t.Run("failed run", func(t *testing.T) {
		job := newJob("empty.ofx", emptyOFX)
		setMapping(job, `{"kind":"ofx","map":{}}`)
		code, body := doReq(t, h, http.MethodPost, "/api/v1/imports/"+job+"/run", "", hdr)
		if code != http.StatusOK {
			t.Fatalf("run: status = %d, want 200 (body %s)", code, body)
		}
		var out struct {
			Status string          `json:"status"`
			Result json.RawMessage `json:"result"`
			Error  *string         `json:"error"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode row: %v (body %s)", err, body)
		}
		if out.Status != "failed" {
			t.Fatalf("status = %q, want failed (error %v)", out.Status, out.Error)
		}
		if out.Error == nil || *out.Error != "no transactions found in file" {
			t.Fatalf("error = %v, want the empty-OFX message", out.Error)
		}
		if len(out.Result) != 0 && string(out.Result) != "null" {
			t.Fatalf("result = %s, want null", out.Result)
		}

		// A failed job is terminal too.
		code, body = doReq(t, h, http.MethodPost, "/api/v1/imports/"+job+"/run", "", hdr)
		wantAPIError(t, code, body, http.StatusConflict, codeConflict, "import job must be in the mapping state to run")

		// A failed run inserted nothing -> it must not enqueue a
		// categorise job.
		if r := jobRows(); len(r) != 3 {
			t.Fatalf("jobs after failed run = %d, want 3: %+v", len(r), r)
		}
	})
}

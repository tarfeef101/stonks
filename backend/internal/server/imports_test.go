// Route tests for the import job endpoints (plan steps 4.1 + 4.5):
// POST /api/v1/imports (multipart upload: file + account_id, 10 MB cap),
// GET /api/v1/imports (limit/offset query params, newest first),
// GET /api/v1/imports/{id}, plus the 4.5 mapping endpoints on the
// seeded jobs (analyze, preview, set-mapping, the preset list in
// importmap_test.go). All in-memory: memImportStore mirrors the
// repo.ImportJobStore semantics the handlers depend on (filename
// validation, the account must belong to the caller, limit 50/100 clamp,
// offset >= 0, created_at DESC then id DESC order, user-id scoping,
// SetMapping/ReadFile scope by the account's owner) and injects
// per-operation errors for the mapping table. Sealing under the DEK
// stays with the repo testcontainer tests (2.5a convention); the shared
// helpers (authHdr, apiErr, idFrom, nextTestID, doReq, listIDs,
// sameIDs) live in accounts_test.go, settings_test.go and
// notifications_test.go.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"finance-tracker/internal/repo"

	"github.com/lestrrat-go/jwx/v3/jwk"
)

// memImportStore mirrors the repo.ImportJobStore semantics the handlers
// depend on: ValidateFileName before anything else, a missing or foreign
// account is ErrAccountNotFound (existence is not disclosed), a limit
// <= 0 becomes 50 and a limit > 100 becomes 100 (the repo's unexported
// clamp consts), a negative offset becomes 0, rows come back created_at
// DESC then id DESC, and every operation is scoped by user id. A job
// "belongs to" the user who owns its account: Create only ever succeeds
// for the caller's own accounts, so the account's owner is the scoping
// key (repo.ImportJob carries no user id). The owner is the bootstrap
// row id ("id-" + sub, what requireUser passes to the store), not the
// raw sub.
type memImportStore struct {
	// accounts maps account id -> owner user id.
	accounts map[string]string
	rows     []repo.ImportJob
	// files maps job id -> uploaded file bytes (step 4.5: the mapping
	// and preview endpoints read the file back through the store).
	files map[string][]byte

	createErr     error
	getErr        error
	listErr       error
	setMappingErr error
	readFileErr   error
}

func (m *memImportStore) Create(_ context.Context, userID string, in repo.ImportJobIn, _ []byte) (repo.ImportJob, error) {
	if m.createErr != nil {
		return repo.ImportJob{}, m.createErr
	}
	if err := repo.ValidateFileName(in.FileName); err != nil {
		return repo.ImportJob{}, err
	}
	if m.accounts[in.AccountID] != userID {
		return repo.ImportJob{}, repo.ErrAccountNotFound
	}
	now := time.Now().UTC()
	j := repo.ImportJob{
		ID:        nextTestID(),
		AccountID: in.AccountID,
		FileName:  in.FileName,
		Status:    "uploaded",
		CreatedAt: now,
		UpdatedAt: now,
	}
	m.rows = append(m.rows, j)
	return j, nil
}

func (m *memImportStore) Get(_ context.Context, userID, id string, _ []byte) (repo.ImportJob, error) {
	if m.getErr != nil {
		return repo.ImportJob{}, m.getErr
	}
	for _, j := range m.rows {
		if j.ID == id && m.accounts[j.AccountID] == userID {
			return j, nil
		}
	}
	return repo.ImportJob{}, repo.ErrImportJobNotFound
}

func (m *memImportStore) List(_ context.Context, userID string, f repo.ImportJobFilter, _ []byte) ([]repo.ImportJob, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 50 // repo.defaultImportJobLimit (unexported)
	}
	if limit > 100 {
		limit = 100 // repo.maxImportJobLimit (unexported)
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	mine := make([]repo.ImportJob, 0)
	for _, j := range m.rows {
		if m.accounts[j.AccountID] == userID {
			mine = append(mine, j)
		}
	}
	sort.SliceStable(mine, func(i, k int) bool {
		if !mine[i].CreatedAt.Equal(mine[k].CreatedAt) {
			return mine[i].CreatedAt.After(mine[k].CreatedAt)
		}
		return mine[i].ID > mine[k].ID // nextTestID ids are zero-padded: lex = numeric
	})
	if f.Offset > len(mine) {
		f.Offset = len(mine)
	}
	end := f.Offset + limit
	if end > len(mine) {
		end = len(mine)
	}
	return mine[f.Offset:end], nil
}

func (m *memImportStore) SetMapping(_ context.Context, userID, id string, mapping json.RawMessage, _ []byte) (repo.ImportJob, error) {
	if m.setMappingErr != nil {
		return repo.ImportJob{}, m.setMappingErr
	}
	for i, j := range m.rows {
		if j.ID != id || m.accounts[j.AccountID] != userID {
			continue
		}
		m.rows[i].Mapping = mapping
		m.rows[i].Status = "mapping"
		m.rows[i].UpdatedAt = time.Now().UTC()
		return m.rows[i], nil
	}
	return repo.ImportJob{}, repo.ErrImportJobNotFound
}

func (m *memImportStore) ReadFile(_ context.Context, userID, id string, _ []byte) ([]byte, error) {
	if m.readFileErr != nil {
		return nil, m.readFileErr
	}
	for _, j := range m.rows {
		if j.ID != id || m.accounts[j.AccountID] != userID {
			continue
		}
		f, ok := m.files[id]
		if !ok {
			return nil, repo.ErrImportJobNotFound
		}
		return f, nil
	}
	return nil, repo.ErrImportJobNotFound
}

// seedJob appends a row directly to the store (the app-side writers do
// not exist until step 4.3+) with a pinned created_at and returns its
// id. The row's account must already be in the store's accounts map;
// the account's owner is the scoping key.
func seedJob(store *memImportStore, accountID, fileName string, createdAt time.Time) string {
	now := time.Now().UTC()
	j := repo.ImportJob{
		ID:        nextTestID(),
		AccountID: accountID,
		FileName:  fileName,
		Status:    "uploaded",
		CreatedAt: createdAt,
		UpdatedAt: now,
	}
	store.rows = append(store.rows, j)
	return j.ID
}

// seedJobFile is seedJob plus the uploaded file bytes (the 4.5
// analyze/preview endpoints read the file back through the store).
func seedJobFile(store *memImportStore, accountID, fileName string, createdAt time.Time, file []byte) string {
	id := seedJob(store, accountID, fileName, createdAt)
	if store.files == nil {
		store.files = map[string][]byte{}
	}
	store.files[id] = file
	return id
}

// newImportsServer builds a full router with a live token verifier, an
// in-memory bootstrap, and the given import store (nil allowed — the
// 503 path). The key is returned so tests can mint tokens for several
// users under the same verifier.
func newImportsServer(t *testing.T, store ImportStore) (http.Handler, jwk.Key) {
	t.Helper()
	key := mintKey(t)
	b, err := NewBootstrap(newMemStore(), testKEK(t))
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return New(fakeReady{}, BuildInfo{}, liveAuth(t, key), b, nil, nil, nil, nil, nil, nil, nil, store, nil, nil, nil, nil), key
}

// doMultipart posts a multipart/form-data body to POST /api/v1/imports:
// the named fields plus an optional "file" part (omitted when
// fileBytes is nil, which is the "missing file part" case). It returns
// the status and raw body; doReq cannot be reused because it
// hardcodes the JSON Content-Type.
func doMultipart(t *testing.T, h http.Handler, hdr map[string]string, fields map[string]string, fileName string, fileBytes []byte) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatalf("write field %s: %v", k, err)
		}
	}
	if fileBytes != nil {
		fw, err := mw.CreateFormFile("file", fileName)
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		if _, err := fw.Write(fileBytes); err != nil {
			t.Fatalf("write file part: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/imports", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	out, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(out)
}

// TestImportsUpload covers the happy path: a multipart upload answers
// 201 with the created row (status "uploaded", the file name echoed
// back), the wire shape (result/error present as null, no timestamps),
// and the store row the handler created.
func TestImportsUpload(t *testing.T) {
	accountID := nextTestID()
	store := &memImportStore{accounts: map[string]string{accountID: "id-up-a"}}
	h, key := newImportsServer(t, store)
	hdr := authHdr(mintIdentityToken(t, key, "up-a", "a@localhost", "A", "a"))

	code, body := doMultipart(t, h, hdr, map[string]string{"account_id": accountID}, "statement.csv", []byte("date,amount\n"))
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", code, body)
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(body), &row); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, body)
	}
	if row["id"] != store.rows[0].ID {
		t.Errorf("id = %v, want %s (the store's row)", row["id"], store.rows[0].ID)
	}
	if row["account_id"] != accountID {
		t.Errorf("account_id = %v, want %s", row["account_id"], accountID)
	}
	if row["file_name"] != "statement.csv" {
		t.Errorf("file_name = %v, want statement.csv", row["file_name"])
	}
	if row["status"] != "uploaded" {
		t.Errorf("status = %v, want uploaded", row["status"])
	}
	if _, ok := row["result"]; !ok {
		t.Error("result key missing (must be present as null)")
	} else if row["result"] != nil {
		t.Errorf("result = %v, want null", row["result"])
	}
	if _, ok := row["error"]; !ok {
		t.Error("error key missing (must be present as null)")
	} else if row["error"] != nil {
		t.Errorf("error = %v, want null", row["error"])
	}
	if _, ok := row["created_at"]; ok {
		t.Error("created_at must not be on the wire")
	}
	if _, ok := row["updated_at"]; ok {
		t.Error("updated_at must not be on the wire")
	}
	if len(store.rows) != 1 {
		t.Fatalf("store rows = %d, want 1", len(store.rows))
	}
	if store.rows[0].FileName != "statement.csv" || store.rows[0].Status != "uploaded" || store.rows[0].AccountID != accountID {
		t.Errorf("store row = %+v, want uploaded statement.csv in %s", store.rows[0], accountID)
	}
}

// TestImportsUploadErrors covers the 400 table (missing file part,
// non-multipart body, empty/over-long filename, missing or malformed
// account id) and the 413 cap. The shared store's createErr would make
// any store call a 500, so every 400/413 proves the store was never
// reached.
func TestImportsUploadErrors(t *testing.T) {
	accountID := nextTestID()
	store := &memImportStore{
		accounts:  map[string]string{accountID: "id-up-e"},
		createErr: errors.New("boom"),
	}
	h, key := newImportsServer(t, store)
	hdr := authHdr(mintIdentityToken(t, key, "up-e", "e@localhost", "E", "e"))

	cases := []struct {
		name     string
		fields   map[string]string
		fileName string
		noFile   bool
		message  string
	}{
		{name: "missing file part", fields: map[string]string{"account_id": accountID}, noFile: true, message: "file is required"},
		{name: "empty file name", fields: map[string]string{"account_id": accountID}, message: "file name must not be empty"},
		{name: "file name over 255 characters", fields: map[string]string{"account_id": accountID}, fileName: strings.Repeat("a", 256), message: "file name must be at most 255 characters"},
		{name: "missing account id", fields: nil, fileName: "statement.csv", message: "account_id is required"},
		{name: "malformed account id", fields: map[string]string{"account_id": "nope"}, fileName: "statement.csv", message: "account_id must be a valid UUID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var fileBytes []byte
			if !tc.noFile {
				fileBytes = []byte("date,amount\n")
			}
			code, body := doMultipart(t, h, hdr, tc.fields, tc.fileName, fileBytes)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", code, body)
			}
			if c, m := apiErr(t, body); c != codeInvalidRequest || m != tc.message {
				t.Errorf("envelope = %s/%q, want invalid_request/%q", c, m, tc.message)
			}
		})
	}

	// A non-multipart body fails the same first check as a missing part.
	code, body := doReq(t, h, http.MethodPost, "/api/v1/imports", `{"file":"x"}`, hdr)
	if code != http.StatusBadRequest {
		t.Errorf("non-multipart: status = %d, want 400 (body %s)", code, body)
	} else if c, m := apiErr(t, body); c != codeInvalidRequest || m != "file is required" {
		t.Errorf("non-multipart: envelope = %s/%q, want invalid_request/file is required", c, m)
	}

	t.Run("over 10 MB is 413", func(t *testing.T) {
		code, body := doMultipart(t, h, hdr, map[string]string{"account_id": accountID}, "big.csv", bytes.Repeat([]byte("A"), maxUploadBytes+1))
		if code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413 (body %s)", code, body)
		}
		if c, m := apiErr(t, body); c != codePayloadTooLarge || m != "file must be at most 10 MB" {
			t.Errorf("envelope = %s/%q, want payload_too_large/file must be at most 10 MB", c, m)
		}
	})

	if len(store.rows) != 0 {
		t.Errorf("store saw %d rows, want 0 (400s and 413 must not reach the store)", len(store.rows))
	}
}

// TestImportsList covers the order (created_at DESC, id DESC tie
// break), the limit/offset page math, the wire shape (no timestamps;
// result/error present as null), the exclusion of other users' rows,
// and the non-nil empty page past the end.
func TestImportsList(t *testing.T) {
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	accountA := nextTestID()
	accountB := nextTestID()
	store := &memImportStore{accounts: map[string]string{accountA: "id-lst-a", accountB: "id-lst-b"}}
	// i4 and i5 share a created_at (the tie the id DESC break must
	// resolve, later-minted first).
	i1 := seedJob(store, accountA, "a.csv", base)
	i2 := seedJob(store, accountA, "b.csv", base.Add(time.Minute))
	i4 := seedJob(store, accountA, "d.csv", base.Add(2*time.Minute))
	i5 := seedJob(store, accountA, "e.csv", base.Add(2*time.Minute))
	i3 := seedJob(store, accountA, "c.csv", base.Add(3*time.Minute))
	_ = seedJob(store, accountB, "foreign.csv", base.Add(4*time.Minute))
	h, key := newImportsServer(t, store)
	hdr := authHdr(mintIdentityToken(t, key, "lst-a", "a@localhost", "A", "a"))

	code, out := doReq(t, h, http.MethodGet, "/api/v1/imports", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", code, out)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if !sameIDs(listIDs(t, out), i3, i5, i4, i2, i1) {
		t.Fatalf("order = %v, want [%s, %s, %s, %s, %s] (created_at DESC, id DESC tie break; B's row excluded)",
			listIDs(t, out), i3, i5, i4, i2, i1)
	}
	first := rows[0]
	if first["account_id"] != accountA || first["file_name"] != "c.csv" || first["status"] != "uploaded" {
		t.Errorf("first row = %v, want account %s c.csv uploaded", first, accountA)
	}
	if _, ok := first["created_at"]; ok {
		t.Error("created_at must not be on the wire")
	}
	if _, ok := first["updated_at"]; ok {
		t.Error("updated_at must not be on the wire")
	}
	if _, ok := first["result"]; !ok || first["result"] != nil {
		t.Errorf("result = %v, want null (present)", first["result"])
	}
	if _, ok := first["error"]; !ok || first["error"] != nil {
		t.Errorf("error = %v, want null (present)", first["error"])
	}

	code, out = doReq(t, h, http.MethodGet, "/api/v1/imports?limit=2&offset=1", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("limit/offset: status = %d, want 200 (body %s)", code, out)
	}
	if !sameIDs(listIDs(t, out), i5, i4) {
		t.Errorf("limit=2&offset=1 = %v, want [%s, %s]", listIDs(t, out), i5, i4)
	}

	code, out = doReq(t, h, http.MethodGet, "/api/v1/imports?offset=100", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("offset past end: status = %d, want 200 (body %s)", code, out)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("offset past end = %q, want [] (non-nil)", out)
	}
}

// TestImportsBadParams covers the list query-param validation (limit and
// offset must be integers >= 0, empty-string params are absent) and
// proves a valid request reaches the store (injected error -> 500).
func TestImportsBadParams(t *testing.T) {
	store := &memImportStore{accounts: map[string]string{}, listErr: errors.New("boom")}
	h, key := newImportsServer(t, store)
	hdr := authHdr(mintIdentityToken(t, key, "lp-a", "a@localhost", "A", "a"))

	cases := []struct {
		query   string
		message string
	}{
		{"?limit=abc", "limit must be an integer >= 0"},
		{"?limit=-1", "limit must be an integer >= 0"},
		{"?offset=abc", "offset must be an integer >= 0"},
		{"?offset=-1", "offset must be an integer >= 0"},
	}
	for _, tc := range cases {
		code, body := doReq(t, h, http.MethodGet, "/api/v1/imports"+tc.query, "", hdr)
		if code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (body %s)", tc.query, code, body)
			continue
		}
		if c, m := apiErr(t, body); c != codeInvalidRequest || m != tc.message {
			t.Errorf("%s: envelope = %s/%q, want invalid_request/%q", tc.query, c, m, tc.message)
		}
	}

	// No params (and empty-string params, which are absent) reach the
	// store: the injected error is a 500, not a 400.
	for _, query := range []string{"", "?limit=&offset="} {
		code, body := doReq(t, h, http.MethodGet, "/api/v1/imports"+query, "", hdr)
		if code != http.StatusInternalServerError {
			t.Errorf("%q: status = %d, want 500 (body %s)", query, code, body)
			continue
		}
		if c, m := apiErr(t, body); c != codeInternal || m != "import operation failed" {
			t.Errorf("%q: envelope = %s/%q, want internal/import operation failed", query, c, m)
		}
	}
}

// TestImportsGet covers the single-row happy path: the decrypted file
// name is echoed, the account id is carried, and the wire shape has no
// timestamps with result/error present as null.
func TestImportsGet(t *testing.T) {
	accountID := nextTestID()
	store := &memImportStore{accounts: map[string]string{accountID: "id-get-a"}}
	base := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	j1 := seedJob(store, accountID, "statement.csv", base)
	_ = seedJob(store, accountID, "other.csv", base.Add(time.Minute))
	h, key := newImportsServer(t, store)
	hdr := authHdr(mintIdentityToken(t, key, "get-a", "a@localhost", "A", "a"))

	code, body := doReq(t, h, http.MethodGet, "/api/v1/imports/"+j1, "", hdr)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", code, body)
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(body), &row); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, body)
	}
	if row["id"] != j1 || row["account_id"] != accountID || row["file_name"] != "statement.csv" || row["status"] != "uploaded" {
		t.Errorf("row = %v, want %s/%s/statement.csv/uploaded", row, j1, accountID)
	}
	if _, ok := row["created_at"]; ok {
		t.Error("created_at must not be on the wire")
	}
	if _, ok := row["updated_at"]; ok {
		t.Error("updated_at must not be on the wire")
	}
	if _, ok := row["result"]; !ok || row["result"] != nil {
		t.Errorf("result = %v, want null (present)", row["result"])
	}
	if _, ok := row["error"]; !ok || row["error"] != nil {
		t.Errorf("error = %v, want null (present)", row["error"])
	}
}

// TestImportsErrors covers the handler's error mapping: a malformed path
// id answers 404 before the store (proven by the injected getErr), a
// well-formed id the caller does not own is 404 (existence is not
// disclosed) while the owner sees it, a POST to a foreign account is 404
// "account not found", and a generic store failure is 500 "import
// operation failed" on every route.
func TestImportsErrors(t *testing.T) {
	accountA := nextTestID()
	accountB := nextTestID()
	store := &memImportStore{accounts: map[string]string{accountA: "id-err-a", accountB: "id-err-b"}}
	base := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	bRow := seedJob(store, accountB, "b.csv", base)
	h, key := newImportsServer(t, store)
	tokA := mintIdentityToken(t, key, "err-a", "a@localhost", "A", "a")
	tokB := mintIdentityToken(t, key, "err-b", "b@localhost", "B", "b")

	// Malformed path id: 404 before the store; the injected getErr
	// would be a 500 if the store were ever reached.
	store.getErr = errors.New("boom")
	code, body := doReq(t, h, http.MethodGet, "/api/v1/imports/nope", "", authHdr(tokA))
	if code != http.StatusNotFound {
		t.Fatalf("malformed id: status = %d, want 404 (body %s)", code, body)
	}
	if c, m := apiErr(t, body); c != codeNotFound || m != "import job not found" {
		t.Errorf("malformed id: envelope = %s/%q, want not_found/import job not found", c, m)
	}
	store.getErr = nil

	// Well-formed id the caller does not own: 404, not 200/500.
	code, body = doReq(t, h, http.MethodGet, "/api/v1/imports/"+bRow, "", authHdr(tokA))
	if code != http.StatusNotFound {
		t.Errorf("foreign row: status = %d, want 404 (body %s)", code, body)
	} else if c, m := apiErr(t, body); c != codeNotFound || m != "import job not found" {
		t.Errorf("foreign row: envelope = %s/%q, want not_found/import job not found", c, m)
	}
	// The owner of the row sees it.
	code, body = doReq(t, h, http.MethodGet, "/api/v1/imports/"+bRow, "", authHdr(tokB))
	if code != http.StatusOK {
		t.Errorf("owner GET: status = %d, want 200 (body %s)", code, body)
	}

	// POST to a foreign account: 404 "account not found".
	code, body = doMultipart(t, h, authHdr(tokA), map[string]string{"account_id": accountB}, "a.csv", []byte("x"))
	if code != http.StatusNotFound {
		t.Errorf("foreign account: status = %d, want 404 (body %s)", code, body)
	} else if c, m := apiErr(t, body); c != codeNotFound || m != "account not found" {
		t.Errorf("foreign account: envelope = %s/%q, want not_found/account not found", c, m)
	}

	// Store failures: 500 "import operation failed" on every route.
	store.createErr = errors.New("boom")
	code, body = doMultipart(t, h, authHdr(tokA), map[string]string{"account_id": accountA}, "a.csv", []byte("x"))
	if code != http.StatusInternalServerError {
		t.Errorf("create boom: status = %d, want 500 (body %s)", code, body)
	} else if c, m := apiErr(t, body); c != codeInternal || m != "import operation failed" {
		t.Errorf("create boom: envelope = %s/%q, want internal/import operation failed", c, m)
	}
	store.createErr = nil
	store.listErr = errors.New("boom")
	code, body = doReq(t, h, http.MethodGet, "/api/v1/imports", "", authHdr(tokA))
	if code != http.StatusInternalServerError {
		t.Errorf("list boom: status = %d, want 500 (body %s)", code, body)
	} else if c, m := apiErr(t, body); c != codeInternal || m != "import operation failed" {
		t.Errorf("list boom: envelope = %s/%q, want internal/import operation failed", c, m)
	}
	store.listErr = nil
	store.getErr = errors.New("boom")
	code, body = doReq(t, h, http.MethodGet, "/api/v1/imports/"+bRow, "", authHdr(tokB))
	if code != http.StatusInternalServerError {
		t.Errorf("get boom: status = %d, want 500 (body %s)", code, body)
	} else if c, m := apiErr(t, body); c != codeInternal || m != "import operation failed" {
		t.Errorf("get boom: envelope = %s/%q, want internal/import operation failed", c, m)
	}
}

// TestImportsAuth covers the auth and 503 edges: missing/garbage tokens
// are 401 on every route (status only — the body is the authn package's
// business), and a valid token against a nil store is a 503 on every
// route.
func TestImportsAuth(t *testing.T) {
	t.Run("missing and garbage tokens are 401", func(t *testing.T) {
		// testAuth verifies against a nil key set: every token fails.
		h := New(fakeReady{}, BuildInfo{}, testAuth(), nil, nil, nil, nil, nil, nil, nil, nil, &memImportStore{}, nil, nil, nil, nil)
		if code, _ := doReq(t, h, http.MethodGet, "/api/v1/imports", "", nil); code != http.StatusUnauthorized {
			t.Errorf("no token (list): status = %d, want 401", code)
		}
		if code, _ := doReq(t, h, http.MethodPost, "/api/v1/imports", `{"file":"x"}`,
			map[string]string{"Authorization": "Bearer not.a.jwt"}); code != http.StatusUnauthorized {
			t.Errorf("garbage token (post): status = %d, want 401", code)
		}
		if code, _ := doReq(t, h, http.MethodGet, "/api/v1/imports/"+nextTestID(), "", nil); code != http.StatusUnauthorized {
			t.Errorf("no token (get): status = %d, want 401", code)
		}
	})

	t.Run("nil store is 503 with a valid token", func(t *testing.T) {
		h, key := newImportsServer(t, nil)
		tok := mintIdentityToken(t, key, "imp-nc", "nc@localhost", "N", "nc")
		hdr := authHdr(tok)
		code, out := doMultipart(t, h, hdr, map[string]string{"account_id": nextTestID()}, "a.csv", []byte("x"))
		if code != http.StatusServiceUnavailable {
			t.Errorf("POST nil store: status = %d, want 503 (body %s)", code, out)
		} else if c, m := apiErr(t, out); c != codeInternal || m != "database not configured" {
			t.Errorf("POST nil store: envelope = %s/%q, want internal/database not configured", c, m)
		}
		for _, path := range []string{"/api/v1/imports", "/api/v1/imports/" + nextTestID()} {
			code, out := doReq(t, h, http.MethodGet, path, "", hdr)
			if code != http.StatusServiceUnavailable {
				t.Errorf("GET %s nil store: status = %d, want 503 (body %s)", path, code, out)
				continue
			}
			if c, m := apiErr(t, out); c != codeInternal || m != "database not configured" {
				t.Errorf("GET %s nil store: envelope = %s/%q, want internal/database not configured", path, c, m)
			}
		}
	})
}

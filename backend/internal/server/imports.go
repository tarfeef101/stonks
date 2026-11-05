// Import job endpoints (plan step 4.1): POST /api/v1/imports (multipart
// upload: the statement file plus the target account, 10 MB cap),
// GET /api/v1/imports (page, newest first) and GET /api/v1/imports/{id}
// (single job — the file name is decrypted server-side for the owner).
// Every store call is scoped to the caller's users.id.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"

	"finance-tracker/internal/repo"

	"github.com/go-chi/chi/v5"
)

// maxUploadBytes caps the statement file (plan 4.1: 10 MB).
const maxUploadBytes = 10 << 20

// ImportStore is the persistence surface the import handlers need.
// *repo.ImportJobStore satisfies it; route tests use an in-memory
// implementation.
type ImportStore interface {
	Create(ctx context.Context, userID string, in repo.ImportJobIn, dek []byte) (repo.ImportJob, error)
	Get(ctx context.Context, userID, id string, dek []byte) (repo.ImportJob, error)
	List(ctx context.Context, userID string, f repo.ImportJobFilter, dek []byte) ([]repo.ImportJob, error)
	SetMapping(ctx context.Context, userID, id string, mapping json.RawMessage, dek []byte) (repo.ImportJob, error)
	ReadFile(ctx context.Context, userID, id string, dek []byte) ([]byte, error)
	StartRun(ctx context.Context, userID, id string, dek []byte) (repo.ImportJob, error)
	FinishRun(ctx context.Context, userID, id string, status string, result *json.RawMessage, runErr *string, dek []byte) (repo.ImportJob, error)
}

// importOut is the wire row. FileName is the decrypted original
// filename. Result is the raw jsonb outcome (null while unset), Mapping
// the confirmed field mapping (null while unset, step 4.5), and Error
// the failure message (null when unset). Timestamps are
// bookkeeping and excluded (3.1 convention); the list order
// (created_at DESC) carries the freshness signal.
type importOut struct {
	ID        string           `json:"id"`
	AccountID string           `json:"account_id"`
	FileName  string           `json:"file_name"`
	Status    string           `json:"status"`
	Result    *json.RawMessage `json:"result"`
	Mapping   json.RawMessage  `json:"mapping"`
	Error     *string          `json:"error"`
}

func toImportOut(j repo.ImportJob) importOut {
	return importOut{
		ID:        j.ID,
		AccountID: j.AccountID,
		FileName:  j.FileName,
		Status:    j.Status,
		Result:    j.Result,
		Mapping:   j.Mapping,
		Error:     j.Error,
	}
}

// importFilterFromQuery parses the list query params (limit/offset
// integers >= 0, the notification convention; the repo applies the
// default/cap). Empty-string params are treated as absent.
func importFilterFromQuery(q url.Values) (repo.ImportJobFilter, error) {
	var f repo.ImportJobFilter
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return f, errors.New("limit must be an integer >= 0")
		}
		f.Limit = n
	}
	if s := q.Get("offset"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return f, errors.New("offset must be an integer >= 0")
		}
		f.Offset = n
	}
	return f, nil
}

// importError maps store errors to responses. The sentinel mapping is
// what the frontend keys off; everything else is a 500 with the details
// only in the server log.
func (h *handler) importError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, repo.ErrImportJobNotFound):
		writeAPIError(w, http.StatusNotFound, codeNotFound, "import job not found")
	case errors.Is(err, repo.ErrAccountNotFound):
		writeAPIError(w, http.StatusNotFound, codeNotFound, "account not found")
	default:
		slog.Error("import operation failed", "op", op, "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "import operation failed")
	}
}

// multipartFile reads the "file" and "account_id" parts from a
// multipart/form-data body with the given boundary (the handler takes
// it from the validated Content-Type header). It reports tooLarge when
// the file (or the total body) exceeds maxUploadBytes, readErr for a
// malformed body, and hasFile when a "file" part was present. Unknown
// part names are ignored (forward-compatible with extra UI fields).
func multipartFile(r *http.Request, boundary string) (fileBytes []byte, fileName, accountID string, hasFile, tooLarge bool, readErr error) {
	// Total-body guard: the file plus the multipart framing (small).
	// The writer is nil on purpose — MaxBytesReader would otherwise
	// write its own 413 first and prefix our JSON envelope with plain
	// text; the 413 is the handler's to write.
	r.Body = http.MaxBytesReader(nil, r.Body, maxUploadBytes+64<<10)
	mr := multipart.NewReader(r.Body, boundary)

	var maxBytesErr *http.MaxBytesError
loop:
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if errors.As(err, &maxBytesErr) {
				tooLarge = true
			} else {
				readErr = err
			}
			break
		}
		switch part.FormName() {
		case "file":
			b, rerr := io.ReadAll(io.LimitReader(part, maxUploadBytes+1))
			if rerr != nil {
				readErr = rerr
				break loop
			}
			if len(b) > maxUploadBytes {
				tooLarge = true
				break loop
			}
			fileBytes, fileName, hasFile = b, part.FileName(), true
		case "account_id":
			if b, rerr := io.ReadAll(part); rerr == nil {
				accountID = string(b)
			}
		}
	}
	return fileBytes, fileName, accountID, hasFile, tooLarge, readErr
}

// postImport serves POST /api/v1/imports: a multipart/form-data upload
// with a "file" part (the statement, capped at 10 MB) and an
// "account_id" field (the account the job lands in). It answers 201
// with the created row (status "uploaded").
func (h *handler) postImport(w http.ResponseWriter, r *http.Request) {
	if h.imports == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "file is required")
		return
	}
	fileBytes, fileName, accountID, hasFile, tooLarge, readErr := multipartFile(r, params["boundary"])
	if tooLarge {
		writeAPIError(w, http.StatusRequestEntityTooLarge, codePayloadTooLarge, "file must be at most 10 MB")
		return
	}
	if readErr != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "file is required")
		return
	}
	if !hasFile {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "file is required")
		return
	}
	if err := repo.ValidateFileName(fileName); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	if accountID == "" {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "account_id is required")
		return
	}
	if !uuidShapeOK(accountID) {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "account_id must be a valid UUID")
		return
	}
	job, err := h.imports.Create(r.Context(), c.row.ID, repo.ImportJobIn{
		AccountID: accountID,
		FileName:  fileName,
		File:      fileBytes,
	}, c.dek)
	if err != nil {
		h.importError(w, "create", err)
		return
	}
	writeJSON(w, http.StatusCreated, toImportOut(job))
}

// importsList serves GET /api/v1/imports: a page of the caller's
// import jobs, newest first (limit/offset like the notification list).
func (h *handler) importsList(w http.ResponseWriter, r *http.Request) {
	if h.imports == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	f, err := importFilterFromQuery(r.URL.Query())
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	jobs, err := h.imports.List(r.Context(), c.row.ID, f, c.dek)
	if err != nil {
		h.importError(w, "list", err)
		return
	}
	out := make([]importOut, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, toImportOut(j))
	}
	writeJSON(w, http.StatusOK, out)
}

// importGet serves GET /api/v1/imports/{id}: one of the caller's import
// jobs (the file name is decrypted server-side). A malformed id answers
// 404 (not_found) instead of reaching the DB; a well-formed id the
// caller does not own is also 404 (existence is not disclosed).
func (h *handler) importGet(w http.ResponseWriter, r *http.Request) {
	if h.imports == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if !uuidShapeOK(id) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "import job not found")
		return
	}
	job, err := h.imports.Get(r.Context(), c.row.ID, id, c.dek)
	if err != nil {
		h.importError(w, "get", err)
		return
	}
	writeJSON(w, http.StatusOK, toImportOut(job))
}

// FI institution + credential endpoints (plan step 9.1, migration
// 0010): GET/POST /api/v1/fi/institutions (the GLOBAL institution
// registry — auth required but no user scoping) and
// GET/POST /api/v1/fi/credentials plus DELETE
// /api/v1/fi/credentials/{id} (the per-user tokens). Both secrets are
// DEK-encrypted at rest by the repo layer; the handlers only ever see
// plaintext in and out. There is deliberately NO institution-delete
// route (9.1 scope): institutions are shared vocabulary, so there is
// no orphan path to serve.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"finance-tracker/internal/repo"

	"github.com/go-chi/chi/v5"
)

type InstitutionStore interface {
	Get(ctx context.Context, id string) (repo.Institution, error)
	GetByCode(ctx context.Context, code string) (repo.Institution, error)
	Create(ctx context.Context, in repo.InstitutionIn) (repo.Institution, error)
	List(ctx context.Context) ([]repo.Institution, error)
}

type FICredentialStore interface {
	List(ctx context.Context, userID string, dek []byte) ([]repo.Credential, error)
	Get(ctx context.Context, userID, id string, dek []byte) (repo.Credential, error)
	Create(ctx context.Context, userID string, in repo.FICredentialIn, dek []byte) (repo.Credential, error)
	Delete(ctx context.Context, userID, id string) error
}

// institutionOut is the wire DTO (RAW, no envelope, NO timestamps —
// the 3.1 convention) in the pinned field order.
type institutionOut struct {
	ID     string `json:"id"`
	Code   string `json:"code"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// credentialOut is the wire DTO in the pinned field order. The
// institution code/name are joined in by the repo; last_synced_at is
// null until the 9.2 sync job stamps it (RFC3339 string after that).
// The secrets never reach the wire: the repo opens the packs to
// validate them, then the DTO drops the plaintext.
type credentialOut struct {
	ID              string     `json:"id"`
	InstitutionID   string     `json:"institution_id"`
	InstitutionCode string     `json:"institution_code"`
	InstitutionName string     `json:"institution_name"`
	LastSyncedAt    *time.Time `json:"last_synced_at"`
}

func toInstitutionOut(i repo.Institution) institutionOut {
	return institutionOut{ID: i.ID, Code: i.Code, Name: i.Name, Status: i.Status}
}

func toCredentialOut(c repo.Credential) credentialOut {
	return credentialOut{
		ID:              c.ID,
		InstitutionID:   c.InstitutionID,
		InstitutionCode: c.InstitutionCode,
		InstitutionName: c.InstitutionName,
		LastSyncedAt:    c.LastSyncedAt,
	}
}

// institutionIn is the POST /api/v1/fi/institutions body
// {code, name}; unknown JSON fields are ignored by the decoder.
type institutionIn struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

// fiCredentialIn is the POST /api/v1/fi/credentials body
// {institution_id, token, refresh_token}; the secrets arrive as
// plaintext (manual token paste) and seal in the repo layer.
type fiCredentialIn struct {
	InstitutionID string `json:"institution_id"`
	Token         string `json:"token"`
	RefreshToken  string `json:"refresh_token"`
}

// fiInstitutionCodePattern is the pinned code shape: 1-64 characters
// of lowercase letters, digits, and hyphens, starting and ending with
// a letter or digit (a single character is allowed).
var fiInstitutionCodePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)

// maxInstitutionNameLen is the institution name limit (the 9.1 pin).
const maxInstitutionNameLen = 120

// maxFICredentialSecretLen is the token/refresh_token limit (the 9.1
// pin).
const maxFICredentialSecretLen = 4096

// validateInstitutionIn collects EVERY create problem (code shape,
// name length) and joins them with "; " (the mortgage convention), so
// one 400 reports all of them.
func validateInstitutionIn(in *institutionIn) error {
	var problems []string
	if !fiInstitutionCodePattern.MatchString(in.Code) {
		problems = append(problems, "institution code must be 1-64 characters of lowercase letters, digits, and hyphens, starting and ending with a letter or digit")
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || utf8.RuneCountInString(name) > maxInstitutionNameLen {
		problems = append(problems, "institution name must be 1-120 characters")
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// validateFICredentialIn collects EVERY create problem (all three
// fields) and joins them with "; " (the mortgage convention).
func validateFICredentialIn(in *fiCredentialIn) error {
	var problems []string
	if !uuidShapeOK(in.InstitutionID) {
		problems = append(problems, "institution_id must be a valid UUID")
	}
	if token := strings.TrimSpace(in.Token); token == "" || utf8.RuneCountInString(token) > maxFICredentialSecretLen {
		problems = append(problems, "token must be 1-4096 characters")
	}
	if refresh := strings.TrimSpace(in.RefreshToken); refresh == "" || utf8.RuneCountInString(refresh) > maxFICredentialSecretLen {
		problems = append(problems, "refresh_token must be 1-4096 characters")
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// institutionError maps institution-store errors to responses. The
// sentinel mapping is what the frontend keys off; everything else is a
// 500 with the details only in the server log.
func (h *handler) institutionError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, repo.ErrInstitutionCodeTaken):
		writeAPIError(w, http.StatusConflict, codeConflict, "institution code already exists")
	case errors.Is(err, repo.ErrInstitutionNotFound):
		writeAPIError(w, http.StatusNotFound, codeNotFound, "institution not found")
	default:
		slog.Error("fi institution operation failed", "op", op, "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "fi institution operation failed")
	}
}

// credentialError maps credential-store errors to responses (same
// convention as institutionError).
func (h *handler) credentialError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, repo.ErrFICredentialExists):
		writeAPIError(w, http.StatusConflict, codeConflict, "a credential for this institution already exists")
	case errors.Is(err, repo.ErrFICredentialNotFound):
		writeAPIError(w, http.StatusNotFound, codeNotFound, "fi credential not found")
	default:
		slog.Error("fi credential operation failed", "op", op, "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "fi credential operation failed")
	}
}

// fiInstitutionsList serves GET /api/v1/fi/institutions: the GLOBAL
// registry (auth required, no user scoping, no body) in the repo's
// code order ([] when empty).
func (h *handler) fiInstitutionsList(w http.ResponseWriter, r *http.Request) {
	if h.institutions == nil {
		notConfigured(w)
		return
	}
	if _, ok := h.requireUser(w, r); !ok {
		return
	}
	is, err := h.institutions.List(r.Context())
	if err != nil {
		h.institutionError(w, "list", err)
		return
	}
	out := make([]institutionOut, 0, len(is))
	for _, i := range is {
		out = append(out, toInstitutionOut(i))
	}
	writeJSON(w, http.StatusOK, out)
}

// fiInstitutionsCreate serves POST /api/v1/fi/institutions and answers
// 201 with the created row. Shape validation of ALL fields runs first
// (every problem "; "-joined, one 400) before any DB call.
func (h *handler) fiInstitutionsCreate(w http.ResponseWriter, r *http.Request) {
	if h.institutions == nil {
		notConfigured(w)
		return
	}
	if _, ok := h.requireUser(w, r); !ok {
		return
	}
	var in institutionIn
	if err := decodeBody(r, &in); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "body must be a JSON object: "+err.Error())
		return
	}
	if err := validateInstitutionIn(&in); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	i, err := h.institutions.Create(r.Context(), repo.InstitutionIn{
		Code: in.Code,
		Name: strings.TrimSpace(in.Name),
	})
	if err != nil {
		h.institutionError(w, "create", err)
		return
	}
	writeJSON(w, http.StatusCreated, toInstitutionOut(i))
}

// fiCredentialsList serves GET /api/v1/fi/credentials: the caller's
// credentials with both secrets opened in the repo layer (the DTO
// carries none of them) and the institution code/name joined in, in
// the repo's institution-code order ([] when empty).
func (h *handler) fiCredentialsList(w http.ResponseWriter, r *http.Request) {
	if h.fiCredentials == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	cs, err := h.fiCredentials.List(r.Context(), c.row.ID, c.dek)
	if err != nil {
		h.credentialError(w, "list", err)
		return
	}
	out := make([]credentialOut, 0, len(cs))
	for _, c := range cs {
		out = append(out, toCredentialOut(c))
	}
	writeJSON(w, http.StatusOK, out)
}

// fiCredentialsCreate serves POST /api/v1/fi/credentials and answers
// 201 with the created row (last_synced_at null until the first sync).
// The checks run in this order (the 9.1 pin): shape validation of ALL
// fields first (every problem "; "-joined, one 400, before any DB
// call), then the institution existence check (404). Both secrets
// seal in the repo layer.
func (h *handler) fiCredentialsCreate(w http.ResponseWriter, r *http.Request) {
	if h.fiCredentials == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	var in fiCredentialIn
	if err := decodeBody(r, &in); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "body must be a JSON object: "+err.Error())
		return
	}
	if err := validateFICredentialIn(&in); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	if _, err := h.institutions.Get(r.Context(), in.InstitutionID); err != nil {
		if errors.Is(err, repo.ErrInstitutionNotFound) {
			writeAPIError(w, http.StatusNotFound, codeNotFound, "institution not found")
			return
		}
		h.credentialError(w, "create", err)
		return
	}
	cred, err := h.fiCredentials.Create(r.Context(), c.row.ID, repo.FICredentialIn{
		InstitutionID: in.InstitutionID,
		Token:         strings.TrimSpace(in.Token),
		RefreshToken:  strings.TrimSpace(in.RefreshToken),
	}, c.dek)
	if err != nil {
		h.credentialError(w, "create", err)
		return
	}
	writeJSON(w, http.StatusCreated, toCredentialOut(cred))
}

// fiCredentialsDelete serves DELETE /api/v1/fi/credentials/{id} and
// answers 204 (no content) on success. A malformed id answers 404
// (not_found) instead of reaching the store; a well-formed id the
// caller does not own is also 404 (existence is not disclosed).
func (h *handler) fiCredentialsDelete(w http.ResponseWriter, r *http.Request) {
	if h.fiCredentials == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if !uuidShapeOK(id) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "fi credential not found")
		return
	}
	if err := h.fiCredentials.Delete(r.Context(), c.row.ID, id); err != nil {
		h.credentialError(w, "delete", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Account endpoints (plan step 3.1): GET/POST /api/v1/accounts and
// PATCH/DELETE /api/v1/accounts/{id} over the repo.AccountStore. Names
// are DEK-encrypted at rest by the repo layer; the handlers only ever
// see plaintext in and out. Every store call is scoped to the caller's
// users.id, so a token can only ever touch its own rows.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"finance-tracker/internal/repo"

	"github.com/go-chi/chi/v5"
)

// AccountStore is the persistence surface the account handlers need. It
// mirrors *repo.AccountStore's method set, which satisfies it; route
// tests use an in-memory implementation.
type AccountStore interface {
	List(ctx context.Context, userID string, dek []byte) ([]repo.Account, error)
	Get(ctx context.Context, userID, id string, dek []byte) (repo.Account, error)
	Create(ctx context.Context, userID string, in repo.AccountIn, dek []byte) (repo.Account, error)
	Update(ctx context.Context, userID, id string, p repo.AccountPatch, dek []byte) (repo.Account, error)
	Delete(ctx context.Context, userID, id string) error
}

// Wire DTOs: the repo types carry "" for unset NULLs, but the API speaks
// JSON null for those, so the handlers translate through strOrNil.
type accountOut struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	Type            string  `json:"type"`
	Subtype         *string `json:"subtype"`
	Currency        string  `json:"currency"`
	DefaultSchemaID *string `json:"default_schema_id"`
}

func toAccountOut(a repo.Account) accountOut {
	return accountOut{
		ID:              a.ID,
		Name:            a.Name,
		Type:            a.Type,
		Subtype:         strOrNil(a.Subtype),
		Currency:        a.Currency,
		DefaultSchemaID: strOrNil(a.DefaultSchemaID),
	}
}

// accountIn is the POST body. Subtype and DefaultSchemaID are null when
// unset; an empty string is treated the same as null (the repo trims
// subtypes to "" into NULL).
type accountIn struct {
	Name            string  `json:"name"`
	Type            string  `json:"type"`
	Subtype         *string `json:"subtype"`
	Currency        string  `json:"currency"`
	DefaultSchemaID *string `json:"default_schema_id"`
}

// accountPatch is the PATCH body, mirroring repo.AccountPatch: simple
// fields are pointers (absent = untouched) and Subtype /
// DefaultSchemaID are tri-state (absent = untouched, null = clear,
// string = new value).
type accountPatch struct {
	Name            *string        `json:"name"`
	Type            *string        `json:"type"`
	Subtype         triStateString `json:"subtype"`
	Currency        *string        `json:"currency"`
	DefaultSchemaID triStateString `json:"default_schema_id"`
}

// accountTypeOK checks the 0002 accounts.type vocabulary (exported by
// the repo package).
func accountTypeOK(t string) bool {
	for _, v := range repo.AccountTypes {
		if v == t {
			return true
		}
	}
	return false
}

// validateAccountIn checks every POST field; all problems are collected
// and joined with "; " (the settings convention).
func (h *handler) validateAccountIn(in *accountIn) error {
	var problems []string
	if err := checkName(in.Name); err != nil {
		problems = append(problems, err.Error())
	}
	if !accountTypeOK(in.Type) {
		problems = append(problems, "type must be one of "+strings.Join(repo.AccountTypes, ", "))
	}
	if !iso4217Re.MatchString(in.Currency) {
		problems = append(problems, "currency must be a 3-letter uppercase ISO-4217 code")
	}
	if in.Subtype != nil && utf8.RuneCountInString(*in.Subtype) > maxSubtypeLen {
		problems = append(problems, fmt.Sprintf("subtype must be at most %d characters", maxSubtypeLen))
	}
	if in.DefaultSchemaID != nil && *in.DefaultSchemaID != "" && !uuidShapeOK(*in.DefaultSchemaID) {
		problems = append(problems, "default_schema_id must be a valid UUID")
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// validateAccountPatch checks the set PATCH fields the same way.
func (h *handler) validateAccountPatch(p *accountPatch) error {
	var problems []string
	if p.Name != nil {
		if err := checkName(*p.Name); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if p.Type != nil && !accountTypeOK(*p.Type) {
		problems = append(problems, "type must be one of "+strings.Join(repo.AccountTypes, ", "))
	}
	if p.Currency != nil && !iso4217Re.MatchString(*p.Currency) {
		problems = append(problems, "currency must be a 3-letter uppercase ISO-4217 code")
	}
	if p.Subtype.set {
		if inner := p.Subtype.value; inner != nil && utf8.RuneCountInString(*inner) > maxSubtypeLen {
			problems = append(problems, fmt.Sprintf("subtype must be at most %d characters", maxSubtypeLen))
		}
	}
	if p.DefaultSchemaID.set {
		if inner := p.DefaultSchemaID.value; inner != nil && *inner != "" && !uuidShapeOK(*inner) {
			problems = append(problems, "default_schema_id must be a valid UUID")
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// accountError maps store errors to responses. The sentinel mapping is
// what the frontend (1.4b+) keys off; everything else is a 500 with the
// details only in the server log.
func (h *handler) accountError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, repo.ErrAccountNotFound):
		writeAPIError(w, http.StatusNotFound, codeNotFound, "account not found")
	case errors.Is(err, repo.ErrNameTaken):
		writeAPIError(w, http.StatusConflict, codeConflict, "an account with this name already exists")
	case errors.Is(err, repo.ErrSchemaNotFound):
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "default schema not found")
	default:
		slog.Error("account operation failed", "op", op, "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "account operation failed")
	}
}

// accountsList serves GET /api/v1/accounts: the caller's accounts,
// ordered by creation time (repo contract).
func (h *handler) accountsList(w http.ResponseWriter, r *http.Request) {
	if h.accounts == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	accounts, err := h.accounts.List(r.Context(), c.row.ID, c.dek)
	if err != nil {
		h.accountError(w, "list", err)
		return
	}
	out := make([]accountOut, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, toAccountOut(a))
	}
	writeJSON(w, http.StatusOK, out)
}

// accountsCreate serves POST /api/v1/accounts and answers 201 with the
// created row.
func (h *handler) accountsCreate(w http.ResponseWriter, r *http.Request) {
	if h.accounts == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	var in accountIn
	if err := decodeBody(r, &in); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "body must be a JSON object: "+err.Error())
		return
	}
	if err := h.validateAccountIn(&in); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	acc, err := h.accounts.Create(r.Context(), c.row.ID, repo.AccountIn{
		Name:            in.Name,
		Type:            in.Type,
		Subtype:         in.Subtype,
		Currency:        in.Currency,
		DefaultSchemaID: in.DefaultSchemaID,
	}, c.dek)
	if err != nil {
		h.accountError(w, "create", err)
		return
	}
	writeJSON(w, http.StatusCreated, toAccountOut(acc))
}

// accountsUpdate serves PATCH /api/v1/accounts/{id}. A malformed id
// answers 404 (not_found) instead of reaching the DB; a well-formed id
// the caller does not own is also 404 (existence is not disclosed).
func (h *handler) accountsUpdate(w http.ResponseWriter, r *http.Request) {
	if h.accounts == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if !uuidShapeOK(id) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "account not found")
		return
	}
	var p accountPatch
	if err := decodeBody(r, &p); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "body must be a JSON object: "+err.Error())
		return
	}
	if p.Name == nil && p.Type == nil && !p.Subtype.set && p.Currency == nil && !p.DefaultSchemaID.set {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "empty patch: provide at least one field")
		return
	}
	if err := h.validateAccountPatch(&p); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	acc, err := h.accounts.Update(r.Context(), c.row.ID, id, repo.AccountPatch{
		Name:            p.Name,
		Type:            p.Type,
		Subtype:         p.Subtype.toRepo(),
		Currency:        p.Currency,
		DefaultSchemaID: p.DefaultSchemaID.toRepo(),
	}, c.dek)
	if err != nil {
		h.accountError(w, "update", err)
		return
	}
	writeJSON(w, http.StatusOK, toAccountOut(acc))
}

// accountsDelete serves DELETE /api/v1/accounts/{id} and answers 204
// (no content) on success.
func (h *handler) accountsDelete(w http.ResponseWriter, r *http.Request) {
	if h.accounts == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if !uuidShapeOK(id) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "account not found")
		return
	}
	if err := h.accounts.Delete(r.Context(), c.row.ID, id); err != nil {
		h.accountError(w, "delete", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

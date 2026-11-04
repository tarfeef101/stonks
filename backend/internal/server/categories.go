// Category endpoints (plan step 3.1): GET/POST /api/v1/categories and
// PATCH/DELETE /api/v1/categories/{id} over the repo.CategoryStore
// (user_categories). Names are DEK-encrypted at rest by the repo layer;
// the handlers only ever see plaintext in and out. Every store call is
// scoped to the caller's users.id, so a token can only ever touch its
// own rows.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"finance-tracker/internal/repo"

	"github.com/go-chi/chi/v5"
)

// CategoryStore is the persistence surface the category handlers need.
// It mirrors *repo.CategoryStore's method set, which satisfies it; route
// tests use an in-memory implementation.
type CategoryStore interface {
	List(ctx context.Context, userID string, dek []byte) ([]repo.Category, error)
	Get(ctx context.Context, userID, id string, dek []byte) (repo.Category, error)
	Create(ctx context.Context, userID string, in repo.CategoryIn, dek []byte) (repo.Category, error)
	Update(ctx context.Context, userID, id string, p repo.CategoryPatch, dek []byte) (repo.Category, error)
	Delete(ctx context.Context, userID, id string) error
}

// Wire DTOs: the repo type carries "" for an unset parent, but the API
// speaks JSON null for that, so the handlers translate through strOrNil.
type categoryOut struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	ParentID  *string `json:"parent_id"`
	Active    bool    `json:"active"`
	SortOrder int     `json:"sort_order"`
}

func toCategoryOut(c repo.Category) categoryOut {
	return categoryOut{
		ID:        c.ID,
		Name:      c.Name,
		ParentID:  strOrNil(c.ParentID),
		Active:    c.Active,
		SortOrder: c.SortOrder,
	}
}

// categoryIn is the POST body. ParentID is null (or absent) for a root
// category; otherwise it must be the id of one of the caller's existing
// categories.
type categoryIn struct {
	Name     string  `json:"name"`
	ParentID *string `json:"parent_id"`
}

// categoryPatch is the PATCH body, mirroring repo.CategoryPatch: Name
// and Active are pointers (absent = untouched) and ParentID is tri-state
// (absent = untouched, null = demote to root, string = new parent).
type categoryPatch struct {
	Name     *string        `json:"name"`
	ParentID triStateString `json:"parent_id"`
	Active   *bool          `json:"active"`
}

// validateCategoryIn checks the POST fields.
func (h *handler) validateCategoryIn(in *categoryIn) error {
	var problems []string
	if err := checkName(in.Name); err != nil {
		problems = append(problems, err.Error())
	}
	if in.ParentID != nil && *in.ParentID != "" && !uuidShapeOK(*in.ParentID) {
		problems = append(problems, "parent_id must be a valid UUID")
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// validateCategoryPatch checks the set PATCH fields the same way.
func (h *handler) validateCategoryPatch(p *categoryPatch) error {
	var problems []string
	if p.Name != nil {
		if err := checkName(*p.Name); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if p.ParentID.set {
		if inner := p.ParentID.value; inner != nil && *inner != "" && !uuidShapeOK(*inner) {
			problems = append(problems, "parent_id must be a valid UUID")
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// categoryError maps store errors to responses. The sentinel mapping is
// what the frontend (1.4b+) keys off; everything else is a 500 with the
// details only in the server log.
func (h *handler) categoryError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, repo.ErrCategoryNotFound):
		writeAPIError(w, http.StatusNotFound, codeNotFound, "category not found")
	case errors.Is(err, repo.ErrNameTaken):
		writeAPIError(w, http.StatusConflict, codeConflict, "a category with this name already exists")
	case errors.Is(err, repo.ErrParentNotFound):
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "parent category not found")
	default:
		slog.Error("category operation failed", "op", op, "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "category operation failed")
	}
}

// categoriesList serves GET /api/v1/categories: the caller's categories
// ordered by (sort_order, created_at) (repo contract).
func (h *handler) categoriesList(w http.ResponseWriter, r *http.Request) {
	if h.categories == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	cats, err := h.categories.List(r.Context(), c.row.ID, c.dek)
	if err != nil {
		h.categoryError(w, "list", err)
		return
	}
	out := make([]categoryOut, 0, len(cats))
	for _, c := range cats {
		out = append(out, toCategoryOut(c))
	}
	writeJSON(w, http.StatusOK, out)
}

// categoriesCreate serves POST /api/v1/categories and answers 201 with
// the created row (active, sort_order 0, root unless parent_id is set).
func (h *handler) categoriesCreate(w http.ResponseWriter, r *http.Request) {
	if h.categories == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	var in categoryIn
	if err := decodeBody(r, &in); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "body must be a JSON object: "+err.Error())
		return
	}
	if err := h.validateCategoryIn(&in); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	cat, err := h.categories.Create(r.Context(), c.row.ID, repo.CategoryIn{
		Name:     in.Name,
		ParentID: in.ParentID,
	}, c.dek)
	if err != nil {
		h.categoryError(w, "create", err)
		return
	}
	writeJSON(w, http.StatusCreated, toCategoryOut(cat))
}

// categoriesUpdate serves PATCH /api/v1/categories/{id}. A malformed id
// answers 404 (not_found) instead of reaching the DB; a well-formed id
// the caller does not own is also 404 (existence is not disclosed).
func (h *handler) categoriesUpdate(w http.ResponseWriter, r *http.Request) {
	if h.categories == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if !uuidShapeOK(id) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "category not found")
		return
	}
	var p categoryPatch
	if err := decodeBody(r, &p); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "body must be a JSON object: "+err.Error())
		return
	}
	if p.Name == nil && !p.ParentID.set && p.Active == nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "empty patch: provide at least one field")
		return
	}
	if err := h.validateCategoryPatch(&p); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	cat, err := h.categories.Update(r.Context(), c.row.ID, id, repo.CategoryPatch{
		Name:     p.Name,
		ParentID: p.ParentID.toRepo(),
		Active:   p.Active,
	}, c.dek)
	if err != nil {
		h.categoryError(w, "update", err)
		return
	}
	writeJSON(w, http.StatusOK, toCategoryOut(cat))
}

// categoriesDelete serves DELETE /api/v1/categories/{id} and answers
// 204 (no content) on success.
func (h *handler) categoriesDelete(w http.ResponseWriter, r *http.Request) {
	if h.categories == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if !uuidShapeOK(id) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "category not found")
		return
	}
	if err := h.categories.Delete(r.Context(), c.row.ID, id); err != nil {
		h.categoryError(w, "delete", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

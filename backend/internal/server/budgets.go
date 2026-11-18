// Budget endpoints (plan step 3.3): GET/POST /api/v1/budgets and
// PATCH/DELETE /api/v1/budgets/{id} over the repo.BudgetStore. Names
// and targets are DEK-encrypted at rest by the repo layer; the handlers
// only ever see plaintext in and out. Every store call is scoped to the
// caller's users.id, so a token can only ever touch its own rows. There
// is no single-GET route (the List covers it, like transactions).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"finance-tracker/internal/repo"

	"github.com/go-chi/chi/v5"
)

// BudgetStore is the persistence surface the budget handlers need (the
// summary handler, step 3.2, only uses ActiveTargets; the CRUD routes
// use the rest). *repo.BudgetStore satisfies it (its extra Get is
// unused here); route tests use an in-memory implementation.
type BudgetStore interface {
	List(ctx context.Context, userID string, dek []byte) ([]repo.Budget, error)
	ActiveTargets(ctx context.Context, userID, period string, dek []byte) ([]repo.ActiveBudget, error)
	Create(ctx context.Context, userID string, in repo.BudgetIn, dek []byte) (repo.Budget, error)
	Update(ctx context.Context, userID, id string, p repo.BudgetPatch, dek []byte) (repo.Budget, error)
	Delete(ctx context.Context, userID, id string) error
}

// Wire DTOs. budgetOut carries no timestamps (bookkeeping, per the 3.1
// convention); Targets is the decrypted targets document (a JSON object
// keyed by category id) passed through as-is.
type budgetOut struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Period  string          `json:"period"`
	Active  bool            `json:"active"`
	Targets json.RawMessage `json:"targets"`
}

func toBudgetOut(b repo.Budget) budgetOut {
	return budgetOut{
		ID:      b.ID,
		Name:    b.Name,
		Period:  b.Period,
		Active:  b.Active,
		Targets: b.Targets,
	}
}

// budgetIn is the POST body. Active is null = default true; Targets must
// be a non-empty JSON object keyed by the caller's own category ids.
type budgetIn struct {
	Name    string          `json:"name"`
	Period  string          `json:"period"`
	Active  *bool           `json:"active"`
	Targets json.RawMessage `json:"targets"`
}

// budgetPatch is the PATCH body, mirroring repo.BudgetPatch: plain
// pointers (absent or null = untouched, set = new value). Period is
// deliberately absent — it is immutable (see the repo package header).
// Unknown JSON fields are ignored by the decoder.
type budgetPatch struct {
	Name    *string          `json:"name"`
	Active  *bool            `json:"active"`
	Targets *json.RawMessage `json:"targets"`
}

// validateBudgetIn checks every POST field through the repo's exported
// validators so a bad value is a 400 carrying the repo's exact message
// before any store call; all problems are collected and joined with
// "; " (the settings convention).
func (h *handler) validateBudgetIn(in *budgetIn) error {
	var problems []string
	if err := checkName(in.Name); err != nil {
		problems = append(problems, err.Error())
	}
	if err := repo.ValidatePeriod(in.Period); err != nil {
		problems = append(problems, err.Error())
	}
	if err := repo.ValidateTargets(in.Targets); err != nil {
		problems = append(problems, err.Error())
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// validateBudgetPatch checks the set PATCH fields the same way.
func (h *handler) validateBudgetPatch(p *budgetPatch) error {
	var problems []string
	if p.Name != nil {
		if err := checkName(*p.Name); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if p.Targets != nil {
		if err := repo.ValidateTargets(*p.Targets); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// budgetError maps store errors to responses. The sentinel mapping is
// what the frontend keys off; everything else is a 500 with the details
// only in the server log.
func (h *handler) budgetError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, repo.ErrBudgetNotFound):
		writeAPIError(w, http.StatusNotFound, codeNotFound, "budget not found")
	case errors.Is(err, repo.ErrCategoryNotFound):
		writeAPIError(w, http.StatusNotFound, codeNotFound, "category not found")
	default:
		slog.Error("budget operation failed", "op", op, "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "budget operation failed")
	}
}

// budgetsList serves GET /api/v1/budgets: the caller's budgets, ordered
// by creation time (repo contract).
func (h *handler) budgetsList(w http.ResponseWriter, r *http.Request) {
	if h.budgets == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	budgets, err := h.budgets.List(r.Context(), c.row.ID, c.dek)
	if err != nil {
		h.budgetError(w, "list", err)
		return
	}
	out := make([]budgetOut, 0, len(budgets))
	for _, b := range budgets {
		out = append(out, toBudgetOut(b))
	}
	writeJSON(w, http.StatusOK, out)
}

// budgetsCreate serves POST /api/v1/budgets and answers 201 with the
// created row.
func (h *handler) budgetsCreate(w http.ResponseWriter, r *http.Request) {
	if h.budgets == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	var in budgetIn
	if err := decodeBody(r, &in); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "body must be a JSON object: "+err.Error())
		return
	}
	if err := h.validateBudgetIn(&in); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	b, err := h.budgets.Create(r.Context(), c.row.ID, repo.BudgetIn{
		Name:    in.Name,
		Period:  in.Period,
		Active:  in.Active,
		Targets: in.Targets,
	}, c.dek)
	if err != nil {
		h.budgetError(w, "create", err)
		return
	}
	writeJSON(w, http.StatusCreated, toBudgetOut(b))
}

// budgetsUpdate serves PATCH /api/v1/budgets/{id}. A malformed id
// answers 404 (not_found) instead of reaching the DB; a well-formed id
// the caller does not own is also 404 (existence is not disclosed).
func (h *handler) budgetsUpdate(w http.ResponseWriter, r *http.Request) {
	if h.budgets == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if !uuidShapeOK(id) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "budget not found")
		return
	}
	var p budgetPatch
	if err := decodeBody(r, &p); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "body must be a JSON object: "+err.Error())
		return
	}
	if p.Name == nil && p.Active == nil && p.Targets == nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "empty patch: provide at least one field")
		return
	}
	if err := h.validateBudgetPatch(&p); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	b, err := h.budgets.Update(r.Context(), c.row.ID, id, repo.BudgetPatch{
		Name:    p.Name,
		Active:  p.Active,
		Targets: p.Targets,
	}, c.dek)
	if err != nil {
		h.budgetError(w, "update", err)
		return
	}
	writeJSON(w, http.StatusOK, toBudgetOut(b))
}

// budgetsDelete serves DELETE /api/v1/budgets/{id} and answers 204 (no
// content) on success.
func (h *handler) budgetsDelete(w http.ResponseWriter, r *http.Request) {
	if h.budgets == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if !uuidShapeOK(id) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "budget not found")
		return
	}
	if err := h.budgets.Delete(r.Context(), c.row.ID, id); err != nil {
		h.budgetError(w, "delete", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

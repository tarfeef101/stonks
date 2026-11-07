// Dashboard card endpoints (plan step 6.5, migration 0008):
// GET/POST /api/v1/dashboard-cards, GET/PATCH/DELETE
// /api/v1/dashboard-cards/{id} and PUT /api/v1/dashboard-cards/order
// over the repo.DashboardCardStore. Cards are saved Explore views: name
// + the canonical explore_state query string are DEK-encrypted at rest
// by the repo layer; the handlers only ever see plaintext in and out.
// Every store call is scoped to the caller's users.id, so a token can
// only ever touch its own rows. The frontend is the source of truth for
// explore_state canonical form (it only ever stores serializeState()
// output), so the server does NOT re-validate it.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"finance-tracker/internal/repo"

	"github.com/go-chi/chi/v5"
)

// CardStore is the persistence surface the dashboard card routes use.
// *repo.DashboardCardStore satisfies it; route tests use an in-memory
// implementation.
type CardStore interface {
	List(ctx context.Context, userID string, dek []byte) ([]repo.DashboardCard, error)
	Get(ctx context.Context, userID, id string, dek []byte) (repo.DashboardCard, error)
	Create(ctx context.Context, userID string, in repo.DashboardCardIn, dek []byte) (repo.DashboardCard, error)
	Update(ctx context.Context, userID, id string, p repo.DashboardCardPatch, dek []byte) (repo.DashboardCard, error)
	Delete(ctx context.Context, userID, id string) error
	SetOrder(ctx context.Context, userID string, ids []string) error
}

// maxExploreStateLen is the explore_state cap (the canonical query
// string is a few key=value pairs; 1024 is generous and keeps the
// encrypted pack small).
const maxExploreStateLen = 1024

// Wire DTO (RAW, no envelope, NO timestamps — 3.1 convention).
// ExploreState is "" for the default (all-time) view.
type cardOut struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	ExploreState string `json:"explore_state"`
	SortOrder    int    `json:"sort_order"`
}

func toCardOut(c repo.DashboardCard) cardOut {
	return cardOut{
		ID:           c.ID,
		Name:         c.Name,
		ExploreState: c.ExploreState,
		SortOrder:    c.SortOrder,
	}
}

// cardIn is the POST body. ExploreState is a plain string: "" (absent)
// IS the default all-time view, so no pointer.
type cardIn struct {
	Name         string `json:"name"`
	ExploreState string `json:"explore_state"`
}

// cardPatch is the PATCH body, mirroring repo.DashboardCardPatch: plain
// pointers (absent or null = untouched, set = new value). Unknown JSON
// fields are ignored by the decoder.
type cardPatch struct {
	Name         *string `json:"name"`
	ExploreState *string `json:"explore_state"`
}

// validateExploreState enforces the length cap; canonical form is the
// frontend's business (see the package comment).
func validateExploreState(state string) error {
	if utf8.RuneCountInString(state) > maxExploreStateLen {
		return errors.New("explore_state must be at most 1024 characters")
	}
	return nil
}

// validateCardIn checks every POST field so a bad value is a 400 before
// any store call; all problems are collected and joined with "; " (the
// settings convention, like validateBudgetIn). A non-string
// explore_state never gets here: the string field fails in decodeBody
// with "body must be a JSON object: ..." (no separate message).
func (h *handler) validateCardIn(in *cardIn) error {
	var problems []string
	if err := checkName(in.Name); err != nil {
		problems = append(problems, err.Error())
	}
	if err := validateExploreState(in.ExploreState); err != nil {
		problems = append(problems, err.Error())
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// validateCardPatch checks the set PATCH fields the same way.
func (h *handler) validateCardPatch(p *cardPatch) error {
	var problems []string
	if p.Name != nil {
		if err := checkName(*p.Name); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if p.ExploreState != nil {
		if err := validateExploreState(*p.ExploreState); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// cardError maps store errors to responses. The sentinel mapping is
// what the frontend keys off; everything else is a 500 with the details
// only in the server log.
func (h *handler) cardError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, repo.ErrDashboardCardNotFound):
		writeAPIError(w, http.StatusNotFound, codeNotFound, "dashboard card not found")
	default:
		slog.Error("dashboard card operation failed", "op", op, "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "dashboard card operation failed")
	}
}

// cardsList serves GET /api/v1/dashboard-cards: the caller's cards
// ordered by sort_order ([] when empty).
func (h *handler) cardsList(w http.ResponseWriter, r *http.Request) {
	if h.cards == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	cards, err := h.cards.List(r.Context(), c.row.ID, c.dek)
	if err != nil {
		h.cardError(w, "list", err)
		return
	}
	out := make([]cardOut, 0, len(cards))
	for _, card := range cards {
		out = append(out, toCardOut(card))
	}
	writeJSON(w, http.StatusOK, out)
}

// cardsGet serves GET /api/v1/dashboard-cards/{id}. A malformed id
// answers 404 (not_found) instead of reaching the DB; a well-formed id
// the caller does not own is also 404 (existence is not disclosed).
func (h *handler) cardsGet(w http.ResponseWriter, r *http.Request) {
	if h.cards == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if !uuidShapeOK(id) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "dashboard card not found")
		return
	}
	card, err := h.cards.Get(r.Context(), c.row.ID, id, c.dek)
	if err != nil {
		h.cardError(w, "get", err)
		return
	}
	writeJSON(w, http.StatusOK, toCardOut(card))
}

// cardsCreate serves POST /api/v1/dashboard-cards and answers 201 with
// the created row.
func (h *handler) cardsCreate(w http.ResponseWriter, r *http.Request) {
	if h.cards == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	var in cardIn
	if err := decodeBody(r, &in); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "body must be a JSON object: "+err.Error())
		return
	}
	if err := h.validateCardIn(&in); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	card, err := h.cards.Create(r.Context(), c.row.ID, repo.DashboardCardIn{
		Name:         in.Name,
		ExploreState: in.ExploreState,
	}, c.dek)
	if err != nil {
		h.cardError(w, "create", err)
		return
	}
	writeJSON(w, http.StatusCreated, toCardOut(card))
}

// cardsUpdate serves PATCH /api/v1/dashboard-cards/{id} (same id rules
// as cardsGet). A patch with neither field set is an empty patch.
func (h *handler) cardsUpdate(w http.ResponseWriter, r *http.Request) {
	if h.cards == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if !uuidShapeOK(id) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "dashboard card not found")
		return
	}
	var p cardPatch
	if err := decodeBody(r, &p); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "body must be a JSON object: "+err.Error())
		return
	}
	if p.Name == nil && p.ExploreState == nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "empty patch: provide at least one field")
		return
	}
	if err := h.validateCardPatch(&p); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	card, err := h.cards.Update(r.Context(), c.row.ID, id, repo.DashboardCardPatch{
		Name:         p.Name,
		ExploreState: p.ExploreState,
	}, c.dek)
	if err != nil {
		h.cardError(w, "update", err)
		return
	}
	writeJSON(w, http.StatusOK, toCardOut(card))
}

// cardsDelete serves DELETE /api/v1/dashboard-cards/{id} and answers
// 204 (no content) on success.
func (h *handler) cardsDelete(w http.ResponseWriter, r *http.Request) {
	if h.cards == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if !uuidShapeOK(id) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "dashboard card not found")
		return
	}
	if err := h.cards.Delete(r.Context(), c.row.ID, id); err != nil {
		h.cardError(w, "delete", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// cardsSetOrder serves PUT /api/v1/dashboard-cards/order. The body is a
// BARE JSON array of the caller's card uuids, every one exactly once,
// in the new order; the new sort_order of ids[i] is i+1. The checks
// run in this exact order (plan 6.5 pin):
//  1. body not a JSON array → 400 "body must be a JSON array: "+err
//     (json.NewDecoder directly — decodeBody is for objects);
//  2. any element not uuidShapeOK → 400 "order must contain only card
//     ids";
//  3. any id not among the caller's cards → 404 "dashboard card not
//     found" (existence is not disclosed, so the id is NOT echoed);
//  4. count mismatch or duplicate → 400 "order must list every card
//     exactly once" (an empty array against zero cards IS valid);
//  5. SetOrder → 204.
func (h *handler) cardsSetOrder(w http.ResponseWriter, r *http.Request) {
	if h.cards == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	var ids []string
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&ids); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "body must be a JSON array: "+err.Error())
		return
	}
	for _, id := range ids {
		if !uuidShapeOK(id) {
			writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "order must contain only card ids")
			return
		}
	}
	cards, err := h.cards.List(r.Context(), c.row.ID, c.dek)
	if err != nil {
		h.cardError(w, "list", err)
		return
	}
	owned := make(map[string]bool, len(cards))
	for _, card := range cards {
		owned[card.ID] = true
	}
	for _, id := range ids {
		if !owned[id] {
			writeAPIError(w, http.StatusNotFound, codeNotFound, "dashboard card not found")
			return
		}
	}
	if len(ids) != len(cards) {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "order must list every card exactly once")
		return
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "order must list every card exactly once")
			return
		}
		seen[id] = true
	}
	if err := h.cards.SetOrder(r.Context(), c.row.ID, ids); err != nil {
		h.cardError(w, "setorder", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

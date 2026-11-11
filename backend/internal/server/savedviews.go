// Saved Explore view endpoints (round 16, migration 0015):
// GET/POST /api/v1/saved-views, PATCH/DELETE /api/v1/saved-views/{id}
// over the repo.SavedViewStore. A saved view is a per-user named
// snapshot of the Explore page's URL state — the canonical
// explore_state query string ("" = the default view, the frontend
// codec's serializeState() output). The Explore view bar saves, opens,
// updates, renames and deletes them; the dashboard add-widget flow
// picks one and the widget stores its OWN copy of the state (a copy,
// not a reference — deleting a saved view never breaks a widget).
// name + state are DEK-encrypted at rest by the repo layer; the
// handlers only ever see plaintext in and out. Every store call is
// scoped to the caller's users.id, so a token can only ever touch its
// own rows. The frontend is the source of truth for explore_state
// canonical form (it only ever stores serializeState() output), so the
// server does NOT re-validate it — only the length cap (1024 runes,
// the same cap as an explore widget's content). Like the dashboards
// there is no single-GET route and no unique name.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"unicode/utf8"

	"finance-tracker/internal/repo"

	"github.com/go-chi/chi/v5"
)

// SavedViewStore is the persistence surface the saved-view routes use.
// *repo.SavedViewStore satisfies it; route tests use an in-memory
// implementation.
type SavedViewStore interface {
	List(ctx context.Context, userID string, dek []byte) ([]repo.SavedView, error)
	Create(ctx context.Context, userID string, in repo.SavedViewIn, dek []byte) (repo.SavedView, error)
	Update(ctx context.Context, userID, id string, p repo.SavedViewPatch, dek []byte) (repo.SavedView, error)
	Delete(ctx context.Context, userID, id string) error
}

// maxSavedViewStateLen caps a saved view's state (runes): the same
// ceiling as an explore widget's content — a canonical explore_state
// string is a handful of key=value pairs, 1024 keeps the encrypted
// pack small (the old dashboard_cards cap).
const maxSavedViewStateLen = 1024

// Wire DTO (RAW, no envelope, NO timestamps — 3.1 convention).
type savedViewOut struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
}

func toSavedViewOut(v repo.SavedView) savedViewOut {
	return savedViewOut{ID: v.ID, Name: v.Name, State: v.State}
}

// savedViewIn is the POST /v1/saved-views body. State is a plain
// string: "" (absent) IS the default view (the same convention as the
// explore widget content).
type savedViewIn struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

// savedViewPatch is the PATCH body, mirroring repo.SavedViewPatch:
// plain pointers (absent or null = untouched, set = new value).
type savedViewPatch struct {
	Name  *string `json:"name"`
	State *string `json:"state"`
}

// checkSavedViewState enforces the state length cap (runes). Canonical
// form is the frontend's business (see the package comment).
func checkSavedViewState(state string) error {
	if utf8.RuneCountInString(state) > maxSavedViewStateLen {
		return fmt.Errorf("state must be at most %d characters", maxSavedViewStateLen)
	}
	return nil
}

// savedViewError maps store errors to responses. The sentinel is a 404
// with a fixed message — existence is not disclosed (the dashboard
// convention); everything else is a 500 with the details only in the
// server log.
func (h *handler) savedViewError(w http.ResponseWriter, op string, err error) {
	if errors.Is(err, repo.ErrSavedViewNotFound) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "saved view not found")
		return
	}
	slog.Error("saved view operation failed", "op", op, "error", err)
	writeAPIError(w, http.StatusInternalServerError, codeInternal, "saved view operation failed")
}

// savedViewsList serves GET /api/v1/saved-views: the caller's saved
// views, created_at asc (id asc tiebreak) ([] when empty).
func (h *handler) savedViewsList(w http.ResponseWriter, r *http.Request) {
	if h.savedViews == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	views, err := h.savedViews.List(r.Context(), c.row.ID, c.dek)
	if err != nil {
		h.savedViewError(w, "list", err)
		return
	}
	out := make([]savedViewOut, 0, len(views))
	for _, v := range views {
		out = append(out, toSavedViewOut(v))
	}
	writeJSON(w, http.StatusOK, out)
}

// savedViewsCreate serves POST /api/v1/saved-views and answers 201
// with the created row.
func (h *handler) savedViewsCreate(w http.ResponseWriter, r *http.Request) {
	if h.savedViews == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	var in savedViewIn
	if err := decodeBody(r, &in); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "body must be a JSON object: "+err.Error())
		return
	}
	if err := checkName(in.Name); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	if err := checkSavedViewState(in.State); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	v, err := h.savedViews.Create(r.Context(), c.row.ID, repo.SavedViewIn{Name: in.Name, State: in.State}, c.dek)
	if err != nil {
		h.savedViewError(w, "create", err)
		return
	}
	writeJSON(w, http.StatusCreated, toSavedViewOut(v))
}

// savedViewsUpdate serves PATCH /api/v1/saved-views/{id}. A malformed
// id answers 404 (not_found) instead of reaching the DB; a well-formed
// id the caller does not own is also 404 (existence is not disclosed).
// A patch with neither field set is an empty patch.
func (h *handler) savedViewsUpdate(w http.ResponseWriter, r *http.Request) {
	if h.savedViews == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if !uuidShapeOK(id) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "saved view not found")
		return
	}
	var p savedViewPatch
	if err := decodeBody(r, &p); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "body must be a JSON object: "+err.Error())
		return
	}
	if p.Name == nil && p.State == nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "empty patch: provide at least one field")
		return
	}
	if p.Name != nil {
		if err := checkName(*p.Name); err != nil {
			writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
			return
		}
	}
	if p.State != nil {
		if err := checkSavedViewState(*p.State); err != nil {
			writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
			return
		}
	}
	v, err := h.savedViews.Update(r.Context(), c.row.ID, id, repo.SavedViewPatch{Name: p.Name, State: p.State}, c.dek)
	if err != nil {
		h.savedViewError(w, "update", err)
		return
	}
	writeJSON(w, http.StatusOK, toSavedViewOut(v))
}

// savedViewsDelete serves DELETE /api/v1/saved-views/{id} and answers
// 204 (no content) on success. Widgets store their own copy of the
// state, so deleting a saved view never breaks a widget.
func (h *handler) savedViewsDelete(w http.ResponseWriter, r *http.Request) {
	if h.savedViews == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if !uuidShapeOK(id) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "saved view not found")
		return
	}
	if err := h.savedViews.Delete(r.Context(), c.row.ID, id); err != nil {
		h.savedViewError(w, "delete", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

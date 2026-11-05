// Categorise routes (plan 5.4): POST /api/v1/categorise enqueues a
// manual categorisation job for the caller's uncategorised
// transactions (202 Accepted, the job row as JSON); GET
// /api/v1/categorise/{id} reads the caller's job by id (200, or 404 —
// a well-formed id the caller does not own is also 404, existence is
// not disclosed). One active job per user: a second POST while a job
// is pending or running answers 409 conflict. The API never runs the
// job inline — the worker (cmd/worker) claims and drives it.
package server

import (
	"errors"
	"net/http"

	"finance-tracker/internal/repo"

	"github.com/go-chi/chi/v5"
)

// postCategorise serves POST /api/v1/categorise: enqueue one manual
// categorisation job for the caller. 202 with the created job row
// (status "pending"); 409 while one of the caller's jobs is still
// active (pending or running).
func (h *handler) postCategorise(w http.ResponseWriter, r *http.Request) {
	if h.jobs == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	active, err := h.jobs.ActiveExists(r.Context(), c.row.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}
	if active {
		writeAPIError(w, http.StatusConflict, codeConflict, "a categorisation job is already in progress")
		return
	}
	job, err := h.jobs.Create(r.Context(), c.row.ID, "manual", nil)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

// getCategorise serves GET /api/v1/categorise/{id}: the caller's job
// by id, for the SPA to poll until the terminal state. A malformed id
// answers 404 without reaching the DB; a well-formed id the caller
// does not own is also 404 (existence is not disclosed).
func (h *handler) getCategorise(w http.ResponseWriter, r *http.Request) {
	if h.jobs == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if !uuidShapeOK(id) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "categorise job not found")
		return
	}
	job, err := h.jobs.Get(r.Context(), c.row.ID, id)
	if err != nil {
		if errors.Is(err, repo.ErrCategoriseJobNotFound) {
			writeAPIError(w, http.StatusNotFound, codeNotFound, "categorise job not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

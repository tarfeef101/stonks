// Dashboard + widget endpoints (round 6 rework, migration 0013):
// GET/POST /api/v1/dashboards, PATCH/DELETE /api/v1/dashboards/{id},
// GET/POST /api/v1/dashboards/{id}/widgets, PATCH/DELETE
// /api/v1/dashboards/{id}/widgets/{wid} and
// PUT /api/v1/dashboards/{id}/widgets/order over the
// repo.DashboardStore + repo.DashboardWidgetStore. A dashboard is a
// named container of widgets; a widget is kind "explore" (content = the
// canonical explore_state query string, "" = the default all-time view)
// or "markdown" (content = the markdown source). name + content are
// DEK-encrypted at rest by the repo layer; the handlers only ever see
// plaintext in and out. Every store call is scoped to the caller's
// users.id (widget calls to the dashboard too), so a token can only
// ever touch its own rows. The frontend is the source of truth for
// explore_state canonical form (it only ever stores serializeState()
// output), so the server does NOT re-validate it — only the per-kind
// length cap.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"finance-tracker/internal/repo"

	"github.com/go-chi/chi/v5"
)

// DashboardStore is the persistence surface the dashboard routes use.
// *repo.DashboardStore satisfies it; route tests use an in-memory
// implementation.
type DashboardStore interface {
	List(ctx context.Context, userID string, dek []byte) ([]repo.Dashboard, error)
	Create(ctx context.Context, userID string, in repo.DashboardIn, dek []byte) (repo.Dashboard, error)
	Update(ctx context.Context, userID, id string, p repo.DashboardPatch, dek []byte) (repo.Dashboard, error)
	Delete(ctx context.Context, userID, id string) error
}

// DashboardWidgetStore is the persistence surface the widget routes
// use. *repo.DashboardWidgetStore satisfies it; route tests use an
// in-memory implementation.
type DashboardWidgetStore interface {
	List(ctx context.Context, userID, dashboardID string, dek []byte) ([]repo.DashboardWidget, error)
	Get(ctx context.Context, userID, dashboardID, widgetID string, dek []byte) (repo.DashboardWidget, error)
	Create(ctx context.Context, userID, dashboardID string, in repo.DashboardWidgetIn, dek []byte) (repo.DashboardWidget, error)
	Update(ctx context.Context, userID, dashboardID, widgetID string, p repo.DashboardWidgetPatch, dek []byte) (repo.DashboardWidget, error)
	Delete(ctx context.Context, userID, dashboardID, widgetID string) error
	SetOrder(ctx context.Context, userID, dashboardID string, ids []string) error
}

// Widget kinds (0013 CHECK enum) and their content caps. explore_state
// is a few key=value pairs (1024 keeps the encrypted pack small, the
// old card cap); markdown is the user's free text (16384).
const (
	kindExplore  = "explore"
	kindMarkdown = "markdown"

	maxExploreContentLen  = 1024
	maxMarkdownContentLen = 16384

	// maxWidgetRowSpan is the 0014 row_span CHECK ceiling: a widget can
	// occupy at most 8 fixed-height grid rows (taller = scroll inside
	// the card).
	maxWidgetRowSpan = 8
)

// Wire DTOs (RAW, no envelope, NO timestamps — 3.1 convention).
type dashboardOut struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	WidgetCount int    `json:"widget_count"`
}

func toDashboardOut(d repo.Dashboard) dashboardOut {
	return dashboardOut{ID: d.ID, Name: d.Name, WidgetCount: d.WidgetCount}
}

type widgetOut struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Content   string `json:"content"`
	Span      int    `json:"span"`
	RowSpan   int    `json:"row_span"`
	SortOrder int    `json:"sort_order"`
}

func toWidgetOut(w repo.DashboardWidget) widgetOut {
	return widgetOut{
		ID:        w.ID,
		Name:      w.Name,
		Kind:      w.Kind,
		Content:   w.Content,
		Span:      w.Span,
		RowSpan:   w.RowSpan,
		SortOrder: w.SortOrder,
	}
}

// dashboardIn is the POST /v1/dashboards body.
type dashboardIn struct {
	Name string `json:"name"`
}

// dashboardPatch is the PATCH body, mirroring repo.DashboardPatch:
// plain pointers (absent or null = untouched, set = new value).
type dashboardPatch struct {
	Name *string `json:"name"`
}

// widgetIn is the POST widgets body. Content is a plain string: ""
// (absent) IS the default all-time view for explore widgets. Span and
// RowSpan are pointers: absent = the default 6 and the per-kind default
// (explore 4, markdown 2).
type widgetIn struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Content string `json:"content"`
	Span    *int   `json:"span"`
	RowSpan *int   `json:"row_span"`
}

// widgetPatch is the PATCH widgets body, mirroring
// repo.DashboardWidgetPatch: plain pointers (absent or null =
// untouched, set = new value). Unknown JSON fields are ignored by the
// decoder.
type widgetPatch struct {
	Name    *string `json:"name"`
	Kind    *string `json:"kind"`
	Content *string `json:"content"`
	Span    *int    `json:"span"`
	RowSpan *int    `json:"row_span"`
}

// checkWidgetName mirrors repo.validateWidgetName (message strings
// included): like checkName, a name over 255 runes is a 400, but an
// empty name is ALLOWED — a widget may be nameless (its title can live
// in the markdown body; the client shows a placeholder).
func checkWidgetName(name string) error {
	if utf8.RuneCountInString(name) > maxNameLen {
		return fmt.Errorf("name must be at most %d characters", maxNameLen)
	}
	return nil
}

// validKind reports whether kind is one of the 0013 enum values.
func validKind(kind string) bool {
	return kind == kindExplore || kind == kindMarkdown
}

// contentCapFor is the content length cap (runes) for the kind.
func contentCapFor(kind string) int {
	if kind == kindMarkdown {
		return maxMarkdownContentLen
	}
	return maxExploreContentLen
}

// validateWidgetContent enforces the cap for the kind in effect.
// Canonical form is the frontend's business (see the package comment).
func validateWidgetContent(kind, content string) error {
	if utf8.RuneCountInString(content) > contentCapFor(kind) {
		return fmt.Errorf("content must be at most %d characters", contentCapFor(kind))
	}
	return nil
}

// validateSpan enforces the 1..12 grid-column span (0013 CHECK).
func validateSpan(span int) error {
	if span < 1 || span > 12 {
		return errors.New("span must be between 1 and 12")
	}
	return nil
}

// validateRowSpan enforces the 1..maxWidgetRowSpan grid-row span (0014
// CHECK).
func validateRowSpan(rowSpan int) error {
	if rowSpan < 1 || rowSpan > maxWidgetRowSpan {
		return fmt.Errorf("row_span must be between 1 and %d", maxWidgetRowSpan)
	}
	return nil
}

// validateWidgetIn checks every POST field so a bad value is a 400
// before any store call; all problems are collected and joined with
// "; " (the settings convention, like validateCardIn). A non-string
// field never gets here: it fails in decodeBody with "body must be a
// JSON object: ...". An invalid kind is capped as explore (the kind
// problem itself is already in the joined list).
func (h *handler) validateWidgetIn(in *widgetIn) error {
	var problems []string
	if err := checkWidgetName(in.Name); err != nil {
		problems = append(problems, err.Error())
	}
	if !validKind(in.Kind) {
		problems = append(problems, `kind must be "explore" or "markdown"`)
	}
	if err := validateWidgetContent(in.Kind, in.Content); err != nil {
		problems = append(problems, err.Error())
	}
	if in.Span != nil {
		if err := validateSpan(*in.Span); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if in.RowSpan != nil {
		if err := validateRowSpan(*in.RowSpan); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// validateWidgetPatch checks the set PATCH fields the same way, using
// the kind IN EFFECT (the patch's kind when set, else the widget's
// current kind). The content cap applies to the set content, and —
// fetch-then-check — to the EXISTING content when the patch swaps the
// kind without setting content (a long markdown body would violate the
// tighter explore cap).
func (h *handler) validateWidgetPatch(p *widgetPatch, existingKind, existingContent string) error {
	var problems []string
	kindInEffect := existingKind
	if p.Kind != nil {
		if !validKind(*p.Kind) {
			problems = append(problems, `kind must be "explore" or "markdown"`)
		} else {
			kindInEffect = *p.Kind
		}
	}
	if p.Name != nil {
		if err := checkWidgetName(*p.Name); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if p.Content != nil {
		if err := validateWidgetContent(kindInEffect, *p.Content); err != nil {
			problems = append(problems, err.Error())
		}
	} else if p.Kind != nil && validKind(*p.Kind) && *p.Kind != existingKind {
		if err := validateWidgetContent(kindInEffect, existingContent); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if p.Span != nil {
		if err := validateSpan(*p.Span); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if p.RowSpan != nil {
		if err := validateRowSpan(*p.RowSpan); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// dashboardError maps store errors to responses. The sentinel mapping
// is what the frontend keys off; everything else is a 500 with the
// details only in the server log. Both sentinels are 404s with fixed
// messages — existence (and dashboard-vs-widget) is not disclosed.
func (h *handler) dashboardError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, repo.ErrDashboardNotFound):
		writeAPIError(w, http.StatusNotFound, codeNotFound, "dashboard not found")
	case errors.Is(err, repo.ErrDashboardWidgetNotFound):
		writeAPIError(w, http.StatusNotFound, codeNotFound, "dashboard widget not found")
	default:
		slog.Error("dashboard operation failed", "op", op, "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "dashboard operation failed")
	}
}

// dashboardsList serves GET /api/v1/dashboards: the caller's
// dashboards (each with its widget count), created_at asc (id asc
// tiebreak) ([] when empty).
func (h *handler) dashboardsList(w http.ResponseWriter, r *http.Request) {
	if h.dashboards == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	dashboards, err := h.dashboards.List(r.Context(), c.row.ID, c.dek)
	if err != nil {
		h.dashboardError(w, "list", err)
		return
	}
	out := make([]dashboardOut, 0, len(dashboards))
	for _, d := range dashboards {
		out = append(out, toDashboardOut(d))
	}
	writeJSON(w, http.StatusOK, out)
}

// dashboardsCreate serves POST /api/v1/dashboards and answers 201 with
// the created row (widget count 0 — a fresh dashboard is empty).
func (h *handler) dashboardsCreate(w http.ResponseWriter, r *http.Request) {
	if h.dashboards == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	var in dashboardIn
	if err := decodeBody(r, &in); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "body must be a JSON object: "+err.Error())
		return
	}
	if err := checkName(in.Name); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	d, err := h.dashboards.Create(r.Context(), c.row.ID, repo.DashboardIn{Name: in.Name}, c.dek)
	if err != nil {
		h.dashboardError(w, "create", err)
		return
	}
	writeJSON(w, http.StatusCreated, toDashboardOut(d))
}

// dashboardsUpdate serves PATCH /api/v1/dashboards/{id}. A malformed id
// answers 404 (not_found) instead of reaching the DB; a well-formed id
// the caller does not own is also 404 (existence is not disclosed). A
// patch with neither field set is an empty patch.
func (h *handler) dashboardsUpdate(w http.ResponseWriter, r *http.Request) {
	if h.dashboards == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if !uuidShapeOK(id) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "dashboard not found")
		return
	}
	var p dashboardPatch
	if err := decodeBody(r, &p); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "body must be a JSON object: "+err.Error())
		return
	}
	if p.Name == nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "empty patch: provide at least one field")
		return
	}
	if err := checkName(*p.Name); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	d, err := h.dashboards.Update(r.Context(), c.row.ID, id, repo.DashboardPatch{Name: p.Name}, c.dek)
	if err != nil {
		h.dashboardError(w, "update", err)
		return
	}
	writeJSON(w, http.StatusOK, toDashboardOut(d))
}

// dashboardsDelete serves DELETE /api/v1/dashboards/{id} and answers
// 204 (no content) on success; the dashboard's widgets cascade (0013
// FK).
func (h *handler) dashboardsDelete(w http.ResponseWriter, r *http.Request) {
	if h.dashboards == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if !uuidShapeOK(id) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "dashboard not found")
		return
	}
	if err := h.dashboards.Delete(r.Context(), c.row.ID, id); err != nil {
		h.dashboardError(w, "delete", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// widgetsList serves GET /api/v1/dashboards/{id}/widgets: the
// dashboard's widgets in sort_order order ([] for a fresh dashboard).
func (h *handler) widgetsList(w http.ResponseWriter, r *http.Request) {
	if h.widgets == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	dashID := chi.URLParam(r, "id")
	if !uuidShapeOK(dashID) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "dashboard not found")
		return
	}
	widgets, err := h.widgets.List(r.Context(), c.row.ID, dashID, c.dek)
	if err != nil {
		h.dashboardError(w, "list widgets", err)
		return
	}
	out := make([]widgetOut, 0, len(widgets))
	for _, w := range widgets {
		out = append(out, toWidgetOut(w))
	}
	writeJSON(w, http.StatusOK, out)
}

// widgetsCreate serves POST /api/v1/dashboards/{id}/widgets and
// answers 201 with the created row (appended at the end of the grid;
// an absent span is the default 6, an absent row_span the per-kind
// default — explore 4, markdown 2).
func (h *handler) widgetsCreate(w http.ResponseWriter, r *http.Request) {
	if h.widgets == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	dashID := chi.URLParam(r, "id")
	if !uuidShapeOK(dashID) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "dashboard not found")
		return
	}
	var in widgetIn
	if err := decodeBody(r, &in); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "body must be a JSON object: "+err.Error())
		return
	}
	if err := h.validateWidgetIn(&in); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	span := 6
	if in.Span != nil {
		span = *in.Span
	}
	rowSpan := 4
	if in.Kind == kindMarkdown {
		rowSpan = 2
	}
	if in.RowSpan != nil {
		rowSpan = *in.RowSpan
	}
	created, err := h.widgets.Create(r.Context(), c.row.ID, dashID, repo.DashboardWidgetIn{
		Name:    in.Name,
		Kind:    in.Kind,
		Content: in.Content,
		Span:    span,
		RowSpan: rowSpan,
	}, c.dek)
	if err != nil {
		h.dashboardError(w, "create widget", err)
		return
	}
	writeJSON(w, http.StatusCreated, toWidgetOut(created))
}

// widgetsUpdate serves PATCH /api/v1/dashboards/{id}/widgets/{wid}.
// The id checks run before the body (a malformed dashboard id is a
// dashboard 404, a malformed widget id a widget 404, before any decode
// work); a patch with no fields set is an empty patch; the validation
// uses the kind in effect (the patch's kind when set, else the
// widget's), so a kind swap without content re-checks the EXISTING
// content against the new kind's cap (fetch-then-check).
func (h *handler) widgetsUpdate(w http.ResponseWriter, r *http.Request) {
	if h.widgets == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	dashID := chi.URLParam(r, "id")
	if !uuidShapeOK(dashID) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "dashboard not found")
		return
	}
	wid := chi.URLParam(r, "wid")
	if !uuidShapeOK(wid) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "dashboard widget not found")
		return
	}
	var p widgetPatch
	if err := decodeBody(r, &p); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "body must be a JSON object: "+err.Error())
		return
	}
	if p.Name == nil && p.Kind == nil && p.Content == nil && p.Span == nil && p.RowSpan == nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "empty patch: provide at least one field")
		return
	}
	cur, err := h.widgets.Get(r.Context(), c.row.ID, dashID, wid, c.dek)
	if err != nil {
		h.dashboardError(w, "get widget", err)
		return
	}
	if err := h.validateWidgetPatch(&p, cur.Kind, cur.Content); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	upd, err := h.widgets.Update(r.Context(), c.row.ID, dashID, wid, repo.DashboardWidgetPatch{
		Name:    p.Name,
		Kind:    p.Kind,
		Content: p.Content,
		Span:    p.Span,
		RowSpan: p.RowSpan,
	}, c.dek)
	if err != nil {
		h.dashboardError(w, "update widget", err)
		return
	}
	writeJSON(w, http.StatusOK, toWidgetOut(upd))
}

// widgetsDelete serves DELETE /api/v1/dashboards/{id}/widgets/{wid}
// and answers 204 (no content) on success. A widget id that is not in
// the named dashboard is 404 (cross-dashboard writes are "not found",
// never 400).
func (h *handler) widgetsDelete(w http.ResponseWriter, r *http.Request) {
	if h.widgets == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	dashID := chi.URLParam(r, "id")
	if !uuidShapeOK(dashID) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "dashboard not found")
		return
	}
	wid := chi.URLParam(r, "wid")
	if !uuidShapeOK(wid) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "dashboard widget not found")
		return
	}
	if err := h.widgets.Delete(r.Context(), c.row.ID, dashID, wid); err != nil {
		h.dashboardError(w, "delete widget", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// widgetsSetOrder serves PUT /api/v1/dashboards/{id}/widgets/order.
// The body is a BARE JSON array of the dashboard's widget uuids, every
// one exactly once, in the new order; the new sort_order of ids[i] is
// i+1. The checks run in this exact order (mirrors the cards pin):
//  1. dashboard id not uuidShapeOK → 404 "dashboard not found";
//  2. body not a JSON array → 400 "body must be a JSON array: "+err
//     (json.NewDecoder directly — decodeBody is for objects);
//  3. any element not uuidShapeOK → 400 "order must contain only
//     widget ids";
//  4. List (a dashboard id that is not the caller's → 404 "dashboard
//     not found" via the sentinel);
//  5. any id not among the dashboard's widgets → 404 "dashboard widget
//     not found" (existence is not disclosed, so the id is NOT echoed);
//  6. count mismatch or duplicate → 400 "order must list every widget
//     exactly once" (an empty array against a zero-widget dashboard IS
//     valid);
//  7. SetOrder → 204.
func (h *handler) widgetsSetOrder(w http.ResponseWriter, r *http.Request) {
	if h.widgets == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	dashID := chi.URLParam(r, "id")
	if !uuidShapeOK(dashID) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "dashboard not found")
		return
	}
	var ids []string
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&ids); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "body must be a JSON array: "+err.Error())
		return
	}
	for _, id := range ids {
		if !uuidShapeOK(id) {
			writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "order must contain only widget ids")
			return
		}
	}
	widgets, err := h.widgets.List(r.Context(), c.row.ID, dashID, c.dek)
	if err != nil {
		h.dashboardError(w, "list widgets", err)
		return
	}
	owned := make(map[string]bool, len(widgets))
	for _, w := range widgets {
		owned[w.ID] = true
	}
	for _, id := range ids {
		if !owned[id] {
			writeAPIError(w, http.StatusNotFound, codeNotFound, "dashboard widget not found")
			return
		}
	}
	if len(ids) != len(widgets) {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "order must list every widget exactly once")
		return
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "order must list every widget exactly once")
			return
		}
		seen[id] = true
	}
	if err := h.widgets.SetOrder(r.Context(), c.row.ID, dashID, ids); err != nil {
		h.dashboardError(w, "set widget order", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

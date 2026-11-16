// Notification endpoints (plan step 3.3): GET /api/v1/notifications
// (unread/limit/offset filters), POST /api/v1/notifications/{id}/read
// (idempotent, 200 with the updated row) and
// GET /api/v1/notifications/unread-count over the repo.NotificationStore.
// Notifications are app-written (budget alerts, step 8.x) and user-owned;
// every store call is scoped to the caller's users.id.
//
// Round 33 adds the self-heals: the list endpoint first reconciles the
// lazy rows to the live state (the review queue count, and the live
// aberrant items per mortgage) so only still-relevant notifications are
// served; the review mutations reconcile on the way out (review.go).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"finance-tracker/internal/categorize"
	"finance-tracker/internal/repo"

	"github.com/go-chi/chi/v5"
)

// NotificationStore is the persistence surface the notification handlers
// need. *repo.NotificationStore satisfies it; Create and FindByKindRef
// serve the lazy notification creation on the mortgage matching report
// (round 32), and ListByKind / Update / Delete serve the round-33
// self-heals that refresh or purge stale rows. Route tests use an
// in-memory implementation.
type NotificationStore interface {
	List(ctx context.Context, userID string, f repo.NotificationFilter) ([]repo.Notification, error)
	ListByKind(ctx context.Context, userID, kind string) ([]repo.Notification, error)
	MarkRead(ctx context.Context, userID, id string) (repo.Notification, error)
	CountUnread(ctx context.Context, userID string) (int, error)
	Create(ctx context.Context, userID string, in repo.NotificationIn) (repo.Notification, error)
	Update(ctx context.Context, userID, id string, in repo.NotificationUpdate) (repo.Notification, error)
	Delete(ctx context.Context, userID, id string) error
	FindByKindRef(ctx context.Context, userID, kind string, ref []byte) (repo.Notification, error)
}

// notificationOut is the wire row. ReadAt is null while the
// notification is unread; Ref is the raw pointer document (null when
// unset). CreatedAt is bookkeeping and excluded (3.1 convention); the
// list order (created_at DESC) carries the freshness signal.
type notificationOut struct {
	ID     string          `json:"id"`
	Kind   string          `json:"kind"`
	Title  string          `json:"title"`
	Body   string          `json:"body"`
	Ref    json.RawMessage `json:"ref"`
	ReadAt *time.Time      `json:"read_at"`
}

func toNotificationOut(n repo.Notification) notificationOut {
	return notificationOut{
		ID:     n.ID,
		Kind:   n.Kind,
		Title:  n.Title,
		Body:   n.Body,
		Ref:    n.Ref,
		ReadAt: n.ReadAt,
	}
}

// unreadCountOut is the GET /api/v1/notifications/unread-count body.
type unreadCountOut struct {
	Unread int `json:"unread"`
}

// notificationFilterFromQuery parses the list query params: unread is a
// boolean, limit/offset integers >= 0 (the tx filter convention; the
// repo then applies the default/cap). Empty-string params are treated
// as absent.
func notificationFilterFromQuery(q url.Values) (repo.NotificationFilter, error) {
	var f repo.NotificationFilter
	if s := q.Get("unread"); s != "" {
		b, err := strconv.ParseBool(s)
		if err != nil {
			return f, errors.New("unread must be a boolean")
		}
		f.Unread = b
	}
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

// notificationError maps store errors to responses. The sentinel mapping
// is what the frontend keys off; everything else is a 500 with the
// details only in the server log.
func (h *handler) notificationError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, repo.ErrNotificationNotFound):
		writeAPIError(w, http.StatusNotFound, codeNotFound, "notification not found")
	default:
		slog.Error("notification operation failed", "op", op, "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "notification operation failed")
	}
}

// reviewNotificationJobID recovers the job_id from a review notification
// ref (round 33); "" when the ref is unset or does not carry one.
func reviewNotificationJobID(ref json.RawMessage) string {
	if len(ref) == 0 {
		return ""
	}
	var r struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(ref, &r); err != nil {
		return ""
	}
	return r.JobID
}

// syncReviewNotification reconciles the "N transactions need review"
// notification to the live review queue (round 33, best-effort): the
// queue count is authoritative — zero pending rows purge EVERY "review"
// row, a positive count keeps exactly one row (the newest, refreshed to
// the live count and ref) and purges the duplicates, and a positive
// count with no row creates one. Failures are logged only: a
// notification write must never change the response of the mutation
// that triggered the sync.
func (h *handler) syncReviewNotification(ctx context.Context, c caller) {
	if h.review == nil || h.notifications == nil {
		return
	}
	log := func(op string, err error) {
		slog.Error("review notification reconcile failed", "op", op, "error", err)
	}
	q, err := h.review.CountReviewPending(ctx, c.row.ID)
	if err != nil {
		log("count", err)
		return
	}
	rows, err := h.notifications.ListByKind(ctx, c.row.ID, categorize.KindReview)
	if err != nil {
		log("list", err)
		return
	}
	delete := func(id string) {
		if derr := h.notifications.Delete(ctx, c.row.ID, id); derr != nil && !errors.Is(derr, repo.ErrNotificationNotFound) {
			slog.Error("review notification reconcile failed", "op", "delete", "notification", id, "error", derr)
		}
	}
	if q == 0 {
		for _, n := range rows {
			delete(n.ID)
		}
		return
	}
	title := categorize.ReviewNotificationTitle(q)
	if len(rows) == 0 {
		if _, cerr := h.notifications.Create(ctx, c.row.ID, repo.NotificationIn{
			Kind:  categorize.KindReview,
			Title: title,
			Body:  title,
			Ref:   categorize.ReviewNotificationRef(q, ""),
		}); cerr != nil {
			log("create", cerr)
		}
		return
	}
	// Keep the newest row (ListByKind is newest-first), refresh it to
	// the live count, and purge the duplicates.
	keep := rows[0]
	for _, n := range rows[1:] {
		delete(n.ID)
	}
	if _, uerr := h.notifications.Update(ctx, c.row.ID, keep.ID, repo.NotificationUpdate{
		Title: title,
		Body:  title,
		Ref:   categorize.ReviewNotificationRef(q, reviewNotificationJobID(keep.Ref)),
	}); uerr != nil {
		log("update", uerr)
	}
}

// notificationsList serves GET /api/v1/notifications: a page of the
// caller's notifications, newest first (repo contract). The round-33
// self-heals run BEFORE the list so the response never serves a stale
// row.
func (h *handler) notificationsList(w http.ResponseWriter, r *http.Request) {
	if h.notifications == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	h.syncReviewNotification(r.Context(), c)
	h.syncAberrantNotifications(r.Context(), c)
	f, err := notificationFilterFromQuery(r.URL.Query())
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	nots, err := h.notifications.List(r.Context(), c.row.ID, f)
	if err != nil {
		h.notificationError(w, "list", err)
		return
	}
	out := make([]notificationOut, 0, len(nots))
	for _, n := range nots {
		out = append(out, toNotificationOut(n))
	}
	writeJSON(w, http.StatusOK, out)
}

// notificationsMarkRead serves POST /api/v1/notifications/{id}/read and
// answers 200 with the updated row. It is idempotent: marking an
// already-read row again keeps the original read_at. A malformed id
// answers 404 (not_found) instead of reaching the DB; a well-formed id
// the caller does not own is also 404 (existence is not disclosed).
func (h *handler) notificationsMarkRead(w http.ResponseWriter, r *http.Request) {
	if h.notifications == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if !uuidShapeOK(id) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "notification not found")
		return
	}
	n, err := h.notifications.MarkRead(r.Context(), c.row.ID, id)
	if err != nil {
		h.notificationError(w, "mark-read", err)
		return
	}
	writeJSON(w, http.StatusOK, toNotificationOut(n))
}

// notificationsUnreadCount serves GET
// /api/v1/notifications/unread-count: the count of the caller's unread
// notifications (the browser badge source).
func (h *handler) notificationsUnreadCount(w http.ResponseWriter, r *http.Request) {
	if h.notifications == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	n, err := h.notifications.CountUnread(r.Context(), c.row.ID)
	if err != nil {
		h.notificationError(w, "count-unread", err)
		return
	}
	writeJSON(w, http.StatusOK, unreadCountOut{Unread: n})
}

// Notification endpoints (plan step 3.3): GET /api/v1/notifications
// (unread/limit/offset filters), POST /api/v1/notifications/{id}/read
// (idempotent, 200 with the updated row) and
// GET /api/v1/notifications/unread-count over the repo.NotificationStore.
// Notifications are app-written (budget alerts, step 8.x) and user-owned;
// every store call is scoped to the caller's users.id.
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

	"finance-tracker/internal/repo"

	"github.com/go-chi/chi/v5"
)

// NotificationStore is the persistence surface the notification handlers
// need. *repo.NotificationStore satisfies it (its extra Create/Delete
// are unused here); route tests use an in-memory implementation.
type NotificationStore interface {
	List(ctx context.Context, userID string, f repo.NotificationFilter) ([]repo.Notification, error)
	MarkRead(ctx context.Context, userID, id string) (repo.Notification, error)
	CountUnread(ctx context.Context, userID string) (int, error)
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

// notificationsList serves GET /api/v1/notifications: a page of the
// caller's notifications, newest first (repo contract).
func (h *handler) notificationsList(w http.ResponseWriter, r *http.Request) {
	if h.notifications == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
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

// Per-user notification repository (plan step 2.6, migration 0001).
//
// Plaintext by design — no DEK: a notification is an in-app event
// (e.g. "3 transactions need review", plan 5.5) carrying a pointer
// (ref jsonb) into other tables, never account/transaction content.
// The 0001 unread index is (user_id, created_at DESC) WHERE
// read_at IS NULL; List with Unread=true matches it (created_at DESC,
// id DESC).
//
// read_at is set once: MarkRead is idempotent and COALESCE keeps the
// FIRST read timestamp (0001 CHECK: read_at >= created_at holds
// trivially, but the semantics are "when did it become read").
package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Field caps: the API 400s before the DB sees oversized rows (the
// columns are unbounded text/jsonb).
const (
	maxNotificationKindLen  = 64
	maxNotificationTitleLen = 200
	maxNotificationBodyLen  = 4000
	maxNotificationRefBytes = 64 * 1024
)

// defaultNotificationLimit / maxNotificationLimit bound
// NotificationFilter.Limit (same convention as transactions).
const (
	defaultNotificationLimit = 50
	maxNotificationLimit     = 100
)

// Notification is one notifications row. Ref is the raw jsonb pointer
// (nil when unset). ReadAt is nil while the notification is unread.
type Notification struct {
	ID        string
	Kind      string
	Title     string
	Body      string
	Ref       json.RawMessage
	ReadAt    *time.Time
	CreatedAt time.Time
}

// NotificationIn is the Create input. Kind/Title/Body are trimmed and
// must be non-blank. Ref is nil or "" = unset (NULL), otherwise a
// valid JSON document.
type NotificationIn struct {
	Kind  string
	Title string
	Body  string
	Ref   json.RawMessage
}

// NotificationUpdate is the Update input: it replaces title and body
// (the self-heals refresh the kept row's live count) and optionally the
// ref. Kind and ReadAt are not mutable here. Title/Body are trimmed and
// must be non-blank; Ref is nil or "" = unset (NULL), otherwise a valid
// JSON document.
type NotificationUpdate struct {
	Title string
	Body  string
	Ref   json.RawMessage
}

// NotificationFilter is the List query. Unread = unread rows only.
// Limit defaults to 50, capped at 100. Rows come back ordered
// created_at DESC, id DESC.
type NotificationFilter struct {
	Unread bool
	Limit  int
	Offset int
}

// NotificationStore persists notifications. One store serves every
// user; each method takes a userID and scopes its SQL on it.
type NotificationStore struct {
	pool *pgxpool.Pool
}

// NewNotificationStore returns a NotificationStore backed by pool (nil
// for a nil pool, mirroring the 1.3c UserStore constructor).
func NewNotificationStore(pool *pgxpool.Pool) *NotificationStore {
	if pool == nil {
		return nil
	}
	return &NotificationStore{pool: pool}
}

// validateNotification checks the plaintext input fields (in place:
// kind/title/body come back trimmed).
func validateNotification(in *NotificationIn) error {
	in.Kind = strings.TrimSpace(in.Kind)
	in.Title = strings.TrimSpace(in.Title)
	in.Body = strings.TrimSpace(in.Body)
	if in.Kind == "" {
		return errors.New("kind must not be empty")
	}
	if utf8.RuneCountInString(in.Kind) > maxNotificationKindLen {
		return fmt.Errorf("kind must be at most %d characters", maxNotificationKindLen)
	}
	if in.Title == "" {
		return errors.New("title must not be empty")
	}
	if utf8.RuneCountInString(in.Title) > maxNotificationTitleLen {
		return fmt.Errorf("title must be at most %d characters", maxNotificationTitleLen)
	}
	if in.Body == "" {
		return errors.New("body must not be empty")
	}
	if utf8.RuneCountInString(in.Body) > maxNotificationBodyLen {
		return fmt.Errorf("body must be at most %d characters", maxNotificationBodyLen)
	}
	if len(in.Ref) > 0 {
		if !json.Valid(in.Ref) {
			return errors.New("ref must be a valid JSON document")
		}
		if len(in.Ref) > maxNotificationRefBytes {
			return fmt.Errorf("ref must be at most %d bytes", maxNotificationRefBytes)
		}
	}
	return nil
}

// notificationFrom assembles a Notification from a row's columns. Ref
// scans as []byte (jsonb gotcha, same pin as import-schemas spec);
// read_at as sql.NullTime -> *time.Time.
func notificationFrom(id string, kind, title, body string, ref []byte, readAt sql.NullTime, createdAt time.Time) Notification {
	n := Notification{ID: id, Kind: kind, Title: title, Body: body, CreatedAt: createdAt}
	if len(ref) > 0 {
		n.Ref = json.RawMessage(ref)
	}
	if readAt.Valid {
		t := readAt.Time
		n.ReadAt = &t
	}
	return n
}

// Create inserts a notification for the user and returns the stored
// row (jsonb round-trips normalized, so the answer is a Get, not the
// input).
func (s *NotificationStore) Create(ctx context.Context, userID string, in NotificationIn) (Notification, error) {
	if err := validateNotification(&in); err != nil {
		return Notification{}, fmt.Errorf("repo: %w", err)
	}
	var refArg any // nil = NULL; jsonb binds as string (pgx gotcha)
	if len(in.Ref) > 0 {
		refArg = string(in.Ref)
	}
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO notifications (user_id, kind, title, body, ref)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id`,
		userID, in.Kind, in.Title, in.Body, refArg).Scan(&id)
	if err != nil {
		return Notification{}, fmt.Errorf("repo: create notification: %w", err)
	}
	return s.Get(ctx, userID, id)
}

// Get returns one of the user's notifications, or
// ErrNotificationNotFound when the id is not the user's (existence is
// not disclosed across users).
func (s *NotificationStore) Get(ctx context.Context, userID, id string) (Notification, error) {
	var (
		kind, title, body string
		ref               []byte
		readAt            sql.NullTime
		createdAt         time.Time
	)
	err := s.pool.QueryRow(ctx, `
		SELECT kind, title, body, ref, read_at, created_at
		FROM notifications
		WHERE user_id = $1 AND id = $2`, userID, id).
		Scan(&kind, &title, &body, &ref, &readAt, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Notification{}, ErrNotificationNotFound
	}
	if err != nil {
		return Notification{}, fmt.Errorf("repo: get notification: %w", err)
	}
	return notificationFrom(id, kind, title, body, ref, readAt, createdAt), nil
}

// FindByKindRef returns one of the user's notifications of kind whose
// ref is jsonb-equal to ref (any read state), or
// ErrNotificationNotFound when there is none. The ref comparison is
// jsonb equality (key order irrelevant), which is what round 32 needs
// to dedup its lazily-created mortgage-aberrant notifications on the
// (kind, ref) pair.
func (s *NotificationStore) FindByKindRef(ctx context.Context, userID, kind string, ref []byte) (Notification, error) {
	var (
		id, title, body string
		refCol          []byte
		readAt          sql.NullTime
		createdAt       time.Time
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id, title, body, ref, read_at, created_at
		FROM notifications
		WHERE user_id = $1 AND kind = $2 AND ref = $3::jsonb
		ORDER BY created_at DESC, id DESC
		LIMIT 1`, userID, kind, string(ref)).
		Scan(&id, &title, &body, &refCol, &readAt, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Notification{}, ErrNotificationNotFound
	}
	if err != nil {
		return Notification{}, fmt.Errorf("repo: find notification by kind/ref: %w", err)
	}
	return notificationFrom(id, kind, title, body, refCol, readAt, createdAt), nil
}

// List returns a page of the user's notifications (see
// NotificationFilter).
func (s *NotificationStore) List(ctx context.Context, userID string, f NotificationFilter) ([]Notification, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultNotificationLimit
	}
	if limit > maxNotificationLimit {
		limit = maxNotificationLimit
	}
	if f.Offset < 0 {
		f.Offset = 0
	}

	where := []string{"user_id = $1"}
	args := []any{userID}
	if f.Unread {
		where = append(where, "read_at IS NULL")
	}

	rows, err := s.pool.Query(ctx, `
		SELECT id, kind, title, body, ref, read_at, created_at
		FROM notifications
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY created_at DESC, id DESC
		LIMIT `+fmt.Sprint(limit)+` OFFSET `+fmt.Sprint(f.Offset), args...)
	if err != nil {
		return nil, fmt.Errorf("repo: list notifications: %w", err)
	}
	defer rows.Close()
	out := make([]Notification, 0, limit)
	for rows.Next() {
		var (
			id, kind, title, body string
			ref                   []byte
			readAt                sql.NullTime
			createdAt             time.Time
		)
		if err := rows.Scan(&id, &kind, &title, &body, &ref, &readAt, &createdAt); err != nil {
			return nil, fmt.Errorf("repo: scan notification: %w", err)
		}
		out = append(out, notificationFrom(id, kind, title, body, ref, readAt, createdAt))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo: list notifications: %w", err)
	}
	return out, nil
}

// ListByKind returns EVERY notification of the user's with the given
// kind (no limit: the kinds in use hold at most a handful of rows),
// newest first (the List ordering). The round-33 self-heals walk these
// to drop stale rows.
func (s *NotificationStore) ListByKind(ctx context.Context, userID, kind string) ([]Notification, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, kind, title, body, ref, read_at, created_at
		FROM notifications
		WHERE user_id = $1 AND kind = $2
		ORDER BY created_at DESC, id DESC`, userID, kind)
	if err != nil {
		return nil, fmt.Errorf("repo: list notifications by kind: %w", err)
	}
	defer rows.Close()
	out := make([]Notification, 0)
	for rows.Next() {
		var (
			id, k, title, body string
			ref                []byte
			readAt             sql.NullTime
			createdAt          time.Time
		)
		if err := rows.Scan(&id, &k, &title, &body, &ref, &readAt, &createdAt); err != nil {
			return nil, fmt.Errorf("repo: scan notification: %w", err)
		}
		out = append(out, notificationFrom(id, k, title, body, ref, readAt, createdAt))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo: list notifications by kind: %w", err)
	}
	return out, nil
}

// MarkRead sets read_at (first read wins: COALESCE keeps the original
// timestamp) and returns the updated row. Idempotent — marking an
// already-read notification leaves read_at untouched. An unknown or
// other user's id is ErrNotificationNotFound.
func (s *NotificationStore) MarkRead(ctx context.Context, userID, id string) (Notification, error) {
	cmdTag, err := s.pool.Exec(ctx, `
		UPDATE notifications
		SET read_at = COALESCE(read_at, now())
		WHERE user_id = $1 AND id = $2`, userID, id)
	if err != nil {
		return Notification{}, fmt.Errorf("repo: mark notification read: %w", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return Notification{}, ErrNotificationNotFound
	}
	return s.Get(ctx, userID, id)
}

// CountUnread returns the number of the user's unread notifications
// (the 0001 partial index answers it).
func (s *NotificationStore) CountUnread(ctx context.Context, userID string) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM notifications WHERE user_id = $1 AND read_at IS NULL",
		userID).Scan(&n); err != nil {
		return 0, fmt.Errorf("repo: count unread notifications: %w", err)
	}
	return n, nil
}

// Update replaces a notification's title, body, and ref in place (the
// round-33 self-heal refreshes the kept review row's live count). The
// kind is not mutable here. Same field validation as Create; Ref is
// nil or "" = unset (NULL). An unknown or other user's id is
// ErrNotificationNotFound; the row is left untouched. Returns the
// updated row (a Get, as in Create).
func (s *NotificationStore) Update(ctx context.Context, userID, id string, in NotificationUpdate) (Notification, error) {
	in.Title = strings.TrimSpace(in.Title)
	in.Body = strings.TrimSpace(in.Body)
	if in.Title == "" {
		return Notification{}, fmt.Errorf("repo: title must not be empty")
	}
	if utf8.RuneCountInString(in.Title) > maxNotificationTitleLen {
		return Notification{}, fmt.Errorf("repo: title must be at most %d characters", maxNotificationTitleLen)
	}
	if in.Body == "" {
		return Notification{}, fmt.Errorf("repo: body must not be empty")
	}
	if utf8.RuneCountInString(in.Body) > maxNotificationBodyLen {
		return Notification{}, fmt.Errorf("repo: body must be at most %d characters", maxNotificationBodyLen)
	}
	if len(in.Ref) > 0 {
		if !json.Valid(in.Ref) {
			return Notification{}, fmt.Errorf("repo: ref must be a valid JSON document")
		}
		if len(in.Ref) > maxNotificationRefBytes {
			return Notification{}, fmt.Errorf("repo: ref must be at most %d bytes", maxNotificationRefBytes)
		}
	}
	var refArg any // nil = NULL; jsonb binds as string (pgx gotcha)
	if len(in.Ref) > 0 {
		refArg = string(in.Ref)
	}
	cmdTag, err := s.pool.Exec(ctx, `
		UPDATE notifications
		SET title = $3, body = $4, ref = $5
		WHERE user_id = $1 AND id = $2`, userID, id, in.Title, in.Body, refArg)
	if err != nil {
		return Notification{}, fmt.Errorf("repo: update notification: %w", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return Notification{}, ErrNotificationNotFound
	}
	return s.Get(ctx, userID, id)
}

// Delete removes the user's notification. An unknown or other user's
// id is ErrNotificationNotFound; the row is left untouched.
func (s *NotificationStore) Delete(ctx context.Context, userID, id string) error {
	cmdTag, err := s.pool.Exec(ctx, `
		DELETE FROM notifications
		WHERE user_id = $1 AND id = $2`, userID, id)
	if err != nil {
		return fmt.Errorf("repo: delete notification: %w", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return ErrNotificationNotFound
	}
	return nil
}

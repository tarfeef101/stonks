// Per-user dashboards + widgets repository (round 6 rework, migration
// 0013): the single implicit dashboard (0008's dashboard_cards) becomes a
// browsable multi-dashboard area. dashboards are named containers (one
// DEK-encrypted pack for the user-authored name, same convention as
// accounts/categories/budgets); dashboard_widgets are the grid items —
// kind 'explore' (content = the codec's canonical explore_state query
// string, "" = the default all-time view) or 'markdown' (content = the
// markdown source), name AND content each ONE DEK-encrypted pack (content
// can carry the free-text q search term, like explore_state did). span is
// the 1..12 grid column span; row_span is the 1..8 fixed-row span (0014 —
// the editor's grid has fixed-height rows, so a widget whose content
// overflows its row_span scrolls inside the card); sort_order is the
// display order, rewritten atomically by SetOrder (the PUT /order
// endpoint); new rows take MAX(sort_order)+1 over the dashboard. Like the
// old cards there is NO unique constraint on names (duplicate labels
// allowed).
//
// Backfill is the one-time app-level data migration that moves the old
// dashboard_cards rows in (DEK encryption is app-layer — SQL cannot
// produce the ciphertext) and owns the DROP of the old table.
package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"finance-tracker/internal/crypto"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Dashboard is one dashboards row with its name opened. WidgetCount is
// the number of widgets in the dashboard (the list endpoint shows it).
type Dashboard struct {
	ID          string
	Name        string
	WidgetCount int
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// DashboardIn is the Create input.
type DashboardIn struct {
	Name string
}

// DashboardPatch is the Update input; nil = untouched (absent or null in
// the PATCH body, like BudgetPatch).
type DashboardPatch struct {
	Name *string
}

// Dashboard is one dashboard_widgets row with its encrypted fields
// opened. Kind is "explore" or "markdown" (CHECK-enum'd in 0013);
// Content is the explore_state query string or the markdown source
// ("" = the default all-time view for explore widgets).
type DashboardWidget struct {
	ID        string
	Name      string
	Kind      string
	Content   string
	Span      int
	RowSpan   int
	SortOrder int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// DashboardWidgetIn is the Create input. Span 0 (absent) means the
// default 6; RowSpan 0 (absent) means the per-kind default (explore 4,
// markdown 2 — defaultRowSpan). Kind is the server-validated enum value;
// the server has already applied the content cap for the kind.
type DashboardWidgetIn struct {
	Name    string
	Kind    string
	Content string
	Span    int
	RowSpan int
}

// DashboardWidgetPatch is the Update input; nil = untouched.
type DashboardWidgetPatch struct {
	Name    *string
	Kind    *string
	Content *string
	Span    *int
	RowSpan *int
}

// DashboardStore persists dashboards. One store serves every user; each
// method takes a userID and scopes its SQL on it. Deleting a dashboard
// cascades to its widgets (0013 FK).
type DashboardStore struct {
	pool *pgxpool.Pool
}

// NewDashboardStore returns a DashboardStore backed by pool (nil for a
// nil pool, mirroring the NewBudgetStore constructor).
func NewDashboardStore(pool *pgxpool.Pool) *DashboardStore {
	if pool == nil {
		return nil
	}
	return &DashboardStore{pool: pool}
}

// decrypt opens the name pack. A pack that does not open is a key/data
// mismatch the API surfaces as 500 (the DEK came from the user's own
// wrapped_dek, so this is corruption, not a client error) — same rule as
// the other stores.
func (d *Dashboard) decrypt(namePack []byte, dek []byte) error {
	name, err := crypto.Open(dek, namePack)
	if err != nil {
		return fmt.Errorf("repo: decrypt dashboard %s name: %w", d.ID, err)
	}
	d.Name = string(name)
	return nil
}

// getByID returns one of the user's dashboards (with its widget count) or
// ErrDashboardNotFound when the id is not the user's (existence is not
// disclosed across users).
func (s *DashboardStore) getByID(ctx context.Context, userID, id string, dek []byte) (Dashboard, error) {
	var (
		d        Dashboard
		namePack []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT d.name_encrypted, COUNT(w.id), d.created_at, d.updated_at
		FROM dashboards d
		LEFT JOIN dashboard_widgets w ON w.dashboard_id = d.id
		WHERE d.user_id = $1 AND d.id = $2
		GROUP BY d.name_encrypted, d.created_at, d.updated_at`, userID, id).
		Scan(&namePack, &d.WidgetCount, &d.CreatedAt, &d.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Dashboard{}, ErrDashboardNotFound
	}
	if err != nil {
		return Dashboard{}, fmt.Errorf("repo: get dashboard: %w", err)
	}
	d.ID = id
	if err := d.decrypt(namePack, dek); err != nil {
		return Dashboard{}, err
	}
	return d, nil
}

// List returns the user's dashboards (each with its widget count)
// ordered by created_at, id as the tiebreak. dek decrypts names.
func (s *DashboardStore) List(ctx context.Context, userID string, dek []byte) ([]Dashboard, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT d.id, d.name_encrypted, COUNT(w.id), d.created_at, d.updated_at
		FROM dashboards d
		LEFT JOIN dashboard_widgets w ON w.dashboard_id = d.id
		WHERE d.user_id = $1
		GROUP BY d.id, d.name_encrypted, d.created_at, d.updated_at
		ORDER BY d.created_at, d.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("repo: list dashboards: %w", err)
	}
	defer rows.Close()
	out := make([]Dashboard, 0, 4)
	for rows.Next() {
		var (
			d        Dashboard
			namePack []byte
		)
		if err := rows.Scan(&d.ID, &namePack, &d.WidgetCount, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, fmt.Errorf("repo: scan dashboard: %w", err)
		}
		if err := d.decrypt(namePack, dek); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo: list dashboards: %w", err)
	}
	return out, nil
}

// Create inserts a dashboard for the user and returns it (widget count
// 0 — a fresh dashboard has no widgets).
func (s *DashboardStore) Create(ctx context.Context, userID string, in DashboardIn, dek []byte) (Dashboard, error) {
	if err := validateName(in.Name); err != nil {
		return Dashboard{}, fmt.Errorf("repo: %w", err)
	}
	namePack, err := crypto.Seal(dek, []byte(strings.TrimSpace(in.Name)))
	if err != nil {
		return Dashboard{}, fmt.Errorf("repo: seal dashboard name: %w", err)
	}
	var id string
	err = s.pool.QueryRow(ctx, `
		INSERT INTO dashboards (user_id, name_encrypted)
		VALUES ($1, $2)
		RETURNING id`, userID, namePack).Scan(&id)
	if err != nil {
		return Dashboard{}, fmt.Errorf("repo: create dashboard: %w", err)
	}
	return s.getByID(ctx, userID, id, dek)
}

// Update applies the non-nil fields of p to the user's dashboard and
// returns the updated row. A renamed name is re-sealed under the same
// DEK. An unchanged patch (same trimmed name) is a no-op: the row comes
// back without an UPDATE, so updated_at stays put (the budgets no-op
// rule).
func (s *DashboardStore) Update(ctx context.Context, userID, id string, p DashboardPatch, dek []byte) (Dashboard, error) {
	cur, err := s.getByID(ctx, userID, id, dek)
	if err != nil {
		return Dashboard{}, err
	}
	if p.Name != nil {
		if err := validateName(*p.Name); err != nil {
			return Dashboard{}, fmt.Errorf("repo: %w", err)
		}
		if n := strings.TrimSpace(*p.Name); n != cur.Name {
			namePack, err := crypto.Seal(dek, []byte(n))
			if err != nil {
				return Dashboard{}, fmt.Errorf("repo: seal dashboard name: %w", err)
			}
			cmdTag, err := s.pool.Exec(ctx,
				"UPDATE dashboards SET name_encrypted = $3, updated_at = now() "+
					"WHERE user_id = $1 AND id = $2", userID, id, namePack)
			if err != nil {
				return Dashboard{}, fmt.Errorf("repo: update dashboard: %w", err)
			}
			if cmdTag.RowsAffected() == 0 {
				return Dashboard{}, ErrDashboardNotFound // deleted after the SELECT above
			}
		}
	}
	return s.getByID(ctx, userID, id, dek)
}

// Delete removes the user's dashboard; its widgets cascade (0013 FK). An
// unknown or other user's id is ErrDashboardNotFound; the row is left
// untouched.
func (s *DashboardStore) Delete(ctx context.Context, userID, id string) error {
	cmdTag, err := s.pool.Exec(ctx, `
		DELETE FROM dashboards
		WHERE user_id = $1 AND id = $2`, userID, id)
	if err != nil {
		return fmt.Errorf("repo: delete dashboard: %w", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return ErrDashboardNotFound
	}
	return nil
}

// DashboardWidgetStore persists dashboard widgets. One store serves
// every user; each method takes a userID and scopes its SQL on it (and,
// for widget-of-dashboard calls, on the dashboard too — a widget that is
// not in the named dashboard is "not found", never a cross-dashboard
// write).
type DashboardWidgetStore struct {
	pool *pgxpool.Pool
}

// NewDashboardWidgetStore returns a DashboardWidgetStore backed by pool
// (nil for a nil pool, mirroring the NewBudgetStore constructor).
func NewDashboardWidgetStore(pool *pgxpool.Pool) *DashboardWidgetStore {
	if pool == nil {
		return nil
	}
	return &DashboardWidgetStore{pool: pool}
}

// decrypt opens the name + content packs (same corruption rule as
// Dashboard.decrypt above).
func (w *DashboardWidget) decrypt(namePack, contentPack []byte, dek []byte) error {
	name, err := crypto.Open(dek, namePack)
	if err != nil {
		return fmt.Errorf("repo: decrypt dashboard widget %s name: %w", w.ID, err)
	}
	w.Name = string(name)
	content, err := crypto.Open(dek, contentPack)
	if err != nil {
		return fmt.Errorf("repo: decrypt dashboard widget %s content: %w", w.ID, err)
	}
	w.Content = string(content)
	return nil
}

// checkDashboard reports ErrDashboardNotFound when the dashboard id is
// not the caller's. List/Create/SetOrder call it up front: otherwise the
// FK (or an empty result) would make a missing dashboard surface as a 500
// (or the wrong 404), and the server pins "dashboard not found".
func (s *DashboardWidgetStore) checkDashboard(ctx context.Context, userID, dashboardID string) error {
	var exists bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM dashboards WHERE user_id = $1 AND id = $2)`, userID, dashboardID).
		Scan(&exists)
	if err != nil {
		return fmt.Errorf("repo: check dashboard %s: %w", dashboardID, err)
	}
	if !exists {
		return ErrDashboardNotFound
	}
	return nil
}

// List returns the widgets of the user's dashboard ordered by sort_order.
// A dashboard id that is not the user's is ErrDashboardNotFound. dek
// decrypts names + contents.
func (s *DashboardWidgetStore) List(ctx context.Context, userID, dashboardID string, dek []byte) ([]DashboardWidget, error) {
	if err := s.checkDashboard(ctx, userID, dashboardID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, kind, name_encrypted, content_encrypted, span, row_span, sort_order, created_at, updated_at
		FROM dashboard_widgets
		WHERE user_id = $1 AND dashboard_id = $2
		ORDER BY sort_order, id`, userID, dashboardID)
	if err != nil {
		return nil, fmt.Errorf("repo: list dashboard widgets: %w", err)
	}
	defer rows.Close()
	out := make([]DashboardWidget, 0, 4)
	for rows.Next() {
		var (
			w           DashboardWidget
			namePack    []byte
			contentPack []byte
		)
		if err := rows.Scan(&w.ID, &w.Kind, &namePack, &contentPack, &w.Span, &w.RowSpan, &w.SortOrder, &w.CreatedAt, &w.UpdatedAt); err != nil {
			return nil, fmt.Errorf("repo: scan dashboard widget: %w", err)
		}
		if err := w.decrypt(namePack, contentPack, dek); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo: list dashboard widgets: %w", err)
	}
	return out, nil
}

// WidgetIDs returns the ids of the dashboard's widgets without
// decrypting any widget config. The set-order endpoint only needs the
// id set for validation; the full List (which decrypts every widget)
// was pure overhead here.
func (s *DashboardWidgetStore) WidgetIDs(ctx context.Context, userID, dashboardID string) ([]string, error) {
	if err := s.checkDashboard(ctx, userID, dashboardID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id
		FROM dashboard_widgets
		WHERE user_id = $1 AND dashboard_id = $2
		ORDER BY sort_order, id`, userID, dashboardID)
	if err != nil {
		return nil, fmt.Errorf("repo: list dashboard widget ids: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0, 4)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("repo: scan dashboard widget: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo: list dashboard widget ids: %w", err)
	}
	return out, nil
}

// Get returns one widget of the user's dashboard, or
// ErrDashboardWidgetNotFound when the id is not in that dashboard (this
// covers unknown dashboards too — the row simply does not exist for the
// (user, dashboard) pair; existence is not disclosed).
func (s *DashboardWidgetStore) Get(ctx context.Context, userID, dashboardID, widgetID string, dek []byte) (DashboardWidget, error) {
	var (
		w           DashboardWidget
		namePack    []byte
		contentPack []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT kind, name_encrypted, content_encrypted, span, row_span, sort_order, created_at, updated_at
		FROM dashboard_widgets
		WHERE user_id = $1 AND dashboard_id = $2 AND id = $3`, userID, dashboardID, widgetID).
		Scan(&w.Kind, &namePack, &contentPack, &w.Span, &w.RowSpan, &w.SortOrder, &w.CreatedAt, &w.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return DashboardWidget{}, ErrDashboardWidgetNotFound
	}
	if err != nil {
		return DashboardWidget{}, fmt.Errorf("repo: get dashboard widget: %w", err)
	}
	w.ID = widgetID
	if err := w.decrypt(namePack, contentPack, dek); err != nil {
		return DashboardWidget{}, err
	}
	return w, nil
}

// validateWidgetName is validateName minus the non-empty rule: a widget
// may be nameless (its "title" can live in the markdown body instead,
// where it renders as a real heading; the client shows a placeholder in
// the card head). A non-empty name still obeys the 255-rune cap.
func validateWidgetName(name string) error {
	if utf8.RuneCountInString(name) > 255 {
		return errors.New("name must be at most 255 characters")
	}
	return nil
}

// defaultRowSpan is the row span a new widget gets when the create omits
// row_span: explore views chart over a wider canvas than markdown notes
// (0014), so they start taller.
func defaultRowSpan(kind string) int {
	if kind == "markdown" {
		return 2
	}
	return 4
}

// Create inserts a widget into the user's dashboard and returns it. The
// new row's sort_order is COALESCE(MAX(sort_order), 0) + 1 over the
// dashboard (appended at the end, gaps from deletes are not re-packed).
// A dashboard id that is not the user's is ErrDashboardNotFound.
func (s *DashboardWidgetStore) Create(ctx context.Context, userID, dashboardID string, in DashboardWidgetIn, dek []byte) (DashboardWidget, error) {
	if err := s.checkDashboard(ctx, userID, dashboardID); err != nil {
		return DashboardWidget{}, err
	}
	if err := validateWidgetName(in.Name); err != nil {
		return DashboardWidget{}, fmt.Errorf("repo: %w", err)
	}
	span := in.Span
	if span <= 0 {
		span = 6 // absent on the wire = the DB default
	}
	rowSpan := in.RowSpan
	if rowSpan <= 0 {
		rowSpan = defaultRowSpan(in.Kind) // absent on the wire = the per-kind default
	}
	namePack, err := crypto.Seal(dek, []byte(strings.TrimSpace(in.Name)))
	if err != nil {
		return DashboardWidget{}, fmt.Errorf("repo: seal dashboard widget name: %w", err)
	}
	contentPack, err := crypto.Seal(dek, []byte(in.Content))
	if err != nil {
		return DashboardWidget{}, fmt.Errorf("repo: seal dashboard widget content: %w", err)
	}
	var id string
	err = s.pool.QueryRow(ctx, `
		INSERT INTO dashboard_widgets
			(user_id, dashboard_id, kind, name_encrypted, content_encrypted, span, row_span, sort_order)
		VALUES ($1, $2, $3, $4, $5, $6, $7,
			COALESCE((SELECT MAX(sort_order) FROM dashboard_widgets WHERE user_id = $1 AND dashboard_id = $2), 0) + 1)
		RETURNING id`,
		userID, dashboardID, in.Kind, namePack, contentPack, span, rowSpan).Scan(&id)
	if err != nil {
		return DashboardWidget{}, fmt.Errorf("repo: create dashboard widget: %w", err)
	}
	return s.Get(ctx, userID, dashboardID, id, dek)
}

// Update applies the non-nil fields of p to the user's widget in the
// named dashboard and returns the updated row. Changed name / content
// are re-sealed under the same DEK (kind, span and row_span are
// plaintext columns). An unchanged patch is a no-op: the row comes back
// without an UPDATE, so updated_at stays put (the budgets no-op rule).
func (s *DashboardWidgetStore) Update(ctx context.Context, userID, dashboardID, widgetID string, p DashboardWidgetPatch, dek []byte) (DashboardWidget, error) {
	cur, err := s.Get(ctx, userID, dashboardID, widgetID, dek)
	if err != nil {
		return DashboardWidget{}, err
	}

	var namePack, contentPack []byte
	nameChanged := false
	if p.Name != nil {
		if err := validateWidgetName(*p.Name); err != nil {
			return DashboardWidget{}, fmt.Errorf("repo: %w", err)
		}
		if n := strings.TrimSpace(*p.Name); n != cur.Name {
			namePack, err = crypto.Seal(dek, []byte(n))
			if err != nil {
				return DashboardWidget{}, fmt.Errorf("repo: seal dashboard widget name: %w", err)
			}
			nameChanged = true
		}
	}
	contentChanged := false
	if p.Content != nil && *p.Content != cur.Content {
		contentPack, err = crypto.Seal(dek, []byte(*p.Content))
		if err != nil {
			return DashboardWidget{}, fmt.Errorf("repo: seal dashboard widget content: %w", err)
		}
		contentChanged = true
	}
	kindChanged := p.Kind != nil && *p.Kind != cur.Kind
	spanChanged := p.Span != nil && *p.Span != cur.Span
	rowSpanChanged := p.RowSpan != nil && *p.RowSpan != cur.RowSpan

	if !nameChanged && !contentChanged && !kindChanged && !spanChanged && !rowSpanChanged {
		// No-op patch: return the current row without an UPDATE.
		return cur, nil
	}

	if !nameChanged {
		// The UPDATE writes both packs; re-seal the untouched one so the
		// column is not clobbered with a zero pack.
		namePack, err = crypto.Seal(dek, []byte(cur.Name))
		if err != nil {
			return DashboardWidget{}, fmt.Errorf("repo: seal dashboard widget name: %w", err)
		}
	}
	if !contentChanged {
		contentPack, err = crypto.Seal(dek, []byte(cur.Content))
		if err != nil {
			return DashboardWidget{}, fmt.Errorf("repo: seal dashboard widget content: %w", err)
		}
	}

	kind, span, rowSpan := cur.Kind, cur.Span, cur.RowSpan
	if kindChanged {
		kind = *p.Kind
	}
	if spanChanged {
		span = *p.Span
	}
	if rowSpanChanged {
		rowSpan = *p.RowSpan
	}
	cmdTag, err := s.pool.Exec(ctx,
		"UPDATE dashboard_widgets SET name_encrypted = $4, content_encrypted = $5, "+
			"kind = $6, span = $7, row_span = $8, updated_at = now() "+
			"WHERE user_id = $1 AND dashboard_id = $2 AND id = $3",
		userID, dashboardID, widgetID, namePack, contentPack, kind, span, rowSpan)
	if err != nil {
		return DashboardWidget{}, fmt.Errorf("repo: update dashboard widget: %w", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return DashboardWidget{}, ErrDashboardWidgetNotFound // deleted after the SELECT above
	}
	return s.Get(ctx, userID, dashboardID, widgetID, dek)
}

// Delete removes the widget from the user's dashboard (a plain row
// delete — nothing else references dashboard_widgets). A widget id that
// is not in the named dashboard is ErrDashboardWidgetNotFound.
func (s *DashboardWidgetStore) Delete(ctx context.Context, userID, dashboardID, widgetID string) error {
	cmdTag, err := s.pool.Exec(ctx, `
		DELETE FROM dashboard_widgets
		WHERE user_id = $1 AND dashboard_id = $2 AND id = $3`, userID, dashboardID, widgetID)
	if err != nil {
		return fmt.Errorf("repo: delete dashboard widget: %w", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return ErrDashboardWidgetNotFound
	}
	return nil
}

// SetOrder rewrites the user's widgets' sort_order in one transaction:
// ids[i] becomes i+1. An id that is not in the named dashboard (or
// vanished between the caller's List and this call) is
// ErrDashboardWidgetNotFound and the whole transaction rolls back. The
// server (PUT /order) checks membership before calling; this is
// defense-in-depth, as with the per-method scoping everywhere else in
// this package.
func (s *DashboardWidgetStore) SetOrder(ctx context.Context, userID, dashboardID string, ids []string) error {
	if err := s.checkDashboard(ctx, userID, dashboardID); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("repo: begin set dashboard widget order: %w", err)
	}
	defer tx.Rollback(ctx) // no-op after Commit
	for i, id := range ids {
		cmdTag, err := tx.Exec(ctx, `
			UPDATE dashboard_widgets
			SET sort_order = $4
			WHERE user_id = $1 AND dashboard_id = $2 AND id = $3`, userID, dashboardID, id, i+1)
		if err != nil {
			return fmt.Errorf("repo: set dashboard widget order: %w", err)
		}
		if cmdTag.RowsAffected() == 0 {
			return ErrDashboardWidgetNotFound
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("repo: commit set dashboard widget order: %w", err)
	}
	return nil
}

// Backfill is the one-time round 6 data migration (DASHBOARDS-REWORK.md
// "Data migration"): every dashboard_cards row moves into a per-user
// dashboard named "Default" as an explore widget (name = the card's
// name, content = the card's explore_state, span 4 — the old grid was
// 3-up, sort_order values copied as-is), the cards are then deleted,
// and once EVERY user has moved the old table is dropped. DEK
// encryption is app-layer, so the migration runs here (after goose, at
// startup) and owns the DROP — 0013's down cannot produce ciphertext.
//
// Idempotent: a silent no-op when dashboard_cards is absent (already
// migrated) or empty; each user moves inside ONE transaction, so a
// crashed run never leaves a user half-migrated and a rerun is safe. A
// user whose DEK will not unwrap (or whose copy otherwise fails) is
// logged and skipped WITHOUT dropping the table — the next boot
// retries. Returns nil when there is nothing to do or everything moved.
func Backfill(ctx context.Context, pool *pgxpool.Pool, kek []byte) error {
	var exists bool
	if err := pool.QueryRow(ctx,
		"SELECT to_regclass('public.dashboard_cards') IS NOT NULL").Scan(&exists); err != nil {
		return fmt.Errorf("repo: backfill check dashboard_cards: %w", err)
	}
	if !exists {
		return nil // already migrated (the table was dropped)
	}
	rows, err := pool.Query(ctx,
		"SELECT DISTINCT user_id FROM dashboard_cards ORDER BY user_id")
	if err != nil {
		return fmt.Errorf("repo: backfill list card owners: %w", err)
	}
	userIDs := make([]string, 0, 4)
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			rows.Close()
			return fmt.Errorf("repo: backfill list card owners: %w", err)
		}
		userIDs = append(userIDs, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("repo: backfill list card owners: %w", err)
	}
	if len(userIDs) == 0 {
		return nil // empty table: nothing to move
	}

	dashboardsMoved, widgetsMoved, failed := 0, 0, 0
	for _, userID := range userIDs {
		var wrapped []byte
		err := pool.QueryRow(ctx, "SELECT wrapped_dek FROM users WHERE id = $1", userID).
			Scan(&wrapped)
		if errors.Is(err, sql.ErrNoRows) {
			// The FK should make this impossible; log and keep the table
			// so the anomaly is visible on the next boot.
			slog.Error("dashboard cards backfill: card owner has no users row", "user_id", userID)
			failed++
			continue
		}
		if err != nil {
			slog.Error("dashboard cards backfill: fetch users row failed", "user_id", userID, "error", err)
			failed++
			continue
		}
		dek, err := crypto.UnwrapDEK(kek, wrapped)
		if err != nil {
			// The DEK cannot be recovered without the KEK that sealed it;
			// skip the user and do NOT drop the table (retry next boot).
			slog.Error("dashboard cards backfill: unwrap DEK failed, cards kept for retry",
				"user_id", userID, "error", err)
			failed++
			continue
		}
		n, err := backfillUser(ctx, pool, dek, userID)
		if err != nil {
			// The per-user transaction rolled back; the cards are intact.
			slog.Error("dashboard cards backfill: user move failed, cards kept for retry",
				"user_id", userID, "error", err)
			failed++
			continue
		}
		dashboardsMoved++
		widgetsMoved += n
	}
	if failed > 0 {
		return fmt.Errorf("repo: dashboard cards backfill incomplete: %d of %d user(s) failed; dashboard_cards retained for retry",
			failed, len(userIDs))
	}
	if _, err := pool.Exec(ctx, "DROP TABLE IF EXISTS dashboard_cards"); err != nil {
		return fmt.Errorf("repo: backfill drop dashboard_cards: %w", err)
	}
	slog.Info("dashboard cards backfill complete",
		"dashboards", dashboardsMoved, "widgets", widgetsMoved)
	return nil
}

// backfillUser moves ONE user's cards into a new "Default" dashboard
// inside a single transaction and returns the number of widgets
// created. Any failure rolls the whole user back (the cards stay).
func backfillUser(ctx context.Context, pool *pgxpool.Pool, dek []byte, userID string) (int, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) // no-op after Commit

	namePack, err := crypto.Seal(dek, []byte("Default"))
	if err != nil {
		return 0, fmt.Errorf("seal dashboard name: %w", err)
	}
	var dashID string
	if err := tx.QueryRow(ctx,
		"INSERT INTO dashboards (user_id, name_encrypted) VALUES ($1, $2) RETURNING id",
		userID, namePack).Scan(&dashID); err != nil {
		return 0, fmt.Errorf("create dashboard: %w", err)
	}

	// Buffer the cards BEFORE any more commands: the pgx conn behind the
	// tx allows one in-flight query, so issuing tx.Exec while these rows
	// are still streaming fails with "conn busy".
	type backfillCard struct {
		namePack  []byte
		statePack []byte
		sort      int
	}
	crows, err := tx.Query(ctx, `
		SELECT name_encrypted, explore_state_encrypted, sort_order
		FROM dashboard_cards
		WHERE user_id = $1
		ORDER BY sort_order, id`, userID)
	if err != nil {
		return 0, fmt.Errorf("list cards: %w", err)
	}
	var cards []backfillCard
	for crows.Next() {
		var c backfillCard
		if err := crows.Scan(&c.namePack, &c.statePack, &c.sort); err != nil {
			crows.Close()
			return 0, fmt.Errorf("scan card: %w", err)
		}
		cards = append(cards, c)
	}
	crows.Close()
	if err := crows.Err(); err != nil {
		return 0, fmt.Errorf("list cards: %w", err)
	}

	n := 0
	for _, c := range cards {
		cardName, err := crypto.Open(dek, c.namePack)
		if err != nil {
			return 0, fmt.Errorf("open card name: %w", err)
		}
		state, err := crypto.Open(dek, c.statePack)
		if err != nil {
			return 0, fmt.Errorf("open card explore_state: %w", err)
		}
		wNamePack, err := crypto.Seal(dek, []byte(strings.TrimSpace(string(cardName))))
		if err != nil {
			return 0, fmt.Errorf("seal widget name: %w", err)
		}
		wContentPack, err := crypto.Seal(dek, state)
		if err != nil {
			return 0, fmt.Errorf("seal widget content: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO dashboard_widgets
				(user_id, dashboard_id, kind, name_encrypted, content_encrypted, span, sort_order)
			VALUES ($1, $2, 'explore', $3, $4, 4, $5)`,
			userID, dashID, wNamePack, wContentPack, c.sort); err != nil {
			return 0, fmt.Errorf("insert widget: %w", err)
		}
		n++
	}

	if _, err := tx.Exec(ctx, "DELETE FROM dashboard_cards WHERE user_id = $1", userID); err != nil {
		return 0, fmt.Errorf("delete cards: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return n, nil
}

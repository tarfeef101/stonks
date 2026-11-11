// Per-user saved Explore views (round 16, migration 0015): a named
// snapshot of the Explore page's URL state — the canonical
// explore_state query string ("" = the default view, the frontend
// codec's serializeState() output). The Explore view bar saves, opens,
// updates, renames and deletes them; the dashboard add-widget flow
// picks one and the widget stores its OWN copy of the state (a copy,
// not a reference — deleting a saved view never breaks a widget).
// name AND state are each ONE DEK-encrypted pack (the state can carry
// the free-text q search term, like explore_state did in the old
// dashboard_cards). Like the dashboards there is NO unique constraint
// on names (duplicate labels allowed).
package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"finance-tracker/internal/crypto"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SavedView is one saved_views row with its encrypted fields opened.
// State is the canonical explore_state query string ("" = the default
// view).
type SavedView struct {
	ID        string
	Name      string
	State     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// SavedViewIn is the Create input. Name is validateName'd here (the
// repo is the owner of the house name rule); State is the raw
// canonical explore_state string ("" = the default view) — its length
// cap is the server's (same split as the dashboard widget content).
type SavedViewIn struct {
	Name  string
	State string
}

// SavedViewPatch is the Update input; nil = untouched (absent or null
// in the PATCH body, like DashboardPatch).
type SavedViewPatch struct {
	Name  *string
	State *string
}

// SavedViewStore persists saved views. One store serves every user;
// each method takes a userID and scopes its SQL on it, so a token can
// only ever touch its own rows.
type SavedViewStore struct {
	pool *pgxpool.Pool
}

// NewSavedViewStore returns a SavedViewStore backed by pool (nil for a
// nil pool, mirroring the NewDashboardStore constructor).
func NewSavedViewStore(pool *pgxpool.Pool) *SavedViewStore {
	if pool == nil {
		return nil
	}
	return &SavedViewStore{pool: pool}
}

// decrypt opens the name + state packs. A pack that does not open is a
// key/data mismatch the API surfaces as 500 (the DEK came from the
// user's own wrapped_dek, so this is corruption, not a client error) —
// same rule as the other stores.
func (v *SavedView) decrypt(namePack, statePack []byte, dek []byte) error {
	name, err := crypto.Open(dek, namePack)
	if err != nil {
		return fmt.Errorf("repo: decrypt saved view %s name: %w", v.ID, err)
	}
	v.Name = string(name)
	state, err := crypto.Open(dek, statePack)
	if err != nil {
		return fmt.Errorf("repo: decrypt saved view %s state: %w", v.ID, err)
	}
	v.State = string(state)
	return nil
}

// getByID returns one of the user's saved views or
// ErrSavedViewNotFound when the id is not the user's (existence is not
// disclosed across users).
func (s *SavedViewStore) getByID(ctx context.Context, userID, id string, dek []byte) (SavedView, error) {
	var (
		v         SavedView
		namePack  []byte
		statePack []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT name_encrypted, state_encrypted, created_at, updated_at
		FROM saved_views
		WHERE user_id = $1 AND id = $2`, userID, id).
		Scan(&namePack, &statePack, &v.CreatedAt, &v.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SavedView{}, ErrSavedViewNotFound
	}
	if err != nil {
		return SavedView{}, fmt.Errorf("repo: get saved view: %w", err)
	}
	v.ID = id
	if err := v.decrypt(namePack, statePack, dek); err != nil {
		return SavedView{}, err
	}
	return v, nil
}

// List returns the user's saved views ordered by created_at, id as the
// tiebreak. dek decrypts names + states.
func (s *SavedViewStore) List(ctx context.Context, userID string, dek []byte) ([]SavedView, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name_encrypted, state_encrypted, created_at, updated_at
		FROM saved_views
		WHERE user_id = $1
		ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, fmt.Errorf("repo: list saved views: %w", err)
	}
	defer rows.Close()
	out := make([]SavedView, 0, 4)
	for rows.Next() {
		var (
			v         SavedView
			namePack  []byte
			statePack []byte
		)
		if err := rows.Scan(&v.ID, &namePack, &statePack, &v.CreatedAt, &v.UpdatedAt); err != nil {
			return nil, fmt.Errorf("repo: scan saved view: %w", err)
		}
		if err := v.decrypt(namePack, statePack, dek); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo: list saved views: %w", err)
	}
	return out, nil
}

// Create inserts a saved view for the user and returns it.
func (s *SavedViewStore) Create(ctx context.Context, userID string, in SavedViewIn, dek []byte) (SavedView, error) {
	if err := validateName(in.Name); err != nil {
		return SavedView{}, fmt.Errorf("repo: %w", err)
	}
	namePack, err := crypto.Seal(dek, []byte(strings.TrimSpace(in.Name)))
	if err != nil {
		return SavedView{}, fmt.Errorf("repo: seal saved view name: %w", err)
	}
	statePack, err := crypto.Seal(dek, []byte(in.State))
	if err != nil {
		return SavedView{}, fmt.Errorf("repo: seal saved view state: %w", err)
	}
	var id string
	err = s.pool.QueryRow(ctx, `
		INSERT INTO saved_views (user_id, name_encrypted, state_encrypted)
		VALUES ($1, $2, $3)
		RETURNING id`, userID, namePack, statePack).Scan(&id)
	if err != nil {
		return SavedView{}, fmt.Errorf("repo: create saved view: %w", err)
	}
	return s.getByID(ctx, userID, id, dek)
}

// Update applies the non-nil fields of p to the user's saved view and
// returns the updated row. A changed name / state is re-sealed under
// the same DEK. An unchanged patch is a no-op: the row comes back
// without an UPDATE, so updated_at stays put (the budgets no-op rule).
func (s *SavedViewStore) Update(ctx context.Context, userID, id string, p SavedViewPatch, dek []byte) (SavedView, error) {
	cur, err := s.getByID(ctx, userID, id, dek)
	if err != nil {
		return SavedView{}, err
	}

	var namePack, statePack []byte
	nameChanged := false
	if p.Name != nil {
		if err := validateName(*p.Name); err != nil {
			return SavedView{}, fmt.Errorf("repo: %w", err)
		}
		if n := strings.TrimSpace(*p.Name); n != cur.Name {
			namePack, err = crypto.Seal(dek, []byte(n))
			if err != nil {
				return SavedView{}, fmt.Errorf("repo: seal saved view name: %w", err)
			}
			nameChanged = true
		}
	}
	stateChanged := false
	if p.State != nil && *p.State != cur.State {
		statePack, err = crypto.Seal(dek, []byte(*p.State))
		if err != nil {
			return SavedView{}, fmt.Errorf("repo: seal saved view state: %w", err)
		}
		stateChanged = true
	}
	if !nameChanged && !stateChanged {
		// No-op patch: return the current row without an UPDATE.
		return cur, nil
	}
	if !nameChanged {
		// The UPDATE writes both packs; re-seal the untouched one so the
		// column is not clobbered with a zero pack.
		namePack, err = crypto.Seal(dek, []byte(cur.Name))
		if err != nil {
			return SavedView{}, fmt.Errorf("repo: seal saved view name: %w", err)
		}
	}
	if !stateChanged {
		statePack, err = crypto.Seal(dek, []byte(cur.State))
		if err != nil {
			return SavedView{}, fmt.Errorf("repo: seal saved view state: %w", err)
		}
	}
	cmdTag, err := s.pool.Exec(ctx,
		"UPDATE saved_views SET name_encrypted = $3, state_encrypted = $4, updated_at = now() "+
			"WHERE user_id = $1 AND id = $2", userID, id, namePack, statePack)
	if err != nil {
		return SavedView{}, fmt.Errorf("repo: update saved view: %w", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return SavedView{}, ErrSavedViewNotFound // deleted after the SELECT above
	}
	return s.getByID(ctx, userID, id, dek)
}

// Delete removes the user's saved view (a plain row delete — widgets
// store their own copy of the state, so nothing else references
// saved_views). An unknown or other user's id is
// ErrSavedViewNotFound; the row is left untouched.
func (s *SavedViewStore) Delete(ctx context.Context, userID, id string) error {
	cmdTag, err := s.pool.Exec(ctx, `
		DELETE FROM saved_views
		WHERE user_id = $1 AND id = $2`, userID, id)
	if err != nil {
		return fmt.Errorf("repo: delete saved view: %w", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return ErrSavedViewNotFound
	}
	return nil
}

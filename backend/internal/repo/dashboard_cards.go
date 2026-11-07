// Per-user dashboard card repository (plan step 6.5, migration 0008):
// saved Explore views. name + the codec's canonical explore_state query
// string are each one DEK-encrypted pack (the table is not enumerated in
// the plan §5 inventory, but the house convention applies: user-authored
// names are DEK-encrypted like accounts/categories/budgets, and
// explore_state can carry the free-text q search term). sort_order is
// the per-user display order, rewritten atomically by SetOrder (the
// PUT /order endpoint); new rows take MAX(sort_order)+1 over the
// caller's rows. Like budgets there is NO unique constraint on the name
// (duplicate labels allowed).
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

// DashboardCard is one dashboard_cards row with its encrypted fields
// opened. Name is decrypted; ExploreState is the codec's canonical
// query string ("" = the all-time default view).
type DashboardCard struct {
	ID           string
	Name         string
	ExploreState string
	SortOrder    int
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// DashboardCardIn is the Create input. ExploreState is a plain string:
// "" IS the default (all-time view), no pointer needed.
type DashboardCardIn struct {
	Name         string
	ExploreState string
}

// DashboardCardPatch is the Update input; nil = untouched (absent or
// null in the PATCH body, like BudgetPatch).
type DashboardCardPatch struct {
	Name         *string
	ExploreState *string
}

// DashboardCardStore persists dashboard cards. One store serves every
// user; each method takes a userID and scopes its SQL on it.
type DashboardCardStore struct {
	pool *pgxpool.Pool
}

// NewDashboardCardStore returns a DashboardCardStore backed by pool (nil
// for a nil pool, mirroring the NewBudgetStore constructor).
func NewDashboardCardStore(pool *pgxpool.Pool) *DashboardCardStore {
	if pool == nil {
		return nil
	}
	return &DashboardCardStore{pool: pool}
}

// decrypt opens the name + explore_state packs. A pack that does not
// open is a key/data mismatch the API surfaces as 500 (the DEK came from
// the user's own wrapped_dek, so this is corruption, not a client error)
// — same rule as the other stores.
func (c *DashboardCard) decrypt(namePack, statePack []byte, dek []byte) error {
	name, err := crypto.Open(dek, namePack)
	if err != nil {
		return fmt.Errorf("repo: decrypt dashboard card %s name: %w", c.ID, err)
	}
	c.Name = string(name)
	state, err := crypto.Open(dek, statePack)
	if err != nil {
		return fmt.Errorf("repo: decrypt dashboard card %s explore_state: %w", c.ID, err)
	}
	c.ExploreState = string(state)
	return nil
}

// List returns the user's cards ordered by sort_order. dek decrypts
// names + explore states.
func (s *DashboardCardStore) List(ctx context.Context, userID string, dek []byte) ([]DashboardCard, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name_encrypted, explore_state_encrypted, sort_order, created_at, updated_at
		FROM dashboard_cards
		WHERE user_id = $1
		ORDER BY sort_order`, userID)
	if err != nil {
		return nil, fmt.Errorf("repo: list dashboard cards: %w", err)
	}
	defer rows.Close()
	out := make([]DashboardCard, 0, 4)
	for rows.Next() {
		var (
			c         DashboardCard
			namePack  []byte
			statePack []byte
		)
		if err := rows.Scan(&c.ID, &namePack, &statePack, &c.SortOrder, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("repo: scan dashboard card: %w", err)
		}
		if err := c.decrypt(namePack, statePack, dek); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo: list dashboard cards: %w", err)
	}
	return out, nil
}

// Get returns one of the user's cards, or ErrDashboardCardNotFound when
// the id is not the user's (existence is not disclosed across users).
func (s *DashboardCardStore) Get(ctx context.Context, userID, id string, dek []byte) (DashboardCard, error) {
	var (
		c         DashboardCard
		namePack  []byte
		statePack []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT name_encrypted, explore_state_encrypted, sort_order, created_at, updated_at
		FROM dashboard_cards
		WHERE user_id = $1 AND id = $2`, userID, id).
		Scan(&namePack, &statePack, &c.SortOrder, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return DashboardCard{}, ErrDashboardCardNotFound
	}
	if err != nil {
		return DashboardCard{}, fmt.Errorf("repo: get dashboard card: %w", err)
	}
	c.ID = id
	if err := c.decrypt(namePack, statePack, dek); err != nil {
		return DashboardCard{}, err
	}
	return c, nil
}

// Create inserts a card for the user and returns it. The new row's
// sort_order is COALESCE(MAX(sort_order), 0) + 1 over the caller's rows
// (appended at the end, gaps from deletes are not re-packed).
func (s *DashboardCardStore) Create(ctx context.Context, userID string, in DashboardCardIn, dek []byte) (DashboardCard, error) {
	if err := validateName(in.Name); err != nil {
		return DashboardCard{}, fmt.Errorf("repo: %w", err)
	}
	name := strings.TrimSpace(in.Name)
	namePack, err := crypto.Seal(dek, []byte(name))
	if err != nil {
		return DashboardCard{}, fmt.Errorf("repo: seal dashboard card name: %w", err)
	}
	statePack, err := crypto.Seal(dek, []byte(in.ExploreState))
	if err != nil {
		return DashboardCard{}, fmt.Errorf("repo: seal dashboard card explore_state: %w", err)
	}
	var id string
	err = s.pool.QueryRow(ctx, `
		INSERT INTO dashboard_cards (user_id, name_encrypted, explore_state_encrypted, sort_order)
		VALUES ($1, $2, $3,
			COALESCE((SELECT MAX(sort_order) FROM dashboard_cards WHERE user_id = $1), 0) + 1)
		RETURNING id`,
		userID, namePack, statePack).Scan(&id)
	if err != nil {
		return DashboardCard{}, fmt.Errorf("repo: create dashboard card: %w", err)
	}
	return s.Get(ctx, userID, id, dek)
}

// Update applies the non-nil fields of p to the user's card and returns
// the updated row. Renamed names / changed explore states are re-sealed
// under the same DEK. An unchanged patch (same trimmed name and
// explore_state) is a no-op: the row comes back without an UPDATE, so
// updated_at stays put (the budgets no-op rule).
func (s *DashboardCardStore) Update(ctx context.Context, userID, id string, p DashboardCardPatch, dek []byte) (DashboardCard, error) {
	var (
		namePack  []byte
		statePack []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT name_encrypted, explore_state_encrypted
		FROM dashboard_cards
		WHERE user_id = $1 AND id = $2`, userID, id).
		Scan(&namePack, &statePack)
	if errors.Is(err, sql.ErrNoRows) {
		return DashboardCard{}, ErrDashboardCardNotFound
	}
	if err != nil {
		return DashboardCard{}, fmt.Errorf("repo: get dashboard card: %w", err)
	}
	curName, err := crypto.Open(dek, namePack)
	if err != nil {
		return DashboardCard{}, fmt.Errorf("repo: decrypt dashboard card %s name: %w", id, err)
	}
	curState, err := crypto.Open(dek, statePack)
	if err != nil {
		return DashboardCard{}, fmt.Errorf("repo: decrypt dashboard card %s explore_state: %w", id, err)
	}

	nameChanged := false
	if p.Name != nil {
		if err := validateName(*p.Name); err != nil {
			return DashboardCard{}, fmt.Errorf("repo: %w", err)
		}
		if n := strings.TrimSpace(*p.Name); n != string(curName) {
			pack, err := crypto.Seal(dek, []byte(n))
			if err != nil {
				return DashboardCard{}, fmt.Errorf("repo: seal dashboard card name: %w", err)
			}
			namePack, nameChanged = pack, true
		}
	}
	stateChanged := false
	if p.ExploreState != nil && *p.ExploreState != string(curState) {
		pack, err := crypto.Seal(dek, []byte(*p.ExploreState))
		if err != nil {
			return DashboardCard{}, fmt.Errorf("repo: seal dashboard card explore_state: %w", err)
		}
		statePack, stateChanged = pack, true
	}

	if !nameChanged && !stateChanged {
		// No-op patch: return the current row without an UPDATE.
		return s.Get(ctx, userID, id, dek)
	}

	cmdTag, err := s.pool.Exec(ctx,
		"UPDATE dashboard_cards SET name_encrypted = $3, explore_state_encrypted = $4, "+
			"updated_at = now() WHERE user_id = $1 AND id = $2",
		userID, id, namePack, statePack)
	if err != nil {
		return DashboardCard{}, fmt.Errorf("repo: update dashboard card: %w", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return DashboardCard{}, ErrDashboardCardNotFound // deleted after the SELECT above
	}
	return s.Get(ctx, userID, id, dek)
}

// Delete removes the user's card (nothing references dashboard_cards;
// 0008 has no inbound FKs). An unknown or other user's id is
// ErrDashboardCardNotFound; the row is left untouched.
func (s *DashboardCardStore) Delete(ctx context.Context, userID, id string) error {
	cmdTag, err := s.pool.Exec(ctx, `
		DELETE FROM dashboard_cards
		WHERE user_id = $1 AND id = $2`, userID, id)
	if err != nil {
		return fmt.Errorf("repo: delete dashboard card: %w", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return ErrDashboardCardNotFound
	}
	return nil
}

// SetOrder rewrites the caller's cards' sort_order in one transaction:
// ids[i] becomes i+1. An id that is not the caller's (or vanished
// between the caller's List and this call) is ErrDashboardCardNotFound
// and the whole transaction rolls back. The server (PUT /order) checks
// ownership before calling; this is defense-in-depth, as with the
// per-method scoping everywhere else in this package.
func (s *DashboardCardStore) SetOrder(ctx context.Context, userID string, ids []string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("repo: begin set dashboard card order: %w", err)
	}
	defer tx.Rollback(ctx) // no-op after Commit
	for i, id := range ids {
		cmdTag, err := tx.Exec(ctx, `
			UPDATE dashboard_cards
			SET sort_order = $3
			WHERE user_id = $1 AND id = $2`, userID, id, i+1)
		if err != nil {
			return fmt.Errorf("repo: set dashboard card order: %w", err)
		}
		if cmdTag.RowsAffected() == 0 {
			return ErrDashboardCardNotFound
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("repo: commit set dashboard card order: %w", err)
	}
	return nil
}

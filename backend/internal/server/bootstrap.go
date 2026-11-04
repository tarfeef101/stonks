// User bootstrap (plan step 1.3c): the first authenticated request for a
// given sub idempotently creates the users row and stores the KEK-wrapped
// per-user DEK. Later logins (same or concurrent) return the stored row
// and never overwrite it.
package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"finance-tracker/internal/authn"
	"finance-tracker/internal/crypto"

	"github.com/jackc/pgx/v5/pgxpool"
)

// UserRow is one users-table row (migration 0001). ID is the database
// uuid, returned as a string: the module has no direct uuid dependency
// (pgx v5 scans uuid columns into string without one).
type UserRow struct {
	ID          string
	Sub         string
	Email       string
	DisplayName string
	WrappedDEK  []byte
}

// UserStore persists users rows. Implementations must be idempotent for a
// given sub: repeat calls return the stored row (first login wins), never
// a second row and never an overwritten wrapped_dek.
type UserStore interface {
	UpsertUser(ctx context.Context, sub, email, displayName string, wrappedDEK []byte) (UserRow, error)
}

// pgUserStore is the production UserStore backed by the users table.
type pgUserStore struct {
	pool *pgxpool.Pool
}

// NewPGUserStore returns a UserStore backed by pool (nil for a nil pool;
// NewBootstrap fails fast on a nil store).
func NewPGUserStore(pool *pgxpool.Pool) UserStore {
	if pool == nil {
		return nil
	}
	return &pgUserStore{pool: pool}
}

func (s *pgUserStore) UpsertUser(ctx context.Context, sub, email, displayName string, wrappedDEK []byte) (UserRow, error) {
	// Idempotent create. A conflicting concurrent INSERT blocks on the
	// winner's committed transaction, so the SELECT below is guaranteed to
	// see the row. ON CONFLICT DO NOTHING yields no RETURNING row on
	// conflict, hence the second statement. Losers' freshly wrapped DEKs
	// are simply discarded — they never touch the stored pack.
	_, err := s.pool.Exec(ctx, `
		INSERT INTO users (auth0_sub, email, display_name, wrapped_dek)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (auth0_sub) DO NOTHING`, sub, email, displayName, wrappedDEK)
	if err != nil {
		return UserRow{}, fmt.Errorf("server: insert user %q: %w", sub, err)
	}
	var row UserRow
	err = s.pool.QueryRow(ctx, `
		SELECT id, auth0_sub, email, display_name, wrapped_dek
		FROM users
		WHERE auth0_sub = $1`, sub).
		Scan(&row.ID, &row.Sub, &row.Email, &row.DisplayName, &row.WrappedDEK)
	if errors.Is(err, sql.ErrNoRows) {
		return UserRow{}, fmt.Errorf("server: user %q not found after insert", sub)
	}
	if err != nil {
		return UserRow{}, fmt.Errorf("server: fetch user %q: %w", sub, err)
	}
	return row, nil
}

// Bootstrap wires a UserStore to the KEK so the API can ensure a user row
// on first authenticated request.
type Bootstrap struct {
	store UserStore
	kek   []byte
}

// NewBootstrap returns a Bootstrap. It fails fast on a nil store or a KEK
// that is not exactly crypto.KEKSize bytes.
func NewBootstrap(store UserStore, kek []byte) (*Bootstrap, error) {
	if store == nil {
		return nil, errors.New("server: user store is required")
	}
	if len(kek) != crypto.KEKSize {
		return nil, fmt.Errorf("server: KEK must be %d bytes, got %d", crypto.KEKSize, len(kek))
	}
	return &Bootstrap{store: store, kek: kek}, nil
}

// EnsureUser idempotently creates the users row for id. The first call
// mints a fresh DEK, wraps it with the KEK, and stores it; later calls
// (repeat or concurrent first logins) return the stored row and discard
// the freshly wrapped DEK. Display fields fall back through the token's
// profile claims so the NOT NULL columns always get non-empty values:
// email = email | sub, display_name = name | preferred_username | email.
func (b *Bootstrap) EnsureUser(ctx context.Context, id authn.Identity) (UserRow, error) {
	if id.Sub == "" {
		return UserRow{}, errors.New("server: identity has no sub")
	}
	email := id.Email
	if email == "" {
		email = id.Sub
	}
	displayName := id.Name
	if displayName == "" {
		displayName = id.Username
	}
	if displayName == "" {
		displayName = email
	}
	dek, err := crypto.NewDEK()
	if err != nil {
		return UserRow{}, fmt.Errorf("server: generate DEK: %w", err)
	}
	wrapped, err := crypto.WrapDEK(b.kek, dek)
	if err != nil {
		return UserRow{}, fmt.Errorf("server: wrap DEK: %w", err)
	}
	return b.store.UpsertUser(ctx, id.Sub, email, displayName, wrapped)
}

// UnwrapDEK opens the row's wrapped DEK pack with the KEK so handlers can
// pass the raw key to the repo layer (step 3.1). A pack that does not
// open means the row was written under a different KEK (or is corrupt):
// callers 500 it — re-wrapping would lose the data the old key sealed.
func (b *Bootstrap) UnwrapDEK(row UserRow) ([]byte, error) {
	dek, err := crypto.UnwrapDEK(b.kek, row.WrappedDEK)
	if err != nil {
		return nil, fmt.Errorf("server: unwrap DEK for %s: %w", row.Sub, err)
	}
	return dek, nil
}

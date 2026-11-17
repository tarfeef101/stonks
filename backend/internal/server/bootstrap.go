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
	"log/slog"
	"sync"
	"time"

	"finance-tracker/internal/authn"
	"finance-tracker/internal/crypto"
	"finance-tracker/internal/mail"

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
// a second row and never an overwritten wrapped_dek. created is true only
// on the call that inserted the row (false for conflict losers, which see
// the winner's row) — it is the exactly-once flag for first-login side
// effects (step 8.1: the welcome email).
type UserStore interface {
	UpsertUser(ctx context.Context, sub, email, displayName string, wrappedDEK []byte) (UserRow, bool, error)
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

func (s *pgUserStore) UpsertUser(ctx context.Context, sub, email, displayName string, wrappedDEK []byte) (UserRow, bool, error) {
	// Idempotent create. A conflicting concurrent INSERT blocks on the
	// winner's committed transaction, so the SELECT below is guaranteed to
	// see the row. ON CONFLICT DO NOTHING yields no RETURNING row on
	// conflict, hence the second statement. Losers' freshly wrapped DEKs
	// are simply discarded — they never touch the stored pack.
	// RowsAffected doubles as the exactly-once flag: 1 on the winning
	// insert, 0 on every conflict (no extra query).
	res, err := s.pool.Exec(ctx, `
		INSERT INTO users (auth0_sub, email, display_name, wrapped_dek)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (auth0_sub) DO NOTHING`, sub, email, displayName, wrappedDEK)
	if err != nil {
		return UserRow{}, false, fmt.Errorf("server: insert user %q: %w", sub, err)
	}
	var row UserRow
	err = s.pool.QueryRow(ctx, `
		SELECT id, auth0_sub, email, display_name, wrapped_dek
		FROM users
		WHERE auth0_sub = $1`, sub).
		Scan(&row.ID, &row.Sub, &row.Email, &row.DisplayName, &row.WrappedDEK)
	if errors.Is(err, sql.ErrNoRows) {
		return UserRow{}, false, fmt.Errorf("server: user %q not found after insert", sub)
	}
	if err != nil {
		return UserRow{}, false, fmt.Errorf("server: fetch user %q: %w", sub, err)
	}
	return row, res.RowsAffected() == 1, nil
}

// Bootstrap wires a UserStore to the KEK so the API can ensure a user row
// on first authenticated request. mailer (may be nil = disabled) sends
// the exactly-once welcome email on the row-creating call (step 8.1).
type Bootstrap struct {
	store  UserStore
	kek    []byte
	mailer mail.Sender

	// dekCache holds unwrapped per-user DEKs, keyed by users.id and
	// guarded by mu (see UnwrapDEK). It is bounded by the number of
	// users this process has served.
	mu       sync.Mutex
	dekCache map[string]dekCacheEntry
}

// NewBootstrap returns a Bootstrap. It fails fast on a nil store or a KEK
// that is not exactly crypto.KEKSize bytes. mailer may be nil: the
// welcome email is then skipped silently.
func NewBootstrap(store UserStore, kek []byte, mailer mail.Sender) (*Bootstrap, error) {
	if store == nil {
		return nil, errors.New("server: user store is required")
	}
	if len(kek) != crypto.KEKSize {
		return nil, fmt.Errorf("server: KEK must be %d bytes, got %d", crypto.KEKSize, len(kek))
	}
	return &Bootstrap{store: store, kek: kek, mailer: mailer, dekCache: map[string]dekCacheEntry{}}, nil
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
	row, created, err := b.store.UpsertUser(ctx, id.Sub, email, displayName, wrapped)
	if err != nil {
		return UserRow{}, err
	}
	// Exactly-once welcome (step 8.1): only the row-creating call sends
	// (the conflict loser gets created=false), and a mail failure is
	// logged and swallowed — it must never fail the request.
	if created && b.mailer != nil {
		msg, cerr := mail.ComposeWelcome(row.Email, row.DisplayName)
		if cerr != nil {
			slog.Error("compose welcome email failed", "sub", row.Sub, "error", cerr)
		} else if serr := b.mailer.Send(ctx, msg); serr != nil {
			slog.Error("send welcome email failed", "sub", row.Sub, "to", row.Email, "error", serr)
		}
	}
	return row, nil
}

// dekCacheEntry is one cached unwrapped DEK.
type dekCacheEntry struct {
	dek     []byte
	expires time.Time
}

// dekCacheTTL is how long an unwrapped DEK may sit in memory. A DEK is
// minted exactly once (at user creation) and the app never re-keys it —
// KEK rotation re-wraps the pack but the DEK bytes stay the same — so a
// cached entry stays valid for the user's whole life. The TTL is pure
// hygiene: it bounds how long a live DEK lingers in process memory
// (crash dumps, heap snapshots) at the cost of one GCM unwrap per user
// per 5 minutes, which is negligible against the per-request Seal/Open
// work it used to replace on every handler.
const dekCacheTTL = 5 * time.Minute

// UnwrapDEK opens the row's wrapped DEK pack with the KEK so handlers can
// pass the raw key to the repo layer (step 3.1). A pack that does not
// open means the row was written under a different KEK (or is corrupt):
// callers 500 it — re-wrapping would lose the data the old key sealed.
// Successful unwraps are cached per users.id for dekCacheTTL (the DEK
// never changes after creation, so a stale entry can never be wrong);
// failed unwraps are never cached and surface the exact error as before.
func (b *Bootstrap) UnwrapDEK(row UserRow) ([]byte, error) {
	if row.ID != "" {
		b.mu.Lock()
		if e, ok := b.dekCache[row.ID]; ok {
			if time.Now().Before(e.expires) {
				b.mu.Unlock()
				return e.dek, nil
			}
			// Expired: drop it so the map doesn't hold stale key bytes.
			delete(b.dekCache, row.ID)
		}
		b.mu.Unlock()
	}
	dek, err := crypto.UnwrapDEK(b.kek, row.WrappedDEK)
	if err != nil {
		return nil, fmt.Errorf("server: unwrap DEK for %s: %w", row.Sub, err)
	}
	if row.ID != "" {
		b.mu.Lock()
		b.dekCache[row.ID] = dekCacheEntry{dek: dek, expires: time.Now().Add(dekCacheTTL)}
		b.mu.Unlock()
	}
	return dek, nil
}

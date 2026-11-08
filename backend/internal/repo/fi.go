// FI institution + credential repository (plan step 9.1, migration
// 0010). Institutions are a GLOBAL registry (no user scoping, no
// encryption); credentials are per-user and each secret (token,
// refresh token) is stored as ONE DEK-encrypted pack per plan §5
// (one pack per secret, two packs per row) with the same pack
// idiom as repo/mortgages.go. last_synced_at is set by the 9.2 sync
// job via MarkSynced and stays NULL until the first sync. The store
// does NOT validate institution/code shape (the server does, like
// mortgages); a bad institution_id surfaces as a foreign-key
// violation. There is no institution Delete: 9.1 has no
// institution-delete API, so there is no orphan path to serve.
package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"finance-tracker/internal/crypto"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Institution struct {
	ID     string
	Code   string
	Name   string
	Status string
}

type InstitutionIn struct {
	Code string
	Name string
}

// Credential is one fi_credentials row with both secrets opened. The
// institution code/name are joined in for the wire DTO; created_at
// stays on the row, not here.
type Credential struct {
	ID              string
	UserID          string
	InstitutionID   string
	InstitutionCode string // joined in, for the wire DTO
	InstitutionName string // joined in, for the wire DTO
	LastSyncedAt    *time.Time
}

// FICredentialIn is the Create input: the institution plus the two
// secrets as plaintext (they seal into the row, never stored raw).
type FICredentialIn struct {
	InstitutionID string
	Token         string
	RefreshToken  string
}

// InstitutionStore persists the global institution registry.
type InstitutionStore struct {
	pool *pgxpool.Pool
}

// NewInstitutionStore returns an InstitutionStore backed by pool (nil
// for a nil pool, mirroring the NewMortgageStore constructor).
func NewInstitutionStore(pool *pgxpool.Pool) *InstitutionStore {
	if pool == nil {
		return nil
	}
	return &InstitutionStore{pool: pool}
}

// FICredentialStore persists per-user FI credentials. One store serves
// every user; each method takes a userID and scopes its SQL on it.
type FICredentialStore struct {
	pool *pgxpool.Pool
}

// NewFICredentialStore returns a FICredentialStore backed by pool (nil
// for a nil pool, mirroring the NewMortgageStore constructor).
func NewFICredentialStore(pool *pgxpool.Pool) *FICredentialStore {
	if pool == nil {
		return nil
	}
	return &FICredentialStore{pool: pool}
}

// sealSecret seals one secret as ONE pack (plan §5: one pack per
// secret, two packs per fi_credentials row).
func sealSecret(dek []byte, secret, what string) ([]byte, error) {
	pack, err := crypto.Seal(dek, []byte(secret))
	if err != nil {
		return nil, fmt.Errorf("repo: seal fi %s: %w", what, err)
	}
	return pack, nil
}

// openSecret opens one pack back to plaintext. A pack that does not
// open is a key/data mismatch the API surfaces as 500 (the DEK came
// from the user's own wrapped_dek, so this is corruption, not a
// client error) — same rule as the other stores.
func openSecret(dek []byte, pack []byte, what string) (string, error) {
	plain, err := crypto.Open(dek, pack)
	if err != nil {
		return "", fmt.Errorf("repo: decrypt fi %s: %w", what, err)
	}
	return string(plain), nil
}

// Get returns one institution by id, or ErrInstitutionNotFound.
func (s *InstitutionStore) Get(ctx context.Context, id string) (Institution, error) {
	var i Institution
	err := s.pool.QueryRow(ctx, `
		SELECT code, name, status
		FROM fi_institutions
		WHERE id = $1`, id).Scan(&i.Code, &i.Name, &i.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return Institution{}, ErrInstitutionNotFound
	}
	if err != nil {
		return Institution{}, fmt.Errorf("repo: get institution: %w", err)
	}
	i.ID = id
	return i, nil
}

// GetByCode returns one institution by its unique code, or
// ErrInstitutionNotFound.
func (s *InstitutionStore) GetByCode(ctx context.Context, code string) (Institution, error) {
	var i Institution
	err := s.pool.QueryRow(ctx, `
		SELECT id, name, status
		FROM fi_institutions
		WHERE code = $1`, code).Scan(&i.ID, &i.Name, &i.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return Institution{}, ErrInstitutionNotFound
	}
	if err != nil {
		return Institution{}, fmt.Errorf("repo: get institution by code: %w", err)
	}
	i.Code = code
	return i, nil
}

// Create inserts an institution and returns it. The status column
// defaults to 'active' (0010 CHECK). A duplicate code surfaces as
// ErrInstitutionCodeTaken (23505).
func (s *InstitutionStore) Create(ctx context.Context, in InstitutionIn) (Institution, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO fi_institutions (code, name)
		VALUES ($1, $2)
		RETURNING id`, in.Code, in.Name).Scan(&id)
	if err != nil {
		switch pgErrCode(err) {
		case "23505":
			return Institution{}, ErrInstitutionCodeTaken
		default:
			return Institution{}, fmt.Errorf("repo: create institution: %w", err)
		}
	}
	return s.Get(ctx, id)
}

// List returns every institution ordered by code.
func (s *InstitutionStore) List(ctx context.Context) ([]Institution, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, code, name, status
		FROM fi_institutions
		ORDER BY code`)
	if err != nil {
		return nil, fmt.Errorf("repo: list institutions: %w", err)
	}
	defer rows.Close()
	out := make([]Institution, 0, 4)
	for rows.Next() {
		var i Institution
		if err := rows.Scan(&i.ID, &i.Code, &i.Name, &i.Status); err != nil {
			return nil, fmt.Errorf("repo: scan institution: %w", err)
		}
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo: list institutions: %w", err)
	}
	return out, nil
}

// List returns the user's credentials ordered by institution code,
// with both secrets opened and the institution code/name joined in.
// dek decrypts the packs.
func (s *FICredentialStore) List(ctx context.Context, userID string, dek []byte) ([]Credential, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.id, c.institution_id, c.token_encrypted, c.refresh_token_encrypted,
		       c.last_synced_at, i.code, i.name
		FROM fi_credentials c JOIN fi_institutions i ON i.id = c.institution_id
		WHERE c.user_id = $1
		ORDER BY i.code`, userID)
	if err != nil {
		return nil, fmt.Errorf("repo: list fi credentials: %w", err)
	}
	defer rows.Close()
	out := make([]Credential, 0, 2)
	for rows.Next() {
		var (
			c           Credential
			tokenPack   []byte
			refreshPack []byte
		)
		if err := rows.Scan(&c.ID, &c.InstitutionID, &tokenPack, &refreshPack,
			&c.LastSyncedAt, &c.InstitutionCode, &c.InstitutionName); err != nil {
			return nil, fmt.Errorf("repo: scan fi credential: %w", err)
		}
		c.UserID = userID
		// Both packs must open under the caller's DEK: a pack that
		// does not open is a key/data mismatch the API surfaces as
		// 500 (same rule as the other stores). The plaintext never
		// leaves the store: the Credential DTO carries no secrets.
		if _, err := openSecret(dek, tokenPack, "token"); err != nil {
			return nil, err
		}
		if _, err := openSecret(dek, refreshPack, "refresh token"); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo: list fi credentials: %w", err)
	}
	return out, nil
}

// Get returns one of the user's credentials, or ErrFICredentialNotFound
// when the id is not the user's (existence is not disclosed across
// users).
func (s *FICredentialStore) Get(ctx context.Context, userID, id string, dek []byte) (Credential, error) {
	var (
		c           Credential
		tokenPack   []byte
		refreshPack []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT c.institution_id, c.token_encrypted, c.refresh_token_encrypted,
		       c.last_synced_at, i.code, i.name
		FROM fi_credentials c JOIN fi_institutions i ON i.id = c.institution_id
		WHERE c.user_id = $1 AND c.id = $2`, userID, id).
		Scan(&c.InstitutionID, &tokenPack, &refreshPack,
			&c.LastSyncedAt, &c.InstitutionCode, &c.InstitutionName)
	if errors.Is(err, sql.ErrNoRows) {
		return Credential{}, ErrFICredentialNotFound
	}
	if err != nil {
		return Credential{}, fmt.Errorf("repo: get fi credential: %w", err)
	}
	// Both packs must open under the caller's DEK (see List): a pack
	// that does not open is corruption, surfaced upstream as 500.
	if _, err := openSecret(dek, tokenPack, "token"); err != nil {
		return Credential{}, err
	}
	if _, err := openSecret(dek, refreshPack, "refresh token"); err != nil {
		return Credential{}, err
	}
	c.ID = id
	c.UserID = userID
	return c, nil
}

// Create inserts a credential for the user and returns it. BOTH
// secrets seal as ONE pack each (plan §5). The store does not check
// that the institution exists — the server does; a bad
// institution_id surfaces as a foreign-key violation. A duplicate
// (user_id, institution_id) surfaces as ErrFICredentialExists (23505).
func (s *FICredentialStore) Create(ctx context.Context, userID string, in FICredentialIn, dek []byte) (Credential, error) {
	tokenPack, err := sealSecret(dek, in.Token, "token")
	if err != nil {
		return Credential{}, err
	}
	refreshPack, err := sealSecret(dek, in.RefreshToken, "refresh token")
	if err != nil {
		return Credential{}, err
	}
	var id string
	err = s.pool.QueryRow(ctx, `
		INSERT INTO fi_credentials (user_id, institution_id, token_encrypted, refresh_token_encrypted)
		VALUES ($1, $2, $3, $4)
		RETURNING id`,
		userID, in.InstitutionID, tokenPack, refreshPack).Scan(&id)
	if err != nil {
		switch pgErrCode(err) {
		case "23505":
			return Credential{}, ErrFICredentialExists
		default:
			return Credential{}, fmt.Errorf("repo: create fi credential: %w", err)
		}
	}
	return s.Get(ctx, userID, id, dek)
}

// Delete removes the user's credential. An unknown or other user's
// id is ErrFICredentialNotFound; the row is left untouched.
func (s *FICredentialStore) Delete(ctx context.Context, userID, id string) error {
	cmdTag, err := s.pool.Exec(ctx, `
		DELETE FROM fi_credentials
		WHERE user_id = $1 AND id = $2`, userID, id)
	if err != nil {
		return fmt.Errorf("repo: delete fi credential: %w", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return ErrFICredentialNotFound
	}
	return nil
}

// MarkSynced stamps the credential's last_synced_at (the 9.2 sync job
// calls it after a successful sync). An unknown or other user's id
// is ErrFICredentialNotFound.
func (s *FICredentialStore) MarkSynced(ctx context.Context, userID, id string, at time.Time) error {
	cmdTag, err := s.pool.Exec(ctx,
		"UPDATE fi_credentials SET last_synced_at = $3 WHERE user_id = $1 AND id = $2",
		userID, id, at)
	if err != nil {
		return fmt.Errorf("repo: mark fi credential synced: %w", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return ErrFICredentialNotFound
	}
	return nil
}

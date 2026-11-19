// User-row store (round 51a, migration 0019): the repo-side slice of the
// users table. The API keeps its own bootstrap UserStore (the 1.3c row
// store in internal/server that resolves the caller's wrapped DEK); this
// package cannot import server (3.1 made server depend on repo — an
// import cycle), so this file carries the one thing the savings area
// adds to the row: the user's own birth year, sealed like every other
// *_encrypted column (birth_year_encrypted holds a DEK-encrypted decimal
// string, NULL = unknown — no plaintext year in any column).
package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"finance-tracker/internal/crypto"

	"github.com/jackc/pgx/v5/pgxpool"
)

// User is one users row; BirthYear is decrypted and nil when unset
// (NULL).
type User struct {
	ID          string
	Auth0Sub    string
	Email       string
	DisplayName string
	BirthYear   *int
}

// UserStore reads and patches users rows by their row id (the uuid the
// bootstrap store hands the API).
type UserStore struct {
	pool *pgxpool.Pool
}

// NewUserStore returns a UserStore backed by pool (nil for a nil pool,
// mirroring the other store constructors).
func NewUserStore(pool *pgxpool.Pool) *UserStore {
	if pool == nil {
		return nil
	}
	return &UserStore{pool: pool}
}

// Get returns the user row, decrypting the birth year with dek. An
// unknown id is ErrUserNotFound.
func (s *UserStore) Get(ctx context.Context, id string, dek []byte) (User, error) {
	var (
		u  User
		by *[]byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT auth0_sub, email, display_name, birth_year_encrypted
		FROM users
		WHERE id = $1`, id).Scan(&u.Auth0Sub, &u.Email, &u.DisplayName, &by)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrUserNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("repo: get user: %w", err)
	}
	u.ID = id
	b, err := openEncryptedPack(by, "user "+id+" birth year", dek)
	if err != nil {
		return User{}, err
	}
	u.BirthYear, err = decodeBirthYear(b)
	if err != nil {
		return User{}, fmt.Errorf("repo: user %s stored birth year: %w", id, err)
	}
	return u, nil
}

// SetBirthYear sets (or clears) the user's own birth year. The tri-state
// mirrors the 0019 account plan fields: nil = untouched (no-op), &"" =
// clear (the column goes to NULL), otherwise the value must pass
// parseBirthYear and is sealed as its decimal string — the sealed pack
// is the only thing that reaches the column.
func (s *UserStore) SetBirthYear(ctx context.Context, id string, birthYear *string, dek []byte) error {
	if birthYear == nil {
		return nil
	}
	var pack *[]byte
	if *birthYear != "" {
		y, err := parseBirthYear(*birthYear)
		if err != nil {
			return fmt.Errorf("repo: %w", err)
		}
		p, err := crypto.Seal(dek, []byte(strconv.Itoa(y)))
		if err != nil {
			return fmt.Errorf("repo: seal user birth year: %w", err)
		}
		pack = &p
	}
	cmdTag, err := s.pool.Exec(ctx, `
		UPDATE users
		SET birth_year_encrypted = $2, updated_at = now()
		WHERE id = $1`, id, pack)
	if err != nil {
		return fmt.Errorf("repo: set user birth year: %w", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	return nil
}

// openEncryptedPack opens a nullable *_encrypted pack: a nil pack (the
// column is NULL) is the empty string; a pack that does not open is a
// key/data mismatch the API surfaces as 500 (same rule as decryptName —
// the DEK came from the user's own wrapped_dek, so this is corruption,
// not a client error).
func openEncryptedPack(pack *[]byte, what string, dek []byte) (string, error) {
	if pack == nil {
		return "", nil
	}
	plain, err := crypto.Open(dek, *pack)
	if err != nil {
		return "", fmt.Errorf("repo: decrypt %s: %w", what, err)
	}
	return string(plain), nil
}

// decodeBirthYear interprets the decrypted contents of a birth-year
// pack: "" (a never-set or cleared column) is nil; a stored value that
// does not pass parseBirthYear is corruption (no valid write path can
// produce one), surfaced as an error for the caller to wrap.
func decodeBirthYear(s string) (*int, error) {
	if s == "" {
		return nil, nil
	}
	y, err := parseBirthYear(s)
	if err != nil {
		return nil, err
	}
	iv := y
	return &iv, nil
}

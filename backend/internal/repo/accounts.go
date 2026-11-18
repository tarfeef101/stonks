// Per-user account repository (plan step 2.5b): accounts (migration
// 0002). Names are DEK-encrypted (name_encrypted holds the [nonce ‖ ct
// ‖ tag] pack from internal/crypto); name_hash is the §5 plaintext
// uniqueness/lookup key (UNIQUE (user_id, name_hash)).
//
// The type vocabulary mirrors the 0002 named CHECK
// (bank|cash|credit_card|mortgage|investment|other) so a bad value is a
// 400 at the repo, not a 23514 surfacing as a 500 at the API.
//
// default_schema_id → import_schemas ON DELETE SET NULL (0002): a
// deleted schema orphans the setting, never the account. The reference
// check IS user-scoped here (a schema is valid iff it is owned by this
// user — the table is custom-only since 0018); the FK alone would accept
// another user's schema.
package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"finance-tracker/internal/crypto"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AccountTypes is the 0002 accounts.type vocabulary.
var AccountTypes = []string{"bank", "cash", "credit_card", "mortgage", "investment", "other"}

var accountTypeOK = func() map[string]bool {
	m := make(map[string]bool, len(AccountTypes))
	for _, t := range AccountTypes {
		m[t] = true
	}
	return m
}()

// currencyRe mirrors the 1.4a settings currency rule (ISO-4217).
var currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)

const maxSubtypeLen = 64

// Account is one accounts row. Name is the decrypted name; Subtype and
// DefaultSchemaID are "" when unset (NULL).
type Account struct {
	ID              string
	Name            string
	Type            string
	Subtype         string
	Currency        string
	DefaultSchemaID string
}

// AccountIn is the Create input. Subtype is nil = unset; DefaultSchemaID
// is nil or "" = none, otherwise a schema owned by the same user (see
// package header).
type AccountIn struct {
	Name            string
	Type            string
	Subtype         *string
	Currency        string
	DefaultSchemaID *string
}

// AccountPatch is the Update input; every field is a pointer, nil =
// untouched. Subtype and DefaultSchemaID are double pointers: nil =
// untouched, non-nil with a nil inner pointer = clear (NULL), otherwise
// the new value.
type AccountPatch struct {
	Name            *string
	Type            *string
	Subtype         **string
	Currency        *string
	DefaultSchemaID **string
}

// AccountStore persists accounts. One store serves every user; each
// method takes a userID and scopes its SQL on it.
type AccountStore struct {
	pool *pgxpool.Pool
}

// NewAccountStore returns an AccountStore backed by pool (nil for a nil
// pool, mirroring the 1.3c UserStore constructor).
func NewAccountStore(pool *pgxpool.Pool) *AccountStore {
	if pool == nil {
		return nil
	}
	return &AccountStore{pool: pool}
}

// validateAccountFields checks the non-encrypted input fields. The name
// goes through the shared validateName.
func validateAccountFields(in AccountIn) error {
	if err := validateName(in.Name); err != nil {
		return err
	}
	if !accountTypeOK[in.Type] {
		return fmt.Errorf("type must be one of %s", strings.Join(AccountTypes, ", "))
	}
	if !currencyRe.MatchString(in.Currency) {
		return errors.New("currency must be a 3-letter uppercase ISO-4217 code")
	}
	if in.Subtype != nil && utf8.RuneCountInString(*in.Subtype) > maxSubtypeLen {
		return fmt.Errorf("subtype must be at most %d characters", maxSubtypeLen)
	}
	return nil
}

// schemaVisible reports whether the schema exists and is usable by this
// user: schemas are per-user (the table is custom-only since 0018). A
// missing or other-user's schema looks identical (ErrSchemaNotFound at
// the call sites) — existence is not disclosed across users.
func (s *AccountStore) schemaVisible(ctx context.Context, userID, schemaID string) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM import_schemas
			WHERE id = $2 AND user_id = $1)`,
		userID, schemaID).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("repo: check schema: %w", err)
	}
	return ok, nil
}

// List returns the user's accounts ordered by created_at (ties: id).
// dek decrypts the names.
func (s *AccountStore) List(ctx context.Context, userID string, dek []byte) ([]Account, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name_encrypted, type, subtype, currency, default_schema_id
		FROM accounts
		WHERE user_id = $1
		ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, fmt.Errorf("repo: list accounts: %w", err)
	}
	defer rows.Close()
	out := make([]Account, 0, 8)
	for rows.Next() {
		var (
			a   Account
			enc []byte
			st  sql.NullString
			ds  sql.NullString
		)
		if err := rows.Scan(&a.ID, &enc, &a.Type, &st, &a.Currency, &ds); err != nil {
			return nil, fmt.Errorf("repo: scan account: %w", err)
		}
		a.Name, err = decryptName(a.ID, enc, dek)
		if err != nil {
			return nil, err
		}
		a.Subtype, a.DefaultSchemaID = st.String, ds.String
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo: list accounts: %w", err)
	}
	return out, nil
}

// Get returns one of the user's accounts, or ErrAccountNotFound when the
// id is not the user's (existence is not disclosed across users).
func (s *AccountStore) Get(ctx context.Context, userID, id string, dek []byte) (Account, error) {
	var (
		a   Account
		enc []byte
		st  sql.NullString
		ds  sql.NullString
	)
	err := s.pool.QueryRow(ctx, `
		SELECT name_encrypted, type, subtype, currency, default_schema_id
		FROM accounts
		WHERE user_id = $1 AND id = $2`, userID, id).
		Scan(&enc, &a.Type, &st, &a.Currency, &ds)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrAccountNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("repo: get account: %w", err)
	}
	a.ID, a.Subtype, a.DefaultSchemaID = id, st.String, ds.String
	a.Name, err = decryptName(id, enc, dek)
	return a, err
}

// Create inserts an account for the user and returns it. A duplicate
// name for this user is ErrNameTaken; a schema that is missing or owned
// by another user is ErrSchemaNotFound.
func (s *AccountStore) Create(ctx context.Context, userID string, in AccountIn, dek []byte) (Account, error) {
	if err := validateAccountFields(in); err != nil {
		return Account{}, fmt.Errorf("repo: %w", err)
	}
	name := strings.TrimSpace(in.Name)
	pack, err := crypto.Seal(dek, []byte(name))
	if err != nil {
		return Account{}, fmt.Errorf("repo: seal account name: %w", err)
	}

	var (
		subtype *string
		schema  *string
	)
	if in.Subtype != nil {
		if t := strings.TrimSpace(*in.Subtype); t != "" {
			subtype = &t
		}
	}
	if in.DefaultSchemaID != nil && *in.DefaultSchemaID != "" {
		ok, err := s.schemaVisible(ctx, userID, *in.DefaultSchemaID)
		if err != nil {
			return Account{}, err
		}
		if !ok {
			return Account{}, ErrSchemaNotFound
		}
		v := *in.DefaultSchemaID
		schema = &v
	}

	var id string
	err = s.pool.QueryRow(ctx, `
		INSERT INTO accounts (user_id, name_encrypted, name_hash, type, subtype, currency, default_schema_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id`,
		userID, pack, hashName(name), in.Type, subtype, in.Currency, schema).Scan(&id)
	if err != nil {
		switch pgErrCode(err) {
		case "23505":
			return Account{}, ErrNameTaken
		case "23503":
			return Account{}, ErrSchemaNotFound // schema deleted between check and insert
		}
		return Account{}, fmt.Errorf("repo: create account: %w", err)
	}
	a := Account{ID: id, Name: name, Type: in.Type, Currency: in.Currency}
	if subtype != nil {
		a.Subtype = *subtype
	}
	if schema != nil {
		a.DefaultSchemaID = *schema
	}
	return a, nil
}

// Update applies the non-nil fields of p to the user's account and
// returns the updated row. Renaming re-encrypts under the same DEK and
// the unique (user_id, name_hash) still applies; clearing the schema
// (inner-nil **string) sets the column to NULL.
func (s *AccountStore) Update(ctx context.Context, userID, id string, p AccountPatch, dek []byte) (Account, error) {
	var (
		enc  []byte
		hash string
		typ  string
		cur  string
		st   sql.NullString
		ds   sql.NullString
	)
	err := s.pool.QueryRow(ctx, `
		SELECT name_encrypted, name_hash, type, currency, subtype, default_schema_id
		FROM accounts
		WHERE user_id = $1 AND id = $2`, userID, id).
		Scan(&enc, &hash, &typ, &cur, &st, &ds)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrAccountNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("repo: get account: %w", err)
	}

	nameChanged, schemaChanged := false, false
	if p.Name != nil {
		if err := validateName(*p.Name); err != nil {
			return Account{}, fmt.Errorf("repo: %w", err)
		}
		if h := hashName(*p.Name); h != hash {
			pack, err := crypto.Seal(dek, []byte(strings.TrimSpace(*p.Name)))
			if err != nil {
				return Account{}, fmt.Errorf("repo: seal account name: %w", err)
			}
			enc, hash, nameChanged = pack, h, true
		}
	}
	if p.Type != nil {
		if !accountTypeOK[*p.Type] {
			return Account{}, fmt.Errorf("repo: type must be one of %s", strings.Join(AccountTypes, ", "))
		}
		typ = *p.Type
	}
	if p.Currency != nil {
		if !currencyRe.MatchString(*p.Currency) {
			return Account{}, fmt.Errorf("repo: currency must be a 3-letter uppercase ISO-4217 code")
		}
		cur = *p.Currency
	}
	var subtypeArg *string
	subtypeChanged := false
	if p.Subtype != nil {
		subtypeChanged = true
		if inner := *p.Subtype; inner != nil {
			if utf8.RuneCountInString(*inner) > maxSubtypeLen {
				return Account{}, fmt.Errorf("repo: subtype must be at most %d characters", maxSubtypeLen)
			}
			if t := strings.TrimSpace(*inner); t != "" {
				subtypeArg = &t
			}
		}
	}
	var schemaArg *string
	if p.DefaultSchemaID != nil {
		schemaChanged = true
		if inner := *p.DefaultSchemaID; inner != nil && *inner != "" {
			ok, err := s.schemaVisible(ctx, userID, *inner)
			if err != nil {
				return Account{}, err
			}
			if !ok {
				return Account{}, ErrSchemaNotFound
			}
			v := *inner
			schemaArg = &v
		}
	}

	if !nameChanged && p.Type == nil && p.Currency == nil && !subtypeChanged && !schemaChanged {
		// No-op patch: return the current row without an UPDATE.
		a := Account{ID: id, Type: typ, Currency: cur, Subtype: st.String, DefaultSchemaID: ds.String}
		a.Name, err = decryptName(id, enc, dek)
		return a, err
	}

	sets := make([]string, 0, 6)
	args := []any{userID, id}
	addSet := func(col string, val any) {
		args = append(args, val)
		sets = append(sets, col+" = $"+strconv.Itoa(len(args)))
	}
	if nameChanged {
		addSet("name_encrypted", enc)
		addSet("name_hash", hash)
	}
	if p.Type != nil {
		addSet("type", typ)
	}
	if p.Currency != nil {
		addSet("currency", cur)
	}
	if subtypeChanged {
		addSet("subtype", subtypeArg)
	}
	if schemaChanged {
		addSet("default_schema_id", schemaArg)
	}
	sets = append(sets, "updated_at = now()")

	cmdTag, err := s.pool.Exec(ctx,
		"UPDATE accounts SET "+strings.Join(sets, ", ")+" WHERE user_id = $1 AND id = $2",
		args...)
	if err != nil {
		switch pgErrCode(err) {
		case "23505":
			return Account{}, ErrNameTaken
		case "23503":
			return Account{}, ErrSchemaNotFound // schema deleted between check and update
		}
		return Account{}, fmt.Errorf("repo: update account: %w", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return Account{}, ErrAccountNotFound // deleted after the SELECT above
	}
	return s.Get(ctx, userID, id, dek)
}

// Delete removes the user's account (its transactions cascade at the DB
// level, 0002 ON DELETE CASCADE). An unknown or other user's id is
// ErrAccountNotFound; the row is left untouched.
func (s *AccountStore) Delete(ctx context.Context, userID, id string) error {
	cmdTag, err := s.pool.Exec(ctx, `
		DELETE FROM accounts
		WHERE user_id = $1 AND id = $2`, userID, id)
	if err != nil {
		return fmt.Errorf("repo: delete account: %w", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return ErrAccountNotFound
	}
	return nil
}

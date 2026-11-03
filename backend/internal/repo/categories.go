// Per-user category repository (plan step 2.5a): user_categories
// (migration 0001). Names are DEK-encrypted (name_encrypted holds the
// [nonce ‖ ct ‖ tag] pack from internal/crypto); name_hash is the §5
// plaintext uniqueness/lookup key (UNIQUE (user_id, name_hash)).
//
// Parenting: parent_id is a plain FK on user_categories.id — it is NOT
// scoped to user_id. The scoped check therefore lives in this repo
// (INSERT…SELECT for create, pre-check for update): without it a category
// could parent to another user's row, and that user's later DELETE would
// cascade across users (0001 ON DELETE CASCADE). Delete of a category
// cascades to its own children at the DB level.
package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"finance-tracker/internal/crypto"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultCategoryNames is the list SeedDefaults inserts for a fresh user
// (onboarding, plan 1.4/2.5). It mirrors DEFAULT_CATEGORY_NAMES in
// frontend/src/lib/settings.ts — the onboarding form previews these — and
// TestDefaultsMatchFrontend pins the two against drift.
var DefaultCategoryNames = []string{
	"Housing",
	"Utilities",
	"Groceries",
	"Transportation",
	"Dining out",
	"Health",
	"Insurance",
	"Entertainment",
	"Subscriptions",
	"Travel",
	"Savings",
	"Other",
}

// Category is one user_categories row. Name is the decrypted name (the
// stored pack is never exposed); ParentID is "" for a root category.
type Category struct {
	ID        string
	Name      string
	ParentID  string
	Active    bool
	SortOrder int
}

// CategoryIn is the Create input. ParentID uses the pointer nullable
// idiom: nil or "" = root; non-empty = the id of an existing category
// owned by the same user (cross-user parents are rejected, see header).
type CategoryIn struct {
	Name     string
	ParentID *string
}

// CategoryPatch is the Update input; every field is a pointer, nil =
// untouched. ParentID is a double pointer: nil = untouched, non-nil with
// a nil inner pointer = demote to root, otherwise the (same-user) parent
// id.
type CategoryPatch struct {
	Name     *string
	ParentID **string
	Active   *bool
}

// CategoryStore persists user_categories. One store serves every user;
// each method takes a userID and scopes its SQL on it.
type CategoryStore struct {
	pool *pgxpool.Pool
}

// NewCategoryStore returns a CategoryStore backed by pool (nil for a nil
// pool, mirroring the 1.3c UserStore constructor).
func NewCategoryStore(pool *pgxpool.Pool) *CategoryStore {
	if pool == nil {
		return nil
	}
	return &CategoryStore{pool: pool}
}

// List returns the user's categories ordered by sort_order (ties:
// created_at). dek decrypts the names.
func (s *CategoryStore) List(ctx context.Context, userID string, dek []byte) ([]Category, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name_encrypted, parent_id, active, sort_order
		FROM user_categories
		WHERE user_id = $1
		ORDER BY sort_order, created_at`, userID)
	if err != nil {
		return nil, fmt.Errorf("repo: list categories: %w", err)
	}
	defer rows.Close()
	out := make([]Category, 0, 16)
	for rows.Next() {
		var (
			c       Category
			nameEnc []byte
			parent  sql.NullString
		)
		if err := rows.Scan(&c.ID, &nameEnc, &parent, &c.Active, &c.SortOrder); err != nil {
			return nil, fmt.Errorf("repo: scan category: %w", err)
		}
		c.Name, err = decryptName(c.ID, nameEnc, dek)
		if err != nil {
			return nil, err
		}
		c.ParentID = parent.String
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo: list categories: %w", err)
	}
	return out, nil
}

// Get returns one of the user's categories, or ErrCategoryNotFound when
// the id is not the user's. The repo does not distinguish "no such id"
// from "somebody else's id": existence is not disclosed across users.
func (s *CategoryStore) Get(ctx context.Context, userID, id string, dek []byte) (Category, error) {
	var (
		nameEnc []byte
		parent  sql.NullString
		active  bool
		sort    int
	)
	err := s.pool.QueryRow(ctx, `
		SELECT name_encrypted, parent_id, active, sort_order
		FROM user_categories
		WHERE user_id = $1 AND id = $2`, userID, id).
		Scan(&nameEnc, &parent, &active, &sort)
	if errors.Is(err, sql.ErrNoRows) {
		return Category{}, ErrCategoryNotFound
	}
	if err != nil {
		return Category{}, fmt.Errorf("repo: get category: %w", err)
	}
	return categoryFrom(id, nameEnc, parent, active, sort, dek)
}

// categoryFrom assembles a Category from a row's columns, decrypting the
// name under the caller's DEK.
func categoryFrom(id string, nameEnc []byte, parent sql.NullString, active bool, sort int, dek []byte) (Category, error) {
	name, err := decryptName(id, nameEnc, dek)
	if err != nil {
		return Category{}, err
	}
	return Category{ID: id, Name: name, ParentID: parent.String, Active: active, SortOrder: sort}, nil
}

// decryptName opens the name_encrypted pack; a pack that does not open is
// a key/data mismatch the API surfaces as 500 (the DEK came from the
// user's own wrapped_dek, so this is corruption, not a client error).
func decryptName(id string, pack []byte, dek []byte) (string, error) {
	name, err := crypto.Open(dek, pack)
	if err != nil {
		return "", fmt.Errorf("repo: decrypt category %s: %w", id, err)
	}
	return string(name), nil
}

// Create inserts a category for the user and returns it (active, root
// unless ParentID is set, sort_order 0). A duplicate name for this user
// is ErrNameTaken; a missing or other user's parent is ErrParentNotFound.
func (s *CategoryStore) Create(ctx context.Context, userID string, in CategoryIn, dek []byte) (Category, error) {
	if err := validateName(in.Name); err != nil {
		return Category{}, fmt.Errorf("repo: %w", err)
	}
	name := strings.TrimSpace(in.Name)
	pack, err := crypto.Seal(dek, []byte(name))
	if err != nil {
		return Category{}, fmt.Errorf("repo: seal category name: %w", err)
	}
	hash := hashName(name)

	parentID := ""
	if in.ParentID != nil {
		parentID = *in.ParentID
	}
	if parentID != "" {
		// One statement does the scoped parent check and the insert: the
		// SELECT matches only this user's rows, so a missing or other
		// user's parent yields no RETURNING row.
		var id string
		err := s.pool.QueryRow(ctx, `
			INSERT INTO user_categories (user_id, name_encrypted, name_hash, parent_id)
			SELECT $1, $2, $3, p.id
			FROM user_categories p
			WHERE p.user_id = $1 AND p.id = $4
			RETURNING id`, userID, pack, hash, parentID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return Category{}, ErrParentNotFound
		}
		if err != nil {
			switch pgErrCode(err) {
			case "23505":
				return Category{}, ErrNameTaken
			case "23503":
				return Category{}, ErrParentNotFound // parent deleted before the FK check
			}
			return Category{}, fmt.Errorf("repo: create category: %w", err)
		}
		return Category{ID: id, Name: name, ParentID: parentID, Active: true, SortOrder: 0}, nil
	}

	var id string
	err = s.pool.QueryRow(ctx, `
		INSERT INTO user_categories (user_id, name_encrypted, name_hash)
		VALUES ($1, $2, $3)
		RETURNING id`, userID, pack, hash).Scan(&id)
	if err != nil {
		if pgErrCode(err) == "23505" {
			return Category{}, ErrNameTaken
		}
		return Category{}, fmt.Errorf("repo: create category: %w", err)
	}
	return Category{ID: id, Name: name, Active: true, SortOrder: 0}, nil
}

// Update applies the non-nil fields of p to the user's category and
// returns the updated row. Renaming re-encrypts under the same DEK and
// the unique (user_id, name_hash) still applies, so renaming onto an
// existing sibling's name is ErrNameTaken.
func (s *CategoryStore) Update(ctx context.Context, userID, id string, p CategoryPatch, dek []byte) (Category, error) {
	var (
		nameEnc  []byte
		nameHash string
		parent   sql.NullString
		active   bool
		sort     int
	)
	err := s.pool.QueryRow(ctx, `
		SELECT name_encrypted, name_hash, parent_id, active, sort_order
		FROM user_categories
		WHERE user_id = $1 AND id = $2`, userID, id).
		Scan(&nameEnc, &nameHash, &parent, &active, &sort)
	if errors.Is(err, sql.ErrNoRows) {
		return Category{}, ErrCategoryNotFound
	}
	if err != nil {
		return Category{}, fmt.Errorf("repo: get category: %w", err)
	}

	nameChanged, parentChanged := false, false
	if p.Name != nil {
		if err := validateName(*p.Name); err != nil {
			return Category{}, fmt.Errorf("repo: %w", err)
		}
		if h := hashName(*p.Name); h != nameHash {
			pack, err := crypto.Seal(dek, []byte(strings.TrimSpace(*p.Name)))
			if err != nil {
				return Category{}, fmt.Errorf("repo: seal category name: %w", err)
			}
			nameEnc, nameHash, nameChanged = pack, h, true
		}
	}
	if p.ParentID != nil {
		var want string
		if inner := *p.ParentID; inner != nil {
			want = *inner
		}
		if want != parent.String {
			if want != "" {
				// The parent must be this user's (see the package header:
				// the FK alone is not user-scoped).
				var exists bool
				err := s.pool.QueryRow(ctx, `
					SELECT EXISTS (
						SELECT 1 FROM user_categories WHERE user_id = $1 AND id = $2)`,
					userID, want).Scan(&exists)
				if err != nil {
					return Category{}, fmt.Errorf("repo: check parent: %w", err)
				}
				if !exists {
					return Category{}, ErrParentNotFound
				}
				parent = sql.NullString{String: want, Valid: true}
			} else {
				parent = sql.NullString{}
			}
			parentChanged = true
		}
	}
	if p.Active != nil {
		active = *p.Active
	}

	if !nameChanged && !parentChanged && p.Active == nil {
		return categoryFrom(id, nameEnc, parent, active, sort, dek)
	}

	sets := make([]string, 0, 5)
	args := []any{userID, id}
	addSet := func(col string, val any) {
		args = append(args, val)
		sets = append(sets, col+" = $"+strconv.Itoa(len(args)))
	}
	if nameChanged {
		addSet("name_encrypted", nameEnc)
		addSet("name_hash", nameHash)
	}
	if parentChanged {
		var parentArg *string
		if parent.Valid {
			v := parent.String
			parentArg = &v
		}
		addSet("parent_id", parentArg)
	}
	if p.Active != nil {
		addSet("active", active)
	}
	sets = append(sets, "updated_at = now()")

	cmdTag, err := s.pool.Exec(ctx,
		"UPDATE user_categories SET "+strings.Join(sets, ", ")+" WHERE user_id = $1 AND id = $2",
		args...)
	if err != nil {
		switch pgErrCode(err) {
		case "23505":
			return Category{}, ErrNameTaken
		case "23503":
			return Category{}, ErrParentNotFound // parent deleted between check and update
		}
		return Category{}, fmt.Errorf("repo: update category: %w", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return Category{}, ErrCategoryNotFound // deleted after the SELECT above
	}
	return categoryFrom(id, nameEnc, parent, active, sort, dek)
}

// Delete removes the user's category. Its children cascade at the DB
// level (0001 ON DELETE CASCADE). An unknown or other user's id is
// ErrCategoryNotFound; the row is left untouched.
func (s *CategoryStore) Delete(ctx context.Context, userID, id string) error {
	cmdTag, err := s.pool.Exec(ctx, `
		DELETE FROM user_categories
		WHERE user_id = $1 AND id = $2`, userID, id)
	if err != nil {
		return fmt.Errorf("repo: delete category: %w", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return ErrCategoryNotFound
	}
	return nil
}

// SeedDefaults inserts, in one transaction, every name in names that the
// user does not already have (matched by name_hash), each as an active
// root category. sort_order continues after the user's current maximum
// (position i gets max+i+1), so re-seeding appends without disturbing the
// user's existing order. Returns the number of rows inserted; 0 for a
// fully seeded user (idempotent).
func (s *CategoryStore) SeedDefaults(ctx context.Context, userID string, names []string, dek []byte) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("repo: begin seed: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = tx.Rollback(rctx)
		}
	}()

	var base int
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(sort_order), -1) + 1 FROM user_categories WHERE user_id = $1`,
		userID).Scan(&base); err != nil {
		return 0, fmt.Errorf("repo: seed sort base: %w", err)
	}

	inserted := 0
	for i, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		pack, err := crypto.Seal(dek, []byte(name))
		if err != nil {
			return 0, fmt.Errorf("repo: seed category %q: %w", name, err)
		}
		hash := hashName(name)
		var id string
		err = tx.QueryRow(ctx, `
			INSERT INTO user_categories (user_id, name_encrypted, name_hash, sort_order)
			SELECT $1, $2, $3, $4
			WHERE NOT EXISTS (
				SELECT 1 FROM user_categories WHERE user_id = $1 AND name_hash = $3)
			RETURNING id`, userID, pack, hash, base+i).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			continue // already present for this user
		}
		if err != nil {
			return 0, fmt.Errorf("repo: seed category %q: %w", name, err)
		}
		inserted++
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("repo: commit seed: %w", err)
	}
	committed = true
	return inserted, nil
}

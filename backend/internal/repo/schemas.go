// Import-schema repository (plan step 2.5b): import_schemas
// (migration 0001). Since 0018 the table is CUSTOM-ONLY: every row is
// per-user (user_id NOT NULL, name DEK-encrypted) — the former global
// builtins were deleted, and the code builtins in internal/presets
// (served by GET /v1/imports/presets) are the sole preset source
// (D-029).
//
// A schema is usable by a user iff it is owned by that user;
// Get/Update/Delete treat "no such id" and "somebody else's schema"
// identically (ErrSchemaNotFound) — existence is not disclosed across
// users.
//
// kind is immutable on update: the importer (step 4.x) compiles its
// parsing pipeline against it, so retyping a preset in place would
// silently change what its stored spec means.
package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"finance-tracker/internal/crypto"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SchemaKinds is the 0001 import_schemas.kind vocabulary.
var SchemaKinds = []string{"csv", "ofx", "json"}

var schemaKindOK = func() map[string]bool {
	m := make(map[string]bool, len(SchemaKinds))
	for _, k := range SchemaKinds {
		m[k] = true
	}
	return m
}()

// Schema is one import_schemas row (customs only since 0018). Name is
// the decrypted name. Spec is the raw jsonb (a JSON object).
type Schema struct {
	ID   string
	Name string
	Kind string
	Spec json.RawMessage
}

// SchemaIn is the Create input.
type SchemaIn struct {
	Name string
	Kind string
	Spec json.RawMessage
}

// SchemaPatch is the Update input; nil = untouched. kind is
// deliberately absent — it is immutable (see package header).
type SchemaPatch struct {
	Name *string
	Spec *json.RawMessage
}

// validateSpec requires a non-empty JSON object: the spec is the
// field-mapping document the importer (4.x) compiles, so a bare
// array/scalar is rejected here rather than at import time.
func validateSpec(spec json.RawMessage) error {
	if len(spec) == 0 || !json.Valid(spec) {
		return errors.New("spec must be a non-empty JSON document")
	}
	var obj map[string]any
	if err := json.Unmarshal(spec, &obj); err != nil {
		return errors.New("spec must be a JSON object")
	}
	return nil
}

// SchemaStore persists import_schemas. One store serves every user;
// each method takes a userID and scopes its SQL on it.
type SchemaStore struct {
	pool *pgxpool.Pool
}

// NewSchemaStore returns a SchemaStore backed by pool (nil for a nil
// pool, mirroring the 1.3c UserStore constructor).
func NewSchemaStore(pool *pgxpool.Pool) *SchemaStore {
	if pool == nil {
		return nil
	}
	return &SchemaStore{pool: pool}
}

// schemaFrom assembles a Schema from a row's columns; the name is
// decrypted under the caller's DEK.
func schemaFrom(id string, enc []byte, kind string, spec []byte, dek []byte) (Schema, error) {
	n, err := decryptName(id, enc, dek)
	if err != nil {
		return Schema{}, err
	}
	return Schema{ID: id, Name: n, Kind: kind, Spec: json.RawMessage(spec)}, nil
}

// List returns the user's custom schemas in created_at, id order. dek
// decrypts the names.
func (s *SchemaStore) List(ctx context.Context, userID string, dek []byte) ([]Schema, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name_encrypted, kind, spec
		FROM import_schemas
		WHERE user_id = $1
		ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, fmt.Errorf("repo: list schemas: %w", err)
	}
	defer rows.Close()
	out := make([]Schema, 0, 8)
	for rows.Next() {
		var (
			id  string
			enc []byte
			nd  string
			sp  []byte
		)
		if err := rows.Scan(&id, &enc, &nd, &sp); err != nil {
			return nil, fmt.Errorf("repo: scan schema: %w", err)
		}
		sc, err := schemaFrom(id, enc, nd, sp, dek)
		if err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo: list schemas: %w", err)
	}
	return out, nil
}

// Get returns the user's own schema, or ErrSchemaNotFound otherwise.
func (s *SchemaStore) Get(ctx context.Context, userID, id string, dek []byte) (Schema, error) {
	var (
		enc []byte
		nd  string
		sp  []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT name_encrypted, kind, spec
		FROM import_schemas
		WHERE id = $1 AND user_id = $2`, id, userID).
		Scan(&enc, &nd, &sp)
	if errors.Is(err, sql.ErrNoRows) {
		return Schema{}, ErrSchemaNotFound
	}
	if err != nil {
		return Schema{}, fmt.Errorf("repo: get schema: %w", err)
	}
	return schemaFrom(id, enc, nd, sp, dek)
}

// Create inserts a custom schema for the user and returns it. The 0018
// shape CHECK (user_id set, name encrypted) holds by construction of
// the INSERT.
func (s *SchemaStore) Create(ctx context.Context, userID string, in SchemaIn, dek []byte) (Schema, error) {
	if err := validateName(in.Name); err != nil {
		return Schema{}, fmt.Errorf("repo: %w", err)
	}
	if !schemaKindOK[in.Kind] {
		return Schema{}, fmt.Errorf("repo: kind must be one of %s", strings.Join(SchemaKinds, ", "))
	}
	if err := validateSpec(in.Spec); err != nil {
		return Schema{}, fmt.Errorf("repo: %w", err)
	}
	name := strings.TrimSpace(in.Name)
	pack, err := crypto.Seal(dek, []byte(name))
	if err != nil {
		return Schema{}, fmt.Errorf("repo: seal schema name: %w", err)
	}
	var id string
	err = s.pool.QueryRow(ctx, `
		INSERT INTO import_schemas (user_id, name_encrypted, kind, spec)
		VALUES ($1, $2, $3, $4)
		RETURNING id`, userID, pack, in.Kind, string(in.Spec)).Scan(&id)
	if err != nil {
		return Schema{}, fmt.Errorf("repo: create schema: %w", err)
	}
	return Schema{ID: id, Name: name, Kind: in.Kind, Spec: in.Spec}, nil
}

// Update applies the non-nil fields of p to the user's schema and
// returns the updated row.
func (s *SchemaStore) Update(ctx context.Context, userID, id string, p SchemaPatch, dek []byte) (Schema, error) {
	var (
		enc []byte
		nd  string
		sp  []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT name_encrypted, kind, spec
		FROM import_schemas
		WHERE id = $1 AND user_id = $2`, id, userID).
		Scan(&enc, &nd, &sp)
	if errors.Is(err, sql.ErrNoRows) {
		return Schema{}, ErrSchemaNotFound
	}
	if err != nil {
		return Schema{}, fmt.Errorf("repo: get schema: %w", err)
	}

	nameChanged, specChanged := false, false
	if p.Name != nil {
		if err := validateName(*p.Name); err != nil {
			return Schema{}, fmt.Errorf("repo: %w", err)
		}
		pack, err := crypto.Seal(dek, []byte(strings.TrimSpace(*p.Name)))
		if err != nil {
			return Schema{}, fmt.Errorf("repo: seal schema name: %w", err)
		}
		enc, nameChanged = pack, true
	}
	var specArg *string
	if p.Spec != nil {
		if err := validateSpec(*p.Spec); err != nil {
			return Schema{}, fmt.Errorf("repo: %w", err)
		}
		v := string(*p.Spec)
		specArg = &v
		specChanged = true
	}

	if !nameChanged && !specChanged {
		return schemaFrom(id, enc, nd, sp, dek)
	}

	sets := make([]string, 0, 3)
	args := []any{userID, id}
	addSet := func(col string, val any) {
		args = append(args, val)
		sets = append(sets, col+" = $"+strconv.Itoa(len(args)))
	}
	if nameChanged {
		addSet("name_encrypted", enc)
	}
	if specChanged {
		addSet("spec", specArg)
	}
	sets = append(sets, "updated_at = now()")

	cmdTag, err := s.pool.Exec(ctx,
		"UPDATE import_schemas SET "+strings.Join(sets, ", ")+" WHERE user_id = $1 AND id = $2",
		args...)
	if err != nil {
		return Schema{}, fmt.Errorf("repo: update schema: %w", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return Schema{}, ErrSchemaNotFound // deleted after the SELECT above
	}
	return s.Get(ctx, userID, id, dek)
}

// Delete removes the user's schema. Accounts pointing at the deleted
// schema have default_schema_id SET NULL by the 0002 FK.
func (s *SchemaStore) Delete(ctx context.Context, userID, id string) error {
	cmdTag, err := s.pool.Exec(ctx, `
		DELETE FROM import_schemas
		WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return fmt.Errorf("repo: delete schema: %w", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return ErrSchemaNotFound // unknown id or another user's row
	}
	return nil
}

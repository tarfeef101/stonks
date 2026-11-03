// Import-schema repository (plan step 2.5b): import_schemas
// (migration 0001). Two shapes under one table (0001 shape CHECK):
// builtins are global and public (user_id NULL, builtin true,
// name plaintext) — they are read-only and maintained by
// EnsureBuiltins; customs are per-user (user_id NOT NULL, name
// DEK-encrypted).
//
// A schema is usable by a user iff it is a builtin or owned by that
// user; Get/Update/Delete treat "no such id" and "somebody else's
// custom" identically (ErrSchemaNotFound) — existence is not disclosed
// across users.
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
	"time"

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

// builtinSchemas are the three global presets EnsureBuiltins maintains
// (idempotent per (name, kind)). Specs start empty: field mappings are
// the user's (or the importer's defaults, step 4.x).
var builtinSchemas = []struct {
	Name string
	Kind string
	Spec json.RawMessage
}{
	{Name: "Generic CSV", Kind: "csv", Spec: json.RawMessage(`{}`)},
	{Name: "OFX Statement", Kind: "ofx", Spec: json.RawMessage(`{}`)},
	{Name: "Generic JSON", Kind: "json", Spec: json.RawMessage(`{}`)},
}

// Schema is one import_schemas row. Name is plaintext for builtins, the
// decrypted name for customs. Spec is the raw jsonb (a JSON object).
type Schema struct {
	ID      string
	Name    string
	Kind    string
	Builtin bool
	Spec    json.RawMessage
}

// SchemaIn is the Create input for a CUSTOM schema (builtins are
// read-only; see EnsureBuiltins).
type SchemaIn struct {
	Name string
	Kind string
	Spec json.RawMessage
}

// SchemaPatch is the Update input for a CUSTOM schema; nil = untouched.
// kind is deliberately absent — it is immutable (see package header).
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
// each method takes a userID and scopes its SQL on it (builtins are
// shared by construction).
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

// schemaFrom assembles a Schema from a row's columns; customs are
// decrypted under the caller's DEK, builtins read the plaintext name.
func schemaFrom(id string, name sql.NullString, enc []byte, kind string, builtin bool, spec []byte, dek []byte) (Schema, error) {
	s := Schema{ID: id, Kind: kind, Builtin: builtin, Spec: json.RawMessage(spec)}
	if builtin {
		s.Name = name.String
		return s, nil
	}
	n, err := decryptName(id, enc, dek)
	if err != nil {
		return Schema{}, err
	}
	s.Name = n
	return s, nil
}

// List returns the builtins plus the user's custom schemas (builtins
// first). dek decrypts the custom names.
func (s *SchemaStore) List(ctx context.Context, userID string, dek []byte) ([]Schema, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, name_encrypted, kind, builtin, spec
		FROM import_schemas
		WHERE builtin = true OR user_id = $1
		ORDER BY builtin DESC, created_at, id`, userID)
	if err != nil {
		return nil, fmt.Errorf("repo: list schemas: %w", err)
	}
	defer rows.Close()
	out := make([]Schema, 0, 8)
	for rows.Next() {
		var (
			id  string
			nm  sql.NullString
			enc []byte
			nd  string
			bl  bool
			sp  []byte
		)
		if err := rows.Scan(&id, &nm, &enc, &nd, &bl, &sp); err != nil {
			return nil, fmt.Errorf("repo: scan schema: %w", err)
		}
		sc, err := schemaFrom(id, nm, enc, nd, bl, sp, dek)
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

// Get returns a schema visible to the user (builtin or own custom), or
// ErrSchemaNotFound otherwise.
func (s *SchemaStore) Get(ctx context.Context, userID, id string, dek []byte) (Schema, error) {
	var (
		nm  sql.NullString
		enc []byte
		nd  string
		bl  bool
		sp  []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT name, name_encrypted, kind, builtin, spec
		FROM import_schemas
		WHERE id = $1 AND (builtin = true OR user_id = $2)`, id, userID).
		Scan(&nm, &enc, &nd, &bl, &sp)
	if errors.Is(err, sql.ErrNoRows) {
		return Schema{}, ErrSchemaNotFound
	}
	if err != nil {
		return Schema{}, fmt.Errorf("repo: get schema: %w", err)
	}
	return schemaFrom(id, nm, enc, nd, bl, sp, dek)
}

// Create inserts a custom schema for the user and returns it. The
// 0001 shape CHECK (custom ⟹ user_id set, name encrypted) holds by
// construction of the INSERT.
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
		INSERT INTO import_schemas (user_id, name_encrypted, kind, builtin, spec)
		VALUES ($1, $2, $3, false, $4)
		RETURNING id`, userID, pack, in.Kind, string(in.Spec)).Scan(&id)
	if err != nil {
		return Schema{}, fmt.Errorf("repo: create schema: %w", err)
	}
	return Schema{ID: id, Name: name, Kind: in.Kind, Builtin: false, Spec: in.Spec}, nil
}

// Update applies the non-nil fields of p to the user's custom schema
// and returns the updated row. Builtins are read-only
// (ErrBuiltinReadOnly, pre-checked before any mutation).
func (s *SchemaStore) Update(ctx context.Context, userID, id string, p SchemaPatch, dek []byte) (Schema, error) {
	var (
		nm  sql.NullString
		enc []byte
		nd  string
		bl  bool
		sp  []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT name, name_encrypted, kind, builtin, spec
		FROM import_schemas
		WHERE id = $1 AND (builtin = true OR user_id = $2)`, id, userID).
		Scan(&nm, &enc, &nd, &bl, &sp)
	if errors.Is(err, sql.ErrNoRows) {
		return Schema{}, ErrSchemaNotFound
	}
	if err != nil {
		return Schema{}, fmt.Errorf("repo: get schema: %w", err)
	}
	if bl {
		return Schema{}, ErrBuiltinReadOnly
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
		return schemaFrom(id, nm, enc, nd, bl, sp, dek)
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

// Delete removes the user's custom schema. Builtins are read-only
// (ErrBuiltinReadOnly). Accounts pointing at the deleted schema have
// default_schema_id SET NULL by the 0002 FK.
func (s *SchemaStore) Delete(ctx context.Context, userID, id string) error {
	var bl bool
	err := s.pool.QueryRow(ctx, `
		SELECT builtin FROM import_schemas
		WHERE id = $1 AND (builtin = true OR user_id = $2)`, id, userID).Scan(&bl)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrSchemaNotFound
	}
	if err != nil {
		return fmt.Errorf("repo: check schema: %w", err)
	}
	if bl {
		return ErrBuiltinReadOnly
	}
	cmdTag, err := s.pool.Exec(ctx, `
		DELETE FROM import_schemas
		WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return fmt.Errorf("repo: delete schema: %w", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return ErrSchemaNotFound // deleted between check and delete
	}
	return nil
}

// EnsureBuiltins inserts every builtinSchemas entry that is missing
// (matched by name+kind), in one transaction under a
// transaction-scoped advisory lock so concurrent first bootstraps
// serialize instead of racing the insert (the table has no unique
// constraint on builtin names — the lock IS the concurrency guard).
// Returns the number of rows inserted (0 for a populated database).
func (s *SchemaStore) EnsureBuiltins(ctx context.Context) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("repo: begin ensure builtins: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = tx.Rollback(rctx)
		}
	}()

	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext('ftrk-import-schemas-builtins'))`); err != nil {
		return 0, fmt.Errorf("repo: advisory lock: %w", err)
	}

	inserted := 0
	for _, b := range builtinSchemas {
		var id string
		err := tx.QueryRow(ctx, `
			INSERT INTO import_schemas (name, kind, builtin, spec)
			SELECT $1, $2, true, $3
			WHERE NOT EXISTS (
				SELECT 1 FROM import_schemas
				WHERE builtin = true AND name = $1 AND kind = $2)
			RETURNING id`, b.Name, b.Kind, string(b.Spec)).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			continue // already present
		}
		if err != nil {
			return 0, fmt.Errorf("repo: ensure builtin %q: %w", b.Name, err)
		}
		inserted++
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("repo: commit ensure builtins: %w", err)
	}
	committed = true
	return inserted, nil
}

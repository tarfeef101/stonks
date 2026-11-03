// Package migrate_test runs the goose migrations in backend/migrations
// against a disposable postgres:18 container (testcontainers) and pins the
// schema invariants the data layer relies on: identity uniqueness,
// encrypted-name uniqueness, builtin/custom schema shape, FK cascades.
//
// Runs via `make backend-test` (golang:1.27.3 container with the host
// docker.sock mounted — Makefile DOCKER_GO_TC, D-001). Reuses the local
// postgres:18 image from the dev stack.
package migrate_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// Postgres SQLSTATE codes asserted below. Note 23503 (foreign_key_violation)
// is the code PG raises for an FK reference to a missing parent row; 23F03
// (referential_integrity_violation) is for other RI checks.
const (
	codeUniqueViolation     = "23505"
	codeCheckViolation      = "23514"
	codeForeignKeyViolation = "23503"
)

// migrationsDir is relative to this package's directory (go test's cwd).
const migrationsDir = "../../migrations"

func pgErrCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func assertViolation(t *testing.T, err error, code, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected SQLSTATE %s, got success", what, code)
	}
	if got := pgErrCode(err); got != code {
		t.Fatalf("%s: expected SQLSTATE %s, got %q: %v", what, code, got, err)
	}
}

func countRows(t *testing.T, db *sql.DB, ctx context.Context, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

// migratedDB starts a disposable postgres:18 container, applies all
// migrations with goose, and returns a connected *sql.DB. The container is
// stopped and the DB closed when the test ends.
//
// Readiness: in testcontainers v0.44.0, postgres.Run with no wait strategy
// only waits for the container to START; the postgres image then spends a
// few seconds on initdb + server startup before it accepts connections.
// BasicWaitStrategies (the module's own readiness wait) covers that; the
// ping retry loop below is a hard guarantee on top, because on this host
// the readiness log was observed ~1.5–2 s before the mapped port actually
// served connections (root-caused in step 2.1, D-019).
func migratedDB(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	ctx := context.Background()

	ctr, err := postgres.Run(ctx, "postgres:18", postgres.BasicWaitStrategies())
	if err != nil {
		t.Fatalf("testcontainers postgres: %v", err)
	}
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = ctr.Stop(sctx, nil)
	})

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := pingUntilReady(ctx, db, 60*time.Second); err != nil {
		t.Fatalf("postgres readiness: %v", err)
	}

	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("goose dialect: %v", err)
	}
	if err := goose.UpContext(ctx, db, migrationsDir); err != nil {
		t.Fatalf("goose up: %v", err)
	}
	return db, ctx
}

// pingUntilReady polls db.PingContext until it succeeds or the budget runs
// out. See migratedDB for why the built-in wait alone is not trusted.
func pingUntilReady(ctx context.Context, db *sql.DB, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	var lastErr error
	for {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		lastErr = db.PingContext(pctx)
		cancel()
		if lastErr == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("after %v: %w", budget, lastErr)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// TestGooseLifecycle covers the done-when: a fresh DB migrates cleanly to
// version 1 (all four tables present), a second `up` is a no-op, and `down`
// rolls everything back.
func TestGooseLifecycle(t *testing.T) {
	db, ctx := migratedDB(t)

	ver, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		t.Fatalf("version after up: %v", err)
	}
	if ver != 1 {
		t.Fatalf("version after up = %d, want 1", ver)
	}
	for _, table := range []string{"users", "user_categories", "import_schemas", "notifications"} {
		n := countRows(t, db, ctx,
			`SELECT count(*) FROM information_schema.tables
			  WHERE table_schema = 'public' AND table_name = $1`, table)
		if n != 1 {
			t.Fatalf("table %s present = %d, want 1", table, n)
		}
	}

	// Idempotency — `make dev` runs the migrate service on every boot.
	if err := goose.UpContext(ctx, db, migrationsDir); err != nil {
		t.Fatalf("idempotent up: %v", err)
	}
	if ver, err = goose.GetDBVersionContext(ctx, db); err != nil || ver != 1 {
		t.Fatalf("version after re-up = %d (err %v), want 1", ver, err)
	}

	if err := goose.DownContext(ctx, db, migrationsDir); err != nil {
		t.Fatalf("goose down: %v", err)
	}
	if ver, err = goose.GetDBVersionContext(ctx, db); err != nil || ver != 0 {
		t.Fatalf("version after down = %d (err %v), want 0", ver, err)
	}
	n := countRows(t, db, ctx,
		`SELECT count(*) FROM information_schema.tables
		  WHERE table_schema = 'public'
		    AND table_name IN ('users', 'user_categories', 'import_schemas', 'notifications')`)
	if n != 0 {
		t.Fatalf("tables remaining after down = %d, want 0", n)
	}
}

// TestSchemaConstraints pins the 0001 invariants: users identity + defaults,
// per-user category uniqueness and parent rules, import_schemas kind +
// builtin/custom shape, notifications rules, and the cascade semantics.
func TestSchemaConstraints(t *testing.T) {
	db, ctx := migratedDB(t)

	// --- users ----------------------------------------------------------

	// Defaults: only auth0_sub + wrapped_dek (plus email/display_name,
	// which step 1.3 fills from the ID token) are the required inputs.
	var (
		userID     string
		lang, cur  string
		per        string
		mo, qu, an bool
		createdAt  time.Time
	)
	err := db.QueryRowContext(ctx, `
		INSERT INTO users (auth0_sub, email, display_name, wrapped_dek)
		VALUES ('sub-1', 'a@example.com', 'A', $1)
		RETURNING id, language, default_currency, default_period,
		          notify_monthly, notify_quarterly, notify_annual, created_at`,
		[]byte("wrapped-dek-pack")).Scan(&userID, &lang, &cur, &per, &mo, &qu, &an, &createdAt)
	if err != nil {
		t.Fatalf("insert minimal user: %v", err)
	}
	if userID == "" {
		t.Fatal("user id defaulted to empty, want a uuid")
	}
	if lang != "en" || cur != "USD" || per != "month" || mo || qu || an {
		t.Fatalf("defaults = (%q, %q, %q, %v, %v, %v), want (en, USD, month, false, false, false)",
			lang, cur, per, mo, qu, an)
	}
	if createdAt.IsZero() {
		t.Fatal("created_at defaulted to zero")
	}

	// Duplicate auth0_sub is the idempotency key for user bootstrap
	// (step 1.3): a repeat login must never create a second row.
	_, err = db.ExecContext(ctx, `
		INSERT INTO users (auth0_sub, email, display_name, wrapped_dek)
		VALUES ('sub-1', 'b@example.com', 'B', $1)`, []byte("x"))
	assertViolation(t, err, codeUniqueViolation, "duplicate auth0_sub")

	_, err = db.ExecContext(ctx, `
		INSERT INTO users (auth0_sub, email, display_name, wrapped_dek, default_period)
		VALUES ('sub-2', 'c@example.com', 'C', $1, 'week')`, []byte("x"))
	assertViolation(t, err, codeCheckViolation, "default_period outside vocabulary")

	// --- user_categories --------------------------------------------------

	insertCategory := func(ownerID, nameHash string, parentID *string) (string, error) {
		var id string
		err := db.QueryRowContext(ctx, `
			INSERT INTO user_categories (user_id, name_encrypted, name_hash, parent_id, sort_order)
			VALUES ($1, $2, $3, $4, 0) RETURNING id`,
			ownerID, []byte("name-pack"), nameHash, parentID).Scan(&id)
		return id, err
	}

	catA, err := insertCategory(userID, "hash-a", nil)
	if err != nil {
		t.Fatalf("insert category: %v", err)
	}
	if _, err := insertCategory(userID, "hash-a", nil); err == nil {
		t.Fatal("duplicate (user_id, name_hash) accepted, want unique violation")
	} else if got := pgErrCode(err); got != codeUniqueViolation {
		t.Fatalf("duplicate (user_id, name_hash): SQLSTATE %q, want %s: %v", got, codeUniqueViolation, err)
	}

	// Same hash under a different user is fine (per-user namespaces).
	otherSub := "sub-3"
	var userID2 string
	err = db.QueryRowContext(ctx, `
		INSERT INTO users (auth0_sub, email, display_name, wrapped_dek)
		VALUES ($1, 'd@example.com', 'D', $2) RETURNING id`, otherSub, []byte("dek2")).Scan(&userID2)
	if err != nil {
		t.Fatalf("insert second user: %v", err)
	}
	if _, err := insertCategory(userID2, "hash-a", nil); err != nil {
		t.Fatalf("same name_hash under another user: %v", err)
	}

	ghost := "22222222-2222-3333-4444-555555555555"
	_, err = db.ExecContext(ctx, `
		INSERT INTO user_categories (user_id, name_encrypted, name_hash)
		VALUES ($1, $2, 'orphan')`, ghost, []byte("name-pack"))
	assertViolation(t, err, codeForeignKeyViolation, "category without user")

	// Self-parent is impossible (the CHECK uses the same value for both
	// columns in one INSERT).
	selfID := "11111111-2222-3333-4444-555555555555"
	_, err = db.ExecContext(ctx, `
		INSERT INTO user_categories (id, user_id, name_encrypted, name_hash, parent_id)
		VALUES ($1, $2, $3, 'self-parent', $1)`, selfID, userID, []byte("name-pack"))
	assertViolation(t, err, codeCheckViolation, "self-parent category")

	if _, err := insertCategory(userID, "hash-b", &ghost); err == nil {
		t.Fatal("category with missing parent accepted, want FK violation")
	} else if got := pgErrCode(err); got != codeForeignKeyViolation {
		t.Fatalf("missing parent: SQLSTATE %q, want %s: %v", got, codeForeignKeyViolation, err)
	}

	// --- import_schemas ---------------------------------------------------

	_, err = db.ExecContext(ctx, `
		INSERT INTO import_schemas (user_id, name_encrypted, kind, builtin, spec)
		VALUES ($1, $2, 'xml', false, '{}')`, userID, []byte("name-pack"))
	assertViolation(t, err, codeCheckViolation, "kind outside csv/ofx/json")

	_, err = db.ExecContext(ctx, `
		INSERT INTO import_schemas (user_id, name, kind, builtin, spec)
		VALUES ($1, 'Mine', 'csv', true, '{}')`, userID)
	assertViolation(t, err, codeCheckViolation, "builtin with user_id")

	_, err = db.ExecContext(ctx, `
		INSERT INTO import_schemas (user_id, name, kind, builtin, spec)
		VALUES ($1, 'Mine', 'csv', false, '{}')`, userID)
	assertViolation(t, err, codeCheckViolation, "custom schema with plaintext name")

	// Valid shapes: a global builtin (public name) and a per-user custom
	// (DEK-encrypted name only).
	var builtinID, customID string
	err = db.QueryRowContext(ctx, `
		INSERT INTO import_schemas (name, kind, builtin, spec)
		VALUES ('Bank CSV', 'csv', true, '{"delimiter": ","}') RETURNING id`).Scan(&builtinID)
	if err != nil {
		t.Fatalf("insert builtin schema: %v", err)
	}
	err = db.QueryRowContext(ctx, `
		INSERT INTO import_schemas (user_id, name_encrypted, kind, builtin, spec)
		VALUES ($1, $2, 'ofx', false, '{}') RETURNING id`, userID, []byte("name-pack")).Scan(&customID)
	if err != nil {
		t.Fatalf("insert custom schema: %v", err)
	}

	// --- notifications ----------------------------------------------------

	_, err = db.ExecContext(ctx, `
		INSERT INTO notifications (user_id, kind, title, body)
		VALUES ($1, 'budget', 't', 'b')`, ghost)
	assertViolation(t, err, codeForeignKeyViolation, "notification without user")

	// read_at earlier than created_at is impossible (CHECK read_at >=
	// created_at) — a notification is read no earlier than it was created.
	_, err = db.ExecContext(ctx, `
		INSERT INTO notifications (user_id, kind, title, body, read_at, created_at)
		VALUES ($1, 'budget', 't', 'b',
		        now() - interval '2 hours', now() - interval '1 hour')`, userID)
	assertViolation(t, err, codeCheckViolation, "read_at before created_at")

	// --- cascades -----------------------------------------------------------

	// Delete user 2 and verify every per-user row goes with it —
	// categories (incl. children via the self-FK), custom schemas,
	// notifications. The global builtin survives.
	var childID string
	parentID, err := insertCategory(userID2, "hash-p", nil)
	if err != nil {
		t.Fatalf("insert parent category: %v", err)
	}
	if childID, err = insertCategory(userID2, "hash-c", &parentID); err != nil {
		t.Fatalf("insert child category: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO notifications (user_id, kind, title, body)
		VALUES ($1, 'budget', 't', 'b')`, userID2); err != nil {
		t.Fatalf("insert notification: %v", err)
	}

	if _, err := db.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID2); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if n := countRows(t, db, ctx, `SELECT count(*) FROM user_categories WHERE user_id = $1 OR parent_id = $1`, userID2); n != 0 {
		t.Fatalf("categories surviving user delete = %d, want 0", n)
	}
	if n := countRows(t, db, ctx, `SELECT count(*) FROM user_categories WHERE id = $1`, childID); n != 0 {
		t.Fatalf("child category survived parent cascade")
	}
	if n := countRows(t, db, ctx, `SELECT count(*) FROM import_schemas WHERE user_id = $1`, userID2); n != 0 {
		t.Fatalf("custom schemas surviving user delete = %d, want 0", n)
	}
	if n := countRows(t, db, ctx, `SELECT count(*) FROM notifications WHERE user_id = $1`, userID2); n != 0 {
		t.Fatalf("notifications surviving user delete = %d, want 0", n)
	}
	if n := countRows(t, db, ctx, `SELECT count(*) FROM import_schemas WHERE id = $1`, builtinID); n != 1 {
		t.Fatalf("global builtin survived? = %d, want 1", n)
	}
	if n := countRows(t, db, ctx, `SELECT count(*) FROM user_categories WHERE id = $1`, catA); n != 1 {
		t.Fatalf("first user's category should survive, present = %d", n)
	}
	_ = customID
}

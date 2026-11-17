// Package migrate_test runs the goose migrations in backend/migrations
// against a disposable postgres:18 container (testcontainers) and pins the
// schema invariants the data layer relies on: identity uniqueness,
// encrypted-name uniqueness, builtin/custom schema shape, account type
// vocabulary, the transaction dedup key, the shared period vocabulary,
// per-user LLM-provider uniqueness, FK cascades.
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
// version 17 (all nineteen tables present, fx_rates from 0017 on top of
// saved_views from 0015 on top of dashboard_widgets.row_span from 0014 on
// top of dashboards +
// dashboard_widgets from 0013 on top of
// offset_suggestions + transactions.offset_group_id from 0012 on top of
// users.theme from 0011 on top of fi_institutions + fi_credentials from
// 0010 on top of mortgages from 0009 + dashboard_cards from 0008 +
// categorise_jobs from 0007 + import_jobs' 0006 mapping column), a second
// `up` is a no-op, and `down` rolls back exactly one step (0017 drops
// fx_rates; saved_views keeps 0015, dashboard_widgets keeps 0014's row_span,
// the dashboards +
// dashboard_widgets tables keep 0013, offset_suggestions keeps 0012's
// transactions.offset_group_id, users keeps its 0011 theme, and the fi
// tables, mortgages, dashboard_cards, categorise_jobs, import_jobs' 0006
// mapping column and 0001+0002+0003+0004 stay in place, users keeps its
// 0004 onboarded_at column).
func TestGooseLifecycle(t *testing.T) {
	db, ctx := migratedDB(t)

	ver, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		t.Fatalf("version after up: %v", err)
	}
	if ver != 17 {
		t.Fatalf("version after up = %d, want 17", ver)
	}
	for _, table := range []string{
		"users", "user_categories", "import_schemas", "notifications",
		"accounts", "transactions", "budgets", "llm_credentials",
		"import_jobs", "categorise_jobs", "dashboard_cards", "mortgages",
		"fi_institutions", "fi_credentials", "offset_suggestions",
		"dashboards", "dashboard_widgets", "saved_views", "fx_rates",
	} {
		n := countRows(t, db, ctx,
			`SELECT count(*) FROM information_schema.tables
			  WHERE table_schema = 'public' AND table_name = $1`, table)
		if n != 1 {
			t.Fatalf("table %s present = %d, want 1", table, n)
		}
	}

	if n := countRows(t, db, ctx,
		`SELECT count(*) FROM information_schema.columns
		  WHERE table_schema = 'public' AND table_name = 'users'
		    AND column_name = 'onboarded_at'`); n != 1 {
		t.Fatalf("users.onboarded_at after up = %d, want 1", n)
	}
	if n := countRows(t, db, ctx,
		`SELECT count(*) FROM information_schema.columns
		  WHERE table_schema = 'public' AND table_name = 'import_jobs'
		    AND column_name = 'mapping'`); n != 1 {
		t.Fatalf("import_jobs.mapping after up = %d, want 1", n)
	}
	if n := countRows(t, db, ctx,
		`SELECT count(*) FROM information_schema.columns
		  WHERE table_schema = 'public' AND table_name = 'transactions'
		    AND column_name = 'offset_group_id'`); n != 1 {
		t.Fatalf("transactions.offset_group_id after up = %d, want 1", n)
	}

	// Idempotency — `make dev` runs the migrate service on every boot.
	if err := goose.UpContext(ctx, db, migrationsDir); err != nil {
		t.Fatalf("idempotent up: %v", err)
	}
	if ver, err = goose.GetDBVersionContext(ctx, db); err != nil || ver != 17 {
		t.Fatalf("version after re-up = %d (err %v), want 17", ver, err)
	}

	// One-step down: goose rolls back the latest migration (0017) only —
	// fx_rates goes; 0015 stays (saved_views), 0014 stays
	// (dashboard_widgets.row_span), 0013
	// stays (dashboards + dashboard_widgets), 0012 stays (offset_suggestions
	// plus transactions.offset_group_id), users keeps its 0011 theme, and
	// the fi tables (0010), mortgages (0009), dashboard_cards (0008),
	// categorise_jobs, import_jobs' 0006 mapping column and the
	// 0001+0002+0003+0004 tables stay.
	if err := goose.DownContext(ctx, db, migrationsDir); err != nil {
		t.Fatalf("goose down: %v", err)
	}
	if ver, err = goose.GetDBVersionContext(ctx, db); err != nil || ver != 15 {
		t.Fatalf("version after down = %d (err %v), want 15", ver, err)
	}
	if n := countRows(t, db, ctx,
		`SELECT count(*) FROM information_schema.tables
		  WHERE table_schema = 'public' AND table_name = 'fx_rates'`); n != 0 {
		t.Fatalf("fx_rates remaining after one-step down = %d, want 0 (0017 rolls back)", n)
	}
	if n := countRows(t, db, ctx,
		`SELECT count(*) FROM information_schema.tables
		  WHERE table_schema = 'public' AND table_name = 'saved_views'`); n != 1 {
		t.Fatalf("saved_views remaining after one-step down = %d, want 1 (0015 stays)", n)
	}
	if n := countRows(t, db, ctx,
		`SELECT count(*) FROM information_schema.tables
		  WHERE table_schema = 'public' AND table_name = 'dashboards'`); n != 1 {
		t.Fatalf("dashboards remaining after one-step down = %d, want 1 (only 0017 rolls back)", n)
	}
	if n := countRows(t, db, ctx,
		`SELECT count(*) FROM information_schema.tables
		  WHERE table_schema = 'public' AND table_name = 'dashboard_widgets'`); n != 1 {
		t.Fatalf("dashboard_widgets remaining after one-step down = %d, want 1 (only 0017 rolls back)", n)
	}
	if n := countRows(t, db, ctx,
		`SELECT count(*) FROM information_schema.columns
		  WHERE table_schema = 'public' AND table_name = 'dashboard_widgets'
		    AND column_name = 'row_span'`); n != 1 {
		t.Fatalf("dashboard_widgets.row_span remaining after one-step down = %d, want 1 (0014 stays)", n)
	}
	if n := countRows(t, db, ctx,
		`SELECT count(*) FROM information_schema.tables
		  WHERE table_schema = 'public' AND table_name = 'offset_suggestions'`); n != 1 {
		t.Fatalf("offset_suggestions remaining after one-step down = %d, want 1 (0012 stays)", n)
	}
	if n := countRows(t, db, ctx,
		`SELECT count(*) FROM information_schema.columns
		  WHERE table_schema = 'public' AND table_name = 'transactions'
		    AND column_name = 'offset_group_id'`); n != 1 {
		t.Fatalf("transactions.offset_group_id remaining after one-step down = %d, want 1 (0012 stays)", n)
	}
	if n := countRows(t, db, ctx,
		`SELECT count(*) FROM information_schema.columns
		  WHERE table_schema = 'public' AND table_name = 'users'
		    AND column_name = 'theme'`); n != 1 {
		t.Fatalf("users.theme remaining after one-step down = %d, want 1 (0011 stays)", n)
	}
	if n := countRows(t, db, ctx,
		`SELECT count(*) FROM information_schema.tables
		  WHERE table_schema = 'public' AND table_name = 'mortgages'`); n != 1 {
		t.Fatalf("mortgages remaining after one-step down = %d, want 1 (0009 stays)", n)
	}
	if n := countRows(t, db, ctx,
		`SELECT count(*) FROM information_schema.tables
		  WHERE table_schema = 'public' AND table_name = 'fi_institutions'`); n != 1 {
		t.Fatalf("fi_institutions remaining after one-step down = %d, want 1 (0010 stays)", n)
	}
	if n := countRows(t, db, ctx,
		`SELECT count(*) FROM information_schema.tables
		  WHERE table_schema = 'public' AND table_name = 'fi_credentials'`); n != 1 {
		t.Fatalf("fi_credentials remaining after one-step down = %d, want 1 (0010 stays)", n)
	}
	if n := countRows(t, db, ctx,
		`SELECT count(*) FROM information_schema.tables
		  WHERE table_schema = 'public' AND table_name = 'dashboard_cards'`); n != 1 {
		t.Fatalf("dashboard_cards remaining after one-step down = %d, want 1 (0008 stays)", n)
	}
	if n := countRows(t, db, ctx,
		`SELECT count(*) FROM information_schema.tables
		  WHERE table_schema = 'public' AND table_name = 'categorise_jobs'`); n != 1 {
		t.Fatalf("categorise_jobs remaining after one-step down = %d, want 1 (0007 stays)", n)
	}
	if n := countRows(t, db, ctx,
		`SELECT count(*) FROM information_schema.tables
		  WHERE table_schema = 'public' AND table_name = 'import_jobs'`); n != 1 {
		t.Fatalf("import_jobs remaining after one-step down = %d, want 1", n)
	}
	if n := countRows(t, db, ctx,
		`SELECT count(*) FROM information_schema.columns
		  WHERE table_schema = 'public' AND table_name = 'import_jobs'
		    AND column_name = 'mapping'`); n != 1 {
		t.Fatalf("import_jobs.mapping remaining after one-step down = %d, want 1 (0006 stays)", n)
	}
	n := countRows(t, db, ctx,
		`SELECT count(*) FROM information_schema.tables
		  WHERE table_schema = 'public'
		    AND table_name IN ('users', 'user_categories', 'import_schemas', 'notifications',
		                       'accounts', 'transactions', 'budgets', 'llm_credentials')`)
	if n != 8 {
		t.Fatalf("0001+0002+0003+0004 tables remaining after one-step down = %d, want 8", n)
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
		per, thm   string
		mo, qu, an bool
		createdAt  time.Time
	)
	err := db.QueryRowContext(ctx, `
		INSERT INTO users (auth0_sub, email, display_name, wrapped_dek)
		VALUES ('sub-1', 'a@example.com', 'A', $1)
		RETURNING id, language, default_currency, default_period, theme,
		          notify_monthly, notify_quarterly, notify_annual, created_at`,
		[]byte("wrapped-dek-pack")).Scan(&userID, &lang, &cur, &per, &thm, &mo, &qu, &an, &createdAt)
	if err != nil {
		t.Fatalf("insert minimal user: %v", err)
	}
	if userID == "" {
		t.Fatal("user id defaulted to empty, want a uuid")
	}
	if lang != "en" || cur != "USD" || per != "month" || thm != "system" || mo || qu || an {
		t.Fatalf("defaults = (%q, %q, %q, %q, %v, %v, %v), want (en, USD, month, system, false, false, false)",
			lang, cur, per, thm, mo, qu, an)
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

	// theme shares the same CHECK-defined vocabulary discipline (0011).
	_, err = db.ExecContext(ctx, `
		INSERT INTO users (auth0_sub, email, display_name, wrapped_dek, theme)
		VALUES ('sub-2', 'c@example.com', 'C', $1, 'sepia')`, []byte("x"))
	assertViolation(t, err, codeCheckViolation, "theme outside vocabulary")

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

// TestAccountTransactionConstraints pins the 0002 invariants: account
// type vocabulary + per-user encrypted-name uniqueness, the transaction
// dedup key (account_id, date_posted, dedup_hash), the date rule, and
// cascade semantics (accounts/transactions CASCADE, categories SET NULL).
func TestAccountTransactionConstraints(t *testing.T) {
	db, ctx := migratedDB(t)

	var userID, otherUserID, schemaID string
	err := db.QueryRowContext(ctx, `
		INSERT INTO users (auth0_sub, email, display_name, wrapped_dek)
		VALUES ('sub-acc', 'a@example.com', 'A', $1) RETURNING id`,
		[]byte("wrapped-dek-pack")).Scan(&userID)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	err = db.QueryRowContext(ctx, `
		INSERT INTO users (auth0_sub, email, display_name, wrapped_dek)
		VALUES ('sub-acc2', 'b@example.com', 'B', $1) RETURNING id`,
		[]byte("wrapped-dek-pack2")).Scan(&otherUserID)
	if err != nil {
		t.Fatalf("insert second user: %v", err)
	}
	err = db.QueryRowContext(ctx, `
		INSERT INTO import_schemas (name, kind, builtin, spec)
		VALUES ('Bank CSV', 'csv', true, '{}') RETURNING id`).Scan(&schemaID)
	if err != nil {
		t.Fatalf("insert builtin schema: %v", err)
	}

	// --- accounts ---------------------------------------------------------

	var acctID, acctID2 string
	err = db.QueryRowContext(ctx, `
		INSERT INTO accounts
			(user_id, name_encrypted, name_hash, type, subtype, currency, default_schema_id)
		VALUES ($1, $2, 'hash-a', 'bank', 'chequing', 'CAD', $3) RETURNING id`,
		userID, []byte("name-pack"), schemaID).Scan(&acctID)
	if err != nil {
		t.Fatalf("insert account with schema: %v", err)
	}
	err = db.QueryRowContext(ctx, `
		INSERT INTO accounts (user_id, name_encrypted, name_hash, type, currency)
		VALUES ($1, $2, 'hash-b', 'credit_card', 'USD') RETURNING id`,
		userID, []byte("name-pack2")).Scan(&acctID2)
	if err != nil {
		t.Fatalf("insert account without schema: %v", err)
	}

	_, err = db.ExecContext(ctx, `
		INSERT INTO accounts (user_id, name_encrypted, name_hash, type, currency)
		VALUES ($1, $2, 'hash-x', 'safe-deposit', 'CAD')`, userID, []byte("name-pack"))
	assertViolation(t, err, codeCheckViolation, "type outside vocabulary")

	// Same per-user namespace rule as user_categories.
	_, err = db.ExecContext(ctx, `
		INSERT INTO accounts (user_id, name_encrypted, name_hash, type, currency)
		VALUES ($1, $2, 'hash-a', 'bank', 'CAD')`, userID, []byte("name-pack"))
	assertViolation(t, err, codeUniqueViolation, "duplicate (user_id, name_hash)")

	if _, err := db.ExecContext(ctx, `
		INSERT INTO accounts (user_id, name_encrypted, name_hash, type, currency)
		VALUES ($1, $2, 'hash-a', 'mortgage', 'CAD')`, otherUserID, []byte("name-pack")); err != nil {
		t.Fatalf("same name_hash under another user: %v", err)
	}

	ghost := "22222222-2222-3333-4444-555555555555"
	_, err = db.ExecContext(ctx, `
		INSERT INTO accounts (user_id, name_encrypted, name_hash, type, currency)
		VALUES ($1, $2, 'orphan', 'bank', 'CAD')`, ghost, []byte("name-pack"))
	assertViolation(t, err, codeForeignKeyViolation, "account without user")

	_, err = db.ExecContext(ctx, `
		INSERT INTO accounts
			(user_id, name_encrypted, name_hash, type, currency, default_schema_id)
		VALUES ($1, $2, 'ghost-schema', 'bank', 'CAD', $3)`,
		userID, []byte("name-pack"), ghost)
	assertViolation(t, err, codeForeignKeyViolation, "account with missing schema")

	// --- transactions -----------------------------------------------------

	// Minimal insert: date_posted + amount + description + dedup_hash are
	// the only required inputs. amount scans back as its decimal text
	// (plaintext numeric — the §5 decision under test).
	var (
		txID   string
		amount string
		review bool
	)
	err = db.QueryRowContext(ctx, `
		INSERT INTO transactions
			(user_id, account_id, date_posted, amount, description_encrypted, dedup_hash)
		VALUES ($1, $2, '2026-09-15', 123.45, $3, 'h1')
		RETURNING id, amount, review_pending`,
		userID, acctID, []byte("desc-pack")).Scan(&txID, &amount, &review)
	if err != nil {
		t.Fatalf("insert minimal transaction: %v", err)
	}
	if amount != "123.45" {
		t.Fatalf("amount = %q, want \"123.45\"", amount)
	}
	if review {
		t.Fatal("review_pending defaulted true, want false")
	}

	// The dedup key: the same (account_id, date_posted, dedup_hash) is a
	// re-import and is rejected — even with a different amount, because
	// the hash already pins amount (it is part of the hash input).
	_, err = db.ExecContext(ctx, `
		INSERT INTO transactions
			(user_id, account_id, date_posted, amount, description_encrypted, dedup_hash)
		VALUES ($1, $2, '2026-09-15', 999.99, $3, 'h1')`, userID, acctID, []byte("desc-pack"))
	assertViolation(t, err, codeUniqueViolation, "duplicate dedup_hash on (account, posted)")

	// The key is per-account: the same hash on another account is a
	// different event.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO transactions
			(user_id, account_id, date_posted, amount, description_encrypted, dedup_hash)
		VALUES ($1, $2, '2026-09-15', -42.10, $3, 'h1')`, userID, acctID2, []byte("d")); err != nil {
		t.Fatalf("same dedup_hash on a different account: %v", err)
	}
	// ... and a different hash on the same account + date is distinct.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO transactions
			(user_id, account_id, date_posted, amount, description_encrypted, dedup_hash)
		VALUES ($1, $2, '2026-09-15', 50.00, $3, 'h2')`, userID, acctID, []byte("d")); err != nil {
		t.Fatalf("same (account, posted) with different dedup_hash: %v", err)
	}

	// A transaction may not post before it executed.
	_, err = db.ExecContext(ctx, `
		INSERT INTO transactions
			(user_id, account_id, date_executed, date_posted, amount, description_encrypted, dedup_hash)
		VALUES ($1, $2, '2026-09-20', '2026-09-15', 1, $3, 'h3')`,
		userID, acctID, []byte("d"))
	assertViolation(t, err, codeCheckViolation, "date_executed after date_posted")

	// executed == posted is fine (the minimal insert already covered NULL).
	if _, err := db.ExecContext(ctx, `
		INSERT INTO transactions
			(user_id, account_id, date_executed, date_posted, amount, description_encrypted, dedup_hash)
		VALUES ($1, $2, '2026-09-16', '2026-09-16', 1, $3, 'h4')`,
		userID, acctID, []byte("d")); err != nil {
		t.Fatalf("date_executed = date_posted: %v", err)
	}

	_, err = db.ExecContext(ctx, `
		INSERT INTO transactions
			(user_id, account_id, date_posted, amount, description_encrypted, dedup_hash)
		VALUES ($1, $2, '2026-09-15', 1, $3, 'h5')`, userID, ghost, []byte("d"))
	assertViolation(t, err, codeForeignKeyViolation, "transaction without account")

	_, err = db.ExecContext(ctx, `
		INSERT INTO transactions
			(user_id, account_id, date_posted, amount, category_id, description_encrypted, dedup_hash)
		VALUES ($1, $2, '2026-09-15', 1, $3, $4, 'h6')`,
		userID, acctID, ghost, []byte("d"))
	assertViolation(t, err, codeForeignKeyViolation, "transaction with missing category")

	// --- cascades -----------------------------------------------------------

	// Category delete: the event survives, uncategorised (SET NULL).
	var catID string
	err = db.QueryRowContext(ctx, `
		INSERT INTO user_categories (user_id, name_encrypted, name_hash)
		VALUES ($1, $2, 'hash-c') RETURNING id`, userID, []byte("name-pack")).Scan(&catID)
	if err != nil {
		t.Fatalf("insert category: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE transactions SET category_id = $1 WHERE id = $2`, catID, txID); err != nil {
		t.Fatalf("assign category: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM user_categories WHERE id = $1`, catID); err != nil {
		t.Fatalf("delete category: %v", err)
	}
	var catAfter *string
	if err := db.QueryRowContext(ctx,
		`SELECT category_id FROM transactions WHERE id = $1`, txID).Scan(&catAfter); err != nil {
		t.Fatalf("scan category after delete: %v", err)
	}
	if catAfter != nil {
		t.Fatalf("category_id = %v after category delete, want NULL (SET NULL)", *catAfter)
	}
	if n := countRows(t, db, ctx, `SELECT count(*) FROM transactions WHERE id = $1`, txID); n != 1 {
		t.Fatalf("transaction present after category delete = %d, want 1", n)
	}

	// Account delete: its events go with it.
	if _, err := db.ExecContext(ctx, `DELETE FROM accounts WHERE id = $1`, acctID2); err != nil {
		t.Fatalf("delete account: %v", err)
	}
	if n := countRows(t, db, ctx, `SELECT count(*) FROM transactions WHERE account_id = $1`, acctID2); n != 0 {
		t.Fatalf("transactions surviving account delete = %d, want 0", n)
	}

	// User delete: accounts and transactions all cascade away; the other
	// user's account survives.
	if _, err := db.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if n := countRows(t, db, ctx, `SELECT count(*) FROM accounts WHERE user_id = $1`, userID); n != 0 {
		t.Fatalf("accounts surviving user delete = %d, want 0", n)
	}
	if n := countRows(t, db, ctx, `SELECT count(*) FROM transactions WHERE user_id = $1`, userID); n != 0 {
		t.Fatalf("transactions surviving user delete = %d, want 0", n)
	}
	if n := countRows(t, db, ctx, `SELECT count(*) FROM accounts WHERE user_id = $1`, otherUserID); n != 1 {
		t.Fatalf("other user's account present = %d, want 1", n)
	}
}

// TestBudgetCredentialConstraints pins the 0003 invariants: the shared
// period vocabulary on budgets and the per-user provider uniqueness +
// vocabulary on llm_credentials, plus the user-delete cascade.
func TestBudgetCredentialConstraints(t *testing.T) {
	db, ctx := migratedDB(t)

	var userID, otherUserID string
	err := db.QueryRowContext(ctx, `
		INSERT INTO users (auth0_sub, email, display_name, wrapped_dek)
		VALUES ('sub-budget', 'a@example.com', 'A', $1) RETURNING id`,
		[]byte("wrapped-dek-pack")).Scan(&userID)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	err = db.QueryRowContext(ctx, `
		INSERT INTO users (auth0_sub, email, display_name, wrapped_dek)
		VALUES ('sub-budget2', 'b@example.com', 'B', $1) RETURNING id`,
		[]byte("wrapped-dek-pack2")).Scan(&otherUserID)
	if err != nil {
		t.Fatalf("insert second user: %v", err)
	}
	ghost := "22222222-2222-3333-4444-555555555555"

	// --- budgets -----------------------------------------------------------

	// name + the whole targets jsonb ride as one encrypted blob each.
	var budgetID string
	err = db.QueryRowContext(ctx, `
		INSERT INTO budgets (user_id, period, name_encrypted, targets_encrypted)
		VALUES ($1, 'month', $2, $3) RETURNING id`,
		userID, []byte("name-pack"), []byte("targets-pack")).Scan(&budgetID)
	if err != nil {
		t.Fatalf("insert budget: %v", err)
	}

	// Period shares the vocabulary pinned in 0001 (week is deliberately
	// outside the set, however DESIGN.md phrases it).
	_, err = db.ExecContext(ctx, `
		INSERT INTO budgets (user_id, period, name_encrypted, targets_encrypted)
		VALUES ($1, 'week', $2, $3)`, userID, []byte("name-pack"), []byte("targets-pack"))
	assertViolation(t, err, codeCheckViolation, "budget period outside vocabulary")

	_, err = db.ExecContext(ctx, `
		INSERT INTO budgets (user_id, period, name_encrypted, targets_encrypted)
		VALUES ($1, 'month', $2, $3)`, ghost, []byte("name-pack"), []byte("targets-pack"))
	assertViolation(t, err, codeForeignKeyViolation, "budget without user")

	// --- llm_credentials ------------------------------------------------------

	// The default case: self-hosted llama.cpp takes no api_key.
	var credID string
	err = db.QueryRowContext(ctx, `
		INSERT INTO llm_credentials (user_id, provider, endpoint_encrypted, api_key_encrypted)
		VALUES ($1, 'self_hosted', $2, NULL) RETURNING id`,
		userID, []byte("endpoint-pack")).Scan(&credID)
	if err != nil {
		t.Fatalf("insert self-hosted credential (no key): %v", err)
	}

	// The unique key: one row per (user, provider).
	_, err = db.ExecContext(ctx, `
		INSERT INTO llm_credentials (user_id, provider, endpoint_encrypted)
		VALUES ($1, 'self_hosted', $2)`, userID, []byte("endpoint-pack"))
	assertViolation(t, err, codeUniqueViolation, "duplicate (user_id, provider)")

	// Same provider under another user, and a second provider for this
	// user, are both fine.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO llm_credentials (user_id, provider, endpoint_encrypted, api_key_encrypted)
		VALUES ($1, 'self_hosted', $2, NULL)`, otherUserID, []byte("endpoint-pack")); err != nil {
		t.Fatalf("same provider under another user: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO llm_credentials (user_id, provider, endpoint_encrypted, api_key_encrypted)
		VALUES ($1, 'openai', $2, $3)`, userID, []byte("endpoint-pack"), []byte("key-pack")); err != nil {
		t.Fatalf("second provider for the same user: %v", err)
	}

	_, err = db.ExecContext(ctx, `
		INSERT INTO llm_credentials (user_id, provider, endpoint_encrypted)
		VALUES ($1, 'anthropic', $2)`, userID, []byte("endpoint-pack"))
	assertViolation(t, err, codeCheckViolation, "provider outside vocabulary")

	_, err = db.ExecContext(ctx, `
		INSERT INTO llm_credentials (user_id, provider, endpoint_encrypted)
		VALUES ($1, 'openai', $2)`, ghost, []byte("endpoint-pack"))
	assertViolation(t, err, codeForeignKeyViolation, "credential without user")

	// --- cascade ---------------------------------------------------------------

	if _, err := db.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if n := countRows(t, db, ctx, `SELECT count(*) FROM budgets WHERE id = $1`, budgetID); n != 0 {
		t.Fatalf("budget surviving user delete = %d, want 0", n)
	}
	if n := countRows(t, db, ctx, `SELECT count(*) FROM llm_credentials WHERE id = $1`, credID); n != 0 {
		t.Fatalf("credential surviving user delete = %d, want 0", n)
	}
	if n := countRows(t, db, ctx, `SELECT count(*) FROM llm_credentials WHERE user_id = $1`, otherUserID); n != 1 {
		t.Fatalf("other user's credential present = %d, want 1", n)
	}
}

// TestImportJobsConstraints pins the 0005 invariants: the status
// vocabulary, the user/account FKs, the NULL-ability of result and
// error on a fresh job, and the cascade semantics (account delete and
// user delete both CASCADE the jobs, like the account's transactions).
func TestImportJobsConstraints(t *testing.T) {
	db, ctx := migratedDB(t)

	var userID, otherUserID, acctID string
	err := db.QueryRowContext(ctx, `
		INSERT INTO users (auth0_sub, email, display_name, wrapped_dek)
		VALUES ('sub-import', 'a@example.com', 'A', $1) RETURNING id`,
		[]byte("wrapped-dek-pack")).Scan(&userID)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	err = db.QueryRowContext(ctx, `
		INSERT INTO users (auth0_sub, email, display_name, wrapped_dek)
		VALUES ('sub-import2', 'b@example.com', 'B', $1) RETURNING id`,
		[]byte("wrapped-dek-pack2")).Scan(&otherUserID)
	if err != nil {
		t.Fatalf("insert second user: %v", err)
	}
	err = db.QueryRowContext(ctx, `
		INSERT INTO accounts (user_id, name_encrypted, name_hash, type, currency)
		VALUES ($1, $2, 'hash-a', 'bank', 'USD') RETURNING id`,
		userID, []byte("name-pack")).Scan(&acctID)
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}
	ghost := "22222222-2222-3333-4444-555555555555"

	// --- status vocabulary -------------------------------------------------

	_, err = db.ExecContext(ctx, `
		INSERT INTO import_jobs (user_id, account_id, status, file_encrypted, file_name_encrypted)
		VALUES ($1, $2, 'bogus', $3, $4)`, userID, acctID, []byte("file-pack"), []byte("name-pack"))
	assertViolation(t, err, codeCheckViolation, "status outside vocabulary")

	// --- FKs -----------------------------------------------------------------

	_, err = db.ExecContext(ctx, `
		INSERT INTO import_jobs (user_id, account_id, status, file_encrypted, file_name_encrypted)
		VALUES ($1, $2, 'uploaded', $3, $4)`, ghost, acctID, []byte("file-pack"), []byte("name-pack"))
	assertViolation(t, err, codeForeignKeyViolation, "import job without user")

	_, err = db.ExecContext(ctx, `
		INSERT INTO import_jobs (user_id, account_id, status, file_encrypted, file_name_encrypted)
		VALUES ($1, $2, 'uploaded', $3, $4)`, userID, ghost, []byte("file-pack"), []byte("name-pack"))
	assertViolation(t, err, codeForeignKeyViolation, "import job without account")

	// --- nullable terminal columns ---------------------------------------------

	// Minimal insert: result and error stay NULL until a later step
	// (4.3+) reaches a terminal state.
	var (
		jobID string
		res   sql.NullString
		er    sql.NullString
	)
	err = db.QueryRowContext(ctx, `
		INSERT INTO import_jobs (user_id, account_id, status, file_encrypted, file_name_encrypted)
		VALUES ($1, $2, 'uploaded', $3, $4)
		RETURNING id, result, error`,
		userID, acctID, []byte("file-pack"), []byte("name-pack")).Scan(&jobID, &res, &er)
	if err != nil {
		t.Fatalf("insert minimal import job: %v", err)
	}
	if res.Valid || er.Valid {
		t.Fatalf("result/error on fresh job = (%v, %v), want both NULL", res, er)
	}

	// A second job under the other user (the schema scopes by user_id in
	// the queries, not in the FKs, so the shared account is valid here).
	var jobID2 string
	err = db.QueryRowContext(ctx, `
		INSERT INTO import_jobs (user_id, account_id, status, file_encrypted, file_name_encrypted)
		VALUES ($1, $2, 'uploaded', $3, $4)
		RETURNING id`,
		otherUserID, acctID, []byte("file-pack"), []byte("name-pack")).Scan(&jobID2)
	if err != nil {
		t.Fatalf("insert second import job: %v", err)
	}

	// --- cascades -------------------------------------------------------------

	if _, err := db.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, otherUserID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if n := countRows(t, db, ctx, `SELECT count(*) FROM import_jobs WHERE id = $1`, jobID2); n != 0 {
		t.Fatalf("import job surviving user delete = %d, want 0", n)
	}

	if _, err := db.ExecContext(ctx, `DELETE FROM accounts WHERE id = $1`, acctID); err != nil {
		t.Fatalf("delete account: %v", err)
	}
	if n := countRows(t, db, ctx, `SELECT count(*) FROM import_jobs WHERE id = $1`, jobID); n != 0 {
		t.Fatalf("import job surviving account delete = %d, want 0", n)
	}
}

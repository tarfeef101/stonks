// Data-access repositories (plan step 2.5): categories, accounts,
// import schemas. Every method is scoped by userID — authorization is
// structural: no query shape reads or writes another user's rows.
//
// Tables with DEK-encrypted columns take the caller's unwrapped DEK
// (the handler resolves it from the users row: bootstrap +
// crypto.UnwrapDEK); the repos never see the KEK.
//
// name_hash = hex(sha256(TrimSpace(name))). Plan §5 keeps it plaintext
// for uniqueness/lookup. Exact-case sensitive: "Groceries" and
// "groceries" are distinct rows — the UI may canonicalize case, the
// schema does not.
//
// The plan's other 2.5 surfaces already exist: settings get/update is
// the 1.4a SettingsStore in internal/server, and the users row store is
// the 1.3c bootstrap UserStore (same package). Both operate on the same
// users table this package shares.
package repo

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
)

// Sentinel errors the API layer (phase 3) maps to status codes.
var (
	ErrCategoryNotFound = errors.New("repo: category not found")
	ErrParentNotFound   = errors.New("repo: parent category not found")
	ErrAccountNotFound  = errors.New("repo: account not found")
	ErrSchemaNotFound   = errors.New("repo: schema not found")
	ErrNameTaken        = errors.New("repo: name already exists for this user")
	ErrBuiltinReadOnly  = errors.New("repo: builtin schemas are read-only")

	// 2.6 transactions.
	ErrTransactionNotFound = errors.New("repo: transaction not found")
	ErrDedupConflict       = errors.New("repo: a transaction with the same dedup key already exists")
	// ErrDateRule is deliberately user-facing (no "repo:" prefix): the
	// 3.2 handler maps it to a 400 carrying this exact message.
	ErrDateRule = errors.New("date_executed must not be after date_posted")

	// 2.6b budgets + notifications + credentials.
	ErrBudgetNotFound       = errors.New("repo: budget not found")
	ErrCredentialNotFound   = errors.New("repo: llm credential not found")
	ErrNotificationNotFound = errors.New("repo: notification not found")

	// 4.1 import jobs.
	ErrImportJobNotFound = errors.New("repo: import job not found")

	// 5.4 categorise jobs.
	ErrCategoriseJobNotFound  = errors.New("repo: categorise job not found")
	ErrNoPendingCategoriseJob = errors.New("repo: no pending categorise job")

	// 6.5 dashboard cards.
	ErrDashboardCardNotFound = errors.New("repo: dashboard card not found")

	// 7.1 mortgages.
	ErrMortgageNotFound = errors.New("repo: mortgage not found")
)

// hashName computes the §5 plaintext name_hash.
func hashName(name string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(name)))
	return hex.EncodeToString(sum[:])
}

// validateName rejects empty (or blank) names and names over 255 runes
// (the column is text, but the API should 400 before the DB sees them).
func validateName(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("name must not be empty")
	}
	if utf8.RuneCountInString(name) > 255 {
		return errors.New("name must be at most 255 characters")
	}
	return nil
}

// pgErrCode extracts the SQLSTATE from a pgx error, "" when the error is
// not a postgres one. Classification: 23505 = unique_violation,
// 23503 = foreign_key_violation (see backend/migrations headers).
func pgErrCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

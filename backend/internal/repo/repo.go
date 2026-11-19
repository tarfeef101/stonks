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
	"fmt"
	"strconv"
	"strings"
	"time"
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

	// 6.5 dashboards + widgets (round 6 rework of dashboard cards, migration 0013).
	ErrDashboardNotFound       = errors.New("repo: dashboard not found")
	ErrDashboardWidgetNotFound = errors.New("repo: dashboard widget not found")

	// 16.1 saved Explore views (round 16, migration 0015).
	ErrSavedViewNotFound = errors.New("repo: saved view not found")

	// 7.1 mortgages.
	ErrMortgageNotFound = errors.New("repo: mortgage not found")

	// 9.1 FI institutions + credentials.
	ErrInstitutionNotFound  = errors.New("repo: institution not found")
	ErrInstitutionCodeTaken = errors.New("repo: institution code already taken")
	ErrFICredentialNotFound = errors.New("repo: fi credential not found")
	ErrFICredentialExists   = errors.New("repo: a fi credential for this institution already exists")

	// Offsets (migration 0012): transactions that net each other out.
	ErrOffsetSuggestionNotFound = errors.New("repo: offset suggestion not found")
	ErrOffsetGroupNotFound      = errors.New("repo: offset group not found")
	ErrOffsetAlreadyGrouped     = errors.New("repo: a transaction is already in an offset group")
	ErrOffsetNotBalanced        = errors.New("repo: the offset transactions do not net to zero")

	// Savings / registered plans (round 51, migration 0019).
	ErrSnapshotNotFound      = errors.New("repo: snapshot not found")
	ErrSnapshotConflict      = errors.New("repo: a snapshot already exists for this account and as-of date")
	ErrLimitOverrideNotFound = errors.New("repo: limit override not found")
	ErrAdjustmentNotFound    = errors.New("repo: room adjustment not found")
	ErrUserNotFound          = errors.New("repo: user not found")
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

// parseBirthYear validates and parses a birth year supplied by the user:
// exactly 4 digits with 1900 ≤ y ≤ the current UTC year. The year is
// personal data and is stored only in sealed form (see the *_encrypted
// columns added by migration 0019); this validator is shared by the
// account beneficiary and the user's own birth year.
func parseBirthYear(s string) (int, error) {
	if len(s) != 4 {
		return 0, errors.New("birth year must be exactly 4 digits")
	}
	y := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errors.New("birth year must be exactly 4 digits")
		}
		y = y*10 + int(r-'0')
	}
	if y < 1900 || y > time.Now().UTC().Year() {
		return 0, errors.New("birth year must be between 1900 and the current year")
	}
	return y, nil
}

// parseNumericToCents converts a postgres numeric rendered as a string
// into integer cents. It accepts the same grammar as canonicalAmount
// (optional sign, at most two fractional digits, optional surrounding
// whitespace) and rejects anything else: empty, no digits, more than
// two fractional digits, non-digits, or a value outside int64 range.
// Sums over numeric columns are rendered by pgx without scale
// preservation ("0.00" -> "0", "100.5" -> "100.5"), so this is the
// only safe numeric -> cents path for aggregate results (never
// float64).
func parseNumericToCents(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("numeric %q is empty", s)
	}
	neg := false
	switch s[0] {
	case '-':
		neg = true
		s = s[1:]
	case '+':
		s = s[1:]
	}
	intPart, fracPart := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, fracPart = s[:i], s[i+1:]
	}
	if len(intPart)+len(fracPart) == 0 {
		return 0, fmt.Errorf("numeric %q is not a valid decimal", s)
	}
	if !allDigits(intPart) || !allDigits(fracPart) {
		return 0, fmt.Errorf("numeric %q is not a valid decimal", s)
	}
	if len(fracPart) > 2 {
		return 0, fmt.Errorf("numeric %q has more than 2 decimal places", s)
	}
	for len(fracPart) < 2 {
		fracPart += "0"
	}
	cents, err := strconv.ParseInt(intPart+fracPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("numeric %q is out of range: %w", s, err)
	}
	if neg {
		cents = -cents
	}
	return cents, nil
}

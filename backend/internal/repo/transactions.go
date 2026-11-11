// Transaction store (plan step 2.6, first half of the 2.6 split).
//
// One row per financial event (0002). amount is plaintext by decision
// (SQL sums/ranges/sorts it); description/notes/raw are DEK-encrypted
// so merchant identity stays private in a dump. Every method is scoped
// by userID — authorization is structural (package header).
//
// dedup_hash = hex(sha256(account_id ‖ 0x00 ‖ date_posted "2006-01-02"
// ‖ 0x00 ‖ canonical amount ‖ 0x00 ‖ normalized description)), the 0002
// formula, computed in-app pre-encryption. The NUL separators make the
// concatenation unambiguous (no "ab‖c" == "a‖bc" collisions).
// UNIQUE (account_id, date_posted, dedup_hash) makes re-importing
// overlapping statement data a no-op (DESIGN.md "gracefully
// de-duplicate"): Upsert returns the existing row with inserted=false.
//
// Date columns are bound as "2006-01-02" strings, never time.Time: pgx
// encodes a bound time.Time as a timestamptz, and date/timestamptz
// comparison in SQL is session-timezone dependent (a UTC-midnight date
// can compare as the previous day in a negative-offset session).
// Scanning date -> time.Time is safe (UTC midnight).
//
// Description search is a candidate fetch + in-app decrypt (plan 2.6
// documented trade-off, personal scale <=100K rows): List fetches the
// most recent searchCandidateLimit rows matching the NON-search filters
// and substring-matches decrypted descriptions in Go.
package repo

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"finance-tracker/internal/crypto"
)

// searchCandidateLimit bounds how far back in-app description search
// reaches (the most recent rows matching the other filters).
const searchCandidateLimit = 1000

// defaultTxLimit / maxTxLimit bound TransactionFilter.Limit.
const (
	defaultTxLimit = 50
	maxTxLimit     = 100
)

// Transaction is one transactions row with its encrypted fields opened.
// DateExecuted is nil when unset; CategoryID/Notes/Raw are "" when unset
// (NULL). Amount is the canonical 2-decimal form ("10.00", "-5.50"),
// re-canonicalized on scan (see decrypt).
type Transaction struct {
	ID            string
	AccountID     string
	DateExecuted  *time.Time
	DatePosted    time.Time
	Amount        string
	CategoryID    string
	OffsetGroupID *string
	Description   string
	Notes         string
	Raw           string
	DedupHash     string
	ReviewPending bool
	CreatedAt     time.Time
	UpdatedAt     time.Time

	scan txScan // scratch for the NULL-carrying scan targets
}

// TransactionIn is the Create/Upsert input. CategoryID is nil or "" =
// uncategorized, otherwise a category owned by the same user. Notes/Raw
// are nil or "" = unset (NULL). Description may be "" (real statements
// have blank memos). Amount is any decimal with <=2 fractional digits;
// it is canonicalized before storage and hashing.
type TransactionIn struct {
	AccountID     string
	DateExecuted  *time.Time
	DatePosted    time.Time
	Amount        string
	CategoryID    *string
	Description   string
	Notes         *string
	Raw           *string
	ReviewPending bool
}

// TransactionPatch is the Update input. nil = untouched. The **string
// fields follow the 2.5 clear idiom: &nil or &"" = clear to NULL,
// &"value" = set. AccountID is deliberately not patchable: the account
// is part of the dedup identity, so moving a row to another account
// would silently defeat re-import dedup.
type TransactionPatch struct {
	Description   *string
	CategoryID    **string
	Notes         **string
	Raw           **string
	Amount        *string
	DatePosted    *time.Time
	DateExecuted  **time.Time
	ReviewPending *bool
}

// TransactionFilter is the List query. nil/empty fields = unfiltered.
// CategoryIDs: each value is a category UUID or "" (uncategorized only,
// IS NULL), combined with OR. DateFrom/DateTo bound
// date_posted (inclusive). MinAmount/MaxAmount are canonicalized
// (inclusive); with Sign set they are absolute magnitudes — spending
// matches -max..-min (amount < 0), income matches min..max (amount >
// 0). Sign is "spending" (amount < 0) or "income" (amount > 0); "" =
// any. Search is a case-insensitive description substring over the
// candidate window (see package doc). Limit defaults to 50, capped at
// 100. Rows come back ordered date_posted DESC, id DESC.
type TransactionFilter struct {
	AccountIDs  []string
	CategoryIDs []string
	DateFrom   *time.Time
	DateTo     *time.Time
	MinAmount  *string
	MaxAmount  *string
	Sign       string
	Search     string
	Limit      int
	Offset     int
}

// TransactionStore is the transactions repository.
type TransactionStore struct {
	pool *pgxpool.Pool
}

// NewTransactionStore returns a TransactionStore over pool.
func NewTransactionStore(pool *pgxpool.Pool) *TransactionStore {
	return &TransactionStore{pool: pool}
}

// canonicalAmount normalizes a decimal amount to numeric(14,2) form
// (always two fractional digits, no leading zeros): "10" -> "10.00",
// "-5.5" -> "-5.50", "007.5" -> "7.50". It rejects empty input,
// non-decimal input, more than 2 fractional digits, and values that do
// not fit numeric(14,2) (14 significant digits total) so the caller
// gets a 400 instead of a 22003 from the DB.
func canonicalAmount(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("amount must not be empty")
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
		return "", fmt.Errorf("amount %q is not a valid decimal", s)
	}
	if !allDigits(intPart) || !allDigits(fracPart) {
		return "", fmt.Errorf("amount %q is not a valid decimal", s)
	}
	if len(fracPart) > 2 {
		return "", fmt.Errorf("amount %q has more than 2 decimal places", s)
	}
	intPart = strings.TrimLeft(intPart, "0")
	if intPart == "" {
		intPart = "0"
	}
	if len(intPart)+len(fracPart) > 14 {
		return "", fmt.Errorf("amount %q is out of range for numeric(14,2)", s)
	}
	for len(fracPart) < 2 {
		fracPart += "0"
	}
	out := intPart + "." + fracPart
	if neg && !(intPart == "0" && fracPart == "00") {
		out = "-" + out
	}
	return out, nil
}

// CanonicalAmount is the exported form of canonicalAmount. The 3.2
// transaction and summary handlers run user-supplied amounts and filter
// bounds through it before any store call, so a bad value is a 400
// carrying this exact message instead of a 500 surfaced from inside a
// repo method.
func CanonicalAmount(s string) (string, error) { return canonicalAmount(s) }

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// normalizeDescription is the dedup-description form: trimmed,
// case-folded, internal whitespace collapsed. Statement vendors
// re-issue the same memo with different spacing/case; re-import must
// dedup on it.
func normalizeDescription(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// dedupHash implements the 0002 formula (see package header).
func dedupHash(accountID string, datePosted time.Time, amount, description string) string {
	h := sha256.New()
	h.Write([]byte(accountID))
	h.Write([]byte{0})
	h.Write([]byte(datePosted.UTC().Format("2006-01-02")))
	h.Write([]byte{0})
	h.Write([]byte(amount))
	h.Write([]byte{0})
	h.Write([]byte(normalizeDescription(description)))
	return hex.EncodeToString(h.Sum(nil))
}

// dateStr renders a date for binding (see package header for why dates
// are bound as strings).
func dateStr(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

// validateTxIn checks the shared references/invariants of Create and
// Upsert and returns the canonical amount. A missing/foreign account is
// ErrAccountNotFound; a missing/foreign category is ErrCategoryNotFound
// (existence not disclosed — same rule as the 2.5 schema ref).
func (s *TransactionStore) validateTxIn(ctx context.Context, userID string, in TransactionIn) (string, error) {
	ok, err := s.owned(ctx, `SELECT EXISTS (SELECT 1 FROM accounts WHERE user_id = $1 AND id = $2)`, userID, in.AccountID)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", ErrAccountNotFound
	}
	if in.CategoryID != nil && *in.CategoryID != "" {
		ok, err := s.owned(ctx, `SELECT EXISTS (SELECT 1 FROM user_categories WHERE user_id = $1 AND id = $2)`, userID, *in.CategoryID)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", ErrCategoryNotFound
		}
	}
	amount, err := canonicalAmount(in.Amount)
	if err != nil {
		return "", err
	}
	if in.DateExecuted != nil && afterDate(*in.DateExecuted, in.DatePosted) {
		return "", ErrDateRule
	}
	return amount, nil
}

// afterDate reports whether a is strictly after b (date precision: both
// are compared as calendar days).
func afterDate(a, b time.Time) bool {
	return dateStr(a) > dateStr(b)
}

// owned runs a single-bool scoped EXISTS probe (reference check).
func (s *TransactionStore) owned(ctx context.Context, query string, args ...any) (bool, error) {
	var ok bool
	if err := s.pool.QueryRow(ctx, query, args...).Scan(&ok); err != nil {
		return false, fmt.Errorf("repo: check transaction reference: %w", err)
	}
	return ok, nil
}

// sealOpt seals s, or returns a NULL argument when s is empty (notes/raw
// are stored NULL, never as an empty pack).
func sealOpt(dek []byte, s string) (any, error) {
	if s == "" {
		return nil, nil
	}
	pack, err := crypto.Seal(dek, []byte(s))
	if err != nil {
		return nil, fmt.Errorf("repo: seal transaction field: %w", err)
	}
	return pack, nil
}

// derefStr returns the pointed value, "" for nil.
func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Create inserts a new transaction. A row with the same dedup key is
// ErrDedupConflict (the importer's dedup-aware path is Upsert).
func (s *TransactionStore) Create(ctx context.Context, userID string, in TransactionIn, dek []byte) (Transaction, error) {
	amount, err := s.validateTxIn(ctx, userID, in)
	if err != nil {
		return Transaction{}, err
	}
	id, err := s.insertValidated(ctx, userID, in, amount, "", dek)
	if err != nil {
		return Transaction{}, err
	}
	return s.Get(ctx, userID, id, dek)
}

// Upsert inserts a transaction or, when a row with the same dedup key
// already exists, returns it with inserted=false (0002 re-import rule).
func (s *TransactionStore) Upsert(ctx context.Context, userID string, in TransactionIn, dek []byte) (Transaction, bool, error) {
	amount, err := s.validateTxIn(ctx, userID, in)
	if err != nil {
		return Transaction{}, false, err
	}
	id, err := s.insertValidated(ctx, userID, in, amount, " ON CONFLICT (account_id, date_posted, dedup_hash) DO NOTHING", dek)
	if errors.Is(err, sql.ErrNoRows) {
		// Conflict: the existing row is the answer, inserted=false.
		existing, err := s.findByDedup(ctx, userID, in.AccountID, in.DatePosted, dedupHash(in.AccountID, in.DatePosted, amount, in.Description), dek)
		if err != nil {
			return Transaction{}, false, err
		}
		return existing, false, nil
	}
	if err != nil {
		return Transaction{}, false, err
	}
	got, err := s.Get(ctx, userID, id, dek)
	if err != nil {
		return Transaction{}, false, err
	}
	return got, true, nil
}

// insertValidated seals and inserts a pre-validated input; it returns
// the new id. With an empty conflictClause it is a plain INSERT (23505
// -> ErrDedupConflict); with the DO NOTHING clause it returns
// sql.ErrNoRows on a conflict.
func (s *TransactionStore) insertValidated(ctx context.Context, userID string, in TransactionIn, amount, conflictClause string, dek []byte) (string, error) {
	descPack, err := crypto.Seal(dek, []byte(in.Description))
	if err != nil {
		return "", fmt.Errorf("repo: seal transaction description: %w", err)
	}
	notesArg, err := sealOpt(dek, derefStr(in.Notes))
	if err != nil {
		return "", err
	}
	rawArg, err := sealOpt(dek, derefStr(in.Raw))
	if err != nil {
		return "", err
	}
	var execArg any
	if in.DateExecuted != nil {
		execArg = dateStr(*in.DateExecuted)
	}
	var catArg *string // "" (or nil) = NULL, never an empty-string uuid
	if in.CategoryID != nil && *in.CategoryID != "" {
		v := *in.CategoryID
		catArg = &v
	}
	var id string
	err = s.pool.QueryRow(ctx, `
		INSERT INTO transactions (user_id, account_id, date_executed, date_posted, amount,
		                             category_id, description_encrypted, notes_encrypted,
		                             raw_encrypted, dedup_hash, review_pending)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`+conflictClause+`
		RETURNING id`,
		userID, in.AccountID, execArg, dateStr(in.DatePosted), amount,
		catArg, descPack, notesArg, rawArg,
		dedupHash(in.AccountID, in.DatePosted, amount, in.Description),
		in.ReviewPending).Scan(&id)
	if err != nil {
		if pgErrCode(err) == "23505" {
			return "", ErrDedupConflict
		}
		return "", fmt.Errorf("repo: insert transaction: %w", err)
	}
	return id, nil
}

// Get returns one of the user's transactions with its fields opened.
func (s *TransactionStore) Get(ctx context.Context, userID, id string, dek []byte) (Transaction, error) {
	var tx Transaction
	err := s.pool.QueryRow(ctx, txSelect+" WHERE user_id = $1 AND id = $2", userID, id).Scan(txScanFields(&tx)...)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Transaction{}, ErrTransactionNotFound
		}
		return Transaction{}, fmt.Errorf("repo: get transaction: %w", err)
	}
	if err := tx.decrypt(dek); err != nil {
		return Transaction{}, err
	}
	return tx, nil
}

// findByDedup loads the existing row an upsert conflicted with (scoped:
// account_id already implies the user, but the user_id filter keeps the
// rule structural).
func (s *TransactionStore) findByDedup(ctx context.Context, userID, accountID string, datePosted time.Time, hash string, dek []byte) (Transaction, error) {
	var tx Transaction
	err := s.pool.QueryRow(ctx, txSelect+
		" WHERE user_id = $1 AND account_id = $2 AND date_posted = $3 AND dedup_hash = $4",
		userID, accountID, dateStr(datePosted), hash).Scan(txScanFields(&tx)...)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// The row vanished between the conflict and this SELECT.
			return Transaction{}, ErrTransactionNotFound
		}
		return Transaction{}, fmt.Errorf("repo: find transaction by dedup: %w", err)
	}
	if err := tx.decrypt(dek); err != nil {
		return Transaction{}, err
	}
	return tx, nil
}

// txSelect is the shared column list (order = txScanFields order).
const txSelect = `SELECT id, account_id, date_executed, date_posted, amount, category_id,
               offset_group_id, description_encrypted, notes_encrypted, raw_encrypted,
               dedup_hash, review_pending, created_at, updated_at
		FROM transactions`

// txScanFields is the Scan target list for one transactions row. The
// NULL-carrying columns land in the unexported scan field until
// decrypt reassembles the public shape ("", nil).
func txScanFields(tx *Transaction) []any {
	tx.scan = txScan{}
	return []any{
		&tx.ID, &tx.AccountID, &tx.scan.dateExecuted, &tx.DatePosted,
		&tx.Amount, &tx.scan.categoryID, &tx.scan.offsetGroupID, &tx.scan.descEnc,
		&tx.scan.notesEnc, &tx.scan.rawEnc, &tx.DedupHash, &tx.ReviewPending,
		&tx.CreatedAt, &tx.UpdatedAt,
	}
}

// txScan is the scratch for one scanned row (the public Transaction
// keeps "" / nil for unset values, which SQL returns as NULL).
type txScan struct {
	dateExecuted  sql.NullTime
	categoryID    sql.NullString
	offsetGroupID sql.NullString
	descEnc       []byte
	notesEnc      []byte
	rawEnc        []byte
}

// decrypt opens the three encrypted fields and reassembles the public
// shape. A pack that does not open is a key/data mismatch the API
// surfaces as 500 (the DEK came from the user's own wrapped_dek, so
// this is corruption, not a client error) — same rule as the other
// stores.
func (tx *Transaction) decrypt(dek []byte) error {
	// Re-canonicalize the scanned amount: pgx's numeric -> string decode
	// is not scale-preserving ("0.00" arrives as "0"), so restore the
	// documented canonical form. The column is numeric(14,2), so the
	// scanned value is always a valid decimal with at most two
	// fractional digits and this cannot fail; a failure means the row is
	// corrupt (500, same rule as a field that will not open).
	can, err := canonicalAmount(tx.Amount)
	if err != nil {
		return fmt.Errorf("repo: transaction %s amount %q not canonical: %w", tx.ID, tx.Amount, err)
	}
	tx.Amount = can
	desc, err := openTxField(tx.ID, tx.scan.descEnc, dek)
	if err != nil {
		return err
	}
	tx.Description = desc
	if len(tx.scan.notesEnc) > 0 {
		notes, err := openTxField(tx.ID, tx.scan.notesEnc, dek)
		if err != nil {
			return err
		}
		tx.Notes = notes
	}
	if len(tx.scan.rawEnc) > 0 {
		raw, err := openTxField(tx.ID, tx.scan.rawEnc, dek)
		if err != nil {
			return err
		}
		tx.Raw = raw
	}
	if tx.scan.dateExecuted.Valid {
		t := tx.scan.dateExecuted.Time
		tx.DateExecuted = &t
	}
	tx.CategoryID = tx.scan.categoryID.String
	if tx.scan.offsetGroupID.Valid {
		s := tx.scan.offsetGroupID.String
		tx.OffsetGroupID = &s
	}
	return nil
}

// openTxField opens one encrypted transaction field.
func openTxField(id string, pack []byte, dek []byte) (string, error) {
	plain, err := crypto.Open(dek, pack)
	if err != nil {
		return "", fmt.Errorf("repo: decrypt transaction %s field: %w", id, err)
	}
	return string(plain), nil
}

// List returns a page of the user's transactions (see TransactionFilter
// and the package doc for the search trade-off).
func (s *TransactionStore) List(ctx context.Context, userID string, f TransactionFilter, dek []byte) ([]Transaction, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultTxLimit
	}
	if limit > maxTxLimit {
		limit = maxTxLimit
	}
	if f.Offset < 0 {
		f.Offset = 0
	}

	// offset_group_id IS NULL hides rows grouped as offsets (migration
	// 0012): they net each other out and are not real money flow. The
	// review queue and uncategorized views deliberately keep them.
	where := []string{"user_id = $1", "offset_group_id IS NULL"}
	args := []any{userID}
	add := func(cond string, val any) {
		args = append(args, val)
		where = append(where, fmt.Sprintf("%s $%d", cond, len(args)))
	}
	if len(f.AccountIDs) > 0 {
		ph := make([]string, len(f.AccountIDs))
		for i, id := range f.AccountIDs {
			args = append(args, id)
			ph[i] = "$" + strconv.Itoa(len(args))
		}
		where = append(where, "account_id IN ("+strings.Join(ph, ", ")+")")
	}
	if len(f.CategoryIDs) > 0 {
		var conds []string
		var uuids []string
		for _, c := range f.CategoryIDs {
			if c == "" {
				conds = append(conds, "category_id IS NULL")
			} else {
				uuids = append(uuids, c)
			}
		}
		if len(uuids) > 0 {
			ph := make([]string, len(uuids))
			for i, id := range uuids {
				args = append(args, id)
				ph[i] = "$" + strconv.Itoa(len(args))
			}
			conds = append(conds, "category_id IN ("+strings.Join(ph, ", ")+")")
		}
		if len(conds) == 1 {
			where = append(where, conds[0])
		} else {
			where = append(where, "("+strings.Join(conds, " OR ")+")")
		}
	}
	if f.DateFrom != nil {
		add("date_posted >=", dateStr(*f.DateFrom))
	}
	if f.DateTo != nil {
		add("date_posted <=", dateStr(*f.DateTo))
	}
	var minCan, maxCan string
	hasMin, hasMax := false, false
	if f.MinAmount != nil {
		can, err := canonicalAmount(*f.MinAmount)
		if err != nil {
			return nil, err
		}
		minCan, hasMin = can, true
	}
	if f.MaxAmount != nil {
		can, err := canonicalAmount(*f.MaxAmount)
		if err != nil {
			return nil, err
		}
		maxCan, hasMax = can, true
	}
	// With Sign set the bounds are absolute magnitudes: spending rows
	// (amount < 0) fall in -max..-min, income rows (amount > 0) in
	// min..max.
	switch f.Sign {
	case "spending":
		if hasMin {
			add("amount <=", "-"+minCan)
		}
		if hasMax {
			add("amount >=", "-"+maxCan)
		}
		add("amount <", 0)
	case "income":
		if hasMin {
			add("amount >=", minCan)
		}
		if hasMax {
			add("amount <=", maxCan)
		}
		add("amount >", 0)
	default:
		if hasMin {
			add("amount >=", minCan)
		}
		if hasMax {
			add("amount <=", maxCan)
		}
	}

	base := txSelect + " WHERE " + strings.Join(where, " AND ") +
		" ORDER BY date_posted DESC, id DESC"

	if strings.TrimSpace(f.Search) == "" {
		args = append(args, limit, f.Offset)
		q := fmt.Sprintf("%s LIMIT $%d OFFSET $%d", base, len(args)-1, len(args))
		rows, err := s.pool.Query(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("repo: list transactions: %w", err)
		}
		defer rows.Close()
		return scanTxRows(rows, dek)
	}

	// Search: candidate fetch + in-app decrypt (package doc). The
	// candidate window is the most recent rows matching the OTHER
	// filters; the substring match runs in Go over decrypted text.
	rows, err := s.pool.Query(ctx, base+" LIMIT "+strconv.Itoa(searchCandidateLimit), args...)
	if err != nil {
		return nil, fmt.Errorf("repo: list transactions: %w", err)
	}
	defer rows.Close()
	all, err := scanTxRows(rows, dek)
	if err != nil {
		return nil, err
	}
	needle := strings.ToLower(strings.TrimSpace(f.Search))
	matches := make([]Transaction, 0, len(all))
	for _, tx := range all {
		if strings.Contains(strings.ToLower(tx.Description), needle) {
			matches = append(matches, tx)
		}
	}
	if f.Offset >= len(matches) {
		return []Transaction{}, nil
	}
	end := f.Offset + limit
	if end > len(matches) {
		end = len(matches)
	}
	return matches[f.Offset:end], nil
}

// scanTxRows scans rows into Transactions, opening the encrypted fields.
func scanTxRows(rows pgx.Rows, dek []byte) ([]Transaction, error) {
	out := make([]Transaction, 0, 32)
	for rows.Next() {
		var tx Transaction
		if err := rows.Scan(txScanFields(&tx)...); err != nil {
			return nil, fmt.Errorf("repo: scan transaction: %w", err)
		}
		if err := tx.decrypt(dek); err != nil {
			return nil, err
		}
		out = append(out, tx)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo: iterate transactions: %w", err)
	}
	return out, nil
}

// CategorySpend is one (category, period) row of Aggregate. Spend and
// Income are non-negative magnitudes (spend = the absolute sum of the
// negative amounts, income = the sum of the positive ones); Net is
// income minus spend (negative when the spend wins). All three are
// canonical 2-decimal strings. CategoryID "" = the uncategorized group.
type CategorySpend struct {
	CategoryID string
	Spend      string
	Income     string
	Net        string
}

// Aggregate sums the caller's transactions with date_posted in
// [from, to] (inclusive, date precision) grouped by category. It is the
// whole-period computation behind GET /api/v1/summary: no search or
// paging filters apply. accountIDs (may be nil) restricts the sum to
// those accounts; an empty or nil slice means "all accounts". The sums
// are re-canonicalized on the way out — pgx renders numeric without
// preserving scale (a 0.00 sum comes back as "0") — so the output
// always matches the 2-decimal form the rest of the API speaks.
func (s *TransactionStore) Aggregate(ctx context.Context, userID string, from, to time.Time, accountIDs []string) ([]CategorySpend, error) {
	// offset_group_id IS NULL: grouped rows net each other out and must
	// not count toward the summary (see List).
	where := []string{"user_id = $1", "offset_group_id IS NULL"}
	args := []any{userID}
	add := func(cond string, val any) {
		args = append(args, val)
		where = append(where, fmt.Sprintf("%s $%d", cond, len(args)))
	}
	add("date_posted >=", dateStr(from))
	add("date_posted <=", dateStr(to))
	if len(accountIDs) > 0 {
		ph := make([]string, len(accountIDs))
		for i, id := range accountIDs {
			args = append(args, id)
			ph[i] = "$" + strconv.Itoa(len(args))
		}
		where = append(where, "account_id IN ("+strings.Join(ph, ", ")+")")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT COALESCE(category_id::text, ''),
		       COALESCE(SUM(CASE WHEN amount < 0 THEN -amount END), 0),
		       COALESCE(SUM(CASE WHEN amount > 0 THEN amount END), 0),
		       COALESCE(SUM(amount), 0)
		FROM transactions
		WHERE `+strings.Join(where, " AND ")+`
		GROUP BY category_id
		ORDER BY category_id NULLS FIRST`, args...)
	if err != nil {
		return nil, fmt.Errorf("repo: aggregate transactions: %w", err)
	}
	defer rows.Close()
	out := make([]CategorySpend, 0, 4)
	for rows.Next() {
		var (
			g   CategorySpend
			sp  string
			in  string
			net string
		)
		if err := rows.Scan(&g.CategoryID, &sp, &in, &net); err != nil {
			return nil, fmt.Errorf("repo: aggregate transactions: %w", err)
		}
		spend, err := canonicalAmount(sp)
		if err != nil {
			return nil, err
		}
		income, err := canonicalAmount(in)
		if err != nil {
			return nil, err
		}
		total, err := canonicalAmount(net)
		if err != nil {
			return nil, err
		}
		g.Spend, g.Income, g.Net = spend, income, total
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo: aggregate transactions: %w", err)
	}
	return out, nil
}

// Update applies the non-nil fields of p to one of the user's
// transactions and returns the updated row. A patch that changes
// description, amount, or date_posted recomputes dedup_hash; the new
// hash colliding with a sibling row is ErrDedupConflict. A no-op patch
// returns the current row without an UPDATE.
func (s *TransactionStore) Update(ctx context.Context, userID, id string, p TransactionPatch, dek []byte) (Transaction, error) {
	cur, err := s.Get(ctx, userID, id, dek)
	if err != nil {
		return Transaction{}, err
	}

	desc := cur.Description
	if p.Description != nil {
		desc = *p.Description
	}
	amount := cur.Amount
	if p.Amount != nil {
		amount, err = canonicalAmount(*p.Amount)
		if err != nil {
			return Transaction{}, err
		}
	}
	datePosted := cur.DatePosted
	if p.DatePosted != nil {
		datePosted = *p.DatePosted
	}
	dateExec := cur.DateExecuted
	dateExecChanged := p.DateExecuted != nil
	if dateExecChanged {
		dateExec = *p.DateExecuted
	}
	var catArg *string
	catChanged := p.CategoryID != nil
	if catChanged && **p.CategoryID != "" {
		ok, err := s.owned(ctx, `SELECT EXISTS (SELECT 1 FROM user_categories WHERE user_id = $1 AND id = $2)`, userID, **p.CategoryID)
		if err != nil {
			return Transaction{}, err
		}
		if !ok {
			return Transaction{}, ErrCategoryNotFound
		}
		v := **p.CategoryID
		catArg = &v
	}
	var notesArg any
	notesChanged := p.Notes != nil
	if notesChanged {
		notesArg, err = sealOpt(dek, derefStr(*p.Notes))
		if err != nil {
			return Transaction{}, err
		}
	}
	var rawArg any
	rawChanged := p.Raw != nil
	if rawChanged {
		rawArg, err = sealOpt(dek, derefStr(*p.Raw))
		if err != nil {
			return Transaction{}, err
		}
	}
	if desc == cur.Description && amount == cur.Amount && datePosted.Equal(cur.DatePosted) &&
		!catChanged && !notesChanged && !rawChanged && !dateExecChanged &&
		(p.ReviewPending == nil || *p.ReviewPending == cur.ReviewPending) {
		// No-op patch: return the current row without an UPDATE.
		return cur, nil
	}

	// The 0002 date check, app-side: 400, not a 23514. ErrDateRule is
	// user-facing on purpose (no "repo:" prefix) so the handler can map
	// it to a 400 instead of a 500.
	if (dateExecChanged || !datePosted.Equal(cur.DatePosted)) && dateExec != nil && afterDate(*dateExec, datePosted) {
		return Transaction{}, ErrDateRule
	}

	sets := make([]string, 0, 8)
	args := []any{userID, id}
	addSet := func(col string, val any) {
		args = append(args, val)
		sets = append(sets, col+" = $"+strconv.Itoa(len(args)))
	}
	if desc != cur.Description {
		pack, err := crypto.Seal(dek, []byte(desc))
		if err != nil {
			return Transaction{}, fmt.Errorf("repo: seal transaction description: %w", err)
		}
		addSet("description_encrypted", pack)
	}
	if amount != cur.Amount {
		addSet("amount", amount)
	}
	if !datePosted.Equal(cur.DatePosted) {
		addSet("date_posted", dateStr(datePosted))
	}
	if desc != cur.Description || amount != cur.Amount || !datePosted.Equal(cur.DatePosted) {
		addSet("dedup_hash", dedupHash(cur.AccountID, datePosted, amount, desc))
	}
	if catChanged {
		addSet("category_id", catArg)
	}
	if notesChanged {
		addSet("notes_encrypted", notesArg)
	}
	if rawChanged {
		addSet("raw_encrypted", rawArg)
	}
	if dateExecChanged {
		var execArg any
		if dateExec != nil {
			execArg = dateStr(*dateExec)
		}
		addSet("date_executed", execArg)
	}
	if p.ReviewPending != nil {
		addSet("review_pending", *p.ReviewPending)
	}
	sets = append(sets, "updated_at = now()")

	cmdTag, err := s.pool.Exec(ctx,
		"UPDATE transactions SET "+strings.Join(sets, ", ")+" WHERE user_id = $1 AND id = $2",
		args...)
	if err != nil {
		switch pgErrCode(err) {
		case "23505":
			return Transaction{}, ErrDedupConflict
		case "23503":
			return Transaction{}, ErrCategoryNotFound // category deleted between check and update
		}
		return Transaction{}, fmt.Errorf("repo: update transaction: %w", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return Transaction{}, ErrTransactionNotFound // deleted after the Get above
	}
	return s.Get(ctx, userID, id, dek)
}

// Delete removes one of the user's transactions.
func (s *TransactionStore) Delete(ctx context.Context, userID, id string) error {
	cmdTag, err := s.pool.Exec(ctx, `
		DELETE FROM transactions WHERE user_id = $1 AND id = $2`, userID, id)
	if err != nil {
		return fmt.Errorf("repo: delete transaction: %w", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return ErrTransactionNotFound
	}
	return nil
}

// UncategorizedItem is one uncategorized transaction for the
// categorisation worker (step 5.4): the decrypted description and the
// raw numeric amount (the worker canonicalizes it for the prompt).
type UncategorizedItem struct {
	ID          string
	Description string // decrypted
	Amount      float64
}

// Uncategorized returns up to limit of the user's uncategorized,
// not-yet-review-pending transactions, oldest first (0007 runner
// batch input; the query is pinned by the step 5.4 design). The
// descriptions are decrypted under dek.
func (s *TransactionStore) Uncategorized(ctx context.Context, userID string, limit int, dek []byte) ([]UncategorizedItem, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("repo: uncategorized limit must be positive")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, description_encrypted, amount FROM transactions
		WHERE user_id = $1 AND category_id IS NULL AND review_pending = false
		ORDER BY id
		LIMIT $2`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("repo: list uncategorized transactions: %w", err)
	}
	defer rows.Close()
	out := make([]UncategorizedItem, 0, limit)
	for rows.Next() {
		var (
			id       string
			descPack []byte
			amount   float64
		)
		if err := rows.Scan(&id, &descPack, &amount); err != nil {
			return nil, fmt.Errorf("repo: scan uncategorized transaction: %w", err)
		}
		desc, err := openTxField(id, descPack, dek)
		if err != nil {
			return nil, err
		}
		out = append(out, UncategorizedItem{ID: id, Description: desc, Amount: amount})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo: list uncategorized transactions: %w", err)
	}
	return out, nil
}

// CountUncategorized returns the count of the user's uncategorized,
// not-yet-review-pending transactions (the job's total, set once
// before the batch loop). dek is accepted for signature symmetry with
// the sibling methods; the count touches no encrypted columns.
func (s *TransactionStore) CountUncategorized(ctx context.Context, userID string, dek []byte) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM transactions
		WHERE user_id = $1 AND category_id IS NULL AND review_pending = false`, userID).
		Scan(&n); err != nil {
		return 0, fmt.Errorf("repo: count uncategorized transactions: %w", err)
	}
	return n, nil
}

// ApplySuggestion applies one validated suggestion to one of the
// user's transactions (step 5.4): review=false sets category_id (the
// category must be the user's own — the Update ownership probe),
// review=true sets review_pending (category_id stays NULL). Both
// columns are plaintext — no DEK needed.
func (s *TransactionStore) ApplySuggestion(ctx context.Context, userID, txnID, categoryID string, review bool) error {
	var cmdTag pgconn.CommandTag
	var err error
	if review {
		cmdTag, err = s.pool.Exec(ctx, `
			UPDATE transactions
			SET review_pending = true, updated_at = now()
			WHERE user_id = $1 AND id = $2`, userID, txnID)
	} else {
		ok, oerr := s.owned(ctx,
			`SELECT EXISTS (SELECT 1 FROM user_categories WHERE user_id = $1 AND id = $2)`,
			userID, categoryID)
		if oerr != nil {
			return oerr
		}
		if !ok {
			return ErrCategoryNotFound
		}
		cmdTag, err = s.pool.Exec(ctx, `
			UPDATE transactions
			SET category_id = $3, updated_at = now()
			WHERE user_id = $1 AND id = $2`, userID, txnID, categoryID)
	}
	if err != nil {
		return fmt.Errorf("repo: apply suggestion: %w", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return ErrTransactionNotFound // not the user's row (or gone)
	}
	return nil
}

// ReviewPending returns the user's review queue (step 5.5): every row
// the categorise worker flagged (review_pending) that still has no
// category, oldest id first. A row that somehow gained a category
// while flagged (manual edit) is already resolved and is excluded.
// Descriptions are decrypted under dek.
func (s *TransactionStore) ReviewPending(ctx context.Context, userID string, dek []byte) ([]Transaction, error) {
	rows, err := s.pool.Query(ctx,
		txSelect+" WHERE user_id = $1 AND review_pending = true AND category_id IS NULL\n\t\tORDER BY id", userID)
	if err != nil {
		return nil, fmt.Errorf("repo: list review pending: %w", err)
	}
	defer rows.Close()
	out := make([]Transaction, 0)
	for rows.Next() {
		var tx Transaction
		if err := rows.Scan(txScanFields(&tx)...); err != nil {
			return nil, fmt.Errorf("repo: scan review pending: %w", err)
		}
		if err := tx.decrypt(dek); err != nil {
			return nil, err
		}
		out = append(out, tx)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo: list review pending: %w", err)
	}
	return out, nil
}

// GetReviewPending loads one review-pending row that belongs to the
// caller (same queue predicate as ReviewPending, by id). Any other row
// — foreign, already accepted, or not flagged — is not found.
func (s *TransactionStore) GetReviewPending(ctx context.Context, userID, id string, dek []byte) (Transaction, error) {
	var tx Transaction
	err := s.pool.QueryRow(ctx,
		txSelect+" WHERE user_id = $1 AND id = $2 AND review_pending = true AND category_id IS NULL",
		userID, id).Scan(txScanFields(&tx)...)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Transaction{}, ErrTransactionNotFound
		}
		return Transaction{}, fmt.Errorf("repo: get review pending: %w", err)
	}
	if err := tx.decrypt(dek); err != nil {
		return Transaction{}, err
	}
	return tx, nil
}

// AcceptReview resolves one review-pending row with one of the user's
// own categories: it sets category_id and clears review_pending in one
// scoped update. The category must be owned by the user (the owned()
// probe, mirroring ApplySuggestion); 0 rows affected means the row is
// not the caller's pending row (concurrent accept included) —
// ErrTransactionNotFound, never a cross-user write.
func (s *TransactionStore) AcceptReview(ctx context.Context, userID, txnID, categoryID string) error {
	ok, err := s.owned(ctx,
		`SELECT EXISTS (SELECT 1 FROM user_categories WHERE user_id = $1 AND id = $2)`,
		userID, categoryID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrCategoryNotFound
	}
	cmdTag, err := s.pool.Exec(ctx, `
		UPDATE transactions
		SET category_id = $3, review_pending = false, updated_at = now()
		WHERE user_id = $1 AND id = $2 AND review_pending = true AND category_id IS NULL`,
		userID, txnID, categoryID)
	if err != nil {
		return fmt.Errorf("repo: accept review: %w", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return ErrTransactionNotFound
	}
	return nil
}

// AcceptAll resolves every review-pending row of the user with one
// category (the accept-all bulk action) and returns how many rows it
// applied. Same ownership probe and scoped update as AcceptReview.
func (s *TransactionStore) AcceptAll(ctx context.Context, userID, categoryID string) (int, error) {
	ok, err := s.owned(ctx,
		`SELECT EXISTS (SELECT 1 FROM user_categories WHERE user_id = $1 AND id = $2)`,
		userID, categoryID)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, ErrCategoryNotFound
	}
	cmdTag, err := s.pool.Exec(ctx, `
		UPDATE transactions
		SET category_id = $2, review_pending = false, updated_at = now()
		WHERE user_id = $1 AND review_pending = true AND category_id IS NULL`,
		userID, categoryID)
	if err != nil {
		return 0, fmt.Errorf("repo: accept all review: %w", err)
	}
	return int(cmdTag.RowsAffected()), nil
}

// Account snapshots + equity series (round 51, migration 0019).
//
// A snapshot pins a known account balance at a date. (account_id, as_of)
// is unique; as_of is immutable (it is part of the snapshot's identity);
// the balance is the canonical numeric(14,2) decimal, >= 0. The note is
// sealed under the owner's DEK and stored NULL when empty (same rule as
// the transaction notes).
//
// EquitySeries feeds the appreciation chart: it derives the true balance
// from the latest snapshot (if any) plus post-snapshot transactions, or
// from the opening balance plus all transactions, so it touches no
// encrypted column and takes no DEK.
package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Snapshot is one decoded account_snapshots row: the balance in
// canonical decimal ("1234.56") and the note decrypted ("" when the
// column is NULL).
type Snapshot struct {
	ID, UserID, AccountID string
	AsOf                  time.Time // date precision
	Balance               string    // canonical decimal, e.g. "1234.56"
	Note                  string    // decrypted; "" when NULL
	CreatedAt, UpdatedAt  time.Time
}

// SnapshotIn is the CreateSnapshot input. Balance is a canonical
// decimal >= 0 (the canonicalAmount grammar); Note is optional,
// "" = NULL.
type SnapshotIn struct {
	AccountID string
	AsOf      time.Time
	Balance   string
	Note      string
}

// SnapshotPatch is the UpdateSnapshot input. Balance: nil = untouched,
// otherwise a canonical decimal >= 0. Note tri-state: nil = untouched,
// &"" = clear (NULL), otherwise set. as_of is not patchable — it is part
// of the snapshot's identity.
type SnapshotPatch struct {
	Balance *string
	Note    *string
}

// SnapshotStore persists account_snapshots. One store serves every user;
// each method takes a userID and scopes its SQL on it (same shape as
// AccountStore).
type SnapshotStore struct {
	pool *pgxpool.Pool
}

// NewSnapshotStore returns a SnapshotStore over pool (nil for a nil
// pool, mirroring the AccountStore constructor).
func NewSnapshotStore(pool *pgxpool.Pool) *SnapshotStore {
	if pool == nil {
		return nil
	}
	return &SnapshotStore{pool: pool}
}

// validateSnapshotBalance normalizes s to the canonicalAmount grammar
// (reused from transactions.go — the same numeric(14,2) column) and
// rejects negatives: a snapshot balance is a known balance, >= 0. It
// returns the canonical string to persist.
func validateSnapshotBalance(s string) (string, error) {
	can, err := canonicalAmount(s)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(can, "-") {
		return "", fmt.Errorf("snapshot balance %q must not be negative", s)
	}
	return can, nil
}

// formatCents renders integer cents as the canonical decimal of the
// numeric(14,2) columns: "1234.56", "0.00". Snapshot balances are
// >= 0, but the formatter is total (negatives render "-5.50").
func formatCents(cents int64) string {
	neg := cents < 0
	if neg {
		cents = -cents
	}
	frac := strconv.FormatInt(cents%100, 10)
	if len(frac) < 2 {
		frac = "0" + frac
	}
	out := strconv.FormatInt(cents/100, 10) + "." + frac
	if neg {
		out = "-" + out
	}
	return out
}

// snapshotColumns is the shared single-row SELECT/RETURNING shape:
// balance arrives as numeric text (re-canonicalized from cents — pgx's
// numeric -> string decode is not scale-preserving), the note as a
// nullable pack.
const snapshotColumns = "id, user_id, account_id, as_of, balance, note_encrypted, created_at, updated_at"

// decodeSnapshot decodes one account_snapshots row: the balance
// round-trips through integer cents (never float64), the note pack
// opens (NULL -> ""). A stored balance that does not parse is
// corruption, surfaced as an error for the caller to wrap (same rule as
// a field that will not open).
func decodeSnapshot(id, userID, accountID string, asOf time.Time, balance string, noteEnc *[]byte, ca, ua time.Time, dek []byte) (Snapshot, error) {
	cents, err := parseNumericToCents(balance)
	if err != nil {
		return Snapshot{}, fmt.Errorf("repo: snapshot %s balance %q not canonical: %w", id, balance, err)
	}
	note, err := openEncryptedPack(noteEnc, "snapshot "+id+" note", dek)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{
		ID: id, UserID: userID, AccountID: accountID,
		AsOf: asOf, Balance: formatCents(cents), Note: note,
		CreatedAt: ca, UpdatedAt: ua,
	}, nil
}

// queryRowSnapshot runs a single-row snapshot query (SELECT/RETURNING)
// and decodes it. sql.ErrNoRows is ErrSnapshotNotFound: the WHERE is
// scoped on user_id + id, and a missing row and a row owned by another
// user look identical by design (existence not disclosed across users).
func (s *SnapshotStore) queryRowSnapshot(ctx context.Context, q string, args []any, dek []byte) (Snapshot, error) {
	var (
		id        string
		userID    string
		accountID string
		asOf      time.Time
		balance   string
		noteEnc   *[]byte
		ca        time.Time
		ua        time.Time
	)
	err := s.pool.QueryRow(ctx, q, args...).Scan(&id, &userID, &accountID, &asOf, &balance, &noteEnc, &ca, &ua)
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, ErrSnapshotNotFound
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("repo: query snapshot: %w", err)
	}
	return decodeSnapshot(id, userID, accountID, asOf, balance, noteEnc, ca, ua, dek)
}

// CreateSnapshot inserts one snapshot and returns it. The account must
// exist and be owned by the user (ErrAccountNotFound otherwise —
// existence not disclosed); the balance must pass
// validateSnapshotBalance. The note is sealed (NULL when empty). The
// insert is a single statement, so a 23505 can only be the
// (account_id, as_of) unique constraint: that is ErrSnapshotConflict.
func (s *SnapshotStore) CreateSnapshot(ctx context.Context, userID string, in SnapshotIn, dek []byte) (Snapshot, error) {
	balance, err := validateSnapshotBalance(in.Balance)
	if err != nil {
		return Snapshot{}, err
	}
	var ok bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM accounts WHERE user_id = $1 AND id = $2)`,
		userID, in.AccountID).Scan(&ok); err != nil {
		return Snapshot{}, fmt.Errorf("repo: check snapshot account: %w", err)
	}
	if !ok {
		return Snapshot{}, ErrAccountNotFound
	}
	note, err := sealOpt(dek, in.Note) // "" -> SQL NULL
	if err != nil {
		return Snapshot{}, fmt.Errorf("repo: seal snapshot note: %w", err)
	}
	snap, err := s.queryRowSnapshot(ctx, `
		INSERT INTO account_snapshots (user_id, account_id, as_of, balance, note_encrypted)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+snapshotColumns,
		[]any{userID, in.AccountID, dateStr(in.AsOf), balance, note}, dek)
	if err != nil && pgErrCode(err) == "23505" {
		return Snapshot{}, ErrSnapshotConflict
	}
	return snap, err
}

// ListSnapshots returns the account's snapshots for the user, as_of
// DESC. A missing/foreign account yields an empty list (structural
// scoping: the query cannot see another user's rows).
func (s *SnapshotStore) ListSnapshots(ctx context.Context, userID, accountID string, dek []byte) ([]Snapshot, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+snapshotColumns+`
		FROM account_snapshots
		WHERE user_id = $1 AND account_id = $2
		ORDER BY as_of DESC`, userID, accountID)
	if err != nil {
		return nil, fmt.Errorf("repo: list snapshots: %w", err)
	}
	defer rows.Close()
	out := []Snapshot{}
	for rows.Next() {
		var (
			id, rowUser, rowAcc string
			asOf                time.Time
			balance             string
			noteEnc             *[]byte
			ca, ua              time.Time
		)
		if err := rows.Scan(&id, &rowUser, &rowAcc, &asOf, &balance, &noteEnc, &ca, &ua); err != nil {
			return nil, fmt.Errorf("repo: scan snapshot: %w", err)
		}
		snap, err := decodeSnapshot(id, rowUser, rowAcc, asOf, balance, noteEnc, ca, ua, dek)
		if err != nil {
			return nil, err
		}
		out = append(out, snap)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo: list snapshots: %w", err)
	}
	return out, nil
}

// GetSnapshot returns one snapshot owned by the user;
// ErrSnapshotNotFound when it is absent OR owned by another user.
func (s *SnapshotStore) GetSnapshot(ctx context.Context, userID, id string, dek []byte) (Snapshot, error) {
	return s.queryRowSnapshot(ctx, `
		SELECT `+snapshotColumns+`
		FROM account_snapshots
		WHERE user_id = $1 AND id = $2`, []any{userID, id}, dek)
}

// UpdateSnapshot changes balance and/or note (as_of is immutable) and
// returns the updated row; ErrSnapshotNotFound when the snapshot is
// absent or owned by another user. An empty patch is a no-op that
// returns the current row without touching updated_at.
func (s *SnapshotStore) UpdateSnapshot(ctx context.Context, userID, id string, p SnapshotPatch, dek []byte) (Snapshot, error) {
	var (
		balance *string
		note    any
		noteSet bool
	)
	if p.Balance != nil {
		b, err := validateSnapshotBalance(*p.Balance)
		if err != nil {
			return Snapshot{}, err
		}
		balance = &b
	}
	if p.Note != nil {
		n, err := sealOpt(dek, *p.Note) // "" -> SQL NULL (clear)
		if err != nil {
			return Snapshot{}, fmt.Errorf("repo: seal snapshot note: %w", err)
		}
		note, noteSet = n, true
	}
	if balance == nil && !noteSet {
		return s.queryRowSnapshot(ctx, `
			SELECT `+snapshotColumns+`
			FROM account_snapshots
			WHERE user_id = $1 AND id = $2`, []any{userID, id}, dek)
	}
	sets := []string{}
	args := []any{}
	add := func(col string, val any) {
		args = append(args, val)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if balance != nil {
		add("balance", *balance)
	}
	if noteSet {
		add("note_encrypted", note)
	}
	sets = append(sets, "updated_at = now()")
	args = append(args, userID, id)
	return s.queryRowSnapshot(ctx, fmt.Sprintf(`
		UPDATE account_snapshots
		SET %s
		WHERE user_id = $%d AND id = $%d
		RETURNING `+snapshotColumns,
		strings.Join(sets, ", "), len(args)-1, len(args)), args, dek)
}

// DeleteSnapshot removes one snapshot owned by the user;
// ErrSnapshotNotFound when it is absent or owned by another user.
func (s *SnapshotStore) DeleteSnapshot(ctx context.Context, userID, id string) error {
	cmd, err := s.pool.Exec(ctx, `
		DELETE FROM account_snapshots
		WHERE user_id = $1 AND id = $2`, userID, id)
	if err != nil {
		return fmt.Errorf("repo: delete snapshot: %w", err)
	}
	if cmd.RowsAffected() == 0 {
		return ErrSnapshotNotFound
	}
	return nil
}

// EquityPoint is one point of the account's equity series: the true
// balance and the lifetime net contributions at a date, both in cents.
type EquityPoint struct {
	AsOf             time.Time // date precision
	Balance          int64     // cents
	NetContributions int64     // cents
}

// EquitySeries returns the appreciation-chart series for one account
// over [from, to]. No DEK: it reads no encrypted column.
//
// Contract:
//
//	balance(t) — the TRUE balance at t. If a snapshot with as_of <= t
//	exists, take the LATEST: its balance + the sum of the account's
//	transactions with date_posted strictly after that snapshot's as_of
//	and <= t. Else account.opening_balance + the sum of ALL of the
//	account's transactions with date_posted <= t.
//
//	netContributions(t) — sum of inflows minus sum of outflows over
//	ALL of the account's transactions with date_posted <= t
//	(unfiltered — the lifetime net; balance - net is appreciation).
//
// Points = {from} ∪ {tx date_posted in [from, to]} ∪ {snapshot as_of in
// [from, to]}, deduped, ascending. Offset-grouped transactions are
// excluded everywhere (they net each other out and are not real money
// flow — same rule as TransactionStore.List).
//
// Fetch budget: 1 account row + 1 snapshot query (as_of <= to) + 1
// transaction query (date_posted <= to), all ownership-scoped; the
// series is computed in Go in integer cents (no float64). No
// transactions/snapshots -> a single point at from with (opening, 0).
// A missing/foreign account is ErrAccountNotFound.
func (s *SnapshotStore) EquitySeries(ctx context.Context, userID, accountID string, from, to time.Time) ([]EquityPoint, error) {
	// 1) the account row, ownership-scoped: opening balance plus the
	// not-found rule (missing and foreign accounts look identical —
	// existence not disclosed across users).
	var openingStr string
	err := s.pool.QueryRow(ctx, `
		SELECT opening_balance
		FROM accounts
		WHERE user_id = $1 AND id = $2`, userID, accountID).Scan(&openingStr)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAccountNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("repo: equity series account: %w", err)
	}
	opening, err := parseNumericToCents(openingStr)
	if err != nil {
		return nil, fmt.Errorf("repo: equity series account %s opening balance %q not canonical: %w",
			accountID, openingStr, err)
	}

	fromD, toD := dateStr(from), dateStr(to)

	// 2) snapshots with as_of <= to, ascending: the anchor at any t is
	// the latest of these with as_of <= t.
	snaps, err := s.equitySnapshots(ctx, userID, accountID, toD)
	if err != nil {
		return nil, err
	}

	// 3) transactions with date_posted <= to, ascending (offset-grouped
	// rows excluded — see the contract).
	txs, err := s.equityTxs(ctx, userID, accountID, toD)
	if err != nil {
		return nil, err
	}

	// Cumulative signed sum over the sorted distinct posting days:
	// cum[i] = the sum of amounts of every tx with day <= txDays[i]
	// (the lifetime net through that day).
	var txDays []string
	var cum []int64
	running := int64(0)
	for _, tx := range txs {
		running += tx.cents
		if len(txDays) == 0 || tx.day != txDays[len(txDays)-1] {
			txDays = append(txDays, tx.day)
			cum = append(cum, running)
		} else {
			cum[len(cum)-1] = running
		}
	}
	// prefixAt(d) = the sum of signed amounts of every tx with day <= d.
	prefixAt := func(d string) int64 {
		i := sort.Search(len(txDays), func(i int) bool { return txDays[i] > d })
		if i == 0 {
			return 0
		}
		return cum[i-1]
	}
	// anchorAt(d) = the latest snapshot with as_of <= d, if any.
	anchorAt := func(d string) (string, int64, bool) {
		for i := len(snaps) - 1; i >= 0; i-- {
			if snaps[i].day <= d {
				return snaps[i].day, snaps[i].cents, true
			}
		}
		return "", 0, false
	}

	// Point days = {from} ∪ {tx days in [from, to]} ∪ {snapshot days in
	// [from, to]}, deduped, ascending.
	pointDays := map[string]bool{fromD: true}
	for _, tx := range txs {
		if tx.day >= fromD && tx.day <= toD {
			pointDays[tx.day] = true
		}
	}
	for _, sn := range snaps {
		if sn.day >= fromD && sn.day <= toD {
			pointDays[sn.day] = true
		}
	}
	days := make([]string, 0, len(pointDays))
	for d := range pointDays {
		days = append(days, d)
	}
	sort.Strings(days)

	points := make([]EquityPoint, 0, len(days))
	for _, d := range days {
		day, err := time.Parse("2006-01-02", d)
		if err != nil {
			return nil, fmt.Errorf("repo: equity series day %q: %w", d, err)
		}
		net := prefixAt(d)
		balance := opening + net
		if ad, acents, ok := anchorAt(d); ok {
			// The snapshot pins the true balance at ad; the transactions
			// strictly after ad through d move it from there.
			balance = acents + net - prefixAt(ad)
		}
		points = append(points, EquityPoint{AsOf: day, Balance: balance, NetContributions: net})
	}
	return points, nil
}

// equityDay is a (day, cents) pair for the equity series. day is the
// UTC "2006-01-02" form (date columns compare and dedupe as strings,
// matching how dates are bound).
type equityDay struct {
	day   string
	cents int64
}

// equitySnapshots loads the user's snapshots for the account with
// as_of <= toDay, ascending, in cents.
func (s *SnapshotStore) equitySnapshots(ctx context.Context, userID, accountID, toDay string) ([]equityDay, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT as_of, balance
		FROM account_snapshots
		WHERE user_id = $1 AND account_id = $2 AND as_of <= $3
		ORDER BY as_of ASC`, userID, accountID, toDay)
	if err != nil {
		return nil, fmt.Errorf("repo: equity series snapshots: %w", err)
	}
	defer rows.Close()
	var out []equityDay
	for rows.Next() {
		var (
			asOf    time.Time
			balance string
		)
		if err := rows.Scan(&asOf, &balance); err != nil {
			return nil, fmt.Errorf("repo: scan equity snapshot: %w", err)
		}
		cents, err := parseNumericToCents(balance)
		if err != nil {
			return nil, fmt.Errorf("repo: equity series snapshot balance %q not canonical: %w", balance, err)
		}
		out = append(out, equityDay{day: dateStr(asOf), cents: cents})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo: equity series snapshots: %w", err)
	}
	return out, nil
}

// equityTxs loads the user's transactions for the account with
// date_posted <= toDay, ascending, in signed cents. Offset-grouped rows
// are excluded (they net each other out — same rule as List).
func (s *SnapshotStore) equityTxs(ctx context.Context, userID, accountID, toDay string) ([]equityDay, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT date_posted, amount
		FROM transactions
		WHERE user_id = $1 AND account_id = $2 AND date_posted <= $3
		  AND offset_group_id IS NULL
		ORDER BY date_posted ASC`, userID, accountID, toDay)
	if err != nil {
		return nil, fmt.Errorf("repo: equity series transactions: %w", err)
	}
	defer rows.Close()
	var out []equityDay
	for rows.Next() {
		var (
			posted time.Time
			amount string
		)
		if err := rows.Scan(&posted, &amount); err != nil {
			return nil, fmt.Errorf("repo: scan equity transaction: %w", err)
		}
		cents, err := parseNumericToCents(amount)
		if err != nil {
			return nil, fmt.Errorf("repo: equity series transaction amount %q not canonical: %w", amount, err)
		}
		out = append(out, equityDay{day: dateStr(posted), cents: cents})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo: equity series transactions: %w", err)
	}
	return out, nil
}

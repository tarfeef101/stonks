// Offset groups (migration 0012): transactions that net each other out.
//
// Some rows are internal movements between the user's own accounts —
// paying a credit card from chequing, moving money between two
// chequing accounts — and must not count as spend in one account and
// income in the other. The matcher finds pairs of the caller's
// ungrouped rows with exactly mirrored amounts and close posting
// dates (both accounts in the same currency); the proposed pairs are
// stored in offset_suggestions so the user can accept (which creates a
// group) or dismiss them. A group is a set of the caller's rows whose
// amounts sum to exactly zero; grouped rows are hidden from List and
// Aggregate (the review queue and uncategorized views keep them).
package repo

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// offsetMatchLimit caps the pairs one scan returns. A large import can
// produce many candidates; 1000 is more than one person can review in
// a sitting, and the scan is idempotent so the rest surface next time.
const offsetMatchLimit = 1000

// OffsetCandidate is one pair the matcher found: two rows of the
// caller whose amounts are exact mirrors (A.Amount = -B.Amount) with
// close posting dates. A.ID < B.ID always holds (the uuid order the
// offset_suggestions pair key uses).
type OffsetCandidate struct {
	A Transaction
	B Transaction
}

// OffsetSuggestion is a pending proposal row with both legs opened.
type OffsetSuggestion struct {
	ID        string
	A         Transaction
	B         Transaction
	CreatedAt time.Time
}

// OffsetGroup is one offset group: the member rows whose amounts sum
// to exactly zero, in (date_posted, id) order.
type OffsetGroup struct {
	ID      string
	Members []Transaction
}

// FindOffsetCandidates scans the caller's ungrouped transactions for
// offset pairs. A pair (a, b), a.id < b.id, qualifies when
//
//   - b.amount = -a.amount (exactly mirrored),
//   - neither row is in a group,
//   - both accounts hold the same currency,
//   - |a.date_posted - b.date_posted| <= windowDays,
//   - when from/to are set, at least one leg is in [from, to] (the
//     import hook passes the inserted date range, so a new row may
//     offset against an older one),
//   - no offset_suggestions row for the pair exists in ANY status —
//     the unique pair key makes scans idempotent, and dismissals stick
//     until the group work is undone (clearing a group deletes the
//     touching suggestion rows).
//
// At most offsetMatchLimit pairs are returned, oldest a first.
func (s *TransactionStore) FindOffsetCandidates(ctx context.Context, userID string, dek []byte, from, to *time.Time, windowDays int) ([]OffsetCandidate, error) {
	var fromArg, toArg any
	if from != nil {
		fromArg = dateStr(*from)
	}
	if to != nil {
		toArg = dateStr(*to)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, b.id
		FROM transactions a
		JOIN transactions b
		    ON b.user_id = a.user_id
		   AND b.amount = -a.amount
		   AND a.id < b.id
		   AND a.offset_group_id IS NULL
		   AND b.offset_group_id IS NULL
		   AND ABS(a.date_posted - b.date_posted) <= $2
		JOIN accounts aa ON aa.id = a.account_id
		JOIN accounts ba ON ba.id = b.account_id
		WHERE a.user_id = $1
		  AND aa.currency = ba.currency
		  AND ($3::date IS NULL OR a.date_posted >= $3::date OR b.date_posted >= $3::date)
		  AND ($4::date IS NULL OR a.date_posted <= $4::date OR b.date_posted <= $4::date)
		  AND NOT EXISTS (
		      SELECT 1 FROM offset_suggestions s
		      WHERE s.user_id = a.user_id
		        AND s.tx_a = a.id AND s.tx_b = b.id
		  )
		ORDER BY a.date_posted, a.id
		LIMIT `+strconv.Itoa(offsetMatchLimit), userID, windowDays, fromArg, toArg)
	if err != nil {
		return nil, fmt.Errorf("repo: find offset candidates: %w", err)
	}
	defer rows.Close()
	type pair struct{ a, b string }
	var pairs []pair
	var ids []string
	seen := map[string]bool{}
	addID := func(id string) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.a, &p.b); err != nil {
			return nil, fmt.Errorf("repo: scan offset pair: %w", err)
		}
		addID(p.a)
		addID(p.b)
		pairs = append(pairs, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo: iterate offset pairs: %w", err)
	}
	if len(pairs) == 0 {
		return []OffsetCandidate{}, nil
	}
	txs, err := s.txsByIDs(ctx, userID, ids, dek)
	if err != nil {
		return nil, err
	}
	out := make([]OffsetCandidate, 0, len(pairs))
	for _, p := range pairs {
		a, okA := txs[p.a]
		b, okB := txs[p.b]
		if !okA || !okB {
			continue // a leg vanished mid-scan — skip it
		}
		out = append(out, OffsetCandidate{A: a, B: b})
	}
	return out, nil
}

// InsertOffsetSuggestions stores the matcher's pairs as pending
// suggestions. The pair key (user_id, tx_a, tx_b) is unique with tx_a
// < tx_b (re-sorted defensively here); a pair that already has a
// suggestion in ANY status is skipped silently, so re-scans never
// re-propose dismissed or accepted pairs. Returns how many rows were
// actually inserted.
func (s *TransactionStore) InsertOffsetSuggestions(ctx context.Context, userID string, cands []OffsetCandidate) (int, error) {
	if len(cands) == 0 {
		return 0, nil
	}
	args := make([]any, 0, len(cands)*3)
	phs := make([]string, 0, len(cands))
	for _, c := range cands {
		a, b := c.A.ID, c.B.ID
		if a > b {
			a, b = b, a
		}
		args = append(args, userID, a, b)
		n := len(args)
		phs = append(phs, fmt.Sprintf("($%d, $%d, $%d)", n-2, n-1, n))
	}
	query := "INSERT INTO offset_suggestions (user_id, tx_a, tx_b) VALUES " +
		strings.Join(phs, ", ") +
		" ON CONFLICT (user_id, tx_a, tx_b) DO NOTHING"
	tag, err := s.pool.Exec(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("repo: insert offset suggestions: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ListOffsetSuggestions returns the caller's pending suggestions,
// oldest first, with both legs opened.
func (s *TransactionStore) ListOffsetSuggestions(ctx context.Context, userID string, dek []byte) ([]OffsetSuggestion, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, tx_a, tx_b, created_at
		FROM offset_suggestions
		WHERE user_id = $1 AND status = 'pending'
		ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, fmt.Errorf("repo: list offset suggestions: %w", err)
	}
	defer rows.Close()
	type sugRow struct {
		id        string
		a, b      string
		createdAt time.Time
	}
	var sugRows []sugRow
	var ids []string
	seen := map[string]bool{}
	addID := func(id string) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for rows.Next() {
		var r sugRow
		if err := rows.Scan(&r.id, &r.a, &r.b, &r.createdAt); err != nil {
			return nil, fmt.Errorf("repo: scan offset suggestion: %w", err)
		}
		addID(r.a)
		addID(r.b)
		sugRows = append(sugRows, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo: iterate offset suggestions: %w", err)
	}
	if len(sugRows) == 0 {
		return []OffsetSuggestion{}, nil
	}
	txs, err := s.txsByIDs(ctx, userID, ids, dek)
	if err != nil {
		return nil, err
	}
	out := make([]OffsetSuggestion, 0, len(sugRows))
	for _, r := range sugRows {
		a, okA := txs[r.a]
		b, okB := txs[r.b]
		if !okA || !okB {
			continue
		}
		out = append(out, OffsetSuggestion{ID: r.id, A: a, B: b, CreatedAt: r.createdAt})
	}
	return out, nil
}

// AcceptOffsetSuggestion turns one pending suggestion into a group:
// both legs get a fresh shared offset_group_id and the suggestion row
// is marked 'accepted' — one database transaction, so either both
// legs are grouped and the suggestion accepted, or nothing changes.
//
// Errors: ErrOffsetSuggestionNotFound (the id is unknown, not the
// caller's, or already resolved), ErrOffsetAlreadyGrouped (a leg was
// grouped in the meantime), ErrOffsetNotBalanced (a leg was edited so
// the pair no longer mirrors). Returns the new group id.
func (s *TransactionStore) AcceptOffsetSuggestion(ctx context.Context, userID, id string, dek []byte) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("repo: begin accept offset: %w", err)
	}
	defer tx.Rollback(ctx) // no-op after Commit

	var txA, txB string
	err = tx.QueryRow(ctx, `
		SELECT tx_a, tx_b FROM offset_suggestions
		WHERE id = $1 AND user_id = $2 AND status = 'pending'
		FOR UPDATE`, id, userID).Scan(&txA, &txB)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrOffsetSuggestionNotFound
	}
	if err != nil {
		return "", fmt.Errorf("repo: load offset suggestion: %w", err)
	}

	legs, err := s.loadOffsetLegs(ctx, tx, userID, []string{txA, txB}, dek)
	if err != nil {
		return "", err
	}
	if err := offsetBalanced(legs[0].Amount, legs[1].Amount); err != nil {
		return "", err
	}

	groupID := uuid.NewString()
	if _, err := tx.Exec(ctx, `
		UPDATE transactions
		SET offset_group_id = $1, updated_at = now()
		WHERE user_id = $2 AND id IN ($3, $4)`, groupID, userID, txA, txB); err != nil {
		return "", fmt.Errorf("repo: group offset legs: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE offset_suggestions SET status = 'accepted' WHERE id = $1`, id); err != nil {
		return "", fmt.Errorf("repo: mark offset accepted: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("repo: commit accept offset: %w", err)
	}
	return groupID, nil
}

// DismissOffsetSuggestion drops a pending suggestion out of future
// scans. Dismissals stick: the matcher skips pairs that already have a
// suggestion in any status, and only clearing the legs' group deletes
// the touching rows (re-opening the pair to the matcher).
func (s *TransactionStore) DismissOffsetSuggestion(ctx context.Context, userID, id string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE offset_suggestions SET status = 'dismissed'
		WHERE id = $1 AND user_id = $2 AND status = 'pending'`, id, userID)
	if err != nil {
		return fmt.Errorf("repo: dismiss offset suggestion: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrOffsetSuggestionNotFound
	}
	return nil
}

// SetOffsetGroup groups a hand-picked set of the caller's rows: every
// id must be the caller's, none may already be grouped, and the
// amounts must sum to exactly zero (exact cents — a 0.01 difference
// is rejected). On success the rows share a fresh offset_group_id and
// it is returned.
func (s *TransactionStore) SetOffsetGroup(ctx context.Context, userID string, txIDs []string, dek []byte) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("repo: begin set offset group: %w", err)
	}
	defer tx.Rollback(ctx) // no-op after Commit

	rows, err := tx.Query(ctx, txSelect+" WHERE user_id = $1 AND id = ANY($2) FOR UPDATE", userID, txIDs)
	if err != nil {
		return "", fmt.Errorf("repo: load offset group members: %w", err)
	}
	txs, err := scanTxRows(rows, dek)
	rows.Close()
	if err != nil {
		return "", err
	}
	if len(txs) != len(txIDs) {
		return "", ErrTransactionNotFound
	}
	amounts := make([]string, 0, len(txs))
	for _, t := range txs {
		if t.OffsetGroupID != nil {
			return "", ErrOffsetAlreadyGrouped
		}
		amounts = append(amounts, t.Amount)
	}
	if err := offsetBalanced(amounts...); err != nil {
		return "", err
	}

	groupID := uuid.NewString()
	if _, err := tx.Exec(ctx, `
		UPDATE transactions
		SET offset_group_id = $1, updated_at = now()
		WHERE user_id = $2 AND id = ANY($3)`, groupID, userID, txIDs); err != nil {
		return "", fmt.Errorf("repo: set offset group: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("repo: commit set offset group: %w", err)
	}
	return groupID, nil
}

// ClearOffsetGroup dissolves one of the caller's groups: the member
// rows go back to ungrouped (visible in List/Aggregate again), and
// every suggestion row touching a member is deleted — otherwise a
// stale 'accepted' row would block the pair from being re-suggested
// forever (the matcher skips pairs that already have a row in any
// status).
func (s *TransactionStore) ClearOffsetGroup(ctx context.Context, userID, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("repo: begin clear offset group: %w", err)
	}
	defer tx.Rollback(ctx) // no-op after Commit

	rows, err := tx.Query(ctx, `
		SELECT id FROM transactions
		WHERE user_id = $1 AND offset_group_id = $2
		FOR UPDATE`, userID, id)
	if err != nil {
		return fmt.Errorf("repo: load offset group members: %w", err)
	}
	var memberIDs []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			rows.Close()
			return fmt.Errorf("repo: scan offset group member: %w", err)
		}
		memberIDs = append(memberIDs, m)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("repo: iterate offset group members: %w", err)
	}
	rows.Close()
	if len(memberIDs) == 0 {
		return ErrOffsetGroupNotFound
	}

	if _, err := tx.Exec(ctx, `
		UPDATE transactions
		SET offset_group_id = NULL, updated_at = now()
		WHERE user_id = $1 AND offset_group_id = $2`, userID, id); err != nil {
		return fmt.Errorf("repo: clear offset group: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM offset_suggestions
		WHERE user_id = $1 AND (tx_a = ANY($2) OR tx_b = ANY($2))`, userID, memberIDs); err != nil {
		return fmt.Errorf("repo: delete offset suggestions: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("repo: commit clear offset group: %w", err)
	}
	return nil
}

// ListOffsetGroups returns the caller's active groups, members opened
// and in (date_posted, id) order within a group.
func (s *TransactionStore) ListOffsetGroups(ctx context.Context, userID string, dek []byte) ([]OffsetGroup, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT offset_group_id, id
		FROM transactions
		WHERE user_id = $1 AND offset_group_id IS NOT NULL
		ORDER BY offset_group_id, date_posted, id`, userID)
	if err != nil {
		return nil, fmt.Errorf("repo: list offset groups: %w", err)
	}
	defer rows.Close()
	type member struct{ groupID, txID string }
	var members []member
	var ids []string
	seen := map[string]bool{}
	addID := func(id string) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for rows.Next() {
		var m member
		if err := rows.Scan(&m.groupID, &m.txID); err != nil {
			return nil, fmt.Errorf("repo: scan offset group member: %w", err)
		}
		addID(m.txID)
		members = append(members, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo: iterate offset group members: %w", err)
	}
	if len(members) == 0 {
		return []OffsetGroup{}, nil
	}
	txs, err := s.txsByIDs(ctx, userID, ids, dek)
	if err != nil {
		return nil, err
	}
	byGroup := map[string]*OffsetGroup{}
	var order []string
	for _, m := range members {
		g, ok := byGroup[m.groupID]
		if !ok {
			g = &OffsetGroup{ID: m.groupID}
			byGroup[m.groupID] = g
			order = append(order, m.groupID)
		}
		if tx, ok := txs[m.txID]; ok {
			g.Members = append(g.Members, tx)
		}
	}
	out := make([]OffsetGroup, 0, len(order))
	for _, gid := range order {
		out = append(out, *byGroup[gid])
	}
	return out, nil
}

// txsByIDs batch-loads the caller's transactions by id in one query.
// An id that is not the caller's (or has vanished) is silently absent
// from the map — the callers treat a missing leg as "skip the pair".
func (s *TransactionStore) txsByIDs(ctx context.Context, userID string, ids []string, dek []byte) (map[string]Transaction, error) {
	if len(ids) == 0 {
		return map[string]Transaction{}, nil
	}
	rows, err := s.pool.Query(ctx, txSelect+" WHERE user_id = $1 AND id = ANY($2)", userID, ids)
	if err != nil {
		return nil, fmt.Errorf("repo: fetch offset legs: %w", err)
	}
	defer rows.Close()
	txs, err := scanTxRows(rows, dek)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Transaction, len(txs))
	for _, tx := range txs {
		out[tx.ID] = tx
	}
	return out, nil
}

// loadOffsetLegs loads (and locks) a set of transactions by id within
// an open database transaction: every id must be the caller's, and
// none may already be grouped (ErrOffsetAlreadyGrouped — a leg got
// grouped between the proposal and the accept).
func (s *TransactionStore) loadOffsetLegs(ctx context.Context, tx pgx.Tx, userID string, ids []string, dek []byte) ([]Transaction, error) {
	out := make([]Transaction, 0, len(ids))
	for _, id := range ids {
		var t Transaction
		err := tx.QueryRow(ctx, txSelect+" WHERE user_id = $1 AND id = $2 FOR UPDATE", userID, id).
			Scan(txScanFields(&t)...)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrTransactionNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("repo: load offset leg: %w", err)
		}
		if t.OffsetGroupID != nil {
			return nil, ErrOffsetAlreadyGrouped
		}
		if err := t.decrypt(dek); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// amountToCents converts a canonical amount string to exact int64
// cents — no floats, so the net-to-zero check is exact (numeric(14,2)
// fits int64 with enormous headroom).
func amountToCents(amount string) (int64, error) {
	s := strings.TrimSpace(amount)
	if s == "" {
		return 0, errors.New("repo: offset amount is empty")
	}
	neg := false
	if s[0] == '-' || s[0] == '+' {
		neg = s[0] == '-'
		s = s[1:]
	}
	whole, frac := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		whole, frac = s[:i], s[i+1:]
	}
	if whole == "" {
		whole = "0"
	}
	if len(frac) > 2 {
		return 0, fmt.Errorf("repo: offset amount %q has more than 2 decimal places", amount)
	}
	for len(frac) < 2 {
		frac += "0"
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("repo: bad offset amount %q: %w", amount, err)
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("repo: bad offset amount %q: %w", amount, err)
	}
	cents := w*100 + f
	if neg {
		cents = -cents
	}
	return cents, nil
}

// offsetBalanced is the group invariant: the amounts sum to exactly
// zero cents.
func offsetBalanced(amounts ...string) error {
	var sum int64
	for _, a := range amounts {
		c, err := amountToCents(a)
		if err != nil {
			return err
		}
		sum += c
	}
	if sum != 0 {
		return ErrOffsetNotBalanced
	}
	return nil
}

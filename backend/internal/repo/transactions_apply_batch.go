// Batched apply for the categorise worker (plan 5.4): the runner used
// to issue one UPDATE per suggestion (plus an ownership probe per
// auto-apply) — 25–50 round trips for a default 25-row batch. These
// two methods apply a whole batch in at most three round trips (one
// ownership probe + one UPDATE for the auto-applied categories, one
// UPDATE for the review flags), with the same per-row strictness as
// ApplySuggestion: a foreign or missing category is
// ErrCategoryNotFound, and a row that vanished (or is not the
// caller's) between the worker's fetch and its apply is
// ErrTransactionNotFound. Both columns are plaintext — no DEK needed.
package repo

import (
	"context"
	"fmt"
	"strings"
)

// CategoryPair is one (transaction, category) assignment for
// ApplyCategories: the transaction must belong to the caller, the
// category must be the caller's own (the batched ownership probe).
type CategoryPair struct {
	TxnID      string
	CategoryID string
}

// ApplyCategories sets category_id on the caller's transactions in one
// update. Every distinct category id is probed for ownership in a
// single round trip (any missing or foreign category ->
// ErrCategoryNotFound — the same strictness as the per-row
// ApplySuggestion, batched); the update is one statement: a plain SET
// when every pair shares one category, a CASE when they do not.
// Duplicate txn ids are collapsed (first pair wins, the Run()
// duplicate-index convention) and the affected count must equal the
// number of distinct txn ids — otherwise ErrTransactionNotFound (a row
// vanished or is not the caller's, never a cross-user write). Returns
// the number of rows updated (0 for empty input).
func (s *TransactionStore) ApplyCategories(ctx context.Context, userID string, pairs []CategoryPair) (int, error) {
	if len(pairs) == 0 {
		return 0, nil
	}
	// Collapse duplicate txn ids and collect the distinct category ids
	// in first-seen order.
	uniq := make([]CategoryPair, 0, len(pairs))
	seenTxn := make(map[string]bool, len(pairs))
	var catIDs []string
	seenCat := make(map[string]bool, len(pairs))
	for _, p := range pairs {
		if seenTxn[p.TxnID] {
			continue
		}
		seenTxn[p.TxnID] = true
		uniq = append(uniq, p)
		if !seenCat[p.CategoryID] {
			seenCat[p.CategoryID] = true
			catIDs = append(catIDs, p.CategoryID)
		}
	}
	// One ownership probe for the whole batch.
	found := make(map[string]bool, len(catIDs))
	rows, err := s.pool.Query(ctx,
		`SELECT id FROM user_categories WHERE user_id = $1 AND id = ANY($2)`,
		userID, catIDs)
	if err != nil {
		return 0, fmt.Errorf("repo: check category ownership: %w", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("repo: check category ownership: %w", err)
		}
		found[id] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("repo: check category ownership: %w", err)
	}
	rows.Close()
	for _, id := range catIDs {
		if !found[id] {
			return 0, ErrCategoryNotFound
		}
	}
	txnIDs := make([]string, len(uniq))
	for i, p := range uniq {
		txnIDs[i] = p.TxnID
	}
	var (
		query string
		args  []any
	)
	if len(catIDs) == 1 {
		query = `
			UPDATE transactions
			SET category_id = $2, updated_at = now()
			WHERE user_id = $1 AND id = ANY($3)`
		args = []any{userID, catIDs[0], txnIDs}
	} else {
		var sb strings.Builder
		sb.WriteString("UPDATE transactions SET\n\t\tcategory_id = CASE id")
		for i := range uniq {
			// ::uuid anchors the CASE result type: the bare $n params
			// would resolve to text and the text -> uuid cast is
			// explicit-only (SQLSTATE 42804 on the assignment).
			sb.WriteString(fmt.Sprintf("\n\t\tWHEN $%d THEN $%d::uuid", i*2+1, i*2+2))
		}
		where := len(uniq)*2 + 1
		sb.WriteString(fmt.Sprintf(
			"\n\t\tEND,\n\t\tupdated_at = now()\n\t\tWHERE user_id = $%d AND id = ANY($%d)",
			where, where+1))
		query = sb.String()
		args = make([]any, 0, len(uniq)*2+2)
		for _, p := range uniq {
			args = append(args, p.TxnID, p.CategoryID)
		}
		args = append(args, userID, txnIDs)
	}
	cmdTag, err := s.pool.Exec(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("repo: apply categories: %w", err)
	}
	if cmdTag.RowsAffected() != int64(len(uniq)) {
		return 0, ErrTransactionNotFound // a row vanished (or is foreign)
	}
	return len(uniq), nil
}

// MarkReviewPending flags the caller's transactions for review in one
// update (review_pending = true; category_id is left untouched — the
// worker flags rows it will not auto-categorise, so they stay NULL).
// Same strictness as the per-row review path: the affected count must
// equal the number of distinct txn ids, otherwise ErrTransactionNotFound
// (a row vanished or is not the caller's). Returns the number of rows
// flagged (0 for empty input).
func (s *TransactionStore) MarkReviewPending(ctx context.Context, userID string, txnIDs []string) (int, error) {
	if len(txnIDs) == 0 {
		return 0, nil
	}
	ids := make([]string, 0, len(txnIDs))
	seen := make(map[string]bool, len(txnIDs))
	for _, id := range txnIDs {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	cmdTag, err := s.pool.Exec(ctx, `
		UPDATE transactions
		SET review_pending = true, updated_at = now()
		WHERE user_id = $1 AND id = ANY($2)`, userID, ids)
	if err != nil {
		return 0, fmt.Errorf("repo: mark review pending: %w", err)
	}
	if cmdTag.RowsAffected() != int64(len(ids)) {
		return 0, ErrTransactionNotFound // a row vanished (or is foreign)
	}
	return len(ids), nil
}

// Batch dedup-aware insert for the 4.6 import runner (T-P1-3a).
//
// The runner used to call Upsert per row; each call paid the account
// reference probe, the description Seal, one INSERT, and a post-
// INSERT read (~3 round trips per row: ~300K for a 100K-row
// statement). UpsertBatch collapses a whole file to ONE account probe
// plus, per chunk, ONE dedup probe + ONE plain multi-row INSERT: a
// 100K-row statement is 200 chunks, 401 round trips total.
//
// Contract: the inputs are import-shaped — one shared AccountID (the
// runner builds every row from job.AccountID), CategoryID / Notes /
// Raw unset, ReviewPending false. The batch INSERT binds no category
// and omits review_pending for the column DEFAULT (false), so a
// caller that set those would be silently dropped — validateBatchRow
// rejects them instead (see there for the why).
//
// perRow[i] is true iff ins[i] landed as a new row; false means its
// dedup identity already existed (a re-import row, or an earlier row
// of this same batch — first occurrence wins, the per-row order the
// legacy Upsert loop gave).
package repo

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"finance-tracker/internal/crypto"
)

// upsertBatchChunkSize is the max rows per (dedup probe + multi-row
// INSERT) pair: 500 rows x 7 bound columns is 3500 parameters, far
// under the postgres 65535-per-statement limit, and a 100K-row
// statement stays 200 chunks.
const upsertBatchChunkSize = 500

// batchRow is one input prepared for the chunk loop: the canonical
// amount and the dedup hash over it (the hash must use the canonical
// form, as the stored rows do), the date_executed bind arg (nil for
// the NULL column), and the sealed description (one Seal per row, up
// front). in is kept so the 23505 fallback can re-run the row through
// the per-row Upsert with its original input.
type batchRow struct {
	idx    int // position in the input slice (the perRow index)
	in     TransactionIn
	amount string
	hash   string
	exec   any
	pack   []byte
}

// UpsertBatch inserts the given import rows, dedup-checked, and
// reports per row which ones landed (see the file header for the
// perRow contract and the import-shape contract on ins).
func (s *TransactionStore) UpsertBatch(ctx context.Context, userID string, ins []TransactionIn, dek []byte) ([]bool, error) {
	perRow := make([]bool, len(ins))
	if len(ins) == 0 {
		// An empty batch never touches the DB — not even the account
		// probe. The runner does not call it with zero rows, but the
		// all-bad-file path must stay "done" without a round trip.
		return perRow, nil
	}

	// One account, probed ONCE for the whole run (the per-row Upsert
	// paid the EXISTS probe on every call). A mixed batch is a caller
	// bug the single probe could not cover.
	accountID := ins[0].AccountID
	for _, in := range ins {
		if in.AccountID != accountID {
			return nil, errors.New("repo: upsert batch: every input must share one account_id")
		}
	}
	ok, err := s.owned(ctx, `SELECT EXISTS (SELECT 1 FROM accounts WHERE user_id = $1 AND id = $2)`, userID, accountID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrAccountNotFound
	}

	// Up-front pass: canonicalize, the date rule, reject fields the
	// INSERT cannot carry, and one Seal per row (the legacy path
	// sealed exactly once per row too; on the rare 23505 fallback the
	// per-row Upsert seals again — the packs are just unused there).
	rows := make([]batchRow, len(ins))
	for i, in := range ins {
		br, err := validateBatchRow(in, dek)
		if err != nil {
			return nil, err
		}
		br.idx = i
		rows[i] = br
	}

	// Chunk loop: each chunk is one dedup probe + one multi-row
	// INSERT. Chunks are separate statements (no shared transaction),
	// so an earlier chunk is committed before a later chunk's probe —
	// a duplicate that crosses the chunk boundary is caught by the
	// later chunk's DB probe, not by the within-chunk seen map.
	for start := 0; start < len(rows); start += upsertBatchChunkSize {
		end := start + upsertBatchChunkSize
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]

		// Within-chunk duplicates: the first occurrence (input order)
		// wins, the rest skip — exactly what sequential per-row
		// Upserts would have produced.
		seen := make(map[string]bool, len(chunk))
		pending := make([]batchRow, 0, len(chunk))
		for _, br := range chunk {
			if seen[br.hash] {
				continue
			}
			seen[br.hash] = true
			pending = append(pending, br)
		}
		if len(pending) == 0 {
			continue
		}

		hashes := make([]string, len(pending))
		for i, br := range pending {
			hashes[i] = br.hash
		}
		existing, err := s.probeExistingHashes(ctx, userID, accountID, hashes)
		if err != nil {
			return nil, err
		}
		fresh := make([]batchRow, 0, len(pending))
		for _, br := range pending {
			if !existing[br.hash] {
				fresh = append(fresh, br)
			}
		}
		if len(fresh) == 0 {
			continue // the whole chunk is already in the DB
		}

		if err := s.insertBatchRows(ctx, userID, fresh); err != nil {
			if pgErrCode(err) == "23505" {
				// A plain multi-row INSERT is atomic: the conflict
				// means NONE of fresh landed (a concurrent writer
				// won a race between the probe and the INSERT — a
				// single running import cannot hit this). Re-run the
				// WHOLE chunk, in input order, through the per-row
				// Upsert: the colliding rows come back
				// inserted=false, the rest insert — exactly the
				// legacy per-row semantics for the chunk.
				for _, br := range chunk {
					_, inserted, uerr := s.Upsert(ctx, userID, br.in, dek)
					if uerr != nil {
						return nil, uerr
					}
					perRow[br.idx] = inserted
				}
				continue
			}
			return nil, fmt.Errorf("repo: upsert batch: insert: %w", err)
		}
		for _, br := range fresh {
			perRow[br.idx] = true
		}
	}
	return perRow, nil
}

// validateBatchRow preps one import-shaped input for the chunk loop:
// it re-canonicalizes the amount (the dedup hash must use the
// canonical form the stored rows do), re-checks the date rule, and
// seals the description. Fields the batch INSERT cannot carry are
// rejected rather than silently dropped: the import runner never sets
// them — it builds only AccountID / DateExecuted / DatePosted /
// Amount / Description, and CategoryID stays nil because imports have
// no category (the 5.4 categorise chain fills categories after the
// run) — so a set value is a caller bug worth failing loudly on.
func validateBatchRow(in TransactionIn, dek []byte) (batchRow, error) {
	if in.CategoryID != nil && *in.CategoryID != "" {
		return batchRow{}, errors.New("repo: upsert batch: category_id is not supported for batch insert")
	}
	if in.Notes != nil && *in.Notes != "" {
		return batchRow{}, errors.New("repo: upsert batch: notes are not supported for batch insert")
	}
	if in.Raw != nil && *in.Raw != "" {
		return batchRow{}, errors.New("repo: upsert batch: raw is not supported for batch insert")
	}
	if in.ReviewPending {
		return batchRow{}, errors.New("repo: upsert batch: review_pending is not supported for batch insert")
	}
	amount, err := canonicalAmount(in.Amount)
	if err != nil {
		return batchRow{}, err
	}
	if in.DateExecuted != nil && afterDate(*in.DateExecuted, in.DatePosted) {
		return batchRow{}, ErrDateRule
	}
	pack, err := crypto.Seal(dek, []byte(in.Description))
	if err != nil {
		return batchRow{}, fmt.Errorf("repo: seal transaction description: %w", err)
	}
	var exec any
	if in.DateExecuted != nil {
		exec = dateStr(*in.DateExecuted)
	}
	return batchRow{
		in:     in,
		amount: amount,
		hash:   dedupHash(in.AccountID, in.DatePosted, amount, in.Description),
		exec:   exec,
		pack:   pack,
	}, nil
}

// probeExistingHashes reports which of hashes the DB already holds for
// (userID, accountID) — one round trip for a whole chunk (the legacy
// per-row path paid an INSERT + conflict + re-SELECT per colliding
// row). dedup_hash already encodes the posted date, so account_id +
// dedup_hash is the full dedup identity.
func (s *TransactionStore) probeExistingHashes(ctx context.Context, userID, accountID string, hashes []string) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT dedup_hash FROM transactions
		WHERE user_id = $1 AND account_id = $2 AND dedup_hash = ANY($3)`,
		userID, accountID, hashes)
	if err != nil {
		return nil, fmt.Errorf("repo: upsert batch: dedup probe: %w", err)
	}
	defer rows.Close()
	existing := make(map[string]bool, len(hashes))
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, fmt.Errorf("repo: upsert batch: dedup probe: %w", err)
		}
		existing[h] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo: upsert batch: dedup probe: %w", err)
	}
	return existing, nil
}

// insertBatchRows is the plain multi-row INSERT the chunk loop runs
// for a chunk's fresh rows: seven bound columns per row (user_id,
// account_id, date_executed, date_posted, amount,
// description_encrypted, dedup_hash) — notes/raw/category stay NULL
// and review_pending takes the column DEFAULT (false) — and no ON
// CONFLICT, so a race loses as one atomic 23505 the caller falls back
// on (see UpsertBatch).
func (s *TransactionStore) insertBatchRows(ctx context.Context, userID string, rows []batchRow) error {
	var sb strings.Builder
	sb.WriteString("INSERT INTO transactions (user_id, account_id, date_executed, date_posted, amount, description_encrypted, dedup_hash) VALUES ")
	args := make([]any, 0, len(rows)*7)
	for i, br := range rows {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteByte('(')
		for c := 0; c < 7; c++ {
			if c > 0 {
				sb.WriteString(", ")
			}
			fmt.Fprintf(&sb, "$%d", len(args)+c+1)
		}
		sb.WriteByte(')')
		args = append(args, userID, br.in.AccountID, br.exec, dateStr(br.in.DatePosted), br.amount, br.pack, br.hash)
	}
	_, err := s.pool.Exec(ctx, sb.String(), args...)
	return err
}

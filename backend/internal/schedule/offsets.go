package schedule

// T3: the offset-suggestion tick (round 34 / T-P1-4). The manual scan
// (POST /v1/offsets/scan) is the only place candidates were discovered
// before; this job re-runs the same discovery on a 30-minute cadence
// for every user with new activity, so suggestions surface without the
// user having to hit the endpoint.

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"finance-tracker/internal/repo"
)

// offsetSuggestOverlap is how far into the past each scan looks. It
// must stay >= the job cadence (30 minutes) so consecutive scans
// overlap and cadence drift loses no pairs.
const offsetSuggestOverlap = 2 * time.Hour

// offsetSuggestWindowDays is the posting-date window handed to
// FindOffsetCandidatesSince. Keep in lockstep with
// offsetScanWindowDays = 7 in internal/server/offsets.go (the manual
// scan's window): the automatic scan must surface exactly what the
// manual scan surfaces.
const offsetSuggestWindowDays = 7

// OffsetSuggester re-discovers offset-suggestion candidates on a fixed
// cadence (30 minutes) instead of only when a user hits the manual scan
// endpoint. The scan window reaches 2 hours into the past (overlap) so
// cadence drift loses no pairs; InsertOffsetSuggestions is idempotent
// on the (user_id, tx_a, tx_b) pair, so overlapping scans only produce
// deduped inserts.
type OffsetSuggester struct {
	Log        *slog.Logger
	Clock      Clock
	Txns       OffsetScanner
	DekForUser func(ctx context.Context, uid string) ([]byte, error)
}

// OffsetScanner is the slice of *repo.TransactionStore the suggester
// needs (all three methods are committed by D1).
type OffsetScanner interface {
	UsersWithTxSince(ctx context.Context, since time.Time) ([]string, error)
	FindOffsetCandidatesSince(ctx context.Context, userID string, dek []byte, windowDays int, since time.Time) ([]repo.OffsetCandidate, error)
	InsertOffsetSuggestions(ctx context.Context, userID string, cands []repo.OffsetCandidate) (int, error)
}

// Run scans every user with transactions since
// Clock.Now()-offsetSuggestOverlap. A UsersWithTxSince error aborts
// the run (mirrors the dispatcher's ListUsers handling); per-user
// failures are logged and skipped.
func (s *OffsetSuggester) Run(ctx context.Context) error {
	since := s.Clock.Now().Add(-offsetSuggestOverlap)
	users, err := s.Txns.UsersWithTxSince(ctx, since)
	if err != nil {
		return fmt.Errorf("schedule: list users with tx: %w", err)
	}
	inserted := 0
	for _, uid := range users {
		dek, err := s.DekForUser(ctx, uid)
		if err != nil {
			s.Log.Warn("offset suggestion scan failed", "op", "dek", "user", uid, "error", err)
			continue
		}
		cands, err := s.Txns.FindOffsetCandidatesSince(ctx, uid, dek, offsetSuggestWindowDays, since)
		if err != nil {
			s.Log.Warn("offset suggestion scan failed", "op", "candidates", "user", uid, "error", err)
			continue
		}
		if len(cands) == 0 {
			continue
		}
		n, err := s.Txns.InsertOffsetSuggestions(ctx, uid, cands)
		if err != nil {
			s.Log.Warn("offset suggestion scan failed", "op", "insert", "user", uid, "error", err)
			continue
		}
		inserted += n
	}
	s.Log.Info("offset suggestion scan finished", "users", len(users), "inserted", inserted)
	return nil
}

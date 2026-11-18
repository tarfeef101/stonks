package schedule

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Advisory lock keys, one per scheduled job kind (pinned by step 8.3).
const (
	// LockKeyReport guards the daily report dispatch.
	LockKeyReport int64 = 71001
	// LockKeyFiSync guards the hourly FI-sync hook.
	LockKeyFiSync int64 = 71002
	// LockKeyFxRates guards the daily FX rate fetch.
	LockKeyFxRates int64 = 71003
	// LockKeyOffsetSuggest guards the offset-suggestion scan.
	LockKeyOffsetSuggest int64 = 71004
	// LockKeyMortgageSelfHeal guards the mortgage self-heal.
	LockKeyMortgageSelfHeal int64 = 71005
)

// Leader is the multi-replica guard: at most one process holds a given
// key at a time. The production implementation is Postgres session
// advisory locks on a dedicated connection that outlives pool churn
// (no tables, no migrations — the lock is the ONLY multi-replica
// guard).
type Leader interface {
	// TryLock attempts to take key; it returns whether the caller
	// holds it after the call.
	TryLock(ctx context.Context, key int64) (bool, error)
	// Unlock releases the session's lock on key.
	Unlock(ctx context.Context, key int64) error
}

// AdvisoryLock implements Leader with pg_try_advisory_lock /
// pg_advisory_unlock on one *pgx.Conn (deliberately NOT the pool: the
// lock must survive pool churn; the worker keeps the connection open
// for the process lifetime).
type AdvisoryLock struct {
	conn *pgx.Conn
}

// NewAdvisoryLock returns an AdvisoryLock over conn.
func NewAdvisoryLock(conn *pgx.Conn) *AdvisoryLock {
	return &AdvisoryLock{conn: conn}
}

// TryLock runs pg_try_advisory_lock(key) and reports whether the lock
// is held after the call.
func (l *AdvisoryLock) TryLock(ctx context.Context, key int64) (bool, error) {
	var ok bool
	err := l.conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("schedule: try lock %d: %w", key, err)
	}
	return ok, nil
}

// Unlock runs pg_advisory_unlock(key); Postgres returns false when the
// session did not hold the lock, which Unlock surfaces as an error so
// the caller can log it.
func (l *AdvisoryLock) Unlock(ctx context.Context, key int64) error {
	var ok bool
	if err := l.conn.QueryRow(ctx, "SELECT pg_advisory_unlock($1)", key).Scan(&ok); err != nil {
		return fmt.Errorf("schedule: unlock %d: %w", key, err)
	}
	if !ok {
		return fmt.Errorf("schedule: advisory lock %d was not held", key)
	}
	return nil
}

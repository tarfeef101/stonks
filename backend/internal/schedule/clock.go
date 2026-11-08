// Package schedule is the worker-side scheduler (plan step 8.3): a
// robfig/cron loop over named job entries (the daily report dispatch
// and the hourly FI-sync hook), Postgres session advisory locks as the
// multi-replica leader election, and a WORKER_FAKE_NOW clock override
// for deterministic dev runs. The dispatch core and every adapter are
// fake-injectable (Clock, Leader, UserLister, ReportCollector,
// DekResolver, mail.Sender, Notifier) so the whole pipeline unit-tests
// without a database or network.
package schedule

import (
	"log/slog"
	"os"
	"time"
)

// Clock is the time source the scheduler and dispatcher read.
// Implementations normalize to UTC.
type Clock interface {
	Now() time.Time
}

// RealClock returns the current time.
type RealClock struct{}

// Now returns time.Now() in UTC.
func (RealClock) Now() time.Time { return time.Now().UTC() }

// FixedClock always returns one fixed instant (the WORKER_FAKE_NOW
// dev-only override).
type FixedClock struct {
	At time.Time
}

// Now returns the fixed instant.
func (c FixedClock) Now() time.Time { return c.At }

// NewClock returns the production clock: when WORKER_FAKE_NOW holds a
// valid RFC3339 instant, a FixedClock pinned to it (read ONCE at
// construction — the instant is fixed for the process lifetime);
// otherwise the RealClock. An invalid value falls back to the RealClock
// with a warning: the override is dev-only and a typo must not brick
// the worker.
func NewClock() Clock {
	v := os.Getenv("WORKER_FAKE_NOW")
	if v == "" {
		return RealClock{}
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		slog.Warn("WORKER_FAKE_NOW is not valid RFC3339; using the real clock", "value", v, "error", err)
		return RealClock{}
	}
	return FixedClock{At: t.UTC()}
}

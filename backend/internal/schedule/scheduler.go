package schedule

import (
	"context"
	"log/slog"
	"time"

	"github.com/robfig/cron/v3"
)

// The four pinned schedules (UTC, 5-field cron, StandardParser).
const (
	// ReportDispatchSpec is the daily report dispatch: 00:15 UTC.
	ReportDispatchSpec = "15 0 * * *"
	// FiSyncSpec is the hourly FI-sync hook: :05 past the hour.
	FiSyncSpec = "5 * * * *"
	// OffsetSuggestSpec is the offset-suggestion scan: 30-minute cadence;
	// the 2-hour scan overlap in offsets.go covers cadence drift.
	OffsetSuggestSpec = "*/30 * * * *"
	// MortgageSelfHealSpec is the mortgage self-heal: hourly at minute 23,
	// deliberately off the :05 fi-sync and 00:15 report dispatch minutes.
	MortgageSelfHealSpec = "23 * * * *"
)

// Entry is one scheduled job: a cron spec, its advisory lock key, and
// the work to run when it fires (leader election is the scheduler's
// job, so Run is invoked only by the replica that holds the lock).
// Run's error is logged, never fatal: one entry's failure must not
// kill the cron loop.
type Entry struct {
	Name string
	Spec string
	Key  int64
	Run  func(ctx context.Context) error
}

// Scheduler is the cron loop with per-entry leader election.
type Scheduler struct {
	Clock   Clock
	Lock    Leader
	Entries []Entry
	Log     *slog.Logger
}

// NewScheduler returns a Scheduler (Log defaults to slog.Default()).
func NewScheduler(clock Clock, lock Leader, entries []Entry) *Scheduler {
	return &Scheduler{Clock: clock, Lock: lock, Entries: entries, Log: slog.Default()}
}

// Start runs the startup instant check (the WORKER_FAKE_NOW override),
// then the cron loop until ctx is cancelled; on cancellation it stops
// the cron cleanly (no leaked goroutines blocking shutdown). It blocks
// for the process lifetime — the worker calls it as a goroutine with
// the signal context.
func (s *Scheduler) Start(ctx context.Context) {
	s.startupRun(ctx)

	cr := cron.New(cron.WithLocation(time.UTC))
	for _, e := range s.Entries {
		if _, err := cr.AddFunc(e.Spec, func() { s.runEntry(ctx, e) }); err != nil {
			// A bad spec is a build-time bug; log and continue so
			// the remaining entries still run.
			s.Log.ErrorContext(ctx, "add cron entry failed", "entry", e.Name, "spec", e.Spec, "error", err)
			continue
		}
	}
	cr.Start()

	<-ctx.Done()
	// Stop signals the cron loop and returns a context that is Done once
	// every in-flight job has finished; wait for it so no job outlives
	// shutdown (our jobs all observe ctx, so this returns promptly).
	<-cr.Stop().Done()
	s.Log.Info("scheduler stopped")
}

// startupRun runs each entry whose spec matches the clock instant at
// minute precision EXACTLY ONCE, before the cron loop begins — the
// WORKER_FAKE_NOW dev override (with the real clock it only matches
// when the process starts inside the entry's minute, in which case the
// run is the day's one, since cron's next occurrence is strictly after
// now).
func (s *Scheduler) startupRun(ctx context.Context) {
	now := s.Clock.Now()
	for _, e := range s.Entries {
		if !specMatchesMinute(e.Spec, now) {
			continue
		}
		s.Log.InfoContext(ctx, "startup run (clock instant matches)", "entry", e.Name)
		s.runEntry(ctx, e)
	}
}

// specMatchesMinute reports whether spec fires in now's minute (UTC,
// minute precision): the next occurrence strictly after the previous
// minute must be the current minute (cron entries fire at second 0 of
// a matching minute).
func specMatchesMinute(spec string, now time.Time) bool {
	sched, err := cron.ParseStandard(spec)
	if err != nil {
		return false
	}
	minute := now.UTC().Truncate(time.Minute)
	return sched.Next(minute.Add(-time.Minute)).Equal(minute)
}

// runEntry is one scheduled execution with leader election: acquire
// the entry's advisory lock (another replica holding it skips the
// whole execution at info level), run the work, and release the lock
// best-effort (an unlock failure is logged and does not abort).
func (s *Scheduler) runEntry(ctx context.Context, e Entry) {
	if ctx.Err() != nil {
		return
	}
	held, err := s.Lock.TryLock(ctx, e.Key)
	if err != nil {
		s.Log.ErrorContext(ctx, "leader lock failed", "entry", e.Name, "key", e.Key, "error", err)
		return
	}
	if !held {
		s.Log.InfoContext(ctx, "another replica is leader", "entry", e.Name, "key", e.Key)
		return
	}
	defer func() {
		// The unlock must outlive the (possibly cancelled) run
		// context: use a fresh one.
		if uerr := s.Lock.Unlock(context.Background(), e.Key); uerr != nil {
			s.Log.ErrorContext(context.Background(), "leader unlock failed", "entry", e.Name, "key", e.Key, "error", uerr)
		}
	}()
	if rerr := e.Run(ctx); rerr != nil {
		s.Log.ErrorContext(ctx, "entry run failed", "entry", e.Name, "error", rerr)
	}
}

// FiSyncHook is the hourly FI-sync entry (step 9.2 fills the phase-9
// placeholder): RunFI is one FI-sync pass; nil RunFI (nothing wired)
// keeps the pinned no-op log line so the hourly entry, its lock, and
// the wiring stay exercised end to end.
type FiSyncHook struct {
	Log   *slog.Logger
	Clock Clock                                          // non-nil whenever RunFI is set
	RunFI func(ctx context.Context, now time.Time) error // nil = placeholder no-op
}

// NewFiSyncHook returns a FiSyncHook (Log defaults to slog.Default()).
func NewFiSyncHook() *FiSyncHook {
	return &FiSyncHook{Log: slog.Default()}
}

// Run is the fi-sync entry's Run: nil RunFI logs the pinned no-op
// line, otherwise one FI-sync pass at the clock's current instant.
func (h *FiSyncHook) Run(ctx context.Context) error {
	if h.RunFI == nil {
		h.Log.InfoContext(ctx, "fi sync tick (phase 9 hook, no adapters registered)")
		return nil
	}
	return h.RunFI(ctx, h.Clock.Now())
}

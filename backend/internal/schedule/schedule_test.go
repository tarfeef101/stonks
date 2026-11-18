package schedule

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"finance-tracker/internal/mail"
	"finance-tracker/internal/report"
)

// --- fakes (no DB, no network, no sleeps) ---------------------------

type fakeClock struct{ now time.Time }

func (c fakeClock) Now() time.Time { return c.now }

type fakeMailSink struct {
	mu     sync.Mutex // guards msgs (Send runs concurrently)
	msgs   []mail.Message
	errFor func(to string) error
}

func (m *fakeMailSink) Send(ctx context.Context, msg mail.Message) error {
	if m.errFor != nil {
		if err := m.errFor(msg.To); err != nil {
			return err
		}
	}
	m.mu.Lock()
	m.msgs = append(m.msgs, msg)
	m.mu.Unlock()
	return nil
}

type fakeUsers struct {
	users []User
	err   error
	calls int
}

func (f *fakeUsers) List(ctx context.Context) ([]User, error) {
	f.calls++
	return f.users, f.err
}

// fakeCollector returns a minimal valid Data (one category row with
// canonical money, a category name; nil Merchants/PriorCategorySpend —
// Generate and both renderers accept it, and Generate's output always
// passes checkReport). Collect runs concurrently under the dispatcher's
// bounded fan-out: calls is mutex-guarded, and inflight/maxFlight are
// atomics the bounded-concurrency test asserts on (incr before the
// work, max-observe, decr after).
type fakeCollector struct {
	mu        sync.Mutex // guards calls
	errFor    func(userID string, p report.Period) error
	calls     []string
	blocked   chan struct{}
	inflight  int64 // atomic: Collects currently in flight
	maxFlight int64 // atomic: observed max concurrent Collects
}

func (c *fakeCollector) Collect(ctx context.Context, userID string, p report.Period, asOf time.Time, dek []byte) (report.Data, error) {
	n := atomic.AddInt64(&c.inflight, 1)
	for {
		cur := atomic.LoadInt64(&c.maxFlight)
		if n <= cur {
			break
		}
		if atomic.CompareAndSwapInt64(&c.maxFlight, cur, n) {
			break
		}
	}
	defer atomic.AddInt64(&c.inflight, -1)

	if c.blocked != nil {
		select {
		case <-ctx.Done():
			return report.Data{}, ctx.Err()
		case <-c.blocked:
		}
	}
	c.mu.Lock()
	c.calls = append(c.calls, userID+"/"+string(p))
	c.mu.Unlock()
	if c.errFor != nil {
		if err := c.errFor(userID, p); err != nil {
			return report.Data{}, err
		}
	}
	return report.Data{
		Period:        p,
		AsOf:          asOf,
		CategorySpend: []report.CategorySpend{{CategoryID: "cat-1", Spend: "10.00", Income: "20.00", Net: "10.00"}},
		CategoryNames: map[string]string{"cat-1": "Groceries"},
	}, nil
}

type fakeNotify struct {
	mu      sync.Mutex // guards entries/users (Notify runs concurrently)
	entries []NotificationIn
	users   []string
	err     error
}

func (f *fakeNotify) Notify(ctx context.Context, userID string, in NotificationIn) error {
	if f.err != nil {
		return f.err
	}
	f.mu.Lock()
	f.entries = append(f.entries, in)
	f.users = append(f.users, userID)
	f.mu.Unlock()
	return nil
}

// fakeAdvisoryLock is an in-memory Leader: held keys block other
// TryLocks; watchHeld exposes when a key is first acquired (the
// two-replica test's synchronization point).
type fakeAdvisoryLock struct {
	mu       sync.Mutex
	held     map[int64]bool
	acquired []int64
	released []int64
	watchers map[int64][]chan struct{}
}

func newFakeAdvisoryLock() *fakeAdvisoryLock {
	return &fakeAdvisoryLock{held: map[int64]bool{}, watchers: map[int64][]chan struct{}{}}
}

func (l *fakeAdvisoryLock) TryLock(ctx context.Context, key int64) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held[key] {
		return false, nil
	}
	l.held[key] = true
	l.acquired = append(l.acquired, key)
	for _, w := range l.watchers[key] {
		close(w)
	}
	l.watchers[key] = nil // the watchers fire once; avoid a double close
	return true, nil
}

func (l *fakeAdvisoryLock) Unlock(ctx context.Context, key int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.held[key] {
		return fmt.Errorf("fake lock: %d not held", key)
	}
	l.held[key] = false
	l.released = append(l.released, key)
	return nil
}

func (l *fakeAdvisoryLock) watchHeld(key int64) <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	ch := make(chan struct{})
	l.watchers[key] = append(l.watchers[key], ch)
	return ch
}

func (l *fakeAdvisoryLock) snapshot() (acquired, released []int64, held map[int64]bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := make([]int64, len(l.acquired))
	copy(a, l.acquired)
	r := make([]int64, len(l.released))
	copy(r, l.released)
	h := make(map[int64]bool)
	for k, v := range l.held {
		if v {
			h[k] = true
		}
	}
	return a, r, h
}

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	var b strings.Builder
	return slog.New(slog.NewJSONHandler(&b, nil))
}

func newTestDispatcher(t *testing.T, now time.Time, users *fakeUsers, coll *fakeCollector, sink *fakeMailSink, notif *fakeNotify) *Dispatcher {
	t.Helper()
	d := NewDispatcher(fakeClock{now: now}, users, coll,
		func(ctx context.Context, userID string) ([]byte, error) { return []byte("dek"), nil },
		sink, notif)
	d.Log = testLogger(t)
	return d
}

func assertPeriods(t *testing.T, got []report.Period, want ...report.Period) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("DuePeriods = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("DuePeriods[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func subjectsOf(msgs []mail.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Subject)
	}
	return out
}

// --- scenarios ------------------------------------------------------

// 1. 1st of the month: only notify_monthly onboarded users get a
// report; exact subject, recipient, non-empty bodies; nothing else.
func TestDispatchFirstOfTheMonth(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 15, 0, 0, time.UTC)
	users := &fakeUsers{users: []User{
		{ID: "u1", Email: "a@example.com", Onboarded: true, Monthly: true},
		{ID: "u2", Email: "b@example.com", Onboarded: true, Quarterly: true},
	}}
	sink := &fakeMailSink{}
	notif := &fakeNotify{}
	d := newTestDispatcher(t, now, users, &fakeCollector{}, sink, notif)

	if err := d.Dispatch(context.Background()); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(sink.msgs) != 1 {
		t.Fatalf("want 1 mail, got %d: %v", len(sink.msgs), subjectsOf(sink.msgs))
	}
	m := sink.msgs[0]
	if m.To != "a@example.com" {
		t.Errorf("To = %q, want a@example.com", m.To)
	}
	if m.Subject != "Personal Finance Tracker: October 2026 report" {
		t.Errorf("Subject = %q", m.Subject)
	}
	if m.Text == "" || m.HTML == "" {
		t.Error("Text/HTML must be non-empty")
	}
	if len(notif.entries) != 0 {
		t.Errorf("want 0 notifications, got %d", len(notif.entries))
	}
	if users.calls != 1 {
		t.Errorf("user listing ran %d times, want 1", users.calls)
	}
}

// 2. Quarter boundary: monthly+quarterly gets exactly two mails (the
// just-ended month + the just-ended quarter); annual does not fire.
func TestDispatchQuarterBoundary(t *testing.T) {
	now := time.Date(2026, 4, 1, 0, 15, 0, 0, time.UTC)
	users := &fakeUsers{users: []User{
		{ID: "u1", Email: "a@example.com", Onboarded: true, Monthly: true, Quarterly: true},
		{ID: "u3", Email: "c@example.com", Onboarded: true, Annual: true},
	}}
	coll := &fakeCollector{}
	sink := &fakeMailSink{}
	notif := &fakeNotify{}
	d := newTestDispatcher(t, now, users, coll, sink, notif)

	if err := d.Dispatch(context.Background()); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	// The bounded fan-out makes completion order nondeterministic:
	// compare as sorted sets, not positions.
	want := []string{
		"Personal Finance Tracker: March 2026 report",
		"Personal Finance Tracker: Q1 2026 report",
	}
	got := subjectsOf(sink.msgs)
	if len(got) != len(want) {
		t.Fatalf("subjects = %v, want %v", got, want)
	}
	sort.Strings(got)
	sort.Strings(want)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("subjects = %v, want %v", got, want)
		}
	}
	wantCalls := []string{"u1/monthly", "u1/quarterly"}
	if len(coll.calls) != len(wantCalls) {
		t.Fatalf("collector calls = %v, want %v", coll.calls, wantCalls)
	}
	sort.Strings(coll.calls)
	for i := range wantCalls {
		if coll.calls[i] != wantCalls[i] {
			t.Errorf("collector calls = %v, want %v", coll.calls, wantCalls)
		}
	}
	if len(notif.entries) != 0 {
		t.Errorf("want 0 notifications, got %d", len(notif.entries))
	}
}

// 3. Jan 1: all three flags -> exactly three mails (December + Q4 +
// the prior year).
func TestDispatchJanuaryFirst(t *testing.T) {
	now := time.Date(2027, 1, 1, 0, 15, 0, 0, time.UTC)
	users := &fakeUsers{users: []User{
		{ID: "u1", Email: "a@example.com", Onboarded: true, Monthly: true, Quarterly: true, Annual: true},
	}}
	coll := &fakeCollector{}
	sink := &fakeMailSink{}
	d := newTestDispatcher(t, now, users, coll, sink, &fakeNotify{})

	if err := d.Dispatch(context.Background()); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	// The bounded fan-out makes completion order nondeterministic:
	// compare as sorted sets, not positions.
	want := []string{
		"Personal Finance Tracker: December 2026 report",
		"Personal Finance Tracker: Q4 2026 report",
		"Personal Finance Tracker: 2026 report",
	}
	got := subjectsOf(sink.msgs)
	if len(got) != len(want) {
		t.Fatalf("subjects = %v, want %v", got, want)
	}
	sort.Strings(got)
	sort.Strings(want)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("subjects = %v, want %v", got, want)
		}
	}
}

// 4. Mid-month: zero mails, zero notifications, and the user listing
// is not even run.
func TestDispatchMidMonth(t *testing.T) {
	now := time.Date(2026, 11, 15, 0, 15, 0, 0, time.UTC)
	users := &fakeUsers{users: []User{
		{ID: "u1", Email: "a@example.com", Onboarded: true, Monthly: true},
	}}
	sink := &fakeMailSink{}
	notif := &fakeNotify{}
	d := newTestDispatcher(t, now, users, &fakeCollector{}, sink, notif)

	if err := d.Dispatch(context.Background()); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(sink.msgs) != 0 || len(notif.entries) != 0 {
		t.Fatalf("want nothing sent/notified, got %d mails / %d notifications", len(sink.msgs), len(notif.entries))
	}
	if users.calls != 0 {
		t.Errorf("user listing ran %d times, want 0 (early return)", users.calls)
	}
}

// 5. One user's collect (or send) error -> exactly one report_failed
// row with the pinned shape, and the other users still get their mail.
func TestDispatchPerUserFailure(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 15, 0, 0, time.UTC)
	users := &fakeUsers{users: []User{
		{ID: "u1", Email: "good@example.com", Onboarded: true, Monthly: true},
		{ID: "u2", Email: "bad@example.com", Onboarded: true, Monthly: true},
	}}

	for name, tc := range map[string]struct {
		collErr func(string, report.Period) error
		sendErr func(string) error
		needle  string
	}{
		"collect error": {collErr: func(uid string, p report.Period) error {
			if uid == "u2" {
				return errors.New("boom-collect")
			}
			return nil
		}, needle: "boom-collect"},
		"send error": {sendErr: func(to string) error {
			if to == "bad@example.com" {
				return errors.New("boom-send")
			}
			return nil
		}, needle: "boom-send"},
	} {
		t.Run(name, func(t *testing.T) {
			coll := &fakeCollector{errFor: tc.collErr}
			sink := &fakeMailSink{errFor: tc.sendErr}
			notif := &fakeNotify{}
			d := newTestDispatcher(t, now, users, coll, sink, notif)

			if err := d.Dispatch(context.Background()); err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			if len(sink.msgs) != 1 || sink.msgs[0].To != "good@example.com" {
				t.Fatalf("the good user must still be mailed: %v", subjectsOf(sink.msgs))
			}
			if len(notif.entries) != 1 {
				t.Fatalf("want exactly 1 failure notification, got %d", len(notif.entries))
			}
			in := notif.entries[0]
			if notif.users[0] != "u2" {
				t.Errorf("notification user = %q, want u2", notif.users[0])
			}
			if in.Kind != "report_failed" {
				t.Errorf("Kind = %q", in.Kind)
			}
			if in.Title != "October 2026 report failed" {
				t.Errorf("Title = %q", in.Title)
			}
			if !strings.HasPrefix(in.Body, "report dispatch failed: ") || !strings.Contains(in.Body, tc.needle) {
				t.Errorf("Body = %q (want prefix + %q)", in.Body, tc.needle)
			}
			if got := string(in.Ref); got != `{"label":"October 2026","period":"monthly"}` {
				t.Errorf("Ref = %s", got)
			}
		})
	}
}

// 6. A not-onboarded user present in the raw list (flags set) is
// excluded: zero mails for them, the collector is never called.
func TestDispatchNotOnboardedExcluded(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 15, 0, 0, time.UTC)
	users := &fakeUsers{users: []User{
		{ID: "u1", Email: "a@example.com", Onboarded: true, Monthly: true},
		{ID: "u2", Email: "b@example.com", Onboarded: false, Monthly: true},
	}}
	coll := &fakeCollector{}
	sink := &fakeMailSink{}
	d := newTestDispatcher(t, now, users, coll, sink, &fakeNotify{})

	if err := d.Dispatch(context.Background()); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(sink.msgs) != 1 || sink.msgs[0].To != "a@example.com" {
		t.Fatalf("want exactly the onboarded user's mail, got %v", subjectsOf(sink.msgs))
	}
	if len(coll.calls) != 1 || coll.calls[0] != "u1/monthly" {
		t.Errorf("collector calls = %v (the not-onboarded user must never be collected)", coll.calls)
	}
}

// 7. Leader: two in-process schedulers sharing a fake advisory lock on
// the same tick -> exactly ONE dispatch batch (one mail per eligible
// user, not two); the loser logs the pinned skip line.
func TestTwoReplicasOneDispatch(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 15, 0, 0, time.UTC)
	lock := newFakeAdvisoryLock()
	users := &fakeUsers{users: []User{
		{ID: "u1", Email: "a@example.com", Onboarded: true, Monthly: true},
		{ID: "u2", Email: "b@example.com", Onboarded: true, Monthly: true},
	}}
	coll := &fakeCollector{blocked: make(chan struct{})}
	sink := &fakeMailSink{}
	notif := &fakeNotify{}
	d := newTestDispatcher(t, now, users, coll, sink, notif)

	entries := []Entry{{Name: "report-dispatch", Spec: ReportDispatchSpec, Key: LockKeyReport, Run: d.Dispatch}}
	sa, logA := NewScheduler(fakeClock{now: now}, lock, entries), &strings.Builder{}
	sa.Log = slog.New(slog.NewJSONHandler(logA, nil))
	sb, logB := NewScheduler(fakeClock{now: now}, lock, entries), &strings.Builder{}
	sb.Log = slog.New(slog.NewJSONHandler(logB, nil))

	ctx := context.Background()
	held := lock.watchHeld(LockKeyReport)
	done := make(chan struct{})
	go func() {
		sa.startupRun(ctx)
		close(done)
	}()
	<-held // replica A holds the lock (mid-batch: blocked in Collect)

	sb.startupRun(ctx) // replica B must skip the whole execution

	close(coll.blocked) // let A finish its batch
	<-done

	if len(sink.msgs) != 2 {
		t.Fatalf("want exactly 2 mails (one per user, NOT 4), got %d: %v", len(sink.msgs), subjectsOf(sink.msgs))
	}
	tos := map[string]bool{sink.msgs[0].To: true, sink.msgs[1].To: true}
	if !tos["a@example.com"] || !tos["b@example.com"] {
		t.Errorf("mail recipients = %v, want a@ and b@ exactly once each", tos)
	}
	if len(coll.calls) != 2 {
		t.Errorf("collector calls = %v, want exactly one per user", coll.calls)
	}
	if !strings.Contains(logB.String(), "another replica is leader") {
		t.Errorf("replica B log must contain the skip line: %s", logB.String())
	}
	if strings.Contains(logA.String(), "another replica is leader") {
		t.Errorf("replica A log must NOT contain the skip line: %s", logA.String())
	}
	acquired, released, heldNow := lock.snapshot()
	if len(acquired) != 1 || acquired[0] != LockKeyReport || len(released) != 1 || released[0] != LockKeyReport {
		t.Errorf("acquired = %v, released = %v, want one acquire+release of %d", acquired, released, LockKeyReport)
	}
	if len(heldNow) != 0 {
		t.Errorf("lock must be released: still held %v", heldNow)
	}
	if len(notif.entries) != 0 {
		t.Errorf("want 0 failure notifications, got %d", len(notif.entries))
	}
}

// 8. Hourly FI hook: the :05 entry fires at 12:05, logs the pinned
// no-op line, acquires/releases lock 71002, and the report entry does
// NOT run.
func TestFiSyncHourly(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 5, 0, 0, time.UTC)
	lock := newFakeAdvisoryLock()
	hook := NewFiSyncHook()
	reportRan := false
	entries := []Entry{
		{Name: "report-dispatch", Spec: ReportDispatchSpec, Key: LockKeyReport,
			Run: func(ctx context.Context) error { reportRan = true; return nil }},
		{Name: "fi-sync", Spec: FiSyncSpec, Key: LockKeyFiSync, Run: hook.Run},
	}
	var logB strings.Builder
	s := NewScheduler(fakeClock{now: now}, lock, entries)
	s.Log = slog.New(slog.NewJSONHandler(&logB, nil))
	hook.Log = s.Log

	s.startupRun(context.Background())

	if !strings.Contains(logB.String(), "fi sync tick (phase 9 hook, no adapters registered)") {
		t.Errorf("log must contain the pinned FI line: %s", logB.String())
	}
	if reportRan {
		t.Error("the report entry must not run at 12:05")
	}
	acquired, released, heldNow := lock.snapshot()
	if len(acquired) != 1 || acquired[0] != LockKeyFiSync || len(released) != 1 || released[0] != LockKeyFiSync {
		t.Errorf("acquired = %v, released = %v, want one acquire+release of %d", acquired, released, LockKeyFiSync)
	}
	if len(heldNow) != 0 {
		t.Errorf("lock must be released: still held %v", heldNow)
	}
}

// 9. All pairs dispatched: 3 users each wanting all three kinds, on
// Jan 1 -> the collector sees all 9 (user, period) dispatches. The
// fan-out order is nondeterministic, so compare by set + length.
func TestDispatchAllPairs(t *testing.T) {
	now := time.Date(2027, 1, 1, 0, 15, 0, 0, time.UTC)
	users := &fakeUsers{users: []User{
		{ID: "u1", Email: "a@example.com", Onboarded: true, Monthly: true, Quarterly: true, Annual: true},
		{ID: "u2", Email: "b@example.com", Onboarded: true, Monthly: true, Quarterly: true, Annual: true},
		{ID: "u3", Email: "c@example.com", Onboarded: true, Monthly: true, Quarterly: true, Annual: true},
	}}
	coll := &fakeCollector{}
	sink := &fakeMailSink{}
	d := newTestDispatcher(t, now, users, coll, sink, &fakeNotify{})

	if err := d.Dispatch(context.Background()); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	wantCalls := map[string]bool{}
	for _, uid := range []string{"u1", "u2", "u3"} {
		for _, p := range []report.Period{report.PeriodMonthly, report.PeriodQuarterly, report.PeriodAnnual} {
			wantCalls[uid+"/"+string(p)] = true
		}
	}
	if len(coll.calls) != 9 {
		t.Fatalf("collector calls = %v, want all 9 (user, period) pairs", coll.calls)
	}
	for _, c := range coll.calls {
		if !wantCalls[c] {
			t.Fatalf("unexpected collector call %q in %v", c, coll.calls)
		}
	}
	if len(sink.msgs) != 9 {
		t.Fatalf("want 9 mails, got %d: %v", len(sink.msgs), subjectsOf(sink.msgs))
	}
}

// 10. Bounded concurrency: with 9 pairs due, the fake collector's
// atomic in-flight counter must never observe more than
// dispatchConcurrency concurrent Collects.
func TestDispatchBoundedConcurrency(t *testing.T) {
	now := time.Date(2027, 1, 1, 0, 15, 0, 0, time.UTC)
	users := &fakeUsers{users: []User{
		{ID: "u1", Email: "a@example.com", Onboarded: true, Monthly: true, Quarterly: true, Annual: true},
		{ID: "u2", Email: "b@example.com", Onboarded: true, Monthly: true, Quarterly: true, Annual: true},
		{ID: "u3", Email: "c@example.com", Onboarded: true, Monthly: true, Quarterly: true, Annual: true},
	}}
	coll := &fakeCollector{}
	sink := &fakeMailSink{}
	d := newTestDispatcher(t, now, users, coll, sink, &fakeNotify{})

	if err := d.Dispatch(context.Background()); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if max := atomic.LoadInt64(&coll.maxFlight); max > dispatchConcurrency {
		t.Fatalf("observed max in-flight Collects = %d, want <= %d", max, dispatchConcurrency)
	}
}

// --- guards ---------------------------------------------------------

func TestSpecMatchesMinute(t *testing.T) {
	for name, tc := range map[string]struct {
		spec string
		now  time.Time
		want bool
	}{
		"00:15 second 0":  {"15 0 * * *", time.Date(2026, 11, 1, 0, 15, 0, 0, time.UTC), true},
		"00:15 second 30": {"15 0 * * *", time.Date(2026, 11, 1, 0, 15, 30, 0, time.UTC), true},
		"00:15 second 59": {"15 0 * * *", time.Date(2026, 11, 1, 0, 15, 59, 0, time.UTC), true},
		"00:14 off-by-1":  {"15 0 * * *", time.Date(2026, 11, 1, 0, 14, 59, 0, time.UTC), false},
		"00:16 off-by-1":  {"15 0 * * *", time.Date(2026, 11, 1, 0, 16, 0, 0, time.UTC), false},
		"hourly :05":      {"5 * * * *", time.Date(2026, 11, 1, 9, 5, 10, 0, time.UTC), true},
		"hourly :04":      {"5 * * * *", time.Date(2026, 11, 1, 9, 4, 59, 0, time.UTC), false},
		"daily midnight":  {"0 0 * * *", time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), true},
		"daily 23:59":     {"0 0 * * *", time.Date(2026, 11, 2, 23, 59, 0, 0, time.UTC), false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := specMatchesMinute(tc.spec, tc.now); got != tc.want {
				t.Fatalf("specMatchesMinute(%q, %s) = %v, want %v", tc.spec, tc.now, got, tc.want)
			}
		})
	}
}

func TestDuePeriods(t *testing.T) {
	for name, tc := range map[string]struct {
		now  time.Time
		want []report.Period
	}{
		"nov 1":      {time.Date(2026, 11, 1, 0, 15, 0, 0, time.UTC), []report.Period{report.PeriodMonthly}},
		"apr 1":      {time.Date(2026, 4, 1, 0, 15, 0, 0, time.UTC), []report.Period{report.PeriodMonthly, report.PeriodQuarterly}},
		"jul 1":      {time.Date(2026, 7, 1, 0, 15, 0, 0, time.UTC), []report.Period{report.PeriodMonthly, report.PeriodQuarterly}},
		"oct 1":      {time.Date(2026, 10, 1, 0, 15, 0, 0, time.UTC), []report.Period{report.PeriodMonthly, report.PeriodQuarterly}},
		"jan 1":      {time.Date(2027, 1, 1, 0, 15, 0, 0, time.UTC), []report.Period{report.PeriodMonthly, report.PeriodQuarterly, report.PeriodAnnual}},
		"nov 15":     {time.Date(2026, 11, 15, 0, 15, 0, 0, time.UTC), nil},
		"jan 15":     {time.Date(2026, 1, 15, 0, 15, 0, 0, time.UTC), nil},
		"apr 1 noon": {time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC), []report.Period{report.PeriodMonthly, report.PeriodQuarterly}},
	} {
		t.Run(name, func(t *testing.T) { assertPeriods(t, DuePeriods(tc.now), tc.want...) })
	}
}

func TestPeriodAsOf(t *testing.T) {
	for name, tc := range map[string]struct {
		now   time.Time
		p     report.Period
		want  time.Time
		label string
	}{
		"monthly nov 1": {time.Date(2026, 11, 1, 0, 15, 0, 0, time.UTC), report.PeriodMonthly,
			time.Date(2026, 10, 1, 0, 15, 0, 0, time.UTC), "October 2026"},
		"quarterly apr 1": {time.Date(2026, 4, 1, 0, 15, 0, 0, time.UTC), report.PeriodQuarterly,
			time.Date(2026, 1, 1, 0, 15, 0, 0, time.UTC), "Q1 2026"},
		"annual jan 1": {time.Date(2027, 1, 1, 0, 15, 0, 0, time.UTC), report.PeriodAnnual,
			time.Date(2026, 1, 1, 0, 15, 0, 0, time.UTC), "2026"},
	} {
		t.Run(name, func(t *testing.T) {
			got := PeriodAsOf(tc.now, tc.p)
			if !got.Equal(tc.want) {
				t.Fatalf("PeriodAsOf = %s, want %s", got, tc.want)
			}
			if _, _, label := report.Range(got, tc.p); label != tc.label {
				t.Fatalf("Range label = %q, want %q", label, tc.label)
			}
		})
	}
}

func TestNewClock(t *testing.T) {
	t.Run("unset", func(t *testing.T) {
		os.Unsetenv("WORKER_FAKE_NOW")
		if _, isFixed := NewClock().(FixedClock); isFixed {
			t.Fatal("unset WORKER_FAKE_NOW must give the real clock")
		}
	})
	t.Run("empty", func(t *testing.T) {
		t.Setenv("WORKER_FAKE_NOW", "")
		if _, isFixed := NewClock().(FixedClock); isFixed {
			t.Fatal("empty WORKER_FAKE_NOW must give the real clock")
		}
	})
	t.Run("valid", func(t *testing.T) {
		t.Setenv("WORKER_FAKE_NOW", "2026-11-01T00:15:00Z")
		c, isFixed := NewClock().(FixedClock)
		if !isFixed {
			t.Fatal("valid WORKER_FAKE_NOW must give the fixed clock")
		}
		want := time.Date(2026, 11, 1, 0, 15, 0, 0, time.UTC)
		if !c.Now().Equal(want) {
			t.Fatalf("fixed clock Now = %s, want %s", c.Now(), want)
		}
	})
	t.Run("invalid", func(t *testing.T) {
		t.Setenv("WORKER_FAKE_NOW", "not-a-date")
		if _, isFixed := NewClock().(FixedClock); isFixed {
			t.Fatal("invalid WORKER_FAKE_NOW must fall back to the real clock")
		}
	})
}

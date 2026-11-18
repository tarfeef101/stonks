package schedule

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/sync/errgroup"

	"finance-tracker/internal/mail"
	"finance-tracker/internal/report"
)

// Dispatch pins (step 8.3): the mail subject prefix, the failure
// notification kind, and the failure body cap (the repo's 4000-rune
// body cap, enforced in runes).
const (
	reportSubjectPrefix = "Personal Finance Tracker: "

	notificationKindFailed = "report_failed"

	maxBodyRunes = 4000
)

// User is one row of the dispatch user listing (the pinned query,
// decoded). Onboarded is always true for production rows (the query
// filters onboarded_at IS NOT NULL); the field exists so a raw list can
// express not-onboarded users (they are excluded from every period).
type User struct {
	ID        string
	Email     string
	Onboarded bool
	Monthly   bool
	Quarterly bool
	Annual    bool
}

// UserLister lists the users a dispatch may touch (production: the
// pinned query over *pgxpool.Pool; tests: a fake).
type UserLister interface {
	List(ctx context.Context) ([]User, error)
}

// ReportCollector gathers one report's Data snapshot; the production
// implementation is *report.Collector.
type ReportCollector interface {
	Collect(ctx context.Context, userID string, p report.Period, asOf time.Time, dek []byte) (report.Data, error)
}

// DekResolver resolves a user's unwrapped DEK (production: the worker's
// users.wrapped_dek lookup + KEK unwrap).
type DekResolver func(ctx context.Context, userID string) ([]byte, error)

// NotificationIn is the failure notification the dispatcher writes;
// the production implementation stores it via repo.NotificationStore.
type NotificationIn struct {
	Kind  string
	Title string
	Body  string
	Ref   json.RawMessage
}

// Notifier persists one failure notification for a user.
type Notifier interface {
	Notify(ctx context.Context, userID string, in NotificationIn) error
}

// Dispatcher runs one report dispatch at the clock instant: list the
// users, work out which periods are due, and for every (user, due
// period) run the pinned per-user pipeline (DEK -> Collect -> Generate
// -> RenderText/RenderHTML -> Send). One failing user never aborts the
// batch; its failure becomes a "report_failed" notification.
type Dispatcher struct {
	Clock      Clock
	Users      UserLister
	Collector  ReportCollector
	DekForUser DekResolver
	Mailer     mail.Sender
	Notify     Notifier
	Log        *slog.Logger
}

// NewDispatcher returns a Dispatcher with the given deps (Log defaults
// to slog.Default()).
func NewDispatcher(clock Clock, users UserLister, collector ReportCollector, dekForUser DekResolver, mailer mail.Sender, notify Notifier) *Dispatcher {
	return &Dispatcher{
		Clock:      clock,
		Users:      users,
		Collector:  collector,
		DekForUser: dekForUser,
		Mailer:     mailer,
		Notify:     notify,
		Log:        slog.Default(),
	}
}

// DuePeriods returns the report periods due at now (UTC): the 1st of a
// month is monthly; the 1st of Jan/Apr/Jul/Oct adds quarterly; Jan 1
// adds annual. Any other instant has nothing due.
func DuePeriods(now time.Time) []report.Period {
	u := now.UTC()
	if u.Day() != 1 {
		return nil
	}
	out := []report.Period{report.PeriodMonthly}
	switch u.Month() {
	case time.January, time.April, time.July, time.October:
		out = append(out, report.PeriodQuarterly)
	}
	if u.Month() == time.January {
		out = append(out, report.PeriodAnnual)
	}
	return out
}

// PeriodAsOf shifts now back into the just-ended target period: the
// 1st-of-month tick reports the previous month, the quarter-boundary
// tick the previous quarter, the Jan-1 tick the previous year. The
// result is the asOf whose report.Range is the target period. An
// unknown period panics — the app only produces the three constants.
func PeriodAsOf(now time.Time, p report.Period) time.Time {
	u := now.UTC()
	switch p {
	case report.PeriodMonthly:
		return u.AddDate(0, -1, 0)
	case report.PeriodQuarterly:
		return u.AddDate(0, -3, 0)
	case report.PeriodAnnual:
		return u.AddDate(-1, 0, 0)
	}
	panic(fmt.Sprintf("schedule: unknown period %q", p))
}

// userWants reports whether u opted into period p.
func userWants(u User, p report.Period) bool {
	switch p {
	case report.PeriodMonthly:
		return u.Monthly
	case report.PeriodQuarterly:
		return u.Quarterly
	case report.PeriodAnnual:
		return u.Annual
	}
	return false
}

// dispatchConcurrency bounds the (user, period) goroutines Dispatch
// fans out. The mail provider's burst ceiling is the sizing rationale:
// several users × up to 3 periods must not burst past it.
const dispatchConcurrency = 4

// dispatchUserPair is one (user, due period) dispatch unit — the
// fan-out work item.
type dispatchUserPair struct {
	user   User
	period report.Period
}

// Dispatch runs one dispatch at the clock instant. Nothing due (any
// non-1st day) returns before listing users. A user-list failure is
// returned — the batch cannot run; per-user failures are recorded and
// the batch continues. The (user, period) pairs fan out over a bounded
// errgroup pool (dispatchConcurrency): the original ctx is passed
// through untouched and every worker returns nil, so one failed
// dispatch neither cancels nor aborts its siblings — dispatchUser
// reports its failure via its bool and the "report_failed"
// notification.
func (d *Dispatcher) Dispatch(ctx context.Context) error {
	now := d.Clock.Now()
	periods := DuePeriods(now)
	if len(periods) == 0 {
		d.Log.InfoContext(ctx, "report dispatch: no periods due", "now", now.Format(time.RFC3339))
		return nil
	}

	users, err := d.Users.List(ctx)
	if err != nil {
		return fmt.Errorf("schedule: list users: %w", err)
	}

	// Collect every (user, period) pair first — the filters are the
	// pinned ones, unchanged: onboarded users only, and only the
	// periods the user opted into.
	var pairs []dispatchUserPair
	for _, u := range users {
		if !u.Onboarded {
			continue // the production query already excludes them; belt and braces
		}
		for _, p := range periods {
			if !userWants(u, p) {
				continue
			}
			pairs = append(pairs, dispatchUserPair{user: u, period: p})
		}
	}

	var (
		g      errgroup.Group
		mu     sync.Mutex
		sent   int
		failed int
	)
	g.SetLimit(dispatchConcurrency)
	for _, pr := range pairs {
		pr := pr
		g.Go(func() error {
			ok := d.dispatchUser(ctx, pr.user, pr.period, now)
			mu.Lock()
			if ok {
				sent++
			} else {
				failed++
			}
			mu.Unlock()
			return nil // dispatchUser reports failure via its bool; never fatal
		})
	}
	_ = g.Wait()
	d.Log.InfoContext(ctx, "report dispatch finished",
		"sent", sent, "failed", failed, "periods", len(periods))
	return nil
}

// dispatchUser runs the pinned per-user pipeline for one (user,
// period) and returns true when the mail was sent; any step's failure
// is recorded via failUser and false returned.
func (d *Dispatcher) dispatchUser(ctx context.Context, u User, p report.Period, now time.Time) bool {
	asOf := PeriodAsOf(now, p)
	_, _, label := report.Range(asOf, p)
	if err := d.runUser(ctx, u, p, asOf, label); err != nil {
		d.failUser(ctx, u, p, label, err)
		return false
	}
	return true
}

// runUser is the pinned per-user pipeline, returning the first error:
// DEK -> Collect -> Generate -> RenderText -> RenderHTML -> Send.
func (d *Dispatcher) runUser(ctx context.Context, u User, p report.Period, asOf time.Time, label string) error {
	dek, err := d.DekForUser(ctx, u.ID)
	if err != nil {
		return fmt.Errorf("schedule: dek: %w", err)
	}
	data, err := d.Collector.Collect(ctx, u.ID, p, asOf, dek)
	if err != nil {
		return fmt.Errorf("schedule: collect: %w", err)
	}
	r := report.Generate(data)
	text, err := report.RenderText(r)
	if err != nil {
		return fmt.Errorf("schedule: render text: %w", err)
	}
	html, err := report.RenderHTML(r)
	if err != nil {
		return fmt.Errorf("schedule: render html: %w", err)
	}
	m := mail.Message{
		To:      u.Email,
		Subject: reportSubjectPrefix + label + " report",
		Text:    text,
		HTML:    html,
	}
	if err := d.Mailer.Send(ctx, m); err != nil {
		return fmt.Errorf("schedule: send: %w", err)
	}
	d.Log.InfoContext(ctx, "report sent", "user", u.ID, "subject", m.Subject)
	return nil
}

// failUser records one user's dispatch failure: an error log, then a
// "report_failed" notification (whose own write failure is logged but
// must not mask the batch — mirrors the categorize runner's notify
// path).
func (d *Dispatcher) failUser(ctx context.Context, u User, p report.Period, label string, err error) {
	d.Log.ErrorContext(ctx, "report dispatch failed", "user", u.ID, "period", string(p), "error", err)
	// json.Marshal sorts map keys, so the ref is deterministic:
	// {"label":"<label>","period":"<period>"}.
	ref, rerr := json.Marshal(map[string]any{"label": label, "period": string(p)})
	if rerr != nil {
		d.Log.ErrorContext(ctx, "marshal failure ref failed", "user", u.ID, "error", rerr)
		return
	}
	in := NotificationIn{
		Kind:  notificationKindFailed,
		Title: label + " report failed",
		Body:  truncateRunes("report dispatch failed: "+err.Error(), maxBodyRunes),
		Ref:   ref,
	}
	if nerr := d.Notify.Notify(ctx, u.ID, in); nerr != nil {
		d.Log.ErrorContext(ctx, "write failure notification failed", "user", u.ID, "error", nerr)
	}
}

// truncateRunes caps s at n runes (the failure body must fit the
// repo's 4000-rune cap; the leading part of the error text is kept).
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

package schedule

// T2: the delete half of the round-33 mortgage self-heal, moved from
// the server's GET /matching request path to the worker (hourly) so
// reads stay cheap. For every user and mortgage with
// kind-"mortgage_aberrant" notifications, the matching engine is
// re-run against the linked account's debits and the stale rows (item
// no longer unclassified, or mortgage gone) are deleted. The create
// half (ensureAberrantNotifications) still runs per request in the
// server package.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"finance-tracker/internal/mortgage"
	"finance-tracker/internal/repo"
)

// AberrantKind is the notification kind this job deletes; must stay in
// lockstep with the server's aberrantNotifKind (which the server keeps
// for its create path).
const AberrantKind = "mortgage_aberrant"

// AberrantRefDoc is the jsonb ref of an aberrant-payment notification:
// the (mortgage, kind, date) pointer the (kind, ref) dedup keys on
// (round 32) and the self-heal parses back (round 33).
type AberrantRefDoc struct {
	MortgageID string `json:"mortgage_id"`
	Kind       string `json:"kind"`
	Date       string `json:"date"`
}

// SpecToEngine converts a stored repo.Mortgage into the engine's numeric
// Spec. The stored spec was validated at create/patch (7.1), so a parse
// failure here is an UNREACHABLE backstop: the error maps to 500 in the
// handler, never to a 400. AnnualRate is the PERCENT (the engine's
// convention); StartDate parses to UTC midnight (time.Parse shape).
func SpecToEngine(m repo.Mortgage) (mortgage.Spec, error) {
	principal, err := strconv.ParseFloat(m.Spec.Principal, 64)
	if err != nil {
		return mortgage.Spec{}, errors.New("stored principal " + m.Spec.Principal + " does not parse: " + err.Error())
	}
	rate, err := strconv.ParseFloat(m.Spec.AnnualRate, 64)
	if err != nil {
		return mortgage.Spec{}, errors.New("stored annual_rate " + m.Spec.AnnualRate + " does not parse: " + err.Error())
	}
	extra, err := strconv.ParseFloat(m.Spec.ExtraPaymentAmount, 64)
	if err != nil {
		return mortgage.Spec{}, errors.New("stored extra_payment_amount " + m.Spec.ExtraPaymentAmount + " does not parse: " + err.Error())
	}
	start, err := time.Parse("2006-01-02", m.Spec.StartDate)
	if err != nil {
		return mortgage.Spec{}, errors.New("stored start_date " + m.Spec.StartDate + " does not parse: " + err.Error())
	}
	events := make([]mortgage.RateEvent, 0, len(m.Spec.RateEvents))
	for i, e := range m.Spec.RateEvents {
		d, err := time.Parse("2006-01-02", e.EffectiveDate)
		if err != nil {
			return mortgage.Spec{}, errors.New("stored rate event " + strconv.Itoa(i) +
				" effective_date " + e.EffectiveDate + " does not parse: " + err.Error())
		}
		er, err := strconv.ParseFloat(e.AnnualRate, 64)
		if err != nil {
			return mortgage.Spec{}, errors.New("stored rate event " + strconv.Itoa(i) +
				" annual_rate " + e.AnnualRate + " does not parse: " + err.Error())
		}
		events = append(events, mortgage.RateEvent{
			Date:       d,
			AnnualRate: er,
			RateType:   e.RateType,
			Action:     e.Action,
		})
	}
	return mortgage.Spec{
		Principal:             principal,
		AnnualRate:            rate,
		AmortizationYears:     m.Spec.AmortizationYears,
		StartDate:             start,
		PaymentFrequency:      m.Spec.PaymentFrequency,
		ExtraPaymentAmount:    extra,
		ExtraPaymentFrequency: m.Spec.ExtraPaymentFrequency,
		ExtraPrepaymentType:   m.Spec.ExtraPrepaymentType,
		TermYears:             m.Spec.TermYears,
		RateType:              m.Spec.RateType,
		RateEvents:            events,
	}, nil
}

// UserIDLister enumerates every user id. Deliberately unfiltered (no
// onboarded/notify flags): the heal must also reach users who stopped
// receiving notifications — their stale rows must still be purged.
type UserIDLister interface {
	ListUserIDs(ctx context.Context) ([]string, error)
}

// NotifStore is the slice of *repo.NotificationStore the healer needs.
type NotifStore interface {
	ListByKind(ctx context.Context, userID, kind string) ([]repo.Notification, error)
	Delete(ctx context.Context, userID, id string) error
}

// MortgageGetter is the slice of *repo.MortgageStore the healer needs.
type MortgageGetter interface {
	Get(ctx context.Context, userID, id string, dek []byte) (repo.Mortgage, error)
}

// DebitLister is the slice of *repo.TransactionStore the healer needs.
type DebitLister interface {
	AccountDebits(ctx context.Context, accountID string) ([]repo.DebitTxn, error)
}

// MortgageHealer re-runs the mortgage matching engine for every mortgage
// that still has kind-"mortgage_aberrant" notifications and deletes the
// notifications whose (kind, date) item no longer appears unclassified in
// the current report. It is the delete half of the round-33 self-heal,
// which moved from the GET /matching request path to the worker (hourly)
// so reads stay cheap. The create half (ensureAberrantNotifications)
// still runs per request. Every failure is logged and skipped — the heal
// must never be fatal.
type MortgageHealer struct {
	Log        *slog.Logger
	Users      UserIDLister
	Notifs     NotifStore
	Mortgages  MortgageGetter
	Txns       DebitLister
	DekForUser func(ctx context.Context, uid string) ([]byte, error)
}

// Run heals every user once. A ListUserIDs error aborts the run
// (mirrors the dispatcher's ListUsers handling); per-user failures are
// logged and skipped.
func (h *MortgageHealer) Run(ctx context.Context) error {
	users, err := h.Users.ListUserIDs(ctx)
	if err != nil {
		return fmt.Errorf("schedule: list users: %w", err)
	}
	deleted := 0
	for _, uid := range users {
		deleted += h.healUser(ctx, uid)
	}
	h.Log.Info("aberrant notification self-heal finished", "users", len(users), "deleted", deleted)
	return nil
}

// healUser heals one user's aberrant rows and returns the number
// deleted. Every failure (DEK, list, fetch, debits, spec, match,
// delete) is logged and the affected user or mortgage group is skipped
// — the run continues.
func (h *MortgageHealer) healUser(ctx context.Context, uid string) (deleted int) {
	dek, err := h.DekForUser(ctx, uid)
	if err != nil {
		h.Log.Warn("aberrant notification reconcile failed", "op", "dek", "user", uid, "error", err)
		return 0
	}
	rows, err := h.Notifs.ListByKind(ctx, uid, AberrantKind)
	if err != nil {
		h.Log.Warn("aberrant notification reconcile failed", "op", "list", "user", uid, "error", err)
		return 0
	}
	// Group the parseable rows by mortgage; a row with an unparseable
	// (or incomplete) ref is deleted outright — no report can ever own
	// it.
	type parsedRow struct {
		repo.Notification
		ref AberrantRefDoc
	}
	byMortgage := map[string][]parsedRow{}
	for _, n := range rows {
		var ref AberrantRefDoc
		if err := json.Unmarshal(n.Ref, &ref); err != nil || ref.MortgageID == "" || ref.Kind == "" || ref.Date == "" {
			if derr := h.Notifs.Delete(ctx, uid, n.ID); derr != nil && !errors.Is(derr, repo.ErrNotificationNotFound) {
				h.Log.Warn("aberrant notification reconcile failed", "op", "delete", "user", uid, "notification", n.ID, "error", derr)
			}
			deleted++
			continue
		}
		byMortgage[ref.MortgageID] = append(byMortgage[ref.MortgageID], parsedRow{n, ref})
	}
	for mid, group := range byMortgage {
		m, err := h.Mortgages.Get(ctx, uid, mid, dek)
		if err != nil {
			if errors.Is(err, repo.ErrMortgageNotFound) {
				// Verified: the mortgage is gone — its rows are
				// orphans, delete the whole group.
				for _, pr := range group {
					if derr := h.Notifs.Delete(ctx, uid, pr.ID); derr != nil && !errors.Is(derr, repo.ErrNotificationNotFound) {
						h.Log.Warn("aberrant notification reconcile failed", "op", "delete", "user", uid, "notification", pr.ID, "error", derr)
					}
					deleted++
				}
				continue
			}
			h.Log.Warn("aberrant notification reconcile failed", "op", "get-mortgage", "user", uid, "mortgage", mid, "error", err)
			continue // unknown: leave this mortgage's rows
		}
		debits, err := h.Txns.AccountDebits(ctx, m.AccountID)
		if err != nil {
			h.Log.Warn("aberrant notification reconcile failed", "op", "debits", "user", uid, "mortgage", mid, "error", err)
			continue
		}
		txns := make([]mortgage.Txn, 0, len(debits))
		ok := true
		for _, d := range debits {
			amt, perr := strconv.ParseFloat(d.Amount, 64)
			if perr != nil {
				// The repo re-canonicalizes amounts on scan, so a parse
				// failure is an unreachable backstop — but a match on
				// a wrong amount would be silently wrong, so fail the
				// mortgage.
				h.Log.Warn("aberrant notification reconcile failed", "op", "parse-amount", "user", uid, "mortgage", mid, "error", perr)
				ok = false
				break
			}
			txns = append(txns, mortgage.Txn{Date: d.Date, Amount: amt})
		}
		if !ok {
			continue
		}
		spec, err := SpecToEngine(m)
		if err != nil {
			h.Log.Warn("aberrant notification reconcile failed", "op", "spec", "user", uid, "mortgage", mid, "error", err)
			continue
		}
		res, err := mortgage.Match(spec, txns)
		if err != nil {
			h.Log.Warn("aberrant notification reconcile failed", "op", "match", "user", uid, "mortgage", mid, "error", err)
			continue
		}
		live := liveAberrantKeys(res, m.Spec.PaymentClassifications)
		for _, pr := range group {
			if live[pr.ref.Kind+"\x00"+pr.ref.Date] {
				continue
			}
			if derr := h.Notifs.Delete(ctx, uid, pr.ID); derr != nil && !errors.Is(derr, repo.ErrNotificationNotFound) {
				h.Log.Warn("aberrant notification reconcile failed", "op", "delete", "user", uid, "notification", pr.ID, "error", derr)
			}
			deleted++
		}
	}
	return deleted
}

// liveAberrantKeys computes the "kind\x00date" keys of the items a
// match result still lists as unclassified (mirrors the server's
// aberrantPrompts over classificationFor). A stored classification for
// the same kind+date removes the item from the live set — the user has
// answered it, so its notification is no longer useful.
func liveAberrantKeys(res mortgage.MatchResult, cls []repo.PaymentClassification) map[string]bool {
	live := map[string]bool{}
	for _, e := range res.Extras {
		dateStr := e.Date.Format("2006-01-02")
		if !classified(cls, "extra", dateStr) {
			live["extra\x00"+dateStr] = true
		}
	}
	for _, s := range res.Shorts {
		dateStr := s.Date.Format("2006-01-02")
		if !classified(cls, "short", dateStr) {
			live["short\x00"+dateStr] = true
		}
	}
	for _, s := range res.Suggestions {
		dateStr := s.EffectiveDate.Format("2006-01-02")
		if !classified(cls, "suggestion", dateStr) {
			live["suggestion\x00"+dateStr] = true
		}
	}
	return live
}

// classified reports whether the stored classifications cover the
// anomaly kind+date; an empty classification counts as unclassified.
func classified(cls []repo.PaymentClassification, kind, dateStr string) bool {
	for _, c := range cls {
		if c.Kind == kind && c.Date == dateStr && c.Classification != "" {
			return true
		}
	}
	return false
}

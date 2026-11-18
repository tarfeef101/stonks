// Classify endpoint (round 32):
// POST /api/v1/mortgages/{id}/classifications records the user's manual
// classification of one aberrant payment the matching report flags (an
// extra or shortfall debit, or a sustained-deviation suggestion) as
// either a one-off payment against principal ("one_off"), a rate
// change ("rate_change", which ALSO upserts a rate event so the stored
// schedule re-amortizes from the new rate), or a rejection of the
// rate-change framing ("rejected": the item counts as answered, no
// rate event is created, and it can be re-classified later). The
// classification lives
// inside the DEK-encrypted spec blob (the round-23 RateEvents
// precedent), so it inherits its encryption — no new table. House order:
// nil store -> 503, auth, malformed id -> 404 BEFORE the store, body
// validation -> 400 (all violations joined), the (kind, date) pair
// missing from the CURRENT report -> 400, then ONE Update (the
// classification and, for a rate change, the event), and finally the
// matching notification for that item is DELETED (best effort, round 33:
// a resolved notification is not kept — the bell stops nudging).
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"finance-tracker/internal/mortgage"
	"finance-tracker/internal/repo"

	"github.com/go-chi/chi/v5"
)

// classifyIn is the POST /classifications body: plain strings, all
// validated by classifyProblems. The three rate fields are only
// meaningful for a rate_change; a one_off and a rejected ignore them.
type classifyIn struct {
	Date           string `json:"date"`
	Kind           string `json:"kind"`
	Classification string `json:"classification"`
	AnnualRate     string `json:"annual_rate"`
	RateType       string `json:"rate_type"`
	Action         string `json:"action"`
}

// classifyProblems returns every violation in the body, in order (the
// handler joins them with "; " and answers 400). date is a strict
// YYYY-MM-DD; kind and classification are closed enums; a rate_change
// additionally requires a positive canonical rate, a fixed/variable
// rate type, and a re_amortize/keep_payment action (canonicalAmount
// only normalizes, so the positivity check is explicit).
func classifyProblems(b classifyIn) []string {
	var problems []string
	if _, err := time.Parse("2006-01-02", b.Date); err != nil {
		problems = append(problems, "date must be YYYY-MM-DD")
	}
	switch b.Kind {
	case "extra", "short", "suggestion":
	default:
		problems = append(problems, "kind must be extra, short or suggestion")
	}
	switch b.Classification {
	case "one_off":
		// The rate fields are ignored for a one-off.
	case "rejected":
		// The rate fields are ignored for a rejection too.
	case "rate_change":
		if b.AnnualRate == "" {
			problems = append(problems, "annual_rate is required for a rate change")
		} else {
			rate, err := repo.CanonicalAmount(b.AnnualRate)
			if err != nil || strings.HasPrefix(rate, "-") || rate == "0.00" {
				problems = append(problems, "annual_rate must be a positive amount")
			}
		}
		switch b.RateType {
		case "fixed", "variable":
		default:
			problems = append(problems, "rate_type must be fixed or variable")
		}
		switch b.Action {
		case "re_amortize", "keep_payment":
		default:
			problems = append(problems, "action must be re_amortize or keep_payment")
		}
	default:
		problems = append(problems, "classification must be one_off, rate_change or rejected")
	}
	return problems
}

// matchingHas reports whether the engine's report carries an item of
// the given kind on the given date (suggestions key on their effective
// date). Classifying a pair the report no longer carries would write a
// classification with nothing to classify, so the handler 400s.
func matchingHas(res mortgage.MatchResult, kind, date string) bool {
	format := "2006-01-02"
	switch kind {
	case "extra":
		for _, e := range res.Extras {
			if e.Date.Format(format) == date {
				return true
			}
		}
	case "short":
		for _, s := range res.Shorts {
			if s.Date.Format(format) == date {
				return true
			}
		}
	case "suggestion":
		for _, s := range res.Suggestions {
			if s.EffectiveDate.Format(format) == date {
				return true
			}
		}
	}
	return false
}

// upsertClassification replaces the stored classification of the same
// (kind, date) in place, or appends one when there is none yet.
func upsertClassification(cur []repo.PaymentClassification, pc repo.PaymentClassification) []repo.PaymentClassification {
	for i := range cur {
		if cur[i].Kind == pc.Kind && cur[i].Date == pc.Date {
			cur[i] = pc
			return cur
		}
	}
	return append(cur, pc)
}

// upsertRateEvent replaces the stored rate event with the same
// effective date in place, or inserts one keeping the slice strictly
// ascending (YYYY-MM-DD strings compare lexicographically).
func upsertRateEvent(cur []repo.RateEvent, ev repo.RateEvent) []repo.RateEvent {
	for i := range cur {
		if cur[i].EffectiveDate == ev.EffectiveDate {
			cur[i] = ev
			return cur
		}
	}
	for i := range cur {
		if ev.EffectiveDate < cur[i].EffectiveDate {
			next := make([]repo.RateEvent, 0, len(cur)+1)
			next = append(next, cur[:i]...)
			next = append(next, ev)
			next = append(next, cur[i:]...)
			return next
		}
	}
	return append(cur, ev)
}

// deleteAberrantNotification deletes the lazily-created aberrant-payment
// notification for this (kind, date) (round 33: the user just answered
// the question, so the row is resolved and is not kept — round 32 kept
// it as read). A missing row is EXPECTED (notifications are only
// created when a report was viewed) and silently ignored; any other
// failure is logged only — the classification itself is already stored.
func (h *handler) deleteAberrantNotification(ctx context.Context, c caller, mortgageID, kind, date string) {
	if h.notifications == nil {
		return
	}
	n, err := h.notifications.FindByKindRef(ctx, c.row.ID, aberrantNotifKind, aberrantRef(mortgageID, kind, date))
	if err != nil {
		if !errors.Is(err, repo.ErrNotificationNotFound) {
			slog.Error("aberrant notification lookup failed", "mortgage", mortgageID, "kind", kind, "date", date, "error", err)
		}
		return
	}
	if err := h.notifications.Delete(ctx, c.row.ID, n.ID); err != nil && !errors.Is(err, repo.ErrNotificationNotFound) {
		slog.Error("aberrant notification not deleted", "notification", n.ID, "error", err)
	}
}

// mortgageClassifyPost serves POST /api/v1/mortgages/{id}/classifications
// (round 32). See the file header for the contract; the response is the
// updated mortgage (200, the PATCH convention) so the client can pick
// up the new rate events without a follow-up GET.
func (h *handler) mortgageClassifyPost(w http.ResponseWriter, r *http.Request) {
	if h.mortgages == nil || h.transactions == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if !uuidShapeOK(id) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "mortgage not found")
		return
	}
	var b classifyIn
	if err := decodeBody(r, &b); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "body must be a JSON object: "+err.Error())
		return
	}
	if problems := classifyProblems(b); len(problems) > 0 {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, strings.Join(problems, "; "))
		return
	}
	m, err := h.mortgages.Get(r.Context(), c.row.ID, id, c.dek)
	if err != nil {
		h.mortgageError(w, "classify", err)
		return
	}
	fail := func(err error) {
		slog.Error("mortgage operation failed", "op", "classify", "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "mortgage operation failed")
	}
	res, err := h.loadMatching(r.Context(), m)
	if err != nil {
		fail(err)
		return
	}
	if !matchingHas(res, b.Kind, b.Date) {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest,
			fmt.Sprintf("no %s on %s in the current matching report", b.Kind, b.Date))
		return
	}
	pc := repo.PaymentClassification{Date: b.Date, Kind: b.Kind, Classification: b.Classification}
	if b.Classification == "rate_change" {
		rate, _ := repo.CanonicalAmount(b.AnnualRate) // validated above
		annualRate, rateType, action := rate, b.RateType, b.Action
		pc.AnnualRate = &annualRate
		pc.RateType = &rateType
		pc.Action = &action
	}
	p := repo.MortgagePatch{
		PaymentClassifications: upsertClassification(m.Spec.PaymentClassifications, pc),
	}
	if b.Classification == "rate_change" {
		p.RateEvents = upsertRateEvent(m.Spec.RateEvents, repo.RateEvent{
			EffectiveDate: b.Date,
			AnnualRate:    *pc.AnnualRate,
			RateType:      *pc.RateType,
			Action:        *pc.Action,
		})
	}
	updated, err := h.mortgages.Update(r.Context(), c.row.ID, id, p, c.dek)
	if err != nil {
		h.mortgageError(w, "classify", err)
		return
	}
	h.deleteAberrantNotification(r.Context(), c, m.ID, b.Kind, b.Date)
	writeJSON(w, http.StatusOK, toMortgageOut(updated))
}

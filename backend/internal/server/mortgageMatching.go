// Matching endpoint (round 25, plan 7.3 continuation):
// GET /api/v1/mortgages/{id}/matching replays the linked account's
// debit transactions against the stored schedule and reports, read-only,
// how the actual payments diverge from the plan (extras, shorts, missing
// periods, the actual-vs-planned balance walk, and rate-change
// suggestions from sustained deviation runs). As with the amortization
// handler, every amount is served as a canonical 2-decimal string and
// every slice is `[]`, never null.
//
// Round 32 layers classification on top: each extra, short, and
// suggestion carries a `classification` (null = the user has not
// answered the one-off-vs-rate-change question yet), and every
// unclassified item lazily creates ONE "mortgage_aberrant"
// notification (deduped on the (kind, ref) pair) so the bell keeps
// nudging the user until the item is classified through
// POST /classifications.
//
// Round 33 closes the loop: classifying an item DELETES its
// notification (mortgageClassify.go). Stale-row reconciliation (a row
// whose item is no longer unclassified, or whose mortgage no longer
// exists) now runs in the worker plane — schedule.MortgageHealer —
// instead of on the notifications GET.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"finance-tracker/internal/mortgage"
	"finance-tracker/internal/repo"
	"finance-tracker/internal/schedule"

	"github.com/go-chi/chi/v5"
)

// aberrantNotifKind is the notification kind of the lazily-created
// aberrant-payment prompts (round 32).
const aberrantNotifKind = "mortgage_aberrant"

// matchAmountOut is one amount reported outside the period table: an
// isolated over/under payment, a debit off the plan grid, or a debit
// after the actual payoff (a positive magnitude). Classification is the
// user's one-off-vs-rate-change answer for this item (round 32; null
// when it has not been answered yet).
type matchAmountOut struct {
	Date           string  `json:"date"`
	Amount         string  `json:"amount"`
	Classification *string `json:"classification"`
}

// matchSuggestionOut is a rate-change candidate derived from a sustained
// deviation run (the effective date is the run's first period).
// Classification as on matchAmountOut.
type matchSuggestionOut struct {
	EffectiveDate     string  `json:"effective_date"`
	ImpliedAnnualRate string  `json:"implied_annual_rate"`
	ObservedPayment   string  `json:"observed_payment"`
	ObservedPeriods   int     `json:"observed_periods"`
	Classification    *string `json:"classification"`
}

// matchPeriodOut is one row of the period table on the wire: one plan
// date from the first plan date to the last matched plan date.
type matchPeriodOut struct {
	Date           string  `json:"date"`
	Payment        *string `json:"payment"` // null when no debit matched
	Extra          *string `json:"extra"`   // magnitude when the period overpaid
	Short          *string `json:"short"`   // magnitude when the period underpaid
	Balance        string  `json:"balance"`
	PlannedBalance string  `json:"planned_balance"`
}

// matchingOut is the GET /matching body: the engine's MatchResult in
// canonical strings (same field order as the struct).
type matchingOut struct {
	ScheduledPayment   string               `json:"scheduled_payment"`
	PlannedPayoffDate  string               `json:"planned_payoff_date"`
	PlannedPayoffIndex int                  `json:"planned_payoff_index"`
	ActualPayoffDate   *string              `json:"actual_payoff_date"`
	LastMatchedDate    *string              `json:"last_matched_date"`
	ActualBalance      string               `json:"actual_balance"`
	PlannedBalance     string               `json:"planned_balance"`
	PeriodsMatched     int                  `json:"periods_matched"`
	PeriodsMissing     int                  `json:"periods_missing"`
	Extras             []matchAmountOut     `json:"extras"`
	Shorts             []matchAmountOut     `json:"shorts"`
	Suggestions        []matchSuggestionOut `json:"suggestions"`
	Flags              []string             `json:"flags"`
	Periods            []matchPeriodOut     `json:"periods"`
}

// toMatchingOut maps the engine's MatchResult to the wire DTO: dates as
// YYYY-MM-DD, every amount via fmtAmt (the engine's round2 guarantees
// cent-exact values). All slices are always served, `[]` included.
func toMatchingOut(m mortgage.MatchResult) matchingOut {
	extras := make([]matchAmountOut, len(m.Extras))
	for i, e := range m.Extras {
		extras[i] = matchAmountOut{Date: e.Date.Format("2006-01-02"), Amount: fmtAmt(e.Amount)}
	}
	shorts := make([]matchAmountOut, len(m.Shorts))
	for i, s := range m.Shorts {
		shorts[i] = matchAmountOut{Date: s.Date.Format("2006-01-02"), Amount: fmtAmt(s.Amount)}
	}
	suggestions := make([]matchSuggestionOut, len(m.Suggestions))
	for i, s := range m.Suggestions {
		suggestions[i] = matchSuggestionOut{
			EffectiveDate:     s.EffectiveDate.Format("2006-01-02"),
			ImpliedAnnualRate: fmtAmt(s.ImpliedAnnualRate),
			ObservedPayment:   fmtAmt(s.ObservedPayment),
			ObservedPeriods:   s.ObservedPeriods,
		}
	}
	periods := make([]matchPeriodOut, len(m.Periods))
	for i, p := range m.Periods {
		row := matchPeriodOut{
			Date:           p.Date.Format("2006-01-02"),
			Balance:        fmtAmt(p.Balance),
			PlannedBalance: fmtAmt(p.PlannedBalance),
		}
		if p.Payment != nil {
			v := fmtAmt(*p.Payment)
			row.Payment = &v
		}
		if p.Extra != nil {
			v := fmtAmt(*p.Extra)
			row.Extra = &v
		}
		if p.Short != nil {
			v := fmtAmt(*p.Short)
			row.Short = &v
		}
		periods[i] = row
	}
	out := matchingOut{
		ScheduledPayment:   fmtAmt(m.ScheduledPayment),
		PlannedPayoffDate:  m.PlannedPayoffDate.Format("2006-01-02"),
		PlannedPayoffIndex: m.PlannedPayoffIndex,
		ActualBalance:      fmtAmt(m.ActualBalance),
		PlannedBalance:     fmtAmt(m.PlannedBalance),
		PeriodsMatched:     m.PeriodsMatched,
		PeriodsMissing:     m.PeriodsMissing,
		Extras:             extras,
		Shorts:             shorts,
		Suggestions:        suggestions,
		Flags:              m.Flags,
		Periods:            periods,
	}
	if m.ActualPayoffDate != nil {
		v := m.ActualPayoffDate.Format("2006-01-02")
		out.ActualPayoffDate = &v
	}
	if m.LastMatchedDate != nil {
		v := m.LastMatchedDate.Format("2006-01-02")
		out.LastMatchedDate = &v
	}
	return out
}

// classificationFor looks up the stored classification of one item of
// the given kind on the given date (suggestions key on their effective
// date); nil when the item has not been classified yet.
func classificationFor(cls []repo.PaymentClassification, kind, date string) *string {
	for _, c := range cls {
		if c.Kind == kind && c.Date == date {
			v := c.Classification
			return &v
		}
	}
	return nil
}

// annotateMatchingOut stamps the stored classifications onto the wire
// report (round 32): extras and shorts key on their debit date,
// suggestions on their effective date. Items without a stored
// classification stay null.
func annotateMatchingOut(out matchingOut, cls []repo.PaymentClassification) matchingOut {
	if len(cls) == 0 {
		return out
	}
	for i := range out.Extras {
		out.Extras[i].Classification = classificationFor(cls, "extra", out.Extras[i].Date)
	}
	for i := range out.Shorts {
		out.Shorts[i].Classification = classificationFor(cls, "short", out.Shorts[i].Date)
	}
	for i := range out.Suggestions {
		out.Suggestions[i].Classification = classificationFor(cls, "suggestion", out.Suggestions[i].EffectiveDate)
	}
	return out
}

// loadMatching replays the linked account's debit transactions against
// the stored schedule and returns the engine's result. The mortgage is
// passed in ALREADY FETCHED; errors are returned unmapped — the callers
// (the GET and classify handlers) log and answer 500 themselves.
func (h *handler) loadMatching(ctx context.Context, m repo.Mortgage) (mortgage.MatchResult, error) {
	spec, err := specToEngine(m)
	if err != nil {
		return mortgage.MatchResult{}, err
	}
	// One narrow query: the linked account's debits only. The matcher
	// needs every debit (a 25-year mortgage has hundreds of payments)
	// and re-filters debits internally, so the credits and the 100-row
	// pagination round trips were pure overhead.
	debits, lerr := h.transactions.AccountDebits(ctx, m.AccountID)
	if lerr != nil {
		return mortgage.MatchResult{}, lerr
	}
	var txns []mortgage.Txn
	for _, d := range debits {
		amt, perr := strconv.ParseFloat(d.Amount, 64)
		if perr != nil {
			// The repo re-canonicalizes amounts on scan, so a parse
			// failure is an unreachable backstop.
			return mortgage.MatchResult{}, perr
		}
		txns = append(txns, mortgage.Txn{Date: d.Date, Amount: amt})
	}
	return mortgage.Match(spec, txns)
}

// aberrantRefDoc is the jsonb ref of an aberrant-payment notification:
// the (mortgage, kind, date) pointer the (kind, ref) dedup keys on
// (round 32) and the self-heal parses back (round 33).
type aberrantRefDoc = schedule.AberrantRefDoc

// aberrantRef builds the ref bytes. encoding/json sorts map keys, so
// the bytes are deterministic and the (kind, ref) dedup is stable —
// keep the map form: the rows created in round 32 are byte-identical.
func aberrantRef(mortgageID, kind, date string) []byte {
	ref, _ := json.Marshal(map[string]any{
		"mortgage_id": mortgageID,
		"kind":        kind,
		"date":        date,
	})
	return ref
}

// aberrantPrompt is one unclassified aberrant item rendered as its
// notification prompt (round 33): the (kind, date) pair that keys the
// ref plus the title both writers share.
type aberrantPrompt struct {
	kind string
	date string
	body string
}

// aberrantPrompts renders every still-unclassified item of the report
// (round 33). The creation pass (ensureAberrantNotifications) and the
// worker self-heal (schedule.MortgageHealer) both key on these, so
// the titles can never drift between the two writers.
func aberrantPrompts(out matchingOut) []aberrantPrompt {
	var prompts []aberrantPrompt
	for _, e := range out.Extras {
		if e.Classification == nil {
			prompts = append(prompts, aberrantPrompt{"extra", e.Date,
				fmt.Sprintf("Mortgage extra of %s on %s needs classification", e.Amount, e.Date)})
		}
	}
	for _, s := range out.Shorts {
		if s.Classification == nil {
			prompts = append(prompts, aberrantPrompt{"short", s.Date,
				fmt.Sprintf("Mortgage shortfall of %s on %s needs classification", s.Amount, s.Date)})
		}
	}
	for _, s := range out.Suggestions {
		if s.Classification == nil {
			prompts = append(prompts, aberrantPrompt{"suggestion", s.EffectiveDate,
				fmt.Sprintf("A sustained payment change since %s needs classification", s.EffectiveDate)})
		}
	}
	return prompts
}

// ensureAberrantNotifications lazily creates ONE notification per
// unclassified aberrant item in the report (round 32) so the bell
// nudges the user to classify it. The dedup is FindByKindRef on
// (kind, ref) in ANY read state, so revisits and re-visits after a
// mark-read never duplicate the row. Skipped when the notification
// store is not wired; Find and Create failures are logged only — the
// report still serves.
func (h *handler) ensureAberrantNotifications(ctx context.Context, c caller, m repo.Mortgage, out matchingOut) {
	if h.notifications == nil {
		return
	}
	for _, p := range aberrantPrompts(out) {
		ref := aberrantRef(m.ID, p.kind, p.date)
		_, err := h.notifications.FindByKindRef(ctx, c.row.ID, aberrantNotifKind, ref)
		if err == nil {
			continue // already exists (any read state)
		}
		if !errors.Is(err, repo.ErrNotificationNotFound) {
			slog.Error("aberrant notification not recorded", "mortgage", m.ID, "kind", p.kind, "date", p.date, "error", err)
			continue
		}
		if _, cerr := h.notifications.Create(ctx, c.row.ID, repo.NotificationIn{
			Kind:  aberrantNotifKind,
			Title: p.body,
			Body:  p.body,
			Ref:   ref,
		}); cerr != nil {
			slog.Error("aberrant notification not recorded", "mortgage", m.ID, "kind", p.kind, "date", p.date, "error", cerr)
		}
	}
}

// mortgageMatchingGet serves GET /api/v1/mortgages/{id}/matching. House
// order as the amortization handler: nil store -> 503, auth, malformed
// id -> 404 BEFORE the store, store error mapping, specToEngine (500
// backstop — a stored spec always validates), then the linked account's
// debit transactions pulled with one narrow query and the engine. The
// engine never errors on well-formed input, so Match failure is also a
// 500 backstop. The stored classifications are stamped onto the report,
// and one notification per still-unclassified item is lazily created
// (round 32).
func (h *handler) mortgageMatchingGet(w http.ResponseWriter, r *http.Request) {
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
	m, err := h.mortgages.Get(r.Context(), c.row.ID, id, c.dek)
	if err != nil {
		h.mortgageError(w, "matching", err)
		return
	}
	fail := func(err error) {
		slog.Error("mortgage operation failed", "op", "matching", "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "mortgage operation failed")
	}
	res, err := h.loadMatching(r.Context(), m)
	if err != nil {
		fail(err)
		return
	}
	out := annotateMatchingOut(toMatchingOut(res), m.Spec.PaymentClassifications)
	h.ensureAberrantNotifications(r.Context(), c, m, out)
	writeJSON(w, http.StatusOK, out)
}

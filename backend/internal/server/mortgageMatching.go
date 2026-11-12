// Matching endpoint (round 25, plan 7.3 continuation):
// GET /api/v1/mortgages/{id}/matching replays the linked account's
// debit transactions against the stored schedule and reports, read-only,
// how the actual payments diverge from the plan (extras, shorts, missing
// periods, the actual-vs-planned balance walk, and rate-change
// suggestions from sustained deviation runs). NOTHING is mutated: a
// confirmed suggestion is applied through the round-23 PATCH rate_events
// path. As with the amortization handler, every amount is served as a
// canonical 2-decimal string and every slice is `[]`, never null.
package server

import (
	"log/slog"
	"net/http"
	"strconv"

	"finance-tracker/internal/mortgage"
	"finance-tracker/internal/repo"

	"github.com/go-chi/chi/v5"
)

// matchAmountOut is one amount reported outside the period table: an
// isolated over/under payment, a debit off the plan grid, or a debit
// after the actual payoff (a positive magnitude).
type matchAmountOut struct {
	Date   string `json:"date"`
	Amount string `json:"amount"`
}

// matchSuggestionOut is a rate-change candidate derived from a sustained
// deviation run (the effective date is the run's first period).
type matchSuggestionOut struct {
	EffectiveDate     string `json:"effective_date"`
	ImpliedAnnualRate string `json:"implied_annual_rate"`
	ObservedPayment   string `json:"observed_payment"`
	ObservedPeriods   int    `json:"observed_periods"`
}

// matchPeriodOut is one row of the period table on the wire: one plan
// date from the first plan date to the last matched plan date.
type matchPeriodOut struct {
	Date           string  `json:"date"`
	Payment        *string `json:"payment"` // null when no debit matched
	Extra          *string `json:"extra"`   // magnitude when the period overpaid
	Short          *string `json:"short"`   // magnitude when the period underpaid
	Balance        string  `json:"balance"`
	PlannedBalance string `json:"planned_balance"`
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

// mortgageMatchingGet serves GET /api/v1/mortgages/{id}/matching. House
// order as the amortization handler: nil store -> 503, auth, malformed
// id -> 404 BEFORE the store, store error mapping, specToEngine (500
// backstop — a stored spec always validates), then the linked account's
// transactions pulled in full (the repo clamps Limit to 100; page until
// a short page) and the engine. The engine never errors on well-formed
// input, so Match failure is also a 500 backstop.
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
	spec, err := specToEngine(m)
	if err != nil {
		slog.Error("mortgage operation failed", "op", "matching", "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "mortgage operation failed")
		return
	}
	fail := func(err error) {
		slog.Error("mortgage operation failed", "op", "matching", "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "mortgage operation failed")
	}
	// Pull the linked account's transactions in full. The matcher needs
	// every debit (a 25-year mortgage has hundreds of payments), so page
	// past the repo's 100-row clamp until a short page.
	var txns []mortgage.Txn
	offset := 0
	for {
		page, lerr := h.transactions.List(r.Context(), c.row.ID, repo.TransactionFilter{
			AccountIDs: []string{m.AccountID},
			Limit:      100,
			Offset:     offset,
		}, c.dek)
		if lerr != nil {
			fail(lerr)
			return
		}
		for _, t := range page {
			amt, perr := strconv.ParseFloat(t.Amount, 64)
			if perr != nil {
				// The repo re-canonicalizes amounts on scan, so a parse
				// failure is an unreachable backstop.
				fail(perr)
				return
			}
			txns = append(txns, mortgage.Txn{Date: t.DatePosted, Amount: amt})
		}
		if len(page) < 100 {
			break
		}
		offset += len(page)
	}
	res, err := mortgage.Match(spec, txns)
	if err != nil {
		fail(err)
		return
	}
	writeJSON(w, http.StatusOK, toMatchingOut(res))
}

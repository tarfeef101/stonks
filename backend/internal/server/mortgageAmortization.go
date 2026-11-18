// Amortization endpoints (plan step 7.3) over the 7.2 mortgage engine:
// GET /api/v1/mortgages/{id}/amortization serves the stored spec's full
// schedule; POST /api/v1/mortgages/{id}/amortization/what-if compares a
// scenario (rate / recurring extra / one-time prepayment / payment
// frequency / renewal — round 23) against the base schedule. This
// handler is the ONLY converter between the
// repo.MortgageSpec canonical strings and the engine's numeric
// mortgage.Spec: the engine (7.2) is pure and never imports the repo
// package. Every amount is served as a canonical 2-decimal string
// (strconv.FormatFloat 'f' 2); the engine's own round2 guarantees the
// values are already cent-exact.
package server

import (
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"finance-tracker/internal/mortgage"
	"finance-tracker/internal/repo"
	"finance-tracker/internal/schedule"

	"github.com/go-chi/chi/v5"
)

// paymentOut is one schedule row on the wire: every amount is a
// canonical 2-decimal string.
type paymentOut struct {
	Date      string `json:"date"`
	Payment   string `json:"payment"`
	Interest  string `json:"interest"`
	Principal string `json:"principal"`
	Extra     string `json:"extra"`
	Balance   string `json:"balance"`
}

// scheduleOut is the amortization body: the 7.2 Schedule in canonical
// strings. len(payments) == payoff_index (the engine's invariant).
// next_renewal (round 23) is the current term's expiry date when the
// generating spec carries term metadata; omitted otherwise (omitempty).
type scheduleOut struct {
	BasePayment   string       `json:"base_payment"`
	PayoffDate    string       `json:"payoff_date"`
	PayoffIndex   int          `json:"payoff_index"`
	TotalPaid     string       `json:"total_paid"`
	TotalInterest string       `json:"total_interest"`
	Payments      []paymentOut `json:"payments"`
	NextRenewal   string       `json:"next_renewal,omitempty"`
}

// whatIfPrepayment is the one-time prepayment sub-object of the what-if
// body (both fields required when the object is present).
type whatIfPrepayment struct {
	Amount *string `json:"amount"`
	Date   *string `json:"date"`
}

// whatIfRenewal is the renewal / rate-change sub-object of the what-if
// body (round 23): on date the rate switches to annual_rate and the
// regular payment is recomputed over the remaining amortization (a
// re_amortize renewal event that supersedes any stored rate events
// dated >= date). date and annual_rate are required when the object is
// present; rate_type is optional metadata.
type whatIfRenewal struct {
	Date       *string `json:"date"`
	AnnualRate *string `json:"annual_rate"`
	RateType   *string `json:"rate_type"`
}

// whatIfIn is the POST body: ALL fields optional, at least one present.
// A nil field keeps the stored spec value.
type whatIfIn struct {
	AnnualRate            *string           `json:"annual_rate"`
	ExtraPaymentAmount    *string           `json:"extra_payment_amount"`
	ExtraPaymentFrequency *string           `json:"extra_payment_frequency"`
	OneTimePrepayment     *whatIfPrepayment `json:"one_time_prepayment"`
	PaymentFrequency      *string           `json:"payment_frequency"`
	Renewal               *whatIfRenewal    `json:"renewal"`
}

// whatIfOut carries EXACTLY the plan's five what-if fields.
// interest_saved is signed (a rate increase costs interest: "-123.45").
type whatIfOut struct {
	PayoffDate    string      `json:"payoff_date"`
	TotalInterest string      `json:"total_interest"`
	MonthsSaved   int         `json:"months_saved"`
	InterestSaved string      `json:"interest_saved"`
	Schedule      scheduleOut `json:"schedule"`
}

// fmtAmt renders a float as the canonical 2-decimal string.
func fmtAmt(v float64) string {
	return strconv.FormatFloat(v, 'f', 2, 64)
}

// toScheduleOut maps the engine's Schedule to the wire DTO: dates as
// YYYY-MM-DD, every amount via fmtAmt. spec is the spec the schedule
// was generated from: next_renewal is computed from IT (a what-if
// schedule passes the composed scenario spec, so a simulated renewal
// moves the date), and omitted when the spec carries no term.
func toScheduleOut(sch mortgage.Schedule, spec mortgage.Spec) scheduleOut {
	payments := make([]paymentOut, len(sch.Payments))
	for i, p := range sch.Payments {
		payments[i] = paymentOut{
			Date:      p.Date.Format("2006-01-02"),
			Payment:   fmtAmt(p.Payment),
			Interest:  fmtAmt(p.Interest),
			Principal: fmtAmt(p.Principal),
			Extra:     fmtAmt(p.Extra),
			Balance:   fmtAmt(p.Balance),
		}
	}
	out := scheduleOut{
		BasePayment:   fmtAmt(sch.BasePayment),
		PayoffDate:    sch.PayoffDate.Format("2006-01-02"),
		PayoffIndex:   sch.PayoffIndex,
		TotalPaid:     fmtAmt(sch.TotalPaid),
		TotalInterest: fmtAmt(sch.TotalInterest),
		Payments:      payments,
	}
	if d, ok := mortgage.NextRenewalDate(spec); ok {
		out.NextRenewal = d.Format("2006-01-02")
	}
	return out
}

// specToEngine converts a stored repo.Mortgage into the engine's numeric
// Spec; the conversion lives in schedule.SpecToEngine, shared with the
// worker's mortgage self-heal. Parse failures remain an UNREACHABLE
// backstop (the stored spec was validated at create/patch, 7.1) that
// maps to 500 here, never to a 400.
func specToEngine(m repo.Mortgage) (mortgage.Spec, error) {
	return schedule.SpecToEngine(m)
}

// whatIfProblems validates the PRESENT what-if fields, in wire order,
// and returns their problems (the caller "; "-joins them into ONE 400).
// Nil fields are skipped (absent = keep the stored value). The Canonical
// amount errors are passed through verbatim (the 7.1 wording).
// storedStart is the stored spec's start date (zero when unparseable —
// the renewal after-start check is then skipped; the stored spec always
// parsed at create/patch, so this is a backstop, not a path).
func whatIfProblems(in *whatIfIn, storedStart time.Time) []string {
	var problems []string
	if in.AnnualRate != nil {
		v, err := strconv.ParseFloat(*in.AnnualRate, 64)
		switch {
		case err != nil:
			problems = append(problems, "annual_rate must be a number")
		case math.IsNaN(v) || v <= 0 || v > 100:
			problems = append(problems, "annual_rate must be greater than 0 and at most 100")
		}
	}
	if in.ExtraPaymentAmount != nil {
		if c, err := repo.CanonicalAmount(*in.ExtraPaymentAmount); err != nil {
			problems = append(problems, err.Error())
		} else if v, _ := strconv.ParseFloat(c, 64); v < 0 {
			problems = append(problems, "extra_payment_amount must not be negative")
		}
	}
	if in.ExtraPaymentFrequency != nil {
		switch *in.ExtraPaymentFrequency {
		case "monthly", "quarterly", "yearly":
		default:
			problems = append(problems, "extra_payment_frequency must be monthly, quarterly, or yearly")
		}
	}
	if in.OneTimePrepayment != nil {
		pt := in.OneTimePrepayment
		if pt.Amount == nil {
			problems = append(problems, "one_time_prepayment.amount is required")
		} else {
			if c, err := repo.CanonicalAmount(*pt.Amount); err != nil {
				problems = append(problems, err.Error())
			} else if v, _ := strconv.ParseFloat(c, 64); v <= 0 {
				problems = append(problems, "one_time_prepayment.amount must be greater than zero")
			}
		}
		if pt.Date == nil {
			problems = append(problems, "one_time_prepayment.date is required")
		} else if _, err := time.Parse("2006-01-02", *pt.Date); err != nil {
			problems = append(problems, "one_time_prepayment.date must be a date in YYYY-MM-DD form")
		}
	}
	if in.PaymentFrequency != nil {
		switch *in.PaymentFrequency {
		case "monthly", "semi_monthly", "biweekly":
		default:
			problems = append(problems, "payment_frequency must be monthly, semi_monthly, or biweekly")
		}
	}
	if in.Renewal != nil {
		ren := in.Renewal
		if ren.Date == nil {
			problems = append(problems, "renewal.date is required")
		} else {
			d, err := time.Parse("2006-01-02", *ren.Date)
			switch {
			case err != nil:
				problems = append(problems, "renewal.date must be a date in YYYY-MM-DD form")
			case !storedStart.IsZero() && !d.After(storedStart):
				problems = append(problems, "renewal.date must be after the start date")
			}
		}
		if ren.AnnualRate == nil {
			problems = append(problems, "renewal.annual_rate is required")
		} else {
			v, err := strconv.ParseFloat(*ren.AnnualRate, 64)
			switch {
			case err != nil:
				problems = append(problems, "renewal.annual_rate must be a number")
			case math.IsNaN(v) || v <= 0 || v > 100:
				problems = append(problems, "renewal.annual_rate must be greater than 0 and at most 100")
			}
		}
		if ren.RateType != nil {
			switch *ren.RateType {
			case "fixed", "variable":
			default:
				problems = append(problems, "renewal.rate_type must be fixed or variable")
			}
		}
	}
	return problems
}

// mortgageAmortizationGet serves GET /api/v1/mortgages/{id}/amortization.
// Same house order as 7.1: nil store -> 503, auth, malformed id -> 404
// BEFORE the store, store error mapping, then the engine (a stored spec
// always validates, so engine failure is a 500 backstop).
func (h *handler) mortgageAmortizationGet(w http.ResponseWriter, r *http.Request) {
	if h.mortgages == nil {
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
		h.mortgageError(w, "amortization", err)
		return
	}
	spec, err := specToEngine(m)
	if err != nil {
		slog.Error("mortgage operation failed", "op", "amortization", "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "mortgage operation failed")
		return
	}
	sch, err := mortgage.Generate(spec)
	if err != nil {
		slog.Error("mortgage operation failed", "op", "amortization", "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "mortgage operation failed")
		return
	}
	writeJSON(w, http.StatusOK, toScheduleOut(sch, spec))
}

// mortgageWhatIfPost serves POST /api/v1/mortgages/{id}/amortization/what-if.
// Steps 1-4 are identical to the GET; then the body: decode (400 house
// text), the all-nil empty check, the present-field validation (ONE 400,
// "; "-joined), the scenario composition, and the engine (the
// after-payoff prepayment error maps to its EXACT 400 text; anything
// else is a 500 backstop).
func (h *handler) mortgageWhatIfPost(w http.ResponseWriter, r *http.Request) {
	if h.mortgages == nil {
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
		h.mortgageError(w, "what_if", err)
		return
	}
	var in whatIfIn
	if err := decodeBody(r, &in); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "body must be a JSON object: "+err.Error())
		return
	}
	if in.AnnualRate == nil && in.ExtraPaymentAmount == nil &&
		in.ExtraPaymentFrequency == nil && in.OneTimePrepayment == nil &&
		in.PaymentFrequency == nil && in.Renewal == nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "empty what-if: provide at least one field")
		return
	}
	// The renewal is checked against the stored start date (the row was
	// fetched before the body validation, per the house order).
	storedStart, _ := time.Parse("2006-01-02", m.Spec.StartDate)
	if problems := whatIfProblems(&in, storedStart); len(problems) > 0 {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, strings.Join(problems, "; "))
		return
	}
	spec, err := specToEngine(m)
	if err != nil {
		slog.Error("mortgage operation failed", "op", "what_if", "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "mortgage operation failed")
		return
	}
	// Compose the scenario: a present (validated) field overrides the
	// stored spec value; nil keeps it. The validation above guarantees
	// every dereference/parse here succeeds.
	var sc mortgage.Scenario
	if in.AnnualRate != nil {
		v, _ := strconv.ParseFloat(*in.AnnualRate, 64)
		sc.AnnualRate = &v
	}
	if in.ExtraPaymentAmount != nil {
		canon, _ := repo.CanonicalAmount(*in.ExtraPaymentAmount)
		v, _ := strconv.ParseFloat(canon, 64)
		sc.ExtraPaymentAmount = &v
	}
	if in.ExtraPaymentFrequency != nil {
		sc.ExtraPaymentFrequency = in.ExtraPaymentFrequency
	}
	if in.OneTimePrepayment != nil {
		canon, _ := repo.CanonicalAmount(*in.OneTimePrepayment.Amount)
		v, _ := strconv.ParseFloat(canon, 64)
		d, _ := time.Parse("2006-01-02", *in.OneTimePrepayment.Date)
		sc.OneTimePrepayment = &mortgage.Prepayment{Amount: v, Date: d}
	}
	if in.PaymentFrequency != nil {
		sc.PaymentFrequency = in.PaymentFrequency
	}
	if in.Renewal != nil {
		rate, _ := strconv.ParseFloat(*in.Renewal.AnnualRate, 64)
		d, _ := time.Parse("2006-01-02", *in.Renewal.Date)
		ren := &mortgage.Renewal{Date: d, AnnualRate: rate}
		if in.Renewal.RateType != nil {
			ren.RateType = *in.Renewal.RateType
		}
		sc.Renewal = ren
	}
	cmp, err := mortgage.WhatIf(spec, sc)
	if err != nil {
		if err.Error() == "one-time prepayment date is after the payoff date" {
			// The one engine runtime error with a 400 meaning (a valid
			// future date is allowed input; the engine decides payoff).
			writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
			return
		}
		slog.Error("mortgage operation failed", "op", "what_if", "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "mortgage operation failed")
		return
	}
	// The schedule's next_renewal is computed from the COMPOSED spec (the
	// simulated renewal replaces the stored events dated on/after it, so
	// the date moves); without term metadata it stays omitted.
	writeJSON(w, http.StatusOK, whatIfOut{
		PayoffDate:    cmp.PayoffDate.Format("2006-01-02"),
		TotalInterest: fmtAmt(cmp.TotalInterest),
		MonthsSaved:   cmp.MonthsSaved,
		InterestSaved: fmtAmt(cmp.InterestSaved), // sign preserved
		Schedule:      toScheduleOut(cmp.Schedule, mortgage.EffectiveSpec(spec, sc)),
	})
}

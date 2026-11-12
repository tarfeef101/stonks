package mortgage

import (
	"errors"
	"time"
)

// Prepayment is a one-time prepayment (WhatIf only).
type Prepayment struct {
	Amount float64   // > 0
	Date   time.Time // applied on the first payment dated >= this date
}

// Renewal is a what-if rate change / renewal: on Date the rate switches to
// AnnualRate (and the rate type to RateType when non-empty) and the regular
// payment is recomputed over the remaining amortization (a re_amortize
// RateEvent). Stored rate events dated >= Date are superseded by it.
type Renewal struct {
	Date       time.Time // > spec.StartDate
	AnnualRate float64   // percent
	RateType   string    // "fixed" | "variable" ("" = unchanged); metadata only
}

// Scenario overrides Spec fields for a what-if comparison; nil fields keep
// the spec value.
type Scenario struct {
	AnnualRate            *float64    // percent; nil = spec value
	ExtraPaymentAmount    *float64    // nil = spec value
	ExtraPaymentFrequency *string     // nil = spec value
	OneTimePrepayment     *Prepayment // nil = none
	PaymentFrequency      *string     // nil = spec value
	Renewal               *Renewal    // nil = none
}

// Comparison carries EXACTLY the plan's five what-if fields
// {payoff_date, total_interest, months_saved, interest_saved, schedule}.
type Comparison struct {
	PayoffDate    time.Time // scenario payoff (plan field payoff_date)
	TotalInterest float64   // scenario total interest, absolute (plan field total_interest)
	MonthsSaved   int       // base.PayoffIndex - scenario.PayoffIndex (in payment PERIODS; plan field months_saved)
	InterestSaved float64   // base.TotalInterest - scenario.TotalInterest (plan field interest_saved)
	Schedule      Schedule  // the scenario's full schedule (plan field schedule)
}

// WhatIf compares the scenario against the base schedule of spec. A rate
// change (AnnualRate) is applied FROM THE START: the regular payment is
// recomputed for the full original term at the new rate, then
// extras/lump sum play out. A Renewal is applied MID-TERM as a re_amortize
// rate event (the regular payment is recomputed over the remaining
// amortization at the event date), superseding stored rate events dated on
// or after the renewal. MonthsSaved and InterestSaved are computed from the
// ROUNDED exported totals (cent-exact by construction). The composed
// scenario spec is validated with the same rules as Generate, so scenario
// values (including the renewal's rate and date) must be valid.
func WhatIf(spec Spec, sc Scenario) (Comparison, error) {
	if err := validate(spec); err != nil {
		return Comparison{}, err
	}
	if sc.OneTimePrepayment != nil && sc.OneTimePrepayment.Amount <= 0 {
		return Comparison{}, errors.New("one-time prepayment amount must be greater than zero")
	}
	base, err := Generate(spec)
	if err != nil {
		return Comparison{}, err
	}
	eff := EffectiveSpec(spec, sc)
	// The composed scenario must itself be a valid Spec: overrides (a
	// Renewal becomes a rate event) are checked with the same rules as
	// Generate, so a bad renewal rate/date fails with the event errors.
	if err := validate(eff); err != nil {
		return Comparison{}, err
	}
	scen, err := runSim(eff, sc.OneTimePrepayment)
	if err != nil {
		return Comparison{}, err
	}
	return Comparison{
		PayoffDate:    scen.PayoffDate,
		TotalInterest: scen.TotalInterest,
		MonthsSaved:   base.PayoffIndex - scen.PayoffIndex,
		InterestSaved: base.TotalInterest - scen.TotalInterest,
		Schedule:      scen,
	}, nil
}

// EffectiveSpec applies the scenario overrides to spec and returns the
// spec the scenario's simulation runs on: each non-nil field replaces
// the spec value, a Renewal is composed into the rate events (via
// composeRenewal). OneTimePrepayment is not part of the spec (the
// simulation takes it separately). The stored spec's slices are never
// mutated. The server uses it for the what-if schedule's next_renewal,
// which must reflect the scenario (renewal composed in), not the stored
// spec.
func EffectiveSpec(spec Spec, sc Scenario) Spec {
	eff := spec
	if sc.AnnualRate != nil {
		eff.AnnualRate = *sc.AnnualRate
	}
	if sc.ExtraPaymentAmount != nil {
		eff.ExtraPaymentAmount = *sc.ExtraPaymentAmount
	}
	if sc.ExtraPaymentFrequency != nil {
		eff.ExtraPaymentFrequency = *sc.ExtraPaymentFrequency
	}
	if sc.PaymentFrequency != nil {
		eff.PaymentFrequency = *sc.PaymentFrequency
	}
	if sc.Renewal != nil {
		eff.RateEvents = composeRenewal(spec.RateEvents, *sc.Renewal)
	}
	return eff
}

// composeRenewal returns the stored rate events with everything dated on or
// after ren.Date dropped and the renewal appended as a re_amortize event.
// The stored slice is never mutated.
func composeRenewal(stored []RateEvent, ren Renewal) []RateEvent {
	out := make([]RateEvent, 0, len(stored)+1)
	for _, ev := range stored {
		if ev.Date.Before(ren.Date) {
			out = append(out, ev)
		}
	}
	out = append(out, RateEvent{
		Date:       ren.Date,
		AnnualRate: ren.AnnualRate,
		RateType:   ren.RateType,
		Action:     "re_amortize",
	})
	return out
}

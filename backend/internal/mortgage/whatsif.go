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

// Scenario overrides Spec fields for a what-if comparison; nil fields keep
// the spec value.
type Scenario struct {
	AnnualRate            *float64    // percent; nil = spec value
	ExtraPaymentAmount    *float64    // nil = spec value
	ExtraPaymentFrequency *string     // nil = spec value
	OneTimePrepayment     *Prepayment // nil = none
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
// change is applied FROM THE START: the regular payment is recomputed for
// the full original term at the new rate, then extras/lump sum play out.
// MonthsSaved and InterestSaved are computed from the ROUNDED exported
// totals (cent-exact by construction).
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

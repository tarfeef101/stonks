// Package mortgage implements the mortgage amortisation engine for
// finance-tracker (plan 7.2).
//
// The engine is pure Go (stdlib only) and defines its own numeric Spec; it
// does NOT import the repo package (the 7.3 API endpoint converts the stored
// spec strings to these numbers).
//
// Rates follow the Canadian convention: an annual nominal rate compounded
// semi-annually, so the per-payment-period rate is
//
//	periodic = (1 + annual/2)^(2/freq) - 1
//
// with annual as a FRACTION and freq the payments per year (monthly 12,
// semi_monthly 24, biweekly 26). All simulation math runs at full float64
// precision; round2 is applied only when writing exported monetary fields.
package mortgage

import (
	"errors"
	"math"
	"time"
)

// Spec -- engine input. AnnualRate is a PERCENT: 4.99 means 4.99%
// (mirrors the stored spec and the API validation 0 < v <= 100).
type Spec struct {
	Principal             float64 // > 0
	AnnualRate            float64 // percent, 0 <= v <= 100
	AmortizationYears     int     // 1..40
	StartDate             time.Time
	PaymentFrequency      string  // "monthly" | "semi_monthly" | "biweekly"
	ExtraPaymentAmount    float64 // >= 0; 0 = no recurring extra
	ExtraPaymentFrequency string  // "monthly" | "quarterly" | "yearly"
	ExtraPrepaymentType   string  // "term" | "principal"
	TermYears             int     // 0 = none (legacy spec); otherwise the renewable term length in years (metadata only)
	RateType              string  // "fixed" | "variable" ("" tolerated as fixed); metadata only
	RateEvents            []RateEvent
}

// RateEvent is a rate change or renewal applied to the simulation. An event
// takes effect on the first payment dated >= Date: the period rate switches
// to AnnualRate for that payment and every payment after it.
//
//	Action "re_amortize" (a standard renewal): the regular payment is
//	recomputed over the remaining original amortization INCLUDING the
//	event's own period, so the event payment is already the new
//	re-amortized amount and the payoff date is preserved.
//
//	Action "keep_payment": the regular payment is unchanged and the payoff
//	date floats — earlier for a rate cut, later for a rate hike (the
//	amortization extends past the original end date when needed).
//
// RateType is metadata only (a fixed/variable switch carries no math).
type RateEvent struct {
	Date       time.Time // > StartDate, strictly ascending within RateEvents
	AnnualRate float64   // percent, 0 < v <= 100
	RateType   string    // "fixed" | "variable" ("" = unchanged); metadata only
	Action     string    // "re_amortize" | "keep_payment"
}

// Payment is one row of an amortisation schedule.
type Payment struct {
	Index     int       // 1-based
	Date      time.Time // UTC midnight
	Payment   float64   // total paid this period (regular + extra, rounded 2dp)
	Interest  float64   // rounded 2dp
	Principal float64   // rounded 2dp
	Extra     float64   // extra portion this period (recurring and/or one-time), rounded 2dp
	Balance   float64   // balance remaining AFTER this payment, rounded 2dp
}

// Schedule is the full amortisation result.
type Schedule struct {
	BasePayment   float64   // initial regular payment (rounded 2dp)
	PayoffDate    time.Time // date of the final payment
	PayoffIndex   int       // number of payments made (== len(Payments))
	TotalPaid     float64   // rounded 2dp
	TotalInterest float64   // rounded 2dp
	Payments      []Payment
}

// EquityPoint is one point of the equity curve.
type EquityPoint struct {
	Date    time.Time
	Balance float64 // remaining balance (rounded 2dp)
	Equity  float64 // Principal - Balance (rounded 2dp)
}

// PeriodicRate returns the per-payment-period rate for Canadian
// semi-annual compounding: periodic = (1 + annual/2)^(2/freq) - 1
// with annual as a FRACTION. annualPercent is the PERCENT form, so the
// body is: math.Pow(1+annualPercent/200, 2.0/paymentsPerYear) - 1.
func PeriodicRate(annualPercent float64, paymentsPerYear int) float64 {
	return math.Pow(1+annualPercent/200, 2.0/float64(paymentsPerYear)) - 1
}

// Generate builds the base schedule for spec (its own recurring extra, no
// one-time prepayment).
func Generate(spec Spec) (Schedule, error) {
	return runSim(spec, nil)
}

// EquityCurve returns one equity point per payment of s. Equity_k is the
// round2 of the cumulative principal paid through payment k, so the curve is
// a pure transformation of the Schedule (cent-equivalent to
// principal-minus-balance).
func EquityCurve(s Schedule) []EquityPoint {
	pts := make([]EquityPoint, len(s.Payments))
	var equity float64
	for i, p := range s.Payments {
		equity += p.Principal
		pts[i] = EquityPoint{Date: p.Date, Balance: p.Balance, Equity: round2(equity)}
	}
	return pts
}

// round2 rounds to the nearest cent (half away from zero).
func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

func paymentsPerYear(freq string) int {
	switch freq {
	case "monthly":
		return 12
	case "semi_monthly":
		return 24
	case "biweekly":
		return 26
	}
	return 0
}

// stepDate advances one payment period from d.
func stepDate(d time.Time, freq string) time.Time {
	switch freq {
	case "monthly":
		return d.AddDate(0, 1, 0)
	case "semi_monthly":
		return d.AddDate(0, 0, 15)
	case "biweekly":
		return d.AddDate(0, 0, 14)
	}
	return d
}

// monthsSince is the whole-month difference between a and b.
func monthsSince(a, b time.Time) int {
	return (b.Year()-a.Year())*12 + (int(b.Month()) - int(a.Month()))
}

// periodMonths maps a recurring-extra frequency to its calendar bucket size.
func periodMonths(freq string) int {
	switch freq {
	case "monthly":
		return 1
	case "quarterly":
		return 3
	case "yearly":
		return 12
	}
	return 1
}

// amortize is the closed-form regular payment over n remaining periods.
func amortize(bal, r float64, n int) float64 {
	if n <= 0 {
		return 0
	}
	if r == 0 {
		return bal / float64(n)
	}
	return bal * r / (1 - math.Pow(1+r, -float64(n)))
}

// validate returns the first failing check, in the pinned order.
func validate(spec Spec) error {
	if spec.Principal <= 0 {
		return errors.New("principal must be greater than zero")
	}
	if math.IsNaN(spec.AnnualRate) || spec.AnnualRate < 0 || spec.AnnualRate > 100 {
		return errors.New("annual rate must be between 0 and 100")
	}
	if spec.AmortizationYears < 1 || spec.AmortizationYears > 40 {
		return errors.New("amortization years must be between 1 and 40")
	}
	if paymentsPerYear(spec.PaymentFrequency) == 0 {
		return errors.New("payment frequency must be monthly, semi_monthly, or biweekly")
	}
	if spec.ExtraPaymentAmount < 0 {
		return errors.New("extra payment amount must not be negative")
	}
	if spec.ExtraPaymentFrequency != "monthly" && spec.ExtraPaymentFrequency != "quarterly" && spec.ExtraPaymentFrequency != "yearly" {
		return errors.New("extra payment frequency must be monthly, quarterly, or yearly")
	}
	if spec.ExtraPrepaymentType != "term" && spec.ExtraPrepaymentType != "principal" {
		return errors.New("extra prepayment type must be term or principal")
	}
	if spec.TermYears < 0 || spec.TermYears > 40 {
		return errors.New("term years must be between 0 and 40")
	}
	if spec.RateType != "" && spec.RateType != "fixed" && spec.RateType != "variable" {
		return errors.New("rate type must be fixed or variable")
	}
	for i, ev := range spec.RateEvents {
		if !ev.Date.After(spec.StartDate) {
			return errors.New("rate event date must be after the start date")
		}
		if math.IsNaN(ev.AnnualRate) || ev.AnnualRate <= 0 || ev.AnnualRate > 100 {
			return errors.New("rate event annual rate must be between 0 and 100")
		}
		if ev.RateType != "" && ev.RateType != "fixed" && ev.RateType != "variable" {
			return errors.New("rate event rate type must be fixed or variable")
		}
		if ev.Action != "re_amortize" && ev.Action != "keep_payment" {
			return errors.New("rate event action must be re_amortize or keep_payment")
		}
		if i > 0 && !ev.Date.After(spec.RateEvents[i-1].Date) {
			return errors.New("rate event dates must be strictly ascending")
		}
	}
	return nil
}

// runSim is the simulation core shared by Generate and WhatIf. one is the
// optional one-time prepayment (WhatIf only); nil means none.
func runSim(spec Spec, one *Prepayment) (Schedule, error) {
	if err := validate(spec); err != nil {
		return Schedule{}, err
	}
	ppy := paymentsPerYear(spec.PaymentFrequency)
	n := spec.AmortizationYears * ppy
	start := time.Date(spec.StartDate.Year(), spec.StartDate.Month(), spec.StartDate.Day(), 0, 0, 0, 0, time.UTC)
	r := PeriodicRate(spec.AnnualRate, ppy)
	pay := amortize(spec.Principal, r, n) // initial regular payment, full precision
	// BasePayment reports THIS initial payment. In principal mode `pay` is
	// re-amortized after every extra, so the loop-end value is the final
	// (smaller) payment — capturing the start value keeps the field's
	// documented meaning (round 22: the seed data exposed it; a
	// principal-mode schedule was reporting its last partial payment as
	// the base payment).
	basePay := pay

	bal := spec.Principal
	pm := periodMonths(spec.ExtraPaymentFrequency)
	prevBucket := -1 // bucket before the first payment, so the first payment qualifies
	var (
		payments   []Payment
		totalPaid  float64
		totalInt   float64
		oneApplied bool
	)
	cap := n // amortization horizon; keep_payment rate hikes extend it
	evIdx := 0
	pendingAction := ""
	d := start
	for k := 1; ; k++ {
		if k > 1 {
			d = stepDate(d, spec.PaymentFrequency)
		}
		// Rate events take effect on the first payment dated >= the event
		// date: the period rate switches BEFORE this period's interest is
		// accrued, so the event's own payment is already at the new rate.
		// A re_amortize renewal goes further: the pre-payment balance is
		// re-amortized over the remaining periods INCLUDING this one, so
		// the event's own payment is the new re-amortized amount (the
		// matching solver solves exactly this model). keep_payment is
		// applied after the payment, on the new balance. At most one event
		// can fire per period (dates are strictly ascending and no two
		// payments share a date).
		if evIdx < len(spec.RateEvents) && !d.Before(spec.RateEvents[evIdx].Date) {
			ev := spec.RateEvents[evIdx]
			r = PeriodicRate(ev.AnnualRate, ppy)
			if ev.Action == "re_amortize" {
				if m := n - k + 1; m > 0 {
					pay = amortize(bal, r, m) // the renewal preserves the original payoff date
					cap = n
				}
				// m <= 0 (an event past the original end, only reachable
				// after a keep_payment extension): keep the current
				// payment/horizon; the rate switch still stands.
			} else {
				pendingAction = ev.Action
			}
			evIdx++
		}
		interest := bal * r
		// Recurring extra: due when the payment lands in a new calendar
		// bucket relative to the previous payment.
		extraDue := 0.0
		bucket := monthsSince(start, d) / pm
		if spec.ExtraPaymentAmount > 0 && bucket > prevBucket {
			extraDue = spec.ExtraPaymentAmount
		}
		prevBucket = bucket
		// One-time prepayment: applied in full on the first payment dated
		// >= its date.
		if one != nil && !oneApplied && !d.Before(one.Date) {
			extraDue += one.Amount
			oneApplied = true
		}
		final := k == cap || bal+interest <= pay+extraDue
		if final {
			// The extra is applied to the balance first and trimmed if it
			// would overshoot; the regular portion then pays off the rest.
			// totalOut is therefore capped at bal+interest.
			extraApplied := extraDue
			if extraApplied > bal {
				extraApplied = bal
			}
			totalOut := (bal - extraApplied) + interest
			rec := Payment{
				Index:     k,
				Date:      d,
				Payment:   round2(totalOut),
				Interest:  round2(interest),
				Principal: round2(totalOut - interest),
				Extra:     round2(extraApplied),
				Balance:   0,
			}
			payments = append(payments, rec)
			totalPaid += totalOut
			totalInt += interest
			if one != nil && !oneApplied {
				return Schedule{}, errors.New("one-time prepayment date is after the payoff date")
			}
			return Schedule{
				BasePayment:   round2(basePay),
				PayoffDate:    d,
				PayoffIndex:   k,
				TotalPaid:     round2(totalPaid),
				TotalInterest: round2(totalInt),
				Payments:      payments,
			}, nil
		}
		totalOut := pay + extraDue
		bal = bal + interest - totalOut
		if spec.ExtraPrepaymentType == "principal" && extraDue > 0 {
			pay = amortize(bal, r, cap-k) // re-amortize the remainder over the remaining term
		}
		// A keep_payment event's action lands AFTER this payment, on the
		// new balance (same idiom as the principal-mode re-amortization).
		// re_amortize was already applied at the firing point above,
		// before this payment.
		if pendingAction == "keep_payment" {
			var m int
			if r == 0 {
				m = int(math.Ceil(bal / pay))
			} else if pay <= bal*r {
				return Schedule{}, errors.New("the payment cannot cover the interest after the rate change")
			} else {
				m = int(math.Ceil(-math.Log(1-bal*r/pay) / math.Log(1+r)))
			}
			if m > 4800 {
				return Schedule{}, errors.New("the payment cannot cover the interest after the rate change")
			}
			if k+m > cap {
				cap = k + m
			}
		}
		pendingAction = ""
		payments = append(payments, Payment{
			Index:     k,
			Date:      d,
			Payment:   round2(totalOut),
			Interest:  round2(interest),
			Principal: round2(totalOut - interest),
			Extra:     round2(extraDue),
			Balance:   round2(bal),
		})
		totalPaid += totalOut
		totalInt += interest
	}
}

// NextRenewalDate returns the date the current term expires: start +
// TermYears, or — when the spec carries re_amortize renewal events — the
// LAST such event's date + TermYears (each renewal starts a new term).
// keep_payment events do not move it. ok is false when TermYears is zero
// (no term metadata, e.g. a legacy spec).
func NextRenewalDate(spec Spec) (time.Time, bool) {
	if spec.TermYears <= 0 {
		return time.Time{}, false
	}
	base := spec.StartDate
	for i := len(spec.RateEvents) - 1; i >= 0; i-- {
		if spec.RateEvents[i].Action == "re_amortize" {
			base = spec.RateEvents[i].Date
			break
		}
	}
	return time.Date(base.Year()+spec.TermYears, base.Month(), base.Day(), 0, 0, 0, 0, time.UTC), true
}

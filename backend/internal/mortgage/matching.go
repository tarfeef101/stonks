// Matching (plan 7.3, round 25): replay an account's debit transactions
// against the stored schedule and report, read-only, how the actual
// payments diverge from the plan. Nothing here mutates the spec: the
// output feeds the suggest-and-confirm panel, and confirmations are
// applied through the regular PATCH rate_events path.
//
// For each plan date d, P(d) is the sum of the debits matched to d (each
// debit pairs with the nearest plan date within matchWindow) and W(d) is
// the stored plan's total payment for that date (regular + recurring
// extra):
//
//	delta = P(d) - W(d)
//
//	|delta| <= 1.00          -> normal
//	delta >  1.00, isolated  -> extra
//	delta < -1.00, isolated  -> short
//
// A sustained run (>= 4 consecutive same-sign deviation periods whose
// delta spread stays within 1.00 plus the plan's own re-amortization
// drift) is a rate-change candidate, solved for an implied annual rate A
// by bisection on
//
//	amortize(B*, PeriodicRate(A, ppy), N*) = Pbar_reg
//
// with B* the actual balance before the run, N* the stored plan's
// remaining periods from the run start, and Pbar_reg the mean of the
// observed regular payments (P minus the planned recurring extra) over
// the run. A candidate becomes a suggestion only when the implied rate is
// at least matchMinShift away from the rate in effect at the run start;
// otherwise the periods fall through to extras/shorts.
//
// Flags: no_transactions (no debits at all), payment_below_minimum_payoff
// (a sustained run whose payment is below the zero-rate amortization),
// payment_above_maximum_rate (a sustained run above the 100%-rate
// amortization).

package mortgage

import (
	"math"
	"sort"
	"time"
)

// Txn is one account transaction as consumed by the matcher. Amount is
// signed: a debit (money out of the account, i.e. a mortgage payment) is
// negative; non-debits are ignored.
type Txn struct {
	Date   time.Time
	Amount float64
}

// MatchAmount is one amount reported outside the period table: a
// non-sustained over/under payment, a debit off the plan grid, or a debit
// after the actual payoff. Amount is the positive magnitude.
type MatchAmount struct {
	Date   time.Time
	Amount float64 // round2, > 0
}

// MatchSuggestion is a rate-change candidate derived from a sustained
// deviation run.
type MatchSuggestion struct {
	EffectiveDate     time.Time // date of the run's first period
	ImpliedAnnualRate float64   // percent, round2
	ObservedPayment   float64   // mean observed regular payment over the run, round2
	ObservedPeriods   int
}

// MatchPeriod is one row of the period table: one plan date, from the
// first plan date to the last matched plan date.
type MatchPeriod struct {
	Date           time.Time
	Payment        *float64 // nil = no debit matched this plan date
	Extra          *float64 // magnitude when delta > +matchTol
	Short          *float64 // magnitude when delta < -matchTol
	Balance        float64  // actual balance after the period, round2
	PlannedBalance float64  // stored plan balance after the period, round2
}

// MatchResult is the full read-only matching report.
type MatchResult struct {
	ScheduledPayment   float64  // stored plan base payment, round2
	PlannedPayoffDate  time.Time
	PlannedPayoffIndex int
	ActualPayoffDate   *time.Time
	LastMatchedDate    *time.Time
	ActualBalance      float64 // actual balance after the last row, round2 (0 after payoff)
	PlannedBalance     float64 // stored plan balance at the last row, round2 (principal when nothing matched)
	PeriodsMatched     int
	PeriodsMissing     int
	Extras             []MatchAmount
	Shorts             []MatchAmount
	Suggestions        []MatchSuggestion
	Flags              []string
	Periods            []MatchPeriod
}

const (
	matchWindow     = 72 * time.Hour // debit pairs with a plan date within this distance
	matchTol        = 1.00           // dollars; |delta| <= this is normal
	matchMinRun     = 4              // consecutive periods for a sustained run
	matchMinShift   = 0.25           // percentage points of implied shift needed for a suggestion
	matchFlagEps    = 0.005          // dollars; half a cent, feasibility checks at 0% and 100%
	matchIterations = 200            // bisection iterations
)

// Match replays txns against the schedule generated from spec and returns
// the divergence report. Txns with Amount >= 0 are ignored. The actual
// balance mirrors the engine's arithmetic at full precision (per-period
// compounding at the stored rate in effect): bal = bal + bal*r - paid,
// round2 only when recorded.
func Match(spec Spec, txns []Txn) (MatchResult, error) {
	sched, err := Generate(spec)
	if err != nil {
		return MatchResult{}, err
	}
	ppy := paymentsPerYear(spec.PaymentFrequency)
	start := time.Date(spec.StartDate.Year(), spec.StartDate.Month(), spec.StartDate.Day(), 0, 0, 0, 0, time.UTC)
	pm := periodMonths(spec.ExtraPaymentFrequency)

	// plannedExtra mirrors runSim's bucket idiom: the recurring extra is
	// due when a payment lands in a new calendar bucket relative to the
	// previous payment; the first payment always qualifies (prevBucket
	// starts at -1).
	plannedExtra := make([]float64, len(sched.Payments))
	prevBucket := -1
	for i, p := range sched.Payments {
		bucket := monthsSince(start, p.Date) / pm
		if spec.ExtraPaymentAmount > 0 && bucket > prevBucket {
			plannedExtra[i] = spec.ExtraPaymentAmount
		}
		prevBucket = bucket
	}

	debts := make([]Txn, 0, len(txns))
	for _, t := range txns {
		if t.Amount < 0 {
			debts = append(debts, t)
		}
	}
	sort.Slice(debts, func(a, b int) bool { return debts[a].Date.Before(debts[b].Date) })

	type row struct {
		debts     []Txn
		matched   bool
		p         float64 // sum of matched debit magnitudes
		delta     *float64
		balBefore float64
		balAfter  float64
		ratePct   float64 // stored rate in effect for this period (percent)
	}
	rows := make([]row, len(sched.Payments))

	extras := make([]MatchAmount, 0)
	shorts := make([]MatchAmount, 0)

	// Pair each debit with the nearest plan date within matchWindow;
	// anything else is an off-grid extra. Plan dates are >= 14 days apart,
	// so at most one date falls inside the window.
	for _, t := range debts {
		i := sort.Search(len(sched.Payments), func(j int) bool { return !sched.Payments[j].Date.Before(t.Date) })
		best := -1
		if i < len(sched.Payments) {
			best = i
		}
		if i > 0 && (best == -1 || sched.Payments[i-1].Date.Sub(t.Date).Abs() < sched.Payments[best].Date.Sub(t.Date).Abs()) {
			best = i - 1
		}
		if best == -1 || sched.Payments[best].Date.Sub(t.Date).Abs() > matchWindow {
			extras = append(extras, MatchAmount{Date: t.Date, Amount: round2(-t.Amount)})
			continue
		}
		rows[best].debts = append(rows[best].debts, t)
	}

	// maxMatched bounds the simulation horizon: periods after the last
	// matched debit are not simulated (no phantom interest accrual).
	maxMatched := -1
	for i := range rows {
		if len(rows[i].debts) > 0 {
			maxMatched = i
		}
	}

	// Balance mirror + per-period deltas, over rows 0..maxMatched.
	bal := spec.Principal
	payoffRow := -1
	evIdx := 0
	for i := 0; maxMatched >= 0 && i <= maxMatched; i++ {
		d := sched.Payments[i].Date
		for evIdx < len(spec.RateEvents) && !spec.RateEvents[evIdx].Date.After(d) {
			evIdx++
		}
		ratePct := spec.AnnualRate
		if evIdx > 0 {
			ratePct = spec.RateEvents[evIdx-1].AnnualRate
		}
		rows[i].ratePct = ratePct
		r := PeriodicRate(ratePct, ppy)
		rows[i].balBefore = bal
		accrued := bal * r
		if len(rows[i].debts) > 0 {
			if payoffRow == -1 {
				var paid float64
				for _, t := range rows[i].debts {
					paid += -t.Amount
				}
				rows[i].p = paid
				rows[i].matched = true
				if paid >= bal+accrued {
					bal = 0
					payoffRow = i
				} else {
					bal = bal + accrued - paid
				}
			} else {
				for _, t := range rows[i].debts {
					extras = append(extras, MatchAmount{Date: t.Date, Amount: round2(-t.Amount)})
				}
			}
		} else {
			bal = bal + accrued
		}
		rows[i].balAfter = bal
		if rows[i].matched {
			delta := rows[i].p - sched.Payments[i].Payment
			rows[i].delta = &delta
		}
	}

	lastRow := maxMatched
	if payoffRow >= 0 {
		lastRow = payoffRow
	}

	res := MatchResult{
		ScheduledPayment:   sched.BasePayment,
		PlannedPayoffDate:  sched.PayoffDate,
		PlannedPayoffIndex: sched.PayoffIndex,
		ActualBalance:      round2(bal),
		Extras:             extras,
		Shorts:             shorts,
		Suggestions:        make([]MatchSuggestion, 0),
		Flags:              make([]string, 0),
		Periods:            make([]MatchPeriod, 0),
	}
	if len(debts) == 0 {
		res.Flags = append(res.Flags, "no_transactions")
		res.PlannedBalance = round2(spec.Principal)
		return res, nil
	}
	if payoffRow >= 0 {
		d := sched.Payments[payoffRow].Date
		res.ActualPayoffDate = &d
	}
	if lastRow < 0 {
		res.PlannedBalance = round2(spec.Principal)
		return res, nil
	}

	res.PlannedBalance = round2(sched.Payments[lastRow].Balance)

	// Period table: plan dates 0..lastRow.
	periods := make([]MatchPeriod, 0, lastRow+1)
	for k := 0; k <= lastRow; k++ {
		mp := MatchPeriod{
			Date:           sched.Payments[k].Date,
			Balance:        round2(rows[k].balAfter),
			PlannedBalance: round2(sched.Payments[k].Balance),
		}
		if rows[k].matched {
			res.PeriodsMatched++
			d := sched.Payments[k].Date
			res.LastMatchedDate = &d
			p := round2(rows[k].p)
			mp.Payment = &p
		} else {
			res.PeriodsMissing++
		}
		if rows[k].delta != nil {
			if *rows[k].delta > matchTol {
				e := round2(*rows[k].delta)
				mp.Extra = &e
			} else if *rows[k].delta < -matchTol {
				s := round2(-*rows[k].delta)
				mp.Short = &s
			}
		}
		periods = append(periods, mp)
	}
	res.Periods = periods

	// Deviation runs.
	addFlag := func(f string) {
		for _, g := range res.Flags {
			if g == f {
				return
			}
		}
		res.Flags = append(res.Flags, f)
	}
	fallThrough := func(i, j int) {
		for k := i; k <= j; k++ {
			if *rows[k].delta > 0 {
				extras = append(extras, MatchAmount{Date: sched.Payments[k].Date, Amount: round2(*rows[k].delta)})
			} else {
				shorts = append(shorts, MatchAmount{Date: sched.Payments[k].Date, Amount: round2(-*rows[k].delta)})
			}
		}
		res.Extras = extras
		res.Shorts = shorts
	}
	dev := func(k int) (int, bool) {
		if rows[k].delta == nil || math.Abs(*rows[k].delta) <= matchTol {
			return 0, false
		}
		if *rows[k].delta > 0 {
			return 1, true
		}
		return -1, true
	}
	for i := 0; i < len(rows); {
		s, ok := dev(i)
		if !ok {
			i++
			continue
		}
		j := i
		for j+1 < len(rows) {
			sj, okj := dev(j + 1)
			if !okj || sj != s {
				break
			}
			j++
		}
		if j-i+1 >= matchMinRun {
			// Sustained candidate: check the delta spread against the
			// plan's own regular-payment drift over the run (principal
			// mode re-amortizes after every extra, so W itself is not
			// flat).
			var minD, maxD, minW, maxW float64
			for k := i; k <= j; k++ {
				d := *rows[k].delta
				if k == i {
					minD, maxD = d, d
				} else {
					if d < minD {
						minD = d
					}
					if d > maxD {
						maxD = d
					}
				}
				w := sched.Payments[k].Payment - plannedExtra[k]
				if k == i {
					minW, maxW = w, w
				} else {
					if w < minW {
						minW = w
					}
					if w > maxW {
						maxW = w
					}
				}
			}
			sustained := maxD-minD <= matchTol+(maxW-minW)
			if sustained {
				n := sched.PayoffIndex - i // remaining periods from the run's first row, inclusive
				var pbar float64
				for k := i; k <= j; k++ {
					pbar += rows[k].p - plannedExtra[k]
				}
				pbar /= float64(j - i + 1)
				a, found, flag := impliedRate(rows[i].balBefore, pbar, ppy, n)
				switch {
				case !found:
					addFlag(flag)
					fallThrough(i, j)
				case math.Abs(a-rows[i].ratePct) < matchMinShift:
					fallThrough(i, j)
				default:
					res.Suggestions = append(res.Suggestions, MatchSuggestion{
						EffectiveDate:     sched.Payments[i].Date,
						ImpliedAnnualRate: round2(a),
						ObservedPayment:   round2(pbar),
						ObservedPeriods:   j - i + 1,
					})
				}
			} else {
				fallThrough(i, j)
			}
		} else {
			fallThrough(i, j)
		}
		i = j + 1
	}
	return res, nil
}

// impliedRate solves amortize(B, PeriodicRate(A, ppy), n) = pbar for A in
// [0, 100] by bisection (f is monotone increasing in A). ok is false with
// a flag when no rate in range fits: the payment is below the zero-rate
// amortization or above the 100%-rate amortization.
func impliedRate(bal, pbar float64, ppy, n int) (a float64, ok bool, flag string) {
	f := func(x float64) float64 { return amortize(bal, PeriodicRate(x, ppy), n) - pbar }
	if f(0) > matchFlagEps {
		return 0, false, "payment_below_minimum_payoff"
	}
	if f(100) < -matchFlagEps {
		return 0, false, "payment_above_maximum_rate"
	}
	lo, hi := 0.0, 100.0
	for k := 0; k < matchIterations; k++ {
		mid := (lo + hi) / 2
		if f(mid) > 0 {
			hi = mid
		} else {
			lo = mid
		}
	}
	return (lo + hi) / 2, true, ""
}

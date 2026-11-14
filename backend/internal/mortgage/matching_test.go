package mortgage

import (
	"math"
	"testing"
	"time"
)

// Round 25 matching fixtures. specA mirrors the live "Home Mortgage"
// minus the recurring extra; specB adds the 200/month principal-mode
// extra, so its schedule matches the live plan exactly (the literals in
// TestMatchFollowPlanWithExtras are the live 2026 payment grid).
func matchSpecA() Spec {
	return Spec{
		Principal:             485000,
		AnnualRate:            4.39,
		AmortizationYears:     25,
		StartDate:             time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC),
		PaymentFrequency:      "semi_monthly",
		ExtraPaymentFrequency: "monthly",
		ExtraPrepaymentType:   "principal",
	}
}

func matchSpecB() Spec {
	s := matchSpecA()
	s.ExtraPaymentAmount = 200
	return s
}

// gridTxns builds one debit per plan period from..to (1-based).
func gridTxns(spec Spec, from, to int, pay func(i int) float64) []Txn {
	s, err := Generate(spec)
	if err != nil {
		panic(err)
	}
	out := make([]Txn, 0, to-from+1)
	for i := from; i <= to; i++ {
		out = append(out, Txn{Date: s.Payments[i-1].Date, Amount: -pay(i)})
	}
	return out
}

func flagHas(flags []string, want string) bool {
	for _, f := range flags {
		if f == want {
			return true
		}
	}
	return false
}

// T1: no debits at all (credits are ignored) -> no_transactions.
func TestMatchNoTransactions(t *testing.T) {
	res, err := Match(matchSpecA(), []Txn{{Date: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), Amount: 2500}})
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if len(res.Flags) != 1 || res.Flags[0] != "no_transactions" {
		t.Fatalf("flags = %v", res.Flags)
	}
	if len(res.Periods) != 0 || res.PeriodsMatched != 0 || res.PeriodsMissing != 0 {
		t.Fatalf("periods = %d matched = %d missing = %d", len(res.Periods), res.PeriodsMatched, res.PeriodsMissing)
	}
	if res.ActualBalance != 485000 || res.PlannedBalance != 485000 {
		t.Fatalf("balances actual = %v planned = %v", res.ActualBalance, res.PlannedBalance)
	}
	if res.LastMatchedDate != nil || res.ActualPayoffDate != nil {
		t.Fatalf("dates set: %v %v", res.LastMatchedDate, res.ActualPayoffDate)
	}
	if res.ScheduledPayment != 1326.18 || res.PlannedPayoffIndex != 600 {
		t.Fatalf("base = %v payoffIndex = %d", res.ScheduledPayment, res.PlannedPayoffIndex)
	}
	if len(res.Extras) != 0 || len(res.Shorts) != 0 || len(res.Suggestions) != 0 {
		t.Fatalf("extras/shorts/suggestions = %v %v %v", res.Extras, res.Shorts, res.Suggestions)
	}
}

// T2: following the plan exactly -> no deviations of any kind.
func TestMatchFollowPlan(t *testing.T) {
	spec := matchSpecA()
	s, err := Generate(spec)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	res, err := Match(spec, gridTxns(spec, 1, 20, func(int) float64 { return s.Payments[0].Payment }))
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if len(res.Flags) != 0 || len(res.Extras) != 0 || len(res.Shorts) != 0 || len(res.Suggestions) != 0 {
		t.Fatalf("deviations: flags = %v extras = %v shorts = %v suggestions = %v", res.Flags, res.Extras, res.Shorts, res.Suggestions)
	}
	if res.PeriodsMatched != 20 || res.PeriodsMissing != 0 || len(res.Periods) != 20 {
		t.Fatalf("matched = %d missing = %d rows = %d", res.PeriodsMatched, res.PeriodsMissing, len(res.Periods))
	}
	// The plan rows pay the full-precision base while the actuals pay the
	// rounded 1326.18, so the balances track within a few cents.
	if res.ActualBalance != 475887.49 || res.PlannedBalance != 475887.46 {
		t.Fatalf("balances actual = %v planned = %v", res.ActualBalance, res.PlannedBalance)
	}
	if got := res.LastMatchedDate; got == nil || !got.Equal(s.Payments[19].Date) {
		t.Fatalf("lastMatched = %v", got)
	}
	for i, p := range res.Periods {
		if p.Payment == nil || p.Extra != nil || p.Short != nil || math.Abs(p.Balance-p.PlannedBalance) > 0.05 {
			t.Fatalf("row %d: %+v", i, p)
		}
	}
}

// T3: a +136.82 sustained run (10 periods, plan flat in specA) is a
// rate-change candidate: one suggestion at 5.40, no flags, no extras.
func TestMatchDriftSuggestion(t *testing.T) {
	spec := matchSpecA()
	s, err := Generate(spec)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	txns := gridTxns(spec, 1, 20, func(i int) float64 {
		if i >= 11 {
			return 1463.00
		}
		return s.Payments[i-1].Payment
	})
	res, err := Match(spec, txns)
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if len(res.Suggestions) != 1 {
		t.Fatalf("suggestions = %v", res.Suggestions)
	}
	got := res.Suggestions[0]
	if !got.EffectiveDate.Equal(s.Payments[10].Date) { // 2026-06-14
		t.Fatalf("effectiveDate = %v", got.EffectiveDate)
	}
	if got.ImpliedAnnualRate != 5.4 {
		t.Fatalf("implied = %v", got.ImpliedAnnualRate)
	}
	if got.ObservedPayment != 1463.00 || got.ObservedPeriods != 10 {
		t.Fatalf("observed = %v x %d", got.ObservedPayment, got.ObservedPeriods)
	}
	if len(res.Flags) != 0 || len(res.Extras) != 0 || len(res.Shorts) != 0 {
		t.Fatalf("flags = %v extras = %v shorts = %v", res.Flags, res.Extras, res.Shorts)
	}
	if res.PeriodsMatched != 20 || res.PeriodsMissing != 0 {
		t.Fatalf("matched = %d missing = %d", res.PeriodsMatched, res.PeriodsMissing)
	}
	if res.ActualPayoffDate != nil {
		t.Fatalf("payoff = %v", *res.ActualPayoffDate)
	}
	if res.ActualBalance >= res.PlannedBalance {
		t.Fatalf("actual %v should be below planned %v", res.ActualBalance, res.PlannedBalance)
	}
	// Every run row carries the delta; the first is +136.82.
	if e := res.Periods[10].Extra; e == nil || *e != 136.82 {
		t.Fatalf("row 11 extra = %v", e)
	}
	if e := res.Periods[19].Extra; e == nil || *e != 136.82 {
		t.Fatalf("row 20 extra = %v", e)
	}
	if res.Periods[9].Extra != nil {
		t.Fatalf("row 10 should be normal")
	}
}

// T4: with the 200/month principal-mode extra, following the LIVE grid
// (re-amortized regular + extra on bucket dates) must classify as
// perfectly normal — the plannedExtra bucket mirror must match runSim.
func TestMatchFollowPlanWithExtras(t *testing.T) {
	spec := matchSpecB()
	live := []float64{
		1526.18, 1325.63, 1525.63, 1525.09, 1324.54, 1324.54,
		1524.54, 1323.99, 1523.99, 1323.43, 1523.43, 1322.88,
		1522.88, 1322.33, 1522.33, 1321.77, 1521.77, 1321.22,
		1521.22, 1320.66,
	}
	s, err := Generate(spec)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for i, w := range live {
		if s.Payments[i].Payment != w {
			t.Fatalf("fixture drift: period %d plan = %v live = %v", i+1, s.Payments[i].Payment, w)
		}
	}
	txns := gridTxns(spec, 1, 20, func(i int) float64 { return live[i-1] })
	res, err := Match(spec, txns)
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if len(res.Flags) != 0 || len(res.Extras) != 0 || len(res.Shorts) != 0 || len(res.Suggestions) != 0 {
		t.Fatalf("deviations: flags = %v extras = %v shorts = %v suggestions = %v", res.Flags, res.Extras, res.Shorts, res.Suggestions)
	}
	if res.PeriodsMatched != 20 || res.PeriodsMissing != 0 {
		t.Fatalf("matched = %d missing = %d", res.PeriodsMatched, res.PeriodsMissing)
	}
	if res.ScheduledPayment != 1326.18 {
		t.Fatalf("base = %v", res.ScheduledPayment)
	}
	if res.ActualBalance != 473906.68 || res.PlannedBalance != 473906.67 {
		t.Fatalf("balances actual = %v planned = %v", res.ActualBalance, res.PlannedBalance)
	}
}

// T5: one isolated overpayment -> a single extra, no suggestion.
func TestMatchIsolatedExtra(t *testing.T) {
	spec := matchSpecA()
	s, err := Generate(spec)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	txns := gridTxns(spec, 1, 20, func(i int) float64 {
		if i == 6 {
			return s.Payments[5].Payment + 5000
		}
		return s.Payments[i-1].Payment
	})
	res, err := Match(spec, txns)
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if len(res.Extras) != 1 {
		t.Fatalf("extras = %v", res.Extras)
	}
	e := res.Extras[0]
	if !e.Date.Equal(s.Payments[5].Date) || e.Amount != 5000.00 {
		t.Fatalf("extra = %+v", e)
	}
	if len(res.Shorts) != 0 || len(res.Suggestions) != 0 || len(res.Flags) != 0 {
		t.Fatalf("shorts = %v suggestions = %v flags = %v", res.Shorts, res.Suggestions, res.Flags)
	}
	if ex := res.Periods[5].Extra; ex == nil || *ex != 5000.00 {
		t.Fatalf("row extra = %v", ex)
	}
	if res.ActualBalance >= res.PlannedBalance {
		t.Fatalf("actual %v should be below planned %v", res.ActualBalance, res.PlannedBalance)
	}
}

// T6: a debit more than 72h from every plan date -> off-cycle extra,
// plan periods untouched.
func TestMatchOffGridExtra(t *testing.T) {
	spec := matchSpecA()
	s, err := Generate(spec)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	txns := gridTxns(spec, 1, 20, func(i int) float64 { return s.Payments[i-1].Payment })
	txns = append(txns, Txn{Date: time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC), Amount: -2000})
	res, err := Match(spec, txns)
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if len(res.Extras) != 1 {
		t.Fatalf("extras = %v", res.Extras)
	}
	if !res.Extras[0].Date.Equal(time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC)) || res.Extras[0].Amount != 2000.00 {
		t.Fatalf("extra = %+v", res.Extras[0])
	}
	if res.PeriodsMatched != 20 || res.PeriodsMissing != 0 || len(res.Periods) != 20 {
		t.Fatalf("matched = %d missing = %d rows = %d", res.PeriodsMatched, res.PeriodsMissing, len(res.Periods))
	}
	if len(res.Shorts) != 0 || len(res.Suggestions) != 0 || len(res.Flags) != 0 {
		t.Fatalf("shorts = %v suggestions = %v flags = %v", res.Shorts, res.Suggestions, res.Flags)
	}
}

// T7: one short period (26.18 under) -> a single short.
func TestMatchShort(t *testing.T) {
	spec := matchSpecA()
	s, err := Generate(spec)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	txns := gridTxns(spec, 1, 20, func(i int) float64 {
		if i == 4 {
			return 1300.00
		}
		return s.Payments[i-1].Payment
	})
	res, err := Match(spec, txns)
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if len(res.Shorts) != 1 {
		t.Fatalf("shorts = %v", res.Shorts)
	}
	sh := res.Shorts[0]
	if !sh.Date.Equal(s.Payments[3].Date) || sh.Amount != 26.18 {
		t.Fatalf("short = %+v", sh)
	}
	if len(res.Extras) != 0 || len(res.Suggestions) != 0 || len(res.Flags) != 0 {
		t.Fatalf("extras = %v suggestions = %v flags = %v", res.Extras, res.Suggestions, res.Flags)
	}
	if sc := res.Periods[3].Short; sc == nil || *sc != 26.18 {
		t.Fatalf("row short = %v", sc)
	}
}

// T8: a sustained 700-under run whose payment is below the zero-rate
// amortization -> payment_below_minimum_payoff, no suggestion, the run
// falls through to shorts.
func TestMatchSustainedShortBelowMin(t *testing.T) {
	spec := matchSpecA()
	s, err := Generate(spec)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	txns := gridTxns(spec, 1, 20, func(i int) float64 {
		if i >= 11 {
			return 626.18
		}
		return s.Payments[i-1].Payment
	})
	res, err := Match(spec, txns)
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if !flagHas(res.Flags, "payment_below_minimum_payoff") {
		t.Fatalf("flags = %v", res.Flags)
	}
	if len(res.Suggestions) != 0 {
		t.Fatalf("suggestions = %v", res.Suggestions)
	}
	if len(res.Shorts) != 10 {
		t.Fatalf("shorts = %v", res.Shorts)
	}
	for i, sh := range res.Shorts {
		if sh.Amount != 700.00 || !sh.Date.Equal(s.Payments[10+i].Date) {
			t.Fatalf("short %d = %+v", i, sh)
		}
	}
	if sc := res.Periods[10].Short; sc == nil || *sc != 700.00 {
		t.Fatalf("row short = %v", sc)
	}
}

// T9: a sustained 50000-over run exceeds the 100%-rate amortization ->
// payment_above_maximum_rate, no suggestion, the run falls through to
// extras.
func TestMatchSustainedAboveMax(t *testing.T) {
	spec := matchSpecA()
	s, err := Generate(spec)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	txns := gridTxns(spec, 1, 20, func(i int) float64 {
		if i >= 11 {
			return s.Payments[i-1].Payment + 50000
		}
		return s.Payments[i-1].Payment
	})
	res, err := Match(spec, txns)
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if !flagHas(res.Flags, "payment_above_maximum_rate") {
		t.Fatalf("flags = %v", res.Flags)
	}
	if len(res.Suggestions) != 0 {
		t.Fatalf("suggestions = %v", res.Suggestions)
	}
	if len(res.Extras) != 10 {
		t.Fatalf("extras = %v", res.Extras)
	}
	for i, e := range res.Extras {
		if e.Amount != 50000.00 || !e.Date.Equal(s.Payments[10+i].Date) {
			t.Fatalf("extra %d = %+v", i, e)
		}
	}
}

// T10: two skipped plan periods -> missing rows (nil payment) with
// interest accruing; no deviations on the matched rows.
func TestMatchMissingPeriods(t *testing.T) {
	spec := matchSpecA()
	s, err := Generate(spec)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	txns := append(gridTxns(spec, 1, 3, func(i int) float64 { return s.Payments[i-1].Payment }),
		gridTxns(spec, 6, 10, func(i int) float64 { return s.Payments[i-1].Payment })...)
	res, err := Match(spec, txns)
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if res.PeriodsMatched != 8 || res.PeriodsMissing != 2 || len(res.Periods) != 10 {
		t.Fatalf("matched = %d missing = %d rows = %d", res.PeriodsMatched, res.PeriodsMissing, len(res.Periods))
	}
	if res.Periods[3].Payment != nil || res.Periods[4].Payment != nil {
		t.Fatalf("skipped rows: %+v %+v", res.Periods[3], res.Periods[4])
	}
	if res.Periods[2].Payment == nil || res.Periods[5].Payment == nil {
		t.Fatalf("matched rows nil")
	}
	if res.ActualBalance <= res.PlannedBalance {
		t.Fatalf("actual %v should be above planned %v (interest accrued, nothing paid)", res.ActualBalance, res.PlannedBalance)
	}
	if len(res.Flags) != 0 || len(res.Extras) != 0 || len(res.Shorts) != 0 || len(res.Suggestions) != 0 {
		t.Fatalf("deviations: %v %v %v %v", res.Flags, res.Extras, res.Shorts, res.Suggestions)
	}
}

// T11: an early payoff stops the table at the payoff row; a later
// off-grid debit is a post-payoff extra.
func TestMatchEarlyPayoff(t *testing.T) {
	spec := matchSpecA()
	s, err := Generate(spec)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	txns := append(gridTxns(spec, 1, 5, func(i int) float64 { return s.Payments[i-1].Payment }),
		Txn{Date: s.Payments[5].Date, Amount: -490000},
		Txn{Date: time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC), Amount: -100})
	res, err := Match(spec, txns)
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if res.ActualPayoffDate == nil || !res.ActualPayoffDate.Equal(s.Payments[5].Date) {
		t.Fatalf("payoff = %v", res.ActualPayoffDate)
	}
	if res.ActualBalance != 0 {
		t.Fatalf("actual balance = %v", res.ActualBalance)
	}
	if len(res.Periods) != 6 {
		t.Fatalf("rows = %d", len(res.Periods))
	}
	last := res.Periods[5]
	if last.Payment == nil || *last.Payment != 490000.00 || last.Balance != 0 {
		t.Fatalf("payoff row = %+v", last)
	}
	// Deterministic order: off-grid debits first, then the payoff excess.
	if len(res.Extras) != 2 ||
		!res.Extras[0].Date.Equal(time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC)) || res.Extras[0].Amount != 100.00 ||
		!res.Extras[1].Date.Equal(s.Payments[5].Date) || res.Extras[1].Amount != 488673.82 {
		t.Fatalf("extras = %v", res.Extras)
	}
	if len(res.Flags) != 0 || len(res.Shorts) != 0 || len(res.Suggestions) != 0 {
		t.Fatalf("flags = %v shorts = %v suggestions = %v", res.Flags, res.Shorts, res.Suggestions)
	}
}

// T12: a sustained run whose implied shift is below matchMinShift (a +4
// overpayment implies ~4.5bp, far under 0.25) is the plan's own drift ->
// no suggestion and no extras: the run is absorbed and its periods
// report as normal (no row badges).
func TestMatchSustainedTinyShift(t *testing.T) {
	spec := matchSpecA()
	s, err := Generate(spec)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	txns := gridTxns(spec, 1, 20, func(i int) float64 {
		if i >= 11 {
			return s.Payments[i-1].Payment + 4.00
		}
		return s.Payments[i-1].Payment
	})
	res, err := Match(spec, txns)
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if len(res.Suggestions) != 0 {
		t.Fatalf("suggestions = %v", res.Suggestions)
	}
	if len(res.Extras) != 0 || len(res.Shorts) != 0 || len(res.Flags) != 0 {
		t.Fatalf("extras = %v shorts = %v flags = %v", res.Extras, res.Shorts, res.Flags)
	}
	for i := 10; i <= 19; i++ {
		if res.Periods[i].Extra != nil || res.Periods[i].Short != nil {
			t.Fatalf("row %d should be absorbed, got %+v", i+1, res.Periods[i])
		}
	}
}

// T13: impliedRate round-trip + the two feasibility flags.
func TestImpliedRateRoundTrip(t *testing.T) {
	p := PeriodicRate(4.39, 24)
	pay := amortize(485000, p, 600)
	bal := 485000.0
	for k := 0; k < 10; k++ {
		bal = bal + bal*p - pay
	}
	// Round-trip at 5.39.
	pbar := amortize(bal, PeriodicRate(5.39, 24), 590)
	a, ok, flag := impliedRate(bal, pbar, 24, 590)
	if !ok || flag != "" || math.Abs(a-5.39) > 0.001 {
		t.Fatalf("round trip: a = %v ok = %v flag = %v", a, ok, flag)
	}
	// Below the zero-rate amortization.
	a, ok, flag = impliedRate(bal, bal/590-10, 24, 590)
	if ok || flag != "payment_below_minimum_payoff" {
		t.Fatalf("below-min: a = %v ok = %v flag = %v", a, ok, flag)
	}
	// Above the 100%-rate amortization.
	a, ok, flag = impliedRate(bal, 50000, 24, 590)
	if ok || flag != "payment_above_maximum_rate" {
		t.Fatalf("above-max: a = %v ok = %v flag = %v", a, ok, flag)
	}
}

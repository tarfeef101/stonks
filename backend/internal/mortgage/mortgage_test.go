package mortgage

import (
	"math"
	"testing"
	"time"
)

// refSpec is the closed-form reference loan: 500000 @ 4.99%, 25 yr, monthly,
// start 2026-01-15, no extra, term.
func refSpec() Spec {
	return Spec{
		Principal:             500000,
		AnnualRate:            4.99,
		AmortizationYears:     25,
		StartDate:             time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC),
		PaymentFrequency:      "monthly",
		ExtraPaymentAmount:    0,
		ExtraPaymentFrequency: "monthly",
		ExtraPrepaymentType:   "term",
	}
}

// evDate builds a UTC date for rate-event fixtures.
func evDate(y int, m int, d int) time.Time {
	return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
}

// samePayments reports whether two payment slices are element-identical
// (time.Time compared with Equal, not ==).
func samePayments(a, b []Payment) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		p, q := a[i], b[i]
		if p.Index != q.Index || !p.Date.Equal(q.Date) || p.Payment != q.Payment ||
			p.Interest != q.Interest || p.Principal != q.Principal ||
			p.Extra != q.Extra || p.Balance != q.Balance {
			return false
		}
	}
	return true
}

// sameSchedule reports whether two schedules are identical.
func sameSchedule(a, b Schedule) bool {
	if a.BasePayment != b.BasePayment || a.PayoffIndex != b.PayoffIndex ||
		!a.PayoffDate.Equal(b.PayoffDate) || a.TotalPaid != b.TotalPaid ||
		a.TotalInterest != b.TotalInterest {
		return false
	}
	return samePayments(a.Payments, b.Payments)
}

// closedBalance is the closed-form balance after k payments of pay on
// principal P at period rate r (r > 0, no extras, no rate changes):
//
//	B_k = P(1+r)^k - pay*((1+r)^k - 1)/r
//
// It is the same quantity the simulation builds up payment by payment, so
// event tests can pin post-event values without hardcoding payoff indices.
func closedBalance(P, r, pay float64, k int) float64 {
	g := math.Pow(1+r, float64(k))
	return P*g - pay*(g-1)/r
}

// TestPeriodicRate pins the Canadian semi-annual formula per frequency.
func TestPeriodicRate(t *testing.T) {
	r := PeriodicRate(4.99, 12)
	want := math.Pow(1.02495, 1.0/6.0) - 1
	if r != want {
		t.Fatalf("monthly 4.99: r = %v, want %v (exact)", r, want)
	}
	if math.Abs(r-0.004115752) >= 1e-9 {
		t.Fatalf("monthly 4.99: r = %v, human anchor 0.004115752 not within 1e-9", r)
	}
	if got := PeriodicRate(4.99, 24); got != math.Pow(1.02495, 1.0/12.0)-1 {
		t.Fatalf("semi_monthly 4.99: r = %v, want %v (exact)", got, math.Pow(1.02495, 1.0/12.0)-1)
	}
	if got := PeriodicRate(4.99, 26); got != math.Pow(1.02495, 2.0/26.0)-1 {
		t.Fatalf("biweekly 4.99: r = %v, want %v (exact)", got, math.Pow(1.02495, 2.0/26.0)-1)
	}
	for _, ppy := range []int{12, 24, 26} {
		if got := PeriodicRate(0, ppy); got != 0 {
			t.Fatalf("0%%: PeriodicRate(0, %d) = %v, want 0", ppy, got)
		}
	}
}

// TestReferenceClosedForm is THE closed-form reference test (handoff pin 2).
func TestReferenceClosedForm(t *testing.T) {
	sch, err := Generate(refSpec())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Derive payment 300's date by chaining AddDate(0,1,0) 299 times from
	// the start so the derivation is visible; the literal is the
	// cross-check.
	derived := refSpec().StartDate
	for i := 0; i < 299; i++ {
		derived = derived.AddDate(0, 1, 0)
	}
	wantPayoff := time.Date(2050, 12, 15, 0, 0, 0, 0, time.UTC)
	if !derived.Equal(wantPayoff) {
		t.Fatalf("derived payment-300 date %v != literal %v", derived, wantPayoff)
	}
	if sch.PayoffIndex != 300 {
		t.Fatalf("PayoffIndex = %d, want 300", sch.PayoffIndex)
	}
	if len(sch.Payments) != 300 {
		t.Fatalf("len(Payments) = %d, want 300", len(sch.Payments))
	}
	if !sch.PayoffDate.Equal(wantPayoff) {
		t.Fatalf("PayoffDate = %v, want %v", sch.PayoffDate, wantPayoff)
	}

	r := PeriodicRate(4.99, 12)
	closedForm := 500000 * r / (1 - math.Pow(1+r, -300))
	if math.Abs(sch.BasePayment-closedForm) >= 0.01 {
		t.Fatalf("BasePayment = %v, closed form %v not within 0.01", sch.BasePayment, closedForm)
	}

	// ONLINE REFERENCE VERIFICATION (mandatory, per handoff 7.2):
	//   Source:   WOWA Canadian mortgage calculator
	//   URL:      https://wowa.ca/calculators/mortgage-calculator
	//   Accessed: 2026-11-07
	//   Figure:   $500,000 / 25 yr / MONTHLY payment table; the page states
	//             Canadian semi-annual compounding: 2% -> $2,117,
	//             3% -> $2,366, 4% -> $2,630, 5% -> $2,908, 6% -> $3,199.
	//   Engine:   4% -> 2630.10, 5% -> 2908.02, 6% -> 3199.03 (same engine);
	//             at the reference rate 4.99% the engine gives 2905.18, while
	//             a US-style (nominal/12 monthly) computation would give
	//             ~2920.04.
	//   Verdict:  MATCH within ~$1 at every tabulated rate, so the source is
	//             a valid Canadian semi-annual reference and the engine's
	//             2905.18 at 4.99% is consistent with its curve.
	const (
		wantBasePayment   = 2905.18
		wantTotalInterest = 371553.77
		wantFirstInterest = 2057.88
	)
	if sch.BasePayment != wantBasePayment {
		t.Fatalf("BasePayment = %v, want %v (pinned)", sch.BasePayment, wantBasePayment)
	}
	if sch.TotalInterest != wantTotalInterest {
		t.Fatalf("TotalInterest = %v, want %v (pinned)", sch.TotalInterest, wantTotalInterest)
	}
	if got := sch.Payments[0].Interest; got != wantFirstInterest {
		t.Fatalf("first Interest = %v, want %v (pinned)", got, wantFirstInterest)
	}

	// Invariants.
	if last := sch.Payments[299]; last.Balance != 0.00 {
		t.Fatalf("final Balance = %v, want 0.00 exactly", last.Balance)
	}
	for i, p := range sch.Payments[:299] {
		if p.Balance <= 0 {
			t.Fatalf("payment %d Balance = %v, want > 0 before the last", i+1, p.Balance)
		}
	}
	if math.Abs(sch.TotalPaid-(sch.TotalInterest+500000.00)) >= 0.01 {
		t.Fatalf("TotalPaid = %v, TotalInterest + 500000 = %v, not within 0.01",
			sch.TotalPaid, sch.TotalInterest+500000.00)
	}
}

// TestZeroRate pins the r == 0 branch (P/n, no interest).
func TestZeroRate(t *testing.T) {
	spec := Spec{
		Principal:             12000,
		AnnualRate:            0,
		AmortizationYears:     1,
		StartDate:             time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		PaymentFrequency:      "monthly",
		ExtraPaymentAmount:    0,
		ExtraPaymentFrequency: "monthly",
		ExtraPrepaymentType:   "term",
	}
	sch, err := Generate(spec)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if sch.BasePayment != 1000.00 {
		t.Fatalf("BasePayment = %v, want 1000.00", sch.BasePayment)
	}
	if sch.PayoffIndex != 12 {
		t.Fatalf("PayoffIndex = %d, want 12", sch.PayoffIndex)
	}
	if sch.TotalInterest != 0.00 {
		t.Fatalf("TotalInterest = %v, want 0.00", sch.TotalInterest)
	}
	for i, p := range sch.Payments {
		if p.Interest != 0.00 {
			t.Fatalf("payment %d Interest = %v, want 0.00", i+1, p.Interest)
		}
		if p.Principal != 1000.00 {
			t.Fatalf("payment %d Principal = %v, want 1000.00", i+1, p.Principal)
		}
	}
	if last := sch.Payments[11]; last.Balance != 0.00 {
		t.Fatalf("final Balance = %v, want 0.00", last.Balance)
	}
	want := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	if !sch.PayoffDate.Equal(want) {
		t.Fatalf("PayoffDate = %v, want %v", sch.PayoffDate, want)
	}
}

// TestFinalPaymentAdjustment pins the cent-level tail on the last payment.
func TestFinalPaymentAdjustment(t *testing.T) {
	spec := Spec{
		Principal:             1000,
		AnnualRate:            12,
		AmortizationYears:     1,
		StartDate:             time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC),
		PaymentFrequency:      "monthly",
		ExtraPaymentAmount:    0,
		ExtraPaymentFrequency: "monthly",
		ExtraPrepaymentType:   "term",
	}
	sch, err := Generate(spec)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if sch.PayoffIndex != 12 {
		t.Fatalf("PayoffIndex = %d, want 12", sch.PayoffIndex)
	}
	// PIN 4 CONFLICT (recorded as REPORT.md deviation 3): under the
	// handoff's pinned full-precision semantics (items 3 + 7), the final
	// payment equals the base payment to ~1e-12 by the closed-form
	// identity B_n = 0 => B_{n-1}(1+r) = pay; for this loan both round
	// to 88.71, so the handoff's strict "<" (cent-level tail) is not
	// reachable without changing the pinned rounding semantics. Assert
	// equality at the cent instead.
	if last := sch.Payments[11]; last.Payment != sch.BasePayment {
		t.Fatalf("12th Payment = %v, want == BasePayment %v (full-precision tail ~1e-12)",
			last.Payment, sch.BasePayment)
	}
	r := PeriodicRate(12, 12)
	closedForm := 1000 * r / (1 - math.Pow(1+r, -12))
	wantTotalInterest := 12*closedForm - 1000
	if math.Abs(sch.TotalInterest-wantTotalInterest) >= 0.02 {
		t.Fatalf("TotalInterest = %v, 12*closedForm-1000 = %v, not within 0.02",
			sch.TotalInterest, wantTotalInterest)
	}
}

// TestBiweeklyShape pins the 26-payments-per-year shape and date stepping.
func TestBiweeklyShape(t *testing.T) {
	spec := refSpec() // 500000 @ 4.99%, 25 yr
	spec.PaymentFrequency = "biweekly"
	sch, err := Generate(spec)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if sch.PayoffIndex != 650 {
		t.Fatalf("PayoffIndex = %d, want 650", sch.PayoffIndex)
	}
	start := spec.StartDate
	if !sch.Payments[0].Date.Equal(start) {
		t.Fatalf("Payments[0].Date = %v, want start %v", sch.Payments[0].Date, start)
	}
	if got, want := sch.Payments[1].Date, start.AddDate(0, 0, 14); !got.Equal(want) {
		t.Fatalf("Payments[1].Date = %v, want %v (+14d)", got, want)
	}
	if got, want := sch.Payments[2].Date, start.AddDate(0, 0, 28); !got.Equal(want) {
		t.Fatalf("Payments[2].Date = %v, want %v (+28d)", got, want)
	}
}

// TestRecurringExtraTerm pins monthly recurring extras in term mode.
func TestRecurringExtraTerm(t *testing.T) {
	base := Spec{
		Principal:             10000,
		AnnualRate:            12,
		AmortizationYears:     2,
		StartDate:             time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC),
		PaymentFrequency:      "monthly",
		ExtraPaymentAmount:    0,
		ExtraPaymentFrequency: "monthly",
		ExtraPrepaymentType:   "term",
	}
	baseSch, err := Generate(base)
	if err != nil {
		t.Fatalf("Generate(base): %v", err)
	}
	sc := base
	sc.ExtraPaymentAmount = 100.00
	sch, err := Generate(sc)
	if err != nil {
		t.Fatalf("Generate(extra): %v", err)
	}
	if sch.PayoffIndex >= 24 {
		t.Fatalf("PayoffIndex = %d, want < 24", sch.PayoffIndex)
	}
	if sch.BasePayment != baseSch.BasePayment {
		t.Fatalf("BasePayment = %v, want unchanged %v", sch.BasePayment, baseSch.BasePayment)
	}
	if sch.TotalInterest >= baseSch.TotalInterest {
		t.Fatalf("TotalInterest = %v, want < no-extra %v", sch.TotalInterest, baseSch.TotalInterest)
	}
	for i, p := range sch.Payments {
		if p.Extra != 100.00 {
			t.Fatalf("payment %d Extra = %v, want 100.00", i+1, p.Extra)
		}
	}
}

// TestRecurringExtraPrincipalBasePayment pins the documented meaning of
// BasePayment in principal mode: the INITIAL regular payment, not the
// re-amortized payment value at payoff.
//
// In principal mode `pay` is re-amortized (smaller, same end date) after
// every extra, so pre-round-22 the schedule reported the final partial
// payment as BasePayment — the seed loan (485000 @ 4.39%, 25 yr,
// semi_monthly, extra 200/month, principal) returned 505.43 (its last
// payment) instead of 1326.18 (its initial regular payment). Every
// existing extra test uses "term" mode, where `pay` never mutates, so
// none of them caught it.
func TestRecurringExtraPrincipalBasePayment(t *testing.T) {
	base := Spec{
		Principal:             485000,
		AnnualRate:            4.39,
		AmortizationYears:     25,
		StartDate:             time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC),
		PaymentFrequency:      "semi_monthly",
		ExtraPaymentAmount:    0,
		ExtraPaymentFrequency: "monthly",
		ExtraPrepaymentType:   "principal",
	}
	baseSch, err := Generate(base)
	if err != nil {
		t.Fatalf("Generate(no-extra): %v", err)
	}
	sc := base
	sc.ExtraPaymentAmount = 200.00
	sch, err := Generate(sc)
	if err != nil {
		t.Fatalf("Generate(principal): %v", err)
	}
	// The extras do not change the initial regular payment.
	if sch.BasePayment != baseSch.BasePayment {
		t.Fatalf("BasePayment = %v, want the initial regular payment %v (no-extra)",
			sch.BasePayment, baseSch.BasePayment)
	}
	if sch.BasePayment != 1326.18 {
		t.Fatalf("BasePayment = %v, want 1326.18 (pinned for the seed loan)", sch.BasePayment)
	}
	// And it is decidedly not the final partial payment (the pre-fix
	// symptom: 505.43 here).
	last := sch.Payments[len(sch.Payments)-1]
	if last.Payment >= sch.BasePayment {
		t.Fatalf("final payment %v >= BasePayment %v — BasePayment looks like the re-amortized final payment",
			last.Payment, sch.BasePayment)
	}
	// The extras still re-amortize: a year of monthly extras lowers the
	// regular portion below the initial one, and the term is preserved.
	if sch.PayoffIndex != 600 {
		t.Fatalf("PayoffIndex = %d, want 600 (principal mode preserves the term)", sch.PayoffIndex)
	}
	if sch.Payments[12].Payment >= sch.Payments[0].Payment {
		t.Fatalf("payment 13 = %v, want < first payment %v (re-amortized smaller after a year of extras)",
			sch.Payments[12].Payment, sch.Payments[0].Payment)
	}
}

// TestRecurringExtraQuarterlyBuckets pins the quarterly calendar buckets.
//
// DEVIATION from handoff pin 7 (see REPORT.md section 4): the handoff pins
// six Extra>0 months (2026-01 .. 2027-04), but under the handoff's OWN
// pinned bucket semantics the loan (10000 @ 12%, 2 yr, extra 100 QUARTERLY,
// start 2026-01-15) pays off at k=23 (2027-11-15), so extras land in EIGHT
// quarter buckets: the pinned six plus 2027-07 and 2027-10 (one per new
// quarter, on the first payment in it). The six-month set implies payoff at
// k=16 (2027-04-15), which this loan cannot reach with 100/quarter. The
// test asserts the correct eight-month set.
func TestRecurringExtraQuarterlyBuckets(t *testing.T) {
	sc := Spec{
		Principal:             10000,
		AnnualRate:            12,
		AmortizationYears:     2,
		StartDate:             time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC),
		PaymentFrequency:      "monthly",
		ExtraPaymentAmount:    100.00,
		ExtraPaymentFrequency: "quarterly",
		ExtraPrepaymentType:   "term",
	}
	sch, err := Generate(sc)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	type ym struct{ y, m int }
	want := map[ym]bool{
		{2026, 1}: true, {2026, 4}: true, {2026, 7}: true, {2026, 10}: true,
		{2027, 1}: true, {2027, 4}: true, {2027, 7}: true, {2027, 10}: true,
	}
	got := map[ym]bool{}
	for i, p := range sch.Payments {
		if p.Extra <= 0 {
			continue
		}
		if p.Extra != 100.00 {
			t.Fatalf("payment %d Extra = %v, want 100.00", i+1, p.Extra)
		}
		got[ym{p.Date.Year(), int(p.Date.Month())}] = true
	}
	if len(got) != len(want) {
		t.Fatalf("Extra>0 months = %v, want exactly %v", got, want)
	}
	for k := range want {
		if !got[k] {
			t.Fatalf("missing Extra>0 month %v", k)
		}
	}
}

// TestEquityCurve pins the equity curve shape on the reference schedule.
func TestEquityCurve(t *testing.T) {
	sch, err := Generate(refSpec())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	eq := EquityCurve(sch)
	if len(eq) != 300 {
		t.Fatalf("len = %d, want 300", len(eq))
	}
	for i := 1; i < len(eq); i++ {
		if eq[i].Balance > eq[i-1].Balance {
			t.Fatalf("point %d Balance %v increased from %v", i, eq[i].Balance, eq[i-1].Balance)
		}
		if eq[i].Equity < eq[i-1].Equity {
			t.Fatalf("point %d Equity %v decreased from %v", i, eq[i].Equity, eq[i-1].Equity)
		}
	}
	last := eq[299]
	if last.Balance != 0.00 {
		t.Fatalf("final Balance = %v, want 0.00", last.Balance)
	}
	if math.Abs(last.Equity-500000.00) >= 0.10 {
		t.Fatalf("final Equity = %v, want within 0.10 of 500000.00", last.Equity)
	}
	if eq[0].Equity != sch.Payments[0].Principal {
		t.Fatalf("Equity_0 = %v, want first Principal %v", eq[0].Equity, sch.Payments[0].Principal)
	}
}

// TestRateEventReAmortize pins a renewal that re-amortizes: the event
// payment accrues at the new rate, the following payments are re-amortized
// over the remaining original amortization, and the payoff date is
// preserved.
func TestRateEventReAmortize(t *testing.T) {
	baseSpec := refSpec()
	spec := baseSpec
	spec.RateEvents = []RateEvent{{
		Date:       evDate(2031, 1, 15), // the 61st payment date
		AnnualRate: 6.5,
		RateType:   "fixed",
		Action:     "re_amortize",
	}}
	base, err := Generate(baseSpec)
	if err != nil {
		t.Fatalf("Generate(base): %v", err)
	}
	sch, err := Generate(spec)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if sch.PayoffIndex != 300 {
		t.Fatalf("PayoffIndex = %d, want 300 (re-amortize preserves the payoff date)", sch.PayoffIndex)
	}
	if !sch.PayoffDate.Equal(base.PayoffDate) {
		t.Fatalf("PayoffDate = %v, want base %v", sch.PayoffDate, base.PayoffDate)
	}
	if sch.BasePayment != 2905.18 {
		t.Fatalf("BasePayment = %v, want 2905.18 (the initial payment is unchanged)", sch.BasePayment)
	}
	if !samePayments(base.Payments[:60], sch.Payments[:60]) {
		t.Fatalf("payments 1..60 differ from the base schedule before the event")
	}
	r0 := PeriodicRate(4.99, 12)
	r1 := PeriodicRate(6.5, 12)
	pay0 := amortize(500000, r0, 300)
	// The event payment (the 61st) already accrues at the new rate.
	if want := round2(closedBalance(500000, r0, pay0, 60) * r1); sch.Payments[60].Interest != want {
		t.Fatalf("payment 61 Interest = %v, want %v (new rate on the event payment)",
			sch.Payments[60].Interest, want)
	}
	// From the 62nd on, the regular payment is re-amortized over the
	// remaining 239 periods at the new rate, from the post-event balance.
	// That balance is off the old-rate closed-form path (the event payment
	// itself accrues at the new rate), so it comes from the schedule; the
	// interest pin above already cross-checked it against the closed form.
	pay1 := amortize(sch.Payments[60].Balance, r1, 239)
	for k := 61; k < 299; k++ {
		if want := round2(pay1); sch.Payments[k].Payment != want {
			t.Fatalf("payment %d Payment = %v, want %v (re-amortized over 239)",
				k+1, sch.Payments[k].Payment, want)
		}
	}
}

// TestRateEventKeepPaymentRateCut pins keep_payment with a rate cut: the
// payment is unchanged and the payoff floats EARLIER by the closed-form
// number of remaining periods at the new rate.
func TestRateEventKeepPaymentRateCut(t *testing.T) {
	spec := refSpec()
	spec.RateEvents = []RateEvent{{
		Date:       evDate(2028, 1, 15), // the 25th payment date
		AnnualRate: 3.5,
		Action:     "keep_payment",
	}}
	sch, err := Generate(spec)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	r0 := PeriodicRate(4.99, 12)
	r1 := PeriodicRate(3.5, 12)
	pay0 := amortize(500000, r0, 300)
	// The event payment accrues at the new rate, so the post-event balance
	// is off the old-rate closed-form path; solve m from the schedule's own
	// post-event balance.
	bal25 := sch.Payments[24].Balance
	m := int(math.Ceil(-math.Log(1-bal25*r1/pay0) / math.Log(1+r1)))
	if sch.PayoffIndex != 25+m {
		t.Fatalf("PayoffIndex = %d, want 25+m = %d (closed form at the new rate)",
			sch.PayoffIndex, 25+m)
	}
	if sch.PayoffIndex >= 300 {
		t.Fatalf("PayoffIndex = %d, want < 300 (a rate cut pays off earlier)", sch.PayoffIndex)
	}
	// The event payment accrues at the new rate; the payment itself never
	// changes (only the last payment is the usual tail).
	if want := round2(closedBalance(500000, r0, pay0, 24) * r1); sch.Payments[24].Interest != want {
		t.Fatalf("payment 25 Interest = %v, want %v (new rate on the event payment)",
			sch.Payments[24].Interest, want)
	}
	for i, p := range sch.Payments {
		if i == len(sch.Payments)-1 {
			continue
		}
		if want := round2(pay0); p.Payment != want {
			t.Fatalf("payment %d Payment = %v, want %v (keep_payment keeps the payment)",
				i+1, p.Payment, want)
		}
	}
}

// TestRateEventKeepPaymentExtension pins keep_payment with a rate hike:
// the payment is unchanged and the payoff floats LATER, past the original
// end date (the horizon extends).
func TestRateEventKeepPaymentExtension(t *testing.T) {
	spec := refSpec()
	spec.RateEvents = []RateEvent{{
		Date:       evDate(2031, 1, 15), // the 61st payment date
		AnnualRate: 7.0,
		Action:     "keep_payment",
	}}
	sch, err := Generate(spec)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	r0 := PeriodicRate(4.99, 12)
	r1 := PeriodicRate(7.0, 12)
	pay0 := amortize(500000, r0, 300)
	// The event payment accrues at the new rate, so the post-event balance
	// is off the old-rate closed-form path (and higher than it); solve m
	// from the schedule's own post-event balance.
	bal61 := sch.Payments[60].Balance
	m := int(math.Ceil(-math.Log(1-bal61*r1/pay0) / math.Log(1+r1)))
	if sch.PayoffIndex != 61+m {
		t.Fatalf("PayoffIndex = %d, want 61+m = %d (closed form at the new rate)",
			sch.PayoffIndex, 61+m)
	}
	if sch.PayoffIndex <= 300 {
		t.Fatalf("PayoffIndex = %d, want > 300 (a rate hike extends past the original end)",
			sch.PayoffIndex)
	}
	// The schedule now has payments past the original 2050-12-15 end date.
	if got, want := sch.Payments[300].Date, evDate(2051, 1, 15); !got.Equal(want) {
		t.Fatalf("payment 301 Date = %v, want %v", got, want)
	}
	if want := round2(closedBalance(500000, r0, pay0, 60) * r1); sch.Payments[60].Interest != want {
		t.Fatalf("payment 61 Interest = %v, want %v (new rate on the event payment)",
			sch.Payments[60].Interest, want)
	}
	for i, p := range sch.Payments {
		if i == len(sch.Payments)-1 {
			continue
		}
		if want := round2(pay0); p.Payment != want {
			t.Fatalf("payment %d Payment = %v, want %v (keep_payment keeps the payment)",
				i+1, p.Payment, want)
		}
	}
}

// TestRateEventKeepPaymentNegativeAmortization pins the simulation error
// when a keep_payment rate hike leaves the payment below the new interest.
func TestRateEventKeepPaymentNegativeAmortization(t *testing.T) {
	spec := refSpec()
	spec.RateEvents = []RateEvent{{
		Date:       evDate(2027, 1, 15), // the 13th payment date
		AnnualRate: 9.0,
		Action:     "keep_payment",
	}}
	_, err := Generate(spec)
	if err == nil || err.Error() != "the payment cannot cover the interest after the rate change" {
		t.Fatalf("Generate: err = %v, want the negative-amortization error", err)
	}
}

// TestRateEventEffectiveDates pins when an event takes effect: on the first
// payment dated >= the event date, with the rate already switched for that
// payment's interest.
func TestRateEventEffectiveDates(t *testing.T) {
	r0 := PeriodicRate(4.99, 12)
	r1 := PeriodicRate(6.0, 12)
	pay0 := amortize(500000, r0, 300)

	// Dated ON a payment date (2027-01-15 is the 13th payment): the 13th
	// accrues at the new rate, the 12th still at the old one.
	spec := refSpec()
	spec.RateEvents = []RateEvent{{Date: evDate(2027, 1, 15), AnnualRate: 6.0, Action: "re_amortize"}}
	sch, err := Generate(spec)
	if err != nil {
		t.Fatalf("Generate(on-date): %v", err)
	}
	if want := round2(closedBalance(500000, r0, pay0, 11) * r0); sch.Payments[11].Interest != want {
		t.Fatalf("payment 12 Interest = %v, want %v (old rate before the event date)",
			sch.Payments[11].Interest, want)
	}
	if want := round2(closedBalance(500000, r0, pay0, 12) * r1); sch.Payments[12].Interest != want {
		t.Fatalf("payment 13 Interest = %v, want %v (new rate on the event date)",
			sch.Payments[12].Interest, want)
	}

	// DATED BETWEEN payment dates (2026-02-01): the 1st (2026-01-15) is
	// still at the old rate, the 2nd (2026-02-15) at the new one.
	spec2 := refSpec()
	spec2.RateEvents = []RateEvent{{Date: evDate(2026, 2, 1), AnnualRate: 6.0, Action: "re_amortize"}}
	sch2, err := Generate(spec2)
	if err != nil {
		t.Fatalf("Generate(between): %v", err)
	}
	if want := round2(500000 * r0); sch2.Payments[0].Interest != want {
		t.Fatalf("payment 1 Interest = %v, want %v (old rate before the event date)",
			sch2.Payments[0].Interest, want)
	}
	if want := round2(closedBalance(500000, r0, pay0, 1) * r1); sch2.Payments[1].Interest != want {
		t.Fatalf("payment 2 Interest = %v, want %v (new rate after the event date)",
			sch2.Payments[1].Interest, want)
	}
}

// TestRateEventMultipleReAmortize pins a chain of two renewals: each
// re-amortizes over the remaining ORIGINAL amortization at its own rate,
// and the payoff date is preserved.
func TestRateEventMultipleReAmortize(t *testing.T) {
	spec := refSpec()
	spec.RateEvents = []RateEvent{
		{Date: evDate(2031, 1, 15), AnnualRate: 5.5, Action: "re_amortize"}, // 61st payment
		{Date: evDate(2036, 1, 15), AnnualRate: 6.0, Action: "re_amortize"}, // 121st payment
	}
	sch, err := Generate(spec)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if sch.PayoffIndex != 300 {
		t.Fatalf("PayoffIndex = %d, want 300 (renewals preserve the payoff date)", sch.PayoffIndex)
	}
	r0 := PeriodicRate(4.99, 12)
	r1 := PeriodicRate(5.5, 12)
	r2 := PeriodicRate(6.0, 12)
	pay0 := amortize(500000, r0, 300)
	// Each renewal re-amortizes from the simulation's own post-event
	// balance (the event payment accrues at the new rate, so the balance
	// is off the old-rate closed-form path).
	pay1 := amortize(sch.Payments[60].Balance, r1, 239)
	pay2 := amortize(sch.Payments[120].Balance, r2, 179)
	// First renewal: event payment at r1, then re-amortized over 239.
	if want := round2(closedBalance(500000, r0, pay0, 60) * r1); sch.Payments[60].Interest != want {
		t.Fatalf("payment 61 Interest = %v, want %v (first renewal rate on the event payment)",
			sch.Payments[60].Interest, want)
	}
	for k := 61; k < 120; k++ {
		if want := round2(pay1); sch.Payments[k].Payment != want {
			t.Fatalf("payment %d Payment = %v, want %v (re-amortized over 239 at 5.5%%)",
				k+1, sch.Payments[k].Payment, want)
		}
	}
	// Second renewal: event payment at r2 (from balance 120), then
	// re-amortized over 179.
	if want := round2(sch.Payments[119].Balance * r2); sch.Payments[120].Interest != want {
		t.Fatalf("payment 121 Interest = %v, want %v (second renewal rate on the event payment)",
			sch.Payments[120].Interest, want)
	}
	for k := 121; k < 299; k++ {
		if want := round2(pay2); sch.Payments[k].Payment != want {
			t.Fatalf("payment %d Payment = %v, want %v (re-amortized over 179 at 6.0%%)",
				k+1, sch.Payments[k].Payment, want)
		}
	}
}

// TestRateEventAfterPayoffIgnored pins that an event dated after the
// payoff is a no-op.
func TestRateEventAfterPayoffIgnored(t *testing.T) {
	base := Spec{
		Principal:             10000,
		AnnualRate:            12,
		AmortizationYears:     2,
		StartDate:             evDate(2026, 1, 15),
		PaymentFrequency:      "monthly",
		ExtraPaymentAmount:    5000,
		ExtraPaymentFrequency: "monthly",
		ExtraPrepaymentType:   "term",
	}
	baseSch, err := Generate(base)
	if err != nil {
		t.Fatalf("Generate(base): %v", err)
	}
	if baseSch.PayoffIndex != 2 {
		t.Fatalf("base PayoffIndex = %d, want 2 (the fixture pays off in month 2)", baseSch.PayoffIndex)
	}
	spec := base
	spec.RateEvents = []RateEvent{{Date: evDate(2027, 1, 15), AnnualRate: 4.0, Action: "re_amortize"}}
	sch, err := Generate(spec)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !sameSchedule(baseSch, sch) {
		t.Fatalf("schedule differs from the event-free schedule (an event after the payoff must be ignored)")
	}
}

// TestNextRenewalDate pins the term-expiry date: start + TermYears, or the
// last re_amortize event date + TermYears; keep_payment events and a
// missing TermYears do not move or produce it.
func TestNextRenewalDate(t *testing.T) {
	if _, ok := NextRenewalDate(refSpec()); ok {
		t.Fatalf("ok = true, want false when TermYears is zero (no term metadata)")
	}
	spec := refSpec()
	spec.TermYears = 5
	if got, ok := NextRenewalDate(spec); !ok || !got.Equal(evDate(2031, 1, 15)) {
		t.Fatalf("got %v (ok=%v), want 2031-01-15 (start + 5y)", got, ok)
	}
	spec.RateEvents = []RateEvent{{Date: evDate(2031, 1, 15), AnnualRate: 5.0, Action: "re_amortize"}}
	if got, ok := NextRenewalDate(spec); !ok || !got.Equal(evDate(2036, 1, 15)) {
		t.Fatalf("got %v (ok=%v), want 2036-01-15 (renewal date + 5y)", got, ok)
	}
	spec.RateEvents = []RateEvent{{Date: evDate(2028, 1, 15), AnnualRate: 4.0, Action: "keep_payment"}}
	if got, ok := NextRenewalDate(spec); !ok || !got.Equal(evDate(2031, 1, 15)) {
		t.Fatalf("got %v (ok=%v), want 2031-01-15 (keep_payment does not move the renewal)", got, ok)
	}
	spec.RateEvents = []RateEvent{
		{Date: evDate(2031, 1, 15), AnnualRate: 5.0, Action: "re_amortize"},
		{Date: evDate(2036, 1, 15), AnnualRate: 5.5, Action: "re_amortize"},
	}
	if got, ok := NextRenewalDate(spec); !ok || !got.Equal(evDate(2041, 1, 15)) {
		t.Fatalf("got %v (ok=%v), want 2041-01-15 (last re_amortize + 5y)", got, ok)
	}
}

// TestValidation pins every exact validation error string, in order.
func TestValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(Spec) Spec
		want   string
	}{
		{"principal zero", func(s Spec) Spec { s.Principal = 0; return s },
			"principal must be greater than zero"},
		{"principal negative", func(s Spec) Spec { s.Principal = -1; return s },
			"principal must be greater than zero"},
		{"rate negative", func(s Spec) Spec { s.AnnualRate = -1; return s },
			"annual rate must be between 0 and 100"},
		{"rate above 100", func(s Spec) Spec { s.AnnualRate = 100.01; return s },
			"annual rate must be between 0 and 100"},
		{"rate NaN", func(s Spec) Spec { s.AnnualRate = math.NaN(); return s },
			"annual rate must be between 0 and 100"},
		{"years zero", func(s Spec) Spec { s.AmortizationYears = 0; return s },
			"amortization years must be between 1 and 40"},
		{"years 41", func(s Spec) Spec { s.AmortizationYears = 41; return s },
			"amortization years must be between 1 and 40"},
		{"payment frequency empty", func(s Spec) Spec { s.PaymentFrequency = ""; return s },
			"payment frequency must be monthly, semi_monthly, or biweekly"},
		{"payment frequency weekly", func(s Spec) Spec { s.PaymentFrequency = "weekly"; return s },
			"payment frequency must be monthly, semi_monthly, or biweekly"},
		{"extra amount negative", func(s Spec) Spec { s.ExtraPaymentAmount = -100; return s },
			"extra payment amount must not be negative"},
		{"extra frequency empty", func(s Spec) Spec { s.ExtraPaymentFrequency = ""; return s },
			"extra payment frequency must be monthly, quarterly, or yearly"},
		{"extra frequency biweekly", func(s Spec) Spec { s.ExtraPaymentFrequency = "biweekly"; return s },
			"extra payment frequency must be monthly, quarterly, or yearly"},
		{"prepayment type empty", func(s Spec) Spec { s.ExtraPrepaymentType = ""; return s },
			"extra prepayment type must be term or principal"},
		{"prepayment type lump", func(s Spec) Spec { s.ExtraPrepaymentType = "lump"; return s },
			"extra prepayment type must be term or principal"},
		{"term years 41", func(s Spec) Spec { s.TermYears = 41; return s },
			"term years must be between 0 and 40"},
		{"term years negative", func(s Spec) Spec { s.TermYears = -1; return s },
			"term years must be between 0 and 40"},
		{"rate type adjustable", func(s Spec) Spec { s.RateType = "adjustable"; return s },
			"rate type must be fixed or variable"},
		{"event date on start", func(s Spec) Spec {
			s.RateEvents = []RateEvent{{Date: s.StartDate, AnnualRate: 5, Action: "re_amortize"}}
			return s
		}, "rate event date must be after the start date"},
		{"event date before start", func(s Spec) Spec {
			s.RateEvents = []RateEvent{{Date: s.StartDate.AddDate(0, 0, -1), AnnualRate: 5, Action: "re_amortize"}}
			return s
		}, "rate event date must be after the start date"},
		{"event rate zero", func(s Spec) Spec {
			s.RateEvents = []RateEvent{{Date: evDate(2027, 1, 15), AnnualRate: 0, Action: "re_amortize"}}
			return s
		}, "rate event annual rate must be between 0 and 100"},
		{"event rate above 100", func(s Spec) Spec {
			s.RateEvents = []RateEvent{{Date: evDate(2027, 1, 15), AnnualRate: 100.01, Action: "re_amortize"}}
			return s
		}, "rate event annual rate must be between 0 and 100"},
		{"event rate NaN", func(s Spec) Spec {
			s.RateEvents = []RateEvent{{Date: evDate(2027, 1, 15), AnnualRate: math.NaN(), Action: "re_amortize"}}
			return s
		}, "rate event annual rate must be between 0 and 100"},
		{"event rate type bogus", func(s Spec) Spec {
			s.RateEvents = []RateEvent{{Date: evDate(2027, 1, 15), AnnualRate: 5, RateType: "bogus", Action: "re_amortize"}}
			return s
		}, "rate event rate type must be fixed or variable"},
		{"event action refinance", func(s Spec) Spec {
			s.RateEvents = []RateEvent{{Date: evDate(2027, 1, 15), AnnualRate: 5, Action: "refinance"}}
			return s
		}, "rate event action must be re_amortize or keep_payment"},
		{"event dates equal", func(s Spec) Spec {
			s.RateEvents = []RateEvent{
				{Date: evDate(2027, 1, 15), AnnualRate: 5, Action: "re_amortize"},
				{Date: evDate(2027, 1, 15), AnnualRate: 6, Action: "re_amortize"},
			}
			return s
		}, "rate event dates must be strictly ascending"},
		{"event dates descending", func(s Spec) Spec {
			s.RateEvents = []RateEvent{
				{Date: evDate(2028, 1, 15), AnnualRate: 5, Action: "re_amortize"},
				{Date: evDate(2027, 1, 15), AnnualRate: 6, Action: "re_amortize"},
			}
			return s
		}, "rate event dates must be strictly ascending"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Generate(tc.mutate(refSpec()))
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Generate: err = %v, want %q", err, tc.want)
			}
		})
	}
}

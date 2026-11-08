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

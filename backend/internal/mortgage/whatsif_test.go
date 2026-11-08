package mortgage

import (
	"testing"
	"time"
)

// whatIfBase is the 10000 @ 12%, 2 yr monthly loan used by the what-if tests.
func whatIfBase() Spec {
	return Spec{
		Principal:             10000,
		AnnualRate:            12,
		AmortizationYears:     2,
		StartDate:             time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC),
		PaymentFrequency:      "monthly",
		ExtraPaymentAmount:    0,
		ExtraPaymentFrequency: "monthly",
		ExtraPrepaymentType:   "term",
	}
}

func lumpDec2026() *Prepayment {
	return &Prepayment{
		Amount: 5000.00,
		Date:   time.Date(2026, 12, 15, 0, 0, 0, 0, time.UTC), // == the 12th payment date
	}
}

// TestOneTimePrepaymentTerm pins the term-mode one-time prepayment.
func TestOneTimePrepaymentTerm(t *testing.T) {
	base := whatIfBase()
	baseSch, err := Generate(base)
	if err != nil {
		t.Fatalf("Generate(base): %v", err)
	}
	if baseSch.PayoffIndex != 24 {
		t.Fatalf("base PayoffIndex = %d, want 24", baseSch.PayoffIndex)
	}
	comp, err := WhatIf(base, Scenario{OneTimePrepayment: lumpDec2026()})
	if err != nil {
		t.Fatalf("WhatIf: %v", err)
	}
	if comp.MonthsSaved <= 0 {
		t.Fatalf("MonthsSaved = %d, want > 0", comp.MonthsSaved)
	}
	if comp.InterestSaved <= 0 {
		t.Fatalf("InterestSaved = %v, want > 0", comp.InterestSaved)
	}
	if comp.Schedule.PayoffIndex >= 24 {
		t.Fatalf("PayoffIndex = %d, want < 24", comp.Schedule.PayoffIndex)
	}
	if comp.Schedule.PayoffIndex != 13 {
		t.Fatalf("PayoffIndex = %d, want 13 (pinned)", comp.Schedule.PayoffIndex)
	}
	if comp.MonthsSaved != baseSch.PayoffIndex-comp.Schedule.PayoffIndex {
		t.Fatalf("MonthsSaved = %d, want base.PayoffIndex - scenario.PayoffIndex = %d",
			comp.MonthsSaved, baseSch.PayoffIndex-comp.Schedule.PayoffIndex)
	}
	if comp.MonthsSaved != 11 {
		t.Fatalf("MonthsSaved = %d, want 11 (pinned)", comp.MonthsSaved)
	}
	if comp.Schedule.BasePayment != baseSch.BasePayment {
		t.Fatalf("scenario BasePayment = %v, want base %v (term keeps the payment)",
			comp.Schedule.BasePayment, baseSch.BasePayment)
	}
}

// TestOneTimePrepaymentPrincipal pins the principal-mode one-time
// prepayment: full term preserved, payment re-amortized smaller.
func TestOneTimePrepaymentPrincipal(t *testing.T) {
	base := whatIfBase()
	base.ExtraPrepaymentType = "principal"
	sc := Scenario{OneTimePrepayment: lumpDec2026()}
	comp, err := WhatIf(base, sc)
	if err != nil {
		t.Fatalf("WhatIf: %v", err)
	}
	if comp.Schedule.PayoffIndex != 24 {
		t.Fatalf("PayoffIndex = %d, want 24 (full term preserved)", comp.Schedule.PayoffIndex)
	}
	if comp.InterestSaved <= 0 {
		t.Fatalf("InterestSaved = %v, want > 0", comp.InterestSaved)
	}
	regular13 := comp.Schedule.Payments[12].Payment - comp.Schedule.Payments[12].Extra
	if regular13 >= comp.Schedule.Payments[10].Payment {
		t.Fatalf("regular portion after the 12th = %v, want < 11th payment %v",
			regular13, comp.Schedule.Payments[10].Payment)
	}
	term := whatIfBase() // same inputs, term mode
	termComp, err := WhatIf(term, sc)
	if err != nil {
		t.Fatalf("WhatIf(term): %v", err)
	}
	if comp.InterestSaved >= termComp.InterestSaved {
		t.Fatalf("principal-mode InterestSaved = %v, want < term-mode %v",
			comp.InterestSaved, termComp.InterestSaved)
	}
}

// TestRateChangeWhatIf pins a rate increase applied from the start.
func TestRateChangeWhatIf(t *testing.T) {
	spec := refSpec() // 500000 @ 4.99%, 25 yr monthly, extra 0
	base, err := Generate(spec)
	if err != nil {
		t.Fatalf("Generate(base): %v", err)
	}
	rate := 5.99
	comp, err := WhatIf(spec, Scenario{AnnualRate: &rate})
	if err != nil {
		t.Fatalf("WhatIf: %v", err)
	}
	if comp.Schedule.PayoffIndex != 300 {
		t.Fatalf("PayoffIndex = %d, want 300 (full term, no extra)", comp.Schedule.PayoffIndex)
	}
	if comp.Schedule.TotalInterest <= base.TotalInterest {
		t.Fatalf("TotalInterest = %v, want > base %v", comp.Schedule.TotalInterest, base.TotalInterest)
	}
	if comp.MonthsSaved != 0 {
		t.Fatalf("MonthsSaved = %d, want 0", comp.MonthsSaved)
	}
	if comp.InterestSaved >= 0 {
		t.Fatalf("InterestSaved = %v, want < 0", comp.InterestSaved)
	}
}

// TestRateChangeDown pins a rate decrease applied from the start.
func TestRateChangeDown(t *testing.T) {
	spec := refSpec()
	rate := 3.99
	comp, err := WhatIf(spec, Scenario{AnnualRate: &rate})
	if err != nil {
		t.Fatalf("WhatIf: %v", err)
	}
	if comp.Schedule.PayoffIndex != 300 {
		t.Fatalf("PayoffIndex = %d, want 300", comp.Schedule.PayoffIndex)
	}
	if comp.InterestSaved <= 0 {
		t.Fatalf("InterestSaved = %v, want > 0", comp.InterestSaved)
	}
	if comp.MonthsSaved != 0 {
		t.Fatalf("MonthsSaved = %d, want 0", comp.MonthsSaved)
	}
}

// TestScenarioExtraOverride pins recurring-extra overrides via Scenario.
func TestScenarioExtraOverride(t *testing.T) {
	base := whatIfBase() // extra 0.00
	baseSch, err := Generate(base)
	if err != nil {
		t.Fatalf("Generate(base): %v", err)
	}
	amount := 500.00
	freq := "monthly"
	comp, err := WhatIf(base, Scenario{ExtraPaymentAmount: &amount, ExtraPaymentFrequency: &freq})
	if err != nil {
		t.Fatalf("WhatIf: %v", err)
	}
	if comp.MonthsSaved <= 0 {
		t.Fatalf("MonthsSaved = %d, want > 0", comp.MonthsSaved)
	}
	if comp.InterestSaved <= 0 {
		t.Fatalf("InterestSaved = %v, want > 0", comp.InterestSaved)
	}
	if comp.Schedule.BasePayment != baseSch.BasePayment {
		t.Fatalf("scenario BasePayment = %v, want base %v", comp.Schedule.BasePayment, baseSch.BasePayment)
	}
}

// TestPrepaymentAfterPayoff pins the sim-time error when the lump-sum date
// is unreachable.
func TestPrepaymentAfterPayoff(t *testing.T) {
	base := Spec{
		Principal:             12000,
		AnnualRate:            0,
		AmortizationYears:     1,
		StartDate:             time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		PaymentFrequency:      "monthly",
		ExtraPaymentAmount:    0,
		ExtraPaymentFrequency: "monthly",
		ExtraPrepaymentType:   "term",
	}
	sc := Scenario{OneTimePrepayment: &Prepayment{
		Amount: 500.00,
		Date:   time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
	}}
	_, err := WhatIf(base, sc)
	if err == nil || err.Error() != "one-time prepayment date is after the payoff date" {
		t.Fatalf("WhatIf: err = %v, want the after-payoff error", err)
	}
}

// TestPrepaymentValidation pins the one-time amount validation error.
func TestPrepaymentValidation(t *testing.T) {
	base := whatIfBase()
	for _, amt := range []float64{0, -1} {
		sc := Scenario{OneTimePrepayment: &Prepayment{
			Amount: amt,
			Date:   time.Date(2026, 12, 15, 0, 0, 0, 0, time.UTC),
		}}
		_, err := WhatIf(base, sc)
		if err == nil || err.Error() != "one-time prepayment amount must be greater than zero" {
			t.Fatalf("WhatIf(amount %v): err = %v, want the amount error", amt, err)
		}
	}
}

// TestComparisonIdentity pins that the five Comparison fields stay
// consistent with the embedded schedule (test 1's inputs).
func TestComparisonIdentity(t *testing.T) {
	base := whatIfBase()
	baseSch, err := Generate(base)
	if err != nil {
		t.Fatalf("Generate(base): %v", err)
	}
	comp, err := WhatIf(base, Scenario{OneTimePrepayment: lumpDec2026()})
	if err != nil {
		t.Fatalf("WhatIf: %v", err)
	}
	if comp.InterestSaved != baseSch.TotalInterest-comp.Schedule.TotalInterest {
		t.Fatalf("InterestSaved = %v, want base.TotalInterest - scenario.TotalInterest = %v",
			comp.InterestSaved, baseSch.TotalInterest-comp.Schedule.TotalInterest)
	}
	if !comp.PayoffDate.Equal(comp.Schedule.PayoffDate) {
		t.Fatalf("PayoffDate = %v, want Schedule.PayoffDate %v",
			comp.PayoffDate, comp.Schedule.PayoffDate)
	}
}

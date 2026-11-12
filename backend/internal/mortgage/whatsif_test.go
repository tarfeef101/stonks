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

// TestRenewalWhatIf pins a mid-term renewal: stored events dated on or
// after the renewal date are superseded, earlier ones are kept, the
// scenario equals Generate of the composed spec, and the stored slice is
// never mutated.
func TestRenewalWhatIf(t *testing.T) {
	spec := refSpec()
	spec.TermYears = 5
	spec.RateEvents = []RateEvent{
		{Date: evDate(2028, 1, 15), AnnualRate: 4.0, Action: "re_amortize"}, // 25th payment, kept
		{Date: evDate(2031, 1, 15), AnnualRate: 6.0, Action: "re_amortize"}, // 61st, superseded ON the date
		{Date: evDate(2036, 1, 15), AnnualRate: 6.5, Action: "re_amortize"}, // 121st, superseded
	}
	comp, err := WhatIf(spec, Scenario{Renewal: &Renewal{
		Date:       evDate(2031, 1, 15),
		AnnualRate: 7.0,
		RateType:   "fixed",
	}})
	if err != nil {
		t.Fatalf("WhatIf: %v", err)
	}
	want := refSpec()
	want.RateEvents = []RateEvent{
		{Date: evDate(2028, 1, 15), AnnualRate: 4.0, Action: "re_amortize"},
		{Date: evDate(2031, 1, 15), AnnualRate: 7.0, RateType: "fixed", Action: "re_amortize"},
	}
	wantSch, err := Generate(want)
	if err != nil {
		t.Fatalf("Generate(want): %v", err)
	}
	if !sameSchedule(comp.Schedule, wantSch) {
		t.Fatalf("scenario schedule differs from the expected renewal schedule")
	}
	if comp.Schedule.PayoffIndex != 300 {
		t.Fatalf("PayoffIndex = %d, want 300 (renewal re-amortizes over the remaining term)",
			comp.Schedule.PayoffIndex)
	}
	if comp.MonthsSaved != 0 {
		t.Fatalf("MonthsSaved = %d, want 0 (a renewal preserves the payoff date)", comp.MonthsSaved)
	}
	if comp.InterestSaved >= 0 {
		t.Fatalf("InterestSaved = %v, want < 0 (renewing at a higher rate costs more)", comp.InterestSaved)
	}
	// The stored slice is never mutated.
	if len(spec.RateEvents) != 3 {
		t.Fatalf("stored events len = %d, want 3 (unchanged)", len(spec.RateEvents))
	}
	if spec.RateEvents[1].AnnualRate != 6.0 || spec.RateEvents[2].AnnualRate != 6.5 {
		t.Fatalf("stored events mutated: %+v", spec.RateEvents)
	}
}

// TestPaymentFrequencyWhatIf pins a payment-frequency override: the
// scenario equals Generate of the same spec at the new frequency, and the
// larger biweekly payoff index makes MonthsSaved negative.
func TestPaymentFrequencyWhatIf(t *testing.T) {
	spec := refSpec()
	freq := "biweekly"
	comp, err := WhatIf(spec, Scenario{PaymentFrequency: &freq})
	if err != nil {
		t.Fatalf("WhatIf: %v", err)
	}
	want := spec
	want.PaymentFrequency = "biweekly"
	wantSch, err := Generate(want)
	if err != nil {
		t.Fatalf("Generate(want): %v", err)
	}
	if !sameSchedule(comp.Schedule, wantSch) {
		t.Fatalf("scenario schedule differs from the biweekly Generate schedule")
	}
	if comp.Schedule.PayoffIndex != 650 {
		t.Fatalf("PayoffIndex = %d, want 650 (25 yr * 26 biweekly)", comp.Schedule.PayoffIndex)
	}
	if comp.MonthsSaved != 300-650 {
		t.Fatalf("MonthsSaved = %d, want 300-650 = -350", comp.MonthsSaved)
	}
}

// TestRenewalWithPrepayment pins a renewal combined with a one-time
// prepayment: the scenario equals runSim of the spec whose only event is
// the renewal, and the prepayment pulls the payoff in earlier.
func TestRenewalWithPrepayment(t *testing.T) {
	spec := refSpec()
	spec.RateEvents = []RateEvent{{Date: evDate(2031, 1, 15), AnnualRate: 6.0, Action: "re_amortize"}}
	prepay := &Prepayment{Amount: 25000.00, Date: evDate(2032, 1, 15)} // the 73rd payment date
	comp, err := WhatIf(spec, Scenario{
		Renewal:           &Renewal{Date: evDate(2031, 1, 15), AnnualRate: 5.0, RateType: "variable"},
		OneTimePrepayment: prepay,
	})
	if err != nil {
		t.Fatalf("WhatIf: %v", err)
	}
	want := refSpec()
	want.RateEvents = []RateEvent{{Date: evDate(2031, 1, 15), AnnualRate: 5.0, RateType: "variable", Action: "re_amortize"}}
	wantSch, err := runSim(want, prepay)
	if err != nil {
		t.Fatalf("runSim(want): %v", err)
	}
	if !sameSchedule(comp.Schedule, wantSch) {
		t.Fatalf("scenario schedule differs from the renewal + prepayment simulation")
	}
	alone, err := WhatIf(spec, Scenario{Renewal: &Renewal{Date: evDate(2031, 1, 15), AnnualRate: 5.0, RateType: "variable"}})
	if err != nil {
		t.Fatalf("WhatIf(alone): %v", err)
	}
	if comp.Schedule.PayoffIndex >= alone.Schedule.PayoffIndex {
		t.Fatalf("combined PayoffIndex = %d, want < renewal-alone %d (the prepayment pulls it in)",
			comp.Schedule.PayoffIndex, alone.Schedule.PayoffIndex)
	}
}

// TestRenewalValidation pins the scenario-level validation of a Renewal:
// the composed spec must satisfy the same rate-event rules as Generate.
func TestRenewalValidation(t *testing.T) {
	spec := refSpec()
	for _, rate := range []float64{0, 101} {
		_, err := WhatIf(spec, Scenario{Renewal: &Renewal{Date: evDate(2031, 1, 15), AnnualRate: rate}})
		if err == nil || err.Error() != "rate event annual rate must be between 0 and 100" {
			t.Fatalf("WhatIf(rate %v): err = %v, want the event rate error", rate, err)
		}
	}
	_, err := WhatIf(spec, Scenario{Renewal: &Renewal{Date: spec.StartDate, AnnualRate: 5}})
	if err == nil || err.Error() != "rate event date must be after the start date" {
		t.Fatalf("WhatIf(date on start): err = %v, want the event date error", err)
	}
}

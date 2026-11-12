// Mortgage API (plan 7.3): the 7.1 mortgage list/create endpoints and the
// 7.3 amortization + what-if endpoints, which serve the 7.2 engine's
// schedule as canonical 2-decimal strings.
//
// Success bodies are the raw values the backend returns (no envelope);
// non-2xx responses are the 3.1 envelope {error: {code, message}} and
// surface as ApiError (see client.ts).

import { api } from './client';

/** One stored rate change (round 23 contract): the rate and its type
 * from the effective date on, and what happens to the payment.
 * Served strictly ascending by effective_date; the rate is a
 * canonical 2-decimal string. */
export interface MortgageRateEvent {
	effective_date: string;
	annual_rate: string;
	rate_type: string;
	action: string;
}

/** A mortgage row (backend mortgageOut). The amounts and the rate are
 * canonical 2-decimal strings; years is a number. The round-23 term
 * fields are appended last, matching the wire order. */
export interface Mortgage {
	id: string;
	account_id: string;
	principal: string;
	annual_rate: string;
	amortization_years: number;
	start_date: string;
	payment_frequency: string;
	extra_payment_amount: string;
	extra_payment_frequency: string;
	extra_prepayment_type: string;
	/** 0 = no renewable term. */
	term_years: number;
	/** 'fixed' | 'variable'; legacy rows serve '' (render as fixed). */
	rate_type: string;
	/** Strictly ascending rate changes; always [] (never null). */
	rate_events: MortgageRateEvent[];
}

/** The 9-field create body (POST /v1/mortgages, 7.1). */
export interface MortgageCreate {
	account_id: string;
	principal: string;
	annual_rate: string;
	amortization_years: number;
	start_date: string;
	payment_frequency: string;
	extra_payment_amount: string;
	extra_payment_frequency: string;
	extra_prepayment_type: string;
	/** Optional term metadata (defaults 0 / 'fixed'); client-sent
	 * rate_events are silently ignored by the backend on create. */
	term_years?: number;
	rate_type?: string;
}

/** One payment of the amortization schedule; every amount is a
 * canonical 2-decimal string. */
export interface AmortizationPayment {
	date: string;
	payment: string;
	interest: string;
	principal: string;
	extra: string;
	balance: string;
}

/** The stored spec's full amortization schedule
 * (GET /v1/mortgages/{id}/amortization, 7.3). */
export interface AmortizationSchedule {
	base_payment: string;
	payoff_date: string;
	payoff_index: number;
	total_paid: string;
	total_interest: string;
	payments: AmortizationPayment[];
	/** Present only when the generating spec has a term
	 * (term_years > 0); the next scheduled renewal date. */
	next_renewal?: string;
}

/** A what-if request: every field optional, at least one required (the
 * backend 400s an empty object). */
export interface WhatIfRequest {
	annual_rate?: string;
	extra_payment_amount?: string;
	extra_payment_frequency?: string;
	one_time_prepayment?: { amount: string; date: string };
	/** Simulate a different payment frequency
	 * (monthly/semi_monthly/biweekly). */
	payment_frequency?: string;
	/** Simulate a renewal at the date with the rate; supersedes stored
	 * events dated on/after it. */
	renewal?: { date: string; annual_rate: string; rate_type?: string };
}

/** A what-if result (POST /v1/mortgages/{id}/amortization/what-if,
 * 7.3). interest_saved may be negative (the scenario costs more). */
export interface WhatIfResult {
	payoff_date: string;
	total_interest: string;
	months_saved: number;
	interest_saved: string;
	schedule: AmortizationSchedule;
}

/** The mortgage list (7.1 endpoint). */
export function listMortgages(): Promise<Mortgage[]> {
	return api<Mortgage[]>('/v1/mortgages');
}

/** Create a mortgage (7.1 endpoint). */
export function createMortgage(body: MortgageCreate): Promise<Mortgage> {
	return api<Mortgage>('/v1/mortgages', { method: 'POST', body });
}

/** The patch body (round 23): every field optional; absent = untouched,
 * present = full replacement. A non-null rate_events replaces the WHOLE
 * array ([] clears it); at least one field is required. */
export interface MortgagePatch {
	principal?: string;
	annual_rate?: string;
	amortization_years?: number;
	start_date?: string;
	payment_frequency?: string;
	extra_payment_amount?: string;
	extra_payment_frequency?: string;
	extra_prepayment_type?: string;
	term_years?: number;
	rate_type?: string;
	rate_events?: MortgageRateEvent[];
}

/** Patch a mortgage (round 23 endpoint). */
export function patchMortgage(id: string, body: MortgagePatch): Promise<Mortgage> {
	return api<Mortgage>(`/v1/mortgages/${id}`, { method: 'PATCH', body });
}

/** The stored spec's full amortization schedule (7.3 endpoint). */
export function getMortgageAmortization(id: string): Promise<AmortizationSchedule> {
	return api<AmortizationSchedule>(`/v1/mortgages/${id}/amortization`);
}

/** Compare the stored spec against a what-if scenario (7.3 endpoint). */
export function whatIfMortgageAmortization(id: string, body: WhatIfRequest): Promise<WhatIfResult> {
	return api<WhatIfResult>(`/v1/mortgages/${id}/amortization/what-if`, { method: 'POST', body });
}

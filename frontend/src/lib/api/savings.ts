// Savings API (round 53): the registered-plan room view, limit
// overrides, room adjustments, balance snapshots and the equity series
// behind the appreciation view.
//
// Success bodies are the raw values the backend returns (no envelope);
// non-2xx responses are the 3.1 envelope {error: {code, message}} and
// surface as ApiError (see client.ts). All money fields are canonical
// 2-decimal strings; dates are "YYYY-MM-DD".

import { api } from './client';

/** The six plan types (backend plans.AllPlanTypes, in that order). */
export const PLAN_TYPES = ['rrsp', 'tfsa', 'fhsa', 'rrif', 'resp', 'rdsp'] as const;

/** A plan type (one of PLAN_TYPES). */
export type PlanType = (typeof PLAN_TYPES)[number];

// --- Room (GET /v1/savings/room) ---

/** One plan card in the room view. */
export interface RoomCard {
	plan_type: string;
	/** "" when the plan carries no beneficiary (RESP/RDSP only). */
	beneficiary: string;
	annual_limit: string | null;
	/** "statutory" | "override" | "none". */
	limit_source: 'statutory' | 'override' | 'none';
	/** Counted contributions in the year. */
	used: string;
	room: string | null;
	/** tfsa/rrsp withdrawals in the year that re-create room next year. */
	re_created_next_year: string;
	lifetime_limit: string | null;
	lifetime_used: string | null;
	lifetime_room: string | null;
	rrif_minimum: string | null;
	/** RRIF minimum-withdrawal factor in basis points. */
	rrif_factor_bps: number | null;
	rrif_age: number | null;
	/** Engine warnings/notes — always arrays, never null. */
	warnings: string[];
	notes: string[];
}

/** The GET /v1/savings/room response. */
export interface RoomView {
	year: number;
	birth_year: number | null;
	cards: RoomCard[];
	registered_total: string;
	investment_total: string;
}

/** The room view for a year (1900..2100; the backend defaults to the
 * current year when omitted). */
export function getRoom(year?: number): Promise<RoomView> {
	const suffix = year === undefined ? '' : `?year=${year}`;
	return api<RoomView>(`/v1/savings/room${suffix}`);
}

// --- Birth year (PATCH /v1/me/birth-year) ---

/** Sets the user's birth year ("YYYY"); "" clears the stored value.
 * The response echoes the stored year (null when cleared). */
export function setBirthYear(birthYear: string): Promise<{ birth_year: number | null }> {
	return api<{ birth_year: number | null }>('/v1/me/birth-year', {
		method: 'PATCH',
		body: { birth_year: birthYear }
	});
}

// --- Limit overrides (*/v1/savings/limits) ---

/** A per-year limit override row. */
export interface LimitOverride {
	id: string;
	plan_type: string;
	year: number;
	annual_limit: string;
	created_at: string;
	updated_at: string;
}

/** Lists overrides; plan_type and year are optional filters (both
 * omitted = all overrides). */
export function listLimits(planType?: string, year?: number): Promise<LimitOverride[]> {
	const q = new URLSearchParams();
	if (planType) q.set('plan_type', planType);
	if (year !== undefined) q.set('year', String(year));
	const suffix = q.toString() ? `?${q.toString()}` : '';
	return api<LimitOverride[]>(`/v1/savings/limits${suffix}`);
}

/** Creates or replaces the override for (plan_type, year) — rrsp/tfsa/
 * fhsa only. 200 with the stored row. */
export function putLimit(input: {
	plan_type: string;
	year: number;
	annual_limit: string;
}): Promise<LimitOverride> {
	return api<LimitOverride>('/v1/savings/limits', { method: 'PUT', body: input });
}

/** Deletes an override (204 → resolves). */
export function deleteLimit(id: string): Promise<void> {
	return api<void>(`/v1/savings/limits/${id}`, { method: 'DELETE' });
}

// --- Room adjustments (*/v1/savings/adjustments) ---

/** A manual room-adjustment row. */
export interface RoomAdjustment {
	id: string;
	plan_type: string;
	year: number;
	amount: string;
	note: string;
	created_at: string;
}

/** Lists adjustments; plan_type and year are optional filters (both
 * omitted = all adjustments). */
export function listAdjustments(planType?: string, year?: number): Promise<RoomAdjustment[]> {
	const q = new URLSearchParams();
	if (planType) q.set('plan_type', planType);
	if (year !== undefined) q.set('year', String(year));
	const suffix = q.toString() ? `?${q.toString()}` : '';
	return api<RoomAdjustment[]>(`/v1/savings/adjustments${suffix}`);
}

/** Adds an adjustment (non-zero amount, note at most 500 chars). 201
 * with the stored row. */
export function createAdjustment(input: {
	plan_type: string;
	year: number;
	amount: string;
	note: string;
}): Promise<RoomAdjustment> {
	return api<RoomAdjustment>('/v1/savings/adjustments', { method: 'POST', body: input });
}

/** Deletes an adjustment (204 → resolves). */
export function deleteAdjustment(id: string): Promise<void> {
	return api<void>(`/v1/savings/adjustments/${id}`, { method: 'DELETE' });
}

// --- Balance snapshots (*/v1/savings/snapshots) ---

/** A manual balance snapshot for one account on one date. */
export interface Snapshot {
	id: string;
	account_id: string;
	as_of: string;
	balance: string;
	note: string;
	created_at: string;
	updated_at: string;
}

/** Lists an account's snapshots, newest first (account_id required). */
export function listSnapshots(accountId: string): Promise<Snapshot[]> {
	return api<Snapshot[]>(`/v1/savings/snapshots?account_id=${accountId}`);
}

/** Adds a snapshot (as_of on or before today, balance at least 0.00).
 * 201 with the stored row; 409 when a snapshot already exists for the
 * account on that date. */
export function createSnapshot(input: {
	account_id: string;
	as_of: string;
	balance: string;
	note: string;
}): Promise<Snapshot> {
	return api<Snapshot>('/v1/savings/snapshots', { method: 'POST', body: input });
}

/** Patches a snapshot. balance and note are tri-state: absent = no-op,
 * null = clear, string = set (a null balance is a 400). 200 with the
 * updated row. */
export function patchSnapshot(
	id: string,
	input: { balance?: string | null; note?: string | null }
): Promise<Snapshot> {
	return api<Snapshot>(`/v1/savings/snapshots/${id}`, { method: 'PATCH', body: input });
}

/** Deletes a snapshot (204 → resolves). */
export function deleteSnapshot(id: string): Promise<void> {
	return api<void>(`/v1/savings/snapshots/${id}`, { method: 'DELETE' });
}

// --- Equity series (GET /v1/savings/equity) ---

/** One point of the appreciation series. */
export interface EquityPoint {
	as_of: string;
	balance: string;
	/** Net counted contributions up to and including this point (may
	 * be negative). */
	net_contributions: string;
}

/** The equity series for an account: snapshot balances with the running
 * balance between them. from/to are "YYYY-MM-DD"; defaults: to = today,
 * from = to minus 10 years. */
export function getEquity(
	accountId: string,
	from?: string,
	to?: string
): Promise<{ points: EquityPoint[] }> {
	const q = new URLSearchParams({ account_id: accountId });
	if (from) q.set('from', from);
	if (to) q.set('to', to);
	return api<{ points: EquityPoint[] }>(`/v1/savings/equity?${q.toString()}`);
}

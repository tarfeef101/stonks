// Account API (step 3.1): the account list for the upload picker and
// the manual-entry form (4.7), plus the CRUD the settings page manages
// (round 4).
//
// Success bodies are the raw values the backend returns (no envelope);
// non-2xx responses are the 3.1 envelope {error: {code, message}} and
// surface as ApiError (see client.ts).

import { api } from './client';

/** An account row (backend accountOut). */
export interface Account {
	id: string;
	name: string;
	type: string;
	subtype: string | null;
	currency: string;
	default_schema_id: string | null;
	beneficiary_name: string | null;
	beneficiary_birth_year: number | null;
	/** Canonical 2-decimal string. */
	opening_balance: string;
	/** "YYYY-MM-DD" or null. */
	opened_at: string | null;
}

/** The 12 account types (the repo.AccountTypes vocabulary; the backend
 * rejects anything else with a 400). The last six are the registered
 * plan types (round 53). */
export const ACCOUNT_TYPES = [
	'bank',
	'cash',
	'credit_card',
	'mortgage',
	'investment',
	'other',
	'rrsp',
	'tfsa',
	'fhsa',
	'rrif',
	'resp',
	'rdsp'
] as const;

/** An account type (one of ACCOUNT_TYPES). */
export type AccountType = (typeof ACCOUNT_TYPES)[number];

/** The account list (3.1 endpoint). */
export function listAccounts(): Promise<Account[]> {
	return api<Account[]>('/v1/accounts');
}

/** Creates an account (201 → the new row). `currency` is required (the
 * backend has no default); `subtype` is optional — omit it to leave the
 * row without one. A duplicate name is a 409. */
export function createAccount(input: {
	name: string;
	type: string;
	currency: string;
	subtype?: string;
	beneficiary_name?: string;
	beneficiary_birth_year?: string;
	opening_balance?: string;
	opened_at?: string;
}): Promise<Account> {
	// JSON.stringify omits undefined optionals, so a create without them
	// is just {name, type, currency} on the wire.
	return api<Account>('/v1/accounts', { method: 'POST', body: input });
}

/** PATCH body. `subtype` is tri-state: `undefined` (omitted) leaves the
 * subtype untouched, `null` clears it, a string sets it. At least one
 * field must be present (an empty patch is a 400). */
export interface AccountPatch {
	name?: string;
	type?: string;
	subtype?: string | null;
	currency?: string;
	/** Round 53 tri-state fields: absent = no-op, null = clear. */
	beneficiary_name?: string | null;
	beneficiary_birth_year?: string | null;
	opening_balance?: string | null;
	opened_at?: string | null;
}

/** Updates an account (200 → the updated row). 404 when unknown, 409 on a
 * name conflict. */
export function patchAccount(id: string, patch: AccountPatch): Promise<Account> {
	return api<Account>(`/v1/accounts/${id}`, { method: 'PATCH', body: patch });
}

/** Deletes an account (204 → undefined). Its transactions, import jobs,
 * and mortgages are removed (FKs ON DELETE CASCADE). 404 when unknown. */
export function deleteAccount(id: string): Promise<void> {
	return api<void>(`/v1/accounts/${id}`, { method: 'DELETE' });
}

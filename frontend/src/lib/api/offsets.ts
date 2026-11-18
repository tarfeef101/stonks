// Offset API (migration 0012): transactions that net each other out.
// An internal movement between the caller's own accounts (paying a
// credit card from chequing) must not count as spend in one account
// and income in the other: the scan endpoint proposes net-to-zero
// pairs, and the suggestions/groups endpoints manage their lifecycle.
//
// Success bodies are the raw values the backend returns (no envelope);
// non-2xx responses are the 3.1 envelope {error: {code, message}} and
// surface as ApiError (see client.ts).

import { api, pagingQuery } from './client';
import type { Transaction } from './transactions';

/** A proposed offset pair (backend offsetSuggestionOut): two rows with
 * exactly mirrored amounts and close posting dates, both ungrouped.
 * created_at is RFC3339. */
export interface OffsetSuggestion {
	id: string;
	a: Transaction;
	b: Transaction;
	created_at: string;
}

/** An active offset group (backend offsetGroupOut): the member rows
 * whose amounts sum to exactly zero, in (date_posted, id) order. */
export interface OffsetGroup {
	id: string;
	members: Transaction[];
}

/** Runs the matcher (POST /v1/offsets/scan). Optional params: from/to
 * (YYYY-MM-DD — a pair qualifies when at least one leg is in [from,
 * to]) and window_days (1-60, the posting-date proximity; the
 * backend default is 7). The scan is idempotent — pairs that already
 * have a suggestion in any status are skipped — so the response is
 * how many NEW pairs were proposed. */
export function scanOffsets(from?: string, to?: string, windowDays?: number): Promise<{ suggested: number }> {
	const q = new URLSearchParams();
	if (from) q.set('from', from);
	if (to) q.set('to', to);
	if (windowDays !== undefined) q.set('window_days', String(windowDays));
	const suffix = q.toString() ? `?${q.toString()}` : '';
	return api<{ suggested: number }>(`/v1/offsets/scan${suffix}`, { method: 'POST' });
}

/** The pending suggestions (GET /v1/offsets/suggestions), oldest
 * first, both legs opened. Optional limit/offset (backend defaults
 * 100/0). */
export function listOffsetSuggestions(limit?: number, offset?: number): Promise<OffsetSuggestion[]> {
	return api<OffsetSuggestion[]>(`/v1/offsets/suggestions${pagingQuery(limit, offset)}`);
}

/** Turns one pending suggestion into a group (POST
 * /v1/offsets/suggestions/{id}/accept — the response is
 * {group_id}). A suggestion that was already resolved answers 404. */
export function acceptOffsetSuggestion(id: string): Promise<{ group_id: string }> {
	return api<{ group_id: string }>(`/v1/offsets/suggestions/${id}/accept`, { method: 'POST' });
}

/** Drops one pending suggestion out of future scans (POST
 * /v1/offsets/suggestions/{id}/dismiss — 204, no body). Dismissals
 * stick until the pair is re-opened by clearing a group. */
export function dismissOffsetSuggestion(id: string): Promise<void> {
	return api<void>(`/v1/offsets/suggestions/${id}/dismiss`, { method: 'POST' });
}

/** Hand-groups a set of the caller's rows (POST /v1/offsets/groups):
 * at least two distinct ids (400 when not) whose amounts must net to
 * exactly zero (400 when not). The response is {group_id}. */
export function createOffsetGroup(transactionIds: string[]): Promise<{ group_id: string }> {
	return api<{ group_id: string }>('/v1/offsets/groups', {
		method: 'POST',
		body: { transaction_ids: transactionIds }
	});
}

/** The active groups (GET /v1/offsets/groups). Optional limit/offset
 * (backend defaults 100/0). */
export function listOffsetGroups(limit?: number, offset?: number): Promise<OffsetGroup[]> {
	return api<OffsetGroup[]>(`/v1/offsets/groups${pagingQuery(limit, offset)}`);
}

/** Dissolves one group (DELETE /v1/offsets/groups/{id} — 204, no
 * body): the members go back to visible in the lists and the
 * aggregates, and the suggestions touching them are deleted. */
export function clearOffsetGroup(id: string): Promise<void> {
	return api<void>(`/v1/offsets/groups/${id}`, { method: 'DELETE' });
}

// Dashboard cards API (plan step 6.5, the 6.5 routes): the caller's saved
// Explore views — a name plus the canonical explore_state query string
// ("" = the default all-time view), DEK-encrypted at rest by the backend.
//
// Success bodies are the raw values the backend returns (no envelope);
// non-2xx responses are the 3.1 envelope {error: {code, message}} and
// surface as ApiError (see client.ts).

import { api } from './client';

/** One dashboard card row (backend cardOut). ExploreState is "" for the
 * default (all-time) view. No timestamps on the wire (3.1 convention). */
export interface DashboardCard {
	id: string;
	name: string;
	explore_state: string;
	sort_order: number;
}

/** The caller's cards (GET /v1/dashboard-cards), in sort_order order. An
 * empty set is []. */
export function listDashboardCards(): Promise<DashboardCard[]> {
	return api<DashboardCard[]>('/v1/dashboard-cards');
}

/** Creates a card (POST /v1/dashboard-cards — 201 + the created row).
 * explore_state is the canonical serializeState() output; "" is the
 * default (all-time) view. */
export function createDashboardCard(name: string, exploreState: string): Promise<DashboardCard> {
	return api<DashboardCard>('/v1/dashboard-cards', {
		method: 'POST',
		body: { name, explore_state: exploreState }
	});
}

/** Fetches one card (GET /v1/dashboard-cards/{id} — 200 + the row). A
 * card the caller does not have answers 404. */
export function getDashboardCard(id: string): Promise<DashboardCard> {
	return api<DashboardCard>(`/v1/dashboard-cards/${id}`);
}

/** Patches a card (PATCH /v1/dashboard-cards/{id} — 200 + the updated
 * row). Only the fields present in the patch are changed. */
export function patchDashboardCard(
	id: string,
	p: { name?: string; explore_state?: string }
): Promise<DashboardCard> {
	return api<DashboardCard>(`/v1/dashboard-cards/${id}`, { method: 'PATCH', body: p });
}

/** Deletes a card (DELETE /v1/dashboard-cards/{id} — 204 → resolves). A
 * card the caller does not have answers 404. */
export function deleteDashboardCard(id: string): Promise<void> {
	return api<void>(`/v1/dashboard-cards/${id}`, { method: 'DELETE' });
}

/** Reorders the caller's cards (PUT /v1/dashboard-cards/order — 204).
 * ids is a BARE JSON array of every card uuid, in the new order (each
 * exactly once); the new sort_order of ids[i] is i+1. The array rides
 * the client's JSON path (client.ts stringifies any non-string,
 * non-FormData body), which emits it as a bare array — no object
 * wrapper. */
export function setDashboardCardOrder(ids: string[]): Promise<void> {
	return api<void>('/v1/dashboard-cards/order', { method: 'PUT', body: ids });
}

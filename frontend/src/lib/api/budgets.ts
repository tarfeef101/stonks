// Budgets API (plan step 6.3, the 3.3 routes): the caller's budgets —
// a name, a period (fixed at creation), an active flag, and a targets
// document (category id → JSON number) the server stores and returns
// as the caller's raw wire text (send 60.00, 60.00 comes back).
//
// Success bodies are the raw values the backend returns (no envelope);
// non-2xx responses are the 3.1 envelope {error: {code, message}} and
// surface as ApiError (see client.ts).

import { api } from './client';
import type { Period } from './summary';

/** One budget row (backend budgetOut). Targets = the raw wire document
 * (category id → JSON number). No timestamps on the wire (3.1
 * convention). */
export interface Budget {
	id: string;
	name: string;
	period: Period;
	active: boolean;
	targets: Record<string, number>;
}

/** The caller's budgets (GET /v1/budgets), in creation order. An empty
 * set is []. */
export function listBudgets(): Promise<Budget[]> {
	return api<Budget[]>('/v1/budgets');
}

/** Creates a budget (POST /v1/budgets — 201 + the created row). Target
 * values must be JSON numbers; an empty targets document is a 400. */
export function createBudget(input: {
	name: string;
	period: Period;
	active: boolean;
	targets: Record<string, number>;
}): Promise<Budget> {
	return api<Budget>('/v1/budgets', { method: 'POST', body: input });
}

/** Patches a budget (PATCH /v1/budgets/{id} — 200 + the updated row).
 * Only the fields present in the patch are changed. There is
 * deliberately no period field — the period is fixed at creation. */
export function updateBudget(
	id: string,
	patch: { name?: string; active?: boolean; targets?: Record<string, number> }
): Promise<Budget> {
	return api<Budget>(`/v1/budgets/${id}`, { method: 'PATCH', body: patch });
}

/** Deletes a budget (DELETE /v1/budgets/{id} — 204 → resolves). A
 * budget the caller does not have answers 404. */
export function deleteBudget(id: string): Promise<void> {
	return api<void>(`/v1/budgets/${id}`, { method: 'DELETE' });
}

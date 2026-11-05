// Review queue API (step 5.5): the transactions the categorise worker
// flagged (a suggestion under the confidence threshold) that the user
// resolves with one of their own categories.
//
// Success bodies are the raw values the backend returns (no envelope);
// non-2xx responses are the 3.1 envelope {error: {code, message}} and
// surface as ApiError (see client.ts).

import { api } from './client';
import type { Transaction } from './transactions';

/** A review queue row (backend reviewOut): kept minimal — the category
 * picker is filled from GET /v1/categories. Amount is the canonical
 * 2-decimal form and date_posted is "YYYY-MM-DD". (Named for the queue
 * so it does not clash with the ReviewRow component.) */
export interface ReviewQueueRow {
	id: string;
	account_id: string;
	date_posted: string;
	amount: string;
	description: string;
}

/** The reprompt response (backend repromptOut): the model's answer for
 * the one row and whether it was applied (confidence at or above the
 * threshold resolves the row). */
export interface RepromptResult {
	category: string;
	confidence: number;
	rationale: string;
	applied: boolean;
}

/** The review queue (GET /v1/review): every flagged row that still has
 * no category, oldest id first. */
export function listReviewQueue(): Promise<ReviewQueueRow[]> {
	return api<ReviewQueueRow[]>('/v1/review');
}

/** Resolves one row with one of the caller's categories (POST
 * /v1/review/{id}/accept — 200 + the full transaction row). A
 * category that is not the caller's answers 400 (invalid_request);
 * a row that is not in the caller's queue answers 404. */
export function acceptReview(id: string, categoryId: string): Promise<Transaction> {
	return api<Transaction>(`/v1/review/${id}/accept`, {
		method: 'POST',
		body: { category_id: categoryId }
	});
}

/** Resolves every row in the queue with one category (POST
 * /v1/review/accept-all — the response is {applied: N}). */
export function acceptAllReview(categoryId: string): Promise<{ applied: number }> {
	return api<{ applied: number }>('/v1/review/accept-all', {
		method: 'POST',
		body: { category_id: categoryId }
	});
}

/** Re-runs the LLM on one row (POST /v1/review/{id}/reprompt). A
 * suggestion at or above the threshold is applied; the response always
 * carries the model's answer. */
export function repromptReview(id: string): Promise<RepromptResult> {
	return api<RepromptResult>(`/v1/review/${id}/reprompt`, { method: 'POST' });
}

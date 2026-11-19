// Transaction API (step 3.1, created from the manual-entry form in
// 4.7): POST /v1/transactions; the filtered list (step 3.2) since 6.4.
//
// Success bodies are the raw values the backend returns (no envelope);
// non-2xx responses are the 3.1 envelope {error: {code, message}} and
// surface as ApiError (see client.ts).

import { api } from './client';

/** A transaction row (backend transactionOut): the date columns are
 * "YYYY-MM-DD" strings and the unset optionals (date_executed,
 * category_id, notes, raw) are JSON null. Amount is the canonical
 * 2-decimal form. */
export interface Transaction {
	id: string;
	account_id: string;
	date_executed: string | null;
	date_posted: string;
	amount: string;
	currency: string;
	/** The amount in the user's default currency — populated in list
	 * responses only; "" when the backend did not convert the row. */
	amount_converted: string;
	converted_currency: string;
	category_id: string | null;
	description: string;
	notes: string | null;
	raw: string | null;
	review_pending: boolean;
	transfer_group_id: string | null;
	external_origin: string | null;
}

/** The POST /v1/transactions body. Absent optionals are OMITTED from
 * the JSON — the client never sends null or '' for them. Amount follows
 * the server's canonical grammar (optional sign, digits, <= 2 fractional
 * digits); a bad amount is a server 400, surfaced via the ApiError
 * message. */
export interface CreateTransactionInput {
	account_id: string;
	amount: string;
	description?: string;
	/** Set only when a category is picked; a supplied category skips the
	 * later auto-categorization phase for the row. */
	category_id?: string;
	/** YYYY-MM-DD. */
	date_posted: string;
	/** YYYY-MM-DD; must not be after date_posted. */
	date_executed?: string;
	notes?: string;
	/** Present, the POST creates a transfer: both legs are stored and
	 * the response is {from, to} instead of a single row. */
	transfer_from_account_id?: string;
	/** Where an external contribution came from. Requires a positive
	 * amount; not allowed on a transfer. */
	external_origin?: 'employer' | 'government_grant' | 'inherited' | 'other';
}

/** Creates a transaction. When transfer_from_account_id is present the
 * backend creates both transfer legs and answers {from, to} instead of
 * a single row. */
export function createTransaction(
	input: CreateTransactionInput
): Promise<Transaction | { from: Transaction; to: Transaction }> {
	return api<Transaction | { from: Transaction; to: Transaction }>('/v1/transactions', {
		method: 'POST',
		body: input
	});
}

/** List filters for GET /v1/transactions (step 3.2), AND-combined on the
 * server. Absent (undefined) params are omitted from the query; an
 * EMPTY `category_id` value means "uncategorized only". */
export interface ListParams {
	/** Inclusive lower bound on date_posted (YYYY-MM-DD). */
	date_from?: string;
	/** Inclusive upper bound on date_posted (YYYY-MM-DD). */
	date_to?: string;
	/** Repeatable account filter (sent once per id). */
	account_id?: string[];
	/** Repeatable category filter (sent once per value; '' =
	 * uncategorized rows only). */
	category_id?: string[];
	min_amount?: string;
	max_amount?: string;
	/** Case-insensitive description substring. */
	search?: string;
	/** 'spending' = negative amounts only; 'income' = positive amounts only. */
	sign?: string;
	limit?: number;
	offset?: number;
}

/** The transaction list (step 3.2): rows in date_posted DESC, id DESC
 * order. The server caps `limit` at 100 silently; an empty match returns
 * `[]`. */
export function listTransactions(params: ListParams = {}): Promise<Transaction[]> {
	const q = new URLSearchParams();
	if (params.date_from !== undefined) q.append('date_from', params.date_from);
	if (params.date_to !== undefined) q.append('date_to', params.date_to);
	for (const id of params.account_id ?? []) q.append('account_id', id);
	for (const id of params.category_id ?? []) q.append('category_id', id);
	if (params.min_amount !== undefined) q.append('min_amount', params.min_amount);
	if (params.max_amount !== undefined) q.append('max_amount', params.max_amount);
	if (params.search !== undefined) q.append('search', params.search);
	if (params.sign !== undefined) q.append('sign', params.sign);
	if (params.limit !== undefined) q.append('limit', String(params.limit));
	if (params.offset !== undefined) q.append('offset', String(params.offset));
	const qs = q.toString();
	return api<Transaction[]>(`/v1/transactions${qs ? `?${qs}` : ''}`);
}

/** The PATCH /v1/transactions/{id} body. Absent keys are left
 * untouched by the server; an empty-string optional clears the column.
 * Amount follows the same canonical grammar as creation. A transaction
 * that is not the caller's answers 404. */
export interface PatchTransactionInput {
	account_id?: string;
	amount?: string;
	description?: string;
	/** '' clears the category (re-flags the row for review). */
	category_id?: string;
	/** YYYY-MM-DD; '' clears the column. */
	date_posted?: string;
	/** YYYY-MM-DD; '' clears the column. */
	date_executed?: string;
	notes?: string;
}

/** Edits one of the caller's transactions (PATCH /v1/transactions/{id}
 * — 200 + the updated row). Setting `category_id` to '' also re-flags
 * the row `review_pending` so the review queue picks it back up. */
export function patchTransaction(id: string, patch: PatchTransactionInput): Promise<Transaction> {
	return api<Transaction>(`/v1/transactions/${id}`, { method: 'PATCH', body: patch });
}

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
	category_id: string | null;
	description: string;
	notes: string | null;
	raw: string | null;
	review_pending: boolean;
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
}

/** Creates a transaction (201 + the row). A row with the same dedup key
 * (account, posted date, amount, normalized description) answers 409
 * (conflict). */
export function createTransaction(input: CreateTransactionInput): Promise<Transaction> {
	return api<Transaction>('/v1/transactions', { method: 'POST', body: input });
}

/** List filters for GET /v1/transactions (step 3.2), AND-combined on the
 * server. Absent (undefined) params are omitted from the query; an empty
 * `category_id` means "uncategorized only". */
export interface ListParams {
	/** Inclusive lower bound on date_posted (YYYY-MM-DD). */
	date_from?: string;
	/** Inclusive upper bound on date_posted (YYYY-MM-DD). */
	date_to?: string;
	/** Repeatable account filter (sent once per id). */
	account_id?: string[];
	/** A category id, or '' for uncategorized rows only. */
	category_id?: string;
	min_amount?: string;
	max_amount?: string;
	/** Case-insensitive description substring. */
	search?: string;
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
	if (params.category_id !== undefined) q.append('category_id', params.category_id);
	if (params.min_amount !== undefined) q.append('min_amount', params.min_amount);
	if (params.max_amount !== undefined) q.append('max_amount', params.max_amount);
	if (params.search !== undefined) q.append('search', params.search);
	if (params.limit !== undefined) q.append('limit', String(params.limit));
	if (params.offset !== undefined) q.append('offset', String(params.offset));
	const qs = q.toString();
	return api<Transaction[]>(`/v1/transactions${qs ? `?${qs}` : ''}`);
}

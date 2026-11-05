// Transaction API (step 3.1, created from the manual-entry form in
// 4.7): POST /v1/transactions.
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

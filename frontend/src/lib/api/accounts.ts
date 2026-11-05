// Account API (step 3.1): the account list for the upload picker and
// the manual-entry form (4.7).
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
}

/** The account list (3.1 endpoint). */
export function listAccounts(): Promise<Account[]> {
	return api<Account[]>('/v1/accounts');
}

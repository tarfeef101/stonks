// Category API (step 3.1): the caller's categories for the manual-entry
// form's category picker (4.7).
//
// Success bodies are the raw values the backend returns (no envelope);
// non-2xx responses are the 3.1 envelope {error: {code, message}} and
// surface as ApiError (see client.ts).

import { api } from './client';

/** A category row (backend categoryOut); parent_id is null for a root
 * category. */
export interface Category {
	id: string;
	name: string;
	parent_id: string | null;
	active: boolean;
	sort_order: number;
}

/** The caller's categories (3.1 endpoint), ordered by (sort_order,
 * created_at). */
export function listCategories(): Promise<Category[]> {
	return api<Category[]>('/v1/categories');
}

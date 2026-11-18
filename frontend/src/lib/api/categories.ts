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

/** Creates a category (201 → the new row). `parent_id` may be omitted or
 * a category id; omitting it creates a root. A duplicate name is a 409. */
export function createCategory(input: { name: string; parent_id?: string }): Promise<Category> {
	// JSON.stringify omits the undefined parent_id, so a root create is
	// just {name} on the wire.
	return api<Category>('/v1/categories', { method: 'POST', body: input });
}

/** PATCH body. `parent_id` is tri-state: `undefined` (omitted) leaves the
 * parent untouched, `null` demotes to root, a string re-parents. At least
 * one field must be present (an empty patch is a 400). */
export interface CategoryPatch {
	name?: string;
	parent_id?: string | null;
	active?: boolean;
}

/** Updates a category (200 → the updated row). 404 when unknown, 409 on a
 * name conflict. */
export function patchCategory(id: string, patch: CategoryPatch): Promise<Category> {
	return api<Category>(`/v1/categories/${id}`, { method: 'PATCH', body: patch });
}

/** Deletes a category (204 → undefined). Transactions in it are kept —
 * the FK is ON DELETE SET NULL. 404 when unknown. */
export function deleteCategory(id: string): Promise<void> {
	return api<void>(`/v1/categories/${id}`, { method: 'DELETE' });
}

/** Seeds the caller's category vocabulary in one shot (onboarding,
 * D-031). Names the caller already has (exact-case) are skipped, so the
 * call is idempotent — a retry re-sends the same set harmlessly.
 * 200 → {created: n}. */
export function seedCategories(names: string[]): Promise<{ created: number }> {
	return api<{ created: number }>('/v1/categories/seed', { method: 'POST', body: { names } });
}

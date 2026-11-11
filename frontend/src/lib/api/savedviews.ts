// Saved Explore views API (round 16): the caller's named snapshots of
// the Explore page's URL state (state = the canonical serialized
// explore state, '' = the default view). The Explore view bar saves,
// opens, updates, renames and deletes them; the dashboard add-widget
// flow picks one and the widget stores its OWN copy of the state (a
// copy, not a reference — deleting a saved view never breaks a
// widget).
//
// Success bodies are the raw values the backend returns (no envelope);
// non-2xx responses are the 3.1 envelope {error: {code, message}} and
// surface as ApiError (see client.ts). There is no single-GET route
// and no unique name (the dashboards convention).

import { api } from './client';

/** One saved view row (backend savedViewOut). state = the canonical
 * serialized explore state, '' = the default view. No timestamps (3.1
 * convention). */
export interface SavedView {
	id: string;
	name: string;
	state: string;
}

/** The caller's saved views (GET /v1/saved-views), created_at asc
 * (id asc tiebreak). An empty set is []. */
export function listSavedViews(): Promise<SavedView[]> {
	return api<SavedView[]>('/v1/saved-views');
}

/** Creates a saved view (POST /v1/saved-views — 201 + the created
 * row). The name must be non-blank and at most 255 characters; the
 * state is at most 1024 characters ('' = the default view). */
export function createSavedView(input: { name: string; state: string }): Promise<SavedView> {
	return api<SavedView>('/v1/saved-views', { method: 'POST', body: input });
}

/** Renames and/or updates a saved view's state (PATCH
 * /v1/saved-views/{id} — 200 + the updated row). Only the fields
 * present are changed; an empty patch is a 400. A view the caller does
 * not have answers 404. */
export function updateSavedView(
	id: string,
	patch: { name?: string; state?: string }
): Promise<SavedView> {
	return api<SavedView>(`/v1/saved-views/${id}`, { method: 'PATCH', body: patch });
}

/** Deletes a saved view (DELETE /v1/saved-views/{id} — 204 →
 * resolves). Widgets store their own copy of the state, so this never
 * breaks a widget. A view the caller does not have answers 404. */
export function deleteSavedView(id: string): Promise<void> {
	return api<void>(`/v1/saved-views/${id}`, { method: 'DELETE' });
}

// Notification API (step 3.3, surfaced in 5.5): the list, mark-read,
// and unread-count endpoints behind the notifications page and the
// nav badge.
//
// Success bodies are the raw values the backend returns (no envelope);
// non-2xx responses are the 3.1 envelope {error: {code, message}} and
// surface as ApiError (see client.ts).

import { api } from './client';

/** A notification row (backend notificationOut). Ref is the raw
 * pointer document (JSON, null when unset); ReadAt is null while the
 * row is unread. There is no created_at — created_at is bookkeeping
 * and excluded by the 3.1 convention; the list order is already
 * newest first. */
export interface Notification {
	id: string;
	kind: string;
	title: string;
	body: string;
	ref: unknown;
	read_at: string | null;
}

/** The caller's notifications (GET /v1/notifications), newest first. */
export function listNotifications(): Promise<Notification[]> {
	return api<Notification[]>('/v1/notifications');
}

/** Marks a row read (POST /v1/notifications/{id}/read — 200 + the
 * updated row; idempotent, re-marking keeps the original read_at). */
export function markNotificationRead(id: string): Promise<Notification> {
	return api<Notification>(`/v1/notifications/${id}/read`, { method: 'POST' });
}

/** The unread count (GET /v1/notifications/unread-count) — the nav
 * badge's source. */
export function unreadNotificationCount(): Promise<{ unread: number }> {
	return api<{ unread: number }>('/v1/notifications/unread-count');
}

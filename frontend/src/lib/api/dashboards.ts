// Dashboards API (round 6): the caller's dashboards — named grids of
// widgets, each widget an embedded saved Explore view (content = the
// canonical serialized explore state) or a markdown text note — plus
// the 9 backend routes (the contract: docs/DASHBOARDS-REWORK.md).
//
// Success bodies are the raw values the backend returns (no envelope);
// non-2xx responses are the 3.1 envelope {error: {code, message}} and
// surface as ApiError (see client.ts).

import { api } from './client';

/** One dashboard row (backend dashboardOut). widget_count = how many
 * widgets the dashboard holds (the widgets are a separate endpoint). */
export interface Dashboard {
	id: string;
	name: string;
	widget_count: number;
}

/** Widget kinds: an embedded Explore view or a markdown text note. */
export type WidgetKind = 'explore' | 'markdown';

/** One widget row (backend widgetOut). content = the raw stored text
 * (explore: the canonical serialized explore state, '' = the default
 * view; markdown: the markdown source). span = 1..12 grid columns;
 * row_span = 1..8 fixed grid rows (round 7 — the editor's rows have a
 * fixed height, overflow scrolls inside the card); sort_order = the
 * 1-based position within the dashboard. */
export interface DashboardWidget {
	id: string;
	name: string;
	kind: WidgetKind;
	content: string;
	span: number;
	row_span: number;
	sort_order: number;
}

/** The caller's dashboards (GET /v1/dashboards), in creation order.
 * An empty set is []. */
export function listDashboards(): Promise<Dashboard[]> {
	return api<Dashboard[]>('/v1/dashboards');
}

/** Creates a dashboard (POST /v1/dashboards — 201 + the created row).
 * The name must be non-blank and at most 255 characters. */
export function createDashboard(name: string): Promise<Dashboard> {
	return api<Dashboard>('/v1/dashboards', { method: 'POST', body: { name } });
}

/** Renames a dashboard (PATCH /v1/dashboards/{id} — 200 + the updated
 * row). The patch is {name}; an empty patch is a 400. */
export function updateDashboard(id: string, patch: { name?: string }): Promise<Dashboard> {
	return api<Dashboard>(`/v1/dashboards/${id}`, { method: 'PATCH', body: patch });
}

/** Deletes a dashboard and its widgets (DELETE /v1/dashboards/{id} —
 * 204 → resolves). A dashboard the caller does not have answers 404. */
export function deleteDashboard(id: string): Promise<void> {
	return api<void>(`/v1/dashboards/${id}`, { method: 'DELETE' });
}

/** The dashboard's widgets (GET /v1/dashboards/{id}/widgets), in
 * sort_order. An empty dashboard is []. */
export function listWidgets(dashboardId: string): Promise<DashboardWidget[]> {
	return api<DashboardWidget[]>(`/v1/dashboards/${dashboardId}/widgets`);
}

/** Creates a widget (POST /v1/dashboards/{id}/widgets — 201 + the
 * created row). content: explore = the serialized state ('' = the
 * default view), markdown = the source text (1024 / 16384 char caps);
 * span defaults to 6 when omitted, row_span to the per-kind default
 * (explore 4, markdown 2). */
export function createWidget(
	dashboardId: string,
	input: { name: string; kind: WidgetKind; content: string; span?: number; row_span?: number }
): Promise<DashboardWidget> {
	return api<DashboardWidget>(`/v1/dashboards/${dashboardId}/widgets`, {
		method: 'POST',
		body: input
	});
}

/** Patches a widget (PATCH /v1/dashboards/{id}/widgets/{widgetId} —
 * 200 + the updated row). Only the fields present are changed; the
 * kind may flip, and the content is re-validated for the new kind. */
export function updateWidget(
	dashboardId: string,
	widgetId: string,
	patch: { name?: string; kind?: WidgetKind; content?: string; span?: number; row_span?: number }
): Promise<DashboardWidget> {
	return api<DashboardWidget>(`/v1/dashboards/${dashboardId}/widgets/${widgetId}`, {
		method: 'PATCH',
		body: patch
	});
}

/** Deletes a widget (DELETE /v1/dashboards/{id}/widgets/{widgetId} —
 * 204 → resolves). A widget the caller does not have answers 404. */
export function deleteWidget(dashboardId: string, widgetId: string): Promise<void> {
	return api<void>(`/v1/dashboards/${dashboardId}/widgets/${widgetId}`, { method: 'DELETE' });
}

/** Reorders the dashboard's widgets (PUT /v1/dashboards/{id}/widgets/
 * order — 204). ids must list every widget exactly once; each widget's
 * sort_order becomes its 1-based position in ids. */
export function setWidgetOrder(dashboardId: string, ids: string[]): Promise<void> {
	return api<void>(`/v1/dashboards/${dashboardId}/widgets/order`, { method: 'PUT', body: ids });
}

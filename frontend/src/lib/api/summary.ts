// Summary API (step 6.2): the period aggregation behind the Summary page —
// KPI totals, per-category spend against active budget targets, and the
// trailing trend window for the sparkline.
//
// Success bodies are the raw values the backend returns (no envelope);
// non-2xx responses are the 3.1 envelope {error: {code, message}} and
// surface as ApiError (see client.ts).

import { api } from './client';

export type Period = 'month' | 'quarter' | 'year';

/** One per-category row (backend summaryGroup). Amounts are canonical
 * 2-decimal strings; a null category is the uncategorized bucket. */
export interface SummaryGroup {
	category_id: string | null;
	category_name: string | null;
	spend: string;
	income: string;
	net: string;
	target: string | null;
}

export interface SummaryTotals {
	spend: string;
	income: string;
	net: string;
}

/** One trend window (backend trendPoint) — the same period as the
 * request; oldest first, ending on the current period. */
export interface TrendPoint {
	from: string;
	to: string;
	spend: string;
	income: string;
	net: string;
}

export interface Summary {
	period: string;
	from: string;
	to: string;
	groups: SummaryGroup[];
	totals: SummaryTotals;
	/** Present only when the request asked for a trend window. */
	trend?: TrendPoint[];
}

/** GET /v1/summary for the period. `accountId` filters to that account
 * (omitted = all accounts); `trend` asks for the trailing N periods
 * (omitted = no trend key in the response). */
export function getSummary(period: Period, accountId?: string, trend?: number): Promise<Summary> {
	const q = new URLSearchParams();
	q.set('period', period);
	if (accountId) q.set('account_id', accountId);
	if (trend !== undefined) q.set('trend', String(trend));
	return api<Summary>(`/v1/summary?${q.toString()}`);
}

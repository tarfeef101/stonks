// Explore URL state + grouping (step 6.4): a PURE, dependency-free module.
//
// Single source of truth for the Explore page's shareable URL state and its
// client-side grouping over the 3.2 List endpoint's rows. It is import-free
// by design: it must run under bare node (the e2e's codec_check.mts) outside
// the Svelte bundler, so it defines its own minimal row type (structurally
// compatible with the API's Transaction) and does no browser or Date work —
// dates are handled as integer strings (timezone-safe), and the ISO week
// math is pure integer arithmetic.
//
// Contract: parseState(serializeState(s)) deep-equals s; a malformed shared
// URL degrades gracefully (invalid values dropped, unknown params ignored)
// and never throws.

/** View-only chart kinds (URL `chart` param; never sent to the API). */
export type ChartKind = 'line' | 'bar' | 'pie' | 'table';
/** Grouping keys (URL `group` param; never sent to the API). */
export type GroupBy = 'month' | 'week' | 'category' | 'account';
/** The metric a chart or table measures (URL `metric` param). */
export type Metric = 'net' | 'spend' | 'income';

/** The full Explore state (flat; every field always holds a value: empty
 * string = absent, empty `account` = none, chart/group/metric hold their
 * defaults when unchosen). */
export interface ExploreState {
	/** YYYY-MM-DD, or '' when absent. */
	from: string;
	/** YYYY-MM-DD, or '' when absent. */
	to: string;
	/** Selected account ids, stored AS WRITTEN (case preserved), in URL
	 * order; no dedup (keeps the round-trip total). */
	account: string[];
	/** '' = any category; 'none' = uncategorized only; else a category id. */
	category: string;
	/** Min amount in the server grammar, or '' when absent. */
	min: string;
	/** Max amount in the server grammar, or '' when absent. */
	max: string;
	/** Description-substring search (any non-empty string), or ''. */
	q: string;
	chart: ChartKind;
	group: GroupBy;
	metric: Metric;
}

/** A transaction row as this module consumes it — structurally compatible
 * with (and therefore assignable from) the API's Transaction. */
export interface TxRow {
	id: string;
	account_id: string;
	/** YYYY-MM-DD. */
	date_posted: string;
	/** Canonical 2-decimal amount string (may carry a leading '-'). */
	amount: string;
	category_id: string | null;
}

/** One grouped bucket. Every amount is INTEGER CENTS (spend/income are
 * positive; net = income − spend, signed). */
export interface Agg {
	/** The grouping key (month 'YYYY-MM', ISO week 'YYYY-Www', category
	 * id or 'none', account id). */
	key: string;
	/** The display label (name; 'Uncategorized' for a null category). */
	label: string;
	count: number;
	/** Σ|amount| over the group's negative rows. */
	spend: number;
	/** Σ amount over the group's positive rows. */
	income: number;
	/** income − spend. */
	net: number;
}

/** Optional name maps for category/account grouping (rows carry only ids;
 * a missing name falls back to the id). */
export interface Names {
	categories?: Record<string, string>;
	accounts?: Record<string, string>;
}

const CHARTS: readonly ChartKind[] = ['line', 'bar', 'pie', 'table'];
const GROUPS: readonly GroupBy[] = ['month', 'week', 'category', 'account'];
const METRICS: readonly Metric[] = ['net', 'spend', 'income'];
const UNCATEGORIZED = 'Uncategorized';

const DATE_RE = /^\d{4}-\d{2}-\d{2}$/;
const UUID_RE = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;
/** Mirror of the server's CanonicalAmount grammar. */
const AMOUNT_RE = /^[+-]?(\d+\.?\d{0,2}|\.\d{1,2})$/;

const MONTH_DAYS = [31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];

function isLeap(y: number): boolean {
	return y % 4 === 0 && (y % 100 !== 0 || y % 400 === 0);
}

/** A real calendar date in YYYY-MM-DD form (integer day-in-month check; no
 * Date — timezone-safe). */
function isValidDate(s: string): boolean {
	if (!DATE_RE.test(s)) return false;
	const m = parseInt(s.slice(5, 7), 10);
	const d = parseInt(s.slice(8, 10), 10);
	if (m < 1 || m > 12) return false;
	const max = m === 2 && isLeap(parseInt(s.slice(0, 4), 10)) ? 29 : MONTH_DAYS[m - 1];
	return d >= 1 && d <= max;
}

/** Parses a query string WITHOUT a leading '?' into the full state.
 * Invalid values are silently dropped; unknown params are ignored; when a
 * param repeats, the first VALID occurrence wins (for `account`, every
 * valid occurrence is kept, in order). */
export function parseState(qs: string): ExploreState {
	const params = new URLSearchParams(qs.startsWith('?') ? qs.slice(1) : qs);
	const state: ExploreState = {
		from: '',
		to: '',
		account: [],
		category: '',
		min: '',
		max: '',
		q: '',
		chart: 'table',
		group: 'month',
		metric: 'net'
	};
	const firstValid = (key: string, ok: (v: string) => boolean): string => {
		for (const v of params.getAll(key)) if (ok(v)) return v;
		return '';
	};
	const pick = <T extends string>(key: string, allowed: readonly T[], dflt: T): T => {
		for (const v of params.getAll(key))
			if ((allowed as readonly string[]).includes(v)) return v as T;
		return dflt;
	};
	state.from = firstValid('from', isValidDate);
	state.to = firstValid('to', isValidDate);
	for (const v of params.getAll('account')) if (UUID_RE.test(v)) state.account.push(v);
	state.category = firstValid('category', (v) => v === 'none' || UUID_RE.test(v));
	state.min = firstValid('min', (v) => AMOUNT_RE.test(v));
	state.max = firstValid('max', (v) => AMOUNT_RE.test(v));
	state.q = params.getAll('q').find((v) => v !== '') ?? '';
	state.chart = pick('chart', CHARTS, 'table');
	state.group = pick('group', GROUPS, 'month');
	state.metric = pick('metric', METRICS, 'net');
	return state;
}

/** Serializes the state to the canonical query string (no leading '?').
 * Defaults are omitted: chart=table / group=month / metric=net and every
 * absent filter. Canonical param order:
 * from,to,account,category,min,max,q,chart,group,metric. */
export function serializeState(s: ExploreState): string {
	const params = new URLSearchParams();
	if (s.from !== '') params.append('from', s.from);
	if (s.to !== '') params.append('to', s.to);
	for (const a of s.account) params.append('account', a);
	if (s.category !== '') params.append('category', s.category);
	if (s.min !== '') params.append('min', s.min);
	if (s.max !== '') params.append('max', s.max);
	if (s.q !== '') params.append('q', s.q);
	if (s.chart !== 'table') params.append('chart', s.chart);
	if (s.group !== 'month') params.append('group', s.group);
	if (s.metric !== 'net') params.append('metric', s.metric);
	return params.toString();
}

/** The exact API query params for one page of the 3.2 List endpoint, in
 * the pinned wire order: date_from,date_to,account_id(repeated),
 * category_id,min_amount,max_amount,search,limit=100,offset. `category`
 * 'none' becomes an EMPTY category_id (uncategorized-only).
 * chart/group/metric are view-only — never present. */
export function apiQuery(s: ExploreState, offset: number): URLSearchParams {
	const params = new URLSearchParams();
	if (s.from !== '') params.append('date_from', s.from);
	if (s.to !== '') params.append('date_to', s.to);
	for (const a of s.account) params.append('account_id', a);
	if (s.category !== '') params.append('category_id', s.category === 'none' ? '' : s.category);
	if (s.min !== '') params.append('min_amount', s.min);
	if (s.max !== '') params.append('max_amount', s.max);
	if (s.q !== '') params.append('search', s.q);
	params.append('limit', '100');
	params.append('offset', String(offset));
	return params;
}

/** Integer cents for a canonical amount string (integer arithmetic only —
 * never parseFloat on amounts). A malformed input yields 0 (rows come from
 * the server, which canonicalizes). */
function toCents(s: string): number {
	const m = /^(-?)(\d+)(?:\.(\d{1,2}))?$/.exec(s);
	if (!m) return 0;
	const v = parseInt(m[2], 10) * 100 + parseInt((m[3] ?? '').padEnd(2, '0'), 10);
	return m[1] === '-' ? -v : v;
}

/** Integer cents → canonical string, matching the server's canonicalAmount
 * output exactly: always 2 decimals, no leading zeros in the integer part,
 * a sign only when negative, and NEGATIVE ZERO NORMALIZED (-0 → "0.00").
 * Pure string math. */
export function formatCents(cents: number): string {
	const neg = cents < 0;
	const s = String(neg ? -cents : cents).padStart(3, '0');
	return (neg ? '-' : '') + s.slice(0, -2) + '.' + s.slice(-2);
}

/** Sakamoto's day of week: 0 = Sunday … 6 = Saturday (pure integer math). */
function dayOfWeek(y: number, m: number, d: number): number {
	const t = [0, 3, 2, 5, 0, 3, 5, 1, 4, 6, 2, 4];
	y -= m < 3 ? 1 : 0;
	return (y + Math.floor(y / 4) - Math.floor(y / 100) + Math.floor(y / 400) + t[m - 1] + d) % 7;
}

function daysBeforeMonth(m: number, leap: boolean): number {
	const before = [0, 31, 59, 90, 120, 151, 181, 212, 243, 273, 304, 334];
	return before[m - 1] + (m > 2 && leap ? 1 : 0);
}

/** ISO-8601 weeks in a year: 53 iff the year starts on Thursday, or is a
 * leap year starting on Wednesday. */
function isoWeeksInYear(y: number): number {
	const jan1 = dayOfWeek(y, 1, 1);
	return jan1 === 4 || (isLeap(y) && jan1 === 3) ? 53 : 52;
}

/** The ISO-8601 week key "YYYY-Www" for a YYYY-MM-DD date (week 1 contains
 * the year's first Thursday; Monday-based). Pure integer math — no Date. */
function isoWeekKey(date: string): string {
	const y = parseInt(date.slice(0, 4), 10);
	const m = parseInt(date.slice(5, 7), 10);
	const d = parseInt(date.slice(8, 10), 10);
	const isoDow = (dayOfWeek(y, m, d) + 6) % 7; // 1 = Monday … 7 = Sunday
	const doy = daysBeforeMonth(m, isLeap(y)) + d;
	let week = Math.floor((doy - isoDow + 10) / 7);
	let isoYear = y;
	if (week < 1) {
		isoYear = y - 1;
		week = isoWeeksInYear(isoYear);
	} else if (week > isoWeeksInYear(y)) {
		isoYear = y + 1;
		week = 1;
	}
	return `${isoYear}-W${String(week).padStart(2, '0')}`;
}

/** Groups the rows and sums per group (INTEGER CENTS). Ordering:
 * month/week by key asc (chronological); category by name asc with
 * "Uncategorized" LAST (id tiebreak); account by name asc (id tiebreak). */
export function aggregate(rows: readonly TxRow[], group: GroupBy, names?: Names): Agg[] {
	const groups = new Map<string, Agg>();
	for (const row of rows) {
		let key: string;
		let label: string;
		if (group === 'month') {
			key = row.date_posted.slice(0, 7);
			label = key;
		} else if (group === 'week') {
			key = isoWeekKey(row.date_posted);
			label = key;
		} else if (group === 'category') {
			key = row.category_id ?? 'none';
			label = key === 'none' ? UNCATEGORIZED : (names?.categories?.[key] ?? key);
		} else {
			key = row.account_id;
			label = names?.accounts?.[key] ?? key;
		}
		let g = groups.get(key);
		if (!g) {
			g = { key, label, count: 0, spend: 0, income: 0, net: 0 };
			groups.set(key, g);
		}
		const cents = toCents(row.amount);
		g.count += 1;
		if (cents > 0) g.income += cents;
		else if (cents < 0) g.spend += -cents;
	}
	const aggs = [...groups.values()];
	for (const g of aggs) g.net = g.income - g.spend;
	const byKey = (a: Agg, b: Agg) => (a.key < b.key ? -1 : a.key > b.key ? 1 : 0);
	if (group === 'month' || group === 'week') {
		aggs.sort(byKey);
	} else if (group === 'category') {
		aggs.sort((a, b) => {
			const au = a.label === UNCATEGORIZED ? 1 : 0;
			const bu = b.label === UNCATEGORIZED ? 1 : 0;
			if (au !== bu) return au - bu;
			if (a.label !== b.label) return a.label < b.label ? -1 : 1;
			return byKey(a, b);
		});
	} else {
		aggs.sort((a, b) => (a.label !== b.label ? (a.label < b.label ? -1 : 1) : byKey(a, b)));
	}
	return aggs;
}

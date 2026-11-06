<script lang="ts">
	import { resolve } from '$app/paths';
	import { _ } from 'svelte-i18n';
	import * as echarts from 'echarts';
	import { Chart } from 'svelte-echarts';
	import { user } from '#lib/auth/client';
	import { ApiError } from '#lib/api/client';
	import { listAccounts, type Account } from '#lib/api/accounts';
	import { listCategories, type Category } from '#lib/api/categories';
	import { listTransactions, type Transaction, type ListParams } from '#lib/api/transactions';
	import {
		parseState,
		serializeState,
		aggregate,
		formatCents,
		type ExploreState
	} from '#lib/explore/state';

	// Explore page (step 6.4): a filterable, groupable view of the
	// caller's transactions. The filter state lives in the URL (every
	// view is shareable); the grouping/aggregation is client-side over
	// the 3.2 List endpoint's rows (lib/explore/state.ts); the
	// visualization is svelte-echarts (line/bar/pie) or a plain table.
	//
	// Rendered-numbers contract: every displayed amount — table cells,
	// axis ticks, tooltip values — is an aggregate INTEGER-CENTS sum run
	// through formatCents (state.ts). The min/max inputs are RAW TEXT
	// with no client-side grammar: a malformed value is a server 400
	// surfaced verbatim via envelopeMessage (house pattern).
	//
	// House pattern: {#if !$user} sign-in gate, one $effect per concern
	// (rows / catalogs / URL), a seq guard drops stale responses,
	// envelopeMessage carves the 3.1 envelope out of ApiError.

	// The 3.2 endpoint caps limit at 100: page offset 0..900, stop early
	// on a short page; 10 full pages = the 1000-row cap note.
	const PAGE_SIZE = 100;
	const MAX_PAGES = 10;

	/** The URL's state IS the page's state (init once on load; every
	 * change flows back to the address bar via the sync effect). */
	let explore = $state<ExploreState>(parseState(window.location.search.slice(1)));

	let rows = $state<Transaction[]>([]);
	let capped = $state(false);
	let loading = $state(true);
	let loadError = $state<string | null>(null);

	let accounts = $state<Account[]>([]);
	let categories = $state<Category[]>([]);

	let fetchSeq = 0;
	let catalogSeq = 0;

	/** id → display name maps for the category/account groupings (rows
	 * carry only ids; a missing name falls back to the id in aggregate). */
	const nameMaps = $derived({
		categories: Object.fromEntries(categories.map((c) => [c.id, c.name])),
		accounts: Object.fromEntries(accounts.map((a) => [a.id, a.name]))
	});

	/** The current grouping of the loaded rows (integer cents). */
	const aggs = $derived(aggregate(rows, explore.group, nameMaps));

	/** Pie only: the groups hidden because the metric is not positive. */
	const pieHidden = $derived(
		explore.chart === 'pie' ? aggs.filter((g) => g[explore.metric] <= 0).length : 0
	);

	/** The ECharts option for the current view, or null for the table
	 * view / no rows. Tooltip and axis formatters run formatCents over
	 * the raw values (the only displayed-number path). */
	const chartOptions = $derived.by((): echarts.EChartsOption | null => {
		if (explore.chart === 'table' || aggs.length === 0) return null;
		const fmt = (v: unknown): string => formatCents(Math.round(Number(v)));
		if (explore.chart === 'pie') {
			return {
				tooltip: { valueFormatter: fmt },
				series: [
					{
						type: 'pie',
						radius: '65%',
						data: aggs
							.filter((g) => g[explore.metric] > 0)
							.map((g) => ({ name: g.label, value: g[explore.metric] }))
					}
				]
			};
		}
		const labels = aggs.map((g) => g.label);
		const values = aggs.map((g) => g[explore.metric]);
		if (explore.chart === 'line') {
			return {
				tooltip: { trigger: 'axis', valueFormatter: fmt },
				xAxis: { type: 'category', data: labels },
				yAxis: { type: 'value', axisLabel: { formatter: fmt } },
				series: [{ type: 'line', data: values }]
			};
		}
		return {
			tooltip: { trigger: 'axis', valueFormatter: fmt },
			xAxis: { type: 'category', data: labels },
			yAxis: { type: 'value', axisLabel: { formatter: fmt } },
			series: [{ type: 'bar', data: values }]
		};
	});

	// --- Loading (filter-driven) ----------------------------------------

	/** The server-side filter fields only: the URL view fields (chart /
	 * group / metric) are deliberately excluded — they re-render the
	 * view without a refetch and are NEVER sent to the API. */
	type FilterFields = Pick<
		ExploreState,
		'from' | 'to' | 'account' | 'category' | 'min' | 'max' | 'q'
	>;

	/** One page's 3.2 List params (absent = omitted; 'none' =
	 * uncategorized-only). */
	function listParams(s: FilterFields, offset: number): ListParams {
		const p: ListParams = { limit: PAGE_SIZE, offset };
		if (s.from !== '') p.date_from = s.from;
		if (s.to !== '') p.date_to = s.to;
		if (s.account.length > 0) p.account_id = [...s.account];
		if (s.category !== '') p.category_id = s.category === 'none' ? '' : s.category;
		if (s.min !== '') p.min_amount = s.min;
		if (s.max !== '') p.max_amount = s.max;
		if (s.q !== '') p.search = s.q;
		return p;
	}

	/** Loads all matching pages (0..900; a short page ends the walk). */
	async function loadRows(s: FilterFields): Promise<void> {
		const seq = ++fetchSeq;
		loading = true;
		loadError = null;
		try {
			const out: Transaction[] = [];
			let cap = false;
			for (let page = 0; page < MAX_PAGES; page++) {
				const batch = await listTransactions(listParams(s, page * PAGE_SIZE));
				if (seq !== fetchSeq) return;
				out.push(...batch);
				if (batch.length < PAGE_SIZE) break;
				if (page === MAX_PAGES - 1) cap = true;
			}
			if (seq !== fetchSeq) return;
			rows = out;
			capped = cap;
		} catch (e) {
			if (seq !== fetchSeq) return;
			rows = [];
			capped = false;
			loadError = envelopeMessage(e);
		}
		// Only the newest load clears the spinner (a superseded load's
		// flag belongs to the load that started after it).
		if (seq === fetchSeq) loading = false;
	}

	// One load per (identity, filter fields) change. The seven filter
	// fields are read synchronously below — those are the tracked deps.
	$effect(() => {
		const sub = $user?.profile.sub ?? null;
		const snap: FilterFields = {
			from: explore.from,
			to: explore.to,
			account: explore.account,
			category: explore.category,
			min: explore.min,
			max: explore.max,
			q: explore.q
		};
		if (sub === null) {
			fetchSeq++; // invalidate any in-flight load
			rows = [];
			capped = false;
			loadError = null;
			loading = false;
			return;
		}
		void loadRows(snap);
	});

	// --- Catalogs (labels) ----------------------------------------------

	// The account/category catalogs feed the filter bar and the group
	// labels. Load failures are swallowed: the labels fall back to ids.
	$effect(() => {
		const sub = $user?.profile.sub ?? null;
		if (sub === null) {
			catalogSeq++;
			accounts = [];
			categories = [];
			return;
		}
		const seq = ++catalogSeq;
		void Promise.all([listAccounts(), listCategories()])
			.then(([as, cs]) => {
				if (seq !== catalogSeq) return;
				accounts = as;
				categories = cs;
			})
			.catch(() => {
				// Keep the previous catalogs (or none) — the labels
				// fall back to the raw ids.
			});
	});

	// --- URL sync ---------------------------------------------------------

	// The URL is the state: after every change, replace the history
	// entry with the canonical serialized form (shareable; no history
	// spam). Runs once on load too — it canonicalizes whatever URL the
	// page opened with.
	$effect(() => {
		const qs = serializeState({
			from: explore.from,
			to: explore.to,
			account: explore.account,
			category: explore.category,
			min: explore.min,
			max: explore.max,
			q: explore.q,
			chart: explore.chart,
			group: explore.group,
			metric: explore.metric
		});
		const path = window.location.pathname;
		window.history.replaceState(window.history.state, '', qs !== '' ? `${path}?${qs}` : path);
	});

	/** The backend answers 4xx with the 3.1 envelope {error: {code,
	 * message}}; ApiError folds the raw body into its message ("API
	 * 400: {...}") — carve the JSON out for the friendly line, fall
	 * back to the raw message when the body is not an envelope. A 400
	 * message IS the "; "-joined problem list, shown verbatim. */
	function envelopeMessage(e: unknown): string {
		if (e instanceof ApiError) {
			const start = e.message.indexOf('{');
			if (start !== -1) {
				try {
					const env = JSON.parse(e.message.slice(start)) as {
						error?: { message?: string };
					};
					if (env.error?.message) return env.error.message;
				} catch {
					// not JSON — fall through
				}
			}
		}
		return e instanceof Error ? e.message : String(e);
	}

	// --- Presets (browser Date — page-only; state.ts stays Date-free) ----

	function pad2(n: number): string {
		return String(n).padStart(2, '0');
	}

	/** The preset's from/to in local time, or null for 'custom'. */
	function presetRange(key: string): { from: string; to: string } | null {
		const now = new Date();
		const y = now.getFullYear();
		const m = now.getMonth();
		if (key === 'this_month') {
			const last = new Date(y, m + 1, 0).getDate();
			return { from: `${y}-${pad2(m + 1)}-01`, to: `${y}-${pad2(m + 1)}-${pad2(last)}` };
		}
		if (key === 'last_month') {
			const first = new Date(y, m - 1, 1);
			const last = new Date(y, m, 0);
			return {
				from: `${first.getFullYear()}-${pad2(first.getMonth() + 1)}-01`,
				to: `${last.getFullYear()}-${pad2(last.getMonth() + 1)}-${pad2(last.getDate())}`
			};
		}
		if (key === 'this_quarter') {
			const q = Math.floor(m / 3) * 3;
			const last = new Date(y, q + 3, 0);
			return {
				from: `${y}-${pad2(q + 1)}-01`,
				to: `${last.getFullYear()}-${pad2(last.getMonth() + 1)}-${pad2(last.getDate())}`
			};
		}
		if (key === 'this_year') {
			return { from: `${y}-01-01`, to: `${y}-12-31` };
		}
		if (key === 'all_time') {
			return { from: '', to: '' };
		}
		return null;
	}

	const PRESET_KEYS = [
		'this_month',
		'last_month',
		'this_quarter',
		'this_year',
		'all_time'
	] as const;

	/** The preset whose range matches the current from/to — 'custom'
	 * when none (the date inputs hold a hand-picked range). */
	function currentPreset(): string {
		for (const key of PRESET_KEYS) {
			const r = presetRange(key);
			if (r !== null && r.from === explore.from && r.to === explore.to) return key;
		}
		return 'custom';
	}

	function presetLabel(key: string): string {
		switch (key) {
			case 'this_month':
				return $_('explore.preset_this_month');
			case 'last_month':
				return $_('explore.preset_last_month');
			case 'this_quarter':
				return $_('explore.preset_this_quarter');
			case 'this_year':
				return $_('explore.preset_this_year');
			case 'all_time':
				return $_('explore.preset_all_time');
			default:
				return $_('explore.preset_custom');
		}
	}

	function applyPreset(key: string): void {
		if (key === 'custom') return; // display-only — a hand-picked range
		const r = presetRange(key);
		if (r !== null) explore = { ...explore, from: r.from, to: r.to };
	}

	// --- Actions ----------------------------------------------------------

	function onAccountsChange(e: Event): void {
		const sel = e.currentTarget as HTMLSelectElement;
		explore = { ...explore, account: [...sel.selectedOptions].map((o) => o.value) };
	}

	function resetFilters(): void {
		explore = {
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
	}
</script>

<section class="space-y-4">
	<h1 class="text-2xl font-semibold">{$_('explore.heading')}</h1>
	<p class="max-w-2xl text-sm text-zinc-600">{$_('explore.intro')}</p>

	{#if !$user}
		<p class="text-sm text-zinc-600">
			{$_('explore.sign_in_prompt')}
			<a class="underline" href={resolve('/')}>{$_('auth.sign_in')}</a>
		</p>
	{:else}
		<!-- Filter bar: the seven server-side filters. The state is
		     mirrored into the URL by the sync effect; the preset select
		     is a convenience over from/to (a hand-picked range shows
		     "Custom" and selecting it is a no-op). min/max are raw
		     text — a bad value is a server 400 shown verbatim. -->
		<div class="rounded-lg border border-zinc-200 bg-white p-5">
			<p class="text-xs font-medium text-zinc-500 uppercase">{$_('explore.filter_title')}</p>
			<div class="mt-3 flex flex-wrap items-end gap-4">
				<label class="block text-sm">
					<span class="mb-1 block font-medium">{$_('explore.preset_label')}</span>
					<select
						class="w-40 rounded-md border border-zinc-300 px-3 py-1.5"
						value={currentPreset()}
						onchange={(e) => applyPreset(e.currentTarget.value)}
					>
						{#each PRESET_KEYS as key (key)}
							<option value={key}>{presetLabel(key)}</option>
						{/each}
						<option value="custom">{$_('explore.preset_custom')}</option>
					</select>
				</label>
				<label class="block text-sm">
					<span class="mb-1 block font-medium">{$_('explore.date_from')}</span>
					<input
						type="date"
						class="rounded-md border border-zinc-300 px-3 py-1.5"
						bind:value={explore.from}
					/>
				</label>
				<label class="block text-sm">
					<span class="mb-1 block font-medium">{$_('explore.date_to')}</span>
					<input
						type="date"
						class="rounded-md border border-zinc-300 px-3 py-1.5"
						bind:value={explore.to}
					/>
				</label>
				<div class="block text-sm">
					<span class="mb-1 block font-medium">{$_('explore.account_label')}</span>
					<select
						class="h-16 w-56 rounded-md border border-zinc-300 px-3 py-1.5"
						multiple
						value={explore.account}
						onchange={onAccountsChange}
					>
						{#each accounts as a (a.id)}
							<option value={a.id}>{a.name}</option>
						{/each}
					</select>
				</div>
				<label class="block text-sm">
					<span class="mb-1 block font-medium">{$_('explore.category_label')}</span>
					<select
						class="w-48 rounded-md border border-zinc-300 px-3 py-1.5"
						value={explore.category}
						onchange={(e) => (explore = { ...explore, category: e.currentTarget.value })}
					>
						<option value="">{$_('explore.category_any')}</option>
						<option value="none">{$_('explore.category_uncategorized')}</option>
						{#each categories as c (c.id)}
							<option value={c.id}>{c.name}</option>
						{/each}
					</select>
				</label>
				<label class="block text-sm">
					<span class="mb-1 block font-medium">{$_('explore.min_label')}</span>
					<input
						class="w-28 rounded-md border border-zinc-300 px-3 py-1.5"
						inputmode="decimal"
						bind:value={explore.min}
					/>
				</label>
				<label class="block text-sm">
					<span class="mb-1 block font-medium">{$_('explore.max_label')}</span>
					<input
						class="w-28 rounded-md border border-zinc-300 px-3 py-1.5"
						inputmode="decimal"
						bind:value={explore.max}
					/>
				</label>
				<label class="block text-sm">
					<span class="mb-1 block font-medium">{$_('explore.search_label')}</span>
					<input
						class="w-48 rounded-md border border-zinc-300 px-3 py-1.5"
						bind:value={explore.q}
					/>
				</label>
				<button
					class="rounded-md border border-zinc-300 px-4 py-2 text-sm font-medium text-zinc-700 hover:bg-zinc-100"
					type="button"
					onclick={resetFilters}
				>
					{$_('explore.reset')}
				</button>
			</div>
		</div>

		<!-- View bar: chart kind / grouping / metric (URL-only fields —
		     they never refetch) plus the row count and the 1000-row cap
		     note. -->
		<div class="rounded-lg border border-zinc-200 bg-white p-5">
			<div class="flex flex-wrap items-end gap-4">
				<p class="self-center text-xs font-medium text-zinc-500 uppercase">
					{$_('explore.view_title')}
				</p>
				<label class="block text-sm">
					<span class="mb-1 block font-medium">{$_('explore.chart_label')}</span>
					<select
						class="w-32 rounded-md border border-zinc-300 px-3 py-1.5"
						value={explore.chart}
						onchange={(e) =>
							(explore = { ...explore, chart: e.currentTarget.value as ExploreState['chart'] })}
					>
						<option value="line">{$_('explore.chart_line')}</option>
						<option value="bar">{$_('explore.chart_bar')}</option>
						<option value="pie">{$_('explore.chart_pie')}</option>
						<option value="table">{$_('explore.chart_table')}</option>
					</select>
				</label>
				<label class="block text-sm">
					<span class="mb-1 block font-medium">{$_('explore.group_label')}</span>
					<select
						class="w-36 rounded-md border border-zinc-300 px-3 py-1.5"
						value={explore.group}
						onchange={(e) =>
							(explore = { ...explore, group: e.currentTarget.value as ExploreState['group'] })}
					>
						<option value="month">{$_('explore.group_month')}</option>
						<option value="week">{$_('explore.group_week')}</option>
						<option value="category">{$_('explore.group_category')}</option>
						<option value="account">{$_('explore.group_account')}</option>
					</select>
				</label>
				<label class="block text-sm">
					<span class="mb-1 block font-medium">{$_('explore.metric_label')}</span>
					<select
						class="w-32 rounded-md border border-zinc-300 px-3 py-1.5"
						value={explore.metric}
						onchange={(e) =>
							(explore = { ...explore, metric: e.currentTarget.value as ExploreState['metric'] })}
					>
						<option value="net">{$_('explore.metric_net')}</option>
						<option value="spend">{$_('explore.metric_spend')}</option>
						<option value="income">{$_('explore.metric_income')}</option>
					</select>
				</label>
				<p class="self-center text-sm text-zinc-600">
					{$_('explore.row_count', { values: { n: rows.length } })}
				</p>
				{#if capped}
					<p class="self-center text-sm text-amber-700">{$_('explore.cap_note')}</p>
				{/if}
			</div>
		</div>

		{#if loading && rows.length === 0}
			<p class="text-sm text-zinc-600">{$_('explore.loading')}</p>
		{:else if loadError}
			<p class="text-sm text-red-600">{loadError}</p>
		{:else if rows.length === 0}
			<p class="text-sm text-zinc-500">{$_('explore.empty')}</p>
		{:else if explore.chart === 'table'}
			<!-- The table view: the GROUPED rows only (never the raw
			     transactions); every amount is a formatCents of the
			     group's integer-cents sums. -->
			<div class="overflow-x-auto rounded-lg border border-zinc-200 bg-white">
				<table class="w-full text-sm">
					<thead>
						<tr class="border-b border-zinc-200 text-left text-xs text-zinc-500 uppercase">
							<th class="px-4 py-2">{$_('explore.col_group')}</th>
							<th class="px-4 py-2 text-right">{$_('explore.col_count')}</th>
							<th class="px-4 py-2 text-right">{$_('explore.col_spend')}</th>
							<th class="px-4 py-2 text-right">{$_('explore.col_income')}</th>
							<th class="px-4 py-2 text-right">{$_('explore.col_net')}</th>
						</tr>
					</thead>
					<tbody>
						{#each aggs as g (g.key)}
							<tr class="border-b border-zinc-100 last:border-0">
								<td class="px-4 py-1.5">{g.label}</td>
								<td class="px-4 py-1.5 text-right tabular-nums">{g.count}</td>
								<td class="px-4 py-1.5 text-right tabular-nums">
									{formatCents(g.spend)}
								</td>
								<td class="px-4 py-1.5 text-right tabular-nums">
									{formatCents(g.income)}
								</td>
								<td class="px-4 py-1.5 text-right tabular-nums">
									{formatCents(g.net)}
								</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		{:else}
			<!-- Chart view: svelte-echarts over the same grouped data
			     (line/bar x = group labels, y = the metric in cents;
			     the pie drops non-positive groups and says so). -->
			<div class="rounded-lg border border-zinc-200 bg-white p-5">
				{#if explore.chart === 'pie' && pieHidden > 0}
					<p class="mb-2 text-xs text-zinc-500">
						{$_('explore.pie_excluded', { values: { n: pieHidden } })}
					</p>
				{/if}
				{#if chartOptions !== null}
					<div class="h-80">
						<Chart init={echarts.init} options={chartOptions} class="h-full w-full" />
					</div>
				{/if}
			</div>
		{/if}
	{/if}
</section>

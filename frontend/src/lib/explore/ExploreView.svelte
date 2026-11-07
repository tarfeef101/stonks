<script lang="ts">
	import * as echarts from 'echarts';
	import { Chart } from 'svelte-echarts';
	import { _ } from 'svelte-i18n';
	import { user } from '#lib/auth/client';
	import { ApiError } from '#lib/api/client';
	import { listTransactions, type Transaction, type ListParams } from '#lib/api/transactions';
	import { aggregate, formatCents, type ExploreState, type Names } from './state';

	// Explore view (step 6.5): the shared read-only render of an Explore
	// state — the paged 3.2 List data loop, the client-side
	// grouping/aggregation, and the chart/table markup, moved from the
	// explore page so the dashboard cards can render saved views. Until
	// C2 wires the pages, nothing imports this component.
	//
	// Rendered-numbers contract: every displayed amount — table cells,
	// axis ticks, tooltip values — is an aggregate INTEGER-CENTS sum run
	// through formatCents (state.ts).
	//
	// House pattern: one filter-driven $effect, a seq guard drops stale
	// responses, envelopeMessage carves the 3.1 envelope out of ApiError,
	// and a signed-out identity resets the view (the host page owns the
	// sign-in prompt, the filter bar, and the view bar).

	interface Props {
		/** The Explore state to render (read-only — the view never
		 * mutates it; the host page owns the state). */
		state: ExploreState;
		/** id -> display name maps for the category/account groupings
		 * (rows carry only ids; a missing name falls back to the id). */
		names: Names;
	}

	// The prop is bound to a local named `explore` (the page's original
	// local name — the moved code reads below stays verbatim): a local
	// called `state` would shadow the $state rune ($state then parses
	// as a store subscription to the prop).
	let { state: explore, names }: Props = $props();

	// The 3.2 endpoint caps limit at 100: page offset 0..900, stop early
	// on a short page; 10 full pages = the 1000-row cap note.
	const PAGE_SIZE = 100;
	const MAX_PAGES = 10;

	let rows = $state<Transaction[]>([]);
	let capped = $state(false);
	let loading = $state(true);
	let loadError = $state<string | null>(null);

	let fetchSeq = 0;

	/** The current grouping of the loaded rows (integer cents). */
	const aggs = $derived(aggregate(rows, explore.group, names));

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

	/** The server-side filter fields only: the view fields (chart /
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
</script>

<!-- Render section moved from the explore page (step 6.5 C1): the
     1000-row cap note, the loading/error/empty lines, and the
     chart/table view over the same grouped data. -->
<p class="text-sm text-zinc-600">{$_('explore.row_count', { values: { n: rows.length } })}</p>
{#if capped}
	<p class="self-center text-sm text-amber-700">{$_('explore.cap_note')}</p>
{/if}
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

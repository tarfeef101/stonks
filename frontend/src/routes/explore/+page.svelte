<script lang="ts">
	import { resolve } from '$app/paths';
	import { _ } from 'svelte-i18n';
	import { user } from '#lib/auth/client';
	import { ApiError } from '#lib/api/client';
	import { listAccounts, type Account } from '#lib/api/accounts';
	import { listCategories, type Category } from '#lib/api/categories';
	import { createDashboardCard } from '#lib/api/dashboardCards';
	import { parseState, serializeState, type ExploreState } from '#lib/explore/state';
	import ExploreView from '#lib/explore/ExploreView.svelte';

	// Explore page (step 6.4, refactored in 6.5 C2): a filterable,
	// groupable view of the caller's transactions. The filter state
	// lives in the URL (every view is shareable); the row data loop,
	// the grouping/aggregation, and the table/chart render live in the
	// shared ExploreView component (lib/explore), which the dashboard
	// cards embed too. The page owns the filter bar, the view bar
	// (with "Save as card"), the catalogs, and the URL sync.
	//
	// Rendered-numbers contract: every displayed amount — table cells,
	// axis ticks, tooltip values — is an aggregate INTEGER-CENTS sum run
	// through formatCents (state.ts, inside the ExploreView). The
	// min/max inputs are RAW TEXT with no client-side grammar: a
	// malformed value is a server 400 surfaced verbatim via
	// envelopeMessage (house pattern).
	//
	// House pattern: {#if !$user} sign-in gate, one $effect per concern
	// (catalogs / URL), a seq guard drops stale responses,
	// envelopeMessage carves the 3.1 envelope out of ApiError.

	/** The URL's state IS the page's state (init once on load; every
	 * change flows back to the address bar via the sync effect). */
	let explore = $state<ExploreState>(parseState(window.location.search.slice(1)));

	let accounts = $state<Account[]>([]);
	let categories = $state<Category[]>([]);

	let catalogSeq = 0;

	/** id → display name maps for the category/account groupings (rows
	 * carry only ids; a missing name falls back to the id in aggregate). */
	const nameMaps = $derived({
		categories: Object.fromEntries(categories.map((c) => [c.id, c.name])),
		accounts: Object.fromEntries(accounts.map((a) => [a.id, a.name]))
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

	// --- Save as card -----------------------------------------------------

	// The view bar's "Save as card": prompt for a name, then POST the
	// current view — the RAW canonical serializeState(explore) string
	// (no trim or transform; '' is the default view). A cancelled
	// prompt (null) is a no-op; an empty/whitespace name is a server
	// 400 surfaced verbatim via envelopeMessage. The green note clears
	// itself after 2.5s.
	let saveNote = $state<string | null>(null);
	let saveError = $state<string | null>(null);
	let saveTimer: ReturnType<typeof setTimeout> | null = null;

	async function saveAsCard(): Promise<void> {
		const name = window.prompt($_('explore.save_prompt'));
		if (name === null) return; // the user cancelled the prompt
		if (saveTimer !== null) {
			clearTimeout(saveTimer);
			saveTimer = null;
		}
		saveNote = null;
		saveError = null;
		try {
			await createDashboardCard(name, serializeState(explore));
			saveNote = $_('explore.saved');
			saveTimer = setTimeout(() => {
				saveNote = null;
			}, 2500);
		} catch (e) {
			saveError = envelopeMessage(e);
		}
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
	<p class="max-w-2xl text-sm text-zinc-600 dark:text-zinc-400">{$_('explore.intro')}</p>

	{#if !$user}
		<p class="text-sm text-zinc-600 dark:text-zinc-400">
			{$_('explore.sign_in_prompt')}
			<a class="underline" href={resolve('/')}>{$_('auth.sign_in')}</a>
		</p>
	{:else}
		<!-- Filter bar: the seven server-side filters. The state is
		     mirrored into the URL by the sync effect; the preset select
		     is a convenience over from/to (a hand-picked range shows
		     "Custom" and selecting it is a no-op). min/max are raw
		     text — a bad value is a server 400 shown verbatim. -->
		<div class="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
			<p class="text-xs font-medium text-zinc-500 dark:text-zinc-400 uppercase">{$_('explore.filter_title')}</p>
			<div class="mt-3 flex flex-wrap items-end gap-4">
				<label class="block text-sm">
					<span class="mb-1 block font-medium">{$_('explore.preset_label')}</span>
					<select
						class="w-40 rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
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
						class="rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
						bind:value={explore.from}
					/>
				</label>
				<label class="block text-sm">
					<span class="mb-1 block font-medium">{$_('explore.date_to')}</span>
					<input
						type="date"
						class="rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
						bind:value={explore.to}
					/>
				</label>
				<div class="block text-sm">
					<span class="mb-1 block font-medium">{$_('explore.account_label')}</span>
					<select
						class="h-16 w-56 rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
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
						class="w-48 rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
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
						class="w-28 rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
						inputmode="decimal"
						bind:value={explore.min}
					/>
				</label>
				<label class="block text-sm">
					<span class="mb-1 block font-medium">{$_('explore.max_label')}</span>
					<input
						class="w-28 rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
						inputmode="decimal"
						bind:value={explore.max}
					/>
				</label>
				<label class="block text-sm">
					<span class="mb-1 block font-medium">{$_('explore.search_label')}</span>
					<input
						class="w-48 rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
						bind:value={explore.q}
					/>
				</label>
				<button
					class="rounded-md border border-zinc-300 dark:border-zinc-700 px-4 py-2 text-sm font-medium text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-800"
					type="button"
					onclick={resetFilters}
				>
					{$_('explore.reset')}
				</button>
			</div>
		</div>

		<!-- View bar: chart kind / grouping / metric (URL-only fields —
		     they never refetch) plus the "Save as card" button. The row
		     count and the 1000-row cap note render inside the
		     ExploreView (below). -->
		<div class="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
			<div class="flex flex-wrap items-end gap-4">
				<p class="self-center text-xs font-medium text-zinc-500 dark:text-zinc-400 uppercase">
					{$_('explore.view_title')}
				</p>
				<label class="block text-sm">
					<span class="mb-1 block font-medium">{$_('explore.chart_label')}</span>
					<select
						class="w-32 rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
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
						class="w-36 rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
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
						class="w-32 rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
						value={explore.metric}
						onchange={(e) =>
							(explore = { ...explore, metric: e.currentTarget.value as ExploreState['metric'] })}
					>
						<option value="net">{$_('explore.metric_net')}</option>
						<option value="spend">{$_('explore.metric_spend')}</option>
						<option value="income">{$_('explore.metric_income')}</option>
					</select>
				</label>
				<button
					class="rounded-md border border-zinc-300 dark:border-zinc-700 px-4 py-2 text-sm font-medium text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-800"
					type="button"
					onclick={saveAsCard}
				>
					{$_('explore.save_as_card')}
				</button>
			</div>
			{#if saveNote}
				<p class="text-sm text-green-700 dark:text-green-400">{saveNote}</p>
			{/if}
			{#if saveError}
				<p class="text-sm text-red-600 dark:text-red-400">{saveError}</p>
			{/if}
		</div>

		<!-- The result area (row count, 1000-row cap note,
		     loading/error/empty lines, and the table/chart view) is
		     the shared ExploreView component — the same render the
		     dashboard cards embed. -->
		<ExploreView state={explore} names={nameMaps} />
	{/if}
</section>

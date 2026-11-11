<script lang="ts">
	import { goto } from '$app/navigation';
	import { resolve } from '$app/paths';
	import { page } from '$app/state';
	import { _ } from 'svelte-i18n';
	import { login, user } from '#lib/auth/client';
	import { ApiError } from '#lib/api/client';
	import { listAccounts, type Account } from '#lib/api/accounts';
	import { listCategories, type Category } from '#lib/api/categories';
	import {
		listDashboards,
		createDashboard,
		createWidget,
		type Dashboard
	} from '#lib/api/dashboards';
	import {
		listSavedViews,
		createSavedView,
		updateSavedView,
		deleteSavedView,
		type SavedView
	} from '#lib/api/savedviews';
	import {
		EXPLORE_STATE_KEY,
		parseState,
		serializeState,
		withoutRange,
		type ExploreState
	} from '#lib/explore/state';
	import ExploreView from '#lib/explore/ExploreView.svelte';

	// Explore page (step 6.4, refactored in 6.5 C2): a filterable,
	// groupable view of the caller's transactions. The filter state
	// lives in the URL (every view is shareable) AND is remembered as
	// the page's last settings (localStorage), so a bare /explore
	// reopens the user's last view; the row data loop,
	// the grouping/aggregation, and the table/chart render live in the
	// shared ExploreView component (lib/explore), which the dashboard
	// cards embed too. The page owns the filter bar, the view bar
	// (with the "Save to dashboard" panel), the catalogs, and the URL sync.
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

	/** The page's state: URL params WIN (a shared/explicit link is
	 * deterministic — it never merges with the remembered settings);
	 * else the remembered last settings; else the defaults (sign
	 * spending, chart pie, group category). Init once on load; every
	 * change flows back to the address bar via the sync effect. */
	let explore = $state<ExploreState>(initExploreState());

	function initExploreState(): ExploreState {
		const qs = window.location.search.slice(1);
		if (qs !== '') return parseState(qs);
		let stored = '';
		try {
			stored = window.localStorage.getItem(EXPLORE_STATE_KEY) ?? '';
		} catch {
			// storage unavailable (private mode) — the defaults
		}
		return parseState(stored);
	}

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
	// spam) and remember it as the page's last settings (a bare
	// /explore reopens this view). Runs once on load too — it
	// canonicalizes whatever URL the page opened with.
	$effect(() => {
		const qs = serializeState({
			from: explore.from,
			to: explore.to,
			account: explore.account,
			category: explore.category,
			min: explore.min,
			max: explore.max,
			q: explore.q,
			sign: explore.sign,
			chart: explore.chart,
			group: explore.group
		});
		const path = window.location.pathname;
		window.history.replaceState(window.history.state, '', qs !== '' ? `${path}?${qs}` : path);
		// Remember the full last settings (the canonical string of ALL
		// fields; '' reopens the defaults). A write failure (private
		// mode) is a silent no-op — the URL stays the shareable source
		// of truth.
		try {
			window.localStorage.setItem(EXPLORE_STATE_KEY, qs);
		} catch {
			// storage unavailable — skip the remember
		}
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

	// --- Save to a dashboard ----------------------------------------------

	// The view bar's "Save to dashboard" panel: pick one of the caller's
	// dashboards and name the widget, then POST the current view — the
	// RAW canonical serializeState(explore) string (no trim or
	// transform; '' is the default view) — as a 4-column explore widget.
	// When the caller has NO dashboards the panel instead offers a
	// new-dashboard name: it is created FIRST, then the widget is saved
	// into it. An empty name is a server 400 surfaced verbatim via
	// envelopeMessage. The green note clears itself after 2.5s.
	let savePanel = $state(false);
	let saveDashboards = $state<Dashboard[]>([]);
	let saveDashId = $state('');
	let saveName = $state('');
	let newDashName = $state('');
	let saveNote = $state<string | null>(null);
	let saveError = $state<string | null>(null);
	let saveTimer: ReturnType<typeof setTimeout> | null = null;
	let saveSeq = 0;

	// The round-17 handoff: a dashboard's "Create new view" navigates
	// here as /explore?save_to=<id>. The panel then opens pre-aimed at
	// that dashboard — the target is LOCKED (the select and the
	// new-dashboard path don't render) — and saving creates the named
	// saved view AND the widget (a copy of the state) in it, then
	// returns the caller to the dashboard. The URL-sync effect strips
	// the param from the address bar after load, so the handoff lives
	// only in this const for the page's lifetime.
	const saveToId = new URLSearchParams(window.location.search).get('save_to') ?? '';
	let handoffArmed = false;
	$effect(() => {
		if (saveToId === '' || handoffArmed) return;
		if ($user === null) return; // not signed in yet — arm on sign-in
		handoffArmed = true;
		saveName = '';
		saveError = null;
		savePanel = true;
		saveDashId = saveToId;
		// For the target's NAME (the muted "saving to" line) — saving
		// itself needs only the id, so a failed load doesn't block.
		void loadSaveDashboards();
	});

	/** Loads the caller's dashboards for the panel's select (one load
	 * per open; stale loads are dropped). A failed load leaves the
	 * panel on the create-a-dashboard path — saving still works. */
	async function loadSaveDashboards(): Promise<void> {
		const s = ++saveSeq;
		try {
			const ds = await listDashboards();
			if (s !== saveSeq) return; // stale
			saveDashboards = ds;
			if (saveDashId === '' && ds.length > 0) saveDashId = ds[0].id;
		} catch {
			if (s !== saveSeq) return; // stale
			saveDashboards = [];
		}
	}

	/** Toggles the panel; opening it re-loads the dashboards and
	 * clears the widget name (the success note persists across
	 * toggles until its timer runs). */
	function toggleSavePanel(): void {
		savePanel = !savePanel;
		if (savePanel) {
			saveName = '';
			saveError = null;
			void loadSaveDashboards();
		}
	}

	function closeSavePanel(): void {
		savePanel = false;
	}

	/** Saves the current view. The round-17 handoff (saveToId set)
	 * creates the named saved view AND the widget — the view's name,
	 * a COPY of the state — in the target dashboard, then returns the
	 * caller to it (the id came from the URL, so no list lookup).
	 * Otherwise the widget goes into the selected dashboard, or —
	 * when there are none (or the list failed to load) — into a
	 * freshly created one named newDashName.
	 *
	 * Round 20: the WIDGET stores the state WITHOUT its time range
	 * (the dashboard page owns its cards' range via its picker). The
	 * handoff's NAMED SAVED VIEW keeps the full state — it is a
	 * normal Explore artifact, and re-opening it in Explore restores
	 * the dates. */
	async function saveToDashboard(): Promise<void> {
		if (saveTimer !== null) {
			clearTimeout(saveTimer);
			saveTimer = null;
		}
		saveNote = null;
		saveError = null;
		const state = serializeState(explore);
		const tileState = withoutRange(state);
		try {
			if (saveToId !== '') {
				await createSavedView({ name: saveName, state });
				await createWidget(saveToId, {
					name: saveName,
					kind: 'explore',
					content: tileState,
					span: 4
				});
				await goto(`/dashboards/${saveToId}`);
				return;
			}
			const target = saveDashboards.find((d) => d.id === saveDashId);
			const dash = target ?? (await createDashboard(newDashName));
			await createWidget(dash.id, {
				name: saveName,
				kind: 'explore',
				content: tileState,
				span: 4
			});
			saveNote = $_('explore.saved', { values: { name: dash.name } });
			saveTimer = setTimeout(() => {
				saveNote = null;
			}, 2500);
			closeSavePanel();
		} catch (e) {
			saveError = envelopeMessage(e);
		}
	}

	// --- Saved views (round 16) ------------------------------------------

	// The view bar's "Saved views" panel: name + save the CURRENT view
	// (the RAW canonical serializeState(explore) string, '' = the
	// default view), then per row: Open (load the view's state back
	// into the page — the sync effect rewrites the URL + the remembered
	// settings), Update (write the current state back over the saved
	// one), Rename (inline) and Delete (confirm-gated). The list loads
	// once per open with a seq guard (the savePanel pattern); a failed
	// load leaves it empty — saving still works. The green note clears
	// itself after 2.5s.
	let viewsPanel = $state(false);
	let savedViews = $state<SavedView[]>([]);
	let viewsLoading = $state(false);
	let newViewName = $state('');
	let viewNote = $state<string | null>(null);
	let viewError = $state<string | null>(null);
	let viewTimer: ReturnType<typeof setTimeout> | null = null;
	let viewsSeq = 0;
	let renameId = $state<string | null>(null);
	let renameDraft = $state('');

	/** Loads the caller's saved views (one load per open; stale loads
	 * are dropped). A failed load leaves the list empty. */
	async function loadSavedViews(): Promise<void> {
		const s = ++viewsSeq;
		viewsLoading = true;
		try {
			const vs = await listSavedViews();
			if (s !== viewsSeq) return; // stale
			savedViews = vs;
		} catch {
			if (s !== viewsSeq) return; // stale
			savedViews = [];
		}
		if (s === viewsSeq) viewsLoading = false;
	}

	/** Toggles the panel; opening it re-loads the list and clears the
	 * new-view name (the success note persists until its timer runs). */
	function toggleViewsPanel(): void {
		viewsPanel = !viewsPanel;
		if (viewsPanel) {
			newViewName = '';
			viewError = null;
			void loadSavedViews();
		}
	}

	function closeViewsPanel(): void {
		viewsPanel = false;
	}

	/** The 2.5s self-clearing green note (restarts the timer on a
	 * repeat). */
	function showViewNote(msg: string): void {
		if (viewTimer !== null) clearTimeout(viewTimer);
		viewNote = msg;
		viewTimer = setTimeout(() => {
			viewNote = null;
		}, 2500);
	}

	/** Saves the CURRENT view under newViewName: the raw canonical
	 * serializeState(explore) string. The created row is appended; an
	 * empty name is a server 400 surfaced verbatim via envelopeMessage. */
	async function saveView(): Promise<void> {
		viewError = null;
		try {
			const v = await createSavedView({ name: newViewName, state: serializeState(explore) });
			savedViews = [...savedViews, v];
			newViewName = '';
			showViewNote($_('explore.view_saved', { values: { name: v.name } }));
		} catch (e) {
			viewError = envelopeMessage(e);
		}
	}

	/** Opens a saved view: the page's state becomes the view's stored
	 * state. The view row itself is never mutated — the dashboard
	 * widgets copy it, and so does this. */
	function openView(v: SavedView): void {
		explore = parseState(v.state);
	}

	/** Writes the CURRENT view back over the saved one (PATCH {state});
	 * the returned row replaces it in place. */
	async function updateView(v: SavedView): Promise<void> {
		viewError = null;
		try {
			const updated = await updateSavedView(v.id, { state: serializeState(explore) });
			savedViews = savedViews.map((x) => (x.id === v.id ? updated : x));
			showViewNote($_('explore.view_updated', { values: { name: updated.name } }));
		} catch (e) {
			viewError = envelopeMessage(e);
		}
	}

	/** Begins the inline rename: the row's name becomes an input. */
	function startRename(v: SavedView): void {
		renameId = v.id;
		renameDraft = v.name;
	}

	/** Commits the inline rename (PATCH {name}, trimmed); a blank or
	 * unchanged draft cancels. */
	async function commitRename(v: SavedView): Promise<void> {
		if (renameId !== v.id) return;
		renameId = null;
		const next = renameDraft.trim();
		if (next === '' || next === v.name) return; // cancel / no-op
		viewError = null;
		try {
			const updated = await updateSavedView(v.id, { name: next });
			savedViews = savedViews.map((x) => (x.id === v.id ? updated : x));
		} catch (e) {
			viewError = envelopeMessage(e);
		}
	}

	/** Deletes the saved view (confirm-gated). Widgets keep their own
	 * copy of the state, so nothing else is affected. */
	async function deleteView(v: SavedView): Promise<void> {
		if (!window.confirm($_('explore.view_delete_confirm', { values: { name: v.name } })))
			return;
		viewError = null;
		try {
			await deleteSavedView(v.id);
			savedViews = savedViews.filter((x) => x.id !== v.id);
			if (renameId === v.id) renameId = null;
		} catch (e) {
			viewError = envelopeMessage(e);
		}
	}

	/** Auto-focuses the inline rename input when it appears. */
	function autofocus(el: HTMLInputElement): void {
		el.focus();
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

	// Bumped on every multi-select toggle so the checkbox rows re-mount and
	// the DOM can never drift from state (e.g. unchecking the last explicit
	// id canonicalizes to the empty selection, which displays as all
	// checked — without the re-mount the just-unchecked box would go stale).
	let selectionTick = $state(0);

	// "Default all selected" is display-only: the state contract is
	// unchanged (empty selection = any). An empty selection renders every
	// option checked; unchecking from there materializes the complement,
	// and re-selecting the full set canonicalizes back to the empty
	// selection so URLs stay clean and later-created accounts/categories
	// are included automatically.
	function toggleSelection(current: string[], all: string[], id: string, checked: boolean): string[] {
		let next: string[];
		if (checked) {
			next = current.includes(id) ? current : [...current, id];
		} else if (current.length === 0) {
			next = all.filter((v) => v !== id);
		} else {
			next = current.filter((v) => v !== id);
		}
		if (all.length > 0 && next.length === all.length && all.every((v) => next.includes(v))) {
			return [];
		}
		return next;
	}

	function toggleAccount(id: string, checked: boolean): void {
		selectionTick++;
		explore = { ...explore, account: toggleSelection(explore.account, accounts.map((a) => a.id), id, checked) };
	}

	function toggleCategory(id: string, checked: boolean): void {
		selectionTick++;
		explore = { ...explore, category: toggleSelection(explore.category, ['none', ...categories.map((c) => c.id)], id, checked) };
	}

	function resetFilters(): void {
		explore = {
			from: '',
			to: '',
			account: [],
			category: [],
			min: '',
			max: '',
			q: '',
			sign: 'spending',
			chart: 'pie',
			group: 'category'
		};
	}
</script>

<section class="space-y-4">
	<h1 class="text-2xl font-semibold">{$_('explore.heading')}</h1>
	<p class="max-w-2xl text-sm text-zinc-600 dark:text-zinc-400">{$_('explore.intro')}</p>

	{#if !$user}
		<p class="text-sm text-zinc-600 dark:text-zinc-400">
			{$_('explore.sign_in_prompt')}
			<button class="underline" onclick={() => login(page.url.pathname + page.url.search)}>
				{$_('auth.sign_in')}
			</button>
		</p>
	{:else}
		<!-- Filter bar: the eight server-side filters. The state is
		     mirrored into the URL by the sync effect; the preset select
		     is a convenience over from/to (a hand-picked range shows
		     "Custom" and selecting it is a no-op). The Type select
		     doubles as the metric: spending → spend, income → income,
		     all → net (derived in the ExploreView). min/max are raw
		     text — a bad value is a server 400 shown verbatim; with a
		     Type set they are absolute magnitudes. -->
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
					<div
						class="h-24 min-h-16 w-56 resize-y overflow-y-auto rounded-md border border-zinc-300 bg-white p-2 text-sm dark:border-zinc-700 dark:bg-zinc-900"
					>
						{#key selectionTick}
							{#each accounts as a (a.id)}
								<label class="flex cursor-pointer items-center gap-2 py-0.5">
									<input
										type="checkbox"
										class="accent-zinc-700 dark:accent-zinc-300"
										checked={explore.account.length === 0 || explore.account.includes(a.id)}
										onchange={(e) => toggleAccount(a.id, (e.currentTarget as HTMLInputElement).checked)}
									/>
									<span class="truncate">{a.name}</span>
								</label>
							{/each}
						{/key}
					</div>
					<a class="mt-1 block text-xs text-zinc-500 dark:text-zinc-400 underline" href={resolve('/settings#accounts')}>
						{$_('common.manage_accounts')}
					</a>
				</div>
				<div class="block text-sm">
					<span class="mb-1 block font-medium">{$_('explore.category_label')}</span>
					<div
						class="h-24 min-h-16 w-56 resize-y overflow-y-auto rounded-md border border-zinc-300 bg-white p-2 text-sm dark:border-zinc-700 dark:bg-zinc-900"
					>
						{#key selectionTick}
							<label class="flex cursor-pointer items-center gap-2 py-0.5">
								<input
									type="checkbox"
									class="accent-zinc-700 dark:accent-zinc-300"
									checked={explore.category.length === 0 || explore.category.includes('none')}
									onchange={(e) => toggleCategory('none', (e.currentTarget as HTMLInputElement).checked)}
								/>
								<span class="truncate">{$_('explore.category_uncategorized')}</span>
							</label>
							{#each categories as c (c.id)}
								<label class="flex cursor-pointer items-center gap-2 py-0.5">
									<input
										type="checkbox"
										class="accent-zinc-700 dark:accent-zinc-300"
										checked={explore.category.length === 0 || explore.category.includes(c.id)}
										onchange={(e) => toggleCategory(c.id, (e.currentTarget as HTMLInputElement).checked)}
									/>
									<span class="truncate">{c.name}</span>
								</label>
							{/each}
						{/key}
					</div>
				</div>
				<label class="block text-sm">
					<span class="mb-1 block font-medium">{$_(explore.sign === 'spending' ? 'explore.min_spend_label' : explore.sign === 'income' ? 'explore.min_income_label' : 'explore.min_label')}</span>
					<input
						class="w-28 rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
						inputmode="decimal"
						bind:value={explore.min}
					/>
				</label>
				<label class="block text-sm">
					<span class="mb-1 block font-medium">{$_(explore.sign === 'spending' ? 'explore.max_spend_label' : explore.sign === 'income' ? 'explore.max_income_label' : 'explore.max_label')}</span>
					<input
						class="w-28 rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
						inputmode="decimal"
						bind:value={explore.max}
					/>
				</label>
				<label class="block text-sm">
					<span class="mb-1 block font-medium">{$_('explore.type_label')}</span>
					<select
						class="w-36 rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
						value={explore.sign}
						onchange={(e) => (explore = { ...explore, sign: e.currentTarget.value })}
					>
						<option value="">{$_('explore.type_all')}</option>
						<option value="spending">{$_('explore.type_spending')}</option>
						<option value="income">{$_('explore.type_income')}</option>
					</select>
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

		<!-- View bar: chart kind / grouping (URL-only fields — they never
		     refetch; the metric is derived from the Type filter above)
		     plus the "Save to dashboard" button (which opens the inline
		     save panel). The row count and the 1000-row cap note render
		     inside the ExploreView (below). -->
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
				<button
					class="rounded-md border border-zinc-300 dark:border-zinc-700 px-4 py-2 text-sm font-medium text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-800"
					type="button"
					onclick={toggleSavePanel}
				>
					{$_('explore.save_to_dashboard')}
				</button>
				<button
					class="rounded-md border border-zinc-300 dark:border-zinc-700 px-4 py-2 text-sm font-medium text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-800"
					type="button"
					onclick={toggleViewsPanel}
				>
					{$_('explore.saved_views')}
				</button>
			</div>
			{#if savePanel}
				<!-- The save panel: the dashboard select (or a
				     new-dashboard name when the caller has none),
				     the widget name, save / cancel. The round-17
				     handoff (?save_to=<id>) locks the target:
				     the select/new-dashboard block NEVER renders,
				     replaced by a muted "saving to" line
				     (shown once the name has loaded — a
				     list failure doesn't block saving,
				     which needs only the id), and saving
				     also creates the named saved view,
				     then returns to the dashboard. -->
				{@const handoff = saveToId !== ''}
				{@const lockedDash = handoff
					? saveDashboards.find((d) => d.id === saveToId)
					: undefined}
				<div
					class="mt-4 flex flex-wrap items-end gap-3 border-t border-zinc-200 dark:border-zinc-800 pt-4"
				>
					{#if handoff}
						{#if lockedDash !== undefined}
							<p class="self-center text-sm text-zinc-500 dark:text-zinc-400">
								{$_('explore.save_handoff_target', { values: { name: lockedDash.name } })}
							</p>
						{/if}
					{:else if saveDashboards.length > 0}
						<label class="block text-sm">
							<span class="mb-1 block font-medium">{$_('explore.save_dashboard')}</span>
							<select
								class="w-56 rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
								bind:value={saveDashId}
							>
								{#each saveDashboards as d (d.id)}
									<option value={d.id}>{d.name}</option>
								{/each}
							</select>
						</label>
					{:else}
						<label class="block text-sm">
							<span class="mb-1 block font-medium">{$_('explore.save_new_dashboard')}</span>
							<input
								class="w-56 rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
								bind:value={newDashName}
								placeholder={$_('explore.save_new_dashboard_placeholder')}
							/>
						</label>
					{/if}
					<label class="block text-sm">
						<span class="mb-1 block font-medium">{$_('explore.save_name')}</span>
						<input
							class="w-56 rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
							bind:value={saveName}
							placeholder={$_('explore.save_name_placeholder')}
						/>
					</label>
					<button
						class="rounded-md border border-zinc-300 dark:border-zinc-700 px-4 py-2 text-sm font-medium text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-800"
						type="button"
						onclick={() => void saveToDashboard()}
					>
						{$_('explore.save_button')}
					</button>
					<button
						class="self-center text-sm font-medium text-zinc-600 dark:text-zinc-400 underline"
						type="button"
						onclick={closeSavePanel}
					>
						{$_('explore.save_cancel')}
					</button>
				</div>
			{/if}
			{#if viewsPanel}
				<!-- The saved-views panel (round 16): name + save the
				     current view, then the list — per row Open /
				     Update / Rename (inline) / Delete. -->
				<div
					class="mt-4 border-t border-zinc-200 dark:border-zinc-800 pt-4"
				>
					<div class="flex flex-wrap items-end gap-3">
						<label class="block text-sm">
							<span class="mb-1 block font-medium">{$_('explore.view_name')}</span>
							<input
								class="w-56 rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
								bind:value={newViewName}
								placeholder={$_('explore.view_name_placeholder')}
							/>
						</label>
						<button
							class="rounded-md border border-zinc-300 dark:border-zinc-700 px-4 py-2 text-sm font-medium text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-800"
							type="button"
							onclick={() => void saveView()}
						>
							{$_('explore.view_save_button')}
						</button>
						<button
							class="self-center text-sm font-medium text-zinc-600 dark:text-zinc-400 underline"
							type="button"
							onclick={closeViewsPanel}
						>
							{$_('explore.view_cancel')}
						</button>
					</div>
					{#if viewsLoading}
						<p class="mt-3 text-sm text-zinc-600 dark:text-zinc-400">
							{$_('explore.view_loading')}
						</p>
					{:else if savedViews.length > 0}
						<ul class="mt-3 space-y-1.5">
							{#each savedViews as v (v.id)}
								<li class="flex flex-wrap items-center gap-2">
									{#if renameId === v.id}
										<input
											class="w-48 rounded-md border border-zinc-300 dark:border-zinc-700 px-2 py-1 text-sm"
											bind:value={renameDraft}
											use:autofocus
											onkeydown={(e) => {
												if (e.key === 'Enter') void commitRename(v);
												if (e.key === 'Escape') renameId = null;
											}}
										/>
									{:else}
										<span class="text-sm font-medium">{v.name}</span>
									{/if}
									<button
										class="rounded-md border border-zinc-300 dark:border-zinc-700 px-2.5 py-1 text-xs font-medium text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-800"
										type="button"
										onclick={() => openView(v)}
									>
										{$_('explore.view_open')}
									</button>
									<button
										class="rounded-md border border-zinc-300 dark:border-zinc-700 px-2.5 py-1 text-xs font-medium text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-800"
										type="button"
										onclick={() => void updateView(v)}
									>
										{$_('explore.view_update')}
									</button>
									<button
										class="rounded-md border border-zinc-300 dark:border-zinc-700 px-2.5 py-1 text-xs font-medium text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-800"
										type="button"
										onclick={() => startRename(v)}
									>
										{$_('explore.view_rename')}
									</button>
									<button
										class="rounded-md border border-zinc-300 dark:border-zinc-700 px-2.5 py-1 text-xs font-medium text-red-600 dark:text-red-400 hover:bg-zinc-100 dark:hover:bg-zinc-800"
										type="button"
										onclick={() => void deleteView(v)}
									>
										{$_('explore.view_delete')}
									</button>
								</li>
							{/each}
						</ul>
					{:else}
						<p class="mt-3 text-sm text-zinc-600 dark:text-zinc-400">
							{$_('explore.view_empty')}
						</p>
					{/if}
				</div>
			{/if}
			{#if saveNote}
				<p class="text-sm text-green-700 dark:text-green-400">{saveNote}</p>
			{/if}
			{#if saveError}
				<p class="text-sm text-red-600 dark:text-red-400">{saveError}</p>
			{/if}
			{#if viewNote}
				<p class="text-sm text-green-700 dark:text-green-400">{viewNote}</p>
			{/if}
			{#if viewError}
				<p class="text-sm text-red-600 dark:text-red-400">{viewError}</p>
			{/if}
		</div>

		<!-- The result area (row count, 1000-row cap note,
		     loading/error/empty lines, and the table/chart view) is
		     the shared ExploreView component — the same render the
		     dashboard cards embed. -->
		<ExploreView state={explore} names={nameMaps} />
	{/if}
</section>

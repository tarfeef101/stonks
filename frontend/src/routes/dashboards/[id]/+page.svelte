<script lang="ts">
	import { resolve } from '$app/paths';
	import { page } from '$app/state';
	import { _ } from 'svelte-i18n';
	import DOMPurify from 'dompurify';
	import { marked } from 'marked';
	import { user } from '#lib/auth/client';
	import { ApiError } from '#lib/api/client';
	import {
		listDashboards,
		listWidgets,
		createWidget,
		updateWidget,
		deleteWidget,
		setWidgetOrder,
		type Dashboard,
		type DashboardWidget,
		type WidgetKind
	} from '#lib/api/dashboards';
	import { loadCatalogs } from '#lib/explore/catalogs';
	import { parseState, serializeStateForLink, type Names } from '#lib/explore/state';
	import ExploreView from '#lib/explore/ExploreView.svelte';

	// Dashboard editor (round 6): one dashboard's 12-col widget grid.
	// A widget is either a card whose body is the shared ExploreView
	// over the widget's stored (canonical) explore state, or a
	// markdown text card (marked → DOMPurify → @html — user text is
	// untrusted). Widget ops: resize (span presets), move left/right
	// (swap + PUT /order — the round-5 cards page pattern), edit
	// (rename for explore widgets; rename + content for markdown),
	// confirm-gated delete. "Open in Explore" re-serializes the stored
	// state with the sign ALWAYS explicit — a bare /explore opens the
	// remembered last view, not the widget's.
	//
	// There is no GET /v1/dashboards/{id} route — the dashboard row is
	// resolved by id from the list; an unknown id means "not found".

	const dashboardId = $derived(page.params.id ?? '');

	let dashboard = $state<Dashboard | null>(null);
	let widgets = $state<DashboardWidget[]>([]);
	let names = $state<Names>({ accounts: {}, categories: {} });
	let loading = $state(true);
	let loadError = $state<string | null>(null);
	let notFound = $state(false);

	// Shared note for widget-op failures: NOT auto-cleared — the next
	// successful op or a new op start clears it.
	let opError = $state<string | null>(null);

	let seq = 0;
	let editingId = $state<string | null>(null);
	let draftName = $state('');
	let draftContent = $state('');

	let adding = $state(false);
	let addKind = $state<WidgetKind | null>(null);
	let addName = $state('');
	let addContent = $state('');
	let addError = $state<string | null>(null);

	// --- Load --------------------------------------------------------------

	/** The page's single load for THIS dashboard id: the dashboards
	 * list first (no single-dashboard GET route), and only when the
	 * id resolves, its widgets + the catalog name maps (one round).
	 * A seq guard drops stale loads. */
	async function load(id: string): Promise<void> {
		const s = ++seq;
		loading = true;
		loadError = null;
		notFound = false;
		try {
			const ds = await listDashboards();
			if (s !== seq) return; // stale
			dashboard = ds.find((d) => d.id === id) ?? null;
			if (dashboard === null) {
				widgets = [];
				loading = false;
				notFound = true;
				return;
			}
			const [ws, n] = await Promise.all([listWidgets(id), loadCatalogs()]);
			if (s !== seq) return; // stale
			widgets = ws;
			names = n;
			loading = false;
		} catch (e) {
			if (s !== seq) return; // stale
			loading = false;
			loadError = envelopeMessage(e);
		}
	}

	// One load per identity + dashboard change (a signed-out caller
	// sees the sign-in gate).
	$effect(() => {
		const sub = $user?.profile.sub ?? null;
		const id = page.params.id ?? '';
		if (sub === null || id === '') {
			seq++; // invalidate any in-flight load
			dashboard = null;
			widgets = [];
			loading = false;
			loadError = null;
			notFound = id !== '';
			return;
		}
		void load(id);
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

	// --- Grid ---------------------------------------------------------------

	// The widget's grid footprint: TAILWIND JIT needs the class
	// literals spelled out (a runtime-built 'col-span-' + n would be
	// purged). The size select's presets are 3/4/6/8/12; any other
	// stored span (the API allows 1..12) still gets its class.
	const SPAN_CLASSES: Record<number, string> = {
		1: 'md:col-span-1',
		2: 'md:col-span-2',
		3: 'md:col-span-3',
		4: 'md:col-span-4',
		5: 'md:col-span-5',
		6: 'md:col-span-6',
		7: 'md:col-span-7',
		8: 'md:col-span-8',
		9: 'md:col-span-9',
		10: 'md:col-span-10',
		11: 'md:col-span-11',
		12: 'md:col-span-12'
	};
	const SPAN_PRESETS = [3, 4, 6, 8, 12];

	function spanClass(w: DashboardWidget): string {
		return SPAN_CLASSES[w.span] ?? 'md:col-span-6';
	}

	/** The size select's options: the presets, plus the widget's own
	 * span when it is not a preset (the select must show the truth). */
	function spanOptions(span: number): number[] {
		return SPAN_PRESETS.includes(span)
			? SPAN_PRESETS
			: [...SPAN_PRESETS, span].sort((a, b) => a - b);
	}

	/** Renders a markdown widget's body: marked → DOMPurify
	 * (sanitized before the @html — user-authored text is not
	 * trusted). Runs on each render of a markdown card (cheap for
	 * note-sized text — same stance as parseState in the explore
	 * cards). */
	function renderMarkdown(src: string): string {
		return DOMPurify.sanitize(marked.parse(src, { async: false }) as string);
	}

	// --- Widget ops ---------------------------------------------------------

	/** Resizes the widget (the size select); the returned row
	 * replaces it in place. */
	async function resize(w: DashboardWidget, span: number): Promise<void> {
		if (span === w.span) return;
		opError = null;
		try {
			const updated = await updateWidget(dashboardId, w.id, { span });
			widgets = widgets.map((x) => (x.id === w.id ? updated : x));
		} catch (e) {
			opError = envelopeMessage(e);
		}
	}

	/** Swaps the widget with its neighbour (no-op at the edges) and
	 * persists the new order — the swap + PUT /order pattern of the
	 * round-5 cards page. */
	async function move(w: DashboardWidget, dir: -1 | 1): Promise<void> {
		const i = widgets.findIndex((x) => x.id === w.id);
		const j = i + dir;
		if (i === -1 || j < 0 || j >= widgets.length) return; // no neighbour
		const next = [...widgets];
		[next[i], next[j]] = [next[j], next[i]];
		opError = null;
		try {
			await setWidgetOrder(dashboardId, next.map((x) => x.id));
			widgets = next.map((x, k) => ({ ...x, sort_order: k + 1 }));
		} catch (e) {
			opError = envelopeMessage(e);
		}
	}

	function startEdit(w: DashboardWidget): void {
		editingId = w.id;
		draftName = w.name;
		draftContent = w.content;
	}

	function cancelEdit(): void {
		editingId = null;
	}

	/** Commits (or cancels) the widget edit: explore widgets rename
	 * ONLY (editing the saved query in place is a contract non-goal —
	 * a changed view is saved as a NEW widget from the Explore page);
	 * markdown widgets rename + re-edit the content. An empty or
	 * unchanged draft just cancels; otherwise the returned row
	 * replaces the local widget. */
	async function saveEdit(w: DashboardWidget): Promise<void> {
		if (editingId !== w.id) return;
		editingId = null;
		const patch: { name?: string; content?: string } = {};
		if (draftName !== '' && draftName !== w.name) patch.name = draftName;
		if (w.kind === 'markdown' && draftContent !== w.content) patch.content = draftContent;
		if (Object.keys(patch).length === 0) return; // cancel / no-op
		opError = null;
		try {
			const updated = await updateWidget(dashboardId, w.id, patch);
			widgets = widgets.map((x) => (x.id === w.id ? updated : x));
		} catch (e) {
			opError = envelopeMessage(e);
		}
	}

	/** Deletes the widget (confirm-gated). */
	async function removeWidget(w: DashboardWidget): Promise<void> {
		if (!window.confirm($_('dashboards.widget_delete_confirm', { values: { name: w.name } })))
			return;
		opError = null;
		try {
			await deleteWidget(dashboardId, w.id);
			widgets = widgets.filter((x) => x.id !== w.id);
			if (dashboard !== null)
				dashboard = { ...dashboard, widget_count: dashboard.widget_count - 1 };
		} catch (e) {
			opError = envelopeMessage(e);
		}
	}

	// --- Add widget ----------------------------------------------------------

	/** Opens the add-widget card with the kind preselected: "Explore
	 * view" seeds content '' (the default view), "Text (markdown)"
	 * also opens the content textarea. */
	function startAdd(kind: WidgetKind): void {
		adding = true;
		addKind = kind;
		addName = '';
		addContent = '';
		addError = null;
	}

	function cancelAdd(): void {
		adding = false;
		addKind = null;
	}

	/** Creates the widget from the add card (new widgets span 4
	 * columns); the returned row is appended to the grid. */
	async function submitAdd(): Promise<void> {
		if (addKind === null) return;
		addError = null;
		try {
			const w = await createWidget(dashboardId, {
				name: addName,
				kind: addKind,
				content: addKind === 'markdown' ? addContent : '',
				span: 4
			});
			widgets = [...widgets, w];
			if (dashboard !== null)
				dashboard = { ...dashboard, widget_count: dashboard.widget_count + 1 };
			cancelAdd();
		} catch (e) {
			addError = envelopeMessage(e);
		}
	}

	/** Auto-focuses the inline edit / add-name inputs when they appear. */
	function autofocus(el: HTMLInputElement): void {
		el.focus();
	}
</script>

<section class="space-y-4">
	<!-- Header: back to the list + the dashboard's name (the name is
	     resolved from the list — there is no single-dashboard GET). -->
	<div class="flex flex-wrap items-baseline gap-3">
		<a
			href={resolve('/dashboards')}
			class="text-sm font-medium text-zinc-500 dark:text-zinc-400 underline hover:text-zinc-900 dark:hover:text-zinc-100"
		>
			{$_('dashboards.heading')}
		</a>
		{#if dashboard}
			<h1 class="text-2xl font-semibold">{dashboard.name}</h1>
		{/if}
	</div>

	{#if !$user}
		<p class="text-sm text-zinc-600 dark:text-zinc-400">{$_('dashboards.sign_in')}</p>
	{:else if loading}
		<p class="text-sm text-zinc-600 dark:text-zinc-400">{$_('dashboards.loading')}</p>
	{:else if notFound}
		<p class="text-sm text-zinc-600 dark:text-zinc-400">{$_('dashboards.not_found')}</p>
	{:else if loadError}
		<p class="text-sm text-red-600 dark:text-red-400">{loadError}</p>
	{:else}
		{#if opError}
			<p class="text-sm text-red-600 dark:text-red-400">{opError}</p>
		{/if}

		<!-- The widget grid: 12 columns at md+, stacked on small
		     screens. Each card's head carries the name (kind badge),
		     the size select, move / open / edit / delete; the body is
		     the live Explore view, the sanitized markdown, or the
		     markdown edit textarea. -->
		<div class="grid grid-cols-1 gap-4 md:grid-cols-12">
			{#each widgets as w (w.id)}
				<div
					class="flex flex-col rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 {spanClass(w)}"
				>
					<div
						class="flex flex-wrap items-center justify-between gap-2 border-b border-zinc-200 dark:border-zinc-800 px-3 py-2"
					>
						<div class="flex min-w-0 flex-1 items-center gap-2">
							{#if editingId === w.id}
								<input
									class="min-w-0 flex-1 rounded-md border border-zinc-300 dark:border-zinc-700 px-2 py-1 text-sm"
									bind:value={draftName}
									use:autofocus
									aria-label={$_('dashboards.widget_name')}
									onkeydown={(e) => {
										if (e.key === 'Enter') void saveEdit(w);
										if (e.key === 'Escape') cancelEdit();
									}}
								/>
							{:else}
								<h2 class="truncate text-sm font-semibold">{w.name}</h2>
								<span
									class="shrink-0 rounded bg-zinc-100 dark:bg-zinc-800 px-1.5 py-0.5 text-xs text-zinc-500 dark:text-zinc-400"
								>
									{w.kind === 'explore'
										? $_('dashboards.kind_explore')
										: $_('dashboards.kind_markdown')}
								</span>
							{/if}
						</div>
						<div class="flex shrink-0 flex-wrap items-center gap-3 text-sm">
							{#if editingId === w.id}
								<button
									class="font-medium text-emerald-600 dark:text-emerald-400 underline"
									type="button"
									onclick={() => void saveEdit(w)}
								>
									{$_('dashboards.widget_save')}
								</button>
								<button
									class="font-medium text-zinc-600 dark:text-zinc-400 underline"
									type="button"
									onclick={cancelEdit}
								>
									{$_('dashboards.widget_cancel')}
								</button>
							{:else}
								<select
									class="rounded-md border border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 px-1.5 py-1 text-xs"
									value={w.span}
									aria-label={$_('dashboards.widget_size')}
									onchange={(e) => void resize(w, Number(e.currentTarget.value))}
								>
									{#each spanOptions(w.span) as n (n)}
										<option value={n}>{$_('dashboards.size_n', { values: { n } })}</option>
									{/each}
								</select>
								<button
									class="font-medium text-zinc-600 dark:text-zinc-400 underline"
									type="button"
									aria-label={$_('dashboards.widget_move_left')}
									disabled={widgets[0]?.id === w.id}
									onclick={() => void move(w, -1)}
								>
									{$_('dashboards.widget_move_left')}
								</button>
								<button
									class="font-medium text-zinc-600 dark:text-zinc-400 underline"
									type="button"
									aria-label={$_('dashboards.widget_move_right')}
									disabled={widgets.at(-1)?.id === w.id}
									onclick={() => void move(w, 1)}
								>
									{$_('dashboards.widget_move_right')}
								</button>
								{#if w.kind === 'explore'}
									<a
										href={resolve('/explore') + '?' + serializeStateForLink(parseState(w.content))}
										class="font-medium text-zinc-600 dark:text-zinc-400 underline"
									>
										{$_('dashboards.widget_open_explore')}
									</a>
								{/if}
								<button
									class="font-medium text-zinc-600 dark:text-zinc-400 underline"
									type="button"
									aria-label={$_('dashboards.widget_edit')}
									onclick={() => startEdit(w)}
								>
									{$_('dashboards.widget_edit')}
								</button>
								<button
									class="font-medium text-red-600 dark:text-red-400 underline"
									type="button"
									aria-label={$_('dashboards.widget_delete')}
									onclick={() => void removeWidget(w)}
								>
									{$_('dashboards.widget_delete')}
								</button>
							{/if}
						</div>
					</div>
					<div class="flex-1 p-3">
						{#if editingId === w.id && w.kind === 'markdown'}
							<textarea
								class="w-full min-h-32 rounded-md border border-zinc-300 dark:border-zinc-700 px-2 py-1.5 font-mono text-xs"
								bind:value={draftContent}
								onkeydown={(e) => {
									if (e.key === 'Escape') cancelEdit();
									if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) void saveEdit(w);
								}}
							></textarea>
						{:else if w.kind === 'explore'}
							<ExploreView state={parseState(w.content)} names={names} />
						{:else}
							<article class="text-sm">{@html renderMarkdown(w.content)}</article>
						{/if}
					</div>
				</div>
			{:else}
				<p class="text-sm text-zinc-500 dark:text-zinc-400 md:col-span-12"
					>{$_('dashboards.no_widgets')}</p
				>
			{/each}
		</div>
		<!-- Add-widget row: a kind picker; picking one opens the add
		     card (a name + — for markdown — a content textarea).
		     New widgets span 4 columns. -->
		{#if adding}
			<div
				class="space-y-3 rounded-lg border border-dashed border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 p-4"
			>
				{#if addError}
					<p class="text-sm text-red-600 dark:text-red-400">{addError}</p>
				{/if}
				<div class="flex flex-wrap items-center gap-3">
					<span class="text-sm font-medium text-zinc-600 dark:text-zinc-400">
						{addKind === 'explore'
							? $_('dashboards.widget_add_explore')
							: $_('dashboards.widget_add_markdown')}
					</span>
					<input
						class="w-64 rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5 text-sm"
						bind:value={addName}
						placeholder={$_('dashboards.widget_name_placeholder')}
						use:autofocus
						onkeydown={(e) => {
							if (e.key === 'Enter') void submitAdd();
						}}
					/>
					<button
						class="rounded-md border border-zinc-300 dark:border-zinc-700 px-4 py-2 text-sm font-medium text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-800"
						type="button"
						onclick={() => void submitAdd()}
					>
						{$_('dashboards.widget_add_button')}
					</button>
					<button
						class="text-sm font-medium text-zinc-600 dark:text-zinc-400 underline"
						type="button"
						onclick={cancelAdd}
					>
						{$_('dashboards.widget_cancel')}
					</button>
				</div>
				{#if addKind === 'markdown'}
					<textarea
						class="w-full min-h-32 rounded-md border border-zinc-300 dark:border-zinc-700 px-2 py-1.5 font-mono text-xs"
						bind:value={addContent}
						placeholder={$_('dashboards.widget_content_placeholder')}
					></textarea>
				{/if}
			</div>
		{:else}
			<div
				class="flex flex-wrap items-center gap-3 rounded-lg border border-dashed border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 p-4"
			>
				<span class="text-sm font-medium text-zinc-600 dark:text-zinc-400">
					{$_('dashboards.widget_add')}
				</span>
				<button
					class="rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5 text-sm"
					type="button"
					onclick={() => startAdd('explore')}
				>
					{$_('dashboards.widget_add_explore')}
				</button>
				<button
					class="rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5 text-sm"
					type="button"
					onclick={() => startAdd('markdown')}
				>
					{$_('dashboards.widget_add_markdown')}
				</button>
			</div>
		{/if}
	{/if}
</section>

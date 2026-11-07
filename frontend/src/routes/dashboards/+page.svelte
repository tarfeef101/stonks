<script lang="ts">
	import { resolve } from '$app/paths';
	import { _ } from 'svelte-i18n';
	import { user } from '#lib/auth/client';
	import { ApiError } from '#lib/api/client';
	import {
		listDashboardCards,
		patchDashboardCard,
		deleteDashboardCard,
		setDashboardCardOrder,
		type DashboardCard
	} from '#lib/api/dashboardCards';
	import { loadCatalogs } from '#lib/explore/catalogs';
	import { parseState, type Names } from '#lib/explore/state';
	import ExploreView from '#lib/explore/ExploreView.svelte';

	// Dashboards page (step 6.5 C2): the caller's saved Explore views as
	// a live card grid. Each card embeds the shared ExploreView over the
	// card's stored (canonical) explore_state, and the whole card body
	// is a CLICK-THROUGH: an <a> to the same view on the Explore page
	// (the href uses the RAW stored string — '' is the default view;
	// the controls stay OUTSIDE the link, since interactive content
	// inside a link is invalid). Ops: inline rename, move up/down
	// (reorder via PUT /order), confirm-gated delete. Every op surfaces
	// a 3.1 envelope message verbatim in the shared opError note.
	//
	// Note: parseState runs on each render, yielding a fresh state
	// object; a page-level state change can trigger one redundant
	// refetch inside a card — harmless (the view is seq-guarded).

	let cards = $state<DashboardCard[]>([]);
	let names = $state<Names>({ accounts: {}, categories: {} });
	let loading = $state(true);
	let loadError = $state<string | null>(null);

	// Shared note for rename/delete/order failures: NOT auto-cleared —
	// the next successful op or a new op start clears it.
	let opError = $state<string | null>(null);

	let seq = 0;
	let renamingId = $state<string | null>(null);
	let draftName = $state('');

	/** The page's single load: the cards + the catalog name maps (one
	 * round). A seq guard drops stale loads. */
	async function load(): Promise<void> {
		const s = ++seq;
		loading = true;
		loadError = null;
		try {
			const [cs, n] = await Promise.all([listDashboardCards(), loadCatalogs()]);
			if (s !== seq) return; // stale
			cards = cs;
			names = n;
			loadError = null;
			loading = false;
		} catch (e) {
			if (s !== seq) return; // stale
			loading = false;
			loadError = envelopeMessage(e);
		}
	}

	// One load per identity change (a signed-out caller has no cards).
	$effect(() => {
		const sub = $user?.profile.sub ?? null;
		if (sub === null) {
			seq++; // invalidate any in-flight load
			cards = [];
			loading = false;
			loadError = null;
			return;
		}
		void load();
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

	// --- Ops --------------------------------------------------------------

	function startRename(card: DashboardCard): void {
		renamingId = card.id;
		draftName = card.name;
	}

	function cancelRename(): void {
		renamingId = null;
	}

	/** Commits (or cancels) the inline rename: an empty or unchanged
	 * draft just cancels; otherwise the patch's returned row replaces
	 * the local card in place. */
	async function saveRename(card: DashboardCard): Promise<void> {
		if (renamingId !== card.id) return;
		renamingId = null;
		if (draftName === '' || draftName === card.name) return; // cancel / no-op
		opError = null;
		try {
			const updated = await patchDashboardCard(card.id, { name: draftName });
			cards = cards.map((c) => (c.id === card.id ? updated : c));
		} catch (e) {
			opError = envelopeMessage(e);
		}
	}

	/** Swaps the card with its neighbour (no-op at the edges) and
	 * persists the new order; on success the local cards are replaced
	 * with the reordered array (each sort_order = its new 1-based
	 * position), on failure the original array is kept. */
	async function move(card: DashboardCard, dir: -1 | 1): Promise<void> {
		const i = cards.findIndex((c) => c.id === card.id);
		const j = i + dir;
		if (i === -1 || j < 0 || j >= cards.length) return; // no neighbour
		const next = [...cards];
		[next[i], next[j]] = [next[j], next[i]];
		opError = null;
		try {
			await setDashboardCardOrder(next.map((c) => c.id));
			cards = next.map((c, k) => ({ ...c, sort_order: k + 1 }));
		} catch (e) {
			opError = envelopeMessage(e);
		}
	}

	/** Deletes the card (confirm-gated). */
	async function removeCard(card: DashboardCard): Promise<void> {
		if (!window.confirm($_('dashboards.delete_confirm'))) return;
		opError = null;
		try {
			await deleteDashboardCard(card.id);
			cards = cards.filter((c) => c.id !== card.id);
		} catch (e) {
			opError = envelopeMessage(e);
		}
	}

	/** Auto-focuses the inline rename input when it appears. */
	function autofocus(el: HTMLInputElement): void {
		el.focus();
	}
</script>

<section class="space-y-4">
	<h1 class="text-2xl font-semibold">{$_('dashboards.heading')}</h1>

	{#if !$user}
		<p class="text-sm text-zinc-600">{$_('dashboards.sign_in')}</p>
	{:else if loading && cards.length === 0}
		<p class="text-sm text-zinc-600">{$_('dashboards.loading')}</p>
	{:else if loadError}
		<p class="text-sm text-red-600">{loadError}</p>
	{:else if cards.length === 0}
		<!-- No cards yet: point at the Explore page's "Save as card". -->
		<div class="rounded-lg border border-zinc-200 bg-white p-5">
			<p class="text-sm text-zinc-500">{$_('dashboards.empty')}</p>
			<a href={resolve('/explore')} class="underline">{$_('dashboards.empty_cta')}</a>
		</div>
	{:else}
		{#if opError}
			<p class="text-sm text-red-600">{opError}</p>
		{/if}
		<div class="grid grid-cols-1 gap-4 lg:grid-cols-2 2xl:grid-cols-3">
			{#each cards as card, i (card.id)}
				<article class="space-y-3 rounded-lg border border-zinc-200 bg-white p-4">
					<!-- Header row: the name (or the inline rename
					     input) plus the card's controls. -->
					<div class="flex flex-wrap items-start justify-between gap-3">
						{#if renamingId === card.id}
							<input
								class="w-full rounded-md border border-zinc-300 px-3 py-1.5 text-sm"
								bind:value={draftName}
								use:autofocus
								onkeydown={(e) => {
									if (e.key === 'Enter') void saveRename(card);
									if (e.key === 'Escape') cancelRename();
								}}
								onblur={() => void saveRename(card)}
							/>
						{:else}
							<h2 class="text-base font-semibold">{card.name}</h2>
						{/if}
						<div class="flex shrink-0 items-center gap-3">
							<button
								class="text-sm font-medium text-zinc-600 underline hover:text-zinc-900"
								type="button"
								aria-label={$_('dashboards.rename')}
								onclick={() => startRename(card)}
							>
								{$_('dashboards.rename')}
							</button>
							<button
								class="text-sm font-medium text-zinc-600 underline hover:text-zinc-900 disabled:text-zinc-300 disabled:no-underline"
								type="button"
								aria-label={$_('dashboards.move_up')}
								disabled={i === 0}
								onclick={() => void move(card, -1)}
							>
								{$_('dashboards.move_up')}
							</button>
							<button
								class="text-sm font-medium text-zinc-600 underline hover:text-zinc-900 disabled:text-zinc-300 disabled:no-underline"
								type="button"
								aria-label={$_('dashboards.move_down')}
								disabled={i === cards.length - 1}
								onclick={() => void move(card, 1)}
							>
								{$_('dashboards.move_down')}
							</button>
							<button
								class="text-sm font-medium text-red-600 underline hover:text-red-700"
								type="button"
								aria-label={$_('dashboards.delete')}
								onclick={() => void removeCard(card)}
							>
								{$_('dashboards.delete')}
							</button>
						</div>
					</div>

					<!-- The view area, and the CLICK-THROUGH: the whole
					     body links to the same view on the Explore page
					     (the RAW stored explore_state — '' is the default
					     view). The card's controls stay OUTSIDE the <a>. -->
					<a
						class="block"
						href={resolve(
							card.explore_state === '' ? '/explore' : `/explore?${card.explore_state}`
						)}
						aria-label={$_('dashboards.open', { values: { name: card.name } })}
					>
						<ExploreView state={parseState(card.explore_state)} {names} />
					</a>
				</article>
			{/each}
		</div>
	{/if}
</section>

<script lang="ts">
	import { resolve } from '$app/paths';
	import { _ } from 'svelte-i18n';
	import { user } from '#lib/auth/client';
	import { ApiError } from '#lib/api/client';
	import {
		listDashboards,
		createDashboard,
		updateDashboard,
		deleteDashboard,
		type Dashboard
	} from '#lib/api/dashboards';

	// Dashboards page (round 6): the list of the caller's dashboards —
	// one row each (name, widget count, open / rename / delete) plus a
	// new-dashboard add row. A dashboard's widgets live on its own
	// page (/dashboards/{id}, the editor). Every op surfaces a 3.1
	// envelope message verbatim in the shared opError note.

	let dashboards = $state<Dashboard[]>([]);
	let loading = $state(true);
	let loadError = $state<string | null>(null);

	// Shared note for create/rename/delete failures: NOT auto-cleared —
	// the next successful op or a new op start clears it.
	let opError = $state<string | null>(null);

	let seq = 0;
	let renamingId = $state<string | null>(null);
	let draftName = $state('');
	let newName = $state('');

	/** The page's single load: the dashboards list. A seq guard drops
	 * stale loads. */
	async function load(): Promise<void> {
		const s = ++seq;
		loading = true;
		loadError = null;
		try {
			const ds = await listDashboards();
			if (s !== seq) return; // stale
			dashboards = ds;
			loading = false;
		} catch (e) {
			if (s !== seq) return; // stale
			loading = false;
			loadError = envelopeMessage(e);
		}
	}

	// One load per identity change (a signed-out caller has no
	// dashboards).
	$effect(() => {
		const sub = $user?.profile.sub ?? null;
		if (sub === null) {
			seq++; // invalidate any in-flight load
			dashboards = [];
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

	function startRename(d: Dashboard): void {
		renamingId = d.id;
		draftName = d.name;
	}

	function cancelRename(): void {
		renamingId = null;
	}

	/** Commits (or cancels) the inline rename: an empty or unchanged
	 * draft just cancels; otherwise the patch's returned row replaces
	 * the local dashboard in place. */
	async function saveRename(d: Dashboard): Promise<void> {
		if (renamingId !== d.id) return;
		renamingId = null;
		if (draftName === '' || draftName === d.name) return; // cancel / no-op
		opError = null;
		try {
			const updated = await updateDashboard(d.id, { name: draftName });
			dashboards = dashboards.map((x) => (x.id === d.id ? updated : x));
		} catch (e) {
			opError = envelopeMessage(e);
		}
	}

	/** Creates a dashboard from the add row; the returned row is
	 * appended to the list (a new dashboard has no widgets). A blank
	 * name is a server 400 surfaced via opError. */
	async function addDashboard(): Promise<void> {
		opError = null;
		try {
			const d = await createDashboard(newName);
			dashboards = [...dashboards, d];
			newName = '';
		} catch (e) {
			opError = envelopeMessage(e);
		}
	}

	/** Deletes the dashboard AND its widgets (confirm-gated). */
	async function removeDashboard(d: Dashboard): Promise<void> {
		if (!window.confirm($_('dashboards.delete_confirm', { values: { name: d.name } }))) return;
		opError = null;
		try {
			await deleteDashboard(d.id);
			dashboards = dashboards.filter((x) => x.id !== d.id);
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
	<p class="text-sm text-zinc-600 dark:text-zinc-400">{$_('dashboards.intro')}</p>

	{#if !$user}
		<p class="text-sm text-zinc-600 dark:text-zinc-400">{$_('dashboards.sign_in')}</p>
	{:else if loading && dashboards.length === 0}
		<p class="text-sm text-zinc-600 dark:text-zinc-400">{$_('dashboards.loading')}</p>
	{:else if loadError}
		<p class="text-sm text-red-600 dark:text-red-400">{loadError}</p>
	{:else}
		{#if opError}
			<p class="text-sm text-red-600 dark:text-red-400">{opError}</p>
		{/if}
		{#if dashboards.length === 0}
			<!-- No dashboards yet: the add row below creates one. -->
			<div class="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
				<p class="text-sm text-zinc-500 dark:text-zinc-400">{$_('dashboards.empty')}</p>
			</div>
		{:else}
			<div class="space-y-2">
				{#each dashboards as d (d.id)}
					<div
						class="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-4"
					>
						<div class="flex min-w-0 flex-1 flex-wrap items-baseline gap-3">
							{#if renamingId === d.id}
								<input
									class="w-full min-w-48 rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5 text-sm"
									bind:value={draftName}
									use:autofocus
									onkeydown={(e) => {
										if (e.key === 'Enter') void saveRename(d);
										if (e.key === 'Escape') cancelRename();
									}}
									onblur={() => void saveRename(d)}
								/>
							{:else}
								<h2 class="text-base font-semibold">{d.name}</h2>
							{/if}
							<span class="text-sm text-zinc-500 dark:text-zinc-400"
								>{$_('dashboards.widget_count', { values: { n: d.widget_count } })}</span
								>
						</div>
						<div class="flex shrink-0 items-center gap-3">
							<a
								href={resolve('/dashboards/[id]', { id: d.id })}
								class="text-sm font-medium text-zinc-600 dark:text-zinc-400 underline hover:text-zinc-900 dark:hover:text-zinc-100"
							>
								{$_('dashboards.open')}
							</a>
							<button
								class="text-sm font-medium text-zinc-600 dark:text-zinc-400 underline hover:text-zinc-900 dark:hover:text-zinc-100"
								type="button"
								aria-label={$_('dashboards.rename')}
								onclick={() => startRename(d)}
							>
								{$_('dashboards.rename')}
							</button>
							<button
								class="text-sm font-medium text-red-600 dark:text-red-400 underline hover:text-red-700 dark:hover:text-red-400"
								type="button"
								aria-label={$_('dashboards.delete')}
								onclick={() => void removeDashboard(d)}
							>
								{$_('dashboards.delete')}
							</button>
						</div>
					</div>
				{/each}
			</div>
		{/if}

		<!-- Add row: a new dashboard (name; the widgets are added on
		     its editor page). -->
		<div
			class="flex flex-wrap items-center gap-3 rounded-lg border border-dashed border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 p-4"
		>
			<input
				class="w-64 rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5 text-sm"
				bind:value={newName}
				placeholder={$_('dashboards.new_dashboard_placeholder')}
				onkeydown={(e) => {
					if (e.key === 'Enter') void addDashboard();
				}}
			/>
			<button
				class="rounded-md border border-zinc-300 dark:border-zinc-700 px-4 py-2 text-sm font-medium text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-800"
				type="button"
				onclick={() => void addDashboard()}
			>
				{$_('dashboards.create')}
			</button>
		</div>
	{/if}
</section>

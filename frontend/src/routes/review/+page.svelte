<script lang="ts">
	import { page } from '$app/state';
	import { onMount } from 'svelte';
	import { _ } from 'svelte-i18n';
	import { login, user } from '#lib/auth/client';
	import { ApiError } from '#lib/api/client';
	import { listAccounts, type Account } from '#lib/api/accounts';
	import { listCategories, type Category } from '#lib/api/categories';
	import { acceptAllReview, listReviewQueue, type ReviewQueueRow } from '#lib/api/review';
	import {
		acceptOffsetSuggestion,
		clearOffsetGroup,
		dismissOffsetSuggestion,
		listOffsetGroups,
		listOffsetSuggestions,
		scanOffsets,
		type OffsetGroup,
		type OffsetSuggestion
	} from '#lib/api/offsets';
	import ReviewRow from '#lib/components/ReviewRow.svelte';
	import { notifyDataChanged } from '#lib/explore/cache';

	// Review queue (step 5.5): the rows the categorise worker flagged
	// with a sub-threshold suggestion. A row is resolved by picking one
	// of the caller's own categories (per row, or all at once); the
	// reprompt button re-runs the LLM on the single row and applies the
	// answer when it clears the threshold (the per-row state lives in
	// the ReviewRow component — its instances are keyed by the row id).
	// A resolved row leaves the queue, so every successful action ends
	// in a queue refresh.

	/** One page of a list endpoint (the backend default). */
	const PAGE_SIZE = 100;

	let rows = $state<ReviewQueueRow[]>([]);
	let rowsHasMore = $state(false);
	let rowsLoadingMore = $state(false);
	let categories = $state<Category[]>([]);
	let accounts = $state<Account[]>([]);
	let loadError = $state<string | null>(null);
	let loading = $state(true);

	// Apply-all.
	let allCategoryId = $state('');
	let applyingAll = $state(false);
	let appliedAll = $state<number | null>(null);
	let allError = $state<string | null>(null);

	// Offsets (migration 0012): transactions that net each other out
	// (a credit-card payment from chequing must not count as spend in
	// one account and income in the other). The scan runs the matcher
	// and stores its pairs as proposals; accepting a proposal groups
	// the two legs so they leave the lists and the aggregates.
	let suggestions = $state<OffsetSuggestion[]>([]);
	let suggestionsHasMore = $state(false);
	let suggestionsLoadingMore = $state(false);
	let groups = $state<OffsetGroup[]>([]);
	let groupsHasMore = $state(false);
	let groupsLoadingMore = $state(false);
	let scanning = $state(false);
	let scanResult = $state<number | null>(null);
	let scanError = $state<string | null>(null);
	let busySuggestion = $state<string | null>(null);
	let busyGroup = $state<string | null>(null);
	let offsetError = $state<string | null>(null);

	/** Only active categories can be assigned (the manual-entry
	 * picker's rule). */
	const activeCategories = $derived(categories.filter((c) => c.active));

	/** The account name for a review row's account_id (the raw id as
	 * a fallback, e.g. while the account list is still loading). */
	function accountName(id: string): string {
		return accounts.find((a) => a.id === id)?.name ?? id;
	}

	/** Re-fetches the first page of the queue (the pickers' data is
	 * loaded once in onMount, like the imports page). */
	async function refreshQueue(): Promise<void> {
		try {
			const page = await listReviewQueue(PAGE_SIZE, 0);
			rows = page;
			rowsHasMore = page.length >= PAGE_SIZE;
			loadError = null;
		} catch (e) {
			loadError = envelopeMessage(e);
		} finally {
			loading = false;
		}
	}

	/** Appends the next page of the queue (offset = rendered count).
	 * The fetched page is deduped by id against what is already
	 * rendered — while an endpoint ignores offset that makes the
	 * click a no-op instead of a duplicated list. */
	async function loadMoreQueue(): Promise<void> {
		if (rowsLoadingMore) return;
		rowsLoadingMore = true;
		loadError = null;
		try {
			const page = await listReviewQueue(PAGE_SIZE, rows.length);
			const known = new Set(rows.map((r) => r.id));
			const fresh = page.filter((r) => !known.has(r.id));
			if (fresh.length > 0) rows = [...rows, ...fresh];
			rowsHasMore = page.length >= PAGE_SIZE && fresh.length > 0;
		} catch (e) {
			loadError = envelopeMessage(e);
		} finally {
			rowsLoadingMore = false;
		}
	}

	/** Resolves every remaining row with one category. */
	async function applyAll(): Promise<void> {
		if (allCategoryId === '' || applyingAll || rows.length === 0) return;
		applyingAll = true;
		allError = null;
		appliedAll = null;
		try {
			const res = await acceptAllReview(allCategoryId);
			appliedAll = res.applied;
			notifyDataChanged();
			await refreshQueue();
		} catch (e) {
			allError = envelopeMessage(e);
		} finally {
			applyingAll = false;
		}
	}

	/** The backend answers 4xx with the 3.1 envelope {error: {code,
	 * message}}; ApiError folds the raw body into its message ("API
	 * 400: {...}") — carve the JSON out for the friendly line, fall
	 * back to the raw message when the body is not an envelope. */
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

	/** Re-fetches the first pages of the pending suggestions and the
	 * active groups. */
	async function refreshOffsets(): Promise<void> {
		try {
			const [sugs, grps] = await Promise.all([
				listOffsetSuggestions(PAGE_SIZE, 0),
				listOffsetGroups(PAGE_SIZE, 0)
			]);
			suggestions = sugs;
			groups = grps;
			suggestionsHasMore = sugs.length >= PAGE_SIZE;
			groupsHasMore = grps.length >= PAGE_SIZE;
			offsetError = null;
		} catch (e) {
			offsetError = envelopeMessage(e);
		}
	}

	/** Appends the next page of the pending suggestions (offset =
	 * rendered count, deduped by id — see loadMoreQueue). */
	async function loadMoreSuggestions(): Promise<void> {
		if (suggestionsLoadingMore) return;
		suggestionsLoadingMore = true;
		offsetError = null;
		try {
			const page = await listOffsetSuggestions(PAGE_SIZE, suggestions.length);
			const known = new Set(suggestions.map((s) => s.id));
			const fresh = page.filter((s) => !known.has(s.id));
			if (fresh.length > 0) suggestions = [...suggestions, ...fresh];
			suggestionsHasMore = page.length >= PAGE_SIZE && fresh.length > 0;
		} catch (e) {
			offsetError = envelopeMessage(e);
		} finally {
			suggestionsLoadingMore = false;
		}
	}

	/** Appends the next page of the active groups (offset = rendered
	 * count, deduped by id — see loadMoreQueue). */
	async function loadMoreGroups(): Promise<void> {
		if (groupsLoadingMore) return;
		groupsLoadingMore = true;
		offsetError = null;
		try {
			const page = await listOffsetGroups(PAGE_SIZE, groups.length);
			const known = new Set(groups.map((g) => g.id));
			const fresh = page.filter((g) => !known.has(g.id));
			if (fresh.length > 0) groups = [...groups, ...fresh];
			groupsHasMore = page.length >= PAGE_SIZE && fresh.length > 0;
		} catch (e) {
			offsetError = envelopeMessage(e);
		} finally {
			groupsLoadingMore = false;
		}
	}

	/** Runs the matcher over the caller's ungrouped rows. The scan is
	 * idempotent (pairs that already have a suggestion in any status
	 * are skipped), so the result is the count of NEW proposals. */
	async function runScan(): Promise<void> {
		if (scanning) return;
		scanning = true;
		scanError = null;
		scanResult = null;
		try {
			const res = await scanOffsets();
			scanResult = res.suggested;
			await refreshOffsets();
		} catch (e) {
			scanError = envelopeMessage(e);
		} finally {
			scanning = false;
		}
	}

	/** Accepts one proposal: the two legs share a fresh group id and
	 * leave the transaction lists and the aggregates. */
	async function acceptSuggestion(id: string): Promise<void> {
		if (busySuggestion !== null) return;
		busySuggestion = id;
		offsetError = null;
		try {
			await acceptOffsetSuggestion(id);
			notifyDataChanged();
			await refreshOffsets();
		} catch (e) {
			offsetError = envelopeMessage(e);
		} finally {
			busySuggestion = null;
		}
	}

	/** Dismisses one proposal — it is not re-proposed until a group
	 * touching it is cleared. */
	async function dismissSuggestion(id: string): Promise<void> {
		if (busySuggestion !== null) return;
		busySuggestion = id;
		offsetError = null;
		try {
			await dismissOffsetSuggestion(id);
			notifyDataChanged();
			await refreshOffsets();
		} catch (e) {
			offsetError = envelopeMessage(e);
		} finally {
			busySuggestion = null;
		}
	}

	/** Dissolves a group: the members become visible again and the
	 * suggestions touching them are deleted (the pairs can be
	 * re-proposed on the next scan). */
	async function ungroupGroup(id: string): Promise<void> {
		if (busyGroup !== null) return;
		busyGroup = id;
		offsetError = null;
		try {
			await clearOffsetGroup(id);
			notifyDataChanged();
			await refreshOffsets();
		} catch (e) {
			offsetError = envelopeMessage(e);
		} finally {
			busyGroup = null;
		}
	}

	// The queue, the pickers' data and the offset lists. (The layout
	// restores the session reactively; a signed-out load simply 401s
	// and the cards stay hidden behind the sign-in prompt.)
	onMount(() => {
		void Promise.all([
			listReviewQueue(PAGE_SIZE, 0),
			listCategories(),
			listAccounts(),
			listOffsetSuggestions(PAGE_SIZE, 0),
			listOffsetGroups(PAGE_SIZE, 0)
		])
			.then(([txs, cats, accs, sugs, grps]) => {
				rows = txs;
				categories = cats;
				accounts = accs;
				suggestions = sugs;
				groups = grps;
				rowsHasMore = txs.length >= PAGE_SIZE;
				suggestionsHasMore = sugs.length >= PAGE_SIZE;
				groupsHasMore = grps.length >= PAGE_SIZE;
				loadError = null;
			})
			.catch((e) => {
				loadError = envelopeMessage(e);
			})
			.finally(() => {
				loading = false;
			});
	});
</script>

<section class="space-y-3">
	<h1 class="text-2xl font-semibold">{$_('review.title')}</h1>
	<p class="max-w-2xl text-sm text-zinc-600 dark:text-zinc-400">{$_('review.intro')}</p>

	{#if !$user}
		<p class="text-sm text-zinc-600 dark:text-zinc-400">
			{$_('review.sign_in_prompt')}
			<button class="underline" onclick={() => login(page.url.pathname + page.url.search)}>
				{$_('auth.sign_in')}
			</button>
		</p>
	{:else}
		{#if loading}
			<p class="text-sm text-zinc-600 dark:text-zinc-400">{$_('review.loading')}</p>
		{:else}
			{#if loadError}
				<p class="text-sm text-red-600 dark:text-red-400">{loadError}</p>
			{/if}
			{#if rows.length === 0 && !loadError}
				<p class="text-sm text-zinc-500 dark:text-zinc-400">{$_('review.empty')}</p>
			{/if}

			<!-- Apply-all: one category for every remaining row (the
			     card stays up after a successful apply-all so the result
			     is still visible over the now-empty queue). -->
			{#if rows.length > 0 || appliedAll !== null}
				<div class="space-y-2 rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
					<div class="flex flex-wrap items-end gap-4">
						<label class="block text-sm">
							<span class="mb-1 block font-medium">{$_('review.all_category_label')}</span>
							<select
								class="w-64 rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
								value={allCategoryId}
								onchange={(e) => {
									allCategoryId = e.currentTarget.value;
									allError = null;
								}}
							>
								<option value="">{$_('review.category_placeholder')}</option>
								{#each activeCategories as c (c.id)}
									<option value={c.id}>{c.name}</option>
								{/each}
							</select>
						</label>
						<button
							class="rounded-md bg-zinc-900 px-4 py-2 text-sm font-medium text-white hover:bg-zinc-700 disabled:opacity-50"
							disabled={allCategoryId === '' || applyingAll || rows.length === 0}
							type="button"
							onclick={() => void applyAll()}
						>
							{applyingAll ? $_('review.applying_all') : $_('review.apply_all')}
						</button>
						{#if appliedAll !== null}
							<p class="pb-1.5 text-sm text-green-600 dark:text-green-400">
								{$_('review.applied_all', { values: { n: appliedAll } })}
							</p>
						{/if}
					</div>
					{#if allError}
						<p class="text-sm text-red-600 dark:text-red-400">{allError}</p>
					{/if}
				</div>
			{/if}

			{#if rows.length > 0}
				<div class="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
					<div class="overflow-x-auto">
						<table class="w-full text-sm">
							<thead>
								<tr class="border-b border-zinc-200 dark:border-zinc-800 text-left text-xs text-zinc-500 dark:text-zinc-400 uppercase">
									<th class="py-1.5 pr-4">{$_('review.col_date')}</th>
									<th class="py-1.5 pr-4">{$_('review.col_description')}</th>
									<th class="py-1.5 pr-4 text-right">{$_('review.col_amount')}</th>
									<th class="py-1.5 pr-4">{$_('review.col_account')}</th>
									<th class="py-1.5 pr-4">{$_('review.col_category')}</th>
									<th class="py-1.5">{$_('review.col_actions')}</th>
								</tr>
							</thead>
							<tbody>
								{#each rows as r (r.id)}
									<ReviewRow row={r} {accountName} {categories} onResolved={refreshQueue} />
								{/each}
							</tbody>
						</table>
					</div>
					{#if rowsHasMore}
						<div class="mt-3 flex justify-end">
							<button
								class="rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5 text-sm hover:bg-zinc-100 dark:hover:bg-zinc-800 disabled:opacity-50"
								disabled={rowsLoadingMore}
								type="button"
								onclick={() => void loadMoreQueue()}
							>
								{rowsLoadingMore ? $_('common.loading_more') : $_('common.load_more')}
							</button>
						</div>
					{/if}
				</div>
			{/if}

			<!-- Offsets (migration 0012): internal movements that net
			     out. The scan proposes net-to-zero pairs; accepting a
			     proposal groups the legs so they leave the lists and
			     the aggregates. -->
			<div class="space-y-4 rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
				<div>
					<h2 class="text-base font-semibold">{$_('offsets.title')}</h2>
					<p class="mt-1 max-w-2xl text-sm text-zinc-600 dark:text-zinc-400">{$_('offsets.intro')}</p>
				</div>

				<div class="flex flex-wrap items-center gap-4">
					<button
						class="rounded-md bg-zinc-900 px-4 py-2 text-sm font-medium text-white hover:bg-zinc-700 disabled:opacity-50"
						disabled={scanning}
						type="button"
						onclick={() => void runScan()}
					>
						{scanning ? $_('offsets.scanning') : $_('offsets.scan')}
					</button>
					{#if scanResult !== null}
						<p class="text-sm text-green-600 dark:text-green-400">
							{$_('offsets.scanned', { values: { n: scanResult } })}
						</p>
					{/if}
					{#if scanError}
						<p class="text-sm text-red-600 dark:text-red-400">{scanError}</p>
					{/if}
				</div>

				<div>
					<h3 class="text-sm font-medium">{$_('offsets.suggestions_label')}</h3>
					{#if suggestions.length === 0}
						<p class="mt-2 text-sm text-zinc-500 dark:text-zinc-400">{$_('offsets.no_suggestions')}</p>
					{:else}
						<ul class="mt-2 space-y-3">
							{#each suggestions as s (s.id)}
								<li class="rounded-md border border-zinc-200 dark:border-zinc-800 p-3">
									<div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
										{#each [s.a, s.b] as leg (leg.id)}
											<div class="text-sm">
												<p class="font-semibold">{leg.amount}</p>
												<p class="text-zinc-600 dark:text-zinc-400">
													{leg.date_posted} · {leg.description} · {accountName(leg.account_id)}
												</p>
											</div>
										{/each}
									</div>
									<div class="mt-3 flex gap-2">
										<button
											class="rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5 text-sm hover:bg-zinc-100 dark:hover:bg-zinc-800 disabled:opacity-50"
											disabled={busySuggestion !== null}
											type="button"
											onclick={() => void dismissSuggestion(s.id)}
										>
											{busySuggestion === s.id ? $_('offsets.dismissing') : $_('offsets.dismiss')}
										</button>
										<button
											class="rounded-md bg-zinc-900 px-3 py-1.5 text-sm font-medium text-white hover:bg-zinc-700 disabled:opacity-50"
											disabled={busySuggestion !== null}
											type="button"
											onclick={() => void acceptSuggestion(s.id)}
										>
											{busySuggestion === s.id ? $_('offsets.accepting') : $_('offsets.accept')}
										</button>
									</div>
								</li>
							{/each}
						</ul>
						{#if suggestionsHasMore}
							<div class="mt-2 flex justify-end">
								<button
									class="rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5 text-sm hover:bg-zinc-100 dark:hover:bg-zinc-800 disabled:opacity-50"
									disabled={suggestionsLoadingMore}
									type="button"
									onclick={() => void loadMoreSuggestions()}
								>
									{suggestionsLoadingMore ? $_('common.loading_more') : $_('common.load_more')}
								</button>
							</div>
						{/if}
					{/if}
				</div>

				<div>
					<h3 class="text-sm font-medium">{$_('offsets.groups_label')}</h3>
					{#if groups.length === 0}
						<p class="mt-2 text-sm text-zinc-500 dark:text-zinc-400">{$_('offsets.no_groups')}</p>
					{:else}
						<ul class="mt-2 space-y-3">
							{#each groups as g (g.id)}
								<li class="rounded-md border border-zinc-200 dark:border-zinc-800 p-3">
									<ul class="space-y-1 text-sm">
										{#each g.members as m (m.id)}
											<li>
												<span class="float-right font-medium">{m.amount}</span>
												<span class="text-zinc-600 dark:text-zinc-400">
													{m.date_posted} · {m.description} · {accountName(m.account_id)}
												</span>
											</li>
										{/each}
									</ul>
									<button
										class="mt-3 rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5 text-sm hover:bg-zinc-100 dark:hover:bg-zinc-800 disabled:opacity-50"
										disabled={busyGroup !== null}
										type="button"
										onclick={() => void ungroupGroup(g.id)}
									>
										{busyGroup === g.id ? $_('offsets.clearing') : $_('offsets.clear')}
									</button>
								</li>
							{/each}
						</ul>
						{#if groupsHasMore}
							<div class="mt-2 flex justify-end">
								<button
									class="rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5 text-sm hover:bg-zinc-100 dark:hover:bg-zinc-800 disabled:opacity-50"
									disabled={groupsLoadingMore}
									type="button"
									onclick={() => void loadMoreGroups()}
								>
									{groupsLoadingMore ? $_('common.loading_more') : $_('common.load_more')}
								</button>
							</div>
						{/if}
					{/if}
				</div>

				{#if offsetError}
					<p class="text-sm text-red-600 dark:text-red-400">{offsetError}</p>
				{/if}
			</div>
		{/if}
	{/if}
</section>

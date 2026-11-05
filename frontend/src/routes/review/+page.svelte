<script lang="ts">
	import { resolve } from '$app/paths';
	import { onMount } from 'svelte';
	import { _ } from 'svelte-i18n';
	import { user } from '#lib/auth/client';
	import { ApiError } from '#lib/api/client';
	import { listAccounts, type Account } from '#lib/api/accounts';
	import { listCategories, type Category } from '#lib/api/categories';
	import { acceptAllReview, listReviewQueue, type ReviewQueueRow } from '#lib/api/review';
	import ReviewRow from '#lib/components/ReviewRow.svelte';

	// Review queue (step 5.5): the rows the categorise worker flagged
	// with a sub-threshold suggestion. A row is resolved by picking one
	// of the caller's own categories (per row, or all at once); the
	// reprompt button re-runs the LLM on the single row and applies the
	// answer when it clears the threshold (the per-row state lives in
	// the ReviewRow component — its instances are keyed by the row id).
	// A resolved row leaves the queue, so every successful action ends
	// in a queue refresh.

	let rows = $state<ReviewQueueRow[]>([]);
	let categories = $state<Category[]>([]);
	let accounts = $state<Account[]>([]);
	let loadError = $state<string | null>(null);
	let loading = $state(true);

	// Apply-all.
	let allCategoryId = $state('');
	let applyingAll = $state(false);
	let appliedAll = $state<number | null>(null);
	let allError = $state<string | null>(null);

	/** Only active categories can be assigned (the manual-entry
	 * picker's rule). */
	const activeCategories = $derived(categories.filter((c) => c.active));

	/** The account name for a review row's account_id (the raw id as
	 * a fallback, e.g. while the account list is still loading). */
	function accountName(id: string): string {
		return accounts.find((a) => a.id === id)?.name ?? id;
	}

	/** Re-fetches the queue (the pickers' data is loaded once in
	 * onMount, like the imports page). */
	async function refreshQueue(): Promise<void> {
		try {
			rows = await listReviewQueue();
			loadError = null;
		} catch (e) {
			loadError = envelopeMessage(e);
		} finally {
			loading = false;
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

	// The queue and the pickers' data. (The layout restores the
	// session reactively; a signed-out load simply 401s and the queue
	// card stays hidden behind the sign-in prompt.)
	onMount(() => {
		void Promise.all([listReviewQueue(), listCategories(), listAccounts()])
			.then(([txs, cats, accs]) => {
				rows = txs;
				categories = cats;
				accounts = accs;
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
	<p class="max-w-2xl text-sm text-zinc-600">{$_('review.intro')}</p>

	{#if !$user}
		<p class="text-sm text-zinc-600">
			{$_('review.sign_in_prompt')}
			<a class="underline" href={resolve('/')}>{$_('auth.sign_in')}</a>
		</p>
	{:else}
		{#if loading}
			<p class="text-sm text-zinc-600">{$_('review.loading')}</p>
		{:else}
			{#if loadError}
				<p class="text-sm text-red-600">{loadError}</p>
			{/if}
			{#if rows.length === 0 && !loadError}
				<p class="text-sm text-zinc-500">{$_('review.empty')}</p>
			{/if}

			<!-- Apply-all: one category for every remaining row (the
			     card stays up after a successful apply-all so the result
			     is still visible over the now-empty queue). -->
			{#if rows.length > 0 || appliedAll !== null}
				<div class="space-y-2 rounded-lg border border-zinc-200 bg-white p-5">
					<div class="flex flex-wrap items-end gap-4">
						<label class="block text-sm">
							<span class="mb-1 block font-medium">{$_('review.all_category_label')}</span>
							<select
								class="w-64 rounded-md border border-zinc-300 px-3 py-1.5"
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
							<p class="pb-1.5 text-sm text-green-600">
								{$_('review.applied_all', { values: { n: appliedAll } })}
							</p>
						{/if}
					</div>
					{#if allError}
						<p class="text-sm text-red-600">{allError}</p>
					{/if}
				</div>
			{/if}

			{#if rows.length > 0}
				<div class="rounded-lg border border-zinc-200 bg-white p-5">
					<div class="overflow-x-auto">
						<table class="w-full text-sm">
							<thead>
								<tr class="border-b border-zinc-200 text-left text-xs text-zinc-500 uppercase">
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
				</div>
			{/if}
		{/if}
	{/if}
</section>

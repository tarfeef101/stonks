<script lang="ts">
	import { _ } from 'svelte-i18n';
	import { ApiError } from '#lib/api/client';
	import type { Category } from '#lib/api/categories';
	import {
		acceptReview,
		repromptReview,
		type RepromptResult,
		type ReviewQueueRow
	} from '#lib/api/review';

	// One review-queue row (step 5.5): the transaction's details, a
	// category picker, and the per-row actions — resolve with the
	// picked category, or re-run the LLM on the single row. The
	// component instance is keyed by the row id (the parent's {#each}),
	// so its state survives re-renders; a resolved row leaves the queue
	// and the instance unmounts.

	let {
		row,
		accountName,
		categories,
		onResolved
	}: {
		row: ReviewQueueRow;
		accountName: (id: string) => string;
		categories: Category[];
		/** The parent re-fetches the queue (the row is resolved and
		 * gone). */
		onResolved: () => Promise<void>;
	} = $props();

	let categoryId = $state(''); // '' = nothing picked yet
	let accepting = $state(false);
	let reprompting = $state(false);
	let reprompt = $state<RepromptResult | null>(null);
	let error = $state<string | null>(null);

	// Only active categories can be assigned (the manual-entry
	// picker's rule).
	const activeCategories = $derived(categories.filter((c) => c.active));

	/** Resolves the row with the picked category. */
	async function accept(): Promise<void> {
		if (categoryId === '' || accepting) return;
		accepting = true;
		error = null;
		reprompt = null;
		try {
			await acceptReview(row.id, categoryId);
			await onResolved();
		} catch (e) {
			accepting = false;
			error = envelopeMessage(e);
		}
	}

	/** Re-runs the LLM on the row. The model's answer is always shown;
	 * an applied answer resolves the row (the parent refreshes). */
	async function repromptAgain(): Promise<void> {
		if (accepting || reprompting) return;
		reprompting = true;
		error = null;
		try {
			const res = await repromptReview(row.id);
			reprompt = res;
			if (res.applied) await onResolved();
		} catch (e) {
			reprompting = false;
			error = envelopeMessage(e);
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
</script>

<tr class="border-b border-zinc-100 align-top">
	<td class="py-2 pr-4 whitespace-nowrap">{row.date_posted}</td>
	<td class="py-2 pr-4">{row.description}</td>
	<td class="py-2 pr-4 text-right whitespace-nowrap">{row.amount}</td>
	<td class="py-2 pr-4 whitespace-nowrap">{accountName(row.account_id)}</td>
	<td class="py-2 pr-4">
		<select
			class="w-44 rounded-md border border-zinc-300 px-2 py-1"
			value={categoryId}
			onchange={(e) => {
				categoryId = e.currentTarget.value;
				error = null;
			}}
		>
			<option value="">{$_('review.category_placeholder')}</option>
			{#each activeCategories as c (c.id)}
				<option value={c.id}>{c.name}</option>
			{/each}
		</select>
	</td>
	<td class="py-2">
		<div class="flex gap-2">
			<button
				class="rounded-md bg-zinc-900 px-3 py-1 text-sm font-medium text-white hover:bg-zinc-700 disabled:opacity-50"
				disabled={categoryId === '' || accepting}
				type="button"
				onclick={() => void accept()}
			>
				{accepting ? $_('review.applying') : $_('review.apply')}
			</button>
			<button
				class="rounded-md border border-zinc-300 px-3 py-1 text-sm text-zinc-700 hover:bg-zinc-100 disabled:opacity-50"
				disabled={accepting || reprompting}
				type="button"
				onclick={() => void repromptAgain()}
			>
				{reprompting ? $_('review.reprompting') : $_('review.reprompt')}
			</button>
		</div>
		{#if error}
			<p class="mt-1 text-xs text-red-600">{error}</p>
		{/if}
		{#if reprompt}
			<p class="mt-1 text-xs text-zinc-600">
				{$_('review.suggested')}:
				<span class="font-medium text-zinc-900">{reprompt.category}</span>
				({(reprompt.confidence * 100).toFixed(0)}%)
			</p>
			{#if reprompt.rationale !== ''}
				<p class="text-xs text-zinc-500">{reprompt.rationale}</p>
			{/if}
			<p class="text-xs {reprompt.applied ? 'text-green-600' : 'text-amber-600'}">
				{reprompt.applied ? $_('review.applied') : $_('review.not_applied')}
			</p>
		{/if}
	</td>
</tr>

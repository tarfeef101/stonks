<script lang="ts">
	import { onMount } from 'svelte';
	import { _ } from 'svelte-i18n';
	import { ApiError } from '#lib/api/client';
	import { listAccounts, type Account } from '#lib/api/accounts';
	import { listCategories, type Category } from '#lib/api/categories';
	import {
		createTransaction,
		type CreateTransactionInput,
		type Transaction
	} from '#lib/api/transactions';

	// Manual entry (step 4.7): record a transaction that is not in any
	// statement file. The form posts the same wire shape an imported row
	// gets (POST /v1/transactions): category_id goes out only when a
	// category is picked (the pipeline then skips the later
	// categorization phase for the row) and absent optionals are omitted
	// from the body entirely — never null or ''. review_pending is not
	// sent (the pipeline default applies).

	let accounts = $state<Account[]>([]);
	let categories = $state<Category[]>([]);
	let loadError = $state<string | null>(null);

	let accountId = $state('');
	let datePosted = $state('');
	let dateExecuted = $state('');
	let amount = $state('');
	let categoryId = $state(''); // '' = no category
	let description = $state('');
	let notes = $state('');

	let submitting = $state(false);
	let missing = $state(false);
	let error = $state<string | null>(null);
	// The created row's values for the success line, captured before the
	// form resets.
	let created = $state<{ id: string; amount: string; datePosted: string } | null>(null);

	/** Only the active categories are offered; '' is the "no category"
	 * option. */
	const activeCategories = $derived(categories.filter((c) => c.active));

	// The account + category pickers' data. (The parent renders this
	// component only while signed in, so a 401 here would be unexpected;
	// the error line still catches anything else.)
	onMount(() => {
		void Promise.all([listAccounts(), listCategories()])
			.then(([accs, cats]) => {
				accounts = accs;
				categories = cats;
			})
			.catch((e) => {
				loadError = envelopeMessage(e);
			});
	});

	/** Builds the POST body: the required fields always, the optionals
	 * only when non-empty after trimming. */
	function buildInput(): CreateTransactionInput {
		const input: CreateTransactionInput = {
			account_id: accountId,
			amount: amount.trim(),
			date_posted: datePosted
		};
		if (dateExecuted !== '') input.date_executed = dateExecuted;
		if (categoryId !== '') input.category_id = categoryId;
		const desc = description.trim();
		if (desc !== '') input.description = desc;
		const n = notes.trim();
		if (n !== '') input.notes = n;
		return input;
	}

	async function submit(): Promise<void> {
		if (submitting) return;
		if (accountId === '' || datePosted === '' || amount.trim() === '') {
			missing = true;
			error = null;
			return;
		}
		missing = false;
		submitting = true;
		error = null;
		created = null;
		const input = buildInput();
		try {
			const tx: Transaction = await createTransaction(input);
			created = { id: tx.id, amount: input.amount, datePosted: input.date_posted };
			// Reset the entry fields; the account + category keep their
			// selection for back-to-back entries.
			datePosted = '';
			dateExecuted = '';
			amount = '';
			categoryId = '';
			description = '';
			notes = '';
		} catch (e) {
			error = envelopeMessage(e);
		} finally {
			submitting = false;
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

<div class="space-y-3">
	<h2 class="text-lg font-medium">{$_('manual_entry.title')}</h2>
	<p class="max-w-2xl text-sm text-zinc-600">{$_('manual_entry.intro')}</p>

	<form
		class="space-y-4 rounded-lg border border-zinc-200 bg-white p-5"
		onsubmit={(e) => {
			e.preventDefault();
			void submit();
		}}
	>
		<div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
			<label class="block text-sm">
				<span class="mb-1 block font-medium">{$_('manual_entry.account_label')}</span>
				<select bind:value={accountId} class="w-full rounded-md border border-zinc-300 px-3 py-1.5">
					<option value="">{$_('manual_entry.account_placeholder')}</option>
					{#each accounts as a (a.id)}
						<option value={a.id}>{a.name} ({a.currency})</option>
					{/each}
				</select>
				{#if accounts.length === 0 && !loadError}
					<span class="mt-1 block text-xs text-zinc-500">{$_('manual_entry.no_accounts')}</span>
				{/if}
			</label>
			<label class="block text-sm">
				<span class="mb-1 block font-medium">{$_('manual_entry.category_label')}</span>
				<select
					bind:value={categoryId}
					class="w-full rounded-md border border-zinc-300 px-3 py-1.5"
				>
					<option value="">{$_('manual_entry.category_none')}</option>
					{#each activeCategories as c (c.id)}
						<option value={c.id}>{c.name}</option>
					{/each}
				</select>
			</label>
			<label class="block text-sm">
				<span class="mb-1 block font-medium">{$_('manual_entry.date_posted_label')}</span>
				<input
					bind:value={datePosted}
					class="w-full rounded-md border border-zinc-300 px-3 py-1.5"
					type="date"
				/>
			</label>
			<label class="block text-sm">
				<span class="mb-1 block font-medium">{$_('manual_entry.date_executed_label')}</span>
				<input
					bind:value={dateExecuted}
					class="w-full rounded-md border border-zinc-300 px-3 py-1.5"
					type="date"
				/>
			</label>
			<label class="block text-sm">
				<span class="mb-1 block font-medium">{$_('manual_entry.amount_label')}</span>
				<input
					bind:value={amount}
					class="w-full rounded-md border border-zinc-300 px-3 py-1.5"
					inputmode="decimal"
					placeholder={$_('manual_entry.amount_placeholder')}
					type="text"
				/>
				<span class="mt-1 block text-xs text-zinc-500">{$_('manual_entry.amount_note')}</span>
			</label>
			<label class="block text-sm">
				<span class="mb-1 block font-medium">{$_('manual_entry.description_label')}</span>
				<input
					bind:value={description}
					class="w-full rounded-md border border-zinc-300 px-3 py-1.5"
					placeholder={$_('manual_entry.description_placeholder')}
					type="text"
				/>
			</label>
		</div>
		<label class="block text-sm">
			<span class="mb-1 block font-medium">{$_('manual_entry.notes_label')}</span>
			<textarea
				bind:value={notes}
				class="w-full rounded-md border border-zinc-300 px-3 py-1.5"
				placeholder={$_('manual_entry.notes_placeholder')}
				rows="2"></textarea>
		</label>
		{#if loadError}
			<p class="text-sm text-red-600">{loadError}</p>
		{/if}
		{#if missing}
			<p class="text-sm text-red-600">{$_('manual_entry.missing')}</p>
		{/if}
		{#if error}
			<p class="text-sm text-red-600">{error}</p>
		{/if}
		{#if created}
			<p class="text-sm text-green-600">
				{$_('manual_entry.success', {
					values: { amount: created.amount, date: created.datePosted, id: created.id }
				})}
			</p>
		{/if}
		<button
			class="rounded-md bg-zinc-900 px-4 py-2 text-sm font-medium text-white hover:bg-zinc-700 disabled:opacity-50"
			disabled={submitting}
			type="submit"
		>
			{submitting ? $_('manual_entry.submitting') : $_('manual_entry.submit')}
		</button>
	</form>
</div>

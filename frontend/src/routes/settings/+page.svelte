<script lang="ts">
	import { page } from '$app/state';
	import { onMount } from 'svelte';
	import { _ } from 'svelte-i18n';
	import { login, user } from '#lib/auth/client';
	import { ApiError } from '#lib/api/client';
	import {
		deleteLLMCredential,
		listLLMCredentials,
		putLLMCredential,
		testLLMConnection,
		type LLMCredential,
		type LLMProvider,
		type LLMTestResult
	} from '#lib/api/llmcredentials';
	import {
		createCategory,
		deleteCategory,
		listCategories,
		patchCategory,
		type Category
	} from '#lib/api/categories';
	import {
		ACCOUNT_TYPES,
		createAccount,
		deleteAccount,
		listAccounts,
		patchAccount,
		type Account,
		type AccountType
	} from '#lib/api/accounts';
	import { PLAN_TYPES } from '#lib/api/savings';
	import { CURRENCIES, loadSettings, settings, updateSettings } from '#lib/settings';

	// LLM credentials (step 5.6): at most one credential per provider
	// (self-hosted, OpenAI, custom endpoint) for the endpoint the
	// auto-categoriser talks to. The API key is sealed on the server
	// and never read back — when a credential is stored, the key field
	// shows the row's mask (display-only: a save or a test-connection
	// over the mask sends no key, so a stored key is kept by
	// re-entering it). Test-connection fires one trivial call at the
	// values on the screen, not the stored row. There is no model
	// field: the process's LLM model answers for every provider.

	// The in-network address the API container uses to reach the dev
	// llama server (the dev self-hosted default).
	const DEFAULT_ENDPOINT = 'http://llama:8080/v1';

	let credentials = $state<LLMCredential[]>([]);
	let loading = $state(true);
	let loadError = $state<string | null>(null);

	// The form (the selected provider's row, or the defaults).
	let provider = $state<LLMProvider>('self_hosted');
	let endpoint = $state(DEFAULT_ENDPOINT);
	let apiKey = $state('');
	// The key field holds the stored mask rather than a real key (the
	// server never returns the full key): saves and tests send no key
	// while it is displayed.
	let keyIsMask = $state(false);

	// Save.
	let saving = $state(false);
	let saved = $state(false);
	let saveError = $state<string | null>(null);

	// Test-connection (the current form values, not the stored row).
	let testing = $state(false);
	let testResult = $state<LLMTestResult | null>(null);
	let testError = $state<string | null>(null);

	// Delete (the selected provider's stored row).
	let deleting = $state(false);
	let deleteError = $state<string | null>(null);

	// Preferences: appearance theme and default currency. Both PATCH the
	// users row (the layout's theme effect applies the theme live when the
	// store updates).
	let prefSaving = $state(false);
	let prefError = $state<string | null>(null);

	/** The currency picker offers the curated list; a stored currency
	 * outside it (the backend accepts any ISO-4217 code) is kept as an
	 * extra option so it can't be lost. */
	const currencyOptions = $derived.by(() => {
		const cur = $settings?.default_currency ?? '';
		return cur && !CURRENCIES.includes(cur) ? [cur, ...CURRENCIES] : CURRENCIES;
	});

	async function changeTheme(theme: string): Promise<void> {
		if (prefSaving) return;
		prefSaving = true;
		prefError = null;
		try {
			await updateSettings({ theme });
		} catch (e) {
			prefError = envelopeMessage(e);
		} finally {
			prefSaving = false;
		}
	}

	async function changeCurrency(currency: string): Promise<void> {
		if (prefSaving) return;
		prefSaving = true;
		prefError = null;
		try {
			await updateSettings({ default_currency: currency });
		} catch (e) {
			prefError = envelopeMessage(e);
		} finally {
			prefSaving = false;
		}
	}

	// Categories: the classification vocabulary (add, rename, show/hide,
	// remove). `active` is what the pickers (manual entry, review) offer;
	// removing a category keeps its transactions (FK ON DELETE SET NULL —
	// they become uncategorized).
	let categories = $state<Category[]>([]);
	let categoriesLoading = $state(true);
	let categoriesError = $state<string | null>(null);

	let newCategoryName = $state('');
	let adding = $state(false);
	let addError = $state<string | null>(null);

	let editingId: string | null = $state(null);
	let editName = $state('');
	let renaming = $state(false);
	let renameError = $state<string | null>(null);

	let removingId: string | null = $state(null);
	let removeError = $state<string | null>(null);

	async function loadCategories(): Promise<void> {
		categoriesLoading = true;
		categoriesError = null;
		try {
			categories = await listCategories();
		} catch (e) {
			categoriesError = envelopeMessage(e);
		} finally {
			categoriesLoading = false;
		}
	}

	/** Toggles whether the category is offered in the pickers. */
	async function toggleActive(cat: Category): Promise<void> {
		try {
			const updated = await patchCategory(cat.id, { active: !cat.active });
			categories = categories.map((c) => (c.id === updated.id ? updated : c));
		} catch (e) {
			categoriesError = envelopeMessage(e);
		}
	}

	async function addCategory(): Promise<void> {
		const name = newCategoryName.trim();
		if (adding || name === '') return;
		adding = true;
		addError = null;
		try {
			const cat = await createCategory({ name });
			categories = [...categories, cat];
			newCategoryName = '';
		} catch (e) {
			addError = envelopeMessage(e);
		} finally {
			adding = false;
		}
	}

	function startRename(cat: Category): void {
		editingId = cat.id;
		editName = cat.name;
		renameError = null;
	}

	async function commitRename(): Promise<void> {
		const name = editName.trim();
		if (renaming || editingId === null || name === '') return;
		renaming = true;
		renameError = null;
		try {
			const updated = await patchCategory(editingId, { name });
			categories = categories.map((c) => (c.id === updated.id ? updated : c));
			editingId = null;
		} catch (e) {
			renameError = envelopeMessage(e);
		} finally {
			renaming = false;
		}
	}

	function cancelRename(): void {
		editingId = null;
		renameError = null;
	}

	async function removeCategory(cat: Category): Promise<void> {
		if (!confirm($_('settings.cat_delete_confirm', { values: { name: cat.name } }))) return;
		removingId = cat.id;
		removeError = null;
		try {
			await deleteCategory(cat.id);
			categories = categories.filter((c) => c.id !== cat.id);
		} catch (e) {
			removeError = envelopeMessage(e);
		} finally {
			removingId = null;
		}
	}

	// Accounts (round 4): the money transactions, imports, and mortgages
	// point at. `default_schema_id` is import-specific (the schema an
	// import job applies by default) and deliberately not edited here.
	// Deleting an account removes its transactions, import jobs, and
	// mortgages (FKs ON DELETE CASCADE) — the confirm warns about it.
	let accounts = $state<Account[]>([]);
	let accountsLoading = $state(true);
	let accountsError = $state<string | null>(null);

	let newAccountName = $state('');
	let newAccountType = $state<AccountType>('bank');
	let newAccountSubtype = $state('');
	let newAccountCurrency = $state('');
	let newAccountBeneficiary = $state('');
	let newAccountBirthYear = $state('');
	let newAccountBalance = $state('');
	let newAccountOpened = $state('');
	let accountAdding = $state(false);
	let accountAddError = $state<string | null>(null);

	let accountEditingId: string | null = $state(null);
	let accountEditName = $state('');
	let accountEditType = $state<AccountType>('bank');
	let accountEditSubtype = $state('');
	let accountEditCurrency = $state('');
	let accountEditBeneficiary = $state('');
	let accountEditBirthYear = $state('');
	let accountEditBalance = $state('');
	let accountEditOpened = $state('');
	let accountSaving = $state(false);
	let accountSaveError = $state<string | null>(null);

	let accountRemovingId: string | null = $state(null);
	let accountRemoveError = $state<string | null>(null);

	async function loadAccounts(): Promise<void> {
		accountsLoading = true;
		accountsError = null;
		try {
			accounts = await listAccounts();
		} catch (e) {
			accountsError = envelopeMessage(e);
		} finally {
			accountsLoading = false;
		}
	}

	/** Prefills the new-account currency with the settings default until
	 * the user picks one (the select has no empty option, so a choice
	 * sticks). */
	$effect(() => {
		if (newAccountCurrency === '') {
			newAccountCurrency = $settings?.default_currency ?? '';
		}
	});

	/** Local YYYY-MM-DD (opened-date max + validation). */
	const today = (() => {
		const d = new Date();
		const m = String(d.getMonth() + 1).padStart(2, '0');
		const day = String(d.getDate()).padStart(2, '0');
		return `${d.getFullYear()}-${m}-${day}`;
	})();

	async function addAccount(): Promise<void> {
		const name = newAccountName.trim();
		if (accountAdding || name === '' || newAccountCurrency === '') return;
		const plan = isPlanType(newAccountType);
		const beneficiary = newAccountBeneficiary.trim();
		const birthYear = newAccountBirthYear.trim();
		const balance = newAccountBalance.trim();
		const opened = newAccountOpened.trim();
		if (plan && birthYear !== '' && !/^(19|20)\d{2}$/.test(birthYear)) {
			accountAddError = $_('settings.acct_birth_invalid');
			return;
		}
		if (plan && balance !== '' && !/^\d{1,7}(\.\d{1,2})?$/.test(balance)) {
			accountAddError = $_('settings.acct_balance_invalid');
			return;
		}
		if (plan && opened !== '' && opened > today) {
			accountAddError = $_('settings.acct_opened_invalid');
			return;
		}
		accountAdding = true;
		accountAddError = null;
		try {
			const subtype = newAccountSubtype.trim();
			const acct = await createAccount({
				name,
				type: newAccountType,
				currency: newAccountCurrency,
				subtype: subtype === '' ? undefined : subtype,
				beneficiary_name: plan && beneficiary !== '' ? beneficiary : undefined,
				beneficiary_birth_year: plan && birthYear !== '' ? birthYear : undefined,
				opening_balance: plan && balance !== '' ? balance : undefined,
				opened_at: plan && opened !== '' ? opened : undefined
			});
			accounts = [...accounts, acct];
			newAccountName = '';
			newAccountSubtype = '';
			newAccountCurrency = '';
			newAccountBeneficiary = '';
			newAccountBirthYear = '';
			newAccountBalance = '';
			newAccountOpened = '';
		} catch (e) {
			accountAddError = envelopeMessage(e);
		} finally {
			accountAdding = false;
		}
	}

	function startEditAccount(acct: Account): void {
		accountEditingId = acct.id;
		accountEditName = acct.name;
		accountEditType = acct.type as AccountType;
		accountEditSubtype = acct.subtype ?? '';
		accountEditCurrency = acct.currency;
		accountEditBeneficiary = acct.beneficiary_name ?? '';
		accountEditBirthYear =
			acct.beneficiary_birth_year === null ? '' : String(acct.beneficiary_birth_year);
		accountEditBalance = acct.opening_balance;
		accountEditOpened = acct.opened_at ?? '';
		accountSaveError = null;
	}

	async function commitAccountEdit(): Promise<void> {
		const name = accountEditName.trim();
		if (accountSaving || accountEditingId === null || name === '') return;
		const plan = isPlanType(accountEditType);
		const beneficiary = accountEditBeneficiary.trim();
		const birthYear = accountEditBirthYear.trim();
		const balance = accountEditBalance.trim();
		const opened = accountEditOpened.trim();
		if (plan && birthYear !== '' && !/^(19|20)\d{2}$/.test(birthYear)) {
			accountSaveError = $_('settings.acct_birth_invalid');
			return;
		}
		if (plan && balance !== '' && !/^\d{1,7}(\.\d{1,2})?$/.test(balance)) {
			accountSaveError = $_('settings.acct_balance_invalid');
			return;
		}
		if (plan && opened !== '' && opened > today) {
			accountSaveError = $_('settings.acct_opened_invalid');
			return;
		}
		accountSaving = true;
		accountSaveError = null;
		try {
			const subtype = accountEditSubtype.trim();
			const updated = await patchAccount(accountEditingId, {
				name,
				type: accountEditType,
				currency: accountEditCurrency,
				// Tri-state: an empty field clears the subtype.
				subtype: subtype === '' ? null : subtype,
				// Tri-state: the UI is the only gate for the plan fields —
				// a non-plan type clears them, and so does an empty field.
				beneficiary_name: plan ? (beneficiary === '' ? null : beneficiary) : null,
				beneficiary_birth_year: plan ? (birthYear === '' ? null : birthYear) : null,
				opening_balance: plan ? (balance === '' ? null : balance) : null,
				opened_at: plan ? (opened === '' ? null : opened) : null
			});
			accounts = accounts.map((a) => (a.id === updated.id ? updated : a));
			accountEditingId = null;
		} catch (e) {
			accountSaveError = envelopeMessage(e);
		} finally {
			accountSaving = false;
		}
	}

	function cancelEditAccount(): void {
		accountEditingId = null;
		accountSaveError = null;
	}

	async function removeAccount(acct: Account): Promise<void> {
		if (!confirm($_('settings.acct_delete_confirm', { values: { name: acct.name } }))) return;
		accountRemovingId = acct.id;
		accountRemoveError = null;
		try {
			await deleteAccount(acct.id);
			accounts = accounts.filter((a) => a.id !== acct.id);
		} catch (e) {
			accountRemoveError = envelopeMessage(e);
		} finally {
			accountRemovingId = null;
		}
	}

	/** The type's display label (settings.acct_type.<type>). */
	function accountTypeLabel(t: string): string {
		return $_('settings.acct_type.' + t);
	}

	/** The six registered plan types (PLAN_TYPES from #lib/api/savings). */
	function isPlanType(t: string): boolean {
		return (PLAN_TYPES as readonly string[]).includes(t);
	}

	/** The currency picker offers the curated list; a currency outside it
	 * (the backend accepts any ISO-4217 code) is kept as an extra option
	 * so it can't be lost. */
	function accountCurrencyOptions(extra?: string): string[] {
		return extra && !CURRENCIES.includes(extra) ? [extra, ...CURRENCIES] : CURRENCIES;
	}

	/** The stored row for the selected provider, if any. */
	const stored = $derived(credentials.find((c) => c.provider === provider) ?? null);

	/** openai and custom require a key; self_hosted does not. */
	const keyRequired = $derived(provider === 'openai' || provider === 'custom');

	/** Fills the endpoint and key fields from the stored row for the
	 * current provider (or the defaults): the stored endpoint, and the
	 * stored key's mask in the key field ("" when the row has no key). */
	function applyStored(): void {
		const row = credentials.find((c) => c.provider === provider);
		endpoint = row?.endpoint ?? DEFAULT_ENDPOINT;
		apiKey = row?.api_key_masked ?? '';
		keyIsMask = (row?.api_key_masked ?? '') !== '';
	}

	function selectProvider(p: LLMProvider): void {
		provider = p;
		applyStored();
		saved = false;
		saveError = null;
		testResult = null;
		testError = null;
		deleteError = null;
	}

	/** The key value to send: a real key only when the user typed one —
	 * the displayed mask is never sent as a key. */
	function keyToSend(): string | null {
		if (keyIsMask) return null;
		const v = apiKey.trim();
		return v === '' ? null : v;
	}

	/** Saves (or replaces) the selected provider's credential; on
	 * success the key field is replaced with the returned mask, so the
	 * full key never lingers in UI state beyond the request. */
	async function save(): Promise<void> {
		if (saving || endpoint.trim() === '') return;
		saving = true;
		saveError = null;
		saved = false;
		testResult = null;
		testError = null;
		try {
			const row = await putLLMCredential(provider, endpoint.trim(), keyToSend());
			apiKey = row.api_key_masked;
			keyIsMask = row.api_key_masked !== '';
			endpoint = row.endpoint;
			credentials = credentials.map((c) => (c.provider === row.provider ? row : c));
			saved = true;
		} catch (e) {
			saveError = envelopeMessage(e);
		} finally {
			saving = false;
		}
	}

	/** Fires one trivial chat-completions call at the current form
	 * values and reports the latency and token usage (or the error). */
	async function testConnection(): Promise<void> {
		if (testing || endpoint.trim() === '') return;
		testing = true;
		testError = null;
		testResult = null;
		try {
			testResult = await testLLMConnection(provider, endpoint.trim(), keyToSend());
		} catch (e) {
			testError = envelopeMessage(e);
		} finally {
			testing = false;
		}
	}

	/** Removes the selected provider's stored credential (204); the
	 * key field is cleared with it (the endpoint value the user has on
	 * screen is kept). */
	async function remove(): Promise<void> {
		if (deleting || stored === null) return;
		deleting = true;
		deleteError = null;
		try {
			await deleteLLMCredential(provider);
			credentials = credentials.filter((c) => c.provider !== provider);
			apiKey = '';
			keyIsMask = false;
			saved = false;
		} catch (e) {
			deleteError = envelopeMessage(e);
		} finally {
			deleting = false;
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

	// The credential list (the layout restores the session reactively;
	// a signed-out load simply 401s and the card stays hidden behind
	// the sign-in prompt).
	onMount(() => {
		// Step 8.1: the mail_enabled flag drives the "email disabled"
		// notice below; fail-open (a failed load just hides it).
		void loadSettings();
		void loadCategories();
		void loadAccounts();
		void listLLMCredentials()
			.then((rows) => {
				credentials = rows;
				loadError = null;
				applyStored();
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
	<h1 class="text-2xl font-semibold">{$_('settings.title')}</h1>
	<p class="max-w-2xl text-sm text-zinc-600 dark:text-zinc-400">{$_('settings.intro')}</p>

	{#if $settings && !$settings.mail_enabled}
		<p class="text-sm text-zinc-500 dark:text-zinc-400">{$_('settings.mail_disabled')}</p>
	{/if}

	{#if !$user}
		<p class="text-sm text-zinc-600 dark:text-zinc-400">
			{$_('settings.sign_in_prompt')}
			<button class="underline" onclick={() => login(page.url.pathname + page.url.search)}>
				{$_('auth.sign_in')}
			</button>
		</p>
	{:else}
		{#if $settings}
			<div class="space-y-4 rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
				<h2 class="text-lg font-medium">{$_('settings.prefs_title')}</h2>

				<label class="block text-sm">
					<span class="mb-1 block font-medium">{$_('settings.theme_label')}</span>
					<select
						class="w-64 rounded-md border border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 px-3 py-1.5"
						value={$settings.theme}
						disabled={prefSaving}
						onchange={(e) => void changeTheme(e.currentTarget.value)}
					>
						<option value="system">{$_('settings.theme.system')}</option>
						<option value="light">{$_('settings.theme.light')}</option>
						<option value="dark">{$_('settings.theme.dark')}</option>
					</select>
				</label>

				<label class="block text-sm">
					<span class="mb-1 block font-medium">{$_('settings.currency_label')}</span>
					<select
						class="w-64 rounded-md border border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 px-3 py-1.5"
						value={$settings.default_currency}
						disabled={prefSaving}
						onchange={(e) => void changeCurrency(e.currentTarget.value)}
					>
						{#each currencyOptions as code (code)}
							<option value={code}>{code}</option>
						{/each}
					</select>
				</label>

				{#if prefError}
					<p class="text-sm text-red-600 dark:text-red-400">{prefError}</p>
				{/if}
			</div>
		{/if}

		{#if loading}
			<p class="text-sm text-zinc-600 dark:text-zinc-400">{$_('settings.loading')}</p>
		{:else}
			{#if loadError}
				<p class="text-sm text-red-600 dark:text-red-400">{loadError}</p>
			{/if}

			<div class="space-y-4 rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
				<h2 class="text-lg font-medium">{$_('settings.llm_title')}</h2>

				<label class="block text-sm">
					<span class="mb-1 block font-medium">{$_('settings.provider_label')}</span>
					<select
						class="w-64 rounded-md border border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 px-3 py-1.5"
						value={provider}
						onchange={(e) => selectProvider(e.currentTarget.value as LLMProvider)}
					>
						<option value="self_hosted">{$_('settings.provider.self_hosted')}</option>
						<option value="openai">{$_('settings.provider.openai')}</option>
						<option value="custom">{$_('settings.provider.custom')}</option>
					</select>
				</label>

				<label class="block text-sm">
					<span class="mb-1 block font-medium">{$_('settings.endpoint_label')}</span>
					<input
						class="w-full max-w-md rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
						value={endpoint}
						oninput={(e) => {
							endpoint = e.currentTarget.value;
							saved = false;
							saveError = null;
						}}
					/>
				</label>

				<label class="block text-sm">
					<span class="mb-1 block font-medium">{$_('settings.api_key_label')}</span>
					<input
						type="password"
						autocomplete="new-password"
						class="w-full max-w-md rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
						value={apiKey}
						oninput={(e) => {
							apiKey = e.currentTarget.value;
							keyIsMask = false;
							saved = false;
							saveError = null;
						}}
					/>
					<span class="mt-1 block text-xs text-zinc-500 dark:text-zinc-400">
						{#if keyRequired}
							{$_('settings.api_key_required_note')}
						{:else}
							{$_('settings.api_key_optional_note')}
						{/if}
					</span>
				</label>

				{#if keyIsMask}
					<p class="text-xs text-zinc-500 dark:text-zinc-400">{$_('settings.mask_note')}</p>
				{/if}

				<div class="flex flex-wrap items-center gap-3">
					<button
						class="rounded-md bg-zinc-900 px-4 py-2 text-sm font-medium text-white hover:bg-zinc-700 disabled:opacity-50"
						disabled={saving || endpoint.trim() === ''}
						type="button"
						onclick={() => void save()}
					>
						{saving ? $_('settings.saving') : $_('settings.save')}
					</button>
					<button
						class="rounded-md border border-zinc-300 dark:border-zinc-700 px-4 py-2 text-sm font-medium text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-800 disabled:opacity-50"
						disabled={testing || endpoint.trim() === ''}
						type="button"
						onclick={() => void testConnection()}
					>
						{testing ? $_('settings.testing') : $_('settings.test')}
					</button>
					{#if stored}
						<button
							class="rounded-md border border-red-300 dark:border-red-900 px-4 py-2 text-sm font-medium text-red-600 dark:text-red-400 hover:bg-red-50 dark:hover:bg-red-950 disabled:opacity-50"
							disabled={deleting}
							type="button"
							onclick={() => void remove()}
						>
							{deleting ? $_('settings.deleting') : $_('settings.delete')}
						</button>
					{/if}
				</div>

				{#if saveError}
					<p class="text-sm text-red-600 dark:text-red-400">{saveError}</p>
				{/if}
				{#if saved}
					<p class="text-sm text-green-600 dark:text-green-400">{$_('settings.saved')}</p>
				{/if}
				{#if testError}
					<p class="text-sm text-red-600 dark:text-red-400">{testError}</p>
				{/if}
				{#if testResult}
					<p class="text-sm text-green-600 dark:text-green-400">
						{$_('settings.test_ok', {
							values: {
								ms: testResult.latency_ms,
								tin: testResult.tokens_in,
								tout: testResult.tokens_out
							}
						})}
						{#if testResult.model}
							{$_('settings.test_ok_model', { values: { model: testResult.model } })}
						{/if}
					</p>
				{/if}
				{#if deleteError}
					<p class="text-sm text-red-600 dark:text-red-400">{deleteError}</p>
				{/if}
			</div>
		{/if}

		<div class="space-y-4 rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
			<h2 class="text-lg font-medium">{$_('settings.categories_title')}</h2>
			<p class="max-w-2xl text-sm text-zinc-600 dark:text-zinc-400">{$_('settings.categories_note')}</p>

			{#if categoriesLoading}
				<p class="text-sm text-zinc-600 dark:text-zinc-400">{$_('settings.cat_loading')}</p>
			{:else}
				{#if categoriesError}
					<p class="text-sm text-red-600 dark:text-red-400">{categoriesError}</p>
				{:else}
					<ul class="overflow-hidden rounded-md border border-zinc-100 dark:border-zinc-800">
						{#each categories as cat (cat.id)}
							<li
								class="flex items-center gap-3 border-b border-zinc-100 dark:border-zinc-800 px-3 py-2 last:border-b-0"
							>
								<input
									type="checkbox"
									class="size-4"
									checked={cat.active}
									aria-label={$_('settings.cat_active_aria', { values: { name: cat.name } })}
									onchange={() => void toggleActive(cat)}
								/>
								{#if editingId === cat.id}
									<input
										class="flex-1 rounded-md border border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 px-2 py-1 text-sm"
										value={editName}
										oninput={(e) => {
											editName = e.currentTarget.value;
											renameError = null;
										}}
										onkeydown={(e) => {
											if (e.key === 'Enter') void commitRename();
											if (e.key === 'Escape') cancelRename();
										}}
									/>
									<div class="flex gap-2">
										<button
											class="rounded-md bg-zinc-900 px-2.5 py-1 text-xs font-medium text-white hover:bg-zinc-700 disabled:opacity-50"
											disabled={renaming || editName.trim() === ''}
											type="button"
											onclick={() => void commitRename()}
										>
											{$_('settings.save')}
										</button>
										<button
											class="rounded-md border border-zinc-300 dark:border-zinc-700 px-2.5 py-1 text-xs font-medium text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-800"
											type="button"
											onclick={() => cancelRename()}
										>
											{$_('settings.cancel')}
										</button>
									</div>
									{#if renameError}
										<p class="text-xs text-red-600 dark:text-red-400">{renameError}</p>
									{/if}
								{:else}
									<span
										class={
											'flex-1 text-sm ' +
											(cat.active
												? 'text-zinc-900 dark:text-zinc-100'
												: 'text-zinc-500 dark:text-zinc-400')
										}
									>
										{cat.name}
									</span>
									<div class="flex gap-2">
										<button
											class="rounded-md border border-zinc-300 dark:border-zinc-700 px-2.5 py-1 text-xs font-medium text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-800"
											type="button"
											onclick={() => startRename(cat)}
										>
											{$_('settings.cat_rename')}
										</button>
										<button
											class="rounded-md border border-red-300 dark:border-red-900 px-2.5 py-1 text-xs font-medium text-red-600 dark:text-red-400 hover:bg-red-50 dark:hover:bg-red-950 disabled:opacity-50"
											disabled={removingId !== null}
											type="button"
											onclick={() => void removeCategory(cat)}
										>
											{removingId === cat.id ? $_('settings.cat_deleting') : $_('settings.cat_delete')}
										</button>
									</div>
								{/if}
							</li>
						{/each}
					</ul>
					{#if removeError}
						<p class="text-sm text-red-600 dark:text-red-400">{removeError}</p>
					{/if}
				{/if}

				<div class="flex flex-wrap items-end gap-3">
					<label class="block text-sm">
						<span class="mb-1 block font-medium">{$_('settings.cat_add_label')}</span>
						<input
							class="w-56 rounded-md border border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 px-3 py-1.5"
							placeholder={$_('settings.cat_add_placeholder')}
							value={newCategoryName}
							oninput={(e) => {
								newCategoryName = e.currentTarget.value;
								addError = null;
							}}
							onkeydown={(e) => {
								if (e.key === 'Enter') void addCategory();
							}}
						/>
					</label>
					<button
						class="rounded-md bg-zinc-900 px-4 py-2 text-sm font-medium text-white hover:bg-zinc-700 disabled:opacity-50"
						disabled={adding || newCategoryName.trim() === ''}
						type="button"
						onclick={() => void addCategory()}
					>
						{adding ? $_('settings.cat_adding') : $_('settings.cat_add')}
					</button>
				</div>
				{#if addError}
					<p class="text-sm text-red-600 dark:text-red-400">{addError}</p>
				{/if}
			{/if}
		</div>

		<div
			id="accounts"
			class="space-y-4 rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5"
		>
			<h2 class="text-lg font-medium">{$_('settings.accounts_title')}</h2>
			<p class="max-w-2xl text-sm text-zinc-600 dark:text-zinc-400">{$_('settings.accounts_note')}</p>

			{#if accountsLoading}
				<p class="text-sm text-zinc-600 dark:text-zinc-400">{$_('settings.acct_loading')}</p>
			{:else}
				{#if accountsError}
					<p class="text-sm text-red-600 dark:text-red-400">{accountsError}</p>
				{:else}
					<ul class="overflow-hidden rounded-md border border-zinc-100 dark:border-zinc-800">
						{#each accounts as acct (acct.id)}
							<li
								class="flex flex-wrap items-center gap-3 border-b border-zinc-100 dark:border-zinc-800 px-3 py-2 last:border-b-0"
							>
								{#if accountEditingId === acct.id}
									<input
										class="w-44 rounded-md border border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 px-2 py-1 text-sm"
										maxlength="255"
										value={accountEditName}
										oninput={(e) => {
											accountEditName = e.currentTarget.value;
											accountSaveError = null;
										}}
										onkeydown={(e) => {
											if (e.key === 'Enter') void commitAccountEdit();
											if (e.key === 'Escape') cancelEditAccount();
										}}
									/>
									<select
										class="rounded-md border border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 px-2 py-1 text-sm"
										value={accountEditType}
										onchange={(e) => (accountEditType = e.currentTarget.value as AccountType)}
									>
										{#each ACCOUNT_TYPES as t (t)}
											<option value={t}>{accountTypeLabel(t)}</option>
										{/each}
									</select>
									<select
										class="rounded-md border border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 px-2 py-1 text-sm"
										value={accountEditCurrency}
										onchange={(e) => (accountEditCurrency = e.currentTarget.value)}
									>
										{#each accountCurrencyOptions(accountEditCurrency) as code (code)}
											<option value={code}>{code}</option>
										{/each}
									</select>
									<input
										class="w-32 rounded-md border border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 px-2 py-1 text-sm"
										maxlength="64"
										placeholder={$_('settings.acct_subtype_placeholder')}
										value={accountEditSubtype}
										oninput={(e) => {
											accountEditSubtype = e.currentTarget.value;
											accountSaveError = null;
										}}
										onkeydown={(e) => {
											if (e.key === 'Enter') void commitAccountEdit();
											if (e.key === 'Escape') cancelEditAccount();
										}}
									/>
									{#if accountEditType === 'resp' || accountEditType === 'rdsp'}
										<input
											class="w-32 rounded-md border border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 px-2 py-1 text-sm"
											maxlength="255"
											value={accountEditBeneficiary}
											oninput={(e) => {
												accountEditBeneficiary = e.currentTarget.value;
												accountSaveError = null;
											}}
											onkeydown={(e) => {
												if (e.key === 'Enter') void commitAccountEdit();
												if (e.key === 'Escape') cancelEditAccount();
											}}
										/>
										<input
											class="w-20 rounded-md border border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 px-2 py-1 text-sm"
											inputmode="numeric"
											maxlength="4"
											value={accountEditBirthYear}
											oninput={(e) => {
												accountEditBirthYear = e.currentTarget.value;
												accountSaveError = null;
											}}
											onkeydown={(e) => {
												if (e.key === 'Enter') void commitAccountEdit();
												if (e.key === 'Escape') cancelEditAccount();
											}}
										/>
									{/if}
									{#if isPlanType(accountEditType)}
										<input
											class="w-24 rounded-md border border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 px-2 py-1 text-sm"
											inputmode="decimal"
											value={accountEditBalance}
											oninput={(e) => {
												accountEditBalance = e.currentTarget.value;
												accountSaveError = null;
											}}
											onkeydown={(e) => {
												if (e.key === 'Enter') void commitAccountEdit();
												if (e.key === 'Escape') cancelEditAccount();
											}}
										/>
										<input
											class="w-32 rounded-md border border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 px-2 py-1 text-sm"
											type="date"
											max={today}
											value={accountEditOpened}
											oninput={(e) => {
												accountEditOpened = e.currentTarget.value;
												accountSaveError = null;
											}}
											onkeydown={(e) => {
												if (e.key === 'Enter') void commitAccountEdit();
												if (e.key === 'Escape') cancelEditAccount();
											}}
										/>
									{/if}
									<div class="flex gap-2">
										<button
											class="rounded-md bg-zinc-900 px-2.5 py-1 text-xs font-medium text-white hover:bg-zinc-700 disabled:opacity-50"
											disabled={accountSaving || accountEditName.trim() === ''}
											type="button"
											onclick={() => void commitAccountEdit()}
										>
											{$_('settings.save')}
										</button>
										<button
											class="rounded-md border border-zinc-300 dark:border-zinc-700 px-2.5 py-1 text-xs font-medium text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-800"
											type="button"
											onclick={() => cancelEditAccount()}
										>
											{$_('settings.cancel')}
										</button>
									</div>
									{#if accountSaveError}
										<p class="w-full text-xs text-red-600 dark:text-red-400">{accountSaveError}</p>
									{/if}
								{:else}
									<span class="flex-1 text-sm text-zinc-900 dark:text-zinc-100">
										{acct.name}
										<span class="text-zinc-500 dark:text-zinc-400">
											({accountTypeLabel(acct.type)} · {acct.currency}{#if acct.subtype}
												· {acct.subtype}
											{/if}{#if acct.beneficiary_name}
												· {acct.beneficiary_name}
											{/if})</span>
									</span>
									<div class="flex gap-2">
										<button
											class="rounded-md border border-zinc-300 dark:border-zinc-700 px-2.5 py-1 text-xs font-medium text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-800"
											type="button"
											onclick={() => startEditAccount(acct)}
										>
											{$_('settings.acct_edit')}
										</button>
										<button
											class="rounded-md border border-red-300 dark:border-red-900 px-2.5 py-1 text-xs font-medium text-red-600 dark:text-red-400 hover:bg-red-50 dark:hover:bg-red-950 disabled:opacity-50"
											disabled={accountRemovingId !== null}
											type="button"
											onclick={() => void removeAccount(acct)}
										>
											{accountRemovingId === acct.id ? $_('settings.acct_deleting') : $_('settings.acct_delete')}
										</button>
									</div>
								{/if}
							</li>
						{/each}
					</ul>
					{#if accountRemoveError}
						<p class="text-sm text-red-600 dark:text-red-400">{accountRemoveError}</p>
					{/if}
				{/if}

				<div class="flex flex-wrap items-end gap-3">
					<label class="block text-sm">
						<span class="mb-1 block font-medium">{$_('settings.acct_add_label')}</span>
						<input
							class="w-56 rounded-md border border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 px-3 py-1.5"
							maxlength="255"
							placeholder={$_('settings.acct_add_placeholder')}
							value={newAccountName}
							oninput={(e) => {
								newAccountName = e.currentTarget.value;
								accountAddError = null;
							}}
							onkeydown={(e) => {
								if (e.key === 'Enter') void addAccount();
							}}
						/>
					</label>
					<label class="block text-sm">
						<span class="mb-1 block font-medium">{$_('settings.acct_type_label')}</span>
						<select
							class="rounded-md border border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 px-3 py-1.5"
							bind:value={newAccountType}
						>
							{#each ACCOUNT_TYPES as t (t)}
								<option value={t}>{accountTypeLabel(t)}</option>
							{/each}
						</select>
					</label>
					<label class="block text-sm">
						<span class="mb-1 block font-medium">{$_('settings.acct_currency_label')}</span>
						<select
							class="rounded-md border border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 px-3 py-1.5"
							bind:value={newAccountCurrency}
						>
							{#each accountCurrencyOptions(newAccountCurrency) as code (code)}
								<option value={code}>{code}</option>
							{/each}
						</select>
					</label>
					<label class="block text-sm">
						<span class="mb-1 block font-medium">{$_('settings.acct_subtype_label')}</span>
						<input
							class="w-40 rounded-md border border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 px-3 py-1.5"
							maxlength="64"
							placeholder={$_('settings.acct_subtype_placeholder')}
							bind:value={newAccountSubtype}
						/>
					</label>
					{#if newAccountType === 'resp' || newAccountType === 'rdsp'}
						<label class="block text-sm">
							<span class="mb-1 block font-medium">{$_('settings.acct_beneficiary_label')}</span>
							<input
								class="w-40 rounded-md border border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 px-3 py-1.5"
								maxlength="255"
								placeholder={$_('settings.acct_beneficiary_placeholder')}
								bind:value={newAccountBeneficiary}
							/>
						</label>
						<label class="block text-sm">
							<span class="mb-1 block font-medium">{$_('settings.acct_birth_year_label')}</span>
							<input
								class="w-24 rounded-md border border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 px-3 py-1.5"
								inputmode="numeric"
								maxlength="4"
								placeholder={$_('settings.acct_birth_year_placeholder')}
								bind:value={newAccountBirthYear}
							/>
						</label>
					{/if}
					{#if isPlanType(newAccountType)}
						<label class="block text-sm">
							<span class="mb-1 block font-medium">{$_('settings.acct_balance_label')}</span>
							<input
								class="w-28 rounded-md border border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 px-3 py-1.5"
								inputmode="decimal"
								placeholder={$_('settings.acct_balance_placeholder')}
								bind:value={newAccountBalance}
							/>
						</label>
						<label class="block text-sm">
							<span class="mb-1 block font-medium">{$_('settings.acct_opened_label')}</span>
							<input
								class="w-36 rounded-md border border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 px-3 py-1.5"
								type="date"
								max={today}
								bind:value={newAccountOpened}
							/>
						</label>
					{/if}
					<button
						class="rounded-md bg-zinc-900 px-4 py-2 text-sm font-medium text-white hover:bg-zinc-700 disabled:opacity-50"
						disabled={accountAdding || newAccountName.trim() === '' || newAccountCurrency === ''}
						type="button"
						onclick={() => void addAccount()}
					>
						{accountAdding ? $_('settings.acct_adding') : $_('settings.acct_add')}
					</button>
				</div>
				{#if accountAddError}
					<p class="text-sm text-red-600 dark:text-red-400">{accountAddError}</p>
				{/if}
			{/if}
		</div>
	{/if}
</section>

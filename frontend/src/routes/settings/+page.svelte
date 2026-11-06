<script lang="ts">
	import { resolve } from '$app/paths';
	import { onMount } from 'svelte';
	import { _ } from 'svelte-i18n';
	import { user } from '#lib/auth/client';
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
	<p class="max-w-2xl text-sm text-zinc-600">{$_('settings.intro')}</p>

	{#if !$user}
		<p class="text-sm text-zinc-600">
			{$_('settings.sign_in_prompt')}
			<a class="underline" href={resolve('/')}>{$_('auth.sign_in')}</a>
		</p>
	{:else}
		{#if loading}
			<p class="text-sm text-zinc-600">{$_('settings.loading')}</p>
		{:else}
			{#if loadError}
				<p class="text-sm text-red-600">{loadError}</p>
			{/if}

			<div class="space-y-4 rounded-lg border border-zinc-200 bg-white p-5">
				<h2 class="text-lg font-medium">{$_('settings.llm_title')}</h2>

				<label class="block text-sm">
					<span class="mb-1 block font-medium">{$_('settings.provider_label')}</span>
					<select
						class="w-64 rounded-md border border-zinc-300 px-3 py-1.5"
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
						class="w-full max-w-md rounded-md border border-zinc-300 px-3 py-1.5"
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
						class="w-full max-w-md rounded-md border border-zinc-300 px-3 py-1.5"
						value={apiKey}
						oninput={(e) => {
							apiKey = e.currentTarget.value;
							keyIsMask = false;
							saved = false;
							saveError = null;
						}}
					/>
					<span class="mt-1 block text-xs text-zinc-500">
						{#if keyRequired}
							{$_('settings.api_key_required_note')}
						{:else}
							{$_('settings.api_key_optional_note')}
						{/if}
					</span>
				</label>

				{#if keyIsMask}
					<p class="text-xs text-zinc-500">{$_('settings.mask_note')}</p>
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
						class="rounded-md border border-zinc-300 px-4 py-2 text-sm font-medium text-zinc-700 hover:bg-zinc-100 disabled:opacity-50"
						disabled={testing || endpoint.trim() === ''}
						type="button"
						onclick={() => void testConnection()}
					>
						{testing ? $_('settings.testing') : $_('settings.test')}
					</button>
					{#if stored}
						<button
							class="rounded-md border border-red-300 px-4 py-2 text-sm font-medium text-red-600 hover:bg-red-50 disabled:opacity-50"
							disabled={deleting}
							type="button"
							onclick={() => void remove()}
						>
							{deleting ? $_('settings.deleting') : $_('settings.delete')}
						</button>
					{/if}
				</div>

				{#if saveError}
					<p class="text-sm text-red-600">{saveError}</p>
				{/if}
				{#if saved}
					<p class="text-sm text-green-600">{$_('settings.saved')}</p>
				{/if}
				{#if testError}
					<p class="text-sm text-red-600">{testError}</p>
				{/if}
				{#if testResult}
					<p class="text-sm text-green-600">
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
					<p class="text-sm text-red-600">{deleteError}</p>
				{/if}
			</div>
		{/if}
	{/if}
</section>

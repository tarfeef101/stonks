<script lang="ts">
	import { resolve } from '$app/paths';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { onMount } from 'svelte';
	import { _ } from 'svelte-i18n';
	import { displayName, login, user } from '#lib/auth/client';
	import { seedCategories } from '#lib/api/categories';
	import { ApiError } from '#lib/api/client';
	import {
		CURRENCIES,
		DEFAULT_CATEGORY_NAMES,
		loadSettings,
		settings,
		updateSettings
	} from '#lib/settings';

	// Onboarding (step 1.4): the first-login gate. The backend's
	// onboarded_at (1.4a) is the source of truth. The form renders
	// whenever we do NOT positively know onboarding is done — including
	// while the settings load is pending or failed (fail-open: a flaky GET
	// must never block the flow); the prefill then falls back to the
	// profile claim, the same value the backend seeded at bootstrap.
	//
	// Fields: email confirm/edit, default currency, notification cadence,
	// and the default-category choice (D-031): the DEFAULT_CATEGORY_NAMES
	// list as checkboxes (all checked) plus an add-your-own field; on
	// submit the checked names are seeded via POST /v1/categories/seed
	// (idempotent) before the onboarding stamp is set. The endpoint also
	// supports display_name and default_period — deliberately not in this
	// form; they belong to a settings screen, not the first-run gate.

	// Fields initialize once, from whatever is available at mount: the
	// settings store (loaded by the callback page / layout effect) with
	// the profile claim as fallback — the same value the backend seeded
	// at bootstrap.
	function initialEmail(): string {
		const e = $user?.profile.email;
		return typeof e === 'string' && e !== '' ? e : '';
	}

	let email = $state($settings?.email ?? initialEmail());
	let currency = $state($settings?.default_currency || 'CAD');
	let notifyMonthly = $state($settings?.notify_monthly ?? true);
	let notifyQuarterly = $state($settings?.notify_quarterly ?? false);
	let notifyAnnual = $state($settings?.notify_annual ?? false);

	// The category choice (D-031): the defaults are all checked; the
	// user unchecks the ones they don't want and can add their own.
	// The backend's uniqueness key is exact-case, so the UI dedupes
	// case-insensitively (first entry wins) — "Food" and "food" are one
	// choice to the user.
	const MAX_CATEGORY_NAME = 255; // backend user_categories.name limit
	let checked = $state<Record<string, boolean>>(
		Object.fromEntries(DEFAULT_CATEGORY_NAMES.map((n) => [n, true]))
	);
	let custom = $state<{ name: string; checked: boolean }[]>([]);
	let customInput = $state('');

	function addCustom(): void {
		const name = customInput.trim();
		if (name === '') return;
		const dup =
			DEFAULT_CATEGORY_NAMES.some((n) => n.toLowerCase() === name.toLowerCase()) ||
			custom.some((c) => c.name.toLowerCase() === name.toLowerCase());
		if (dup) return;
		custom = [...custom, { name, checked: true }];
		customInput = '';
	}

	/** The names seeded on submit: checked defaults, then checked
	 * custom entries in add order. */
	function selectedCategories(): string[] {
		const out: string[] = [];
		for (const n of DEFAULT_CATEGORY_NAMES) if (checked[n]) out.push(n);
		for (const c of custom) if (c.checked) out.push(c.name);
		return out;
	}

	let submitting = $state(false);
	let error = $state<string | null>(null);

	// The layout's sub-keyed effect loads the settings when the session is
	// restored; retry here for a direct landing with no (or failed) load.
	onMount(() => {
		if (!$settings) void loadSettings();
	});

	const done = $derived($settings !== null && $settings.onboarded_at !== null);

	/** The backend answers 4xx with the 3.1 envelope {error: {code, message}};
	 * ApiError folds the raw body into its message ("API 400: {...}") —
	 * carve the JSON out for the friendly line, fall back to the raw
	 * message when the body is not an envelope. */
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

	async function submit(): Promise<void> {
		submitting = true;
		error = null;
		try {
			// Seed the chosen vocabulary FIRST, then stamp onboarding
			// complete. The seed is idempotent, so a retry after a
			// settings-PATCH failure re-sends the same set (created 0).
			// A seed failure leaves onboarding incomplete — the user
			// stays on the page with the error line and can resubmit.
			await seedCategories(selectedCategories());
			// updateSettings stores the full response the backend
			// returns, so the UserChip prompt disappears the moment we
			// navigate — no second GET needed.
			await updateSettings({
				email: email.trim(),
				default_currency: currency,
				notify_monthly: notifyMonthly,
				notify_quarterly: notifyQuarterly,
				notify_annual: notifyAnnual,
				onboarded: true
			});
			goto(resolve('/'));
		} catch (e) {
			error = envelopeMessage(e);
			submitting = false;
		}
	}
</script>

<section class="space-y-3">
	<h1 class="text-2xl font-semibold">{$_('onboarding.title')}</h1>

	{#if !$user}
		<p class="text-sm text-zinc-600 dark:text-zinc-400">
			<button class="underline" onclick={() => login(page.url.pathname + page.url.search)}>
				{$_('auth.sign_in')}
			</button>
		</p>
	{:else if done}
		<p class="text-sm text-zinc-600 dark:text-zinc-400">{$_('onboarding.already_done')}</p>
		<a class="text-sm underline" href={resolve('/')}>{$_('onboarding.back_to_summary')}</a>
	{:else}
		<p class="text-sm text-zinc-600 dark:text-zinc-400">
			{$_('onboarding.sign_in_as')}
			<span class="font-medium text-zinc-900 dark:text-zinc-100">{displayName($user) || '—'}</span>
		</p>

		<form
			class="max-w-md space-y-5 rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5"
			onsubmit={(e) => {
				e.preventDefault();
				void submit();
			}}
		>
			<label class="block text-sm">
				<span class="mb-1 block font-medium">{$_('onboarding.email_label')}</span>
				<input
					bind:value={email}
					class="w-full rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
					required
					type="email"
				/>
			</label>

			<label class="block text-sm">
				<span class="mb-1 block font-medium">{$_('onboarding.currency_label')}</span>
				<select bind:value={currency} class="w-full rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5">
					{#each CURRENCIES as code (code)}
						<option value={code}>{code}</option>
					{/each}
				</select>
			</label>

			<fieldset class="text-sm">
				<legend class="mb-1 font-medium">{$_('onboarding.notifications')}</legend>
				<div class="space-y-1.5">
					<label class="flex items-center gap-2">
						<input bind:checked={notifyMonthly} type="checkbox" />
						{$_('onboarding.notify_monthly')}
					</label>
					<label class="flex items-center gap-2">
						<input bind:checked={notifyQuarterly} type="checkbox" />
						{$_('onboarding.notify_quarterly')}
					</label>
					<label class="flex items-center gap-2">
						<input bind:checked={notifyAnnual} type="checkbox" />
						{$_('onboarding.notify_annual')}
					</label>
				</div>
			</fieldset>

			<div class="text-sm">
				<p class="mb-1 font-medium">{$_('onboarding.categories_heading')}</p>
				<div class="grid grid-cols-2 gap-1.5">
					{#each DEFAULT_CATEGORY_NAMES as name (name)}
						<label class="flex items-center gap-2">
							<input bind:checked={checked[name]} type="checkbox" />
							{name}
						</label>
					{/each}
					{#each custom as c (c.name)}
						<label class="flex items-center gap-2">
							<input bind:checked={c.checked} type="checkbox" />
							{c.name}
						</label>
					{/each}
				</div>
				<div class="mt-2 flex gap-2">
					<input
						bind:value={customInput}
						class="flex-1 rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
						maxlength={MAX_CATEGORY_NAME}
						placeholder={$_('onboarding.categories_add_placeholder')}
						type="text"
					/>
					<button
						class="rounded-md border border-zinc-300 px-3 py-1.5 dark:border-zinc-700"
						disabled={customInput.trim() === ''}
						onclick={addCustom}
						type="button"
					>
						{$_('onboarding.categories_add')}
					</button>
				</div>
				<p class="mt-1.5 text-xs text-zinc-500 dark:text-zinc-400">{$_('onboarding.categories_note')}</p>
			</div>

			{#if error}
				<p class="text-sm text-red-600 dark:text-red-400">{error}</p>
			{/if}

			<button
				class="rounded-md bg-zinc-900 px-4 py-2 text-sm font-medium text-white hover:bg-zinc-700 disabled:opacity-50"
				disabled={submitting}
				type="submit"
			>
				{submitting ? $_('onboarding.submitting') : $_('onboarding.submit')}
			</button>
		</form>
	{/if}
</section>

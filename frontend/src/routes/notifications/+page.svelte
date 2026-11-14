<script lang="ts">
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import { onMount } from 'svelte';
	import { _ } from 'svelte-i18n';
	import { login, user } from '#lib/auth/client';
	import { ApiError } from '#lib/api/client';
	import {
		listNotifications,
		markNotificationRead,
		type Notification
	} from '#lib/api/notifications';
	import {
		classifyPayment,
		getMortgage,
		getMortgageMatching,
		type PaymentClassificationRequest
	} from '#lib/api/mortgages';

	// Notifications (step 3.3, surfaced in 5.5): the app-written alerts
	// (right now: the worker's "N transactions need review" after a
	// finished categorise job). Newest first (the backend's list order);
	// marking a row read updates it in place from the 200 response.
	//
	// A round-32 aberrant-payment alert whose ref names a rate-change
	// suggestion gets an inline accept/tweak/reject panel instead of the
	// deep link (accept the implied rate as-is, tweak its fields, or
	// reject it); the prefill is fetched lazily, only when the panel
	// opens.

	let items = $state<Notification[]>([]);
	let loadError = $state<string | null>(null);
	let loading = $state(true);

	/** Replaces the marked row with the 200 response's updated row
	 * (read_at is now set). */
	async function markRead(id: string): Promise<void> {
		try {
			const updated = await markNotificationRead(id);
			items = items.map((n) => (n.id === id ? updated : n));
		} catch (e) {
			loadError = envelopeMessage(e);
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

	/** read_at is an RFC 3339 timestamp; shown as the locale's full
	 * date and time. */
	function readAtLabel(iso: string): string {
		return new Date(iso).toLocaleString();
	}

	// The list. (The layout restores the session reactively; a
	// signed-out load simply 401s and the list stays hidden behind the
	// sign-in prompt.)
	async function reload(): Promise<void> {
		try {
			items = await listNotifications();
		} catch (e) {
			loadError = envelopeMessage(e);
		} finally {
			loading = false;
		}
	}
	onMount(() => {
		void reload();
	});

	// --- Suggestion review panel (round 32) --------------------------------
	// The aberrant alert's ref: the mortgage it belongs to, which
	// aberrant item it is (extra / short / suggestion) and its date.
	interface AberrantRef {
		mortgageId: string;
		kind: string;
		date: string;
	}

	/** The parsed ref, or null when the ref is not that shape (an
	 * unparseable ref keeps the plain deep link). */
	function aberrantRef(n: Notification): AberrantRef | null {
		const r = n.ref as Record<string, unknown> | null;
		if (r === null || typeof r !== 'object') return null;
		const mortgageId = r['mortgage_id'];
		const kind = r['kind'];
		const date = r['date'];
		if (
			typeof mortgageId !== 'string' ||
			typeof kind !== 'string' ||
			typeof date !== 'string'
		) {
			return null;
		}
		return { mortgageId, kind, date };
	}

	/** The tweak form: the three rate-change fields, seeded from the
	 * implied rate, the mortgage's rate type and a re-amortize. */
	interface SuggestionForm {
		annualRate: string;
		rateType: 'fixed' | 'variable';
		action: 're_amortize' | 'keep_payment';
	}

	interface SuggestionPanel {
		loading: boolean;
		loadError: string | null;
		/** The matching report's implied annual rate (2-decimal string). */
		impliedRate: string | null;
		/** The mortgage's rate type (the tweak form's seed). */
		rateType: 'fixed' | 'variable';
		showForm: boolean;
		form: SuggestionForm;
		/** The in-flight answer, if any. */
		busy: 'accept' | 'apply' | 'reject' | null;
		error: string | null;
	}

	/** The open panels, keyed by notification id (an entry survives the
	 * list refetch that follows an answer). */
	let panels = $state<Record<string, SuggestionPanel>>({});

	/** Opens the panel for one suggestion notification. The prefill —
	 * the matching report's suggestion row and the mortgage's rate
	 * type — is fetched lazily, only on first open. */
	async function openPanel(n: Notification): Promise<void> {
		const ref = aberrantRef(n);
		if (ref === null || ref.kind !== 'suggestion' || panels[n.id] !== undefined) return;
		let panel: SuggestionPanel = {
			loading: true,
			loadError: null,
			impliedRate: null,
			rateType: 'fixed',
			showForm: false,
			form: { annualRate: '', rateType: 'fixed', action: 're_amortize' },
			busy: null,
			error: null
		};
		panels[n.id] = panel;
		// Mutate through the stored $state proxy, not the raw object:
		// writes via the raw reference bypass the proxy, never
		// invalidate, and would leave the panel on "loading" forever.
		panel = panels[n.id];
		try {
			const [report, m] = await Promise.all([
				getMortgageMatching(ref.mortgageId),
				getMortgage(ref.mortgageId)
			]);
			const s = report.suggestions.find((x) => x.effective_date === ref.date);
			if (s === undefined) {
				panel.loadError = $_('notifications.suggestion_gone');
			} else {
				panel.impliedRate = s.implied_annual_rate;
				panel.rateType = m.rate_type === 'variable' ? 'variable' : 'fixed';
				panel.form = {
					annualRate: s.implied_annual_rate,
					rateType: panel.rateType,
					action: 're_amortize'
				};
			}
		} catch (e) {
			panel.loadError = envelopeMessage(e);
		} finally {
			panel.loading = false;
		}
	}

	/** Sends one of the three answers for the suggestion: accept the
	 * implied rate as-is (re-amortizing, the mortgage's rate type),
	 * apply the tweaked form values, or reject it (no rate fields).
	 * On success the panel closes and the list is refetched — the
	 * classify marks the notification read as a side effect. */
	async function act(n: Notification, which: 'accept' | 'apply' | 'reject'): Promise<void> {
		const ref = aberrantRef(n);
		const panel = panels[n.id];
		if (
			ref === null ||
			ref.kind !== 'suggestion' ||
			panel === undefined ||
			panel.busy !== null
		) {
			return;
		}
		if (which === 'apply' && panel.form.annualRate.trim() === '') {
			panel.error = $_('notifications.suggestion_rate_required');
			return;
		}
		panel.busy = which;
		panel.error = null;
		try {
			const body: PaymentClassificationRequest =
				which === 'reject'
					? { date: ref.date, kind: 'suggestion', classification: 'rejected' }
					: which === 'accept'
						? {
								date: ref.date,
								kind: 'suggestion',
								classification: 'rate_change',
								annual_rate: panel.impliedRate ?? '',
								rate_type: panel.rateType,
								action: 're_amortize'
							}
						: {
								date: ref.date,
								kind: 'suggestion',
								classification: 'rate_change',
								annual_rate: panel.form.annualRate,
								rate_type: panel.form.rateType,
								action: panel.form.action
							};
			await classifyPayment(ref.mortgageId, body);
			delete panels[n.id];
			await reload();
		} catch (e) {
			panel.error = envelopeMessage(e);
		} finally {
			panel.busy = null;
		}
	}

	/** Toggles the tweak form on or off. */
	function toggleForm(n: Notification): void {
		const p = panels[n.id];
		if (p !== undefined) p.showForm = !p.showForm;
	}

	/** Patches one tweak-form field (the selects arrive as strings). */
	function patchForm(n: Notification, field: keyof SuggestionForm, value: string): void {
		const p = panels[n.id];
		if (p === undefined) return;
		if (field === 'annualRate') p.form.annualRate = value;
		else if (field === 'rateType') p.form.rateType = value === 'variable' ? 'variable' : 'fixed';
		else p.form.action = value === 'keep_payment' ? 'keep_payment' : 're_amortize';
	}

	/** The mortgage's rate type, localized. */
	function rateTypeLabel(t: 'fixed' | 'variable'): string {
		return t === 'variable' ? $_('mortgage.rate_variable') : $_('mortgage.rate_fixed');
	}
</script>

<section class="space-y-3">
	<h1 class="text-2xl font-semibold">{$_('notifications.title')}</h1>
	<p class="max-w-2xl text-sm text-zinc-600 dark:text-zinc-400">{$_('notifications.intro')}</p>

	{#if !$user}
		<p class="text-sm text-zinc-600 dark:text-zinc-400">
			{$_('notifications.sign_in_prompt')}
			<button class="underline" onclick={() => login(page.url.pathname + page.url.search)}>
				{$_('auth.sign_in')}
			</button>
		</p>
	{:else}
		{#if loading}
			<p class="text-sm text-zinc-600 dark:text-zinc-400">{$_('notifications.loading')}</p>
		{:else}
			{#if loadError}
				<p class="text-sm text-red-600 dark:text-red-400">{loadError}</p>
			{/if}
			{#if items.length === 0 && !loadError}
				<p class="text-sm text-zinc-500 dark:text-zinc-400">{$_('notifications.empty')}</p>
			{:else}
				<ul class="space-y-2">
					{#each items as n (n.id)}
						<li
							class="flex items-start justify-between gap-4 rounded-lg border p-4 {n.read_at ===
							null
								? 'border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900'
								: 'border-zinc-200 dark:border-zinc-800 bg-zinc-50 dark:bg-zinc-950'}"
						>
							<div class="space-y-1">
								<p
									class="text-sm {n.read_at === null
										? 'font-semibold text-zinc-900 dark:text-zinc-100'
										: 'font-medium text-zinc-600 dark:text-zinc-400'}"
								>
									{n.title}
								</p>
								<p class="text-sm text-zinc-600 dark:text-zinc-400">{n.body}</p>
								<p class="text-xs text-zinc-400">
									{n.read_at === null
										? $_('notifications.unread')
										: `${$_('notifications.read_on')} ${readAtLabel(n.read_at)}`}
								</p>
								<!-- Round 32: an aberrant-payment alert deep-links to the
								 mortgage page; a rate-change suggestion is answered
								 inline instead — accept, tweak, or reject. -->
								{#if n.kind === 'mortgage_aberrant'}
									{@const ref = aberrantRef(n)}
									{#if ref !== null && ref.kind === 'suggestion'}
										{#if panels[n.id] === undefined}
											<button
												class="text-xs font-medium text-zinc-600 dark:text-zinc-400 underline decoration-zinc-300 underline-offset-2 hover:text-zinc-900 dark:hover:text-zinc-100"
												type="button"
												onclick={() => void openPanel(n)}
											>
												{$_('notifications.suggestion_review')}
											</button>
										{:else}
											{@const p = panels[n.id]}
											<div
												class="mt-2 rounded-md border border-zinc-200 dark:border-zinc-800 bg-zinc-50 dark:bg-zinc-950 p-3"
											>
												{#if p.loading}
													<p class="text-sm text-zinc-500 dark:text-zinc-400">
														{$_('notifications.suggestion_loading')}
													</p>
												{:else if p.loadError !== null}
													<p class="text-sm text-red-600 dark:text-red-400">{p.loadError}</p>
												{:else}
													<p class="text-sm text-zinc-600 dark:text-zinc-400">
														{$_('notifications.suggestion_implied')}
														<span class="font-semibold text-zinc-900 dark:text-zinc-100 tabular-nums">
															{p.impliedRate}{$_('mortgage.percent')}
														</span>
														· {$_('notifications.suggestion_current_type')}
														<span class="font-medium text-zinc-900 dark:text-zinc-100"
															>{rateTypeLabel(p.rateType)}</span
														>
													</p>
													{#if p.showForm}
														<div class="mt-2 grid grid-cols-1 gap-2 sm:grid-cols-3">
															<label class="text-xs text-zinc-500 dark:text-zinc-400">
																{$_('notifications.suggestion_rate_label')}
																<input
																	type="text"
																	inputmode="decimal"
																	class="mt-1 w-full rounded-md border border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 px-3 py-1.5 text-sm text-zinc-900 dark:text-zinc-100"
																	value={p.form.annualRate}
																	oninput={(e) =>
																		patchForm(n, 'annualRate', e.currentTarget.value)}
																/>
															</label>
															<label class="text-xs text-zinc-500 dark:text-zinc-400">
																{$_('notifications.suggestion_rate_type_label')}
																<select
																	class="mt-1 w-full rounded-md border border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 px-3 py-1.5 text-sm text-zinc-900 dark:text-zinc-100"
																	value={p.form.rateType}
																	onchange={(e) =>
																		patchForm(n, 'rateType', e.currentTarget.value)}
																>
																	<option value="fixed">{$_('mortgage.rate_fixed')}</option>
																	<option value="variable">{$_('mortgage.rate_variable')}</option>
																</select>
															</label>
															<label class="text-xs text-zinc-500 dark:text-zinc-400">
																{$_('notifications.suggestion_action_label')}
																<select
																	class="mt-1 w-full rounded-md border border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 px-3 py-1.5 text-sm text-zinc-900 dark:text-zinc-100"
																	value={p.form.action}
																	onchange={(e) =>
																		patchForm(n, 'action', e.currentTarget.value)}
																>
																	<option value="re_amortize">
																		{$_('mortgage.event_action_re_amortize')}
																	</option>
																	<option value="keep_payment">
																		{$_('mortgage.event_action_keep_payment')}
																	</option>
																</select>
															</label>
														</div>
														<div class="mt-3 flex items-center gap-2">
															<button
																type="button"
																class="rounded-md bg-zinc-900 px-3 py-1.5 text-sm font-medium text-white hover:bg-zinc-700 dark:bg-zinc-100 dark:text-zinc-900 dark:hover:bg-zinc-300 disabled:opacity-50"
																disabled={p.busy !== null}
																onclick={() => void act(n, 'apply')}
															>
																{p.busy === 'apply'
																	? $_('notifications.suggestion_applying')
																	: $_('notifications.suggestion_apply')}
															</button>
															<button
																type="button"
																class="rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5 text-sm text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-800 disabled:opacity-50"
																disabled={p.busy !== null}
																onclick={() => toggleForm(n)}
															>
																{$_('notifications.suggestion_tweak_close')}
															</button>
														</div>
													{:else}
														<div class="mt-3 flex flex-wrap items-center gap-2">
															<button
																type="button"
																class="rounded-md bg-zinc-900 px-3 py-1.5 text-sm font-medium text-white hover:bg-zinc-700 dark:bg-zinc-100 dark:text-zinc-900 dark:hover:bg-zinc-300 disabled:opacity-50"
																disabled={p.busy !== null}
																onclick={() => void act(n, 'accept')}
															>
																{p.busy === 'accept'
																	? $_('notifications.suggestion_accepting')
																	: $_('notifications.suggestion_accept')}
															</button>
															<button
																type="button"
																class="rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5 text-sm text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-800 disabled:opacity-50"
																disabled={p.busy !== null}
																onclick={() => toggleForm(n)}
															>
																{$_('notifications.suggestion_tweak')}
															</button>
															<button
																type="button"
																class="rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5 text-sm text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-800 disabled:opacity-50"
																disabled={p.busy !== null}
																onclick={() => void act(n, 'reject')}
															>
																{p.busy === 'reject'
																	? $_('notifications.suggestion_rejecting')
																	: $_('notifications.suggestion_reject')}
															</button>
														</div>
													{/if}
													{#if p.error !== null}
														<p class="mt-2 text-sm text-red-600 dark:text-red-400">{p.error}</p>
													{/if}
												{/if}
											</div>
										{/if}
									{:else}
										<a
											class="text-xs font-medium text-zinc-600 dark:text-zinc-400 underline decoration-zinc-300 underline-offset-2 hover:text-zinc-900 dark:hover:text-zinc-100"
											href={resolve('/mortgage')}
										>
											{$_('notifications.goto_mortgage')}
										</a>
									{/if}
								{/if}
							</div>
							{#if n.read_at === null}
								<button
									class="shrink-0 rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5 text-sm text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-800"
									type="button"
									onclick={() => void markRead(n.id)}
								>
									{$_('notifications.mark_read')}
								</button>
							{/if}
						</li>
					{/each}
				</ul>
			{/if}
		{/if}
	{/if}
</section>

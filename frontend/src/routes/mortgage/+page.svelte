<script module lang="ts">
	// Amount parsing + the money-formatter cache, at module scope: the
	// cache is memoisation, not component state. money() is called from
	// template expressions, so the cache must not be reactive state
	// (mutating it during render is a state_unsafe_mutation); a plain
	// module-level object is render-safe.
	//
	// Amounts arrive as canonical 2-decimal strings in the account's
	// currency (3-letter uppercase ISO-4217, backend-validated). The
	// formatter localises them in the browser's own locale with that
	// currency — Intl supplies the symbol (en-CA + CAD → "CA$"), which
	// is never assumed; a missing/unknown currency renders a plain
	// grouped number with two decimals and no symbol.

	const CENTS_RE = /^(-?)(\d+)(?:\.(\d{1,2}))?$/;

	/** A canonical 2-decimal amount string as integer cents. Used for
	 * geometry (chart coordinates, year-row totals, slider seeds) and
	 * as the exact parse behind money()'s display formatting. */
	function toCents(s: string): number {
		const m = CENTS_RE.exec(s);
		if (!m) return 0;
		const cents = (m[3] ?? '').padEnd(2, '0');
		const v = parseInt(m[2], 10) * 100 + parseInt(cents, 10);
		return m[1] === '-' ? -v : v;
	}

	const nfCache: Record<string, Intl.NumberFormat> = {};

	/** The display formatter for a currency code (cached per code):
	 * the browser locale + that currency, or a plain grouped 2-decimal
	 * number when the code is missing or Intl does not know it. */
	function numberFormat(currency: string | null): Intl.NumberFormat {
		const key = currency ?? '';
		let nf = nfCache[key];
		if (nf === undefined) {
			if (currency !== null && /^[A-Z]{3}$/.test(currency)) {
				try {
					nf = new Intl.NumberFormat(undefined, { style: 'currency', currency });
				} catch {
					// not a code Intl knows — fall through to plain
				}
			}
			if (nf === undefined) {
				nf = new Intl.NumberFormat(undefined, {
					minimumFractionDigits: 2,
					maximumFractionDigits: 2
				});
			}
			nfCache[key] = nf;
		}
		return nf;
	}
</script>

<script lang="ts">
	import { resolve } from '$app/paths';
	import { page } from '$app/state';
	import { onMount } from 'svelte';
	import { SvelteMap, SvelteSet } from 'svelte/reactivity';
	import { _ } from 'svelte-i18n';
	import { login, user } from '#lib/auth/client';
	import { ApiError } from '#lib/api/client';
	import { listAccounts, type Account } from '#lib/api/accounts';
	import {
		listMortgages,
		createMortgage,
		patchMortgage,
		getMortgageAmortization,
		whatIfMortgageAmortization,
		getMortgageMatching,
		type AmortizationPayment,
		type AmortizationSchedule,
		type MatchingReport,
		type MatchingSuggestion,
		type Mortgage,
		type MortgageCreate,
		type MortgageRateEvent,
		type WhatIfRequest,
		type WhatIfResult
	} from '#lib/api/mortgages';

	// Mortgage page (step 7.3): the stored mortgage spec — KPI line
	// (incl. the next renewal when the spec has a term), balance/equity
	// chart, yearly-collapsed schedule table, the rate-change events
	// (round 23) — plus the what-if comparison panel over the 7.2 engine
	// (rate, extra payments, payment frequency, renewals).
	//
	// Rendered-numbers contract: the backend serves every amount as a
	// canonical 2-decimal string. Display goes through money(), which
	// localises the value in the browser's own locale with the
	// mortgage account's currency — the symbol is localised, never
	// assumed (a missing/unknown currency renders a plain grouped
	// number). The parse is exact integer-cent arithmetic; an
	// unparseable value renders verbatim. All other numeric parsing is
	// geometry: chart coordinates, year-row totals and slider seeds.

	// --- Data: the mortgage accounts + the mortgage list ----------------
	let mortgageAccounts = $state<Account[]>([]);
	let mortgages = $state<Mortgage[]>([]);
	let selectedId = $state('');
	let listError = $state<string | null>(null);
	let listLoaded = $state(false);

	onMount(() => {
		// Like the root page's account picker: a signed-out load simply
		// 401s and the page stays on the sign-in prompt.
		listAccounts()
			.then((a) => {
				mortgageAccounts = a.filter((x) => x.type === 'mortgage');
			})
			.catch(() => {
				mortgageAccounts = [];
			});
		loadMortgages();
	});

	/** The mortgage list; the first row is auto-selected when nothing
	 * is selected yet. */
	function loadMortgages(): void {
		listMortgages()
			.then((m) => {
				mortgages = m;
				listError = null;
				if (selectedId === '' && m.length > 0) selectedId = m[0].id;
				listLoaded = true;
			})
			.catch((e) => {
				listError = envelopeMessage(e);
				listLoaded = true;
			});
	}

	/** The selected row (null when the list has not loaded it). */
	function selectedMortgage(): Mortgage | null {
		return mortgages.find((m) => m.id === selectedId) ?? null;
	}

	/** The selected row as a derived binding so the template can
	 * null-narrow it inside an {#if}. */
	const detailMortgage = $derived.by(() => selectedMortgage());

	// --- Amortization fetch: one per selected mortgage ------------------
	let amortization = $state<AmortizationSchedule | null>(null);
	let amortizing = $state(false);
	let amortError = $state<string | null>(null);
	let amortSeq = 0;

	/** Fetch the selected mortgage's schedule (seq-guarded against
	 * stale responses) and reset the what-if controls — the stored spec
	 * is the base of every comparison. Also re-run after a successful
	 * rate-event PATCH (a renewal moves next_renewal). */
	function loadAmortization(): Promise<void> {
		const id = selectedId;
		const seq = ++amortSeq;
		amortizing = true;
		amortization = null;
		amortError = null;
		resetWhatIf();
		return getMortgageAmortization(id)
			.then((s) => {
				if (seq === amortSeq) amortization = s;
			})
			.catch((e) => {
				if (seq === amortSeq) amortError = envelopeMessage(e);
			})
			.finally(() => {
				if (seq === amortSeq) amortizing = false;
			});
	}

	$effect(() => {
		const sub = $user?.profile.sub ?? null;
		const id = selectedId;
		if (sub === null || id === '') {
			++amortSeq; // discard any in-flight fetch
			++matchSeq;
			amortization = null;
			amortError = null;
			amortizing = false;
			matching = null;
			matchingError = null;
			matchingLoading = false;
			return;
		}
		void loadAmortization();
		void loadMatching();
	});

	// --- What-if state ----------------------------------------------------
	let wiRate = $state(0.5);
	let wiExtra = $state(0);
	let wiFreq = $state('monthly');
	let wiOneTime = $state(0);
	let wiDate = $state('');
	// Round 24: the simulated payment frequency + a simulated renewal.
	let wiPayFreq = $state('monthly');
	let wiRenewDate = $state('');
	let wiRenewRate = $state('');
	let wiRenewType = $state('fixed');
	// true when the last compare sent a payment_frequency override —
	// months_saved is then measured in PAYMENT PERIODS, not months.
	let wiFreqApplied = $state(false);
	let whatIf = $state<WhatIfResult | null>(null);
	let comparing = $state(false);
	let compareError = $state<string | null>(null);
	let compareSeq = 0;
	let expandedYears = new SvelteSet<string>();

	/** Restore the what-if controls to the stored spec values (and clear
	 * any comparison); the rate slider clamps the stored rate to its
	 * range. */
	function resetWhatIf(): void {
		compareSeq++;
		const m = selectedMortgage();
		wiOneTime = 0;
		wiDate = '';
		wiPayFreq = 'monthly';
		wiRenewDate = '';
		wiRenewRate = '';
		wiRenewType = 'fixed';
		wiFreqApplied = false;
		whatIf = null;
		compareError = null;
		expandedYears.clear();
		if (m === null) return;
		wiRate = clampRate(m.annual_rate);
		wiExtra = toCents(m.extra_payment_amount) / 100;
		wiFreq = m.extra_payment_frequency;
		wiPayFreq = m.payment_frequency;
		wiRenewDate = nextRenewalOf(m) ?? '';
		wiRenewRate = m.annual_rate;
		wiRenewType = m.rate_type === 'variable' ? 'variable' : 'fixed';
	}

	function clampRate(s: string): number {
		const v = parseFloat(s);
		if (!Number.isFinite(v)) return 0.5;
		return Math.min(15, Math.max(0.5, v));
	}

	/** The rate-type label; a legacy empty value reads as fixed (the
	 * backend never fabricates one). */
	function rateTypeLabel(t: string): string {
		return t === 'variable' ? $_('mortgage.rate_variable') : $_('mortgage.rate_fixed');
	}

	/** Client mirror of the backend NextRenewalDate: the last re_amortize
	 * event date (else the start) plus the term; null when the mortgage
	 * has no term. INPUT PREFILL ONLY — the schedule's next_renewal is
	 * the source of truth on display. */
	function nextRenewalOf(m: Mortgage): string | null {
		if (m.term_years <= 0) return null;
		let base = m.start_date;
		for (const ev of m.rate_events) if (ev.action === 're_amortize') base = ev.effective_date;
		const d = new Date(`${base}T00:00:00Z`);
		if (Number.isNaN(d.getTime())) return null;
		d.setUTCFullYear(d.getUTCFullYear() + m.term_years);
		return d.toISOString().slice(0, 10);
	}

	/** Compare the stored spec against the scenario, built by the pinned
	 * rule: the rate always (the slider value, 2 decimals); the extra
	 * pair only when extra > 0; the one-time prepayment only when the
	 * amount > 0 AND the date is set. */
	async function compare(): Promise<void> {
		const m = selectedMortgage();
		if (m === null || comparing) return;
		const seq = ++compareSeq;
		comparing = true;
		compareError = null;
		const body: WhatIfRequest = { annual_rate: wiRate.toFixed(2) };
		if (wiExtra > 0) {
			body.extra_payment_amount = wiExtra.toFixed(2);
			body.extra_payment_frequency = wiFreq;
		}
		if (wiOneTime > 0 && wiDate !== '') {
			body.one_time_prepayment = { amount: wiOneTime.toFixed(2), date: wiDate };
		}
		// Sent only when it differs from the stored frequency; the
		// renewal only when a date is set (the rate type is explicit).
		if (wiPayFreq !== m.payment_frequency) body.payment_frequency = wiPayFreq;
		if (wiRenewDate !== '') {
			body.renewal = { date: wiRenewDate, annual_rate: wiRenewRate, rate_type: wiRenewType };
		}
		try {
			const r = await whatIfMortgageAmortization(m.id, body);
			if (seq !== compareSeq) return;
			whatIf = r;
			wiFreqApplied = body.payment_frequency !== undefined;
		} catch (e) {
			if (seq !== compareSeq) return;
			whatIf = null;
			compareError = envelopeMessage(e);
		} finally {
			if (seq === compareSeq) comparing = false;
		}
	}

	/** Toggle a year row's monthly detail rows. */
	function toggleYear(year: string): void {
		if (expandedYears.has(year)) expandedYears.delete(year);
		else expandedYears.add(year);
	}

	// --- Add form (the 9 create fields + the round-23 term metadata) -----
	let form = $state({
		account_id: '',
		principal: '',
		annual_rate: '',
		amortization_years: 30,
		start_date: '',
		payment_frequency: 'monthly',
		extra_payment_amount: '0.00',
		extra_payment_frequency: 'monthly',
		extra_prepayment_type: 'term',
		term_years: 0,
		rate_type: 'fixed'
	});
	let creating = $state(false);
	let createError = $state<string | null>(null);

	/** Create the mortgage, refresh the list and select the new row;
	 * a server 400 message is shown verbatim on error. term_years is
	 * truncated to an integer (an emptied number input binds null at
	 * runtime despite its number type). */
	async function addMortgage(): Promise<void> {
		if (creating) return;
		creating = true;
		createError = null;
		try {
			const body: MortgageCreate = { ...form, term_years: Math.trunc(form.term_years ?? 0) };
			const m = await createMortgage(body);
			const all = await listMortgages();
			mortgages = all;
			listError = null;
			selectedId = m.id;
		} catch (e) {
			createError = envelopeMessage(e);
		} finally {
			creating = false;
		}
	}

	// --- Rate-change events (round 23 data, round 24 UI) -------------------
	let evDate = $state('');
	let evRate = $state('');
	let evRateType = $state('fixed');
	let evAction = $state('re_amortize');
	let evError = $state<string | null>(null);
	let evBusy = $state(false);

	/** PATCH the full event array (server contract: a non-null
	 * rate_events REPLACES the whole array), swap the row in the list,
	 * and refetch the schedule (a renewal moves next_renewal) and the
	 * matching report (the stored events re-price the actual balance). */
	async function applyRateEvents(id: string, events: MortgageRateEvent[]): Promise<void> {
		const updated = await patchMortgage(id, { rate_events: events });
		mortgages = mortgages.map((x) => (x.id === updated.id ? updated : x));
		await Promise.all([loadAmortization(), loadMatching()]);
	}

	/** Add a rate change; the array is re-sorted ascending before it is
	 * sent (a duplicate date 400s server-side and surfaces verbatim). */
	async function addRateEvent(): Promise<void> {
		const m = selectedMortgage();
		if (m === null || evBusy || evDate === '') return;
		evBusy = true;
		evError = null;
		try {
			const next = [
				...m.rate_events,
				{ effective_date: evDate, annual_rate: evRate, rate_type: evRateType, action: evAction }
			];
			next.sort((a, b) => a.effective_date.localeCompare(b.effective_date));
			await applyRateEvents(m.id, next);
			evDate = '';
			evRate = '';
		} catch (e) {
			evError = envelopeMessage(e);
		} finally {
			evBusy = false;
		}
	}

	/** Remove a stored rate change (index into the stored array). */
	async function removeRateEvent(i: number): Promise<void> {
		const m = selectedMortgage();
		if (m === null || evBusy) return;
		evBusy = true;
		evError = null;
		try {
			const next = m.rate_events.filter((_, j) => j !== i);
			await applyRateEvents(m.id, next);
		} catch (e) {
			evError = envelopeMessage(e);
		} finally {
			evBusy = false;
		}
	}

	// --- Transaction matching (round 25) -----------------------------------
	let matching = $state<MatchingReport | null>(null);
	let matchingLoading = $state(false);
	let matchingError = $state<string | null>(null);
	let matchSeq = 0;

	/** Fetch the selected mortgage's matching report (seq-guarded
	 * against stale responses). Read-only: the backend replays the
	 * linked account's debits against the stored spec's schedule. */
	function loadMatching(): Promise<void> {
		const id = selectedId;
		const seq = ++matchSeq;
		matchingLoading = true;
		matching = null;
		matchingError = null;
		return getMortgageMatching(id)
			.then((r) => {
				if (seq === matchSeq) matching = r;
			})
			.catch((e) => {
				if (seq === matchSeq) matchingError = envelopeMessage(e);
			})
			.finally(() => {
				if (seq === matchSeq) matchingLoading = false;
			});
	}

	/** Localized text for a backend matching flag; a flag we do not
	 * know (the backend added a new one) falls back to the raw token. */
	function matchingFlagLabel(f: string): string {
		switch (f) {
			case 'no_transactions':
				return $_('mortgage.match_flag_no_transactions');
			case 'payment_below_minimum_payoff':
				return $_('mortgage.match_flag_payment_below_minimum_payoff');
			case 'payment_above_maximum_rate':
				return $_('mortgage.match_flag_payment_above_maximum_rate');
			default:
				return f;
		}
	}

	/** Confirm a rate-change suggestion through the round-23 PATCH path:
	 * insert the implied rate as a re-amortizing event (sorted
	 * ascending, the mortgage's rate type kept), then refetch the
	 * schedule and the report. A duplicate date 400s server-side and
	 * surfaces verbatim. */
	async function applySuggestion(s: MatchingSuggestion): Promise<void> {
		const m = selectedMortgage();
		if (m === null || evBusy) return;
		evBusy = true;
		evError = null;
		try {
			const next = [
				...m.rate_events,
				{
					effective_date: s.effective_date,
					annual_rate: s.implied_annual_rate,
					rate_type: m.rate_type === 'variable' ? 'variable' : 'fixed',
					action: 're_amortize'
				}
			];
			next.sort((a, b) => a.effective_date.localeCompare(b.effective_date));
			await applyRateEvents(m.id, next);
		} catch (e) {
			evError = envelopeMessage(e);
		} finally {
			evBusy = false;
		}
	}

	// --- Chart + table geometry -------------------------------------------
	const EQUITY_W = 100;
	const EQUITY_H = 40;

	/** The balance/equity polylines scaled into the fixed viewBox, or
	 * null when the schedule is too short or the stored principal is
	 * not a positive amount (pure float coordinate math). */
	const equitySeries = $derived.by(() => {
		const m = selectedMortgage();
		const s = amortization;
		if (m === null || s === null) return null;
		const pc = toCents(m.principal);
		const n = s.payments.length;
		if (n < 2 || pc <= 0) return null;
		const balance: string[] = [];
		const equity: string[] = [];
		// Full float coordinates: snapping to integers stair-steps
		// both lines at this viewBox's display size.
		for (let i = 0; i < n; i++) {
			const bc = toCents(s.payments[i].balance);
			const x = ((i / (n - 1)) * EQUITY_W).toFixed(2);
			const balanceY = (EQUITY_H * (1 - bc / pc)).toFixed(2);
			const equityY = ((EQUITY_H * bc) / pc).toFixed(2);
			balance.push(`${x},${balanceY}`);
			equity.push(`${x},${equityY}`);
		}
		return { balance: balance.join(' '), equity: equity.join(' ') };
	});

	/** The payments grouped into year rows (calendar order); the yearly
	 * interest/principal totals are summed in integer cents, the end
	 * balance is the last payment's balance string of the year. */
	const yearRows = $derived.by(() => {
		const s = amortization;
		if (s === null) return [];
		const rows: {
			year: string;
			interest: number;
			principal: number;
			endBalance: string;
			months: AmortizationPayment[];
		}[] = [];
		const byYear = new SvelteMap<string, (typeof rows)[number]>();
		for (const p of s.payments) {
			const year = p.date.slice(0, 4);
			let row = byYear.get(year);
			if (row === undefined) {
				row = { year, interest: 0, principal: 0, endBalance: '', months: [] };
				byYear.set(year, row);
				rows.push(row);
			}
			row.interest += toCents(p.interest);
			row.principal += toCents(p.principal);
			row.endBalance = p.balance;
			row.months.push(p);
		}
		return rows;
	});

	/** The interest_saved line colour: green when the scenario saves
	 * interest, red when it costs more, neutral at 0. */
	function savedClass(v: string): string {
		const n = parseFloat(v);
		if (!Number.isFinite(n) || n === 0) return 'text-zinc-900 dark:text-zinc-100';
		return n > 0 ? 'text-green-700 dark:text-green-400' : 'text-red-600 dark:text-red-400';
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

	// --- Localised money display -----------------------------------------
	// CENTS_RE, toCents() and numberFormat() live in the module-scope
	// script above (render-safe memoisation, not component state);
	// money()/moneyNum() format with that formatter, localised to the
	// account currency (see module comment for the symbol contract).

	/** A raw API amount for display, localised to the account currency.
	 * Null/empty pass through as '' so the call site's sentinel stands;
	 * an unparseable value renders verbatim (never mangled). */
	function money(amount: string | null | undefined, currency: string | null): string {
		if (amount === null || amount === undefined || amount === '') return '';
		if (!CENTS_RE.test(amount)) return amount;
		return numberFormat(currency).format(toCents(amount) / 100);
	}

	/** A numeric amount (slider values, row totals) for display, with
	 * the same localisation as money(). */
	function moneyNum(value: number, currency: string | null): string {
		if (!Number.isFinite(value)) return '';
		return numberFormat(currency).format(value);
	}

	/** The currency of an account row, or null when the account list
	 * has not loaded it (the display then omits any symbol). */
	function accountCurrency(accountId: string): string | null {
		const acct = mortgageAccounts.find((a) => a.id === accountId);
		return acct === undefined ? null : acct.currency;
	}

	/** The selected mortgage's account currency, or null (the list
	 * row uses the same lookup on its own account_id). */
	const selectedCurrency = $derived.by(() => {
		const m = selectedMortgage();
		return m === null ? null : accountCurrency(m.account_id);
	});
</script>

<section class="space-y-4">
	<h1 class="text-2xl font-semibold">{$_('mortgage.heading')}</h1>

	{#if !$user}
		<p class="text-sm text-zinc-600 dark:text-zinc-400">
			{$_('mortgage.sign_in_prompt')}
			<button class="underline" onclick={() => login(page.url.pathname + page.url.search)}>
				{$_('auth.sign_in')}
			</button>
		</p>
	{:else if !listLoaded}
		<p class="text-sm text-zinc-600 dark:text-zinc-400">{$_('mortgage.loading')}</p>
	{:else if mortgageAccounts.length === 0}
		<p class="text-zinc-600 dark:text-zinc-400">
			{$_('mortgage.no_mortgage')}
			<a class="underline" href={resolve('/settings#accounts')}>{$_('common.manage_accounts')}</a>
		</p>
	{:else}
		{#if listError}
			<p class="text-sm text-red-600 dark:text-red-400">{listError}</p>
		{/if}

		{#if mortgages.length === 0}
			<!-- No mortgage on file: the 9-field create form. -->
			<div class="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
				<p class="text-xs font-medium text-zinc-500 dark:text-zinc-400 uppercase">{$_('mortgage.add_title')}</p>
				<p class="mt-1 text-sm text-zinc-600 dark:text-zinc-400">{$_('mortgage.add_note')}</p>
				<form
					class="mt-4 grid grid-cols-1 gap-4 sm:grid-cols-2"
					onsubmit={(e) => {
						e.preventDefault();
						void addMortgage();
					}}
				>
					<div class="block text-sm">
						<label class="block text-sm">
							<span class="mb-1 block font-medium">{$_('mortgage.form_account')}</span>
							<select
								class="w-full rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
								bind:value={form.account_id}
							>
								<option value="" disabled>{$_('mortgage.form_account_placeholder')}</option>
								{#each mortgageAccounts as a (a.id)}
									<option value={a.id}>{a.name}</option>
								{/each}
							</select>
						</label>
						<a class="mt-1 block text-xs text-zinc-500 dark:text-zinc-400 underline" href={resolve('/settings#accounts')}>
							{$_('common.manage_accounts')}
						</a>
					</div>
					<label class="block text-sm">
						<span class="mb-1 block font-medium">{$_('mortgage.form_start')}</span>
						<input
							type="date"
							class="w-full rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
							bind:value={form.start_date}
						/>
					</label>
					<label class="block text-sm">
						<span class="mb-1 block font-medium">{$_('mortgage.form_principal')}</span>
						<input
							type="text"
							inputmode="decimal"
							class="w-full rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
							placeholder={$_('mortgage.form_principal_placeholder')}
							bind:value={form.principal}
						/>
					</label>
					<label class="block text-sm">
						<span class="mb-1 block font-medium">{$_('mortgage.form_rate')}</span>
						<input
							type="text"
							inputmode="decimal"
							class="w-full rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
							placeholder={$_('mortgage.form_rate_placeholder')}
							bind:value={form.annual_rate}
						/>
					</label>
					<label class="block text-sm">
						<span class="mb-1 block font-medium">{$_('mortgage.form_years')}</span>
						<input
							type="number"
							min={1}
							class="w-full rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
							placeholder={$_('mortgage.form_years_placeholder')}
							bind:value={form.amortization_years}
						/>
					</label>
					<label class="block text-sm">
						<span class="mb-1 block font-medium">{$_('mortgage.form_term')}</span>
						<input
							type="number"
							min={0}
							max={40}
							step={1}
							class="w-full rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
							placeholder={$_('mortgage.form_term_placeholder')}
							bind:value={form.term_years}
						/>
					</label>
					<label class="block text-sm">
						<span class="mb-1 block font-medium">{$_('mortgage.form_rate_type')}</span>
						<select
							class="w-full rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
							bind:value={form.rate_type}
						>
							<option value="fixed">{$_('mortgage.rate_fixed')}</option>
							<option value="variable">{$_('mortgage.rate_variable')}</option>
						</select>
					</label>
					<label class="block text-sm">
						<span class="mb-1 block font-medium">{$_('mortgage.form_payment_freq')}</span>
						<select
							class="w-full rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
							bind:value={form.payment_frequency}
						>
							<option value="monthly">{$_('mortgage.freq_monthly')}</option>
							<option value="semi_monthly">{$_('mortgage.freq_semi_monthly')}</option>
							<option value="biweekly">{$_('mortgage.freq_biweekly')}</option>
						</select>
					</label>
					<label class="block text-sm">
						<span class="mb-1 block font-medium">{$_('mortgage.form_extra_amount')}</span>
						<input
							type="text"
							inputmode="decimal"
							class="w-full rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
							bind:value={form.extra_payment_amount}
						/>
					</label>
					<label class="block text-sm">
						<span class="mb-1 block font-medium">{$_('mortgage.form_extra_freq')}</span>
						<select
							class="w-full rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
							bind:value={form.extra_payment_frequency}
						>
							<option value="monthly">{$_('mortgage.freq_monthly')}</option>
							<option value="quarterly">{$_('mortgage.freq_quarterly')}</option>
							<option value="yearly">{$_('mortgage.freq_yearly')}</option>
						</select>
					</label>
					<label class="block text-sm">
						<span class="mb-1 block font-medium">{$_('mortgage.form_extra_type')}</span>
						<select
							class="w-full rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
							bind:value={form.extra_prepayment_type}
						>
							<option value="term">{$_('mortgage.type_term')}</option>
							<option value="principal">{$_('mortgage.type_principal')}</option>
						</select>
					</label>
					<div class="sm:col-span-2">
						<button
							type="submit"
							class="text-sm font-medium text-zinc-600 dark:text-zinc-400 underline hover:text-zinc-900 dark:hover:text-zinc-100 disabled:text-zinc-300 dark:disabled:text-zinc-600 disabled:no-underline"
							disabled={creating}
						>
							{creating ? $_('mortgage.creating') : $_('mortgage.submit')}
						</button>
					</div>
				</form>
				{#if createError}
					<p class="mt-3 text-sm text-red-600 dark:text-red-400">
						{$_('mortgage.error_label')}: {createError}
					</p>
				{/if}
			</div>
		{:else}
			<!-- The mortgage list: compact selectable rows. -->
			<div class="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
				<p class="text-xs font-medium text-zinc-500 dark:text-zinc-400 uppercase">{$_('mortgage.list_title')}</p>
				<ul class="mt-3 space-y-1">
					{#each mortgages as m (m.id)}
						<li>
							<button
								type="button"
								class="w-full rounded-md px-3 py-2 text-left hover:bg-zinc-50 dark:hover:bg-zinc-800"
								class:bg-zinc-100={m.id === selectedId}
								aria-pressed={m.id === selectedId}
								onclick={() => {
									selectedId = m.id;
								}}
							>
								<span class="block text-sm font-medium">
								{money(m.principal, accountCurrency(m.account_id))}
							</span>
								<span class="block text-xs text-zinc-600 dark:text-zinc-400">
									{m.annual_rate}{$_('mortgage.percent')} {rateTypeLabel(m.rate_type)} / {m.start_date}
								</span>
							</button>
						</li>
					{/each}
				</ul>
			</div>

			<!-- The selected mortgage's detail. -->
			{#if amortizing}
				<p class="text-sm text-zinc-600 dark:text-zinc-400">{$_('mortgage.loading')}</p>
			{:else if amortError}
				<p class="text-sm text-red-600 dark:text-red-400">{$_('mortgage.error_label')}: {amortError}</p>
			{:else if amortization}
				<!-- KPI line: amounts localised to the account currency. A fifth
					 "next renewal" card when the spec has a term. -->
				<div
					class={amortization.next_renewal !== undefined
						? 'grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-5'
						: 'grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4'}
				>
					<div class="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
						<p class="text-xs font-medium text-zinc-500 dark:text-zinc-400 uppercase">
							{$_('mortgage.label_base_payment')}
						</p>
						<p class="mt-1 text-2xl font-semibold text-zinc-900 dark:text-zinc-100">
							{money(amortization.base_payment, selectedCurrency)}
						</p>
					</div>
					<div class="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
						<p class="text-xs font-medium text-zinc-500 dark:text-zinc-400 uppercase">
							{$_('mortgage.label_payoff_date')}
						</p>
						<p class="mt-1 text-2xl font-semibold text-zinc-900 dark:text-zinc-100">
							{amortization.payoff_date}
						</p>
					</div>
					<div class="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
						<!-- A COUNT, not an amount: the number of payments to
							 payoff (25 y semi-monthly = 600) — no currency
							 symbol applies to it. -->
						<p class="text-xs font-medium text-zinc-500 dark:text-zinc-400 uppercase">
							{$_('mortgage.label_payment_count')}
						</p>
						<p class="mt-1 text-2xl font-semibold text-zinc-900 dark:text-zinc-100">
							{amortization.payoff_index}
						</p>
					</div>
					<div class="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
						<p class="text-xs font-medium text-zinc-500 dark:text-zinc-400 uppercase">
							{$_('mortgage.label_total_interest')}
						</p>
						<p class="mt-1 text-2xl font-semibold text-zinc-900 dark:text-zinc-100">
							{money(amortization.total_interest, selectedCurrency)}
						</p>
					</div>
					{#if amortization.next_renewal !== undefined}
						<div class="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
							<p class="text-xs font-medium text-zinc-500 dark:text-zinc-400 uppercase">
								{$_('mortgage.label_next_renewal')}
							</p>
							<p class="mt-1 text-2xl font-semibold text-zinc-900 dark:text-zinc-100">
								{amortization.next_renewal}
							</p>
						</div>
					{/if}
				</div>

				<!-- Balance vs equity over the schedule (svg-polyline idiom). -->
				<div class="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
					<p class="text-xs font-medium text-zinc-500 dark:text-zinc-400 uppercase">{$_('mortgage.chart_title')}</p>
					{#if equitySeries}
						<svg
							viewBox="0 0 100 40"
							preserveAspectRatio="none"
							class="mt-2 h-28 w-full"
							role="img"
							aria-label={$_('mortgage.chart_aria')}
						>
							<polyline
								points={equitySeries.balance}
								fill="none"
								stroke="currentColor"
								stroke-width="1.5"
								vector-effect="non-scaling-stroke"
								class="text-zinc-400"
							/>
							<polyline
								points={equitySeries.equity}
								fill="none"
								stroke="currentColor"
								stroke-width="1.5"
								vector-effect="non-scaling-stroke"
								class="text-green-600 dark:text-green-400"
							/>
						</svg>
						<div class="mt-2 flex items-center gap-4 text-xs text-zinc-600 dark:text-zinc-400">
							<span class="flex items-center gap-1.5">
								<span class="inline-block h-0.5 w-4 bg-zinc-400 dark:bg-zinc-600"></span>{$_(
									'mortgage.chart_legend_balance'
								)}
							</span>
							<span class="flex items-center gap-1.5">
								<span class="inline-block h-0.5 w-4 bg-green-600"></span>{$_(
									'mortgage.chart_legend_equity'
								)}
							</span>
						</div>
					{:else}
						<p class="mt-2 text-sm text-zinc-500 dark:text-zinc-400">{$_('mortgage.empty')}</p>
					{/if}
				</div>

				<!-- Yearly-collapsed schedule table. -->
				<div class="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
					<p class="text-xs font-medium text-zinc-500 dark:text-zinc-400 uppercase">{$_('mortgage.table_title')}</p>
					<p class="mt-1 text-xs text-zinc-500 dark:text-zinc-400">{$_('mortgage.table_hint')}</p>
					<table class="mt-3 w-full text-sm tabular-nums">
						<thead>
							<tr class="border-b border-zinc-200 dark:border-zinc-800 text-left text-xs text-zinc-500 dark:text-zinc-400 uppercase">
								<th class="py-1.5 pr-3 font-medium">{$_('mortgage.col_year')}</th>
								<th class="py-1.5 pr-3 text-right font-medium">
									{$_('mortgage.col_payment')}
								</th>
								<th class="py-1.5 pr-3 text-right font-medium">
									{$_('mortgage.col_interest')}
								</th>
								<th class="py-1.5 pr-3 text-right font-medium">
									{$_('mortgage.col_principal')}
								</th>
								<th class="py-1.5 pr-3 text-right font-medium">{$_('mortgage.col_extra')}</th>
								<th class="py-1.5 text-right font-medium">{$_('mortgage.col_balance')}</th>
							</tr>
						</thead>
						<tbody>
							{#each yearRows as row (row.year)}
								<tr class="border-b border-zinc-100 dark:border-zinc-800">
									<td class="py-1.5 pr-3">
										<button
											type="button"
											class="font-medium underline decoration-zinc-300 underline-offset-2 hover:text-zinc-900 dark:hover:text-zinc-100"
											aria-expanded={expandedYears.has(row.year)}
											onclick={() => toggleYear(row.year)}
										>
											{row.year}
										</button>
									</td>
									<td class="py-1.5 pr-3 text-right"></td>
									<td class="py-1.5 pr-3 text-right">{moneyNum(row.interest / 100, selectedCurrency)}</td>
									<td class="py-1.5 pr-3 text-right">{moneyNum(row.principal / 100, selectedCurrency)}</td>
									<td class="py-1.5 pr-3 text-right"></td>
									<td class="py-1.5 text-right">{money(row.endBalance, selectedCurrency)}</td>
								</tr>
								{#if expandedYears.has(row.year)}
									{#each row.months as p (p.date)}
										<tr class="bg-zinc-50 dark:bg-zinc-950 text-xs">
											<td class="py-1 pr-3 pl-5 text-zinc-600 dark:text-zinc-400">{p.date}</td>
											<td class="py-1 pr-3 text-right">{money(p.payment, selectedCurrency)}</td>
											<td class="py-1 pr-3 text-right">{money(p.interest, selectedCurrency)}</td>
											<td class="py-1 pr-3 text-right">{money(p.principal, selectedCurrency)}</td>
											<td class="py-1 pr-3 text-right">{money(p.extra, selectedCurrency)}</td>
											<td class="py-1 text-right">{money(p.balance, selectedCurrency)}</td>
										</tr>
									{/each}
								{/if}
							{/each}
						</tbody>
					</table>
				</div>

				<!-- Rate changes: stored events, edited as a full array. -->
				{#if detailMortgage}
					<div class="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
						<p class="text-xs font-medium text-zinc-500 dark:text-zinc-400 uppercase">{$_('mortgage.events_title')}</p>
						<p class="mt-1 text-xs text-zinc-500 dark:text-zinc-400">{$_('mortgage.events_hint')}</p>
						{#if detailMortgage.rate_events.length === 0}
							<p class="mt-3 text-sm text-zinc-500 dark:text-zinc-400">{$_('mortgage.events_empty')}</p>
						{:else}
							<ul class="mt-3 divide-y divide-zinc-100 dark:divide-zinc-800">
								{#each detailMortgage.rate_events as ev, i (ev.effective_date)}
									<li class="flex flex-wrap items-center gap-3 py-2 text-sm">
										<span class="w-24 font-medium">{ev.effective_date}</span>
										<span>{ev.annual_rate}{$_('mortgage.percent')} {rateTypeLabel(ev.rate_type)}</span>
										<span class="text-zinc-500 dark:text-zinc-400">
											{ev.action === 're_amortize'
												? $_('mortgage.event_action_re_amortize')
												: $_('mortgage.event_action_keep_payment')}
										</span>
										<button
											type="button"
											class="ml-auto text-xs font-medium text-zinc-600 dark:text-zinc-400 underline decoration-zinc-300 underline-offset-2 hover:text-red-600 dark:hover:text-red-400 disabled:text-zinc-300 dark:disabled:text-zinc-600 disabled:no-underline"
											disabled={evBusy}
											onclick={() => removeRateEvent(i)}
										>
											{$_('mortgage.event_remove')}
										</button>
									</li>
								{/each}
							</ul>
						{/if}
						<form
							class="mt-4 grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-5"
							onsubmit={(e) => {
								e.preventDefault();
								void addRateEvent();
							}}
						>
							<label class="block text-sm">
								<span class="mb-1 block font-medium">{$_('mortgage.event_date')}</span>
								<input
									type="date"
									class="w-full rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
									bind:value={evDate}
								/>
							</label>
							<label class="block text-sm">
								<span class="mb-1 block font-medium">{$_('mortgage.event_rate')}</span>
								<input
									type="text"
									inputmode="decimal"
									class="w-full rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
									placeholder={$_('mortgage.form_rate_placeholder')}
									bind:value={evRate}
								/>
							</label>
							<label class="block text-sm">
								<span class="mb-1 block font-medium">{$_('mortgage.event_rate_type')}</span>
								<select
									class="w-full rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
									bind:value={evRateType}
								>
									<option value="fixed">{$_('mortgage.rate_fixed')}</option>
									<option value="variable">{$_('mortgage.rate_variable')}</option>
								</select>
							</label>
							<label class="block text-sm">
								<span class="mb-1 block font-medium">{$_('mortgage.event_action')}</span>
								<select
									class="w-full rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
									bind:value={evAction}
								>
									<option value="re_amortize">{$_('mortgage.event_action_re_amortize')}</option>
									<option value="keep_payment">{$_('mortgage.event_action_keep_payment')}</option>
								</select>
							</label>
							<div class="flex items-end">
								<button
									type="submit"
									class="text-sm font-medium text-zinc-600 dark:text-zinc-400 underline hover:text-zinc-900 dark:hover:text-zinc-100 disabled:text-zinc-300 dark:disabled:text-zinc-600 disabled:no-underline"
									disabled={evBusy || evDate === ''}
								>
									{evBusy ? $_('mortgage.event_adding') : $_('mortgage.event_add')}
								</button>
							</div>
						</form>
						{#if evError}
							<p class="mt-3 text-sm text-red-600 dark:text-red-400">
								{$_('mortgage.error_label')}: {evError}
							</p>
						{/if}
					</div>
				{/if}

				<!-- Transaction matching: the linked account's debits
				 replayed against the stored schedule (round 25,
				 read-only). -->
				{#if detailMortgage}
					<div class="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
						<p class="text-xs font-medium text-zinc-500 dark:text-zinc-400 uppercase">{$_('mortgage.match_title')}</p>
						<p class="mt-1 text-xs text-zinc-500 dark:text-zinc-400">{$_('mortgage.match_hint')}</p>
						{#if matchingLoading}
							<p class="mt-3 text-sm text-zinc-500 dark:text-zinc-400">{$_('mortgage.loading')}</p>
						{:else if matchingError !== null}
							<p class="mt-3 text-sm text-red-600 dark:text-red-400">
								{$_('mortgage.error_label')}: {matchingError}
							</p>
						{:else if matching !== null}
							<dl class="mt-3 grid grid-cols-2 gap-3 text-sm sm:grid-cols-3 lg:grid-cols-6">
								<div>
									<dt class="text-xs text-zinc-500 dark:text-zinc-400">{$_('mortgage.match_matched')}</dt>
									<dd class="mt-0.5 font-medium tabular-nums">{matching.periods_matched}</dd>
								</div>
								<div>
									<dt class="text-xs text-zinc-500 dark:text-zinc-400">{$_('mortgage.match_missing')}</dt>
									<dd class="mt-0.5 font-medium tabular-nums">{matching.periods_missing}</dd>
								</div>
								<div>
									<dt class="text-xs text-zinc-500 dark:text-zinc-400">{$_('mortgage.match_last')}</dt>
									<dd class="mt-0.5 font-medium tabular-nums">{matching.last_matched_date ?? '—'}</dd>
								</div>
								<div>
									<dt class="text-xs text-zinc-500 dark:text-zinc-400">{$_('mortgage.label_payoff_date')}</dt>
									<dd class="mt-0.5 font-medium tabular-nums">{matching.actual_payoff_date ?? '—'}</dd>
								</div>
								<div>
									<dt class="text-xs text-zinc-500 dark:text-zinc-400">{$_('mortgage.match_actual_balance')}</dt>
									<dd class="mt-0.5 font-medium tabular-nums">{money(matching.actual_balance, selectedCurrency)}</dd>
								</div>
								<div>
									<dt class="text-xs text-zinc-500 dark:text-zinc-400">{$_('mortgage.match_planned_balance')}</dt>
									<dd class="mt-0.5 font-medium tabular-nums">{money(matching.planned_balance, selectedCurrency)}</dd>
								</div>
							</dl>
							{#if matching.flags.length > 0}
								<ul class="mt-3 space-y-1">
									{#each matching.flags as f (f)}
										<li class="text-sm text-amber-600 dark:text-amber-400">{matchingFlagLabel(f)}</li>
									{/each}
								</ul>
							{/if}
							<p class="mt-4 text-xs font-medium text-zinc-500 dark:text-zinc-400 uppercase">{$_('mortgage.match_suggestions')}</p>
							{#if matching.suggestions.length === 0}
								<p class="mt-1 text-sm text-zinc-500 dark:text-zinc-400">{$_('mortgage.match_suggestions_empty')}</p>
							{:else}
								<ul class="mt-1 divide-y divide-zinc-100 dark:divide-zinc-800">
									{#each matching.suggestions as s (s.effective_date + s.implied_annual_rate)}
										<li class="flex flex-wrap items-center gap-3 py-2 text-sm">
											<span class="w-24 font-medium tabular-nums">{s.effective_date}</span>
											<span class="font-semibold tabular-nums">
												{s.implied_annual_rate}{$_('mortgage.percent')}
											</span>
											<span class="text-zinc-500 dark:text-zinc-400 tabular-nums">
												{$_('mortgage.match_suggestion_observed')} {money(s.observed_payment, selectedCurrency)} · {s.observed_periods}
												{$_('mortgage.match_suggestion_periods')}
											</span>
											<button
												type="button"
												class="ml-auto text-xs font-medium text-zinc-600 dark:text-zinc-400 underline decoration-zinc-300 underline-offset-2 hover:text-zinc-900 dark:hover:text-zinc-100 disabled:text-zinc-300 dark:disabled:text-zinc-600 disabled:no-underline"
												disabled={evBusy}
												onclick={() => void applySuggestion(s)}
											>
												{evBusy ? $_('mortgage.match_suggestion_applying') : $_('mortgage.match_suggestion_apply')}
											</button>
										</li>
									{/each}
								</ul>
							{/if}
							<div class="mt-4 grid grid-cols-1 gap-4 sm:grid-cols-2">
								<div>
									<p class="text-xs font-medium text-zinc-500 dark:text-zinc-400 uppercase">{$_('mortgage.match_extras')}</p>
									{#if matching.extras.length === 0}
										<p class="mt-1 text-sm text-zinc-500 dark:text-zinc-400">{$_('mortgage.match_none')}</p>
									{:else}
										<ul class="mt-1 divide-y divide-zinc-100 dark:divide-zinc-800">
											{#each matching.extras as a, i (i)}
												<li class="flex justify-between py-1.5 text-sm tabular-nums">
													<span>{a.date}</span>
													<span class="font-medium text-emerald-600 dark:text-emerald-400">+{money(a.amount, selectedCurrency)}</span>
												</li>
											{/each}
										</ul>
									{/if}
								</div>
								<div>
									<p class="text-xs font-medium text-zinc-500 dark:text-zinc-400 uppercase">{$_('mortgage.match_shorts')}</p>
									{#if matching.shorts.length === 0}
										<p class="mt-1 text-sm text-zinc-500 dark:text-zinc-400">{$_('mortgage.match_none')}</p>
									{:else}
										<ul class="mt-1 divide-y divide-zinc-100 dark:divide-zinc-800">
											{#each matching.shorts as a, i (i)}
												<li class="flex justify-between py-1.5 text-sm tabular-nums">
													<span>{a.date}</span>
													<span class="font-medium text-red-600 dark:text-red-400">−{money(a.amount, selectedCurrency)}</span>
												</li>
											{/each}
										</ul>
									{/if}
								</div>
							</div>
							{#if matching.periods.length > 0}
								<table class="mt-4 w-full text-sm tabular-nums">
									<thead>
										<tr class="border-b border-zinc-200 dark:border-zinc-800 text-left text-xs text-zinc-500 dark:text-zinc-400 uppercase">
											<th class="py-1.5 pr-3 font-medium">{$_('mortgage.match_col_date')}</th>
											<th class="py-1.5 pr-3 text-right font-medium">{$_('mortgage.col_payment')}</th>
											<th class="py-1.5 pr-3 text-right font-medium">{$_('mortgage.col_extra')}</th>
											<th class="py-1.5 pr-3 text-right font-medium">{$_('mortgage.match_col_short')}</th>
											<th class="py-1.5 pr-3 text-right font-medium">{$_('mortgage.col_balance')}</th>
											<th class="py-1.5 text-right font-medium">{$_('mortgage.match_col_planned_balance')}</th>
										</tr>
									</thead>
									<tbody>
										{#each matching.periods as p (p.date)}
											<tr class="border-b border-zinc-100 dark:border-zinc-800">
												<td class="py-1.5 pr-3">{p.date}</td>
												<td class="py-1.5 pr-3 text-right">{money(p.payment, selectedCurrency) || '—'}</td>
												<td class="py-1.5 pr-3 text-right">{money(p.extra, selectedCurrency)}</td>
												<td class="py-1.5 pr-3 text-right">{money(p.short, selectedCurrency)}</td>
												<td class="py-1.5 pr-3 text-right">{money(p.balance, selectedCurrency)}</td>
												<td class="py-1.5 text-right">{money(p.planned_balance, selectedCurrency)}</td>
											</tr>
										{/each}
									</tbody>
								</table>
							{/if}
						{/if}
					</div>
				{/if}

				<!-- What-if panel: sliders over the stored spec. -->
				<div class="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
					<p class="text-xs font-medium text-zinc-500 dark:text-zinc-400 uppercase">{$_('mortgage.whatif_title')}</p>
					<div class="mt-4 grid grid-cols-1 gap-4 sm:grid-cols-2">
						<label class="block text-sm">
							<span class="mb-1 block font-medium">
								{$_('mortgage.whatif_rate')} · {wiRate.toFixed(2)}{$_('mortgage.percent')}
							</span>
							<input
								type="range"
								min={0.5}
								max={15}
								step={0.01}
								bind:value={wiRate}
								class="w-full"
							/>
						</label>
						<label class="block text-sm">
							<span class="mb-1 block font-medium">
								{$_('mortgage.whatif_extra')} · {moneyNum(wiExtra, selectedCurrency)}
							</span>
							<input
								type="range"
								min={0}
								max={5000}
								step={100}
								bind:value={wiExtra}
								class="w-full"
							/>
						</label>
						<label class="block text-sm">
							<span class="mb-1 block font-medium">{$_('mortgage.whatif_freq')}</span>
							<select
								class="w-full rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
								bind:value={wiFreq}
							>
								<option value="monthly">{$_('mortgage.freq_monthly')}</option>
								<option value="quarterly">{$_('mortgage.freq_quarterly')}</option>
								<option value="yearly">{$_('mortgage.freq_yearly')}</option>
							</select>
						</label>
						<label class="block text-sm">
							<span class="mb-1 block font-medium">
								{$_('mortgage.whatif_one_time')} · {moneyNum(wiOneTime, selectedCurrency)}
							</span>
							<input
								type="range"
								min={0}
								max={25000}
								step={500}
								bind:value={wiOneTime}
								class="w-full"
							/>
						</label>
						<label class="block text-sm">
							<span class="mb-1 block font-medium">{$_('mortgage.whatif_date')}</span>
							<input
								type="date"
								class="w-full rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
								bind:value={wiDate}
							/>
						</label>
						<label class="block text-sm">
							<span class="mb-1 block font-medium">{$_('mortgage.whatif_pay_freq')}</span>
							<select
								class="w-full rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
								bind:value={wiPayFreq}
							>
								<option value="monthly">{$_('mortgage.freq_monthly')}</option>
								<option value="semi_monthly">{$_('mortgage.freq_semi_monthly')}</option>
								<option value="biweekly">{$_('mortgage.freq_biweekly')}</option>
							</select>
						</label>
						<label class="block text-sm">
							<span class="mb-1 block font-medium">{$_('mortgage.whatif_renewal_date')}</span>
							<input
								type="date"
								class="w-full rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
								bind:value={wiRenewDate}
							/>
						</label>
						<label class="block text-sm">
							<span class="mb-1 block font-medium">{$_('mortgage.whatif_renewal_rate')}</span>
							<input
								type="text"
								inputmode="decimal"
								class="w-full rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
								placeholder={$_('mortgage.form_rate_placeholder')}
								bind:value={wiRenewRate}
							/>
						</label>
						<label class="block text-sm">
							<span class="mb-1 block font-medium">{$_('mortgage.whatif_renewal_type')}</span>
							<select
								class="w-full rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
								bind:value={wiRenewType}
							>
								<option value="fixed">{$_('mortgage.rate_fixed')}</option>
								<option value="variable">{$_('mortgage.rate_variable')}</option>
							</select>
						</label>
					</div>
					<div class="mt-4 flex items-center gap-4">
						<button
							type="button"
							class="text-sm font-medium text-zinc-600 dark:text-zinc-400 underline hover:text-zinc-900 dark:hover:text-zinc-100 disabled:text-zinc-300 dark:disabled:text-zinc-600 disabled:no-underline"
							disabled={comparing}
							onclick={() => void compare()}
						>
							{comparing ? $_('mortgage.comparing') : $_('mortgage.compare')}
						</button>
						<button
							type="button"
							class="text-sm font-medium text-zinc-600 dark:text-zinc-400 underline hover:text-zinc-900 dark:hover:text-zinc-100"
							onclick={() => resetWhatIf()}
						>
							{$_('mortgage.reset')}
						</button>
					</div>
					{#if compareError}
						<p class="mt-3 text-sm text-red-600 dark:text-red-400">{$_('mortgage.error_label')}: {compareError}</p>
					{/if}
				</div>

				<!-- Base-vs-scenario cards after a compare. -->
				{#if whatIf}
					<div class="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
						<p class="text-xs font-medium text-zinc-500 dark:text-zinc-400 uppercase">
							{$_('mortgage.compare_title')}
						</p>
						<table class="mt-3 w-full text-sm tabular-nums">
							<thead>
								<tr class="border-b border-zinc-200 dark:border-zinc-800 text-left text-xs text-zinc-500 dark:text-zinc-400 uppercase">
									<th class="py-1.5 pr-3 font-medium"></th>
									<th class="py-1.5 pr-3 text-right font-medium">{$_('mortgage.base')}</th>
									<th class="py-1.5 text-right font-medium">{$_('mortgage.scenario')}</th>
								</tr>
							</thead>
							<tbody>
								<tr class="border-b border-zinc-100 dark:border-zinc-800">
									<td class="py-1.5 pr-3 text-zinc-600 dark:text-zinc-400">{$_('mortgage.cmp_payoff')}</td>
									<td class="py-1.5 pr-3 text-right">{amortization.payoff_date}</td>
									<td class="py-1.5 text-right">{whatIf.payoff_date}</td>
								</tr>
								<tr class="border-b border-zinc-100 dark:border-zinc-800">
									<td class="py-1.5 pr-3 text-zinc-600 dark:text-zinc-400">{$_('mortgage.cmp_interest')}</td>
									<td class="py-1.5 pr-3 text-right">{money(amortization.total_interest, selectedCurrency)}</td>
									<td class="py-1.5 text-right">{money(whatIf.total_interest, selectedCurrency)}</td>
								</tr>
								<tr class="border-b border-zinc-100 dark:border-zinc-800">
									<td class="py-1.5 pr-3 text-zinc-600 dark:text-zinc-400">
										{wiFreqApplied ? $_('mortgage.cmp_periods') : $_('mortgage.cmp_months')}
									</td>
									<td class="py-1.5 pr-3 text-right">0</td>
									<td class="py-1.5 text-right">{whatIf.months_saved}</td>
								</tr>
								{#if amortization.next_renewal !== undefined || whatIf.schedule.next_renewal !== undefined}
									<tr class="border-b border-zinc-100 dark:border-zinc-800">
										<td class="py-1.5 pr-3 text-zinc-600 dark:text-zinc-400">{$_('mortgage.cmp_renewal')}</td>
										<td class="py-1.5 pr-3 text-right">{amortization.next_renewal ?? '—'}</td>
										<td class="py-1.5 text-right">{whatIf.schedule.next_renewal ?? '—'}</td>
									</tr>
								{/if}
								<tr>
									<td class="py-1.5 pr-3 text-zinc-600 dark:text-zinc-400">
										{$_('mortgage.cmp_interest_saved')}
									</td>
									<td class="py-1.5 pr-3 text-right">{moneyNum(0, selectedCurrency)}</td>
									<td class="py-1.5 text-right {savedClass(whatIf.interest_saved)}">
										{money(whatIf.interest_saved, selectedCurrency)}
									</td>
								</tr>
							</tbody>
						</table>
					</div>
				{/if}
			{/if}
		{/if}
	{/if}
</section>

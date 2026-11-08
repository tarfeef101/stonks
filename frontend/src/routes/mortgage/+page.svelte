<script lang="ts">
	import { resolve } from '$app/paths';
	import { onMount } from 'svelte';
	import { SvelteMap, SvelteSet } from 'svelte/reactivity';
	import { _ } from 'svelte-i18n';
	import { user } from '#lib/auth/client';
	import { ApiError } from '#lib/api/client';
	import { listAccounts, type Account } from '#lib/api/accounts';
	import {
		listMortgages,
		createMortgage,
		getMortgageAmortization,
		whatIfMortgageAmortization,
		type AmortizationPayment,
		type AmortizationSchedule,
		type Mortgage,
		type WhatIfRequest,
		type WhatIfResult
	} from '#lib/api/mortgages';

	// Mortgage page (step 7.3): the stored mortgage spec — KPI line,
	// balance/equity chart, yearly-collapsed schedule table — plus the
	// what-if comparison panel over the 7.2 engine.
	//
	// Rendered-numbers contract: every displayed amount is the raw API
	// string (canonical 2-decimal) bound straight into the DOM. Numeric
	// parsing on this page is limited to chart coordinates, year-row
	// totals and slider values (geometry) — never for displayed text.

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

	// --- Amortization fetch: one per selected mortgage ------------------
	let amortization = $state<AmortizationSchedule | null>(null);
	let amortizing = $state(false);
	let amortError = $state<string | null>(null);
	let amortSeq = 0;

	$effect(() => {
		const sub = $user?.profile.sub ?? null;
		const id = selectedId;
		const seq = ++amortSeq;
		if (sub === null || id === '') {
			amortization = null;
			amortError = null;
			amortizing = false;
			return;
		}
		amortizing = true;
		amortization = null;
		amortError = null;
		resetWhatIf();
		getMortgageAmortization(id)
			.then((s) => {
				if (seq !== amortSeq) return;
				amortization = s;
			})
			.catch((e) => {
				if (seq !== amortSeq) return;
				amortError = envelopeMessage(e);
			})
			.finally(() => {
				if (seq !== amortSeq) return;
				amortizing = false;
			});
	});

	// --- What-if state ----------------------------------------------------
	let wiRate = $state(0.5);
	let wiExtra = $state(0);
	let wiFreq = $state('monthly');
	let wiOneTime = $state(0);
	let wiDate = $state('');
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
		whatIf = null;
		compareError = null;
		expandedYears.clear();
		if (m === null) return;
		wiRate = clampRate(m.annual_rate);
		wiExtra = toCents(m.extra_payment_amount) / 100;
		wiFreq = m.extra_payment_frequency;
	}

	function clampRate(s: string): number {
		const v = parseFloat(s);
		if (!Number.isFinite(v)) return 0.5;
		return Math.min(15, Math.max(0.5, v));
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
		try {
			const r = await whatIfMortgageAmortization(m.id, body);
			if (seq !== compareSeq) return;
			whatIf = r;
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

	// --- Add form (the 9 create fields) ----------------------------------
	let form = $state({
		account_id: '',
		principal: '',
		annual_rate: '',
		amortization_years: 30,
		start_date: '',
		payment_frequency: 'monthly',
		extra_payment_amount: '0.00',
		extra_payment_frequency: 'monthly',
		extra_prepayment_type: 'term'
	});
	let creating = $state(false);
	let createError = $state<string | null>(null);

	/** Create the mortgage, refresh the list and select the new row;
	 * a server 400 message is shown verbatim on error. */
	async function addMortgage(): Promise<void> {
		if (creating) return;
		creating = true;
		createError = null;
		try {
			const m = await createMortgage(form);
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

	// --- Chart + table geometry -------------------------------------------
	const EQUITY_W = 100;
	const EQUITY_H = 40;

	/** The balance/equity polylines scaled into the fixed viewBox, or
	 * null when the schedule is too short or the stored principal is
	 * not a positive amount (pure coordinate math). */
	const equitySeries = $derived.by(() => {
		const m = selectedMortgage();
		const s = amortization;
		if (m === null || s === null) return null;
		const pc = toCents(m.principal);
		const n = s.payments.length;
		if (n < 2 || pc <= 0) return null;
		const balance: string[] = [];
		const equity: string[] = [];
		for (let i = 0; i < n; i++) {
			const bc = toCents(s.payments[i].balance);
			const x = Math.round((i / (n - 1)) * EQUITY_W);
			const balanceY = Math.round(EQUITY_H * (1 - bc / pc));
			const equityY = Math.round((EQUITY_H * bc) / pc);
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
		if (!Number.isFinite(n) || n === 0) return 'text-zinc-900';
		return n > 0 ? 'text-green-700' : 'text-red-600';
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

	/** A canonical 2-decimal amount string as integer cents. GEOMETRY
	 * ONLY (chart coordinates, year-row totals, slider seeds) — never
	 * for displayed text, which always renders the raw API string. */
	function toCents(s: string): number {
		const m = /^(-?)(\d+)(?:\.(\d{1,2}))?$/.exec(s);
		if (!m) return 0;
		const cents = (m[3] ?? '').padEnd(2, '0');
		const v = parseInt(m[2], 10) * 100 + parseInt(cents, 10);
		return m[1] === '-' ? -v : v;
	}
</script>

<section class="space-y-4">
	<h1 class="text-2xl font-semibold">{$_('mortgage.heading')}</h1>

	{#if !$user}
		<p class="text-sm text-zinc-600">
			{$_('mortgage.sign_in_prompt')}
			<a class="underline" href={resolve('/')}>{$_('auth.sign_in')}</a>
		</p>
	{:else if !listLoaded}
		<p class="text-sm text-zinc-600">{$_('mortgage.loading')}</p>
	{:else if mortgageAccounts.length === 0}
		<p class="text-zinc-600">{$_('mortgage.no_mortgage')}</p>
	{:else}
		{#if listError}
			<p class="text-sm text-red-600">{listError}</p>
		{/if}

		{#if mortgages.length === 0}
			<!-- No mortgage on file: the 9-field create form. -->
			<div class="rounded-lg border border-zinc-200 bg-white p-5">
				<p class="text-xs font-medium text-zinc-500 uppercase">{$_('mortgage.add_title')}</p>
				<p class="mt-1 text-sm text-zinc-600">{$_('mortgage.add_note')}</p>
				<form
					class="mt-4 grid grid-cols-1 gap-4 sm:grid-cols-2"
					onsubmit={(e) => {
						e.preventDefault();
						void addMortgage();
					}}
				>
					<label class="block text-sm">
						<span class="mb-1 block font-medium">{$_('mortgage.form_account')}</span>
						<select
							class="w-full rounded-md border border-zinc-300 px-3 py-1.5"
							bind:value={form.account_id}
						>
							<option value="" disabled>{$_('mortgage.form_account_placeholder')}</option>
							{#each mortgageAccounts as a (a.id)}
								<option value={a.id}>{a.name}</option>
							{/each}
						</select>
					</label>
					<label class="block text-sm">
						<span class="mb-1 block font-medium">{$_('mortgage.form_start')}</span>
						<input
							type="date"
							class="w-full rounded-md border border-zinc-300 px-3 py-1.5"
							bind:value={form.start_date}
						/>
					</label>
					<label class="block text-sm">
						<span class="mb-1 block font-medium">{$_('mortgage.form_principal')}</span>
						<input
							type="text"
							inputmode="decimal"
							class="w-full rounded-md border border-zinc-300 px-3 py-1.5"
							placeholder={$_('mortgage.form_principal_placeholder')}
							bind:value={form.principal}
						/>
					</label>
					<label class="block text-sm">
						<span class="mb-1 block font-medium">{$_('mortgage.form_rate')}</span>
						<input
							type="text"
							inputmode="decimal"
							class="w-full rounded-md border border-zinc-300 px-3 py-1.5"
							placeholder={$_('mortgage.form_rate_placeholder')}
							bind:value={form.annual_rate}
						/>
					</label>
					<label class="block text-sm">
						<span class="mb-1 block font-medium">{$_('mortgage.form_years')}</span>
						<input
							type="number"
							min={1}
							class="w-full rounded-md border border-zinc-300 px-3 py-1.5"
							placeholder={$_('mortgage.form_years_placeholder')}
							bind:value={form.amortization_years}
						/>
					</label>
					<label class="block text-sm">
						<span class="mb-1 block font-medium">{$_('mortgage.form_payment_freq')}</span>
						<select
							class="w-full rounded-md border border-zinc-300 px-3 py-1.5"
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
							class="w-full rounded-md border border-zinc-300 px-3 py-1.5"
							bind:value={form.extra_payment_amount}
						/>
					</label>
					<label class="block text-sm">
						<span class="mb-1 block font-medium">{$_('mortgage.form_extra_freq')}</span>
						<select
							class="w-full rounded-md border border-zinc-300 px-3 py-1.5"
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
							class="w-full rounded-md border border-zinc-300 px-3 py-1.5"
							bind:value={form.extra_prepayment_type}
						>
							<option value="term">{$_('mortgage.type_term')}</option>
							<option value="principal">{$_('mortgage.type_principal')}</option>
						</select>
					</label>
					<div class="sm:col-span-2">
						<button
							type="submit"
							class="text-sm font-medium text-zinc-600 underline hover:text-zinc-900 disabled:text-zinc-300 disabled:no-underline"
							disabled={creating}
						>
							{creating ? $_('mortgage.creating') : $_('mortgage.submit')}
						</button>
					</div>
				</form>
				{#if createError}
					<p class="mt-3 text-sm text-red-600">
						{$_('mortgage.error_label')}: {createError}
					</p>
				{/if}
			</div>
		{:else}
			<!-- The mortgage list: compact selectable rows. -->
			<div class="rounded-lg border border-zinc-200 bg-white p-5">
				<p class="text-xs font-medium text-zinc-500 uppercase">{$_('mortgage.list_title')}</p>
				<ul class="mt-3 space-y-1">
					{#each mortgages as m (m.id)}
						<li>
							<button
								type="button"
								class="w-full rounded-md px-3 py-2 text-left hover:bg-zinc-50"
								class:bg-zinc-100={m.id === selectedId}
								aria-pressed={m.id === selectedId}
								onclick={() => {
									selectedId = m.id;
								}}
							>
								<span class="block text-sm font-medium">{m.principal}</span>
								<span class="block text-xs text-zinc-600">
									{m.annual_rate}{$_('mortgage.percent')} / {m.start_date}
								</span>
							</button>
						</li>
					{/each}
				</ul>
			</div>

			<!-- The selected mortgage's detail. -->
			{#if amortizing}
				<p class="text-sm text-zinc-600">{$_('mortgage.loading')}</p>
			{:else if amortError}
				<p class="text-sm text-red-600">{$_('mortgage.error_label')}: {amortError}</p>
			{:else if amortization}
				<!-- KPI line: the raw API strings, verbatim. -->
				<div class="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4">
					<div class="rounded-lg border border-zinc-200 bg-white p-5">
						<p class="text-xs font-medium text-zinc-500 uppercase">
							{$_('mortgage.label_base_payment')}
						</p>
						<p class="mt-1 text-2xl font-semibold text-zinc-900">
							{amortization.base_payment}
						</p>
					</div>
					<div class="rounded-lg border border-zinc-200 bg-white p-5">
						<p class="text-xs font-medium text-zinc-500 uppercase">
							{$_('mortgage.label_payoff_date')}
						</p>
						<p class="mt-1 text-2xl font-semibold text-zinc-900">
							{amortization.payoff_date}
						</p>
					</div>
					<div class="rounded-lg border border-zinc-200 bg-white p-5">
						<p class="text-xs font-medium text-zinc-500 uppercase">
							{$_('mortgage.label_payments')}
						</p>
						<p class="mt-1 text-2xl font-semibold text-zinc-900">
							{amortization.payoff_index}
						</p>
					</div>
					<div class="rounded-lg border border-zinc-200 bg-white p-5">
						<p class="text-xs font-medium text-zinc-500 uppercase">
							{$_('mortgage.label_total_interest')}
						</p>
						<p class="mt-1 text-2xl font-semibold text-zinc-900">
							{amortization.total_interest}
						</p>
					</div>
				</div>

				<!-- Balance vs equity over the schedule (svg-polyline idiom). -->
				<div class="rounded-lg border border-zinc-200 bg-white p-5">
					<p class="text-xs font-medium text-zinc-500 uppercase">{$_('mortgage.chart_title')}</p>
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
								class="text-green-600"
							/>
						</svg>
						<div class="mt-2 flex items-center gap-4 text-xs text-zinc-600">
							<span class="flex items-center gap-1.5">
								<span class="inline-block h-0.5 w-4 bg-zinc-400"></span>{$_(
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
						<p class="mt-2 text-sm text-zinc-500">{$_('mortgage.empty')}</p>
					{/if}
				</div>

				<!-- Yearly-collapsed schedule table. -->
				<div class="rounded-lg border border-zinc-200 bg-white p-5">
					<p class="text-xs font-medium text-zinc-500 uppercase">{$_('mortgage.table_title')}</p>
					<p class="mt-1 text-xs text-zinc-500">{$_('mortgage.table_hint')}</p>
					<table class="mt-3 w-full text-sm tabular-nums">
						<thead>
							<tr class="border-b border-zinc-200 text-left text-xs text-zinc-500 uppercase">
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
								<tr class="border-b border-zinc-100">
									<td class="py-1.5 pr-3">
										<button
											type="button"
											class="font-medium underline decoration-zinc-300 underline-offset-2 hover:text-zinc-900"
											aria-expanded={expandedYears.has(row.year)}
											onclick={() => toggleYear(row.year)}
										>
											{row.year}
										</button>
									</td>
									<td class="py-1.5 pr-3 text-right"></td>
									<td class="py-1.5 pr-3 text-right">{(row.interest / 100).toFixed(2)}</td>
									<td class="py-1.5 pr-3 text-right">{(row.principal / 100).toFixed(2)}</td>
									<td class="py-1.5 pr-3 text-right"></td>
									<td class="py-1.5 text-right">{row.endBalance}</td>
								</tr>
								{#if expandedYears.has(row.year)}
									{#each row.months as p (p.date)}
										<tr class="bg-zinc-50 text-xs">
											<td class="py-1 pr-3 pl-5 text-zinc-600">{p.date}</td>
											<td class="py-1 pr-3 text-right">{p.payment}</td>
											<td class="py-1 pr-3 text-right">{p.interest}</td>
											<td class="py-1 pr-3 text-right">{p.principal}</td>
											<td class="py-1 pr-3 text-right">{p.extra}</td>
											<td class="py-1 text-right">{p.balance}</td>
										</tr>
									{/each}
								{/if}
							{/each}
						</tbody>
					</table>
				</div>

				<!-- What-if panel: sliders over the stored spec. -->
				<div class="rounded-lg border border-zinc-200 bg-white p-5">
					<p class="text-xs font-medium text-zinc-500 uppercase">{$_('mortgage.whatif_title')}</p>
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
								{$_('mortgage.whatif_extra')} · {wiExtra.toFixed(2)}
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
								class="w-full rounded-md border border-zinc-300 px-3 py-1.5"
								bind:value={wiFreq}
							>
								<option value="monthly">{$_('mortgage.freq_monthly')}</option>
								<option value="quarterly">{$_('mortgage.freq_quarterly')}</option>
								<option value="yearly">{$_('mortgage.freq_yearly')}</option>
							</select>
						</label>
						<label class="block text-sm">
							<span class="mb-1 block font-medium">
								{$_('mortgage.whatif_one_time')} · {wiOneTime.toFixed(2)}
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
								class="w-full rounded-md border border-zinc-300 px-3 py-1.5"
								bind:value={wiDate}
							/>
						</label>
					</div>
					<div class="mt-4 flex items-center gap-4">
						<button
							type="button"
							class="text-sm font-medium text-zinc-600 underline hover:text-zinc-900 disabled:text-zinc-300 disabled:no-underline"
							disabled={comparing}
							onclick={() => void compare()}
						>
							{comparing ? $_('mortgage.comparing') : $_('mortgage.compare')}
						</button>
						<button
							type="button"
							class="text-sm font-medium text-zinc-600 underline hover:text-zinc-900"
							onclick={() => resetWhatIf()}
						>
							{$_('mortgage.reset')}
						</button>
					</div>
					{#if compareError}
						<p class="mt-3 text-sm text-red-600">{$_('mortgage.error_label')}: {compareError}</p>
					{/if}
				</div>

				<!-- Base-vs-scenario cards after a compare. -->
				{#if whatIf}
					<div class="rounded-lg border border-zinc-200 bg-white p-5">
						<p class="text-xs font-medium text-zinc-500 uppercase">
							{$_('mortgage.compare_title')}
						</p>
						<table class="mt-3 w-full text-sm tabular-nums">
							<thead>
								<tr class="border-b border-zinc-200 text-left text-xs text-zinc-500 uppercase">
									<th class="py-1.5 pr-3 font-medium"></th>
									<th class="py-1.5 pr-3 text-right font-medium">{$_('mortgage.base')}</th>
									<th class="py-1.5 text-right font-medium">{$_('mortgage.scenario')}</th>
								</tr>
							</thead>
							<tbody>
								<tr class="border-b border-zinc-100">
									<td class="py-1.5 pr-3 text-zinc-600">{$_('mortgage.cmp_payoff')}</td>
									<td class="py-1.5 pr-3 text-right">{amortization.payoff_date}</td>
									<td class="py-1.5 text-right">{whatIf.payoff_date}</td>
								</tr>
								<tr class="border-b border-zinc-100">
									<td class="py-1.5 pr-3 text-zinc-600">{$_('mortgage.cmp_interest')}</td>
									<td class="py-1.5 pr-3 text-right">{amortization.total_interest}</td>
									<td class="py-1.5 text-right">{whatIf.total_interest}</td>
								</tr>
								<tr class="border-b border-zinc-100">
									<td class="py-1.5 pr-3 text-zinc-600">{$_('mortgage.cmp_months')}</td>
									<td class="py-1.5 pr-3 text-right">0</td>
									<td class="py-1.5 text-right">{whatIf.months_saved}</td>
								</tr>
								<tr>
									<td class="py-1.5 pr-3 text-zinc-600">
										{$_('mortgage.cmp_interest_saved')}
									</td>
									<td class="py-1.5 pr-3 text-right">0.00</td>
									<td class="py-1.5 text-right {savedClass(whatIf.interest_saved)}">
										{whatIf.interest_saved}
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

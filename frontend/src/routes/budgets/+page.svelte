<script lang="ts">
	import { page } from '$app/state';
	import { _ } from 'svelte-i18n';
	import { login, user } from '#lib/auth/client';
	import { ApiError } from '#lib/api/client';
	import { listCategories, type Category } from '#lib/api/categories';
	import {
		createBudget,
		deleteBudget,
		listBudgets,
		updateBudget,
		type Budget
	} from '#lib/api/budgets';
	import { getSummary, type Period, type Summary, type TrendGroup } from '#lib/api/summary';

	// Budgets page (step 6.3): the caller's budgets — CRUD, and per
	// budget the current-period adherence (spend vs the budget's OWN
	// targets) plus a trailing 6-window adherence history (a plain
	// table — the plan says "chart/table"; the table is the honest
	// minimal choice, the ECharts maintenance call is 6.4's).
	//
	// Rendered-numbers contract: every displayed API amount STRING (the
	// summary spends) is the raw canonical 2-decimal string bound
	// straight into the DOM — no Number(), parseFloat, toFixed,
	// toLocaleString, or arithmetic on displayed values. Budget targets
	// are JSON NUMBERS on the wire and render via Svelte's default
	// number-to-string (a known minor visual deviation from the
	// server's canonical 2-decimal text — deliberately not "fixed" by
	// formatting). Numeric parsing is limited to the bar-width and
	// over/under helpers (geometry) — never for displayed text.

	// Trailing adherence history: always 6 windows (oldest → newest).
	const TREND_PERIODS = 6;

	let budgets = $state<Budget[]>([]);
	let categories = $state<Category[]>([]);
	let summaries = $state<Partial<Record<Period, Summary>>>({});
	let loading = $state(true);
	let loadError = $state<string | null>(null);

	// New-budget form (always visible): the target inputs hold raw text
	// per category; empty = no target for that category.
	let newName = $state('');
	let newPeriod = $state<Period>('month');
	let newTargets = $state<Record<string, string>>({});
	let creating = $state(false);
	let formError = $state<string | null>(null);

	// Inline edit (one at a time) + per-card action state.
	let editingId = $state<string | null>(null);
	let editName = $state('');
	let editTargets = $state<Record<string, string>>({});
	let saving = $state(false);
	let editError = $state<string | null>(null);
	let busy = $state<Record<string, boolean>>({});
	let cardError = $state<Record<string, string>>({});

	/** Only active categories take targets (the manual-entry picker's
	 * rule). */
	const activeCategories = $derived(categories.filter((c) => c.active));

	// --- Data (per identity) -------------------------------------------
	// One load per identity (and after a create): the budgets, the
	// categories, and ONE summary per DISTINCT budget period (deduped,
	// so budgets sharing a period hit the API once). NO account filter
	// (pinned — the plan is silent; adherence is all-accounts). The
	// top-level groups feed current-period adherence; the trend (6
	// windows) feeds the history table. The fetchSeq guard drops stale
	// responses when a newer load has started (or the identity signed
	// out).
	let fetchSeq = 0;
	async function loadData(): Promise<void> {
		const seq = ++fetchSeq;
		loading = true;
		loadError = null;
		try {
			const [bs, cats] = await Promise.all([listBudgets(), listCategories()]);
			const periods: Period[] = [];
			const loads: Promise<Summary>[] = [];
			for (const b of bs) {
				if (!periods.includes(b.period)) {
					periods.push(b.period);
					loads.push(getSummary(b.period, undefined, TREND_PERIODS));
				}
			}
			const results = await Promise.all(loads);
			if (seq !== fetchSeq) return;
			const sums: Partial<Record<Period, Summary>> = {};
			periods.forEach((p, i) => (sums[p] = results[i]));
			budgets = bs;
			categories = cats;
			summaries = sums;
		} catch (e) {
			if (seq !== fetchSeq) return;
			loadError = envelopeMessage(e);
		}
		// Only the newest load clears the spinner (a superseded load's
		// flag belongs to the load that started after it).
		if (seq === fetchSeq) loading = false;
	}

	$effect(() => {
		const sub = $user?.profile.sub ?? null;
		if (sub === null) {
			fetchSeq++; // invalidate any in-flight load
			budgets = [];
			categories = [];
			summaries = {};
			loadError = null;
			loading = false;
			return;
		}
		void loadData();
	});

	/** The backend answers 4xx with the 3.1 envelope {error: {code,
	 * message}}; ApiError folds the raw body into its message ("API
	 * 400: {...}") — carve the JSON out for the friendly line, fall
	 * back to the raw message when the body is not an envelope. A 400
	 * message IS the "; "-joined problem list, shown verbatim. */
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

	/** The target document from the form's raw text inputs: the
	 * NON-EMPTY ones, as JSON numbers (the wire requires numbers). */
	function targetsFromInputs(vals: Record<string, string>): Record<string, number> {
		const out: Record<string, number> = {};
		for (const [id, v] of Object.entries(vals)) {
			if (v.trim() === '') continue;
			out[id] = Number(v);
		}
		return out;
	}

	// --- Actions --------------------------------------------------------

	async function submitNew(): Promise<void> {
		if (creating) return;
		creating = true;
		formError = null;
		try {
			await createBudget({
				name: newName.trim(),
				period: newPeriod,
				active: true,
				targets: targetsFromInputs(newTargets)
			});
			newName = '';
			newTargets = {};
			await loadData(); // full refetch — the new budget appears
		} catch (e) {
			formError = envelopeMessage(e);
		} finally {
			creating = false;
		}
	}

	function startEdit(b: Budget): void {
		editingId = b.id;
		editName = b.name;
		editError = null;
		cardError[b.id] = '';
		const t: Record<string, string> = {};
		for (const c of activeCategories) {
			const n = b.targets[c.id];
			t[c.id] = n === undefined ? '' : String(n);
		}
		editTargets = t;
	}

	function cancelEdit(): void {
		editingId = null;
		editError = null;
	}

	async function saveEdit(b: Budget): Promise<void> {
		if (saving) return;
		saving = true;
		editError = null;
		try {
			const updated = await updateBudget(b.id, {
				name: editName.trim(),
				targets: targetsFromInputs(editTargets)
			});
			replaceBudget(updated);
			editingId = null;
		} catch (e) {
			editError = envelopeMessage(e);
		} finally {
			saving = false;
		}
	}

	/** Flips the checkbox optimistically and rolls the flip back when
	 * the patch fails; on success the server's row is the local one. */
	async function toggleActive(b: Budget): Promise<void> {
		if (busy[b.id]) return;
		const next = !b.active;
		busy[b.id] = true;
		b.active = next;
		try {
			const updated = await updateBudget(b.id, { active: next });
			replaceBudget(updated);
			cardError[b.id] = '';
		} catch (e) {
			b.active = !next;
			cardError[b.id] = envelopeMessage(e);
		} finally {
			busy[b.id] = false;
		}
	}

	/** Confirms, deletes, and removes the card locally (no refetch —
	 * this page's adherence renders each budget's OWN targets, so a
	 * removed budget leaves no stale row). */
	async function removeBudget(b: Budget): Promise<void> {
		if (busy[b.id]) return;
		if (!window.confirm($_('budgets.confirm_delete', { values: { name: b.name } }))) return;
		busy[b.id] = true;
		try {
			await deleteBudget(b.id);
			budgets = budgets.filter((x) => x.id !== b.id);
			if (editingId === b.id) cancelEdit();
			cardError[b.id] = '';
		} catch (e) {
			cardError[b.id] = envelopeMessage(e);
		} finally {
			busy[b.id] = false;
		}
	}

	function replaceBudget(updated: Budget): void {
		const i = budgets.findIndex((x) => x.id === updated.id);
		if (i !== -1) budgets[i] = updated;
	}

	// --- Display helpers --------------------------------------------------

	/** A budget's target categories (id + display name) — the
	 * adherence rows and the history table's columns. */
	function targetCats(b: Budget): { id: string; name: string }[] {
		return Object.entries(b.targets).map(([id]) => ({ id, name: categoryName(id) }));
	}

	/** The category name for a target key (the raw id as a fallback,
	 * e.g. for a category that no longer exists). */
	function categoryName(id: string): string {
		return categories.find((c) => c.id === id)?.name ?? id;
	}

	/** The targets as "Name: value, …" — the values render via
	 * Svelte's default number-to-string (see the page header). */
	function targetsText(b: Budget): string {
		return targetCats(b)
			.map((c) => `${c.name}: ${b.targets[c.id]}`)
			.join(', ');
	}

	function periodLabel(p: Period): string {
		if (p === 'quarter') return $_('budgets.period_quarter');
		if (p === 'year') return $_('budgets.period_year');
		return $_('budgets.period_month');
	}

	/** The current-period spend string for a target category, from
	 * the matching top-level summary group — VERBATIM, or null when
	 * the window has no transactions in the category (the "no
	 * transactions" marker renders instead — never a fabricated
	 * number). */
	function rowSpend(b: Budget, id: string): string | null {
		const s = summaries[b.period];
		if (!s) return null;
		return s.groups.find((g) => g.category_id === id)?.spend ?? null;
	}

	/** Bar fill for an adherence row: spend as a % of the budget's
	 * OWN target (capped at 100), or null when there is no spend to
	 * measure or the target is not positive. GEOMETRY ONLY. */
	function rowPct(b: Budget, id: string): number | null {
		const s = rowSpend(b, id);
		if (s === null) return null;
		const t = targetCents(b.targets[id]);
		if (t <= 0) return null;
		return Math.min(100, Math.round((toCents(s) / t) * 100));
	}

	/** True when the spend exceeds the budget's own target (the red
	 * bar/cell treatment + label). GEOMETRY ONLY. */
	function rowOver(b: Budget, id: string): boolean {
		const s = rowSpend(b, id);
		return s !== null && spendOverTarget(s, b.targets[id]);
	}

	/** The history table's cell: the matching trend group's spend
	 * string VERBATIM, or null for the "no transactions" marker. */
	function trendSpend(groups: TrendGroup[], id: string): string | null {
		return groups.find((g) => g.category_id === id)?.spend ?? null;
	}

	/** True when a history cell's spend exceeds the budget's own
	 * target (the red treatment; no spend = never over). */
	function cellOver(groups: TrendGroup[], id: string, target: number): boolean {
		const s = trendSpend(groups, id);
		return s !== null && spendOverTarget(s, target);
	}

	/** GEOMETRY ONLY (bar widths, over/under) — never for displayed
	 * text, which always renders the raw API string. A canonical
	 * 2-decimal amount string as integer cents. */
	function toCents(s: string): number {
		const m = /^(-?)(\d+)(?:\.(\d{1,2}))?$/.exec(s);
		if (!m) return 0;
		const cents = (m[3] ?? '').padEnd(2, '0');
		const v = parseInt(m[2], 10) * 100 + parseInt(cents, 10);
		return m[1] === '-' ? -v : v;
	}

	/** A budget target number (wire JSON number) as integer cents.
	 * GEOMETRY ONLY. */
	function targetCents(n: number): number {
		return Math.round(n * 100);
	}

	/** True when a spend string exceeds a target number in cents.
	 * GEOMETRY ONLY. */
	function spendOverTarget(spend: string, target: number): boolean {
		const t = targetCents(target);
		return t > 0 && toCents(spend) > t;
	}
</script>

<section class="space-y-4">
	<h1 class="text-2xl font-semibold">{$_('budgets.heading')}</h1>
	<p class="max-w-2xl text-sm text-zinc-600 dark:text-zinc-400">{$_('budgets.intro')}</p>

	{#if !$user}
		<p class="text-sm text-zinc-600 dark:text-zinc-400">
			{$_('budgets.sign_in_prompt')}
			<button class="underline" onclick={() => login(page.url.pathname + page.url.search)}>
				{$_('auth.sign_in')}
			</button>
		</p>
	{:else}
		<!-- New budget (always visible): name + period + a raw-text
		     target input per active category (empty = no target for
		     that category). -->
		<div class="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
			<p class="text-xs font-medium text-zinc-500 dark:text-zinc-400 uppercase">{$_('budgets.new_budget')}</p>
			<div class="mt-3 flex flex-wrap items-end gap-4">
				<label class="block text-sm">
					<span class="mb-1 block font-medium">{$_('budgets.name_label')}</span>
					<input
						class="w-56 rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
						placeholder={$_('budgets.name_placeholder')}
						bind:value={newName}
					/>
				</label>
				<label class="block text-sm">
					<span class="mb-1 block font-medium">{$_('budgets.period_label')}</span>
					<select
						class="w-40 rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
						value={newPeriod}
						onchange={(e) => (newPeriod = e.currentTarget.value as Period)}
					>
						<option value="month">{$_('budgets.period_month')}</option>
						<option value="quarter">{$_('budgets.period_quarter')}</option>
						<option value="year">{$_('budgets.period_year')}</option>
					</select>
				</label>
				<button
					class="rounded-md bg-zinc-900 px-4 py-2 text-sm font-medium text-white hover:bg-zinc-700 disabled:opacity-50"
					disabled={creating}
					type="button"
					onclick={() => void submitNew()}
				>
					{creating ? $_('budgets.creating') : $_('budgets.create')}
				</button>
			</div>
			<div class="mt-4">
				<span class="mb-1 block text-sm font-medium">{$_('budgets.targets_label')}</span>
				<div class="flex flex-wrap items-end gap-4">
					{#each activeCategories as c (c.id)}
						<label class="block text-sm">
							<span class="mb-1 block font-medium">{c.name}</span>
							<input
								class="w-28 rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
								placeholder={$_('budgets.target_none')}
								inputmode="decimal"
								bind:value={newTargets[c.id]}
							/>
						</label>
					{/each}
				</div>
			</div>
			{#if formError}
				<p class="mt-2 text-sm text-red-600 dark:text-red-400">{formError}</p>
			{/if}
		</div>

		{#if loading && budgets.length === 0}
			<p class="text-sm text-zinc-600 dark:text-zinc-400">{$_('budgets.loading')}</p>
		{:else}
			{#if loadError}
				<p class="text-sm text-red-600 dark:text-red-400">{loadError}</p>
			{/if}
			{#if budgets.length === 0 && !loadError}
				<p class="text-sm text-zinc-500 dark:text-zinc-400">{$_('budgets.empty')}</p>
			{:else}
				<div class="space-y-4">
					{#each budgets as b (b.id)}
						<!-- One card per budget: name + period + the active
						     toggle; an inline edit (name + targets; the
						     period is fixed at creation, so it renders
						     read-only) or the current-period adherence
						     against the budget's OWN targets plus the
						     trailing 6-window history table. -->
						<div class="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
							<div class="flex flex-wrap items-center gap-3">
								<h2 class="text-lg font-semibold">{b.name}</h2>
								<span class="rounded bg-zinc-100 dark:bg-zinc-800 px-2 py-0.5 text-xs font-medium text-zinc-600 dark:text-zinc-400">
									{periodLabel(b.period)}
								</span>
								<label class="ml-auto flex items-center gap-2 text-sm text-zinc-600 dark:text-zinc-400">
									<input
										type="checkbox"
										checked={b.active}
										disabled={busy[b.id]}
										onchange={() => void toggleActive(b)}
									/>
									{$_('budgets.active')}
								</label>
								{#if editingId !== b.id}
									<button
										class="rounded-md border border-zinc-300 dark:border-zinc-700 px-4 py-2 text-sm font-medium text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-800 disabled:opacity-50"
										type="button"
										onclick={() => startEdit(b)}
									>
										{$_('budgets.edit')}
									</button>
									<button
										class="rounded-md border border-red-300 dark:border-red-900 px-4 py-2 text-sm font-medium text-red-600 dark:text-red-400 hover:bg-red-50 dark:hover:bg-red-950 disabled:opacity-50"
										disabled={busy[b.id]}
										type="button"
										onclick={() => void removeBudget(b)}
									>
										{$_('budgets.delete')}
									</button>
								{/if}
							</div>
							{#if editingId === b.id}
								<div class="mt-4 border-t border-zinc-100 dark:border-zinc-800 pt-4">
									<div class="flex flex-wrap items-end gap-4">
										<label class="block text-sm">
											<span class="mb-1 block font-medium">{$_('budgets.name_label')}</span>
											<input
												class="w-56 rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
												bind:value={editName}
											/>
										</label>
										<div class="block text-sm">
											<span class="mb-1 block font-medium">{$_('budgets.period_label')}</span>
											<span
												class="inline-flex w-40 items-center rounded-md border border-zinc-200 dark:border-zinc-800 bg-zinc-50 dark:bg-zinc-950 px-3 py-1.5 text-zinc-500 dark:text-zinc-400"
											>
												{periodLabel(b.period)}
											</span>
										</div>
										<button
											class="rounded-md bg-zinc-900 px-4 py-2 text-sm font-medium text-white hover:bg-zinc-700 disabled:opacity-50"
											disabled={saving}
											type="button"
											onclick={() => void saveEdit(b)}
										>
											{saving ? $_('budgets.saving') : $_('budgets.save')}
										</button>
										<button
											class="rounded-md border border-zinc-300 dark:border-zinc-700 px-4 py-2 text-sm font-medium text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-800 disabled:opacity-50"
											disabled={saving}
											type="button"
											onclick={cancelEdit}
										>
											{$_('budgets.cancel')}
										</button>
									</div>
									<div class="mt-4">
										<span class="mb-1 block text-sm font-medium">
											{$_('budgets.targets_label')}
										</span>
										<div class="flex flex-wrap items-end gap-4">
											{#each activeCategories as c (c.id)}
												<label class="block text-sm">
													<span class="mb-1 block font-medium">{c.name}</span>
													<input
														class="w-28 rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
														placeholder={$_('budgets.target_none')}
														inputmode="decimal"
														bind:value={editTargets[c.id]}
													/>
												</label>
											{/each}
										</div>
									</div>
									<p class="mt-2 text-xs text-zinc-500 dark:text-zinc-400">{$_('budgets.period_fixed')}</p>
									{#if editError}
										<p class="mt-2 text-sm text-red-600 dark:text-red-400">{editError}</p>
									{/if}
								</div>
							{:else}
								<!-- Targets as "Name: value, …" (the target
									 numbers render via Svelte's default
									 number-to-string — see the page header). -->
								<p class="mt-3 text-sm text-zinc-600 dark:text-zinc-400">{targetsText(b)}</p>

								<!-- Current-period adherence: the spend strings
									 are the raw API text; the bar and the
									 over/under label are geometry only. -->
								<p class="mt-4 text-xs font-medium text-zinc-500 dark:text-zinc-400 uppercase">
									{$_('budgets.current_window', {
										values: {
											from: summaries[b.period]?.from ?? '',
											to: summaries[b.period]?.to ?? ''
										}
									})}
								</p>
								<ul class="mt-2 space-y-3">
									{#each targetCats(b) as c (c.id)}
										<li class="text-sm">
											<div class="flex items-baseline justify-between gap-2">
												<span class="font-medium">{c.name}</span>
												<span class="text-zinc-600 dark:text-zinc-400 tabular-nums">
													{rowSpend(b, c.id) ?? $_('budgets.no_tx')}
													<span class="text-zinc-400"> / {b.targets[c.id]}</span>
												</span>
											</div>
											<div class="mt-1 h-2 w-full overflow-hidden rounded bg-zinc-100 dark:bg-zinc-800">
												{#if rowPct(b, c.id) !== null}
													<div
														class="h-full rounded {rowOver(b, c.id)
															? 'bg-red-500'
															: 'bg-green-600'}"
														style="width: {rowPct(b, c.id)}%"
													></div>
												{/if}
											</div>
											{#if rowOver(b, c.id)}
												<p class="mt-0.5 text-xs text-red-600 dark:text-red-400">{$_('budgets.over')}</p>
											{:else if rowSpend(b, c.id) !== null}
												<p class="mt-0.5 text-xs text-green-700 dark:text-green-400">{$_('budgets.under')}</p>
											{/if}
										</li>
									{/each}
								</ul>

								<!-- Adherence history: a plain table (oldest →
									 newest); the plan's "chart/table" — the
									 table is the honest minimal choice, the
									 ECharts call is 6.4's. Cells are the raw
									 API strings; red = over the budget's own
									 target for the category. -->
								<p class="mt-4 text-xs font-medium text-zinc-500 dark:text-zinc-400 uppercase">
									{$_('budgets.history')}
								</p>
								{#if (summaries[b.period]?.trend ?? []).length === 0}
									<p class="mt-2 text-sm text-zinc-500 dark:text-zinc-400">{$_('budgets.no_tx')}</p>
								{:else}
									<div class="mt-2 overflow-x-auto">
										<table class="w-full text-sm">
											<thead>
												<tr
													class="border-b border-zinc-200 dark:border-zinc-800 text-left text-xs text-zinc-500 dark:text-zinc-400 uppercase"
												>
													<th class="py-1.5 pr-4">{$_('budgets.col_window')}</th>
													{#each targetCats(b) as c (c.id)}
														<th class="py-1.5 pr-4 text-right">{c.name}</th>
													{/each}
												</tr>
											</thead>
											<tbody>
												{#each summaries[b.period]?.trend ?? [] as p (p.from)}
													<tr class="border-b border-zinc-100 dark:border-zinc-800 last:border-0">
														<td class="py-1.5 pr-4 text-zinc-600 dark:text-zinc-400">
															{$_('budgets.window_label', {
																values: { from: p.from, to: p.to }
															})}
														</td>
														{#each targetCats(b) as c (c.id)}
															<td
																class="py-1.5 pr-4 text-right tabular-nums {cellOver(
																	p.groups,
																	c.id,
																	b.targets[c.id]
																)
																	? 'font-medium text-red-600 dark:text-red-400'
																	: 'text-zinc-600 dark:text-zinc-400'}"
															>
																{trendSpend(p.groups, c.id) ?? $_('budgets.no_tx')}
															</td>
														{/each}
													</tr>
												{/each}
											</tbody>
										</table>
									</div>
								{/if}
							{/if}
							{#if cardError[b.id]}
								<p class="mt-3 text-sm text-red-600 dark:text-red-400">{cardError[b.id]}</p>
							{/if}
						</div>
					{/each}
				</div>
			{/if}
		{/if}
	{/if}
</section>

<script lang="ts">
	import { resolve } from '$app/paths';
	import { onMount } from 'svelte';
	import { _ } from 'svelte-i18n';
	import { user } from '#lib/auth/client';
	import { ApiError } from '#lib/api/client';
	import { listAccounts, type Account } from '#lib/api/accounts';
	import {
		getSummary,
		type Period,
		type Summary,
		type SummaryGroup,
		type TrendPoint
	} from '#lib/api/summary';
	import { settings } from '#lib/settings';

	// Summary page (step 6.2): the period rollup for the signed-in user —
	// KPI totals, per-category spend against active budget targets, a
	// trailing 6-period net sparkline, and an account filter. The period
	// defaults from the settings store (default_period); a user selection
	// afterwards wins.
	//
	// Rendered-numbers contract: every displayed amount is the raw API
	// string (canonical 2-decimal) bound straight into the DOM. Numeric
	// parsing on this page is limited to bar widths and sparkline
	// coordinates (geometry) — never for displayed text.

	// Trailing trend window for the sparkline: always 6 periods.
	const TREND_PERIODS = 6;

	let period = $state<Period>('month');
	let accountId = $state('');
	let summary = $state<Summary | null>(null);
	let accounts = $state<Account[]>([]);
	let loading = $state(true);
	let loadError = $state<string | null>(null);

	// --- Period default from the settings store (step 6.2) --------------
	// Seeded from default_period when the identity (and with it the
	// settings) appears: sub-keyed with a last-seen guard, like the
	// layout's settings effect, so a silent token renewal (same sub,
	// same settings) never re-runs; the settings may land after the
	// identity, so the settings value is part of the key too. A user
	// selection afterwards wins until the identity changes.
	let periodTouched = $state(false);
	let lastSub: string | null = null;
	let lastDefaultPeriod: string | null = null;
	$effect(() => {
		const sub = $user?.profile.sub ?? null;
		const dp = $settings?.default_period ?? null;
		if (sub === lastSub && dp === lastDefaultPeriod) return;
		const identityChanged = sub !== lastSub;
		lastSub = sub;
		lastDefaultPeriod = dp;
		if (sub === null) {
			periodTouched = false;
			period = 'month';
			return;
		}
		if (identityChanged) periodTouched = false;
		if (periodTouched) return;
		period = dp === 'quarter' || dp === 'year' ? dp : 'month';
	});

	function pickPeriod(p: Period): void {
		periodTouched = true;
		period = p;
	}

	// --- Fetch: one per (identity, period, account) ---------------------
	// Keyed on all three, so changing either control refetches; the seq
	// guard drops stale responses when a change outruns an in-flight
	// request.
	let fetchSeq = 0;
	$effect(() => {
		const sub = $user?.profile.sub ?? null;
		if (sub === null) {
			fetchSeq++;
			summary = null;
			loadError = null;
			loading = false;
			return;
		}
		const seq = ++fetchSeq;
		const p = period;
		const a = accountId;
		loading = true;
		loadError = null;
		getSummary(p, a === '' ? undefined : a, TREND_PERIODS)
			.then((s) => {
				if (seq !== fetchSeq) return;
				summary = s;
			})
			.catch((e) => {
				if (seq !== fetchSeq) return;
				summary = null;
				loadError = envelopeMessage(e);
			})
			.finally(() => {
				if (seq !== fetchSeq) return;
				loading = false;
			});
	});

	// The account picker's data (loaded once in onMount, like the review
	// page's pickers; a signed-out load simply 401s and the filter stays
	// "All accounts").
	onMount(() => {
		listAccounts()
			.then((a) => {
				accounts = a;
			})
			.catch(() => {
				accounts = [];
			});
	});

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
	 * ONLY (bar widths, sparkline coordinates) — never for displayed
	 * text, which always renders the raw API string. */
	function toCents(s: string): number {
		const m = /^(-?)(\d+)(?:\.(\d{1,2}))?$/.exec(s);
		if (!m) return 0;
		const cents = (m[3] ?? '').padEnd(2, '0');
		const v = parseInt(m[2], 10) * 100 + parseInt(cents, 10);
		return m[1] === '-' ? -v : v;
	}

	/** Bar fill: spend as a % of the target (capped at 100), or null
	 * when the group has no target (the track stays empty). */
	function barPct(g: SummaryGroup): number | null {
		if (g.target === null) return null;
		const t = toCents(g.target);
		if (t <= 0) return null;
		return Math.min(100, Math.round((toCents(g.spend) / t) * 100));
	}

	/** True when the group has a target and spend exceeds it (the
	 * over-budget bar treatment + label). */
	function overTarget(g: SummaryGroup): boolean {
		if (g.target === null) return false;
		const t = toCents(g.target);
		return t > 0 && toCents(g.spend) > t;
	}

	/** The row label: the category name, or the i18n uncategorized line
	 * for the null-category bucket. */
	function groupName(g: SummaryGroup): string {
		return g.category_name ?? $_('summary.uncategorized');
	}

	// --- Sparkline geometry (trailing trend, oldest → newest) -----------
	const SPARK_W = 100;
	const SPARK_H = 32;
	const SPARK_PAD = 2;

	/** The polyline points for trend[].net scaled into the fixed viewBox
	 * (pure coordinate math; the values are never rendered as text). */
	function sparkPoints(trend: TrendPoint[] | undefined): string | null {
		if (!trend || trend.length < 2) return null;
		const values = trend.map((p) => toCents(p.net));
		const min = Math.min(...values);
		const max = Math.max(...values);
		const span = max - min;
		const stepX = (SPARK_W - 2 * SPARK_PAD) / (values.length - 1);
		const innerH = SPARK_H - 2 * SPARK_PAD;
		return values
			.map((v, i) => {
				const x = Math.round(SPARK_PAD + i * stepX);
				const y =
					span === 0
						? Math.round(SPARK_H / 2)
						: Math.round(SPARK_PAD + innerH * (1 - (v - min) / span));
				return `${x},${y}`;
			})
			.join(' ');
	}
</script>

<section class="space-y-4">
	<h1 class="text-2xl font-semibold">{$_('summary.heading')}</h1>
	<p class="max-w-2xl text-sm text-zinc-600">{$_('summary.intro')}</p>

	{#if !$user}
		<p class="text-sm text-zinc-600">
			{$_('summary.sign_in_prompt')}
			<a class="underline" href={resolve('/')}>{$_('auth.sign_in')}</a>
		</p>
	{:else}
		<!-- Controls: period switcher + account filter; changing either
		     refetches (one fetch per (period, account)). -->
		<div class="flex flex-wrap items-end gap-4">
			<label class="block text-sm">
				<span class="mb-1 block font-medium">{$_('summary.period_label')}</span>
				<select
					class="w-40 rounded-md border border-zinc-300 px-3 py-1.5"
					value={period}
					onchange={(e) => pickPeriod(e.currentTarget.value as Period)}
				>
					<option value="month">{$_('summary.period_month')}</option>
					<option value="quarter">{$_('summary.period_quarter')}</option>
					<option value="year">{$_('summary.period_year')}</option>
				</select>
			</label>
			<label class="block text-sm">
				<span class="mb-1 block font-medium">{$_('summary.account_label')}</span>
				<select
					class="w-64 rounded-md border border-zinc-300 px-3 py-1.5"
					value={accountId}
					onchange={(e) => (accountId = e.currentTarget.value)}
				>
					<option value="">{$_('summary.all_accounts')}</option>
					{#each accounts as a (a.id)}
						<option value={a.id}>{a.name}</option>
					{/each}
				</select>
			</label>
		</div>

		{#if loading && !summary}
			<p class="text-sm text-zinc-600">{$_('summary.loading')}</p>
		{:else if loadError}
			<p class="text-sm text-red-600">{loadError}</p>
		{:else if summary}
			<!-- KPI cards: the raw API strings, verbatim. -->
			<div class="grid grid-cols-1 gap-4 sm:grid-cols-3">
				<div class="rounded-lg border border-zinc-200 bg-white p-5">
					<p class="text-xs font-medium text-zinc-500 uppercase">{$_('summary.kpi_spend')}</p>
					<p class="mt-1 text-2xl font-semibold text-zinc-900">{summary.totals.spend}</p>
				</div>
				<div class="rounded-lg border border-zinc-200 bg-white p-5">
					<p class="text-xs font-medium text-zinc-500 uppercase">{$_('summary.kpi_income')}</p>
					<p class="mt-1 text-2xl font-semibold text-green-700">{summary.totals.income}</p>
				</div>
				<div class="rounded-lg border border-zinc-200 bg-white p-5">
					<p class="text-xs font-medium text-zinc-500 uppercase">{$_('summary.kpi_net')}</p>
					<p class="mt-1 text-2xl font-semibold text-zinc-900">{summary.totals.net}</p>
				</div>
			</div>

			<!-- 6-period net sparkline (oldest → newest, no axis labels). -->
			<div class="rounded-lg border border-zinc-200 bg-white p-5">
				<p class="text-xs font-medium text-zinc-500 uppercase">{$_('summary.trend_title')}</p>
				{#if sparkPoints(summary.trend)}
					<svg
						viewBox="0 0 100 32"
						preserveAspectRatio="none"
						class="mt-2 h-16 w-full"
						role="img"
						aria-label={$_('summary.trend_aria')}
					>
						<polyline
							points={sparkPoints(summary.trend) ?? ''}
							fill="none"
							stroke="currentColor"
							stroke-width="1.5"
							vector-effect="non-scaling-stroke"
							class="text-zinc-700"
						/>
					</svg>
				{:else}
					<p class="mt-2 text-sm text-zinc-500">{$_('summary.empty')}</p>
				{/if}
			</div>

			<!-- Per-category spend vs the active budget target. -->
			<div class="rounded-lg border border-zinc-200 bg-white p-5">
				<p class="text-xs font-medium text-zinc-500 uppercase">{$_('summary.categories_title')}</p>
				{#if summary.groups.length === 0}
					<p class="mt-2 text-sm text-zinc-500">{$_('summary.empty')}</p>
				{:else}
					<ul class="mt-3 space-y-3">
						{#each summary.groups as g (g.category_id ?? 'uncategorized')}
							<li class="text-sm">
								<div class="flex items-baseline justify-between gap-2">
									<span class="font-medium">{groupName(g)}</span>
									<span class="text-zinc-600 tabular-nums">
										{g.spend}{#if g.target !== null}
											<span class="text-zinc-400"> / {g.target}</span>
										{/if}
									</span>
								</div>
								<!-- Bar fill: geometry only (the % is
									 computed from the strings; the numbers
									 themselves render as the raw strings). -->
								<div class="mt-1 h-2 w-full overflow-hidden rounded bg-zinc-100">
									{#if barPct(g) !== null}
										<div
											class="h-full rounded {overTarget(g) ? 'bg-red-500' : 'bg-zinc-700'}"
											style="width: {barPct(g)}%"
										></div>
									{/if}
								</div>
								{#if overTarget(g)}
									<p class="mt-0.5 text-xs text-red-600">{$_('summary.over_budget')}</p>
								{/if}
							</li>
						{/each}
					</ul>
				{/if}
			</div>
		{:else}
			<p class="text-sm text-zinc-500">{$_('summary.empty')}</p>
		{/if}
	{/if}
</section>

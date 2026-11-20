<script lang="ts">
	import { page } from '$app/state';
	import { _ } from 'svelte-i18n';
	import { user } from '#lib/auth/client';
	import { ApiError } from '#lib/api/client';
	import {
		getEquity,
		listSnapshots,
		createSnapshot,
		patchSnapshot,
		deleteSnapshot,
		PLAN_TYPES,
		type Snapshot,
		type EquityPoint
	} from '#lib/api/savings';
	import { listTransactions, type Transaction } from '#lib/api/transactions';
	import { listAccounts, type Account } from '#lib/api/accounts';
	import * as echarts from 'echarts/core';
	import type { EChartsOption } from 'echarts';
	import { LineChart } from 'echarts/charts';
	import { TooltipComponent, GridComponent } from 'echarts/components';
	import { CanvasRenderer } from 'echarts/renderers';
	import { Chart } from 'svelte-echarts';

	// ECharts is tree-shaken to the pieces this page uses: the single
	// line chart, the tooltip + grid it configures, and the canvas
	// renderer. <Chart init={echarts.init}> runs exactly this build, not
	// the full bundle.
	echarts.use([LineChart, TooltipComponent, GridComponent, CanvasRenderer]);

	// Appreciation page (round 53d): one account's equity series (the
	// snapshot balances with the running balance between them), the
	// snapshot add/edit/delete list, and the account's ten most recent
	// transactions. Rendered-numbers contract: every displayed amount is
	// the verbatim canonical string — only the chart's axis/series data
	// goes through Number().

	const accountId = page.params.accountId ?? '';

	const cardClass =
		'rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5';
	const buttonClass =
		'rounded-md border border-zinc-300 dark:border-zinc-700 px-4 py-2 text-sm font-medium ' +
		'text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-800';
	const inputClass = 'rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5 text-sm';
	const linkClass =
		'text-sm text-zinc-600 dark:text-zinc-400 hover:underline ' +
		'hover:text-zinc-900 dark:hover:text-zinc-100';

	let account = $state<Account | null>(null);
	let equityPoints = $state<EquityPoint[]>([]);
	let snapshots = $state<Snapshot[]>([]);
	let recentTx = $state<Transaction[]>([]);
	let loadError = $state('');
	let loading = $state(false);
	let seq = 0;

	let snapDate = $state('');
	let snapBalance = $state('');
	let snapNote = $state('');
	let snapSaving = $state(false);
	let snapError = $state('');

	let editId = $state('');
	let editBalance = $state('');
	let editNote = $state('');
	let editSaving = $state(false);
	let editError = $state('');

	let flashNote = $state('');

	// Local calendar today (YYYY-MM-DD): the date input's max and the
	// validation bound (a snapshot's as_of must not be in the future).
	const now = new Date();
	const today =
		now.getFullYear() +
		'-' +
		String(now.getMonth() + 1).padStart(2, '0') +
		'-' +
		String(now.getDate()).padStart(2, '0');

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

	async function load(): Promise<void> {
		if (!$user?.profile.sub) return;
		const s = ++seq;
		loading = true;
		loadError = '';
		try {
			const [accounts, equity, snaps, txs] = await Promise.all([
				listAccounts(),
				getEquity(accountId),
				listSnapshots(accountId),
				listTransactions({ account_id: [accountId], limit: 10 })
			]);
			if (s !== seq) return;
			account = accounts.find((a) => a.id === accountId) ?? null;
			equityPoints = equity.points;
			snapshots = snaps;
			recentTx = txs;
			loading = false;
		} catch (e) {
			if (s !== seq) return;
			loading = false;
			loadError = envelopeMessage(e);
		}
	}

	// One load per identity change; a signed-out caller sees the gate.
	$effect(() => {
		const sub = $user?.profile.sub ?? null;
		if (sub === null) {
			seq++;
			account = null;
			equityPoints = [];
			snapshots = [];
			recentTx = [];
			loading = false;
			loadError = '';
			return;
		}
		void load();
	});

	/** The line-chart option: x = each point's as_of, y = the balance as
	 * a number (the page's only Number() — axis data, not a displayed
	 * string). The tooltip renders the verbatim strings. */
	const chartOption = $derived.by((): EChartsOption | null => {
		if (equityPoints.length === 0) return null;
		return {
			tooltip: {
				trigger: 'axis',
				formatter: (params: unknown) => {
					const list = Array.isArray(params) ? params : [params];
					const first = list[0] as { dataIndex?: number } | undefined;
					const pt =
						first && typeof first.dataIndex === 'number'
							? equityPoints[first.dataIndex]
							: undefined;
					return pt ? `${pt.as_of} — ${pt.balance}` : '';
				}
			},
			xAxis: { type: 'category', data: equityPoints.map((p) => p.as_of) },
			yAxis: { type: 'value' },
			series: [{ type: 'line', data: equityPoints.map((p) => Number(p.balance)) }]
		};
	});

	/** Shows the green note, auto-clearing after 2.5s (room-page idiom). */
	function setFlash(text: string): void {
		flashNote = text;
		setTimeout(() => {
			flashNote = '';
		}, 2500);
	}

	async function addSnapshot(): Promise<void> {
		if (snapDate === '' || snapDate > today) {
			snapError = $_('savings.snapshot_date_invalid');
			return;
		}
		if (!/^\d{1,7}(\.\d{1,2})?$/.test(snapBalance)) {
			snapError = $_('savings.snapshot_balance_invalid');
			return;
		}
		snapError = '';
		snapSaving = true;
		try {
			await createSnapshot({
				account_id: accountId,
				as_of: snapDate,
				balance: snapBalance,
				note: snapNote
			});
			snapDate = '';
			snapBalance = '';
			snapNote = '';
			snapError = '';
			setFlash($_('savings.snapshot_added'));
			await load();
		} catch (e) {
			snapError = envelopeMessage(e);
		} finally {
			snapSaving = false;
		}
	}

	function startEdit(snap: Snapshot): void {
		editId = snap.id;
		editBalance = snap.balance;
		editNote = snap.note;
		editError = '';
	}

	function cancelEdit(): void {
		editId = '';
		editError = '';
	}

	async function saveEdit(): Promise<void> {
		if (!/^\d{1,7}(\.\d{1,2})?$/.test(editBalance)) {
			editError = $_('savings.snapshot_balance_invalid');
			return;
		}
		editError = '';
		editSaving = true;
		try {
			await patchSnapshot(editId, {
				balance: editBalance,
				note: editNote === '' ? null : editNote
			});
			editId = '';
			setFlash($_('savings.snapshot_saved'));
			await load();
		} catch (e) {
			editError = envelopeMessage(e);
		} finally {
			editSaving = false;
		}
	}

	async function deleteSnap(id: string): Promise<void> {
		try {
			await deleteSnapshot(id);
			setFlash($_('savings.snapshot_deleted'));
			await load();
		} catch (e) {
			setFlash(envelopeMessage(e));
		}
	}
</script>

<section class="space-y-4">
	<div>
		<a href="/savings" class={linkClass}>
			← {$_('savings.appreciation_back')}
		</a>
		<h1 class="mt-1 text-2xl font-semibold">
			{account !== null ? account.name : $_('savings.appreciation_heading')}
		</h1>
		{#if account !== null && (PLAN_TYPES as readonly string[]).includes(account.type)}
			<p class="mt-1 text-sm text-zinc-500 dark:text-zinc-400">
				{$_('savings.plan_' + account.type)}
			</p>
		{/if}
	</div>

	{#if !$user}
		<p class="text-sm text-zinc-600 dark:text-zinc-400">{$_('savings.detail_sign_in')}</p>
	{:else if loading && account === null && !loadError}
		<p class="text-sm text-zinc-600 dark:text-zinc-400">
			{$_('savings.appreciation_loading')}
		</p>
	{:else if loadError}
		<p class="text-sm text-red-600 dark:text-red-400">{loadError}</p>
	{:else if account === null}
		<p class="text-sm text-zinc-600 dark:text-zinc-400">
			{$_('savings.appreciation_not_found')}
		</p>
	{:else}
		<div class={cardClass}>
			<h2 class="text-lg font-semibold">{$_('savings.appreciation_heading')}</h2>
			<p class="mt-1 text-sm text-zinc-600 dark:text-zinc-400">
				{$_('savings.appreciation_hint')}
			</p>
			{#if chartOption === null}
				<p class="mt-3 text-sm text-zinc-600 dark:text-zinc-400">
					{$_('savings.appreciation_chart_empty')}
				</p>
			{:else}
				<div class="mt-3 h-80">
					<Chart init={echarts.init} options={chartOption} class="h-full w-full" />
				</div>
			{/if}
		</div>

		<div class={cardClass}>
			<h2 class="text-lg font-semibold">{$_('savings.snapshots_title')}</h2>
			<div class="mt-3 overflow-x-auto">
				<table class="w-full text-sm">
					<thead>
						<tr
							class="border-b border-zinc-200 dark:border-zinc-800 text-left text-xs text-zinc-500 dark:text-zinc-400 uppercase"
						>
							<th class="px-4 py-2">{$_('savings.snapshot_date_label')}</th>
							<th class="px-4 py-2 text-right">
								{$_('savings.snapshot_balance_label')}
							</th>
							<th class="px-4 py-2">{$_('savings.snapshot_note_label')}</th>
							<th class="px-4 py-2"></th>
						</tr>
					</thead>
					<tbody>
						{#each snapshots as snap (snap.id)}
							{@const editing = editId === snap.id}
							<tr class="border-b border-zinc-100 dark:border-zinc-800 last:border-0">
								<td class="px-4 py-1.5 tabular-nums">{snap.as_of}</td>
								<td class="px-4 py-1.5 text-right">
									{#if editing}
										<input
											type="text"
											inputmode="decimal"
											bind:value={editBalance}
											class={inputClass + ' w-24'}
										/>
									{:else}
										<span class="tabular-nums">{snap.balance}</span>
									{/if}
								</td>
								<td class="px-4 py-1.5">
									{#if editing}
										<input type="text" bind:value={editNote} class={inputClass} />
									{:else}
										{snap.note}
									{/if}
								</td>
								<td class="px-4 py-1.5 text-right">
									{#if editing}
										<button class={linkClass} disabled={editSaving} onclick={() => void saveEdit()}>
											{$_('savings.snapshot_save')}
										</button>
										<button class={linkClass} onclick={() => cancelEdit()}>
											{$_('savings.snapshot_cancel')}
										</button>
									{:else}
										<button class={linkClass} onclick={() => startEdit(snap)}>
											{$_('savings.snapshot_edit')}
										</button>
										<button class={linkClass} onclick={() => void deleteSnap(snap.id)}>
											{$_('savings.snapshot_delete')}
										</button>
									{/if}
								</td>
							</tr>
							{#if editing && editError}
								<tr>
									<td colspan="4" class="px-4 pb-2">
										<p class="text-sm text-red-600 dark:text-red-400">{editError}</p>
									</td>
								</tr>
							{/if}
						{/each}
					</tbody>
				</table>
			</div>
			<div class="mt-3 flex flex-wrap items-center gap-2">
				<input type="date" max={today} bind:value={snapDate} class={inputClass} />
				<input
					type="text"
					inputmode="decimal"
					bind:value={snapBalance}
					placeholder="1234.56"
					class={inputClass + ' w-24'}
				/>
				<input type="text" bind:value={snapNote} maxlength={500} class={inputClass} />
				<button class={buttonClass} disabled={snapSaving} onclick={() => void addSnapshot()}>
					{snapSaving ? $_('savings.snapshot_adding') : $_('savings.snapshot_add')}
				</button>
			</div>
			{#if snapError}
				<p class="mt-2 text-sm text-red-600 dark:text-red-400">{snapError}</p>
			{/if}
		</div>

		<div class={cardClass}>
			<h2 class="text-lg font-semibold">{$_('savings.recent_tx_title')}</h2>
			{#if recentTx.length === 0}
				<p class="mt-3 text-sm text-zinc-600 dark:text-zinc-400">
					{$_('savings.recent_tx_empty')}
				</p>
			{:else}
				<div class="mt-3 overflow-x-auto">
					<table class="w-full text-sm">
						<thead>
							<tr
								class="border-b border-zinc-200 dark:border-zinc-800 text-left text-xs text-zinc-500 dark:text-zinc-400 uppercase"
							>
								<th class="px-4 py-2">{$_('savings.tx_date_label')}</th>
								<th class="px-4 py-2">{$_('savings.tx_description_label')}</th>
								<th class="px-4 py-2 text-right">{$_('savings.tx_amount_label')}</th>
							</tr>
						</thead>
						<tbody>
							{#each recentTx as tx (tx.id)}
								<tr class="border-b border-zinc-100 dark:border-zinc-800 last:border-0">
									<td class="px-4 py-1.5 tabular-nums">{tx.date_posted}</td>
									<td class="px-4 py-1.5">{tx.description}</td>
									<td class="px-4 py-1.5 text-right tabular-nums">
										{tx.amount}
										{#if tx.amount_converted !== '' && tx.converted_currency !== '' && tx.converted_currency !== account.currency}
											<span class="text-xs text-zinc-500 dark:text-zinc-400"
												>→ {tx.amount_converted} {tx.converted_currency}</span
											>
										{/if}
									</td>
								</tr>
							{/each}
						</tbody>
					</table>
				</div>
			{/if}
		</div>

		{#if flashNote}
			<p class="text-sm text-emerald-600 dark:text-emerald-400">{flashNote}</p>
		{/if}
	{/if}
</section>

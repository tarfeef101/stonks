<script lang="ts">
	import { _ } from 'svelte-i18n';
	import { user } from '#lib/auth/client';
	import { ApiError } from '#lib/api/client';
	import { listAccounts } from '#lib/api/accounts';
	import { getEquity, getRoom, type EquityPoint, type RoomView } from '#lib/api/savings';
	import * as echarts from 'echarts/core';
	import type { EChartsOption } from 'echarts';
	import { LineChart } from 'echarts/charts';
	import { TooltipComponent, GridComponent } from 'echarts/components';
	import { CanvasRenderer } from 'echarts/renderers';
	import { Chart } from 'svelte-echarts';

	// ECharts is tree-shaken to the pieces this widget uses: the single
	// line chart, the tooltip + grid it configures, and the canvas
	// renderer. <Chart init={echarts.init}> runs exactly this build, not
	// the full bundle.
	echarts.use([LineChart, TooltipComponent, GridComponent, CanvasRenderer]);

	interface Props {
		content: string;
	}
	let { content }: Props = $props();

	// The stored content encodes the widget's view: '' = the room
	// view; otherwise the appreciation view's JSON {view, account_id}
	// (the dashboard's add flow writes it).
	const cfg = $derived.by((): { account_id: string } | null => {
		if (content === '') return null;
		try {
			const parsed = JSON.parse(content) as { view?: unknown; account_id?: unknown };
			const id = parsed.account_id;
			if (parsed.view === 'appreciation' && typeof id === 'string' && id !== '') {
				return { account_id: id };
			}
			return null;
		} catch {
			// unparseable content — the room view
			return null;
		}
	});
	const accountId = $derived(cfg === null ? '' : cfg.account_id);

	let room = $state<RoomView | null>(null);
	let points = $state<EquityPoint[] | null>(null);
	let accountName = $state('');
	let error = $state<string | null>(null);
	let seq = 0;

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

	async function load(id: string): Promise<void> {
		const s = ++seq;
		room = null;
		points = null;
		accountName = '';
		error = null;
		try {
			if (id === '') {
				room = await getRoom();
			} else {
				const [accts, equity] = await Promise.all([listAccounts(), getEquity(id)]);
				points = equity.points;
				accountName = accts.find((a) => a.id === id)?.name ?? '';
			}
			if (s !== seq) return; // stale
		} catch (e) {
			if (s !== seq) return; // stale
			error = envelopeMessage(e);
		}
	}

	// One load per identity change; a signed-out caller sees the gate.
	$effect(() => {
		const sub = $user?.profile.sub ?? null;
		if (sub === null) {
			seq++;
			room = null;
			points = null;
			accountName = '';
			error = null;
			return;
		}
		void load(accountId);
	});

	/** The line-chart option: x = each point's as_of, y = the balance as
	 * a number (the widget's only Number() — axis data, not a displayed
	 * string). The tooltip renders the verbatim strings. */
	const chartOption = $derived.by((): EChartsOption | null => {
		if (points === null || points.length === 0) return null;
		const pts = points;
		return {
			tooltip: {
				trigger: 'axis',
				formatter: (params: unknown) => {
					const list = Array.isArray(params) ? params : [params];
					const first = list[0] as { dataIndex?: number } | undefined;
					const pt =
						first && typeof first.dataIndex === 'number' ? pts[first.dataIndex] : undefined;
					return pt ? `${pt.as_of} — ${pt.balance}` : '';
				}
			},
			xAxis: { type: 'category', data: pts.map((p) => p.as_of) },
			yAxis: { type: 'value' },
			series: [{ type: 'line', data: pts.map((p) => Number(p.balance)) }]
		};
	});
</script>

{#if !$user}
	<p class="text-sm text-zinc-500 dark:text-zinc-400">{$_('savings.detail_sign_in')}</p>
{:else if error !== null}
	<p class="text-sm text-red-600 dark:text-red-400">{error}</p>
{:else if cfg !== null}
	{#if points === null}
		<p class="text-sm text-zinc-500 dark:text-zinc-400">
			{$_('dashboards.widget_savings_loading')}
		</p>
	{:else if accountName === ''}
		<p class="text-sm text-zinc-500 dark:text-zinc-400">{$_('savings.appreciation_not_found')}</p>
	{:else if chartOption === null}
		<p class="text-sm text-zinc-500 dark:text-zinc-400">{$_('savings.appreciation_chart_empty')}</p>
	{:else}
		<div class="p-4">
			<h3 class="text-sm font-medium">{accountName}</h3>
			<div class="h-80">
				<Chart init={echarts.init} options={chartOption} class="h-full w-full" />
			</div>
		</div>
	{/if}
{:else}
	{#if room === null}
		<p class="text-sm text-zinc-500 dark:text-zinc-400">
			{$_('dashboards.widget_savings_loading')}
		</p>
	{:else}
		<div class="p-4">
			<table class="w-full text-sm">
				<thead class="text-left text-xs text-zinc-500 dark:text-zinc-400 uppercase">
					<tr class="border-b border-zinc-200 dark:border-zinc-800">
						<th class="px-4 py-2">{$_('dashboards.widget_savings_plan_label')}</th>
						<th class="px-4 py-2 text-right">
							{$_('savings.contributed_label', { values: { year: String(room.year) } })}
						</th>
						<th class="px-4 py-2">{$_('savings.room_label')}</th>
					</tr>
				</thead>
				<tbody>
					{#each room.cards as card (card.plan_type + ':' + card.beneficiary)}
						<tr class="border-b border-zinc-100 dark:border-zinc-800 last:border-0">
							<td class="px-4 py-1.5">
								{$_('savings.plan_' + card.plan_type)}
								{#if card.beneficiary !== ''}
									<span class="text-zinc-500 dark:text-zinc-400">— {card.beneficiary}</span>
								{/if}
							</td>
							<td class="px-4 py-1.5 text-right tabular-nums">{card.used}</td>
							<td class="px-4 py-1.5 text-right tabular-nums">
								{#if card.room === null}
									<span class="text-zinc-500 dark:text-zinc-400">—</span>
								{:else}
									{card.room}
								{/if}
							</td>
						</tr>
					{/each}
				</tbody>
			</table>
			<div class="flex justify-between border-t border-zinc-200 dark:border-zinc-800 pt-2 text-sm">
				<span class="text-zinc-500 dark:text-zinc-400">{$_('savings.registered_total')}</span>
				<span class="font-medium tabular-nums">{room.registered_total}</span>
			</div>
			<div class="flex justify-between border-t border-zinc-200 dark:border-zinc-800 pt-2 text-sm">
				<span class="text-zinc-500 dark:text-zinc-400">{$_('savings.investment_total')}</span>
				<span class="font-medium tabular-nums">{room.investment_total}</span>
			</div>
		</div>
	{/if}
{/if}

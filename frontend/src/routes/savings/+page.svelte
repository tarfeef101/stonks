<script lang="ts">
	import { _ } from 'svelte-i18n';
	import { user } from '#lib/auth/client';
	import { ApiError } from '#lib/api/client';
	import {
		getRoom,
		setBirthYear,
		PLAN_TYPES,
		listLimits,
		putLimit,
		deleteLimit,
		listAdjustments,
		createAdjustment,
		deleteAdjustment,
		type RoomView,
		type LimitOverride,
		type RoomAdjustment
	} from '#lib/api/savings';
	import { listAccounts, type Account } from '#lib/api/accounts';
	import { settings } from '#lib/settings';

	const cardClass =
		'rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5';
	const buttonClass =
		'rounded-md border border-zinc-300 dark:border-zinc-700 px-4 py-2 text-sm font-medium ' +
		'text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-800';
	const inputClass = 'rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5 text-sm';
	const linkClass =
		'text-sm text-zinc-600 dark:text-zinc-400 hover:underline ' +
		'hover:text-zinc-900 dark:hover:text-zinc-100';

	let view = $state<RoomView | null>(null);
	let accounts = $state<Account[]>([]);
	let loadError = $state('');
	let loading = $state(false);
	let year = $state(new Date().getFullYear());
	// Mirrors year's initial value without referencing the state (a
	// direct reference triggers state_referenced_locally).
	let yearInput = $state(String(new Date().getFullYear()));
	let seq = 0;
	let birthInput = $state('');
	let birthSaving = $state(false);
	let birthNote = $state('');
	let birthError = $state('');
	let limits = $state<LimitOverride[]>([]);
	let adjustments = $state<RoomAdjustment[]>([]);
	let limitInputs = $state<Record<string, string>>({
		rrsp: '',
		tfsa: '',
		fhsa: '',
		rrif: '',
		resp: '',
		rdsp: ''
	});
	let amountInputs = $state<Record<string, string>>({
		rrsp: '',
		tfsa: '',
		fhsa: '',
		rrif: '',
		resp: '',
		rdsp: ''
	});
	let noteInputs = $state<Record<string, string>>({
		rrsp: '',
		tfsa: '',
		fhsa: '',
		rrif: '',
		resp: '',
		rdsp: ''
	});
	let limitErrors = $state<Record<string, string>>({
		rrsp: '',
		tfsa: '',
		fhsa: '',
		rrif: '',
		resp: '',
		rdsp: ''
	});
	let adjErrors = $state<Record<string, string>>({
		rrsp: '',
		tfsa: '',
		fhsa: '',
		rrif: '',
		resp: '',
		rdsp: ''
	});
	let limitBusy = $state<string | null>(null);
	let adjBusy = $state<string | null>(null);
	let actionNote = $state('');

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
			const [v, accts, lims, adjs] = await Promise.all([
				getRoom(year),
				listAccounts(),
				listLimits(undefined, year),
				listAdjustments(undefined, year)
			]);
			if (s !== seq) return;
			view = v;
			accounts = accts;
			limits = lims;
			adjustments = adjs;
			for (const pt of ['rrsp', 'tfsa', 'fhsa'] as const) {
				limitInputs[pt] =
					lims.find((l) => l.plan_type === pt)?.annual_limit ??
					v.cards.find((c) => c.plan_type === pt)?.annual_limit ??
					'';
			}
			for (const pt of PLAN_TYPES) {
				amountInputs[pt] = '';
				noteInputs[pt] = '';
				limitErrors[pt] = '';
				adjErrors[pt] = '';
			}
			loading = false;
		} catch (e) {
			if (s !== seq) return;
			loading = false;
			loadError = envelopeMessage(e);
			limits = [];
			adjustments = [];
		}
	}

	// Keyed on the identity and, through load(), on `year`, so both the
	// sign-in state and the year picker re-fetch the room.
	$effect(() => {
		const sub = $user?.profile.sub ?? null;
		if (sub === null) {
			seq++;
			view = null;
			accounts = [];
			loading = false;
			loadError = '';
			return;
		}
		void load();
	});

	function handleYearChange(): void {
		if (/^\d{4}$/.test(yearInput) && Number(yearInput) >= 1900 && Number(yearInput) <= 2100) {
			year = Number(yearInput);
		} else {
			yearInput = String(year);
		}
	}

	const planAccounts = $derived(
		accounts.filter((a) => (PLAN_TYPES as readonly string[]).includes(a.type))
	);

	// Distinct non-default currencies among the plan accounts; the note
	// renders only when the joined list is non-empty. A null settings
	// store leaves every currency non-default.
	const nonCadNote = $derived.by(() => {
		const currencies = new Set<string>();
		for (const a of planAccounts) {
			if (a.currency !== $settings?.default_currency) {
				currencies.add(a.currency);
			}
		}
		return [...currencies].sort().join(', ');
	});

	// The warning/note keys are built dynamically from the wire codes.
	// svelte-i18n v2 exports no `has`, so a missing key is detected by
	// the identity of the returned string (the key comes back as-is).
	function warnText(code: string): string {
		const k = 'savings.warning_' + code;
		const out = $_(k);
		return out === k ? code : out;
	}

	function noteText(code: string): string {
		const k = 'savings.note_' + code;
		const out = $_(k);
		return out === k ? code : out;
	}

	function flashNote(key: string): void {
		actionNote = $_(key);
		setTimeout(() => {
			actionNote = '';
		}, 2500);
	}

	async function saveBirth(): Promise<void> {
		if (birthSaving) return;
		if (!/^\d{4}$/.test(birthInput) || Number(birthInput) > new Date().getFullYear()) {
			return;
		}
		birthError = '';
		birthSaving = true;
		try {
			await setBirthYear(birthInput);
			birthNote = $_('savings.birth_saved');
			setTimeout(() => {
				birthNote = '';
			}, 2500);
			await load();
		} catch (e) {
			birthError = envelopeMessage(e);
		} finally {
			birthSaving = false;
		}
	}

	async function saveLimit(pt: string): Promise<void> {
		const value = limitInputs[pt].trim();
		if (!/^\d{1,7}(\.\d{1,2})?$/.test(value)) {
			limitErrors[pt] = $_('savings.override_format_error');
			return;
		}
		limitErrors[pt] = '';
		limitBusy = pt;
		try {
			await putLimit({ plan_type: pt, year, annual_limit: value });
			await load();
			flashNote('savings.override_saved');
		} catch (e) {
			limitErrors[pt] = envelopeMessage(e);
		} finally {
			limitBusy = null;
		}
	}

	async function removeLimit(pt: string): Promise<void> {
		const row = limits.find((l) => l.plan_type === pt);
		if (!row) return;
		limitBusy = pt;
		try {
			await deleteLimit(row.id);
			await load();
			flashNote('savings.override_removed');
		} catch (e) {
			limitErrors[pt] = envelopeMessage(e);
		} finally {
			limitBusy = null;
		}
	}

	async function addAdjustment(pt: string): Promise<void> {
		const value = amountInputs[pt].trim();
		const magnitude = value.startsWith('-') ? value.slice(1) : value;
		if (!/^\d{1,7}(\.\d{1,2})?$/.test(magnitude)) {
			adjErrors[pt] = $_('savings.adjustment_format_error');
			return;
		}
		limitErrors[pt] = '';
		adjBusy = pt;
		try {
			await createAdjustment({
				plan_type: pt,
				year,
				amount: value,
				note: noteInputs[pt].trim()
			});
			await load();
			flashNote('savings.adjustment_added');
		} catch (e) {
			adjErrors[pt] = envelopeMessage(e);
		} finally {
			adjBusy = null;
		}
	}

	async function removeAdjustment(id: string, pt: string): Promise<void> {
		adjBusy = pt;
		try {
			await deleteAdjustment(id);
			await load();
		} catch (e) {
			adjErrors[pt] = envelopeMessage(e);
		} finally {
			adjBusy = null;
		}
	}
</script>

<section class="space-y-4">
	<div class="flex flex-wrap items-start justify-between gap-4">
		<div>
			<h1 class="text-2xl font-semibold">{$_('savings.heading')}</h1>
			<p class="mt-1 text-sm text-zinc-600 dark:text-zinc-400">
				{$_('savings.intro')}
			</p>
		</div>
		<label class="flex items-center gap-2 text-sm text-zinc-600 dark:text-zinc-400">
			{$_('savings.year_label')}
			<input
				type="text"
				inputmode="numeric"
				bind:value={yearInput}
				onchange={handleYearChange}
				class={inputClass + ' w-24'}
			/>
		</label>
	</div>

	{#if !$user}
		<p class="text-sm text-zinc-600 dark:text-zinc-400">
			{$_('savings.sign_in_prompt')}
		</p>
	{:else if loading}
		<p class="text-sm text-zinc-600 dark:text-zinc-400">
			{$_('savings.loading')}
		</p>
	{:else if loadError}
		<p class="text-sm text-red-600 dark:text-red-400">{loadError}</p>
	{:else if view}
		{#if view.birth_year === null}
			<div class={cardClass}>
				<p class="text-sm text-zinc-600 dark:text-zinc-400">
					{$_('savings.birth_banner')}
				</p>
				<div class="mt-3 flex flex-wrap items-center gap-3">
					<label class="flex items-center gap-2 text-sm text-zinc-600 dark:text-zinc-400">
						{$_('savings.birth_year_label')}
						<input
							type="text"
							inputmode="numeric"
							maxlength="4"
							bind:value={birthInput}
							placeholder={$_('savings.birth_year_placeholder')}
							class={inputClass + ' w-24'}
						/>
					</label>
					<button class={buttonClass} disabled={birthSaving} onclick={() => void saveBirth()}>
						{birthSaving ? $_('savings.birth_saving') : $_('savings.birth_save')}
					</button>
				</div>
				{#if birthError}
					<p class="mt-2 text-sm text-red-600 dark:text-red-400">{birthError}</p>
				{/if}
			</div>
		{/if}

		{#if nonCadNote}
			<p class="text-sm text-zinc-600 dark:text-zinc-400">
				{$_('savings.non_cad_note', { values: { cur: nonCadNote } })}
			</p>
		{/if}

		<div class="grid grid-cols-1 gap-4 md:grid-cols-2">
			<div class={cardClass}>
				<p class="text-sm text-zinc-500 dark:text-zinc-400">
					{$_('savings.registered_total')}
				</p>
				<p class="mt-1 text-lg font-semibold">{view.registered_total}</p>
			</div>
			<div class={cardClass}>
				<p class="text-sm text-zinc-500 dark:text-zinc-400">
					{$_('savings.investment_total')}
				</p>
				<p class="mt-1 text-lg font-semibold">{view.investment_total}</p>
			</div>
		</div>

		<div class="grid grid-cols-1 gap-4 md:grid-cols-2">
			{#each view.cards as card (card.plan_type)}
				{@const overrideRow = limits.find((l) => l.plan_type === card.plan_type)}
				{@const cardAdjustments = adjustments.filter((a) => a.plan_type === card.plan_type)}
				<div class={cardClass}>
					<h2 class="text-lg font-semibold">
						{$_('savings.plan_' + card.plan_type)}
					</h2>
					{#if card.beneficiary !== ''}
						<p class="mt-1 text-sm text-zinc-500 dark:text-zinc-400">
							{$_('savings.beneficiary_label')}: {card.beneficiary}
						</p>
					{/if}
					{#if card.annual_limit !== null}
						<div class="mt-3 h-2 overflow-hidden rounded bg-zinc-200 dark:bg-zinc-800">
							<div
								class={'h-2 rounded ' +
									(card.room !== null && Number(card.room) < 0 ? 'bg-red-500' : 'bg-emerald-500')}
								style="width: {Math.min(
									100,
									(Number(card.used) / Number(card.annual_limit)) * 100
								)}%"
							></div>
						</div>
					{/if}
					<div class="mt-3 space-y-1.5">
						<div class="flex items-baseline justify-between gap-3">
							<span class="text-sm text-zinc-500 dark:text-zinc-400">
								{$_('savings.contributed_label', { values: { year: String(year) } })}
							</span>
							<span class="text-sm">{card.used}</span>
						</div>
						<div class="flex items-baseline justify-between gap-3">
							<span class="text-sm text-zinc-500 dark:text-zinc-400">
								{$_('savings.limit_label')}
							</span>
							<span class="text-sm">
								{card.annual_limit !== null ? card.annual_limit : $_('savings.no_limit')}
							</span>
						</div>
						{#if card.room !== null}
							<div class="flex items-baseline justify-between gap-3">
								<span class="text-sm text-zinc-500 dark:text-zinc-400">
									{$_('savings.room_label')}
								</span>
								<span
									class={Number(card.room) < 0
										? 'text-sm text-red-600 dark:text-red-400'
										: 'text-sm'}
								>
									{card.room}
								</span>
							</div>
						{/if}
						{#if Number(card.re_created_next_year) > 0}
							<div class="flex items-baseline justify-between gap-3">
								<span class="text-sm text-zinc-500 dark:text-zinc-400">
									{$_('savings.re_created_label')}
								</span>
								<span class="text-sm">{card.re_created_next_year}</span>
							</div>
						{/if}
						{#if card.lifetime_limit !== null}
							<div class="flex items-baseline justify-between gap-3">
								<span class="text-sm text-zinc-500 dark:text-zinc-400">
									{$_('savings.lifetime_limit_label')}
								</span>
								<span class="text-sm">{card.lifetime_limit}</span>
							</div>
						{/if}
						{#if card.lifetime_used !== null}
							<div class="flex items-baseline justify-between gap-3">
								<span class="text-sm text-zinc-500 dark:text-zinc-400">
									{$_('savings.lifetime_used_label')}
								</span>
								<span class="text-sm">{card.lifetime_used}</span>
							</div>
						{/if}
						{#if card.lifetime_room !== null}
							<div class="flex items-baseline justify-between gap-3">
								<span class="text-sm text-zinc-500 dark:text-zinc-400">
									{$_('savings.lifetime_room_label')}
								</span>
								<span
									class={Number(card.lifetime_room) < 0
										? 'text-sm text-red-600 dark:text-red-400'
										: 'text-sm'}
								>
									{card.lifetime_room}
								</span>
							</div>
						{/if}
						{#if card.rrif_minimum !== null}
							<div class="flex items-baseline justify-between gap-3">
								<span class="text-sm text-zinc-500 dark:text-zinc-400">
									{$_('savings.rrif_minimum_label')}
								</span>
								<span class="text-sm">{card.rrif_minimum}</span>
							</div>
							<p class="text-xs text-zinc-500 dark:text-zinc-400">
								{$_('savings.rrif_minimum_hint')}
							</p>
						{/if}
					</div>
					{#if card.warnings.length > 0}
						<div class="mt-3 border-t border-zinc-200 dark:border-zinc-800 pt-3">
							<p class="text-sm font-medium text-amber-600 dark:text-amber-400">
								{$_('savings.warnings_title')}
							</p>
							<ul class="mt-1 space-y-1">
								{#each card.warnings as code (code)}
									<li class="text-sm text-amber-600 dark:text-amber-400">
										{warnText(code)}
									</li>
								{/each}
							</ul>
						</div>
					{/if}
					{#if card.notes.length > 0}
						<div class="mt-3 border-t border-zinc-200 dark:border-zinc-800 pt-3">
							<p class="text-sm font-medium text-zinc-600 dark:text-zinc-400">
								{$_('savings.notes_title')}
							</p>
							<ul class="mt-1 space-y-1">
								{#each card.notes as code (code)}
									<li class="text-sm text-zinc-600 dark:text-zinc-400">
										{noteText(code)}
									</li>
								{/each}
							</ul>
						</div>
					{/if}
					{#if card.plan_type === 'rrsp' || card.plan_type === 'tfsa' || card.plan_type === 'fhsa'}
						<div class="mt-3 border-t border-zinc-200 dark:border-zinc-800 pt-3">
							<p class="text-xs font-medium text-zinc-500 dark:text-zinc-400 uppercase">
								{$_('savings.override_title')}
							</p>
							<p class="mt-1 text-xs text-zinc-500 dark:text-zinc-400">
								{$_('savings.override_hint')}
							</p>
							<div class="mt-2 flex flex-wrap items-center gap-2">
								<input
									type="text"
									inputmode="decimal"
									bind:value={limitInputs[card.plan_type]}
									placeholder={$_('savings.override_placeholder')}
									class={inputClass + ' w-24'}
								/>
								<button
									class={buttonClass}
									disabled={limitBusy === card.plan_type}
									onclick={() => void saveLimit(card.plan_type)}
								>
									{limitBusy === card.plan_type
										? $_('savings.override_saving')
										: $_('savings.override_save')}
								</button>
								{#if card.limit_source === 'override'}
									<button
										class={buttonClass}
										disabled={limitBusy === card.plan_type}
										onclick={() => void removeLimit(card.plan_type)}
									>
										{$_('savings.override_remove')}
									</button>
								{/if}
							</div>
							{#if limitErrors[card.plan_type]}
								<p class="mt-2 text-sm text-red-600 dark:text-red-400">
									{limitErrors[card.plan_type]}
								</p>
							{/if}
						</div>
					{/if}
					<div class="mt-3 border-t border-zinc-200 dark:border-zinc-800 pt-3">
						<p class="text-xs font-medium text-zinc-500 dark:text-zinc-400 uppercase">
							{$_('savings.adjustments_title')}
						</p>
						<p class="mt-1 text-xs text-zinc-500 dark:text-zinc-400">
							{$_('savings.adjustments_hint')}
						</p>
						{#if cardAdjustments.length === 0}
							<p class="mt-2 text-sm text-zinc-600 dark:text-zinc-400">
								{$_('savings.adjustments_empty')}
							</p>
						{:else}
							<ul class="mt-2 space-y-1">
								{#each cardAdjustments as adj (adj.id)}
									<li class="flex items-baseline justify-between gap-3">
										<span>
											<span class="text-sm tabular-nums">{adj.amount}</span>
											{#if adj.note !== ''}
												<span class="text-xs text-zinc-500 dark:text-zinc-400">
													— {adj.note}
												</span>
											{/if}
										</span>
										<button
											class={linkClass}
											disabled={adjBusy === card.plan_type}
											onclick={() => void removeAdjustment(adj.id, card.plan_type)}
										>
											{$_('savings.adjustment_delete')}
										</button>
									</li>
								{/each}
							</ul>
						{/if}
						<div class="mt-2 flex flex-wrap items-center gap-2">
							<input
								type="text"
								inputmode="decimal"
								bind:value={amountInputs[card.plan_type]}
								placeholder={$_('savings.adjustment_amount_placeholder')}
								class={inputClass + ' w-24'}
							/>
							<input
								type="text"
								bind:value={noteInputs[card.plan_type]}
								maxlength={500}
								placeholder={$_('savings.adjustment_note_placeholder')}
								class={inputClass}
							/>
							<button
								class={buttonClass}
								disabled={adjBusy === card.plan_type}
								onclick={() => void addAdjustment(card.plan_type)}
							>
								{adjBusy === card.plan_type
									? $_('savings.adjustment_adding')
									: $_('savings.adjustment_add')}
							</button>
						</div>
						{#if adjErrors[card.plan_type]}
							<p class="mt-2 text-sm text-red-600 dark:text-red-400">
								{adjErrors[card.plan_type]}
							</p>
						{/if}
					</div>
				</div>
			{/each}
		</div>

		<div class={cardClass}>
			<h2 class="text-lg font-semibold">{$_('savings.plan_accounts_title')}</h2>
			<p class="mt-1 text-sm text-zinc-600 dark:text-zinc-400">
				{$_('savings.plan_accounts_hint')}
			</p>
			{#if planAccounts.length === 0}
				<p class="mt-3 text-sm text-zinc-600 dark:text-zinc-400">
					{$_('savings.no_plan_accounts')}
				</p>
			{:else}
				<ul class="mt-3 space-y-1.5">
					{#each planAccounts as a (a.id)}
						<li>
							<!-- /savings/[id] lands in a later round; the link
								     can switch to resolve('/savings/[accountId]',
								     { accountId }) once the route is typed. -->
							<a href={'/savings/' + a.id} class={linkClass}>
								{a.name}
							</a>
						</li>
					{/each}
				</ul>
			{/if}
		</div>

		{#if birthNote}
			<p class="text-sm text-emerald-600 dark:text-emerald-400">{birthNote}</p>
		{/if}
		{#if actionNote}
			<p class="text-sm text-emerald-600 dark:text-emerald-400">{actionNote}</p>
		{/if}
	{/if}
</section>

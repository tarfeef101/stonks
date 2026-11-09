<script lang="ts">
	import { resolve } from '$app/paths';
	import { onMount } from 'svelte';
	import { _ } from 'svelte-i18n';
	import { user } from '#lib/auth/client';
	import { ApiError } from '#lib/api/client';
	import {
		analyzeImport,
		createImportSchema,
		listAccounts,
		listImportPresets,
		previewImport,
		setImportMapping,
		uploadImport,
		type Account,
		type AnalyzeResult,
		type ImportJob,
		type ImportMapping,
		type ImportPreset,
		type PreviewResult
	} from '#lib/api/imports';
	import ManualEntry from '#lib/components/ManualEntry.svelte';

	// Import mapping (step 4.5): upload a statement file, analyze it
	// (the backend matches a preset from the filename/headers and
	// suggests field mappings), adjust the mapping, preview the mapped
	// rows live, optionally save the mapping as a preset, and confirm —
	// the mapping is stored on the job (the run itself is step 4.6).

	// The 6 canonical fields (csvparse.Fields), in order — the mapping
	// table rows and the preview columns.
	const FIELDS = [
		'date_executed',
		'date_posted',
		'amount',
		'description',
		'category',
		'running_balance'
	] as const;
	type FieldName = (typeof FIELDS)[number];

	// Upload stage. No account is auto-selected: the picker must be
	// chosen explicitly before a file can be uploaded.
	let accounts = $state<Account[]>([]);
	let presets = $state<ImportPreset[]>([]);
	let loadError = $state<string | null>(null);
	let accountId = $state('');
	let fileInput = $state<HTMLInputElement | null>(null);
	let file = $state<File | null>(null);
	let dragOver = $state(false);
	let uploading = $state(false);
	let analyzing = $state(false);
	let error = $state<string | null>(null);

	// Analyze stage.
	let job = $state<ImportJob | null>(null);
	let analysis = $state<AnalyzeResult | null>(null);
	let presetId = $state(''); // '' = the automatic filename/header match

	// Mapping edits (per kind).
	let indexByField = $state<Partial<Record<FieldName, number>>>({});
	let pointersByField = $state<Partial<Record<FieldName, string[]>>>({});
	let arrayPointersText = $state('');

	// Live preview (debounced on the mapping document).
	let preview = $state<PreviewResult | null>(null);
	let previewing = $state(false);
	let previewError = $state<string | null>(null);
	let previewSeq = 0; // drops stale responses
	let previewTimer: ReturnType<typeof setTimeout> | null = null;

	// Save-as-preset.
	let presetName = $state('');
	let savingPreset = $state(false);
	let presetSaved = $state(false);
	let presetError = $state<string | null>(null);

	// Confirm.
	let confirming = $state(false);
	let confirmed = $state(false);

	/** The current mapping document, derived from the table state.
	 * CSV: all 6 fields with an explicit index (-1 = unmapped).
	 * JSON: only the fields with a non-empty pointer; array_pointers
	 * parsed from the text ('(root)' = the root pointer "", blank =
	 * omitted = document root). OFX: the identity. */
	const mapping = $derived<ImportMapping | null>(buildMapping());

	/** The confirm button is enabled only when at least one field is
	 * mapped (OFX is always mapped — its fields are fixed). */
	const hasMappedFields = $derived(
		analysis === null ? false : analysis.kind === 'ofx' ? true : mappedFieldCount() > 0
	);

	/** Presets of the analyzed kind (the select options; the select
	 * only renders once a job is analyzed, so the fallback list is
	 * never shown — it just keeps the null case typed). */
	const kindPresets = $derived.by(() => {
		const a = analysis;
		return a ? presets.filter((p) => p.kind === a.kind) : [];
	});

	function mappedFieldCount(): number {
		if (!analysis) return 0;
		if (analysis.kind === 'csv') {
			return FIELDS.filter((f) => (indexByField[f] ?? -1) >= 0).length;
		}
		if (analysis.kind === 'json') {
			return FIELDS.filter((f) => (pointersByField[f]?.length ?? 0) > 0).length;
		}
		return 1; // ofx
	}

	/** Parses the array-pointers text: comma-joined, '(root)' (any
	 * case) = the root pointer "", blank = undefined (root only). */
	function parseArrayPointers(): string[] | undefined {
		const list = arrayPointersText
			.split(',')
			.map((s) => s.trim())
			.filter((s) => s !== '')
			.map((s) => (s.toLowerCase() === '(root)' ? '' : s));
		return list.length > 0 ? list : undefined;
	}

	function buildMapping(): ImportMapping | null {
		if (!analysis) return null;
		if (analysis.kind === 'ofx') return { kind: 'ofx', map: {} };
		if (analysis.kind === 'csv') {
			const map: ImportMapping['map'] = {};
			for (const f of FIELDS) map[f] = { index: indexByField[f] ?? -1 };
			return { kind: 'csv', map };
		}
		const map: ImportMapping['map'] = {};
		for (const f of FIELDS) {
			const p = pointersByField[f];
			if (p && p.length > 0) map[f] = { pointers: [...p] };
		}
		const ap = parseArrayPointers();
		return ap ? { kind: 'json', array_pointers: ap, map } : { kind: 'json', map };
	}

	/** Fills the table state from the analyze suggestions. CSV: index
	 * (>= 0 only — the suggestions are already header-resolved).
	 * JSON: each field keeps ALL suggested pointers; editing a field
	 * later replaces them with exactly the typed value. presetId is
	 * untouched — the matched preset is displayed from analysis.preset. */
	function applySuggested(a: AnalyzeResult): void {
		if (a.kind === 'csv') {
			const next: Partial<Record<FieldName, number>> = {};
			for (const f of FIELDS) {
				const i = a.suggested.map[f]?.index;
				if (i !== undefined && i >= 0) next[f] = i;
			}
			indexByField = next;
		} else if (a.kind === 'json') {
			const next: Partial<Record<FieldName, string[]>> = {};
			for (const f of FIELDS) {
				next[f] = [...(a.suggested.map[f]?.pointers ?? [])];
			}
			pointersByField = next;
			arrayPointersText = (a.suggested.array_pointers ?? [])
				.map((p) => (p === '' ? '(root)' : p))
				.join(', ');
		}
	}

	function resetWorkflow(): void {
		previewSeq++; // drop in-flight previews
		job = null;
		analysis = null;
		indexByField = {};
		pointersByField = {};
		arrayPointersText = '';
		presetId = '';
		preview = null;
		previewing = false;
		previewError = null;
		presetName = '';
		presetSaved = false;
		presetError = null;
		confirming = false;
		confirmed = false;
		file = null;
		error = null;
	}

	/** Uploads the file and analyzes it; the table is pre-filled from
	 * the suggestions. Any failure (upload, analyze) resets the
	 * workflow — the user re-uploads. */
	async function handleFile(f: File): Promise<void> {
		if (!accountId) return; // the drop zone is inert without an account
		resetWorkflow();
		file = f;
		uploading = true;
		try {
			job = await uploadImport(accountId, f);
			analyzing = true;
			analysis = await analyzeImport(job.id, presetId || undefined);
			applySuggested(analysis);
		} catch (e) {
			error = envelopeMessage(e);
			job = null;
			analysis = null;
		} finally {
			uploading = false;
			analyzing = false;
		}
	}

	/** Re-analyzes with the selected preset (empty = the automatic
	 * filename/header match); the table is re-filled from the new
	 * suggestions. */
	async function changePreset(id: string): Promise<void> {
		presetId = id;
		if (!job || !analysis) return;
		analyzing = true;
		try {
			analysis = await analyzeImport(job.id, id || undefined);
			applySuggested(analysis);
		} catch (e) {
			error = envelopeMessage(e);
		} finally {
			analyzing = false;
		}
	}

	async function savePreset(): Promise<void> {
		if (!analysis || !mapping || !presetName.trim()) return;
		savingPreset = true;
		presetError = null;
		presetSaved = false;
		try {
			await createImportSchema(presetName.trim(), analysis.kind, mapping);
			presetSaved = true;
		} catch (e) {
			presetError = envelopeMessage(e);
		} finally {
			savingPreset = false;
		}
	}

	async function confirmMapping(): Promise<void> {
		if (!job || !mapping || !hasMappedFields || confirming) return;
		confirming = true;
		error = null;
		try {
			job = await setImportMapping(job.id, mapping);
			confirmed = true;
		} catch (e) {
			error = envelopeMessage(e);
		} finally {
			confirming = false;
		}
	}

	function startAnother(): void {
		resetWorkflow();
		if (fileInput) fileInput.value = '';
	}

	/** One path input shows the first pointer; typing replaces the
	 * field's pointers with exactly [the typed value] (blank =
	 * unmapped -> []). */
	function setPointer(f: FieldName, value: string): void {
		const v = value.trim();
		pointersByField = { ...pointersByField, [f]: v === '' ? [] : [v] };
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

	// Live preview: 400 ms debounce on the derived mapping document
	// (limit 10), stale responses dropped by the seq counter. Skipped
	// — with a hint instead of a 400 — while no field is mapped.
	$effect(() => {
		if (!job || !analysis || confirmed || !mapping || !hasMappedFields) {
			if (previewTimer) clearTimeout(previewTimer);
			previewTimer = null;
			preview = null;
			previewing = false;
			previewError = null;
			return;
		}
		previewing = true;
		previewError = null;
		const jobId = job.id; // capture: job is $state, narrowing doesn't survive the closure
		previewTimer = setTimeout(() => {
			void runPreview(jobId, mapping);
		}, 400);
		return () => {
			if (previewTimer) clearTimeout(previewTimer);
		};
	});

	async function runPreview(id: string, m: ImportMapping): Promise<void> {
		const seq = ++previewSeq;
		try {
			const p = await previewImport(id, m, 10);
			if (seq !== previewSeq) return;
			preview = p;
			previewError = null;
		} catch (e) {
			if (seq !== previewSeq) return;
			preview = null;
			previewError = envelopeMessage(e);
		} finally {
			if (seq === previewSeq) previewing = false;
		}
	}

	// The account picker and the preset select data. (The layout
	// restores the session reactively; a signed-out load simply 401s
	// and the upload card stays hidden behind the sign-in prompt.)
	onMount(() => {
		void Promise.all([listAccounts(), listImportPresets()])
			.then(([accs, prs]) => {
				accounts = accs;
				presets = prs;
			})
			.catch((e) => {
				loadError = envelopeMessage(e);
			});
	});
</script>

<section class="space-y-3">
	<h1 class="text-2xl font-semibold">{$_('imports.title')}</h1>
	<p class="max-w-2xl text-sm text-zinc-600 dark:text-zinc-400">{$_('imports.intro')}</p>

	{#if !$user}
		<p class="text-sm text-zinc-600 dark:text-zinc-400">
			{$_('imports.sign_in_prompt')}
			<a class="underline" href={resolve('/')}>{$_('auth.sign_in')}</a>
		</p>
	{:else}
		<!-- Manual entry (4.7): its own section above the import flow. -->
		<ManualEntry />

		<div class="space-y-4 rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
			<div class="flex flex-wrap items-end gap-4">
				<label class="block text-sm">
					<span class="mb-1 block font-medium">{$_('imports.account_label')}</span>
					<select bind:value={accountId} class="w-64 rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5">
						<option value="">{$_('imports.account_placeholder')}</option>
						{#each accounts as a (a.id)}
							<option value={a.id}>{a.name} ({a.currency})</option>
						{/each}
					</select>
				</label>
				{#if accounts.length === 0 && !loadError}
					<p class="pb-1.5 text-xs text-zinc-500 dark:text-zinc-400">{$_('imports.no_accounts')}</p>
				{/if}
			</div>

			<!-- svelte-ignore a11y_no_static_element_interactions -->
			<div
				class="rounded-md border-2 border-dashed p-6 text-center text-sm text-zinc-500 dark:text-zinc-400 {dragOver
					? 'border-zinc-400 bg-zinc-50 dark:bg-zinc-950'
					: 'border-zinc-200 dark:border-zinc-800'}"
				ondragover={(e) => {
					e.preventDefault();
					if (accountId) dragOver = true;
				}}
				ondragleave={() => {
					dragOver = false;
				}}
				ondrop={(e) => {
					e.preventDefault();
					dragOver = false;
					const f = e.dataTransfer?.files?.[0];
					if (f) void handleFile(f);
				}}
			>
				{$_('imports.drop_hint')}
				<br />
				<span class="text-xs text-zinc-400">{$_('imports.supported')}</span>
				<label
					class="mt-3 inline-block cursor-pointer rounded-md bg-zinc-900 px-4 py-2 text-sm font-medium text-white hover:bg-zinc-700 {accountId ===
					''
						? 'pointer-events-none opacity-50'
						: ''}"
				>
					{$_('imports.drop_browse')}
					<input
						bind:this={fileInput}
						class="hidden"
						onchange={(e) => {
							const f = e.currentTarget.files?.[0];
							if (f) void handleFile(f);
							e.currentTarget.value = '';
						}}
						type="file"
					/>
				</label>
			</div>

			{#if file}
				<p class="text-sm">
					{$_('imports.file_label')}:
					<span class="font-medium text-zinc-900 dark:text-zinc-100">{file.name}</span>
				</p>
			{/if}
			{#if loadError}
				<p class="text-sm text-red-600 dark:text-red-400">{loadError}</p>
			{/if}
			{#if uploading}
				<p class="text-sm text-zinc-600 dark:text-zinc-400">{$_('imports.uploading')}</p>
			{/if}
			{#if analyzing}
				<p class="text-sm text-zinc-600 dark:text-zinc-400">{$_('imports.analyzing')}</p>
			{/if}
			{#if error}
				<p class="text-sm text-red-600 dark:text-red-400">{error}</p>
			{/if}
		</div>

		{#if job && analysis}
			{#if confirmed}
				<div class="space-y-3 rounded-lg border border-green-200 dark:border-green-900 bg-green-50 dark:bg-green-950 p-5">
					<h2 class="text-lg font-medium text-green-900 dark:text-green-300">{$_('imports.confirmed_title')}</h2>
					<p class="text-sm text-green-800 dark:text-green-300">
						{$_('imports.confirmed_note')}
						<span class="font-medium">{job.file_name}</span>
					</p>
					<button
						class="rounded-md bg-zinc-900 px-4 py-2 text-sm font-medium text-white hover:bg-zinc-700"
						type="button"
						onclick={() => startAnother()}
					>
						{$_('imports.another')}
					</button>
				</div>
			{:else}
				<div class="space-y-3 rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
					{#if analysis.preset}
						<p class="text-sm text-zinc-600 dark:text-zinc-400">
							{$_('imports.matched_preset')}:
							<span class="font-medium text-zinc-900 dark:text-zinc-100">{analysis.preset.name}</span>
						</p>
					{/if}
					<label class="block text-sm">
						<span class="mb-1 block font-medium">{$_('imports.preset_label')}</span>
						<select
							bind:value={presetId}
							class="w-72 rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
							onchange={() => void changePreset(presetId)}
						>
							<option value="">{$_('imports.preset_auto')}</option>
							{#each kindPresets as p (p.id)}
								<!-- "(saved)" marks the user's own presets against the code builtins. -->
								<option value={p.id}>
									{p.name}{p.bank !== '' ? ` (${p.bank})` : ''}{p.builtin
										? ''
										: ` ${$_('imports.saved')}`}
								</option>
							{/each}
						</select>
					</label>
				</div>

				<div class="space-y-3 rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
					<h2 class="text-lg font-medium">{$_('imports.mapping_title')}</h2>
					{#if analysis.kind === 'csv'}
						<p class="text-xs text-zinc-500 dark:text-zinc-400">{$_('imports.mapping_csv_note')}</p>
						<table class="w-full text-sm">
							<thead>
								<tr class="border-b border-zinc-200 dark:border-zinc-800 text-left text-xs text-zinc-500 dark:text-zinc-400 uppercase">
									<th class="py-1.5 pr-4">{$_('imports.field_label')}</th>
									<th class="py-1.5">{$_('imports.column_label')}</th>
								</tr>
							</thead>
							<tbody>
								{#each FIELDS as f (f)}
									<tr class="border-b border-zinc-100 dark:border-zinc-800">
										<td class="py-2 pr-4 font-medium">{$_(`imports.field_${f}`)}</td>
										<td class="py-2">
											<select
												class="w-full rounded-md border border-zinc-300 dark:border-zinc-700 px-2 py-1"
												value={String(indexByField[f] ?? -1)}
												onchange={(e) => {
													indexByField = {
														...indexByField,
														[f]: Number(e.currentTarget.value)
													};
												}}
											>
												<option value="-1">{$_('imports.unmapped')}</option>
												{#each analysis.columns ?? [] as c (c.index)}
													<option value={c.index}>
														{c.index}. {c.heading}{c.kind !== 'unknown' ? ` (${c.kind})` : ''}
													</option>
												{/each}
											</select>
										</td>
									</tr>
								{/each}
							</tbody>
						</table>
					{:else if analysis.kind === 'json'}
						<p class="text-xs text-zinc-500 dark:text-zinc-400">{$_('imports.mapping_json_note')}</p>
						<label class="block text-sm">
							<span class="mb-1 block font-medium">{$_('imports.array_pointers_label')}</span>
							<input
								bind:value={arrayPointersText}
								class="w-full rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
								placeholder={$_('imports.array_pointers_placeholder')}
								type="text"
							/>
							<span class="mt-1 block text-xs text-zinc-500 dark:text-zinc-400">
								{$_('imports.array_pointers_note')}
							</span>
						</label>
						<table class="mt-4 w-full text-sm">
							<thead>
								<tr class="border-b border-zinc-200 dark:border-zinc-800 text-left text-xs text-zinc-500 dark:text-zinc-400 uppercase">
									<th class="py-1.5 pr-4">{$_('imports.field_label')}</th>
									<th class="py-1.5">{$_('imports.column_label')}</th>
								</tr>
							</thead>
							<tbody>
								{#each FIELDS as f (f)}
									<tr class="border-b border-zinc-100 dark:border-zinc-800">
										<td class="py-2 pr-4 font-medium">{$_(`imports.field_${f}`)}</td>
										<td class="py-2">
											<input
												class="w-full rounded-md border border-zinc-300 dark:border-zinc-700 px-2 py-1"
												onchange={(e) => {
													setPointer(f, e.currentTarget.value);
												}}
												placeholder={$_('imports.pointer_placeholder')}
												type="text"
												value={pointersByField[f]?.[0] ?? ''}
											/>
										</td>
									</tr>
								{/each}
							</tbody>
						</table>
						{#if analysis.keys && analysis.keys.length > 0}
							<p class="text-xs text-zinc-500 dark:text-zinc-400">
								{$_('imports.json_keys')}: {analysis.keys.join(', ')}
							</p>
						{/if}
					{:else}
						<p class="text-sm text-zinc-600 dark:text-zinc-400">{$_('imports.ofx_note')}</p>
					{/if}
					<button
						class="mt-2 rounded-md bg-zinc-900 px-4 py-2 text-sm font-medium text-white hover:bg-zinc-700 disabled:opacity-50"
						disabled={!hasMappedFields || confirming || analyzing}
						type="button"
						onclick={() => void confirmMapping()}
					>
						{confirming ? $_('imports.confirming') : $_('imports.confirm')}
					</button>
				</div>

				<div class="space-y-3 rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
					<h2 class="text-lg font-medium">{$_('imports.preview_title')}</h2>
					{#if !hasMappedFields}
						<p class="text-sm text-zinc-500 dark:text-zinc-400">{$_('imports.preview_empty')}</p>
					{:else}
						{#if previewing}
							<p class="text-sm text-zinc-500 dark:text-zinc-400">{$_('imports.previewing')}</p>
						{/if}
						{#if previewError}
							<p class="text-sm text-red-600 dark:text-red-400">{previewError}</p>
						{/if}
						{#if preview}
							<div class="overflow-x-auto">
								<table class="w-full text-sm">
									<thead>
										<tr class="border-b border-zinc-200 dark:border-zinc-800 text-left text-xs text-zinc-500 dark:text-zinc-400 uppercase">
											{#each preview.columns as col (col)}
												<th class="py-1.5 pr-4">{col}</th>
											{/each}
										</tr>
									</thead>
									<tbody>
										{#each preview.rows as row, i (i)}
											<tr class="border-b border-zinc-100 dark:border-zinc-800">
												{#each row as cell, j (j)}
													<td class="py-1.5 pr-4">{cell}</td>
												{/each}
											</tr>
										{/each}
									</tbody>
								</table>
							</div>
							<p class="text-xs text-zinc-500 dark:text-zinc-400">
								{$_('imports.preview_rows', {
									values: { n: preview.rows.length, total: preview.total }
								})}
							</p>
						{/if}
					{/if}
				</div>

				{#if analysis.kind !== 'ofx'}
					<form
						class="space-y-3 rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5"
						onsubmit={(e) => {
							e.preventDefault();
							void savePreset();
						}}
					>
						<h2 class="text-lg font-medium">{$_('imports.save_preset_title')}</h2>
						<p class="text-xs text-zinc-500 dark:text-zinc-400">{$_('imports.save_preset_hint')}</p>
						<label class="block text-sm">
							<span class="mb-1 block font-medium">{$_('imports.save_preset_name')}</span>
							<input
								bind:value={presetName}
								class="w-72 rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
								required
								type="text"
							/>
						</label>
						{#if presetError}
							<p class="text-sm text-red-600 dark:text-red-400">{presetError}</p>
						{/if}
						{#if presetSaved}
							<p class="text-sm text-green-600 dark:text-green-400">{$_('imports.preset_saved')}</p>
						{/if}
						<button
							class="rounded-md bg-zinc-900 px-4 py-2 text-sm font-medium text-white hover:bg-zinc-700 disabled:opacity-50"
							disabled={savingPreset || confirming}
							type="submit"
						>
							{savingPreset ? $_('imports.saving_preset') : $_('imports.save_preset')}
						</button>
					</form>
				{/if}
			{/if}
		{/if}
	{/if}
</section>

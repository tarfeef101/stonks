<script lang="ts">
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import { onMount } from 'svelte';
	import { _ } from 'svelte-i18n';
	import { login, user } from '#lib/auth/client';
	import { ApiError } from '#lib/api/client';
	import { listAccounts, type Account } from '#lib/api/accounts';
	import { listCategories, type Category } from '#lib/api/categories';
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
	import {
		acceptAllReview,
		acceptReview,
		listReviewQueue,
		rejectReview,
		type ReviewQueueRow
	} from '#lib/api/review';
	import {
		listTransactions,
		patchTransaction,
		type PatchTransactionInput,
		type Transaction
	} from '#lib/api/transactions';

	// Round 33: alerts are actionable in place. A `review` alert opens
	// the whole review queue in a modal (apply per row, reject, edit,
	// apply-all); a `mortgage_aberrant` alert whose ref parses opens its
	// single item in a modal (one-off / rate change / reject for an
	// extra or short, accept-implied / tweak / reject for a suggestion).
	// Answering resolves the alert: the endpoint drops the notification
	// row and the backend's self-heal keeps the list current. Other kinds
	// (errors, summarise alerts) stay plain cards. The shell's badge
	// refreshes after every in-place change via `ftrk:unread-changed`.

	// --- Page state ------------------------------------------------------

	let items = $state<Notification[]>([]);
	let loadError = $state<string | null>(null);
	let loading = $state(true);

	/** Accounts and categories, loaded once (silently) so the review
	 * modal can label rows and its edit form can offer every value. */
	let accounts = $state<Account[]>([]);
	let categories = $state<Category[]>([]);

	/** Only active categories can be assigned (mirrors the review
	 * page). */
	const activeCategories = $derived(categories.filter((c) => c.active));

	/** Labels an account id for the modal's table and edit form. */
	function accountLabel(id: string): string {
		return accounts.find((a) => a.id === id)?.name ?? id;
	}

	/** Replaces the marked row with the 200 response's updated row
	 * (read_at is now set) and lets the shell badge know. */
	async function markRead(id: string): Promise<void> {
		try {
			const updated = await markNotificationRead(id);
			items = items.map((n) => (n.id === id ? updated : n));
			dispatchUnreadChanged();
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
					// not JSON after all — fall through to the raw message
				}
			}
			return e.message;
		}
		return String(e);
	}

	function readAtLabel(iso: string): string {
		return new Date(iso).toLocaleString();
	}

	/** Re-lists the caller's notifications; a stale loadError is
	 * cleared on success so it cannot outlive the mutation that
	 * caused it. */
	async function reload(): Promise<void> {
		try {
			items = await listNotifications();
			loadError = null;
		} catch (e) {
			loadError = envelopeMessage(e);
		} finally {
			loading = false;
		}
	}

	/** The shell listens for this to re-fetch the unread count after
	 * any in-place change here. */
	function dispatchUnreadChanged(): void {
		window.dispatchEvent(new CustomEvent('ftrk:unread-changed'));
	}

	// --- Which cards are actionable --------------------------------------

	/** The ref JSON the backend writes for a mortgage_aberrant alert
	 * (the backend pins the exact keys); null when it does not parse. */
	interface AberrantRef {
		mortgageId: string;
		kind: 'extra' | 'short' | 'suggestion';
		date: string;
	}

	/** Parses a card's `ref` (a JSON object on the wire) into the shape
	 * above; any missing or unknown field makes the card plain. */
	function parseAberrantRef(n: Notification): AberrantRef | null {
		if (n.ref === null || typeof n.ref !== 'object') return null;
		const raw = n.ref as Record<string, unknown>;
		const mortgageId = raw.mortgage_id;
		const kind = raw.kind;
		const date = raw.date;
		if (
			typeof mortgageId !== 'string' ||
			mortgageId === '' ||
			typeof date !== 'string' ||
			date === '' ||
			(kind !== 'extra' && kind !== 'short' && kind !== 'suggestion')
		) {
			return null;
		}
		return { mortgageId, kind, date };
	}

	/** Review alerts and parseable aberrant alerts open a modal;
	 * everything else stays a plain card. */
	function isActionable(n: Notification): boolean {
		if (n.kind === 'review') return true;
		if (n.kind === 'mortgage_aberrant') return parseAberrantRef(n) !== null;
		return false;
	}
	// --- The review modal (one at a time; holds the whole queue) --------

	/** The edit form's fields, all strings; '' means "clear this
	 * optional column" for the nullable ones. */
	interface EditForm {
		accountId: string;
		categoryId: string;
		datePosted: string;
		dateExecuted: string;
		amount: string;
		description: string;
		notes: string;
	}

	/** Per-row state for the review modal: the picked category, the
	 * in-flight action (if any), and the inline editor. */
	interface ReviewRowState {
		categoryId: string;
		busy: 'apply' | 'reject' | 'save' | null;
		error: string | null;
		editing: boolean;
		editLoading: boolean;
		editError: string | null;
		editSeed: Transaction | null;
		editForm: EditForm | null;
	}

	function defaultRowState(): ReviewRowState {
		return {
			categoryId: '',
			busy: null,
			error: null,
			editing: false,
			editLoading: false,
			editError: null,
			editSeed: null,
			editForm: null
		};
	}

	interface ReviewModalState {
		notification: Notification;
		loading: boolean;
		loadError: string | null;
		rows: ReviewQueueRow[];
		rowState: Record<string, ReviewRowState>;
		allCategoryId: string;
		applyingAll: boolean;
		appliedAll: number | null;
		allError: string | null;
	}

	let reviewModal = $state<ReviewModalState | null>(null);

	/** The modal's own row state (null when the row has no entry yet);
	 * always re-fetched after an await, because a concurrent refresh can
	 * re-seed the map. */
	function rowState(id: string): ReviewRowState | null {
		return reviewModal?.rowState[id] ?? null;
	}

	/** Closes whichever modal is open (Escape, the overlay, or the ×). */
	function closeModals(): void {
		reviewModal = null;
		aberrantModal = null;
	}

	/** Opens the card's modal (one at a time) and kicks off its load. */
	function openNotification(n: Notification): void {
		if (n.kind === 'review') {
			reviewModal = {
				notification: n,
				loading: true,
				loadError: null,
				rows: [],
				rowState: {},
				allCategoryId: '',
				applyingAll: false,
				appliedAll: null,
				allError: null
			};
			aberrantModal = null;
			void refreshReviewModal();
			return;
		}
		const ref = parseAberrantRef(n);
		if (ref === null) return;
		aberrantModal = {
			notification: n,
			ref,
			loading: true,
			loadError: null,
			item: null,
			resolved: null,
			rateType: 'fixed',
			showForm: false,
			form: { annualRate: '', rateType: 'fixed', action: 're_amortize' },
			busy: null,
			error: null
		};
		reviewModal = null;
		void loadAberrantModal();
	}

	/** Re-lists the queue into the open modal, keeping every row's
	 * per-row state (category pick, editor) across the refresh. */
	async function refreshReviewModal(): Promise<void> {
		const target = reviewModal;
		if (target === null) return;
		try {
			const rows = await listReviewQueue();
			if (target !== reviewModal) return;
			target.rows = rows;
			for (const r of rows) {
				if (target.rowState[r.id] === undefined) {
					target.rowState[r.id] = defaultRowState();
				}
			}
			for (const id of Object.keys(target.rowState)) {
				if (!rows.some((r) => r.id === id)) delete target.rowState[id];
			}
			target.loadError = null;
		} catch (e) {
			if (target !== reviewModal) return;
			target.loadError = envelopeMessage(e);
		} finally {
			if (target === reviewModal) target.loading = false;
		}
	}

	/** Accepts one row with the picked category; the row disappears
	 * from the queue (its notification goes with it) and the queue
	 * re-lists itself. */
	async function applyRow(row: ReviewQueueRow): Promise<void> {
		const st = rowState(row.id);
		if (st === null || st.busy !== null || st.categoryId === '') return;
		st.busy = 'apply';
		st.error = null;
		try {
			await acceptReview(row.id, st.categoryId);
			const after = rowState(row.id);
			if (after !== null) after.busy = null;
			dispatchUnreadChanged();
			await refreshReviewModal();
		} catch (e) {
			const after = rowState(row.id);
			if (after !== null) {
				after.busy = null;
				after.error = envelopeMessage(e);
			}
		}
	}

	/** Rejects one row: the transaction stays uncategorized, the queue
	 * row is gone. */
	async function rejectRow(row: ReviewQueueRow): Promise<void> {
		const st = rowState(row.id);
		if (st === null || st.busy !== null) return;
		st.busy = 'reject';
		st.error = null;
		try {
			await rejectReview(row.id);
			const after = rowState(row.id);
			if (after !== null) after.busy = null;
			dispatchUnreadChanged();
			await refreshReviewModal();
		} catch (e) {
			const after = rowState(row.id);
			if (after !== null) {
				after.busy = null;
				after.error = envelopeMessage(e);
			}
		}
	}
	/** Opens (or closes) the row's inline editor. Opening seeds the
	 * form from the live transaction, matched by id among the
	 * account's transactions posted on the row's date. */
	async function toggleEdit(row: ReviewQueueRow): Promise<void> {
		const st = rowState(row.id);
		if (st === null || st.busy !== null) return;
		if (st.editing) {
			st.editing = false;
			st.editLoading = false;
			st.editError = null;
			st.editSeed = null;
			st.editForm = null;
			return;
		}
		st.editing = true;
		st.editLoading = true;
		st.editError = null;
		st.editSeed = null;
		st.editForm = null;
		try {
			const txs = await listTransactions({
				account_id: [row.account_id],
				date_from: row.date_posted,
				date_to: row.date_posted
			});
			const seed = txs.find((t) => t.id === row.id) ?? null;
			const after = rowState(row.id);
			if (after === null) return;
			after.editLoading = false;
			if (seed === null) {
				after.editError = $_('review.edit_not_found');
				after.editing = false;
				return;
			}
			after.editSeed = seed;
			after.editForm = {
				accountId: seed.account_id,
				categoryId: seed.category_id ?? '',
				datePosted: seed.date_posted,
				dateExecuted: seed.date_executed ?? '',
				amount: seed.amount,
				description: seed.description,
				notes: seed.notes ?? ''
			};
		} catch (e) {
			const after = rowState(row.id);
			if (after !== null) {
				after.editLoading = false;
				after.editing = false;
				after.editError = envelopeMessage(e);
			}
		}
	}

	/** The diff-only PATCH body: only fields that changed from the seed
	 * go in; '' clears a nullable column. Null when nothing changed. */
	function buildEditPatch(seed: Transaction, form: EditForm): PatchTransactionInput | null {
		const patch: PatchTransactionInput = {};
		if (form.accountId !== seed.account_id) patch.account_id = form.accountId;
		const seedCategory = seed.category_id ?? '';
		if (form.categoryId !== seedCategory) patch.category_id = form.categoryId;
		if (form.amount.trim() !== seed.amount) patch.amount = form.amount.trim();
		if (form.description.trim() !== seed.description) {
			patch.description = form.description.trim();
		}
		const seedNotes = seed.notes ?? '';
		if (form.notes.trim() !== seedNotes) patch.notes = form.notes.trim();
		if (form.datePosted !== seed.date_posted) patch.date_posted = form.datePosted;
		const seedExecuted = seed.date_executed ?? '';
		if (form.dateExecuted !== seedExecuted) patch.date_executed = form.dateExecuted;
		return Object.keys(patch).length === 0 ? null : patch;
	}

	/** True when the open editor has at least one changed field (the
	 * Save button's gate). */
	function editHasDiff(st: ReviewRowState): boolean {
		return (
			st.editSeed !== null &&
			st.editForm !== null &&
			buildEditPatch(st.editSeed, st.editForm) !== null
		);
	}

	/** Saves the edited transaction; the queue row itself is untouched
	 * (it stays pending — only the transaction's fields change). */
	async function saveEdit(row: ReviewQueueRow): Promise<void> {
		const st = rowState(row.id);
		if (st === null || st.busy !== null || st.editSeed === null || st.editForm === null) {
			return;
		}
		const patch = buildEditPatch(st.editSeed, st.editForm);
		if (patch === null) return;
		st.busy = 'save';
		st.editError = null;
		try {
			await patchTransaction(st.editSeed.id, patch);
			const after = rowState(row.id);
			if (after !== null) {
				after.busy = null;
				after.editing = false;
				after.editSeed = null;
				after.editForm = null;
			}
			dispatchUnreadChanged();
			await refreshReviewModal();
		} catch (e) {
			const after = rowState(row.id);
			if (after !== null) {
				after.busy = null;
				after.editError = envelopeMessage(e);
			}
		}
	}

	/** Applies the picked category to every remaining row. */
	async function applyAllRows(): Promise<void> {
		const m = reviewModal;
		if (m === null || m.applyingAll || m.allCategoryId === '') return;
		m.applyingAll = true;
		m.allError = null;
		try {
			const res = await acceptAllReview(m.allCategoryId);
			if (m !== reviewModal) return;
			m.appliedAll = res.applied;
			dispatchUnreadChanged();
			await refreshReviewModal();
		} catch (e) {
			if (m !== reviewModal) return;
			m.allError = envelopeMessage(e);
		} finally {
			if (m === reviewModal) m.applyingAll = false;
		}
	}

	// The modal's selects (and date inputs) take plain values (house
	// pattern: value + onchange, no bind:); the text-like inputs use
	// bind:value like every other form in the app.
	function setRowCategoryId(row: ReviewQueueRow, value: string): void {
		const st = rowState(row.id);
		if (st === null) return;
		st.categoryId = value;
		st.error = null;
	}

	function setAllCategory(value: string): void {
		const m = reviewModal;
		if (m === null) return;
		m.allCategoryId = value;
		m.allError = null;
	}

	function setEditField(row: ReviewQueueRow, field: keyof EditForm, value: string): void {
		const st = rowState(row.id);
		if (st === null || st.editForm === null) return;
		st.editForm[field] = value;
	}
	// --- The aberrant modal (the single alerted item) -------------------

	/** The alerted item, re-fetched live from the matching report (the
	 * notification only carries its address: mortgage + kind + date). */
	interface AberrantItem {
		date: string;
		amount: string | null;
		classification: string | null;
		impliedRate: string | null;
		observedPayment: string | null;
		observedPeriods: number | null;
	}

	interface AberrantForm {
		annualRate: string;
		rateType: string;
		action: string;
	}

	interface AberrantModalState {
		notification: Notification;
		ref: AberrantRef;
		loading: boolean;
		loadError: string | null;
		item: AberrantItem | null;
		/** 'answered' = already classified, 'gone' = no longer in the
		 * report — both are end states for the modal. */
		resolved: 'answered' | 'gone' | null;
		rateType: 'fixed' | 'variable';
		showForm: boolean;
		form: AberrantForm;
		busy: 'one_off' | 'rate_change' | 'reject' | null;
		error: string | null;
	}

	let aberrantModal = $state<AberrantModalState | null>(null);

	/** Loads the item the alert points at (and the mortgage, for the
	 * form's seed values); settles the modal to an end state when the
	 * item is gone or already answered. */
	async function loadAberrantModal(): Promise<void> {
		const target = aberrantModal;
		if (target === null) return;
		const { ref } = target;
		try {
			const [report, m] = await Promise.all([
				getMortgageMatching(ref.mortgageId),
				getMortgage(ref.mortgageId)
			]);
			if (target !== aberrantModal) return;
			target.rateType = m.rate_type === 'variable' ? 'variable' : 'fixed';
			let item: AberrantItem | null = null;
			if (ref.kind === 'suggestion') {
				const s = report.suggestions.find((x) => x.effective_date === ref.date) ?? null;
				if (s !== null) {
					item = {
						date: s.effective_date,
						amount: s.observed_payment,
						classification: s.classification,
						impliedRate: s.implied_annual_rate,
						observedPayment: s.observed_payment,
						observedPeriods: s.observed_periods
					};
				}
			} else {
				const list = ref.kind === 'extra' ? report.extras : report.shorts;
				const a = list.find((x) => x.date === ref.date) ?? null;
				if (a !== null) {
					item = {
						date: a.date,
						amount: a.amount,
						classification: a.classification,
						impliedRate: null,
						observedPayment: null,
						observedPeriods: null
					};
				}
			}
			if (item === null) {
				target.item = null;
				target.resolved = 'gone';
				return;
			}
			target.item = item;
			if (item.classification !== null) {
				target.resolved = 'answered';
				return;
			}
			target.resolved = null;
			target.form = {
				annualRate: item.impliedRate ?? m.annual_rate,
				rateType: target.rateType,
				action: 're_amortize'
			};
		} catch (e) {
			if (target !== aberrantModal) return;
			target.loadError = envelopeMessage(e);
		} finally {
			if (target === aberrantModal) target.loading = false;
		}
	}

	/** Sends the classification for the alerted item; success drops the
	 * notification row (the endpoint deletes it) and closes the modal
	 * over the re-listed page. */
	async function aberrantAct(which: 'one_off' | 'rate_change' | 'reject'): Promise<void> {
		const m = aberrantModal;
		if (m === null || m.busy !== null || m.item === null || m.resolved !== null) {
			return;
		}
		if (which === 'rate_change' && m.showForm && m.form.annualRate.trim() === '') {
			m.error = $_('notifications.suggestion_rate_required');
			return;
		}
		m.busy = which;
		m.error = null;
		const body: PaymentClassificationRequest = {
			date: m.item.date,
			kind: m.ref.kind,
			classification: which === 'reject' ? 'rejected' : which
		};
		if (which === 'rate_change') {
			if (!m.showForm) {
				// Accept-implied (a suggestion): the implied rate as-is.
				body.annual_rate = m.item.impliedRate ?? '';
				body.rate_type = m.rateType;
				body.action = 're_amortize';
			} else {
				body.annual_rate = m.form.annualRate.trim();
				body.rate_type = m.form.rateType === 'variable' ? 'variable' : 'fixed';
				body.action = m.form.action === 'keep_payment' ? 'keep_payment' : 're_amortize';
			}
		}
		try {
			await classifyPayment(m.ref.mortgageId, body);
			dispatchUnreadChanged();
			aberrantModal = null;
			await reload();
		} catch (e) {
			if (m !== aberrantModal) return;
			m.busy = null;
			m.error = envelopeMessage(e);
		}
	}

	function toggleAberrantForm(): void {
		const m = aberrantModal;
		if (m === null || m.busy !== null) return;
		m.showForm = !m.showForm;
		m.error = null;
	}

	function setAberrantField(field: keyof AberrantForm, value: string): void {
		const m = aberrantModal;
		if (m === null) return;
		m.form[field] = value;
		m.error = null;
	}

	/** The mortgage's rate type, localized. */
	function rateTypeLabel(t: 'fixed' | 'variable'): string {
		return t === 'variable' ? $_('mortgage.rate_variable') : $_('mortgage.rate_fixed');
	}

	/** The classification words for the "already answered" line. */
	// `kind` is nullable on the wire; `null` is unreachable here (the
	// 'answered' end state only renders when the item carries a
	// classification) and falls through to the 'rejected' label.
	function classificationLabel(kind: string | null): string {
		if (kind === 'one_off') return $_('notifications.class_one_off');
		if (kind === 'rate_change') return $_('notifications.class_rate_change');
		return $_('notifications.class_rejected');
	}

	// Initial load is reactive on the identity, not on onMount. The
	// layout's fire-and-forget initSession() may still be in flight when
	// onMount runs; a fetch issued before the access token exists 401s and
	// nothing refetches once $user arrives. Keyed on the sub claim with a
	// last-seen guard, same as the layout's settings effect: oidc-client-ts
	// re-emits $user on every silent token renewal (same sub).
	let lastSub: string | null = null;
	$effect(() => {
		const sub = $user?.profile.sub ?? null;
		if (sub === lastSub) return;
		lastSub = sub;
		if (!sub) return;
		// Silent: a failure here only leaves the modal's labels raw.
		void Promise.all([listAccounts(), listCategories()])
			.then(([a, c]) => {
				accounts = a;
				categories = c;
			})
			.catch(() => {});
		void reload();
	});

	onMount(() => {
		const onEscape = (e: KeyboardEvent): void => {
			if (e.key === 'Escape') closeModals();
		};
		window.addEventListener('keydown', onEscape);
		return () => window.removeEventListener('keydown', onEscape);
	});
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
						{@const actionable = isActionable(n)}
						<li
							class="flex items-start justify-between gap-4 rounded-lg border p-4 {n.read_at ===
							null
								? 'border-zinc-300 bg-white dark:border-zinc-700 dark:bg-zinc-900'
								: 'border-zinc-200 bg-zinc-50 dark:border-zinc-800 dark:bg-zinc-950'} {actionable
								? 'cursor-pointer transition-colors hover:border-zinc-400 dark:hover:border-zinc-500'
								: ''}"
						>
							<!-- svelte-ignore a11y_no_noninteractive_tabindex -->
							<div
								class="space-y-1"
								role={actionable ? 'button' : undefined}
								tabindex={actionable ? 0 : undefined}
								aria-label={actionable ? $_('notifications.card_aria') : undefined}
								onclick={actionable ? () => openNotification(n) : undefined}
								onkeydown={actionable
									? (e) => {
											if (e.key === 'Enter') openNotification(n);
										}
									: undefined}
							>
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
							</div>
							{#if n.read_at === null}
								<button
									class="shrink-0 rounded-md border border-zinc-300 px-3 py-1.5 text-sm text-zinc-700 hover:bg-zinc-100 dark:border-zinc-700 dark:text-zinc-300 dark:hover:bg-zinc-800"
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

{#if reviewModal !== null}
	{@const m = reviewModal}
	<!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
	<div
		class="fixed inset-0 z-50 flex items-center justify-center bg-zinc-900/60 p-4"
		onclick={() => closeModals()}
	>
		<!-- svelte-ignore a11y_click_events_have_key_events -->
		<div
			class="relative max-h-[85vh] w-full max-w-3xl overflow-y-auto rounded-lg border border-zinc-200 bg-white p-5 shadow-lg dark:border-zinc-800 dark:bg-zinc-900"
			role="dialog"
			aria-modal="true"
			tabindex="-1"
			onclick={(e) => e.stopPropagation()}
		>
			<button
				class="absolute top-4 right-4 rounded-md border border-zinc-300 px-2 py-0.5 text-sm text-zinc-500 hover:bg-zinc-100 dark:border-zinc-700 dark:hover:bg-zinc-800"
				type="button"
				aria-label={$_('notifications.close')}
				onclick={() => closeModals()}
			>
				×
			</button>

			<h2 class="text-lg font-semibold">{$_('review.title')}</h2>
			<p class="mt-1 text-sm text-zinc-600 dark:text-zinc-400">{m.notification.body}</p>

			<div class="mt-4">
				{#if m.loading}
					<p class="text-sm text-zinc-600 dark:text-zinc-400">{$_('review.loading')}</p>
				{:else}
					{#if m.loadError !== null}
						<p class="text-sm text-red-600 dark:text-red-400">{m.loadError}</p>
					{/if}

					<!-- Apply-all: one category for every remaining row (the
					     card stays up after a successful apply-all so the result
					     is still visible over the now-empty queue). -->
					{#if m.rows.length > 0 || m.appliedAll !== null}
						<div class="space-y-2 rounded-lg border border-zinc-200 p-4 dark:border-zinc-800">
							<p class="text-sm font-medium">{$_('review.all_category_label')}</p>
							<select
								class="w-64 rounded-md border border-zinc-300 bg-white px-2 py-1.5 text-sm dark:border-zinc-700 dark:bg-zinc-900"
								value={m.allCategoryId}
								onchange={(e) => setAllCategory(e.currentTarget.value)}
							>
								<option value="">{$_('review.category_placeholder')}</option>
								{#each activeCategories as c (c.id)}
									<option value={c.id}>{c.name}</option>
								{/each}
							</select>
							<button
								class="rounded-md bg-zinc-900 px-4 py-2 text-sm font-medium text-white hover:bg-zinc-700 disabled:opacity-50 dark:bg-zinc-100 dark:text-zinc-900 dark:hover:bg-zinc-300"
								type="button"
								disabled={m.allCategoryId === '' || m.applyingAll || m.rows.length === 0}
								onclick={() => void applyAllRows()}
							>
								{m.applyingAll ? $_('review.applying_all') : $_('review.apply_all')}
							</button>
							{#if m.appliedAll !== null}
								<p class="text-sm text-green-600 dark:text-green-400">
									{$_('review.applied_all', { values: { n: m.appliedAll } })}
								</p>
							{/if}
							{#if m.allError !== null}
								<p class="text-sm text-red-600 dark:text-red-400">{m.allError}</p>
							{/if}
						</div>
					{/if}

					{#if m.rows.length > 0}
						<div class="mt-3 overflow-x-auto">
							<table class="w-full text-sm">
								<thead>
									<tr
										class="border-b border-zinc-200 text-left text-xs text-zinc-500 uppercase dark:border-zinc-800 dark:text-zinc-400"
									>
										<th class="py-1.5 pr-4">{$_('review.col_date')}</th>
										<th class="py-1.5 pr-4">{$_('review.col_description')}</th>
										<th class="py-1.5 pr-4 text-right">{$_('review.col_amount')}</th>
										<th class="py-1.5 pr-4">{$_('review.col_account')}</th>
										<th class="py-1.5 pr-4">{$_('review.col_category')}</th>
										<th class="py-1.5">{$_('review.col_actions')}</th>
									</tr>
								</thead>
								<tbody>
									{#each m.rows as r (r.id)}
										{@const st = rowState(r.id)}
										{#if st === null}
											<tr>
												<td colspan="6" class="py-2"></td>
											</tr>
										{:else if st.editing}
											<tr>
												<td colspan="6" class="py-3">
													{#if st.editLoading}
														<p class="text-sm text-zinc-600 dark:text-zinc-400">
															{$_('review.edit_loading')}
														</p>
													{:else if st.editSeed === null || st.editForm === null}
														<p class="text-sm text-zinc-600 dark:text-zinc-400">
															{$_('review.edit_not_found')}
														</p>
													{:else}
														<div class="grid gap-3 sm:grid-cols-2">
															<label class="text-sm">
																<span class="block text-zinc-600 dark:text-zinc-400">
																	{$_('manual_entry.account_label')}
																</span>
																<select
																	class="mt-1 w-full rounded-md border border-zinc-300 bg-white px-2 py-1.5 text-sm dark:border-zinc-700 dark:bg-zinc-900"
																	value={st.editForm.accountId}
																	onchange={(e) =>
																		setEditField(r, 'accountId', e.currentTarget.value)}
																>
																	{#each accounts as a (a.id)}
																		<option value={a.id}>{a.name}</option>
																	{/each}
																</select>
															</label>
															<label class="text-sm">
																<span class="block text-zinc-600 dark:text-zinc-400">
																	{$_('manual_entry.category_label')}
																</span>
																<select
																	class="mt-1 w-full rounded-md border border-zinc-300 bg-white px-2 py-1.5 text-sm dark:border-zinc-700 dark:bg-zinc-900"
																	value={st.editForm.categoryId}
																	onchange={(e) =>
																		setEditField(r, 'categoryId', e.currentTarget.value)}
																>
																	<option value="">
																		{$_('manual_entry.category_none')}
																	</option>
																	{#each activeCategories as c (c.id)}
																		<option value={c.id}>{c.name}</option>
																	{/each}
																</select>
															</label>
															<label class="text-sm">
																<span class="block text-zinc-600 dark:text-zinc-400">
																	{$_('manual_entry.date_posted_label')}
																</span>
																<input
																	type="date"
																	class="mt-1 w-full rounded-md border border-zinc-300 bg-white px-2 py-1.5 text-sm dark:border-zinc-700 dark:bg-zinc-900"
																	value={st.editForm.datePosted}
																	onchange={(e) =>
																		setEditField(r, 'datePosted', e.currentTarget.value)}
																/>
															</label>
															<label class="text-sm">
																<span class="block text-zinc-600 dark:text-zinc-400">
																	{$_('manual_entry.date_executed_label')}
																</span>
																<input
																	type="date"
																	class="mt-1 w-full rounded-md border border-zinc-300 bg-white px-2 py-1.5 text-sm dark:border-zinc-700 dark:bg-zinc-900"
																	value={st.editForm.dateExecuted}
																	onchange={(e) =>
																		setEditField(r, 'dateExecuted', e.currentTarget.value)}
																/>
															</label>
															<label class="text-sm">
																<span class="block text-zinc-600 dark:text-zinc-400">
																	{$_('manual_entry.amount_label')}
																</span>
																<input
																	inputmode="decimal"
																	placeholder={$_('manual_entry.amount_placeholder')}
																	class="mt-1 w-full rounded-md border border-zinc-300 bg-white px-2 py-1.5 text-sm dark:border-zinc-700 dark:bg-zinc-900"
																	bind:value={st.editForm.amount}
																/>
															</label>
															<label class="text-sm">
																<span class="block text-zinc-600 dark:text-zinc-400">
																	{$_('manual_entry.description_label')}
																</span>
																<input
																	class="mt-1 w-full rounded-md border border-zinc-300 bg-white px-2 py-1.5 text-sm dark:border-zinc-700 dark:bg-zinc-900"
																	bind:value={st.editForm.description}
																/>
															</label>
															<label class="text-sm sm:col-span-2">
																<span class="block text-zinc-600 dark:text-zinc-400">
																	{$_('manual_entry.notes_label')}
																</span>
																<textarea
																	rows="2"
																	class="mt-1 w-full rounded-md border border-zinc-300 bg-white px-2 py-1.5 text-sm dark:border-zinc-700 dark:bg-zinc-900"
																	bind:value={st.editForm.notes}></textarea>
															</label>
														</div>
														<div class="mt-3 flex gap-2">
															<button
																class="rounded-md bg-zinc-900 px-3 py-1.5 text-sm font-medium text-white hover:bg-zinc-700 disabled:opacity-50 dark:bg-zinc-100 dark:text-zinc-900 dark:hover:bg-zinc-300"
																type="button"
																disabled={st.busy !== null || !editHasDiff(st)}
																onclick={() => void saveEdit(r)}
															>
																{st.busy === 'save'
																	? $_('review.edit_saving')
																	: $_('review.edit_save')}
															</button>
															<button
																class="rounded-md border border-zinc-300 px-3 py-1.5 text-sm text-zinc-700 hover:bg-zinc-100 disabled:opacity-50 dark:border-zinc-700 dark:text-zinc-300 dark:hover:bg-zinc-800"
																type="button"
																disabled={st.busy !== null}
																onclick={() => void toggleEdit(r)}
															>
																{$_('review.edit_cancel')}
															</button>
														</div>
													{/if}
													{#if st.editError !== null}
														<p class="mt-2 text-xs text-red-600 dark:text-red-400">
															{st.editError}
														</p>
													{/if}
												</td>
											</tr>
										{:else}
											<tr class="border-b border-zinc-100 dark:border-zinc-800/50">
												<td class="py-2 pr-4 whitespace-nowrap tabular-nums">{r.date_posted}</td>
												<td class="py-2 pr-4">{r.description}</td>
												<td class="py-2 pr-4 text-right tabular-nums">{r.amount}</td>
												<td class="py-2 pr-4">{accountLabel(r.account_id)}</td>
												<td class="py-2 pr-4">
													<select
														class="w-44 rounded-md border border-zinc-300 bg-white px-2 py-1.5 text-sm dark:border-zinc-700 dark:bg-zinc-900"
														value={st.categoryId}
														onchange={(e) => setRowCategoryId(r, e.currentTarget.value)}
													>
														<option value="">{$_('manual_entry.category_none')}</option>
														{#each activeCategories as c (c.id)}
															<option value={c.id}>{c.name}</option>
														{/each}
													</select>
												</td>
												<td class="py-2">
													<div class="flex flex-wrap gap-2">
														<button
															class="rounded-md bg-zinc-900 px-3 py-1.5 text-sm font-medium text-white hover:bg-zinc-700 disabled:opacity-50 dark:bg-zinc-100 dark:text-zinc-900 dark:hover:bg-zinc-300"
															type="button"
															disabled={st.busy !== null || st.categoryId === ''}
															onclick={() => void applyRow(r)}
														>
															{st.busy === 'apply' ? $_('review.applying') : $_('review.apply')}
														</button>
														<button
															class="rounded-md border border-zinc-300 px-3 py-1.5 text-sm text-zinc-700 hover:bg-zinc-100 disabled:opacity-50 dark:border-zinc-700 dark:text-zinc-300 dark:hover:bg-zinc-800"
															type="button"
															disabled={st.busy !== null}
															onclick={() => void rejectRow(r)}
														>
															{st.busy === 'reject' ? $_('review.rejecting') : $_('review.reject')}
														</button>
														<button
															class="rounded-md border border-zinc-300 px-3 py-1.5 text-sm text-zinc-700 hover:bg-zinc-100 disabled:opacity-50 dark:border-zinc-700 dark:text-zinc-300 dark:hover:bg-zinc-800"
															type="button"
															disabled={st.busy !== null}
															onclick={() => void toggleEdit(r)}
														>
															{$_('review.edit')}
														</button>
													</div>
													{#if st.error !== null}
														<p class="mt-1 text-xs text-red-600 dark:text-red-400">{st.error}</p>
													{/if}
												</td>
											</tr>
										{/if}
									{/each}
								</tbody>
							</table>
						</div>
					{:else if m.appliedAll === null}
						<p class="text-sm text-zinc-500 dark:text-zinc-400">{$_('review.empty')}</p>
					{/if}
				{/if}
			</div>

			<div class="mt-5 flex justify-end border-t border-zinc-200 pt-3 dark:border-zinc-800">
				<a class="text-sm text-zinc-600 underline dark:text-zinc-400" href={resolve('/review')}>
					{$_('review.go_to_review')}
				</a>
			</div>
		</div>
	</div>
{/if}

{#if aberrantModal !== null}
	{@const s = aberrantModal}
	<!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
	<div
		class="fixed inset-0 z-50 flex items-center justify-center bg-zinc-900/60 p-4"
		onclick={() => closeModals()}
	>
		<!-- svelte-ignore a11y_click_events_have_key_events -->
		<div
			class="relative max-h-[85vh] w-full max-w-xl overflow-y-auto rounded-lg border border-zinc-200 bg-white p-5 shadow-lg dark:border-zinc-800 dark:bg-zinc-900"
			role="dialog"
			aria-modal="true"
			tabindex="-1"
			onclick={(e) => e.stopPropagation()}
		>
			<button
				class="absolute top-4 right-4 rounded-md border border-zinc-300 px-2 py-0.5 text-sm text-zinc-500 hover:bg-zinc-100 dark:border-zinc-700 dark:hover:bg-zinc-800"
				type="button"
				aria-label={$_('notifications.close')}
				onclick={() => closeModals()}
			>
				×
			</button>

			<h2 class="text-lg font-semibold">{s.notification.title}</h2>
			<p class="mt-1 text-sm text-zinc-600 dark:text-zinc-400">{s.notification.body}</p>

			<div class="mt-4">
				{#if s.loading}
					<p class="text-sm text-zinc-600 dark:text-zinc-400">
						{$_('notifications.suggestion_loading')}
					</p>
				{:else if s.loadError !== null}
					<p class="text-sm text-red-600 dark:text-red-400">{s.loadError}</p>
				{:else if s.resolved === 'gone'}
					<p class="text-sm text-zinc-500 dark:text-zinc-400">
						{s.ref.kind === 'suggestion'
							? $_('notifications.suggestion_gone')
							: $_('notifications.item_gone')}
					</p>
				{:else if s.resolved === 'answered' && s.item !== null}
					<p class="text-sm text-zinc-600 dark:text-zinc-400">
						{$_('notifications.answered', {
							values: { kind: classificationLabel(s.item.classification) }
						})}
					</p>
				{:else if s.item !== null}
					{#if s.ref.kind === 'suggestion'}
						<p class="text-sm text-zinc-600 dark:text-zinc-400">
							{$_('notifications.suggestion_implied')}:
							<span class="font-semibold text-zinc-900 tabular-nums dark:text-zinc-100">
								{s.item.impliedRate}{$_('mortgage.percent')}
							</span>
							<span class="text-zinc-400">·</span>
							{$_('notifications.suggestion_current_type')}:
							<span class="font-medium">{rateTypeLabel(s.rateType)}</span>
						</p>
						{#if s.item.observedPayment !== null && s.item.observedPeriods !== null}
							<p class="mt-1 text-sm text-zinc-500 dark:text-zinc-400">
								{$_('notifications.suggestion_observed', {
									values: {
										payment: s.item.observedPayment,
										n: s.item.observedPeriods
									}
								})}
							</p>
						{/if}
					{:else}
						<p class="text-sm text-zinc-600 dark:text-zinc-400">
							{$_('notifications.aberrant_amount')}:
							<span class="font-semibold text-zinc-900 tabular-nums dark:text-zinc-100">
								{s.item.amount ?? ''}
							</span>
						</p>
					{/if}

					{#if !s.showForm}
						<div class="mt-3 flex flex-wrap gap-2">
							{#if s.ref.kind === 'suggestion'}
								<button
									class="rounded-md bg-zinc-900 px-3 py-1.5 text-sm font-medium text-white hover:bg-zinc-700 disabled:opacity-50 dark:bg-zinc-100 dark:text-zinc-900 dark:hover:bg-zinc-300"
									type="button"
									disabled={s.busy !== null}
									onclick={() => void aberrantAct('rate_change')}
								>
									{s.busy === 'rate_change'
										? $_('notifications.suggestion_accepting')
										: $_('notifications.suggestion_accept')}
								</button>
								<button
									class="rounded-md border border-zinc-300 px-3 py-1.5 text-sm text-zinc-700 hover:bg-zinc-100 disabled:opacity-50 dark:border-zinc-700 dark:text-zinc-300 dark:hover:bg-zinc-800"
									type="button"
									disabled={s.busy !== null}
									onclick={() => toggleAberrantForm()}
								>
									{$_('notifications.suggestion_tweak')}
								</button>
							{:else}
								<button
									class="rounded-md bg-zinc-900 px-3 py-1.5 text-sm font-medium text-white hover:bg-zinc-700 disabled:opacity-50 dark:bg-zinc-100 dark:text-zinc-900 dark:hover:bg-zinc-300"
									type="button"
									disabled={s.busy !== null}
									onclick={() => void aberrantAct('one_off')}
								>
									{s.busy === 'one_off'
										? $_('notifications.aberrant_one_off_busy')
										: $_('notifications.aberrant_one_off')}
								</button>
								<button
									class="rounded-md border border-zinc-300 px-3 py-1.5 text-sm text-zinc-700 hover:bg-zinc-100 disabled:opacity-50 dark:border-zinc-700 dark:text-zinc-300 dark:hover:bg-zinc-800"
									type="button"
									disabled={s.busy !== null}
									onclick={() => toggleAberrantForm()}
								>
									{$_('notifications.aberrant_rate_change')}
								</button>
							{/if}
							<button
								class="rounded-md border border-zinc-300 px-3 py-1.5 text-sm text-zinc-700 hover:bg-zinc-100 disabled:opacity-50 dark:border-zinc-700 dark:text-zinc-300 dark:hover:bg-zinc-800"
								type="button"
								disabled={s.busy !== null}
								onclick={() => void aberrantAct('reject')}
							>
								{s.busy === 'reject'
									? $_('notifications.suggestion_rejecting')
									: $_('notifications.suggestion_reject')}
							</button>
						</div>
					{:else}
						<div class="mt-3 space-y-3">
							<label class="block text-sm">
								<span class="text-zinc-600 dark:text-zinc-400">
									{$_('notifications.suggestion_rate_label')}
								</span>
								<input
									inputmode="decimal"
									class="mt-1 w-full rounded-md border border-zinc-300 bg-white px-2 py-1.5 text-sm dark:border-zinc-700 dark:bg-zinc-900"
									bind:value={s.form.annualRate}
								/>
							</label>
							<label class="block text-sm">
								<span class="text-zinc-600 dark:text-zinc-400">
									{$_('notifications.suggestion_rate_type_label')}
								</span>
								<select
									class="mt-1 w-full rounded-md border border-zinc-300 bg-white px-2 py-1.5 text-sm dark:border-zinc-700 dark:bg-zinc-900"
									value={s.form.rateType}
									onchange={(e) => setAberrantField('rateType', e.currentTarget.value)}
								>
									<option value="fixed">{$_('mortgage.rate_fixed')}</option>
									<option value="variable">{$_('mortgage.rate_variable')}</option>
								</select>
							</label>
							<label class="block text-sm">
								<span class="text-zinc-600 dark:text-zinc-400">
									{$_('notifications.suggestion_action_label')}
								</span>
								<select
									class="mt-1 w-full rounded-md border border-zinc-300 bg-white px-2 py-1.5 text-sm dark:border-zinc-700 dark:bg-zinc-900"
									value={s.form.action}
									onchange={(e) => setAberrantField('action', e.currentTarget.value)}
								>
									<option value="re_amortize">
										{$_('mortgage.event_action_re_amortize')}
									</option>
									<option value="keep_payment">
										{$_('mortgage.event_action_keep_payment')}
									</option>
								</select>
							</label>
							{#if s.error !== null}
								<p class="text-sm text-red-600 dark:text-red-400">{s.error}</p>
							{/if}
							<div class="flex flex-wrap gap-2">
								<button
									class="rounded-md bg-zinc-900 px-3 py-1.5 text-sm font-medium text-white hover:bg-zinc-700 disabled:opacity-50 dark:bg-zinc-100 dark:text-zinc-900 dark:hover:bg-zinc-300"
									type="button"
									disabled={s.busy !== null}
									onclick={() => void aberrantAct('rate_change')}
								>
									{s.busy === 'rate_change'
										? $_('notifications.suggestion_applying')
										: $_('notifications.suggestion_apply')}
								</button>
								<button
									class="rounded-md border border-zinc-300 px-3 py-1.5 text-sm text-zinc-700 hover:bg-zinc-100 disabled:opacity-50 dark:border-zinc-700 dark:text-zinc-300 dark:hover:bg-zinc-800"
									type="button"
									disabled={s.busy !== null}
									onclick={() => toggleAberrantForm()}
								>
									{$_('notifications.suggestion_tweak_close')}
								</button>
								<button
									class="rounded-md border border-zinc-300 px-3 py-1.5 text-sm text-zinc-700 hover:bg-zinc-100 disabled:opacity-50 dark:border-zinc-700 dark:text-zinc-300 dark:hover:bg-zinc-800"
									type="button"
									disabled={s.busy !== null}
									onclick={() => void aberrantAct('reject')}
								>
									{s.busy === 'reject'
										? $_('notifications.suggestion_rejecting')
										: $_('notifications.suggestion_reject')}
								</button>
							</div>
						</div>
					{/if}
				{:else}
					<p class="text-sm text-zinc-500 dark:text-zinc-400">
						{$_('notifications.suggestion_loading')}
					</p>
				{/if}
			</div>

			<div class="mt-5 flex justify-end border-t border-zinc-200 pt-3 dark:border-zinc-800">
				<a
					class="text-sm text-zinc-600 underline dark:text-zinc-400"
					href={resolve(`/mortgage?m=${s.ref.mortgageId}`)}
				>
					{$_('notifications.goto_mortgage')}
				</a>
			</div>
		</div>
	</div>
{/if}

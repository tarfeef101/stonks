<script lang="ts">
	import { resolve } from '$app/paths';
	import { page } from '$app/state';
	import { _ } from 'svelte-i18n';
	import DOMPurify from 'dompurify';
	import { marked } from 'marked';
	import { user } from '#lib/auth/client';
	import { ApiError } from '#lib/api/client';
	import {
		listDashboards,
		listWidgets,
		createWidget,
		updateWidget,
		deleteWidget,
		setWidgetOrder,
		type Dashboard,
		type DashboardWidget,
		type WidgetKind
	} from '#lib/api/dashboards';
	import { listSavedViews, createSavedView, type SavedView } from '#lib/api/savedviews';
	import { loadCatalogs } from '#lib/explore/catalogs';
	import { parseState, serializeStateForLink, type Names } from '#lib/explore/state';
	import ExploreView from '#lib/explore/ExploreView.svelte';

	// Dashboard editor (rounds 6–7): one dashboard's 12-col × N-row
	// widget grid (fixed 120px row pitch at md+ — the card bodies
	// scroll inside). A widget is either a card whose body is the
	// shared ExploreView over the widget's stored (canonical) explore
	// state, or a markdown text card (marked → DOMPurify → @html —
	// user text is untrusted). Resize is desktop-window-style drag:
	// the right edge (width), the bottom edge (height), the corner
	// (both) — pointer-captured, live-previewed, one PATCH on
	// release — with the ⋯ menu's width/height selects as the a11y
	// fallback. Reorder (round 8) is a header drag: the card reflows
	// live to the pointer's drop position, ONE PUT /order on release
	// — the ⋯ menu's move left/right stay as the a11y fallback. While
	// a drag is live the grid's bounds are drawn (a ring on the grid
	// + 12 column slot frames, each at a card's exact footprint
	// width) so the width limit is visible — otherwise the empty
	// space past a wide card looks like room to drop next to it.
	// Other ops: edit (rename for explore widgets; rename + content
	// for markdown), confirm-gated delete. "Open in Explore"
	// re-serializes the stored state with the sign ALWAYS explicit —
	// a bare /explore opens the remembered last view, not the
	// widget's.
	//
	// There is no GET /v1/dashboards/{id} route — the dashboard row is
	// resolved by id from the list; an unknown id means "not found".

	const dashboardId = $derived(page.params.id ?? '');

	let dashboard = $state<Dashboard | null>(null);
	let widgets = $state<DashboardWidget[]>([]);
	let names = $state<Names>({ accounts: {}, categories: {} });
	let loading = $state(true);
	let loadError = $state<string | null>(null);
	let notFound = $state(false);

	// Shared note for widget-op failures: NOT auto-cleared — the next
	// successful op or a new op start clears it.
	let opError = $state<string | null>(null);

	let seq = 0;
	let editingId = $state<string | null>(null);
	let draftName = $state('');
	let draftContent = $state('');

	let adding = $state(false);
	let addKind = $state<WidgetKind | null>(null);
	let addName = $state('');
	let addContent = $state('');
	let addError = $state<string | null>(null);

	// The add card's saved-views picker (round 16): '' = no saved view
	// (the default view — the old behavior), 'new' = create a NEW saved
	// view first (named after the widget name, the default state) and
	// then the widget, else the picked saved view's id — the widget's
	// content is a COPY of its stored state (a copy, not a reference:
	// deleting the view later never breaks the widget). One list load
	// per add-card open; stale loads are dropped (a failed load leaves
	// the picker on the "no saved view" path — adding still works).
	let addSavedViews = $state<SavedView[]>([]);
	let addSavedView = $state('');
	let addViewsSeq = 0;

	// --- Load --------------------------------------------------------------

	/** The page's single load for THIS dashboard id: the dashboards
	 * list first (no single-dashboard GET route), and only when the
	 * id resolves, its widgets + the catalog name maps (one round).
	 * A seq guard drops stale loads. */
	async function load(id: string): Promise<void> {
		const s = ++seq;
		loading = true;
		loadError = null;
		notFound = false;
		try {
			const ds = await listDashboards();
			if (s !== seq) return; // stale
			dashboard = ds.find((d) => d.id === id) ?? null;
			if (dashboard === null) {
				widgets = [];
				loading = false;
				notFound = true;
				return;
			}
			const [ws, n] = await Promise.all([listWidgets(id), loadCatalogs()]);
			if (s !== seq) return; // stale
			widgets = ws;
			names = n;
			loading = false;
		} catch (e) {
			if (s !== seq) return; // stale
			loading = false;
			loadError = envelopeMessage(e);
		}
	}

	// One load per identity + dashboard change (a signed-out caller
	// sees the sign-in gate).
	$effect(() => {
		const sub = $user?.profile.sub ?? null;
		const id = page.params.id ?? '';
		if (sub === null || id === '') {
			seq++; // invalidate any in-flight load
			dashboard = null;
			widgets = [];
			loading = false;
			loadError = null;
			notFound = id !== '';
			return;
		}
		void load(id);
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

	// --- Grid (round 7: 2D — fixed row pitch + drag-resize) ----------------

	// The md+ grid geometry the drag-resize math uses: 12 columns,
	// gap-4 (16px), and a fixed 120px row pitch (the grid div's
	// md:auto-rows-[120px] — keep the class and the constant in
	// sync). TAILWIND JIT needs the span class literals spelled out
	// (a runtime-built 'col-span-' + n would be purged); any stored
	// span/row_span (the API allows 1..12 / 1..8) still gets its
	// class.
	const GRID_COLS = 12;
	const GRID_GAP = 16;
	const GRID_ROW_PITCH = 120;
	const SPAN_CLASSES: Record<number, string> = {
		1: 'md:col-span-1',
		2: 'md:col-span-2',
		3: 'md:col-span-3',
		4: 'md:col-span-4',
		5: 'md:col-span-5',
		6: 'md:col-span-6',
		7: 'md:col-span-7',
		8: 'md:col-span-8',
		9: 'md:col-span-9',
		10: 'md:col-span-10',
		11: 'md:col-span-11',
		12: 'md:col-span-12'
	};
	const ROW_SPAN_CLASSES: Record<number, string> = {
		1: 'md:row-span-1',
		2: 'md:row-span-2',
		3: 'md:row-span-3',
		4: 'md:row-span-4',
		5: 'md:row-span-5',
		6: 'md:row-span-6',
		7: 'md:row-span-7',
		8: 'md:row-span-8'
	};
	// The ⋯ menu's width/height selects — the a11y fallback for the
	// drag handles — cover the FULL ranges: a drag can land on any
	// value, so the selects must be able to show the truth.
	const WIDTH_OPTIONS = [1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12];
	const HEIGHT_OPTIONS = [1, 2, 3, 4, 5, 6, 7, 8];

	function clampSpan(n: number): number {
		return Math.min(GRID_COLS, Math.max(1, n));
	}
	function clampRows(n: number): number {
		return Math.min(HEIGHT_OPTIONS.length, Math.max(1, n));
	}

	// The live drag-resize preview: while a handle drag is in flight,
	// the dragged card previews its candidate span/row_span here (the
	// stored widgets stay put until the pointerup PATCH); null = no
	// drag. gridEl binds the grid div (the unit math's reference).
	let dragging = $state<{ id: string; span: number; rowSpan: number } | null>(null);
	let gridEl = $state<HTMLDivElement | null>(null);

	// The live drag-move (reorder) preview: while a header drag is in
	// flight, a light "ghost" clone follows the pointer (the ghost in
	// the template) and the card itself renders as a dashed hole in
	// the grid at the live drop position (its head/body/handles are
	// hidden while moving). The stored widgets stay put until the
	// pointerup PUT /order. from = the card's index in the STORED
	// list at pointerdown; drop = a 0..n splice index into the
	// non-dragged list. x/y = the pointer position; dx/dy = the grab
	// offset from the card's top-left corner; width/height = the
	// card's rect at pointerdown (the grid pitch is fixed, so the
	// card's size can't change mid-move); gridW = the grid's content
	// width (re-read on every move — a mid-drag window resize moves
	// the column slot frames with it).
	let dragMove = $state<{
		id: string;
		from: number;
		drop: number;
		x: number;
		y: number;
		dx: number;
		dy: number;
		width: number;
		height: number;
		gridW: number;
	} | null>(null);

	// The display order: the stored widgets, with the dragged card
	// spliced into its drop position while a drag-move is live — its
	// root stays in the grid flow (rendered as the dashed hole, see
	// the template) while the ghost follows the pointer.
	const displayWidgets = $derived.by((): DashboardWidget[] => {
		const m = dragMove;
		if (m === null) return widgets;
		const dragged = widgets.find((x) => x.id === m.id);
		if (dragged === undefined) return widgets;
		const rest = widgets.filter((x) => x.id !== m.id);
		const arr = [...rest];
		arr.splice(Math.min(Math.max(m.drop, 0), rest.length), 0, dragged);
		return arr;
	});

	// The widget a drag-move is live for (the ghost's content) —
	// null while no drag-move is in flight.
	const draggedWidget = $derived.by((): DashboardWidget | null => {
		const m = dragMove;
		if (m === null) return null;
		return widgets.find((x) => x.id === m.id) ?? null;
	});

	// The column slot frames for the drag-move bounds overlay: slot i
	// spans [i·colUnit, i·colUnit + track), colUnit = (gridW + gap)/12
	// (track + gap — the exact pitch the resize snap uses) and track
	// = (gridW - 11·gap)/12 — so each frame is a card's exact
	// footprint width: a card flush-fills its slot and the gutters
	// sit BETWEEN the frames, never inside them. The last slot's
	// right edge lands on gridW exactly. Empty while no drag-move is
	// live. md+ only in the template (below md the grid is one
	// stacked column and has no slots to draw).
	const gridSlots = $derived.by((): { left: number; width: number }[] => {
		const m = dragMove;
		if (m === null || m.gridW <= 0) return [];
		const colUnit = (m.gridW + GRID_GAP) / GRID_COLS;
		const track = (m.gridW - (GRID_COLS - 1) * GRID_GAP) / GRID_COLS;
		const slots: { left: number; width: number }[] = [];
		for (let i = 0; i < GRID_COLS; i++) slots.push({ left: i * colUnit, width: track });
		return slots;
	});

	// The ⋯ overflow menu: at most one open at a time, keyed by the
	// widget's id; closed by the fixed inset-0 click-catcher, by an
	// item click, or by Escape (window keydown while open).
	let menuId = $state<string | null>(null);

	$effect(() => {
		if (menuId === null) return;
		const onKey = (e: KeyboardEvent): void => {
			if (e.key === 'Escape') menuId = null;
		};
		window.addEventListener('keydown', onKey);
		return () => window.removeEventListener('keydown', onKey);
	});

	/** The widget's current grid footprint: the stored span/row_span,
	 * overridden while its drag-resize is live-previewing. */
	function footprint(w: DashboardWidget): { span: number; rowSpan: number } {
		if (dragging !== null && dragging.id === w.id)
			return { span: dragging.span, rowSpan: dragging.rowSpan };
		return { span: w.span, rowSpan: w.row_span };
	}

	function spanClass(span: number): string {
		return SPAN_CLASSES[span] ?? 'md:col-span-6';
	}
	function rowSpanClass(rows: number): string {
		return ROW_SPAN_CLASSES[rows] ?? 'md:row-span-4';
	}

	/** Begins a drag-resize from a card edge/corner handle: pointer
	 * capture on the handle element, deltas converted to grid units,
	 * the live preview in `dragging`, and ONE {span, row_span} PATCH
	 * on release. The stored widgets are never mutated mid-drag, so a
	 * failed PATCH simply drops the preview + sets opError (the
	 * "revert"). mode: 'x' = right edge (width), 'y' = bottom edge
	 * (height), 'xy' = the corner (both). */
	function startDrag(e: PointerEvent, w: DashboardWidget, mode: 'x' | 'y' | 'xy'): void {
		const grid = gridEl;
		const handle = e.currentTarget as HTMLElement;
		if (grid === null) return;
		e.preventDefault();
		const rect = grid.getBoundingClientRect();
		const colUnit = (rect.width + GRID_GAP) / GRID_COLS;
		const rowUnit = GRID_ROW_PITCH + GRID_GAP;
		const startX = e.clientX;
		const startY = e.clientY;
		const startSpan = w.span;
		const startRows = w.row_span;
		handle.setPointerCapture(e.pointerId);
		const onMove = (ev: PointerEvent): void => {
			let span = startSpan;
			let rows = startRows;
			if (mode === 'x' || mode === 'xy')
				span = clampSpan(startSpan + Math.round((ev.clientX - startX) / colUnit));
			if (mode === 'y' || mode === 'xy')
				rows = clampRows(startRows + Math.round((ev.clientY - startY) / rowUnit));
			dragging = { id: w.id, span, rowSpan: rows };
		};
		const finish = (commit: boolean): void => {
			handle.removeEventListener('pointermove', onMove);
			handle.removeEventListener('pointerup', onUp);
			handle.removeEventListener('pointercancel', onCancel);
			if (handle.hasPointerCapture(e.pointerId)) handle.releasePointerCapture(e.pointerId);
			const d = dragging;
			dragging = null;
			if (commit && d !== null && (d.span !== startSpan || d.rowSpan !== startRows))
				void persistResize(w, d.span, d.rowSpan);
		};
		const onUp = (): void => finish(true);
		const onCancel = (): void => finish(false);
		handle.addEventListener('pointermove', onMove);
		handle.addEventListener('pointerup', onUp);
		handle.addEventListener('pointercancel', onCancel);
	}

	/** Persists a drag-resize: one PATCH of both dimensions; the
	 * returned row replaces the widget in place. */
	async function persistResize(w: DashboardWidget, span: number, rows: number): Promise<void> {
		opError = null;
		try {
			const updated = await updateWidget(dashboardId, w.id, { span, row_span: rows });
			widgets = widgets.map((x) => (x.id === w.id ? updated : x));
		} catch (e) {
			opError = envelopeMessage(e);
		}
	}

	/** Maps a pointer position to a drop index (a 0..n splice index
	 * into the NON-dragged list): the first card whose box contains
	 * the pointer wins, and the pointer falling in its top-left
	 * triangle drops before it. null = over no card (the current
	 * drop is kept — empty grid space must not yank the card to the
	 * end). DOM order == display order while the drag is live, so
	 * the query is stable under the reflow. */
	function computeDropIndex(x: number, y: number, dragId: string): number | null {
		const grid = gridEl;
		if (grid === null) return null;
		let pos = 0;
		for (const el of Array.from(grid.querySelectorAll<HTMLElement>('[data-widget-id]'))) {
			if (el.dataset.widgetId === dragId) continue;
			const r = el.getBoundingClientRect();
			if (x < r.left || x > r.right || y < r.top || y > r.bottom) continue;
			const before = (x - r.left) / r.width + (y - r.top) / r.height < 1;
			return before ? pos : pos + 1;
		}
		return null;
	}

	/** Begins a drag-move from the card's header row: window-level
	 * pointermove/pointerup/pointercancel — NO element capture,
	 * because the card's node re-parents in the DOM mid-drag (the
	 * reflow) and that would release a capture. While live, a ghost
	 * follows the pointer and the card renders as a dashed hole at
	 * the drop position (displayWidgets); on release ONE PUT
	 * /order, and the stored widgets are only rewritten on success
	 * (a failure is opError + the preview dropping = the revert).
	 * Skipped while that widget is in edit mode, a menu is open,
	 * another drag is in flight, or the press starts on an
	 * interactive element (the ⋯ button, the name input, …). */
	function startMove(e: PointerEvent, w: DashboardWidget): void {
		if (editingId === w.id || menuId !== null || dragging !== null || dragMove !== null) return;
		if (e.target instanceof Element && e.target.closest('button, a, input, select, textarea'))
			return;
		const from = widgets.findIndex((x) => x.id === w.id);
		if (from === -1) return;
		e.preventDefault();
		// Measure the card (the header row's parent) for the ghost:
		// the grab offset keeps it pinned under the pointer, and the
		// size is fixed for the whole move (the grid pitch is).
		const cardEl = (e.currentTarget as HTMLElement).parentElement;
		const rect = cardEl instanceof HTMLElement ? cardEl.getBoundingClientRect() : null;
		dragMove = {
			id: w.id,
			from,
			drop: from,
			x: e.clientX,
			y: e.clientY,
			dx: e.clientX - (rect?.left ?? e.clientX),
			dy: e.clientY - (rect?.top ?? e.clientY),
			width: rect?.width ?? 0,
			height: rect?.height ?? 0,
			gridW: gridEl?.clientWidth ?? 0
		};
		const onMove = (ev: PointerEvent): void => {
			ev.preventDefault();
			const m = dragMove;
			if (m === null) return;
			// The ghost follows the pointer on every move; the drop
			// slot only changes when the pointer is over a card
			// (empty grid space keeps the current slot).
			const idx = computeDropIndex(ev.clientX, ev.clientY, m.id);
			dragMove = {
				...m,
				x: ev.clientX,
				y: ev.clientY,
				drop: idx === null ? m.drop : idx,
				gridW: gridEl?.clientWidth ?? m.gridW
			};
		};
		const finish = (commit: boolean): void => {
			window.removeEventListener('pointermove', onMove);
			window.removeEventListener('pointerup', onUp);
			window.removeEventListener('pointercancel', onCancel);
			const m = dragMove;
			dragMove = null;
			// drop === from = the card landed where it started: the
			// order is unchanged, so there is nothing to persist.
			if (commit && m !== null && m.drop !== m.from) void persistReorder(m);
		};
		const onUp = (): void => finish(true);
		const onCancel = (): void => finish(false);
		window.addEventListener('pointermove', onMove);
		window.addEventListener('pointerup', onUp);
		window.addEventListener('pointercancel', onCancel);
	}

	/** Persists a drag-move: PUT /order with the display order; on
	 * success the local sort_order is rewritten, on failure the
	 * stored widgets were never mutated (the dropped preview IS the
	 * revert). */
	async function persistReorder(m: { id: string; from: number; drop: number }): Promise<void> {
		const moved = widgets[m.from];
		if (moved === undefined) return;
		const rest = widgets.filter((x) => x.id !== m.id);
		const next = [...rest];
		next.splice(Math.min(Math.max(m.drop, 0), rest.length), 0, moved);
		opError = null;
		try {
			await setWidgetOrder(dashboardId, next.map((x) => x.id));
			widgets = next.map((x, k) => ({ ...x, sort_order: k + 1 }));
		} catch (e) {
			opError = envelopeMessage(e);
		}
	}

	/** The ghost's body preview for a markdown card: the first
	 * non-empty line of the raw source (the ghost is a light clone —
	 * the body is NOT re-rendered into it). */
	function ghostLine(w: DashboardWidget): string {
		for (const line of w.content.split('\n')) {
			const t = line.trim();
			if (t !== '') return t;
		}
		return '';
	}

	/** Renders a markdown widget's body: marked → DOMPurify
	 * (sanitized before the @html — user-authored text is not
	 * trusted). Runs on each render of a markdown card (cheap for
	 * note-sized text — same stance as parseState in the explore
	 * cards). */
	function renderMarkdown(src: string): string {
		return DOMPurify.sanitize(marked.parse(src, { async: false }) as string);
	}

	// --- Widget ops ---------------------------------------------------------

	/** Resizes the widget's width (the ⋯ menu's width select — the
	 * a11y fallback for the drag handle); the returned row replaces
	 * it in place. */
	async function resize(w: DashboardWidget, span: number): Promise<void> {
		if (span === w.span) return;
		opError = null;
		try {
			const updated = await updateWidget(dashboardId, w.id, { span });
			widgets = widgets.map((x) => (x.id === w.id ? updated : x));
		} catch (e) {
			opError = envelopeMessage(e);
		}
	}

	/** Resizes the widget's height (the ⋯ menu's height select). */
	async function resizeHeight(w: DashboardWidget, rows: number): Promise<void> {
		if (rows === w.row_span) return;
		opError = null;
		try {
			const updated = await updateWidget(dashboardId, w.id, { row_span: rows });
			widgets = widgets.map((x) => (x.id === w.id ? updated : x));
		} catch (e) {
			opError = envelopeMessage(e);
		}
	}

	/** Swaps the widget with its neighbour (no-op at the edges) and
	 * persists the new order — the swap + PUT /order pattern of the
	 * round-5 cards page. */
	async function move(w: DashboardWidget, dir: -1 | 1): Promise<void> {
		const i = widgets.findIndex((x) => x.id === w.id);
		const j = i + dir;
		if (i === -1 || j < 0 || j >= widgets.length) return; // no neighbour
		const next = [...widgets];
		[next[i], next[j]] = [next[j], next[i]];
		opError = null;
		try {
			await setWidgetOrder(dashboardId, next.map((x) => x.id));
			widgets = next.map((x, k) => ({ ...x, sort_order: k + 1 }));
		} catch (e) {
			opError = envelopeMessage(e);
		}
	}

	function startEdit(w: DashboardWidget): void {
		editingId = w.id;
		draftName = w.name;
		draftContent = w.content;
	}

	function cancelEdit(): void {
		editingId = null;
	}

	/** Commits (or cancels) the widget edit: explore widgets rename
	 * ONLY (editing the saved query in place is a contract non-goal —
	 * a changed view is saved as a NEW widget from the Explore page);
	 * markdown widgets rename + re-edit the content. An empty or
	 * unchanged draft just cancels; otherwise the returned row
	 * replaces the local widget. */
	async function saveEdit(w: DashboardWidget): Promise<void> {
		if (editingId !== w.id) return;
		editingId = null;
		const patch: { name?: string; content?: string } = {};
		// Names are optional and stored trimmed: a cleared name
		// patches to "" (the markdown body can carry the "title"),
		// and whitespace-only edits are trimmed no-ops.
		if (draftName.trim() !== w.name) patch.name = draftName.trim();
		if (w.kind === 'markdown' && draftContent !== w.content) patch.content = draftContent;
		if (Object.keys(patch).length === 0) return; // cancel / no-op
		opError = null;
		try {
			const updated = await updateWidget(dashboardId, w.id, patch);
			widgets = widgets.map((x) => (x.id === w.id ? updated : x));
		} catch (e) {
			opError = envelopeMessage(e);
		}
	}

	/** Deletes the widget (confirm-gated). */
	async function removeWidget(w: DashboardWidget): Promise<void> {
		const label = w.name === '' ? $_('dashboards.widget_untitled') : w.name;
		if (!window.confirm($_('dashboards.widget_delete_confirm', { values: { name: label } })))
			return;
		opError = null;
		try {
			await deleteWidget(dashboardId, w.id);
			widgets = widgets.filter((x) => x.id !== w.id);
			if (dashboard !== null)
				dashboard = { ...dashboard, widget_count: dashboard.widget_count - 1 };
		} catch (e) {
			opError = envelopeMessage(e);
		}
	}

	// --- Add widget ----------------------------------------------------------

	/** Opens the add-widget card with the kind preselected: "Explore
	 * view" seeds the saved-views picker (its list loads async),
	 * "Text (markdown)" also opens the content textarea. */
	function startAdd(kind: WidgetKind): void {
		adding = true;
		addKind = kind;
		addName = '';
		addContent = '';
		addSavedView = '';
		addError = null;
		if (kind === 'explore') void loadAddSavedViews();
	}

	function cancelAdd(): void {
		adding = false;
		addKind = null;
	}

	/** Loads the caller's saved views for the add card's picker (one
	 * load per open; stale loads are dropped). A failed load leaves
	 * the picker on the "no saved view" path — adding still works. */
	async function loadAddSavedViews(): Promise<void> {
		const s = ++addViewsSeq;
		try {
			const vs = await listSavedViews();
			if (s !== addViewsSeq) return; // stale
			addSavedViews = vs;
		} catch {
			if (s !== addViewsSeq) return; // stale
			addSavedViews = [];
		}
	}

	/** The picker's select changed: a saved view pre-fills the widget
	 * name with the view's own label; "new" and "none" clear it (the
	 * user names the widget — for "new" that name is the new saved
	 * view's name too). */
	function onAddSavedViewChange(e: Event): void {
		const sel = e.currentTarget as HTMLSelectElement;
		addSavedView = sel.value;
		const sv = addSavedViews.find((x) => x.id === addSavedView);
		addName = sv !== undefined ? sv.name : '';
	}

	/** Creates the widget from the add card (new widgets span 4
	 * columns; row_span is left to the server's per-kind default —
	 * explore 4, markdown 2); the returned row is appended to the
	 * grid. The explore content is a COPY of the picked saved view's
	 * state ('new' creates its saved view first — named after the
	 * widget name, the default state — then the widget; "none" is
	 * the old '' path). */
	async function submitAdd(): Promise<void> {
		if (addKind === null) return;
		addError = null;
		try {
			let content = '';
			if (addKind === 'markdown') {
				content = addContent;
			} else if (addSavedView === 'new') {
				// The typed name IS the new saved view's name (and
				// the widget's); blank is a server 400, verbatim.
				await createSavedView({ name: addName, state: '' });
			} else if (addSavedView !== '') {
				// The widget stores its OWN copy of the picked view's
				// state — a copy, not a reference.
				const sv = addSavedViews.find((x) => x.id === addSavedView);
				if (sv !== undefined) content = sv.state;
			}
			const w = await createWidget(dashboardId, {
				name: addName,
				kind: addKind,
				content,
				span: 4
			});
			widgets = [...widgets, w];
			if (dashboard !== null)
				dashboard = { ...dashboard, widget_count: dashboard.widget_count + 1 };
			cancelAdd();
		} catch (e) {
			addError = envelopeMessage(e);
		}
	}

	/** Auto-focuses the inline edit / add-name inputs when they appear. */
	function autofocus(el: HTMLInputElement): void {
		el.focus();
	}
</script>

<section class="space-y-4">
	<!-- Header: back to the list + the dashboard's name (the name is
	     resolved from the list — there is no single-dashboard GET). -->
	<div class="flex flex-wrap items-baseline gap-3">
		<a
			href={resolve('/dashboards')}
			class="text-sm font-medium text-zinc-500 dark:text-zinc-400 underline hover:text-zinc-900 dark:hover:text-zinc-100"
		>
			{$_('dashboards.heading')}
		</a>
		{#if dashboard}
			<h1 class="text-2xl font-semibold">{dashboard.name}</h1>
		{/if}
	</div>

	{#if !$user}
		<p class="text-sm text-zinc-600 dark:text-zinc-400">{$_('dashboards.sign_in')}</p>
	{:else if loading}
		<p class="text-sm text-zinc-600 dark:text-zinc-400">{$_('dashboards.loading')}</p>
	{:else if notFound}
		<p class="text-sm text-zinc-600 dark:text-zinc-400">{$_('dashboards.not_found')}</p>
	{:else if loadError}
		<p class="text-sm text-red-600 dark:text-red-400">{loadError}</p>
	{:else}
		{#if opError}
			<p class="text-sm text-red-600 dark:text-red-400">{opError}</p>
		{/if}

		<!-- The widget grid (round 7): 12 columns at md+, stacked on
		     small screens, with a fixed 120px row pitch at md+ (the
		     card bodies scroll inside). Each card's head carries the
		     name (kind badge) + the ⋯ menu (open in Explore, width /
		     height selects, move, edit, delete); the right edge,
		     bottom edge and corner are drag-resize handles (width /
		     height / both); the body is the live Explore view, the
		     sanitized markdown, or the markdown edit textarea.
		     Nameless cards render headless at rest (round 13) —
		     their head is sr-only until the card is hovered or
		     focused, so the body — a markdown card's own `# Title`
		     heading foremost — fills the whole card. -->
		<div
			class="relative grid grid-cols-1 gap-4 md:grid-cols-12 md:auto-rows-[120px] {dragMove !== null
				? 'rounded-lg ring-2 ring-sky-500/40'
				: ''}"
			bind:this={gridEl}
		>
			{#each displayWidgets as w (w.id)}
				{@const fp = footprint(w)}
				{@const moving = dragMove?.id === w.id}
				<!-- The card root; while its drag-move is live it
				     renders as the dashed hole at the live drop slot
				     (head/body/handles hidden) and the ghost follows
				     the pointer. It keeps its data-widget-id so
				     computeDropIndex's skip-by-id still excludes it
				     from the positional count. -->
				<div
					data-widget-id={w.id}
					class="group relative flex flex-col rounded-lg {spanClass(fp.span)} {rowSpanClass(fp.rowSpan)} {moving
						? 'border-2 border-dashed border-sky-500/60 bg-sky-500/5'
						: dragging?.id === w.id
							? 'border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 ring-1 ring-sky-500/70'
							: 'border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900'}"
				>
					<!-- Compact head (rounds 7 + 13): name (truncated) + kind
					     badge, then the ⋯ overflow menu — the controls no
					     longer bleed out of a narrow card. In edit mode the
					     name input + Save / Cancel take the right side and
					     the ⋯ button hides. The row is ALSO the round-8
					     drag-move surface (cursor-grab; startMove ignores
					     presses on its interactive children). A NAMELESS card
					     keeps the head out of the flow at rest (sr-only —
					     the ⋯ stays keyboard-reachable) so its body — a
					     markdown card's own `# Title` heading foremost —
					     fills the whole card; the head slides back in when
					     the card is hovered or has focus (group-hover /
					     group-focus-within on the root), or whenever its
					     menu is open / it is being edited. The row's
					     padding lives on its children, not the row:
					     the not-sr-only reveal sets padding:0 on the
					     row itself, which would beat the row's own
					     px/py. -->
					<div
						class="flex cursor-grab touch-none items-center gap-2 border-b border-zinc-200 dark:border-zinc-800 active:cursor-grabbing {moving
							? 'hidden'
							: w.name === ''
								? menuId === w.id || editingId === w.id
									? ''
									: 'sr-only group-hover:not-sr-only group-focus-within:not-sr-only'
								: ''}"
						role="group"
						aria-label={$_('dashboards.widget_drag_move')}
						title={$_('dashboards.widget_drag_move')}
						onpointerdown={(e) => startMove(e, w)}
					>
						<div class="flex min-w-0 flex-1 items-center gap-2 px-3 py-1.5">
							{#if editingId === w.id}
								<input
									class="min-w-0 flex-1 rounded-md border border-zinc-300 dark:border-zinc-700 px-2 py-1 text-sm"
									bind:value={draftName}
									use:autofocus
									aria-label={$_('dashboards.widget_name')}
									onkeydown={(e) => {
										if (e.key === 'Enter') void saveEdit(w);
										if (e.key === 'Escape') cancelEdit();
									}}
								/>
							{:else if w.name === ''}
								<!-- Nameless widget: the markdown body can
								     carry the "title" — a muted placeholder
								     stands in here. -->
								<h2 class="truncate text-sm font-semibold text-zinc-400 dark:text-zinc-500 italic">
									{$_('dashboards.widget_untitled')}
								</h2>
							{:else}
								<h2 class="truncate text-sm font-semibold">{w.name}</h2>
								<span
									class="shrink-0 rounded bg-zinc-100 dark:bg-zinc-800 px-1.5 py-0.5 text-xs text-zinc-500 dark:text-zinc-400"
								>
									{w.kind === 'explore'
										? $_('dashboards.kind_explore')
										: $_('dashboards.kind_markdown')}
								</span>
							{/if}
						</div>
						<div class="flex shrink-0 items-center gap-2 py-1.5 pr-3">
							{#if editingId === w.id}
								<button
									class="shrink-0 font-medium text-emerald-600 dark:text-emerald-400 underline"
									type="button"
									onclick={() => void saveEdit(w)}
								>
									{$_('dashboards.widget_save')}
								</button>
								<button
									class="shrink-0 font-medium text-zinc-600 dark:text-zinc-400 underline"
									type="button"
									onclick={cancelEdit}
								>
									{$_('dashboards.widget_cancel')}
								</button>
							{:else}
								<div class="relative shrink-0">
									<button
										class="rounded-md px-1.5 py-0.5 text-base leading-none text-zinc-500 dark:text-zinc-400 hover:bg-zinc-100 dark:hover:bg-zinc-800"
										type="button"
										aria-label={$_('dashboards.widget_menu')}
										aria-expanded={menuId === w.id}
										onclick={() => (menuId = menuId === w.id ? null : w.id)}
									>
										⋯
									</button>
									{#if menuId === w.id}
										<!-- Click-catcher: any click outside the panel
										     closes the menu (z-20, below the z-30 panel). -->
										<div
											class="fixed inset-0 z-20"
											aria-hidden="true"
											onclick={() => (menuId = null)}
										></div>
										<div
											class="absolute right-0 top-full z-30 mt-1 w-48 rounded-md border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 py-1 shadow-lg"
											role="menu"
										>
											{#if w.kind === 'explore'}
												<a
													href={resolve('/explore') + '?' + serializeStateForLink(parseState(w.content))}
													class="block px-3 py-1.5 text-sm text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-800"
													role="menuitem"
													onclick={() => (menuId = null)}
												>
													{$_('dashboards.widget_open_explore')}
												</a>
												<div class="my-1 border-t border-zinc-200 dark:border-zinc-800"></div>
											{/if}
											<label
												class="flex cursor-pointer items-center justify-between gap-2 px-3 py-1.5 text-sm text-zinc-700 dark:text-zinc-300"
											>
												{$_('dashboards.widget_width')}
												<select
													class="rounded-md border border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 px-1.5 py-0.5 text-xs"
													value={fp.span}
													aria-label={$_('dashboards.widget_width')}
													onchange={(e) => {
														menuId = null;
														void resize(w, Number(e.currentTarget.value));
													}}
												>
													{#each WIDTH_OPTIONS as n (n)}
														<option value={n}>{$_('dashboards.size_n', { values: { n } })}</option>
													{/each}
												</select>
											</label>
											<label
												class="flex cursor-pointer items-center justify-between gap-2 px-3 py-1.5 text-sm text-zinc-700 dark:text-zinc-300"
											>
												{$_('dashboards.widget_height')}
												<select
													class="rounded-md border border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 px-1.5 py-0.5 text-xs"
													value={fp.rowSpan}
													aria-label={$_('dashboards.widget_height')}
													onchange={(e) => {
														menuId = null;
														void resizeHeight(w, Number(e.currentTarget.value));
													}}
												>
													{#each HEIGHT_OPTIONS as n (n)}
														<option value={n}>{$_('dashboards.height_n', { values: { n } })}</option>
													{/each}
												</select>
											</label>
											<div class="my-1 border-t border-zinc-200 dark:border-zinc-800"></div>
											<button
												class="block w-full px-3 py-1.5 text-left text-sm text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-800 disabled:text-zinc-400 disabled:hover:bg-transparent dark:disabled:text-zinc-600"
												type="button"
												role="menuitem"
												aria-label={$_('dashboards.widget_move_left')}
												disabled={widgets[0]?.id === w.id}
												onclick={() => {
													menuId = null;
													void move(w, -1);
												}}
											>
												{$_('dashboards.widget_move_left')}
											</button>
											<button
												class="block w-full px-3 py-1.5 text-left text-sm text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-800 disabled:text-zinc-400 disabled:hover:bg-transparent dark:disabled:text-zinc-600"
												type="button"
												role="menuitem"
												aria-label={$_('dashboards.widget_move_right')}
												disabled={widgets.at(-1)?.id === w.id}
												onclick={() => {
													menuId = null;
													void move(w, 1);
												}}
											>
												{$_('dashboards.widget_move_right')}
											</button>
											<div class="my-1 border-t border-zinc-200 dark:border-zinc-800"></div>
											<button
												class="block w-full px-3 py-1.5 text-left text-sm text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-800"
												type="button"
												role="menuitem"
												aria-label={$_('dashboards.widget_edit')}
												onclick={() => {
													menuId = null;
													startEdit(w);
												}}
											>
												{$_('dashboards.widget_edit')}
											</button>
											<button
												class="block w-full px-3 py-1.5 text-left text-sm text-red-600 dark:text-red-400 hover:bg-zinc-100 dark:hover:bg-zinc-800"
												type="button"
												role="menuitem"
												aria-label={$_('dashboards.widget_delete')}
												onclick={() => {
													menuId = null;
													void removeWidget(w);
												}}
											>
												{$_('dashboards.widget_delete')}
											</button>
										</div>
									{/if}
								</div>
							{/if}
						</div>
					</div>
					<div class="min-h-0 flex-1 overflow-y-auto p-3 {moving ? 'hidden' : ''}">
						{#if editingId === w.id && w.kind === 'markdown'}
							<textarea
								class="w-full min-h-32 rounded-md border border-zinc-300 dark:border-zinc-700 px-2 py-1.5 font-mono text-xs"
								bind:value={draftContent}
								onkeydown={(e) => {
									if (e.key === 'Escape') cancelEdit();
									if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) void saveEdit(w);
								}}
							></textarea>
						{:else if w.kind === 'explore'}
							<ExploreView state={parseState(w.content)} names={names} />
						{:else}
							<article class="prose prose-sm dark:prose-invert">{@html renderMarkdown(w.content)}</article>
						{/if}
					</div>
					<!-- Drag-resize handles (round 7): right edge = width,
					     bottom edge = height, the corner = both. The 6px
					     overshoots stay inside the 16px grid gap; pointer
					     capture + the grid-unit math live in startDrag. -->
					<div
						class="absolute inset-y-0 -right-[6px] w-3 cursor-ew-resize touch-none {moving
							? 'hidden'
							: ''}"
						role="separator"
						aria-orientation="vertical"
						aria-label={$_('dashboards.handle_width')}
						onpointerdown={(e) => startDrag(e, w, 'x')}
					></div>
					<div
						class="absolute inset-x-0 -bottom-[6px] h-3 cursor-ns-resize touch-none {moving
							? 'hidden'
							: ''}"
						role="separator"
						aria-orientation="horizontal"
						aria-label={$_('dashboards.handle_height')}
						onpointerdown={(e) => startDrag(e, w, 'y')}
					></div>
					<div
						class="absolute -bottom-[6px] -right-[6px] h-3 w-3 cursor-nwse-resize touch-none {moving
							? 'hidden'
							: ''}"
						role="separator"
						aria-label={$_('dashboards.handle_corner')}
						onpointerdown={(e) => startDrag(e, w, 'xy')}
					></div>
				</div>
			{:else}
				<p class="text-sm text-zinc-500 dark:text-zinc-400 md:col-span-12"
					>{$_('dashboards.no_widgets')}</p
				>
			{/each}
			<!-- The grid's bounds while a drag-move is live
			     (rounds 11-12): the ring on the grid above is the
			     outer edge, and this overlay draws the 12 column
			     slot frames — each at a card's exact footprint
			     width, so a card flush-fills its slot and the
			     gutters sit between slots; without them the empty
			     space past a wide card looks like room to drop
			     next to it, but the card only snaps inside the 12
			     columns. Absolutely positioned with no grid area
			     (its containing block is the grid's padding box)
			     + pointer-events-none, so it never affects the
			     grid's layout or steals a card / handle pointer
			     target. md+ only — below md the grid is one
			     stacked column. -->
			{#if dragMove !== null}
				<div
					class="pointer-events-none absolute inset-0 hidden md:block"
					aria-hidden="true"
				>
					{#each gridSlots as slot (slot.left)}
						<div
							class="absolute inset-y-0 rounded-lg border border-sky-500/40 bg-sky-500/5"
							style="left: {slot.left}px; width: {slot.width}px;"
						></div>
					{/each}
				</div>
			{/if}
		</div>
		<!-- Drag ghost (round 9): a light clone of the dragged card
		     that follows the pointer — the card itself sits in the
		     grid as the dashed hole at the live drop slot. fixed
		     (viewport-relative) positioning keeps it glued to the
		     pointer; pointer-events: none so it never intercepts
		     the grid's pointer targets; aria-hidden — it is a
		     visual echo of a card the user already holds. -->
		{#if dragMove !== null && draggedWidget !== null && dragMove.width > 0}
			{@const preview = draggedWidget.kind === 'markdown' ? ghostLine(draggedWidget) : ''}
			<div
				class="pointer-events-none fixed z-40 flex flex-col overflow-hidden rounded-lg border border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 opacity-90 shadow-lg"
				style="left: {dragMove.x - dragMove.dx}px; top: {dragMove.y - dragMove.dy}px; width: {dragMove.width}px; height: {dragMove.height}px;"
				aria-hidden="true"
			>
				<div class="flex min-w-0 items-center gap-2 border-b border-zinc-200 dark:border-zinc-800 px-3 py-1.5">
					{#if draggedWidget.name === ''}
						<h2 class="truncate text-sm font-semibold text-zinc-400 dark:text-zinc-500 italic">
							{$_('dashboards.widget_untitled')}
						</h2>
					{:else}
						<h2 class="truncate text-sm font-semibold">{draggedWidget.name}</h2>
						<span
							class="shrink-0 rounded bg-zinc-100 dark:bg-zinc-800 px-1.5 py-0.5 text-xs text-zinc-500 dark:text-zinc-400"
						>
							{draggedWidget.kind === 'explore'
								? $_('dashboards.kind_explore')
								: $_('dashboards.kind_markdown')}
						</span>
					{/if}
				</div>
				{#if preview !== ''}
					<p class="truncate px-3 py-1.5 text-sm text-zinc-500 dark:text-zinc-400">{preview}</p>
				{/if}
			</div>
		{/if}
		<!-- Add-widget row: a kind picker; picking one opens the add
		     card (a name + — for markdown — a content textarea).
		     New widgets span 4 columns. -->
		{#if adding}
			<div
				class="space-y-3 rounded-lg border border-dashed border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 p-4"
			>
				{#if addError}
					<p class="text-sm text-red-600 dark:text-red-400">{addError}</p>
				{/if}
				<div class="flex flex-wrap items-center gap-3">
					<span class="text-sm font-medium text-zinc-600 dark:text-zinc-400">
						{addKind === 'explore'
							? $_('dashboards.widget_add_explore')
							: $_('dashboards.widget_add_markdown')}
					</span>
					<input
						class="w-64 rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5 text-sm"
						bind:value={addName}
						placeholder={$_('dashboards.widget_name_placeholder')}
						use:autofocus
						onkeydown={(e) => {
							if (e.key === 'Enter') void submitAdd();
						}}
					/>
					<button
						class="rounded-md border border-zinc-300 dark:border-zinc-700 px-4 py-2 text-sm font-medium text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-800"
						type="button"
						onclick={() => void submitAdd()}
					>
						{$_('dashboards.widget_add_button')}
					</button>
					<button
						class="text-sm font-medium text-zinc-600 dark:text-zinc-400 underline"
						type="button"
						onclick={cancelAdd}
					>
						{$_('dashboards.widget_cancel')}
					</button>
				</div>
				{#if addKind === 'explore'}
					<!-- The saved-views picker (round 16): picking a
					     saved view copies its state into the widget
					     (the widget owns its copy — deleting the view
					     later never breaks it); "new" creates a saved
					     view first (named after the widget name, default
					     state) and then the widget; "none" is the old
					     default-view path. -->
					<label class="block text-sm">
						<span class="mb-1 block font-medium">
							{$_('dashboards.widget_add_saved_view')}
						</span>
						<select
							class="w-64 rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5"
							value={addSavedView}
							onchange={onAddSavedViewChange}
						>
							<option value="">{$_('dashboards.widget_add_saved_none')}</option>
							<option value="new">{$_('dashboards.widget_add_saved_new')}</option>
							{#each addSavedViews as sv (sv.id)}
								<option value={sv.id}>{sv.name}</option>
							{/each}
						</select>
					</label>
				{/if}
				{#if addKind === 'markdown'}
					<textarea
						class="w-full min-h-32 rounded-md border border-zinc-300 dark:border-zinc-700 px-2 py-1.5 font-mono text-xs"
						bind:value={addContent}
						placeholder={$_('dashboards.widget_content_placeholder')}
					></textarea>
				{/if}
			</div>
		{:else}
			<div
				class="flex flex-wrap items-center gap-3 rounded-lg border border-dashed border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 p-4"
			>
				<span class="text-sm font-medium text-zinc-600 dark:text-zinc-400">
					{$_('dashboards.widget_add')}
				</span>
				<button
					class="rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5 text-sm"
					type="button"
					onclick={() => startAdd('explore')}
				>
					{$_('dashboards.widget_add_explore')}
				</button>
				<button
					class="rounded-md border border-zinc-300 dark:border-zinc-700 px-3 py-1.5 text-sm"
					type="button"
					onclick={() => startAdd('markdown')}
				>
					{$_('dashboards.widget_add_markdown')}
				</button>
			</div>
		{/if}
	{/if}
</section>

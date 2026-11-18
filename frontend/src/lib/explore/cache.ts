// Explore walk cache (round 41): the "walk every matching page" of the
// 3.2 List endpoint, deduped and TTL-cached. The Explore view can be
// rendered by several hosts at once (the Explore page + dashboard
// cards) and each host re-walks on every filter change — same (user,
// filter) means the same pages. This module makes one (user, filter)
// cost ONE walk: concurrent callers share the in-flight promise, and a
// completed walk is served from the TTL for its window.
//
// Key: the user's `sub` + the canonical first-page query string
// (apiQuery at offset 0 — pinned wire order, so equal filters always
// key equally). Value: the accumulated walk result.
//
// Invalidation: the 30s TTL, plus the `ftrk:data-changed` window event
// that every mutation site dispatches via notifyDataChanged() below. A
// mutation mid-walk raises the generation, so the (stale) result the
// in-flight walk finishes with is dropped instead of cached — the next
// load re-walks.
//
// SSR-safe: the listener attaches only in the browser (adapter-static
// prerenders the Explore view on node), and notifyDataChanged is a
// no-op there.

import { listTransactions, type ListParams, type Transaction } from '#lib/api/transactions';
import { apiQuery, type ExploreFilters } from './state';

/** One page of the 3.2 List endpoint (the server caps limit at 100). */
const PAGE_SIZE = 100;
/** Pages 0..9: ten full pages = the 1000-row cap note. */
const MAX_PAGES = 10;
/** A completed walk is served from the cache for this long. */
const TTL_MS = 30_000;

/** The result of one full walk (rows in the endpoint's order:
 * date_posted DESC, id DESC). */
export interface ExploreWalk {
	/** Every matched row (up to MAX_PAGES * PAGE_SIZE of them). */
	rows: Transaction[];
	/** True when the walk hit the MAX_PAGES cap (ten full pages) —
	 * the view shows the cap note. */
	capped: boolean;
	/** Date.now() when the walk completed. */
	fetchedAt: number;
}

/** The 3.2 params for one walk page — the same field mapping as
 * apiQuery, in ListParams form (absent = omitted; '' =
 * uncategorized-only). */
function walkParams(s: ExploreFilters, offset: number): ListParams {
	const p: ListParams = { limit: PAGE_SIZE, offset };
	if (s.from !== '') p.date_from = s.from;
	if (s.to !== '') p.date_to = s.to;
	if (s.account.length > 0) p.account_id = [...s.account];
	if (s.category.length > 0) p.category_id = s.category.map((c) => (c === 'none' ? '' : c));
	if (s.min !== '') p.min_amount = s.min;
	if (s.max !== '') p.max_amount = s.max;
	if (s.q !== '') p.search = s.q;
	if (s.sign !== '') p.sign = s.sign;
	return p;
}

/** The cache key: identity + the canonical first-page query string. */
function walkKey(sub: string, s: ExploreFilters): string {
	return sub + '\n' + apiQuery(s, 0).toString();
}

/** Completed walks, fresh within the TTL. */
const ttl = new Map<string, ExploreWalk>();
/** Walks currently in flight: concurrent callers share one fetch. */
const inflight = new Map<string, Promise<ExploreWalk>>();
/** Raised on every ftrk:data-changed: a walk that started before the
 * mutation must not cache its (stale) result. */
let generation = 0;

function invalidate(): void {
	ttl.clear();
	generation++;
}

// Listen once, module-wide (SSR-guarded: no window on node).
if (typeof window !== 'undefined') {
	window.addEventListener('ftrk:data-changed', invalidate);
}

/** Dispatches the invalidation event — the single spelling of
 * `ftrk:data-changed`. Every mutation site calls this after its
 * mutation lands; the cache drops its completed walks and any walk in
 * flight will skip caching. No-op on the server. */
export function notifyDataChanged(): void {
	if (typeof window === 'undefined') return;
	window.dispatchEvent(new CustomEvent('ftrk:data-changed'));
}

/** Walks every matching page (0..900; a short page ends the walk). */
async function runWalk(s: ExploreFilters): Promise<ExploreWalk> {
	const out: Transaction[] = [];
	let capped = false;
	for (let page = 0; page < MAX_PAGES; page++) {
		const batch = await listTransactions(walkParams(s, page * PAGE_SIZE));
		out.push(...batch);
		if (batch.length < PAGE_SIZE) break;
		if (page === MAX_PAGES - 1) capped = true;
	}
	return { rows: out, capped, fetchedAt: Date.now() };
}

/** Loads all matching pages for (user, filters): a fresh walk, the
 * shared in-flight one, or the TTL-cached result. A FAILED walk is not
 * cached (its in-flight entry is dropped on reject, so the next call
 * retries). `rows` are read-only in the UI — a caller that may hold
 * them long-term still copies the array, keeping the shared cache
 * private. */
export function loadExploreRows(sub: string, s: ExploreFilters): Promise<ExploreWalk> {
	const key = walkKey(sub, s);
	const hit = ttl.get(key);
	if (hit !== undefined) {
		if (Date.now() - hit.fetchedAt < TTL_MS) return Promise.resolve(hit);
		ttl.delete(key); // expired
	}
	const running = inflight.get(key);
	if (running !== undefined) return running;
	const gen = generation;
	const walk = runWalk(s).then(
		(w) => {
			inflight.delete(key);
			// A mutation landed mid-walk: its rows predate it — don't
			// cache (the in-flight sharers still receive the data they
			// asked for; the next load re-walks).
			if (gen === generation) ttl.set(key, w);
			return w;
		},
		(e) => {
			inflight.delete(key);
			throw e;
		}
	);
	inflight.set(key, walk);
	return walk;
}

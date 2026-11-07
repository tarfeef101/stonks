// Catalog name maps for the explore views (plan step 6.5): the shared
// loader behind the Explore page's and the dashboard cards' group
// labels (rows carry only ids; a missing name falls back to the id in
// aggregate).
//
// Same semantics as the explore page's catalogs effect: either list
// failing degrades to an EMPTY map for that kind — the labels fall back
// to the raw ids — and the function NEVER throws. The caller's seq
// guard stays with the caller (the page keeps catalogSeq).

import { listAccounts } from '#lib/api/accounts';
import { listCategories } from '#lib/api/categories';
import type { Names } from './state';

/** Loads the account + category name maps (id -> name) with one
 * concurrent round. A failed list yields {} for that map. */
export async function loadCatalogs(): Promise<Names> {
	const [accounts, categories] = await Promise.all([
		listAccounts().catch(() => null),
		listCategories().catch(() => null)
	]);
	return {
		accounts: accounts === null ? {} : Object.fromEntries(accounts.map((a) => [a.id, a.name])),
		categories: categories === null ? {} : Object.fromEntries(categories.map((c) => [c.id, c.name]))
	};
}

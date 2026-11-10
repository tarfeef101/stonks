<script lang="ts">
	import { resolve } from '$app/paths';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import type { RouteId } from '$app/types';
	import { onMount } from 'svelte';
	import { _ } from 'svelte-i18n';
	import { signInCallback } from '#lib/auth/client';
	import { loadSettings } from '#lib/settings';

	// OIDC redirect callback (step 1.2): the IdP lands the browser here
	// with ?code=...&state=... (plus the echoed authorize query params).
	// The root layout is ssr = false, so this page is client-only: the
	// exchange happens in onMount.

	// `return_to` is echoed back by Keycloak from the authorize request.
	// Only same-origin relative paths are honoured (reject absolute URLs,
	// protocol-relative '//evil.com', and non-relative junk).
	const rawReturnTo = new URLSearchParams(page.url.search).get('return_to');
	const returnTo =
		rawReturnTo && rawReturnTo.startsWith('/') && !rawReturnTo.startsWith('//')
			? rawReturnTo
			: null;

	let error = $state<string | null>(null);

	onMount(async () => {
		try {
			await signInCallback();
			// Onboarding gate (step 1.4): the backend's onboarded_at is the
			// source of truth (not a profile claim). Fail-open: loadSettings
			// never throws — if the GET fails we cannot positively know the
			// onboarding state, so send the user home rather than trap them
			// in the onboarding loop; the UserChip prompt still offers the
			// flow. (The layout's sub-keyed effect also GETs settings on
			// identity appearance; this page awaits its own load because it
			// must decide the redirect target before navigating.)
			// Both targets are internal paths — resolve() keeps base-path
			// deployments working. `target` is typed as a parameterless
			// route id with an optional query string: login() only ever
			// sends page.url.pathname + page.url.search, so the echoed
			// return_to is one of this app's own routes. The parameterized
			// route id is EXCLUDED from the cast because SvelteKit 3's
			// typed resolve() demands a second params argument for those
			// (ResolveArgs in $app/paths); a plain string or the full
			// RouteIdWithSearchOrHash union is rejected. At runtime this
			// is harmless — resolve() only treats bracketed ids as routes
			// and any other absolute path as a plain pathname to prefix
			// with the base path.
			const st = await loadSettings();
			const target = (st && st.onboarded_at === null ? '/onboarding' : (returnTo ?? '/')) as
				| Exclude<RouteId, '/dashboards/[id]'>
				| `${Exclude<RouteId, '/dashboards/[id]'>}?${string}`;
			goto(resolve(target), { replaceState: true });
		} catch (e) {
			// e.g. visiting /auth/callback directly with no pending state.
			error = e instanceof Error ? e.message : String(e);
		}
	});
</script>

{#if error}
	<section class="space-y-3">
		<h1 class="text-2xl font-semibold">{$_('auth.error_title')}</h1>
		<p class="text-sm text-red-600 dark:text-red-400">{error}</p>
		<a href={resolve('/')} class="text-sm underline">{$_('auth.go_home')}</a>
	</section>
{:else}
	<section class="space-y-3">
		<h1 class="text-2xl font-semibold">{$_('auth.signing_in')}</h1>
	</section>
{/if}

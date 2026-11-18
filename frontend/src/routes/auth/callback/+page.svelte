<script lang="ts">
	import { resolve } from '$app/paths';
	import { goto } from '$app/navigation';
	import type { RouteId } from '$app/types';
	import { onMount } from 'svelte';
	import { _ } from 'svelte-i18n';
	import { clearReturnTo, readReturnTo, signInCallback } from '#lib/auth/client';
	import { loadSettings } from '#lib/settings';

	// OIDC redirect callback (step 1.2): the IdP lands the browser here
	// with ?code=...&state=... (and nothing else — Keycloak drops custom
	// authorize query params). The root layout is ssr = false, so this
	// page is client-only: the exchange happens in onMount.

	// The post-login target is the localStorage stash written by login()
	// before the redirect (round 10): the callback URL can never carry
	// return_to. readReturnTo() enforces the same-origin relative-path
	// rule (rejects absolute URLs, protocol-relative '//evil.com', and
	// non-relative junk); the stash is cleared right before the
	// successful navigation, so a failed exchange keeps it for a retry.
	const returnTo = readReturnTo();

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
			// route id: login() stashes the pathname ONLY (stashReturnTo
			// trims at the first ? / #) and readReturnTo() re-validates
			// the relative-path rule, so the target is one of this app's
			// own routes. The parameterized route id is EXCLUDED from the
			// cast because SvelteKit 3's typed resolve() demands a second
			// params argument for those (ResolveArgs in $app/paths); a
			// plain string or the full RouteIdWithSearchOrHash union is
			// rejected. At runtime this is harmless — resolve() only
			// treats bracketed ids as routes and any other absolute path
			// as a plain pathname to prefix with the base path.
			const st = await loadSettings();
			const target = (st && st.onboarded_at === null ? '/onboarding' : (returnTo ?? '/')) as
				Exclude<RouteId, '/dashboards/[id]'>;
			clearReturnTo();
			goto(resolve(target), { replace: true });
		} catch (e) {
			// e.g. visiting /auth/callback directly with no pending state,
			// or a state mismatch (double-clicked sign-in): the stale user
			// was already cleared by signInCallback, so this renders the
			// error card over a clean signed-out state. The raw error
			// stays in the console only — IdP error bodies can echo the
			// callback URL (state blob included), which the UI should not
			// show.
			console.error('[auth] OIDC callback failed:', e);
			error = $_('auth.callback_failed');
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

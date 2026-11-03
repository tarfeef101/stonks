<script lang="ts">
	import { resolve } from '$app/paths';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import type { RouteIdWithSearchOrHash } from '$app/types';
	import { onMount } from 'svelte';
	import { _ } from 'svelte-i18n';
	import { user, signInCallback, profileComplete } from '#lib/auth/client';

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
			// Incomplete profile (no email claim) → onboarding (step 1.4
			// builds the flow; the redirect seam is step 1.2). Both targets
			// are internal paths — resolve() keeps base-path deployments
			// working. The cast is safe: return_to only ever carries this
			// app's own page.url (set by login()), so it is a route id —
			// the static-segment routes pass any query string through
			// unchanged.
			const target = (
				profileComplete($user) ? (returnTo ?? '/') : '/onboarding'
			) as RouteIdWithSearchOrHash;
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
		<p class="text-sm text-red-600">{error}</p>
		<a href={resolve('/')} class="text-sm underline">{$_('auth.go_home')}</a>
	</section>
{:else}
	<section class="space-y-3">
		<h1 class="text-2xl font-semibold">{$_('auth.signing_in')}</h1>
	</section>
{/if}

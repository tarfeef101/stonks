<script lang="ts">
	import { page } from '$app/state';
	import { _ } from 'svelte-i18n';
	import type { Snippet } from 'svelte';
	import { user, login } from '#lib/auth/client';

	let { children }: { children: Snippet } = $props();
</script>

<!--
	Protected route — button mode (step 1.2): when signed out we show an
	explicit sign-in button instead of auto-redirecting to the IdP. An
	automatic redirect loop is a worse UX (and harder to test manually, the
	step-1.2 DoD) than a clear "Sign in" affordance; the current URL is
	passed as `return_to` so the user lands back where they clicked.
-->
{#if $user}
	{@render children()}
{:else}
	<button
		class="rounded-md border border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 px-3 py-1.5 text-sm font-medium hover:bg-zinc-50 dark:hover:bg-zinc-800"
		onclick={() => login(page.url.pathname + page.url.search)}
	>
		{$_('auth.sign_in')}
	</button>
{/if}

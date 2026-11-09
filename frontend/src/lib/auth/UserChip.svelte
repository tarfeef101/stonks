<script lang="ts">
	import { resolve } from '$app/paths';
	import { page } from '$app/state';
	import { _ } from 'svelte-i18n';
	import { user, login, logout, displayName } from '#lib/auth/client';
	import { settings } from '#lib/settings';

	// The chip is the app's auth surface (AppShell header, step 1.2c):
	// signed out → sign-in button; signed in → name/email + sign-out, and
	// an onboarding prompt while the backend says onboarding is incomplete
	// (step 1.4: onboarded_at is the gate, not a profile claim). The
	// prompt is hidden while settings load or the load failed — it appears
	// only on positive knowledge of incompleteness, so a flaky settings
	// GET never nags.
	const name = $derived(displayName($user));
	const needsOnboarding = $derived(Boolean($user && $settings && $settings.onboarded_at === null));
</script>

{#if $user}
	<div class="flex items-center gap-3 text-sm">
		{#if needsOnboarding}
			<a
				href={resolve('/onboarding')}
				class="font-medium text-amber-700 dark:text-amber-400 hover:underline"
				title={$_('auth.complete_onboarding')}
			>
				{$_('auth.complete_onboarding')}
			</a>
		{/if}
		<span class="text-zinc-600 dark:text-zinc-400">{name}</span>
		<button
			class="rounded-md border border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 px-3 py-1 font-medium hover:bg-zinc-50 dark:hover:bg-zinc-800"
			onclick={() => void logout()}
		>
			{$_('auth.sign_out')}
		</button>
	</div>
{:else}
	<button
		class="rounded-md border border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 px-3 py-1.5 text-sm font-medium hover:bg-zinc-50 dark:hover:bg-zinc-800"
		onclick={() => login(page.url.pathname + page.url.search)}
	>
		{$_('auth.sign_in')}
	</button>
{/if}

<script lang="ts">
	import { resolve } from '$app/paths';
	import { page } from '$app/state';
	import { _ } from 'svelte-i18n';
	import { user, login, logout, profileComplete, displayName } from '#lib/auth/client';

	// The chip is the app's auth surface (AppShell header, step 1.2c):
	// signed out → sign-in button; signed in → name/email + sign-out, and
	// an onboarding prompt while the profile is incomplete (until step 1.4
	// replaces the claim-based gate with the backend settings state).
	const name = $derived(displayName($user));
</script>

{#if $user}
	<div class="flex items-center gap-3 text-sm">
		{#if !profileComplete($user)}
			<a
				href={resolve('/onboarding')}
				class="font-medium text-amber-700 hover:underline"
				title={$_('auth.complete_onboarding')}
			>
				{$_('auth.complete_onboarding')}
			</a>
		{/if}
		<span class="text-zinc-600">{name}</span>
		<button
			class="rounded-md border border-zinc-300 bg-white px-3 py-1 font-medium hover:bg-zinc-50"
			onclick={() => void logout()}
		>
			{$_('auth.sign_out')}
		</button>
	</div>
{:else}
	<button
		class="rounded-md border border-zinc-300 bg-white px-3 py-1.5 text-sm font-medium hover:bg-zinc-50"
		onclick={() => login(page.url.pathname + page.url.search)}
	>
		{$_('auth.sign_in')}
	</button>
{/if}

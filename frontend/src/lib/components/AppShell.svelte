<script lang="ts">
	import { resolve } from '$app/paths';
	import { _ } from 'svelte-i18n';
	import UserChip from '#lib/auth/UserChip.svelte';
	import type { Snippet } from 'svelte';

	let { children }: { children: Snippet } = $props();

	// Remaining nav entries land with their pages (steps 6.2–6.5, 7.3).
	const navItems = [
		{ href: '/', key: 'nav.summary' },
		{ href: '/imports', key: 'nav.imports' }
	] as const;
</script>

<div class="flex min-h-screen flex-col bg-zinc-50 text-zinc-900">
	<header class="border-b border-zinc-200 bg-white">
		<div class="mx-auto flex max-w-5xl items-center justify-between px-4 py-3">
			<a href={resolve('/')} class="font-semibold">
				{$_('app.title')}
			</a>
			<!-- The app's auth surface: sign-in button when signed out,
			     name + sign-out when signed in (plus an onboarding prompt
			     while the profile is incomplete, until step 1.4). -->
			<div class="flex items-center gap-4">
				<nav class="flex gap-4 text-sm">
					{#each navItems as item (item.href)}
						<a href={resolve(item.href)} class="text-zinc-600 hover:text-zinc-900">
							{$_(item.key)}
						</a>
					{/each}
				</nav>
				<UserChip />
			</div>
		</div>
	</header>

	<main class="mx-auto w-full max-w-5xl flex-1 px-4 py-6">
		{@render children()}
	</main>
</div>

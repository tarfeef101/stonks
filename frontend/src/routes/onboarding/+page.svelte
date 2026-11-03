<script lang="ts">
	import { resolve } from '$app/paths';
	import { _ } from 'svelte-i18n';
	import { user, profileComplete } from '#lib/auth/client';

	// Onboarding (step 1.2 placeholder). This route is the redirect target
	// for first-time sign-ins with an incomplete profile (no email claim).
	// The full flow — default currency prompt, email confirm/edit, category
	// preview → POST /api/settings — lands in step 1.4.
</script>

<section class="space-y-3">
	<h1 class="text-2xl font-semibold">{$_('onboarding.title')}</h1>

	{#if $user}
		<p class="text-sm text-zinc-600">
			{$_('onboarding.sign_in_as')}
			<span class="font-medium text-zinc-900">
				{typeof $user.profile.email === 'string'
					? $user.profile.email
					: typeof $user.profile.name === 'string'
						? $user.profile.name
						: '—'}
			</span>
		</p>
	{:else}
		<p class="text-sm text-zinc-600">
			<a href={resolve('/')} class="underline">{$_('auth.sign_in')}</a>
		</p>
	{/if}

	<p class="text-sm text-zinc-600">{$_('onboarding.placeholder')}</p>

	{#if $user && profileComplete($user)}
		<a href={resolve('/')} class="inline-block text-sm text-zinc-600 underline">
			{$_('onboarding.back_to_summary')}
		</a>
	{/if}
</section>

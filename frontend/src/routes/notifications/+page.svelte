<script lang="ts">
	import { resolve } from '$app/paths';
	import { onMount } from 'svelte';
	import { _ } from 'svelte-i18n';
	import { user } from '#lib/auth/client';
	import { ApiError } from '#lib/api/client';
	import {
		listNotifications,
		markNotificationRead,
		type Notification
	} from '#lib/api/notifications';

	// Notifications (step 3.3, surfaced in 5.5): the app-written alerts
	// (right now: the worker's "N transactions need review" after a
	// finished categorise job). Newest first (the backend's list order);
	// marking a row read updates it in place from the 200 response.

	let items = $state<Notification[]>([]);
	let loadError = $state<string | null>(null);
	let loading = $state(true);

	/** Replaces the marked row with the 200 response's updated row
	 * (read_at is now set). */
	async function markRead(id: string): Promise<void> {
		try {
			const updated = await markNotificationRead(id);
			items = items.map((n) => (n.id === id ? updated : n));
		} catch (e) {
			loadError = envelopeMessage(e);
		}
	}

	/** The backend answers 4xx with the 3.1 envelope {error: {code,
	 * message}}; ApiError folds the raw body into its message ("API
	 * 400: {...}") — carve the JSON out for the friendly line, fall
	 * back to the raw message when the body is not an envelope. */
	function envelopeMessage(e: unknown): string {
		if (e instanceof ApiError) {
			const start = e.message.indexOf('{');
			if (start !== -1) {
				try {
					const env = JSON.parse(e.message.slice(start)) as {
						error?: { message?: string };
					};
					if (env.error?.message) return env.error.message;
				} catch {
					// not JSON — fall through
				}
			}
		}
		return e instanceof Error ? e.message : String(e);
	}

	/** read_at is an RFC 3339 timestamp; shown as the locale's full
	 * date and time. */
	function readAtLabel(iso: string): string {
		return new Date(iso).toLocaleString();
	}

	// The list. (The layout restores the session reactively; a
	// signed-out load simply 401s and the list stays hidden behind the
	// sign-in prompt.)
	onMount(() => {
		void listNotifications()
			.then((nots) => {
				items = nots;
			})
			.catch((e) => {
				loadError = envelopeMessage(e);
			})
			.finally(() => {
				loading = false;
			});
	});
</script>

<section class="space-y-3">
	<h1 class="text-2xl font-semibold">{$_('notifications.title')}</h1>
	<p class="max-w-2xl text-sm text-zinc-600">{$_('notifications.intro')}</p>

	{#if !$user}
		<p class="text-sm text-zinc-600">
			{$_('notifications.sign_in_prompt')}
			<a class="underline" href={resolve('/')}>{$_('auth.sign_in')}</a>
		</p>
	{:else}
		{#if loading}
			<p class="text-sm text-zinc-600">{$_('notifications.loading')}</p>
		{:else}
			{#if loadError}
				<p class="text-sm text-red-600">{loadError}</p>
			{/if}
			{#if items.length === 0 && !loadError}
				<p class="text-sm text-zinc-500">{$_('notifications.empty')}</p>
			{:else}
				<ul class="space-y-2">
					{#each items as n (n.id)}
						<li
							class="flex items-start justify-between gap-4 rounded-lg border p-4 {n.read_at ===
							null
								? 'border-zinc-300 bg-white'
								: 'border-zinc-200 bg-zinc-50'}"
						>
							<div class="space-y-1">
								<p
									class="text-sm {n.read_at === null
										? 'font-semibold text-zinc-900'
										: 'font-medium text-zinc-600'}"
								>
									{n.title}
								</p>
								<p class="text-sm text-zinc-600">{n.body}</p>
								<p class="text-xs text-zinc-400">
									{n.read_at === null
										? $_('notifications.unread')
										: `${$_('notifications.read_on')} ${readAtLabel(n.read_at)}`}
								</p>
							</div>
							{#if n.read_at === null}
								<button
									class="shrink-0 rounded-md border border-zinc-300 px-3 py-1.5 text-sm text-zinc-700 hover:bg-zinc-100"
									type="button"
									onclick={() => void markRead(n.id)}
								>
									{$_('notifications.mark_read')}
								</button>
							{/if}
						</li>
					{/each}
				</ul>
			{/if}
		{/if}
	{/if}
</section>

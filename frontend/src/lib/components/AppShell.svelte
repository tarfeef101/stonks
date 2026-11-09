<script lang="ts">
	import { resolve } from '$app/paths';
	import { onMount } from 'svelte';
	import { _ } from 'svelte-i18n';
	import { user } from '#lib/auth/client';
	import { listAccounts } from '#lib/api/accounts';
	import { unreadNotificationCount } from '#lib/api/notifications';
	import UserChip from '#lib/auth/UserChip.svelte';
	import type { Snippet } from 'svelte';

	let { children }: { children: Snippet } = $props();

	// The full nav. The Mortgage entry is conditional: it renders only
	// while the user has an account with type "mortgage" (mortgage
	// accounts use exactly that type — pinned for step 7.1).
	const navItems = [
		{ href: '/', key: 'nav.summary' },
		{ href: '/explore', key: 'nav.explore' },
		{ href: '/dashboards', key: 'nav.dashboards' },
		{ href: '/budgets', key: 'nav.budgets' },
		{ href: '/imports', key: 'nav.imports' },
		{ href: '/review', key: 'nav.review' },
		{ href: '/notifications', key: 'nav.notifications' },
		{ href: '/mortgage', key: 'nav.mortgage' },
		{ href: '/settings', key: 'nav.settings' }
	] as const;

	// Unread-notification badge on the Notifications link, and the
	// Mortgage nav predicate. Both refresh when the identity appears or
	// changes (keyed on the sub claim with a last-seen guard —
	// oidc-client-ts re-emits the User object on every silent token
	// renewal, same reason the layout's settings effect uses one), are
	// cleared on sign-out, and are polled every 60 s while signed in (one
	// interval, both checks — a mortgage account created later shows its
	// link within a minute without a reload). A failed poll is silent:
	// each keeps its last value (the link starts hidden, so a failed
	// first fetch keeps it hidden).
	let unread = $state(0);
	let hasMortgage = $state(false);
	let lastSub: string | null = null;

	// The items to render: all but Mortgage unless the user has a
	// mortgage account.
	const visibleNavItems = $derived(
		hasMortgage ? navItems : navItems.filter((i) => i.href !== '/mortgage')
	);

	$effect(() => {
		const sub = $user?.profile.sub ?? null;
		if (sub === lastSub) return;
		lastSub = sub;
		if (sub) {
			void refreshUnread();
			void refreshMortgage();
		} else {
			unread = 0;
			hasMortgage = false;
		}
	});

	function refreshUnread(): void {
		void unreadNotificationCount()
			.then((r) => {
				unread = r.unread;
			})
			.catch(() => {
				// keep the last value — a poll hiccup is not a page error
			});
	}

	function refreshMortgage(): void {
		void listAccounts()
			.then((accounts) => {
				hasMortgage = accounts.some((a) => a.type === 'mortgage');
			})
			.catch(() => {
				// keep the last value — a poll hiccup is not a page error
			});
	}

	onMount(() => {
		const t = setInterval(() => {
			if ($user?.profile.sub) {
				refreshUnread();
				refreshMortgage();
			}
		}, 60_000);
		return () => clearInterval(t);
	});
</script>

<div class="flex min-h-screen flex-col bg-zinc-50 dark:bg-zinc-950 text-zinc-900 dark:text-zinc-100">
	<header class="border-b border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900">
		<!-- flex-wrap: 9 nav items + title + UserChip cannot share one
		     line at ~375 px, so the header row and the nav wrap. -->
		<div
			class="mx-auto flex max-w-5xl flex-wrap items-center justify-between gap-x-4 gap-y-2 px-4 py-3"
		>
			<a href={resolve('/')} class="font-semibold">
				{$_('app.title')}
			</a>
			<!-- The app's auth surface: sign-in button when signed out,
			     name + sign-out when signed in (plus an onboarding prompt
			     while the profile is incomplete, until step 1.4). -->
			<div class="flex flex-wrap items-center gap-x-4 gap-y-2">
				<nav class="flex flex-wrap gap-x-4 gap-y-1 text-sm">
					{#each visibleNavItems as item (item.href)}
						<a
							href={resolve(item.href)}
							class="inline-flex items-center gap-1 text-zinc-600 dark:text-zinc-400 hover:text-zinc-900 dark:hover:text-zinc-100"
						>
							{$_(item.key)}
							{#if item.href === '/notifications' && unread > 0}
								<span class="rounded-full bg-zinc-900 px-1.5 text-xs font-medium text-white">
									{unread}
								</span>
							{/if}
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

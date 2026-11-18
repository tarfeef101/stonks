<script lang="ts">
	import '../app.css';
	import { BrowserAgent } from '@newrelic/browser-agent';
	import { page } from '$app/state';
	import { onMount } from 'svelte';
	import { _ } from 'svelte-i18n';
	import AppShell from '#lib/components/AppShell.svelte';
	import { initSession, login, sessionExpired, user } from '#lib/auth/client';
	import { clearSettings, loadSettings, settings } from '#lib/settings';
	import { waitLocale } from '#lib/i18n';
	import type { LayoutProps } from './$types';

	let { children }: LayoutProps = $props();

	// New Relic browser agent — build-time env only (PUBLIC_ prefix; baked
	// into the bundle at build time). Both PUBLIC_NR_LICENSE_KEY and
	// PUBLIC_NR_APPLICATION_ID must be set or the agent is a no-op (its
	// built-in config validation rejects incomplete info). The app itself
	// is identified by applicationID (v1.324.x has no app_name option).
	const nrLicenseKey = import.meta.env.PUBLIC_NR_LICENSE_KEY;
	const nrApplicationId = import.meta.env.PUBLIC_NR_APPLICATION_ID;
	let newrelic: BrowserAgent | undefined;
	if (nrLicenseKey && nrApplicationId) {
		newrelic = new BrowserAgent({
			info: {
				licenseKey: nrLicenseKey,
				applicationID: nrApplicationId
			}
		});
	}

	// Don't render until the locale dictionary has loaded (en.json is a
	// dynamic import) so strings never flash untranslated.
	let ready = $state(false);
	onMount(async () => {
		// Restore the persisted OIDC session (fire-and-forget: the user
		// store updates reactively when it resolves, so first paint never
		// waits on it). Importing #lib/auth/client also wires the API
		// client's access-token provider (setTokenProvider at module load).
		void initSession();
		await waitLocale();
		ready = true;
	});

	// Keep NR page views named by client-side route (the agent's
	// soft_navigations feature tracks the transitions themselves).
	$effect(() => {
		if (newrelic) newrelic.setPageViewName(page.url.pathname);
	});

	// Settings (step 1.4): load when the identity appears, reload when the
	// identity changes, clear on sign-out. Keyed on the sub claim with a
	// last-seen guard: oidc-client-ts re-emits the User object on every
	// silent token renewal (same sub), and the store read alone would
	// refetch settings on each one.
	let lastSub: string | null = null;
	$effect(() => {
		const sub = $user?.profile.sub ?? null;
		if (sub === lastSub) return;
		lastSub = sub;
		if (sub) {
			void loadSettings();
		} else {
			clearSettings();
		}
	});

	// Theme (appearance setting): the server-side value is the source of
	// truth; the pre-paint script in app.html applied the last-saved one
	// before first paint. This effect re-applies the authoritative setting
	// on load and on every change, mirrors it to localStorage for the next
	// pre-paint, and — while the setting is 'system' — follows the
	// OS/browser prefers-color-scheme live (matchMedia change events).
	$effect(() => {
		const theme = $settings?.theme ?? 'system';
		const root = document.documentElement;
		root.classList.toggle(
			'dark',
			theme === 'dark' || (theme === 'system' && matchMedia('(prefers-color-scheme: dark)').matches)
		);
		try {
			localStorage.setItem('ftrk-theme', theme);
		} catch {
			/* storage unavailable (private mode): theme still applies */
		}
		if (theme !== 'system') return;
		const mq = matchMedia('(prefers-color-scheme: dark)');
		const onChange = (e: MediaQueryListEvent) => root.classList.toggle('dark', e.matches);
		mq.addEventListener('change', onChange);
		return () => mq.removeEventListener('change', onChange);
	});
</script>

{#if ready}
	<AppShell>
		{@render children()}
	</AppShell>

	<!-- Session-expired dialog (round 9): raised by the 401 handler in
		lib/auth/client.ts when a request 401s while a session is present
		locally — the access token went stale and the silent renewal could
		not save it. Signed-out loads are NOT caught here: they have no
		local session and each page renders its own fail-open sign-in
		prompt. "Sign in" returns to the current URL (return_to) so the
		user lands back where they were. -->
	{#if $sessionExpired}
		<div class="fixed inset-0 z-50 flex items-center justify-center bg-zinc-900/60 p-4">
			<div
				role="alertdialog"
				aria-modal="true"
				aria-labelledby="session-expired-title"
				class="w-full max-w-sm rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5 shadow-lg"
			>
				<h2 id="session-expired-title" class="text-base font-semibold">
					{$_('auth.session_expired_title')}
				</h2>
				<p class="mt-2 text-sm text-zinc-600 dark:text-zinc-400">
					{$_('auth.session_expired_body')}
				</p>
				<div class="mt-4 flex justify-end gap-2">
					<button
						class="rounded-md border border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 px-3 py-1.5 text-sm font-medium hover:bg-zinc-50 dark:hover:bg-zinc-800"
						onclick={() => sessionExpired.set(false)}
					>
						{$_('auth.session_expired_dismiss')}
					</button>
					<button
						class="rounded-md border border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 px-3 py-1.5 text-sm font-medium hover:bg-zinc-50 dark:hover:bg-zinc-800"
						onclick={() => {
							sessionExpired.set(false);
							login(page.url.pathname + page.url.search);
						}}
					>
						{$_('auth.sign_in')}
					</button>
				</div>
			</div>
		</div>
	{/if}
{/if}

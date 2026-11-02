<script lang="ts">
	import '../app.css';
	import { QueryClientProvider } from '@tanstack/svelte-query';
	import { BrowserAgent } from '@newrelic/browser-agent';
	import { page } from '$app/state';
	import { onMount } from 'svelte';
	import AppShell from '#lib/components/AppShell.svelte';
	import { queryClient } from '#lib/query';
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
		await waitLocale();
		ready = true;
	});

	// Keep NR page views named by client-side route (the agent's
	// soft_navigations feature tracks the transitions themselves).
	$effect(() => {
		if (newrelic) newrelic.setPageViewName(page.url.pathname);
	});
</script>

{#if ready}
	<QueryClientProvider client={queryClient}>
		<AppShell>
			{@render children()}
		</AppShell>
	</QueryClientProvider>
{/if}

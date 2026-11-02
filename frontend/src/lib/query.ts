import { QueryClient } from '@tanstack/svelte-query';

// Shared QueryClient for the SPA (client-only: ssr = false).
export const queryClient = new QueryClient({
	defaultOptions: {
		queries: {
			staleTime: 30_000,
			retry: 1,
			refetchOnWindowFocus: false
		}
	}
});

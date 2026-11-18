// See https://svelte.dev/docs/kit/types#app.d.ts
// for information about these interfaces
declare global {
	namespace App {
		// interface Error {}
		// interface Locals {}
		// interface PageData {}
		// interface PageState {}
		// interface Platform {}

		/**
		 * Build-time env vars (PUBLIC_ prefix — see
		 * https://svelte.dev/docs/kit/types#app-d-ts). The OIDC session
		 * wrapper (src/lib/auth/client.ts) falls back to dev defaults, so
		 * all three are optional here.
		 */
		interface Environment {
			Public: {
				/** OIDC issuer URL — dev: Keycloak realm 'finance'. */
				PUBLIC_OIDC_ISSUER?: string;
				/** Public SPA client id — dev: 'finance-spa'. */
				PUBLIC_OIDC_CLIENT_ID?: string;
				/** Access-token audience — frontend-specific, default 'finance-api'. */
				PUBLIC_OIDC_AUDIENCE?: string;
			};
		}
	}
}

export {};

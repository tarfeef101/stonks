// Public, non-secret app configuration, served at RUNTIME by the backend
// at GET /api/v1/public-config (deploy-time config — round 63). The
// browser is the only consumer; no value here is secret. Absence of a
// field means that feature is off (the backend omits empty values).
//
// Fetched once per page load, same-origin (see src/lib/api/client.ts for
// why /api resolves to the backend in dev AND prod).
export interface PublicConfig {
	oidc: {
		issuer: string;
		client_id: string;
		audience?: string;
	};
	newrelic?: {
		license_key?: string;
		application_id?: string;
	};
}

let cached: Promise<PublicConfig> | undefined;

/** Loads (and memoizes) the public config. Rejects with a clear message
 * on HTTP failure or when the required OIDC fields are missing — the
 * root layout turns that rejection into a user-visible error state. A
 * failure is NOT cached, so a retry can re-fetch. */
export function getPublicConfig(): Promise<PublicConfig> {
	if (!cached) {
		cached = (async () => {
			try {
				const res = await fetch('/api/v1/public-config', { headers: { Accept: 'application/json' } });
				if (!res.ok) throw new Error(`app configuration unavailable (HTTP ${res.status})`);
				const cfg = (await res.json()) as PublicConfig;
				if (!cfg?.oidc?.issuer || !cfg?.oidc?.client_id) {
					throw new Error('app configuration is missing required OIDC settings');
				}
				return cfg;
			} catch (e) {
				cached = undefined; // allow retry
				throw e;
			}
		})();
	}
	return cached;
}

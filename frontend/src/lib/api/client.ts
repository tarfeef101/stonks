// Typed API client: thin fetch wrapper around the backend API.
//
// - Base URL from PUBLIC_API_BASE (build-time; default '/api' — the vite dev
//   server and nginx both proxy /api to the backend).
// - Bearer token injection via a pluggable provider. Step 1.2 wires the
//   OIDC session's access token here: src/lib/auth/client.ts calls
//   setTokenProvider on module load. Until the user has signed in the
//   provider returns null — unauthenticated requests.

const API_BASE = import.meta.env.PUBLIC_API_BASE ?? '/api';

type TokenProvider = () => string | null;

let getToken: TokenProvider = () => null;

/** Installs the OIDC session's access-token provider (called by
 * src/lib/auth/client.ts on module load). */
export function setTokenProvider(provider: TokenProvider): void {
	getToken = provider;
}

export class ApiError extends Error {
	readonly status: number;

	constructor(status: number, body: string) {
		super(`API ${status}: ${body || 'no response body'}`);
		this.name = 'ApiError';
		this.status = status;
	}
}

export interface ApiInit extends Omit<RequestInit, 'body'> {
	/** JSON-serialised unless already a string. */
	body?: unknown;
}

export async function api<T>(path: string, init: ApiInit = {}): Promise<T> {
	const headers = new Headers(init.headers);
	headers.set('accept', 'application/json');
	const token = getToken();
	if (token) headers.set('authorization', `Bearer ${token}`);

	let body: BodyInit | null = null;
	if (init.body !== undefined) {
		if (typeof init.body === 'string') {
			body = init.body;
		} else {
			body = JSON.stringify(init.body);
			if (!headers.has('content-type')) headers.set('content-type', 'application/json');
		}
	}

	const res = await fetch(`${API_BASE}${path}`, { ...init, headers, body });

	if (!res.ok) {
		const text = await res.text().catch(() => '');
		throw new ApiError(res.status, text);
	}

	if (res.status === 204) return undefined as T;
	return (await res.json()) as T;
}

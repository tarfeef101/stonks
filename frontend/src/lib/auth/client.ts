// OIDC session wrapper (step 1.2) — wraps oidc-client-ts' UserManager in a
// Svelte store so pages/components stay SDK-agnostic.
//
// Why oidc-client-ts instead of the plan-named @auth0/auth0-spa-js:
// docs/TECH-DECISIONS.md D-018 — auth0-spa-js 2.30.0 hardcodes Auth0's
// /authorize, /oauth/token and /v2/logout paths (no endpoint override) and
// normalises the expected issuer to https://, so it cannot talk to the local
// Keycloak dev IdP (plan §0 bars dev HTTPS). This wrapper is
// issuer-agnostic via plain OIDC discovery: pointing PUBLIC_AUTH0_ISSUER /
// PUBLIC_AUTH0_CLIENT_ID at a real Auth0 tenant later is env-only (Auth0
// serves the same .well-known discovery document).
//
// Env (build-time, PUBLIC_ prefix — plan §6 canonical names):
//   PUBLIC_AUTH0_ISSUER     issuer URL (dev: Keycloak realm 'finance')
//   PUBLIC_AUTH0_CLIENT_ID  public SPA client id (realm: 'finance-spa')
//   PUBLIC_AUTH0_AUDIENCE   access-token audience — frontend-specific
//                           extension of the §6 names (the realm's audience
//                           mapper adds it to the access token); default
//                           'finance-api'
//
// Session persistence: WebStorageStateStore over localStorage — the user
// (incl. id_token/refresh_token) survives reloads and is restored by
// initSession(); automaticSilentRenew refreshes the access token while the
// browser session is alive (localhost origins are same-site, so the Lax
// IdP cookie flows into the silent iframe).
//
// Client-only module: the root layout exports ssr = false, so everything
// below (window access, localStorage) only ever runs in the browser.
import { writable } from 'svelte/store';
import { UserManager, WebStorageStateStore, type User } from 'oidc-client-ts';
import { setTokenProvider } from '#lib/api/client';

const ISSUER = import.meta.env.PUBLIC_AUTH0_ISSUER ?? 'http://localhost:18081/realms/finance';
const CLIENT_ID = import.meta.env.PUBLIC_AUTH0_CLIENT_ID ?? 'finance-spa';
const AUDIENCE = import.meta.env.PUBLIC_AUTH0_AUDIENCE ?? 'finance-api';

/** Reactive current user (null when signed out). */
export const user = writable<User | null>(null);

/** Non-reactive cache of the store value — read by the token provider. */
let currentUser: User | null = null;

function setStoreUser(next: User | null): void {
	currentUser = next;
	user.set(next);
}

let manager: UserManager | undefined;

function client(): UserManager {
	if (!manager) {
		manager = new UserManager({
			authority: ISSUER,
			client_id: CLIENT_ID,
			redirect_uri: `${window.location.origin}/auth/callback`,
			post_logout_redirect_uri: `${window.location.origin}/`,
			response_type: 'code', // authorization code + PKCE (S256 default)
			scope: 'openid profile email',
			stateStore: new WebStorageStateStore({ store: window.localStorage }),
			userStore: new WebStorageStateStore({ store: window.localStorage }),
			// The id_token carries profile/email claims via the realm's
			// default client scopes — no extra /userinfo round trip.
			loadUserInfo: false,
			automaticSilentRenew: true,
			extraQueryParams: { audience: AUDIENCE }
		});
	}
	return manager;
}

/**
 * Restore the persisted session and subscribe to user events. Called once
 * from the root layout's onMount.
 */
export async function initSession(): Promise<void> {
	const c = client();
	setStoreUser(await c.getUser());
	c.events.addUserLoaded((u) => setStoreUser(u));
	c.events.addUserUnloaded(() => setStoreUser(null));
	c.events.addUserSignedOut(() => setStoreUser(null));
}

/**
 * Claim-based onboarding gate: the profile counts as complete once it has
 * an email. Step 1.4 replaces this with the backend settings state.
 */
export function profileComplete(u: User | null): boolean {
	return Boolean(u?.profile?.email);
}

/**
 * Display name from profile claims (`name` → `email` fallback). Claims are
 * typed `unknown` in oidc-client-ts, so guard the types here — one place.
 */
export function displayName(u: User | null): string {
	if (!u) return '';
	const name = u.profile.name;
	const email = u.profile.email;
	return (typeof name === 'string' && name) || (typeof email === 'string' && email) || '';
}

/**
 * Redirect to the IdP login screen. `returnTo` (a same-origin relative
 * path) is sent as an authorize query param — Keycloak echoes it back on
 * the callback URL so the app can return the user to where they came from.
 */
export function login(returnTo?: string): void {
	void client().signinRedirect({
		// Per-call params so both are guaranteed on the authorize request
		// regardless of settings-merge semantics.
		extraQueryParams: {
			audience: AUDIENCE,
			...(returnTo ? { return_to: returnTo } : {})
		}
	});
}

/** Finish the redirect flow — called from the /auth/callback page. */
export async function signInCallback(): Promise<void> {
	const c = client();
	setStoreUser(await c.signinRedirectCallback(window.location.href));
}

/**
 * Sign out locally and at the IdP, then redirect back to the app root.
 * The local user is cleared BEFORE the redirect: the realm only allows
 * post-logout redirects to the app root (no signout callback route), so
 * the cleared user never rehydrates from stale state.
 */
export async function logout(): Promise<void> {
	const c = client();
	const u = await c.getUser();
	setStoreUser(null);
	void c.signoutRedirect({
		id_token_hint: u?.id_token,
		post_logout_redirect_uri: `${window.location.origin}/`
	});
}

// Wire the access-token provider for the API client (see
// src/lib/api/client.ts) — first import of this module activates it.
setTokenProvider(() => currentUser?.access_token ?? null);

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
import { setTokenProvider, setUnauthorizedHandler } from '#lib/api/client';

const ISSUER = import.meta.env.PUBLIC_AUTH0_ISSUER ?? 'http://localhost:18081/realms/finance';
const CLIENT_ID = import.meta.env.PUBLIC_AUTH0_CLIENT_ID ?? 'finance-spa';
const AUDIENCE = import.meta.env.PUBLIC_AUTH0_AUDIENCE ?? 'finance-api';

/** Reactive current user (null when signed out). */
export const user = writable<User | null>(null);

/** Reactive "session expired" flag (round 9). Set by the 401 handler below
 * when a request 401s WHILE a session is present locally — i.e. the access
 * token went stale (expired and the silent renewal could not save it).
 * A signed-out load simply 401s with no local session, and those are the
 * pages' normal fail-open sign-in prompts, not an expiry — so the flag is
 * never raised for them. Rendered by the root layout as a global dialog
 * with a button back to the IdP sign-in. Cleared by signInCallback() when
 * the redirect exchange succeeds (round 10): a 401 racing that exchange
 * belongs to the pre-login token, not to the session that just completed.
 */
export const sessionExpired = writable(false);

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
 * Display name from profile claims (`name` → `email` fallback). Claims are
 * typed `unknown` in oidc-client-ts, so guard the types here — one place.
 */
export function displayName(u: User | null): string {
	if (!u) return '';
	const name = u.profile.name;
	const email = u.profile.email;
	return (typeof name === 'string' && name) || (typeof email === 'string' && email) || '';
}

// "Return to where you were" (round 10): the target CANNOT ride the
// authorize/callback redirect — Keycloak drops custom authorize query
// params (a live probe confirmed the callback URL carries only
// code/state/session_state/iss), so the round-9 `return_to` param was dead
// weight and post-login navigation always fell back to '/'. The target is
// instead stashed in localStorage before the redirect and read back by the
// /auth/callback page after a successful exchange.
const RETURN_TO_KEY = 'ftrk-return-to';

function stashReturnTo(returnTo: string | undefined): void {
	try {
		// Stash the pathname ONLY (L44): the callback hands the target to
		// goto(resolve(...)), which wants a bare route path — a stashed
		// query/hash would survive the resolve and ride along into the
		// post-login URL. Every call site passes
		// page.url.pathname + page.url.search, so trimming here covers
		// them all.
		const path = returnTo?.split(/[?#]/)[0];
		if (path) window.localStorage.setItem(RETURN_TO_KEY, path);
		else window.localStorage.removeItem(RETURN_TO_KEY);
	} catch {
		/* storage unavailable: the callback falls back to '/' */
	}
}

/** The stashed post-login target, or null. Enforces the same-origin
 * relative-path rule (rejects absolute URLs, protocol-relative
 * '//evil.com', and non-relative junk). */
export function readReturnTo(): string | null {
	try {
		const raw = window.localStorage.getItem(RETURN_TO_KEY);
		return raw && raw.startsWith('/') && !raw.startsWith('//') ? raw : null;
	} catch {
		return null;
	}
}

/** Drops the stash — the callback calls this right before the successful
 * navigation. A failed exchange KEEPS it so a retry still lands the user
 * where they were; a later login() without a target clears it. */
export function clearReturnTo(): void {
	try {
		window.localStorage.removeItem(RETURN_TO_KEY);
	} catch {
		/* ignore */
	}
}

// Double-click guard (round 10): signinRedirect is async (OIDC discovery
// before it writes the state and navigates), so a second click inside that
// window overwrites the stateStore entry with a NEW state while the browser
// commits to the FIRST navigation — the callback then sees a state
// mismatch, the exchange throws, and the stale (expired) user stays
// persisted, re-raising the "session expired" dialog on every reload.
// Ignore clicks while a redirect is in flight; the timer is a backstop for
// paths where the promise never settles in this document.
let redirectInFlight = false;
let redirectInFlightTimer: ReturnType<typeof setTimeout> | undefined;

/**
 * Redirect to the IdP login screen. `returnTo` (a same-origin relative
 * URL — only its pathname is stashed; query/hash are dropped) is kept in
 * localStorage for the /auth/callback page to read — see the
 * RETURN_TO_KEY note above.
 */
export function login(returnTo?: string): void {
	if (redirectInFlight) return;
	redirectInFlight = true;
	if (redirectInFlightTimer) clearTimeout(redirectInFlightTimer);
	redirectInFlightTimer = setTimeout(() => {
		redirectInFlight = false;
		redirectInFlightTimer = undefined;
	}, 5000);
	stashReturnTo(returnTo);
	client()
		.signinRedirect({
			// Per-call so the audience is guaranteed on the authorize
			// request regardless of settings-merge semantics.
			extraQueryParams: { audience: AUDIENCE }
		})
		.catch(() => {
			redirectInFlight = false;
			if (redirectInFlightTimer) {
				clearTimeout(redirectInFlightTimer);
				redirectInFlightTimer = undefined;
			}
		});
}

// Suppress the 401 handler while the callback exchange is in flight and
// briefly after it succeeds (round 10): on the callback document the
// layout's initSession() first restores the PRE-LOGIN persisted user (its
// token expired — that is why we are re-authenticating), and the
// sub-keyed settings effect fires a GET with that stale token. A 401 in
// that window is expected, not an expiry — raising the dialog there is the
// "popup comes right back after re-login" bug.
let signInInFlight = false;
let suppressUntil = 0;

/** Finish the redirect flow — called from the /auth/callback page. */
export async function signInCallback(): Promise<void> {
	const c = client();
	signInInFlight = true;
	try {
		// signinCallback, NOT signinRedirectCallback: this same URL also
		// hosts the SILENT-RENEW iframe's landing. oidc-client-ts
		// dispatches on the state's request type — si:r → redirect
		// exchange in this document; si:s → notify the parent that
		// spawned the iframe, and the PARENT performs the exchange.
		// Calling signinRedirectCallback from the silent child never
		// notifies the parent, so the renew hits its 10 s timeout and the
		// user is dropped. (The store write here is inert in the child
		// case — separate document context; the WebStorageStateStore is
		// written by the parent's exchange, not by this call.)
		setStoreUser((await c.signinCallback(window.location.href)) ?? null);
		// The exchange succeeded: clear any dialog the race above raised,
		// and keep 401s from re-raising it for a short grace window while
		// the layout settles on the new user (a late 401 from the
		// stale-token settings GET can still land after the exchange).
		sessionExpired.set(false);
		suppressUntil = Date.now() + 5000;
	} catch (e) {
		// The exchange failed (state mismatch, direct visit, …). Clear the
		// stale persisted user: otherwise every reload restores the expired
		// token, the settings 401 re-raises the "session expired" dialog,
		// and the user is stuck behind a popup for a session that is
		// already dead (round 10). A clean signed-out state shows the
		// pages' normal fail-open sign-in prompts instead.
		await c.removeUser();
		setStoreUser(null);
		throw e;
	} finally {
		signInInFlight = false;
	}
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

// Wire the 401 handler: raise the "session expired" flag only when a
// session is present locally (see the sessionExpired doc above). Never
// raise it while the callback exchange is in flight or within the
// post-exchange grace window — those 401s belong to the pre-login token,
// not to the session that just completed (round 10).
setUnauthorizedHandler(() => {
	if (signInInFlight || Date.now() < suppressUntil) return;
	if (currentUser) sessionExpired.set(true);
});

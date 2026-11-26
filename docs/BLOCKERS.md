# Blockers

Gaps and deviations found while executing the plan. An entry here is a
recorded deviation, not necessarily a work stop: an item that actually
halts a step is called out in that step's commit message and in
`docs/ITERATION-1-REPORT.md`.

## B-001 — step 1.2 names `@auth0/auth0-spa-js`; it cannot talk to the local dev IdP

**Status:** resolved by design (D-018). Not a work blocker — step 1.2's
done-when is met end-to-end against the local Keycloak realm.

Step 1.2's plan text says to wrap `@auth0/auth0-spa-js`. That SDK (2.30.0,
verified from the pinned tarball's `src/` and typings) cannot
authenticate against the Keycloak dev IdP that plan §0 mandates, for
independent reasons:

1. **No endpoint override.** The core flow hardcodes Auth0's paths —
   `{domain}/authorize`, `{domain}/oauth/token`, `{domain}/v2/logout` —
   while Keycloak serves `{realm}/protocol/openid-connect/auth|token` and
   `.../logout`; `Auth0ClientOptions` offers no way to remap them.
2. **Issuer normalisation.** `getTokenIssuer()` normalises the expected
   `iss` claim to `https://…`; plan §0 bars dev HTTPS, so the local issuer
   is `http://localhost:18081/realms/finance` and every valid id_token
   would fail the SDK's strict `iss` check.
3. **No first-party discovery.** The `.well-known` discovery core in
   2.30.0's dist belongs to the `@auth0/auth0-auth-js` sub-client
   (mfa/passkey/anonymous flows), not to the core login/token/logout path.
4. **No client-side signature verification.** The SDK checks
   `alg === RS256` + claims only — no security delta versus plain OIDC
   discovery, so switching SDKs costs nothing.

**Resolution (D-018):** the frontend uses `oidc-client-ts` 3.5.0 (sole dep
`jwt-decode ^4.0.0`) behind the same wrapper API — plain OIDC discovery,
issuer-agnostic, PKCE S256 code flow, session persistence via
`WebStorageStateStore`. The SDK settings are issuer-agnostic (originally
the `PUBLIC_AUTH0_*` env, renamed to `PUBLIC_OIDC_*` by D-030, and since
round 64 / D-036 served to the browser at RUNTIME via the backend's
`GET /api/v1/public-config` instead of build-time env), so switching to a
real Auth0 tenant later is still an env-only change: Auth0 serves the
same `.well-known` discovery document and the same flow.

**Verification:** `scripts/verify.sh oidc` (scripted PKCE S256 E2E —
discovery, authorize, login form, state round-trip, token exchange,
`id_token`/`access_token` claim assertions; all 16 checks passing) plus
the manual browser E2E steps in `docs/DEVELOPMENT.md`.

# Development guide (iteration 1 — local stack)

Boots the full local stack for iteration 1: PostgreSQL, backend, frontend,
Keycloak (OIDC IdP), and Mailpit (local SMTP sink). One command: `make dev`.

## Prerequisites

- **Docker + the compose plugin** — that's it. Per plan §0 (decision D-001),
  no Go/Node/npm/terraform are installed on the host; every build and test
  runs inside a container image.
- Ports 15432, 18080, 18081, 18082, 1025, 18025 free on the host (see table
  below). If one is taken, stop whatever holds it or rebind in
  `docker-compose.dev.yml`.

## Quick start

```sh
cp .env.example .env   # .env is git-ignored; all vars have dev-safe defaults
make dev               # builds images + boots the stack (first run: a few
                       # minutes for image pulls, npm ci, Keycloak bootstrap)
make dev-ps            # status — done when every service says (healthy)
make dev-logs          # tail all logs
make dev-down          # stop (volumes are kept — realm + DB survive)
```

First boot ≈ 3–4 min (image pulls + `npm ci` + Keycloak import/start ≈ 60–90 s).
Later boots ≈ 1 min.

The `migrate` service runs `goose up` before the backend on every `make dev`
(an idempotent no-op when the schema is current). Standalone: `make migrate`,
`make migrate-down`, `make migrate-status`.

The backend needs a real `KEK` (32 bytes, 64 hex chars) in the git-ignored
`.env` before it can bootstrap a user's DEK — generate one with `make gen-kek`
(the `.env.example` value is a placeholder).

## Services and ports

| Service    | Image                    | Host port(s)      | Notes                                              |
|------------|--------------------------|-------------------|----------------------------------------------------|
| postgres   | `postgres:18` (18.6)     | 15432             | db/user/pass `finance`/`postgres`/`postgres`       |
| backend    | built from `backend/`    | 18080             | Go chi; `GET /healthz`                              |
| keycloak   | `quay.io/keycloak/keycloak:26.8.1` | 18081  | realm `finance`; admin console `http://localhost:18081/admin` |
| frontend   | built from `frontend/`   | 18082             | SvelteKit 3 **dev server** (HMR from source)        |
| mailpit    | `axllent/mailpit:v1.31.6`| 1025 (SMTP), 18025 (UI) | local SMTP sink; no mail yet in 0.4      |

llama.cpp (18090) is defined but commented out until step 5.3.

## Credentials and OIDC

| What | Value |
|---|---|
| Keycloak admin (master realm) | `admin` / `admin` (from `.env`: `KC_ADMIN_USER` / `KC_ADMIN_PASSWORD`) |
| Test user (realm `finance`) | `test` / `test1234` (`.env`: `FTRK_TEST_USER_PASSWORD`) |
| SPA client | `finance-spa` (public, code + PKCE, `directAccessGrantsEnabled`) |
| Audience client | `finance-api` (tokens carry `aud: ["finance-api"]`) |
| Issuer (browser-side = token `iss`) | `http://localhost:18081/realms/finance` |
| Discovery horizon (backend) | `http://keycloak:8080/realms/finance` (`OIDC_DISCOVERY_URL`) |

Get a token (password grant, as the frontend's PKCE flow would). The
`audience=finance-api` parameter selects the access-token `aud` (the realm's
audience mapper) — the backend verifier requires it (step 1.3):

```sh
curl -s -X POST http://localhost:18081/realms/finance/protocol/openid-connect/token \
  -d "grant_type=password&client_id=finance-spa&username=test&password=test1234&audience=finance-api"
```

Test user's account console: `http://localhost:18081/realms/finance/account`.

## Manual auth E2E (browser)

The scripted proof of the OIDC flow is `scripts/verify-oidc.sh` (run from
the repo root after `make dev`): discovery, PKCE S256 authorize, login-form
POST, state round-trip, token exchange, and `id_token`/`access_token`
claim assertions — 16 checks, all passing. The click-through equivalent:

1. Open `http://localhost:18082/`. Signed out, the header (top right)
   shows a **Sign in** button.
2. Click it — Keycloak's login screen for realm `finance` opens. The app
   carried the current URL as `return_to`; Keycloak echoes it back on the
   callback so you land where you started.
3. Log in as the test user: `test` / `test1234`.
4. The browser passes through `http://localhost:18082/auth/callback` (the
   PKCE code exchange happens there) and returns to your page. The header
   now shows the user's name and a **Sign out** button. The test user has
   an `email` claim, so the claim-based onboarding gate counts the profile
   complete and no "Complete onboarding" prompt appears; the placeholder
   route is still directly reachable at `http://localhost:18082/onboarding`.
5. Reload — the session survives (persisted in `localStorage`; restored by
   `initSession()` in the root layout on every page load) and
   `automaticSilentRenew` refreshes the access token in the background
   while the page is open.
6. Click **Sign out** — the local session is cleared, the IdP logout
   endpoint ends the Keycloak session, and you land back on `/` signed out.
7. Failure path: a bad password, stale pending state, or an unreachable
   issuer shows the callback page's "Sign-in failed" state (with a
   **Go home** button) instead of redirecting.

Env gotcha: `PUBLIC_AUTH0_*` are read by the vite dev server at boot (and
baked into production builds at build time). After changing them, do
`make dev-down && make dev` — a bare `docker compose restart` does not
re-read `.env`.

## Keycloak bootstrap semantics (verified 2026-11-03 on 26.8.1)

Realm bootstrap is `deploy/keycloak/dev-init.sh` (the container entrypoint) +
the committed realm export `deploy/keycloak/finance-realm.json`:

- **First boot** (fresh volume): the realm JSON is imported **offline** with
  `kc.sh import --override false` (the `kcadm.sh import` command was removed in
  Keycloak 26.8; `KC_BOOTSTRAP_KEYCLOAK_IMPORT` is inert at runtime) — then the
  server starts, the admin user is created with the `KC_ADMIN_PASSWORD` value,
  and the test user's password is synced.
- **Restart** (volume present): the offline import is skipped — freshness is
  detected by the H2 database artifact `/opt/keycloak/data/h2/keycloakdb.mv.db`
  in the volume (the 26.x dev-file layout puts H2 under `data/h2/`). Realm,
  admin, and the test user persist in the volume.
- The test user's password is re-synced via the raw admin API on **every**
  boot, so docs and DB stay consistent even if the realm JSON ever changes.
- **Changing `KC_ADMIN_PASSWORD` after the first boot does not take effect** —
  the server never re-syncs the admin password from env. dev-init fails fast
  with a message pointing here. Fix:

  ```sh
  make dev-down
  docker volume rm personal_finance_tracker_ftrk-kc-data
  make dev
  ```

  (Same recipe to reset Keycloak to the committed realm JSON for any reason.)

  Volume-naming gotcha: compose names its named volumes
  `<project>_<volume>`, so the real Docker volume is
  `personal_finance_tracker_ftrk-kc-data` — a bare `ftrk-kc-data` is a
  different, empty volume.

- Re-importing over an existing DB is non-destructive anyway: `--override false`
  logs `Realm 'finance' already exists. Import skipped` and exits 0.

## Frontend notes

The dev stack runs the SvelteKit **dev server** (not the nginx production
image): `npm run dev` in the container, source bind-mounted, HMR on.
`/api/*` requests from `http://localhost:18082` are proxied by vite to the
backend (target overridable via `VITE_API_PROXY_TARGET`, default
`http://localhost:18080` inside the compose network). The nginx image from
step 0.3 is the production serving path and is not used in dev.

## Troubleshooting

- **A port is already in use** — `docker compose -f docker-compose.dev.yml ps`
  will show the failing service; find the host listener (`ss -ltnp`) and free
  the port, or rebind in `docker-compose.dev.yml`.
- **`make dev` fails mid-`npm ci` with `ETIMEDOUT` (errno -110)** — transient
  npm registry drop; just re-run `make dev`.
- **Root-owned build artifacts** — container builds write `frontend/build`,
  `frontend/.svelte-kit`, `frontend/node_modules` (bind mount) and
  `backend/bin/` as root. They are git-ignored. Don't `rm -rf` them from the
  host mid-iteration; if a cleanup is ever needed, do it in a container:
  `docker run --rm -v "$PWD/frontend:/f" alpine:3.23 sh -c 'rm -rf /f/build /f/.svelte-kit'`.
- **Keycloak test-user sync 401 on boot** (`dev-init: ERROR: could not sync
  test user …` in `make dev-logs`) — the admin password in `.env` no longer
  matches the volume (see "Changing `KC_ADMIN_PASSWORD`" above).
- **Keycloak takes a while on first boot** — import ≈ 15–20 s + start-dev
  ≈ 30–40 s + a 30 s healthcheck start period; up to ~3 min is normal.

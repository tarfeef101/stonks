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
| worker     | built from `backend/`    | —                 | step 5.4 categorisation worker (`entrypoint: ["worker"]`, same image); drains `categorise_jobs` (SKIP LOCKED); no HTTP port; image `/healthz` healthcheck disabled |
| keycloak   | `quay.io/keycloak/keycloak:26.8.1` | 18081  | realm `finance`; admin console `http://localhost:18081/admin` |
| frontend   | built from `frontend/`   | 18082             | SvelteKit 3 **dev server** (HMR from source)        |
| mailpit    | `axllent/mailpit:v1.31.6`| 1025 (SMTP), 18025 (UI) | local SMTP sink; no mail yet in 0.4      |
| llama      | `ghcr.io/ggml-org/llama.cpp:server-b12124` | 18090 | opt-in: `make dev-llm` (llm profile); OpenAI-compatible `/v1`; see the LLM section |

## LLM (opt-in: `make dev-llm`)

A local OpenAI-compatible model (llama.cpp serving Qwen3-4B-Instruct-2507
Q4_K_M) for LLM categorisation. It is **off** unless you boot it:

```sh
make dev-llm        # start llama-init + llama (the `llm` compose profile)
make dev-llm-down   # stop them (the model volume persists)
make dev-llm-clean  # delete the model volume -- the next dev-llm re-downloads
```

**First boot is slow**: the llama.cpp image is pulled and the ≈2.5 GB model
is downloaded (sha256-verified into the named volume `ftrk-llm-model`).
`llama-init` is a one-shot downloader: once the file is present and valid it
exits 0 in seconds, so every later boot skips the download; `llama` then
loads the model (≈10–60 s). `make dev-llm` targets the two services by name,
so the rest of the dev stack is never restarted.

Pinned artifacts (verified 2026-11-05):

- Image: `ghcr.io/ggml-org/llama.cpp:server-b12124` — the newest published
  build tag; the CPU variant uses the plain `server` prefix (no `-cpu`
  suffix). The image ships its own healthcheck on `/health`, so no compose
  healthcheck is added.
- Model: `https://huggingface.co/unsloth/Qwen3-4B-Instruct-2507-GGUF/resolve/main/Qwen3-4B-Instruct-2507-Q4_K_M.gguf`
  — 2,497,281,120 bytes, sha256
  `3605803b982cb64aead44f6c1b2ae36e3acdb41d8e46c8a94c6533bc4c67e597`.

Manual probes (while `make dev-llm` is up):

```sh
curl -sf localhost:18090/health   # -> {"status":"ok"}
curl -sf localhost:18090/v1/models
curl -s -H 'Content-Type: application/json' \
  -d '{"model":"/models/qwen3-4b-instruct-2507-q4_k_m.gguf","temperature":0,
       "max_tokens":1024,
       "messages":[{"role":"user","content":"Reply with OK."}]}' \
  localhost:18090/v1/chat/completions
```

The server reports the model id as the **full path**
`/models/qwen3-4b-instruct-2507-q4_k_m.gguf` — that exact string is what
clients (and the step 5.4 worker) pass in `model`. The worker (cmd/worker)
calls this service when a categorisation job runs: provider = the user's
stored `llm_credentials` row when present, else the `LLM_*` env (default
`http://llama:8080/v1` in-network, model id as above); the default
categorisation test path stays mock-provider-based.

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

The scripted proof of the OIDC flow is `scripts/verify.sh oidc` (run from
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

Env gotcha: the SPA carries **no** `PUBLIC_*` build args (D-036) — it
reads its OIDC settings at runtime from the backend's
`GET /api/v1/public-config`, and the vite dev server proxies `/api` to
the backend, so the dev source of truth is the backend's `OIDC_*` vars in
`.env`. After changing them, do `make dev-down && make dev` — a bare
`docker compose restart` does not re-read `.env`.

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
step 0.3 is the production serving path and is not used in dev. It takes
**zero build args** (D-036): the build bakes in a CSP *template*, and
`frontend/nginx/entrypoint.sh` renders the security headers from the
container env (`OIDC_ISSUER` required — the container exits 2 when it is
missing) into `/tmp/nginx/headers.conf` at container start; the SPA then
fetches its settings same-origin from `GET /api/v1/public-config`.

## Helm chart (production deploy)

The production chart is `deploy/chart/stonks` (Phase 10; D-033/D-034/D-035).
All helm work runs in a container pinned to **Helm 4.3.1** — there is no
official helm image, so the Makefile uses `alpine:3.22` and verifies the
get.helm.sh tarball by sha256 on every run. **None of the targets below
needs a cluster.**

```sh
make helm-version    # smoke-test: expect v4.3.1
make helm-lint       # lint deploy/chart/stonks
make helm-template   # render release "stonks" to stdout
make chart-push      # package + push oci://ghcr.io/tarfeef101/stonks-chart
```

- The chart is consumed by Terraform `helm_release`
  (`chart = "oci://ghcr.io/tarfeef101/stonks-chart"`,
  `create_namespace: true` — the chart creates no Namespace).
  `helm-template` is therefore run without `-n`; the rendered `default`
  namespace is a local-rendering artifact, not a chart property.
- The chart takes the Secret **name** only (`secrets.existingSecret`,
  default `stonks-secrets`, created by Terraform from SSM). Secret values
  never enter helm values — they would land in the release Secret in etcd
  (D-033). Key contract: `deploy/chart/stonks/README.md`.
- The frontend Deployment consumes `OIDC_ISSUER` (required — its
  entrypoint renders the CSP/security headers from it and exits 2 when it
  is missing) plus the two NR keys (optional) from the same Secret
  (D-036); the backend's `GET /api/v1/public-config` then serves the SPA
  its OIDC settings at runtime.
- `images.*.tag` defaults to `latest`, a **sentinel** that never exists in
  the registry — Terraform MUST override it with the git short SHA the
  images were pushed from.
- `chart-push` is OPERATOR-GATED: the gh auth token needs `write:packages`
  plus a `docker login` to ghcr.io. The first push creates the
  `stonks-chart` package on GHCR; afterwards it must be linked to the
  `tarfeef101/stonks` repo in the GitHub UI (packages → edit → link to
  repo) so it stays public / anonymously pullable.

## Troubleshooting

- **A port is already in use** — `docker compose -f docker-compose.dev.yml ps`
  will show the failing service; find the host listener (`ss -ltnp`) and free
  the port, or rebind in `docker-compose.dev.yml`.
- **`make dev` fails mid-`npm ci` with `ETIMEDOUT` (errno -110)** — transient
  npm registry drop; just re-run `make dev`.
- **A tab renders SvelteKit's "500 / Internal Error" page in dev** — if the
  browser network tab shows a **504** on `/node_modules/.vite/deps/*.js`
  ("Outdated Optimize Dep"), the long-running vite dev server's dep-optimizer
  state has gone stale (a re-optimization left transforms referencing an old
  dep hash; SvelteKit's lazy route-node import then fails → client-side 500).
  The backend is fine — every API call returns 200. A hard refresh does NOT
  help (the state is server-side); restart the frontend container:
  `docker restart personal_finance_tracker-frontend-1` (boot re-runs
  `npm ci`, which wipes the bind-mounted `node_modules/.vite` cache).
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

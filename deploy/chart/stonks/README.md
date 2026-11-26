# stonks-chart

Production Helm chart for the Personal Finance Tracker (plan §10.2).
Installed by Terraform (`helm_release`, release name `stonks`,
`create_namespace: true`) — this chart is a PUBLIC artifact and must
never contain secret values.

## What it creates (release namespace `stonks`)

- Postgres `StatefulSet` (1 replica, 20Gi `longhorn` PVC) + Service + PDB
- backend `Deployment` (API) + Service + PDB + HPA
- worker `Deployment` (same image, `command: ["worker"]`) + PDB
- frontend `Deployment` (nginx:alpine) + Service + PDB
- `Job` `<release>-migrate` — a Helm **post-install / post-upgrade** hook
  running `goose up` from the migrations baked into the backend image
  (D-035: pre-hooks are infeasible — on first install the database does
  not exist yet; post-hooks keep `helm_release` automation correct on
  first install, and the backend `/readyz` keeps traffic off until the
  schema is current)
- 2 `HTTPRoute`s for `stonks.tareef.tech` (below)

It creates nothing in the `envoy` namespace: the `global` Gateway and
`tareef-tech` ListenerSet pre-exist (cluster-level, operator-owned), and
all backendRefs are same-namespace (no ReferenceGrants needed).

## Prerequisites

Since 0.1.3 the contract splits: the Secret carries **only genuinely
secret values** (keys, passwords, DSNs); everything else is a non-secret
chart value.

1. **Secret** `stonks-secrets` (name configurable via
   `secrets.existingSecret`) in the release namespace, created by
   Terraform — never by this chart. Key contract:

   | Key | Required | Purpose |
   |---|---|---|
   | `KEK` | yes | 64-hex key-encryption key |
   | `DATABASE_URL` | yes | pgx DSN for the app DB (carries the DB password) |
   | `POSTGRES_PASSWORD` | yes | postgres StatefulSet |
   | `OIDC_CLIENT_SECRET` | no | empty = graceful (PKCE public client) |
   | `NR_LICENSE_KEY` | no | empty = no telemetry |
   | `SMTP_PASS` | no | empty = mail off |
   | `LLM_API_KEY` | no | empty = LLM off |

   Missing **required** keys fail pod startup (no `optional:` flag on
   those refs). Optional keys render empty when absent, which the app
   treats as "feature disabled" (it reads `os.Getenv` — a missing
   variable IS the empty string). The frontend Deployment additionally
   consumes `NR_LICENSE_KEY` from the same secret (optional — it gates
   the NR browser agent in the runtime CSP).

2. **Required non-secret values** — the chart fails the RENDER when any
   is empty (fail fast in CI, not in a crash-looping pod):

   | Value | Purpose |
   |---|---|
   | `oidc.issuer` | Auth0 issuer URL (e.g. `https://<tenant>/`) |
   | `oidc.clientId` | first-party SPA client ID |
   | `oidc.audience` | resource-server identifier the backend enforces at boot |
   | `postgres.user` | postgres DB user (must match `DATABASE_URL`) |
   | `postgres.db` | postgres database name (must match `DATABASE_URL`) |

3. **Optional non-secret values** (empty = feature disabled, or a
   sensible default):

   | Value | Default when empty |
   |---|---|
   | `oidc.discoveryUrl` | discovery runs against `oidc.issuer` (one-horizon IdPs like Auth0) |
   | `newrelic.applicationId` | no telemetry (the browser agent also needs the `NR_LICENSE_KEY` secret) |
   | `smtp.host` / `smtp.port` / `smtp.user` / `smtp.from` / `smtp.tlsPolicy` | mail off |
   | `llm.baseUrl` / `llm.model` | LLM off |

4. **Image tags**: `images.backend.tag` / `images.frontend.tag` MUST be
   overridden with the git short SHA the images were pushed from — the
   default (`latest`) is a sentinel that never exists in the registry.

5. **Gateway API objects** in ns `envoy`: Gateway `global` (section
   `http`) and ListenerSet `tareef-tech` (section `https`), with a
   terminating certificate covering `stonks.tareef.tech`.

## Edge wiring (2 HTTPRoutes, both in the release namespace)

- `<release>-redirect` on `global/http`: `http://` → 301 to
  `https://stonks.tareef.tech` on port `16443` (the cluster's public
  HTTPS edge).
- `<release>` on `tareef-tech/https`: `PathPrefix /api` →
  `<release>-backend:8080` first, then `PathPrefix /` →
  `<release>-frontend:8080`. `/metrics` is deliberately NOT exposed at
  the edge (internal monitoring only, §10.4).

## Pull

All three GHCR packages are public and linked to the public repo in the
GitHub UI → anonymous pull, no `imagePullSecrets`.

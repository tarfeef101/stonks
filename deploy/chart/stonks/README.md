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

1. **Secret** `stonks-secrets` (name configurable via
   `secrets.existingSecret`) in the release namespace, created by
   Terraform from SSM — never by this chart. Key contract:

   | Key | Required | Purpose |
   |---|---|---|
   | `KEK` | yes | 64-hex key-encryption key |
   | `DATABASE_URL` | yes | pgx DSN for the app DB |
   | `OIDC_ISSUER` | yes | Auth0 issuer URL |
   | `OIDC_CLIENT_ID` | yes | first-party SPA client |
   | `POSTGRES_USER` / `POSTGRES_DB` / `POSTGRES_PASSWORD` | yes | postgres StatefulSet |
   | `OIDC_AUDIENCE`, `OIDC_CLIENT_SECRET`, `OIDC_DISCOVERY_URL` | no | empty = graceful |
   | `NR_LICENSE_KEY` | no | empty = no telemetry |
   | `SMTP_HOST` / `SMTP_PORT` / `SMTP_USER` / `SMTP_PASS` / `SMTP_FROM` / `SMTP_TLS_POLICY` | no | empty = mail off |
   | `LLM_BASE_URL` / `LLM_API_KEY` / `LLM_MODEL` | no | empty = LLM off |

   Missing **required** keys fail pod startup (no `optional:` flag on
   those refs). Optional keys render empty when absent, which the app
   treats as "feature disabled".

2. **Image tags**: `images.backend.tag` / `images.frontend.tag` MUST be
   overridden with the git short SHA the images were pushed from — the
   default (`latest`) is a sentinel that never exists in the registry.

3. **Gateway API objects** in ns `envoy`: Gateway `global` (section
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

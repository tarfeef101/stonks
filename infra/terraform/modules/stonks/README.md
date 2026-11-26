# stonks — child module

Terraform child module that provisions everything the Personal Finance
Tracker app needs to run: the Auth0 tenant settings + SPA client + API, the
7 AWS SSM SecureString parameters (+ secretsync IAM policy), and the
Kubernetes namespace, app Secret, and Helm release (chart
`oci://ghcr.io/tarfeef101/stonks-chart`).

Lives in the app repo under `infra/terraform/` (committed 2026-11-26). It
contains **no secret values** — every sensitive input is a variable with no
committed default (pass via `TF_VAR_*` env vars or a git-ignored `-var-file`
at apply time).

## Using it as a child module

This module declares **no provider blocks of its own** — its providers are
configured by your root and passed down with the `providers` meta-argument,
as with any normal child module. In your root:

```hcl
terraform {
  required_providers {
    kubernetes = { source = "opentofu/kubernetes", version = "~> 2.38.0" }
    helm       = { source = "opentofu/helm",       version = "~> 2.17.0" }
    aws        = { source = "opentofu/aws",        version = ">= 5.0.0" }
    auth0      = { source = "auth0/auth0",         version = ">= 1.7.0" }
  }
}

provider "auth0" {
  api_token = var.auth0_api_token   # root-level variable (sensitive)
}

provider "aws" {
  region = "ca-central-1"
}

provider "kubernetes" {
  # empty = standard discovery (in-cluster first, then KUBECONFIG /
  # ~/.kube/config)
  config_path = var.kubeconfig_path != "" ? var.kubeconfig_path : null
}

provider "helm" {
  kubernetes {   # helm 2.x: a block (3.x made it an attribute)
    config_path = var.kubeconfig_path != "" ? var.kubeconfig_path : null
  }
}

module "stonks" {
  source = "./modules/stonks" # path to this module (app repo: infra/terraform)

  providers = {
    aws        = aws
    auth0      = auth0
    kubernetes = kubernetes
    helm       = helm
  }

  app_origin        = "https://stonks.tareef.tech:16443"
  image_tag         = var.deploy_sha
  kek               = var.stonks_kek
  postgres_password = var.stonks_pg_password

  # everything else has a default (see variables.tf)
}
```

The module declares **no provider pins — floors only**, so your root stays
in control of the exact versions. The provider `source`s in `versions.tf`
must match the ones your root declares — and this is not optional
(verified empirically: a source-less child requirement resolves to
`hashicorp/<name>`, which mismatches the instance the root passes).
Constraints merge with your root's: root `~> 2.38.0` + child
`>= 2.7.0` → 2.38.x.

| Provider | Floor (child) | Why that floor (verified 2026-11-26) |
|---|---|---|
| `auth0` | `>= 1.7.0` | `require_proof_of_possession` on `auth0_client` since 1.7.0; `domain` computed on the `auth0_tenant` data source since 1.7.0 |
| `aws` | `>= 5.0.0` | SSM parameters + IAM policy resources; works with 5.x **and** 6.x (checked against the v6.0 upgrade guide — no affected resources, no `overwrite` usage) |
| `kubernetes` | `>= 2.7.0` | versioned resource names (`*_v1`) introduced in 2.7.0; `kubernetes_manifest` GA since 2.6.0 |
| `helm` | `>= 2.5.0, < 3.0.0` | OCI chart support since 2.5.0; the `< 3.0.0` ceiling is load-bearing — in helm 3.x the `set`/`set_sensitive` blocks and the provider's `kubernetes` block became attributes, which breaks the HCL in this module |

`required_version = ">= 1.5.0"` (OpenTofu-compatible). No backend is
configured — your root module's state backend applies. Never commit state.

## Files

| File | What it manages |
|---|---|
| `versions.tf` | Version floors only — no pins. The four `source`s must match your root's (see note above). No provider blocks — your root configures the providers and passes them via `providers = {}` (see usage above). |
| `auth0.tf` | `auth0_tenant` settings on the token-pinned tenant (`friendly_name`), the SPA client `finance-spa` (OIDC-conformant, PKCE) + resource server `finance-api` (identifier `finance-api`, RS256) + `email`/`profile` scopes, and a `data "auth0_tenant"` read for the tenant `domain` (the resource does not expose it). |
| `aws_ssm.tf` | 7 `/finance-tracker/<NAME>` SecureString params (placeholder values, mirroring the Secret's keys) + `finance-tracker-secretsync` IAM policy. Gated by `ssm_enabled`. |
| `kubernetes.tf` | Namespace `stonks`, Secret `stonks-secrets` (genuinely-secret values only), `helm_release` `stonks` (chart 0.1.3: non-secret config as `values` built from module variables + Auth0 outputs, plus the 3 image-tag `set`s). |
| `pg_backup.tf` | Optional daily Postgres backup CronJob + PVC (mirrors the app repo's `deploy/ops/pg-backup-cronjob.yaml`). Off by default. |
| `outputs.tf` | Issuer, client ID, tenant domain, SSM paths, policy ARN, secret name, release. |

## Prerequisites (operator-owned, out-of-band)

- **Auth0 tenant** — one-time free signup in the dashboard (accounts cannot
  be created via the Management API). Then a Management API token with
  `full` scopes (app repo `docs/AUTH0-SETUP.md`). The token pins the
  tenant — no tenant variable exists on purpose. Everything the provider
  can manage on the tenant (settings, clients, API, scopes) is managed
  here, so a basically blank new tenant is enough.
- **Cluster**: the chart creates nothing in `envoy`; the Gateway `global`
  (section `http`) and ListenerSet `tareef-tech` (section `https`, with a
  cert covering `stonks.tareef.tech`) must pre-exist. Public 80/16443 →
  30080/30443 forwarding and the `stonks.tareef.tech` DNS A record are
  edge/DNS items (app repo `docs/OPERATOR.md` §1).
- **Artifacts**: images + chart pushed to GHCR from the git SHA you deploy
  (app repo: `make images-push && make chart-push`).
- **Credentials (root level, not module variables)**: the Auth0 token on
  the root's `auth0` provider; AWS creds via the standard chain + `region`
  on the root's `aws` provider; kubeconfig via the standard discovery (or
  `config_path` on the root's `kubernetes`/`helm` providers).

## Config split: Secret vs. chart values (chart 0.1.3)

The k8s Secret `stonks-secrets` carries **only genuinely secret values**:
`KEK`, `DATABASE_URL` (derived in a local — it embeds the DB password),
`POSTGRES_PASSWORD`, plus up to four optional keys, present only when the
corresponding variable is non-empty: `OIDC_CLIENT_SECRET`, `NR_LICENSE_KEY`,
`SMTP_PASS`, `LLM_API_KEY`.

Everything else is non-secret and goes to the chart as `values`
(`local.chart_values` in `kubernetes.tf`), built from module input variables
+ Auth0 outputs: `oidc.issuer` (from the tenant domain), `oidc.clientId`
(from the SPA client), `oidc.audience`, `newrelic.applicationId`,
`smtp.{host,port,user,from,tlsPolicy}`, `llm.{baseUrl,model}`,
`postgres.{user,db}`. The 7 SSM parameters mirror the Secret's keys.

## Apply

```sh
export TF_VAR_auth0_api_token='...'      # root-level, sensitive
terraform plan
terraform apply -var app_origin=https://stonks.tareef.tech:16443 \
  -var image_tag=<git-short-sha> \
  -var kek=<64-hex> -var postgres_password='...'
```

Every secret is a **sensitive variable with no committed default** — pass
them via `TF_VAR_*` env vars or a git-ignored `-var-file`. Required module
variables, no default: `app_origin`, `image_tag`, `kek`,
`postgres_password`. Provider credentials (`auth0_api_token`, the AWS
`region`, kubeconfig) are configured on the root's provider blocks, not
here. Optional features (NR, SMTP, LLM) default to empty = feature off:
their non-secret parts are always passed as chart values (empty strings
render fine — the app treats a missing/empty env var as "feature
disabled"), and their secret parts only appear in the Secret when set.

After apply, follow the first-install verification battery in the app
repo's `docs/OPERATOR.md` §2.3.

## SSM values stay out-of-band

Terraform creates the 7 SSM parameters with **empty** placeholder values
and carries `ignore_changes = [value]` — it will never clobber operator-set
values. If you use the opt-in `secretsync` Job, set the same values in SSM
via `aws ssm put-parameter` (app repo `docs/AUTH0-SETUP.md`). The
k8s Secret managed here is built from the Terraform variables + Auth0
outputs, so the deployment does **not** depend on the SSM values being set.

Caveat (carried over from the app repo): whether AWS accepts an *empty*
SecureString value at creation is unverified. If `apply` is rejected, set
a distinct placeholder (e.g. `PLACEHOLDER_SET_VIA_CLI`) in
`aws_ssm_parameter.values` or create the parameter with its real value via
`put-parameter` first and `terraform import` it.

## Updating the deployment

1. Push new images + chart from a new git SHA (app repo Makefile targets).
2. Re-apply with `-var image_tag=<new-sha>` (and bump `chart_version` /
   `postgres_image_tag` if they moved).
3. `helm_release` re-runs; the chart's post-upgrade `goose` migration Job
   handles schema changes, and the backend `/readyz` keeps traffic off
   until the schema is current.

## What this does NOT manage

- The Auth0 tenant/account itself (one-time dashboard signup) — but all of
  its provider-manageable settings, clients, and APIs are managed here.
- Provider configuration itself — that's your root's job (see above).
- DNS, and the public edge forwarding (80/16443) — operator items.
- Anything in the `envoy` namespace (D-034).
- The KEK rotation runbook (app repo `docs/OPERATOR.md` — ad-hoc, by design, D-037).

# Auth0 + SSM setup (operator runbook)

Terraform templates in `infra/` are **validate-only in iteration 1**: no
tenant, no account, no AWS resource is ever created by repo automation
(plan §0, step 1.1). During iteration 1 the **local Keycloak dev realm is
the only IdP** (see `docs/DEVELOPMENT.md`). This runbook is what the
operator follows later, when swapping to a real Auth0 tenant — a
deployment change is then env-only (`OIDC_ISSUER` / `OIDC_CLIENT_ID`).

**Current path (2026-11-26, D-038):** provisioning is done by the
Terraform child module `infra/terraform/modules/stonks` — one module
manages the Auth0 tenant settings + SPA + API, the 7 SSM SecureStrings
(exactly the k8s Secret's key set), the `stonks-secrets` Secret
(genuinely secret values only), and the Helm release (chart 0.1.3 —
everything non-secret is a chart value). The `infra/auth0` /
`infra/aws-ssm` templates documented below are the legacy validate-only
templates — the module's 7-parameter SSM set is canonical, and the module
passes `oidc.issuer` / `oidc.clientId` to the chart directly (no manual
copying).

Since D-030 the env-var names are IdP-agnostic (`OIDC_*`, not `AUTH0_*`):
dev Keycloak and prod Auth0 use the SAME names, so swapping IdPs is a value
change, not a rename. The SSM parameter names in `infra/aws-ssm` now match
the app's `OIDC_*` env names (no exception: the module creates
`OIDC_ISSUER` / `OIDC_CLIENT_ID` / `OIDC_CLIENT_SECRET`), so the
`secretsync` Job maps each parameter onto the same-named app env var
(see the note in the SSM section below).

`make tf-validate` keeps every template green (per directory: `terraform
init -backend=false`, `validate`, `fmt -check`).

## What the templates define

| Path                        | Creates (when applied by the operator)                                              |
|-----------------------------|-------------------------------------------------------------------------------------|
| `infra/auth0/environments/test` | A test tenant's settings: SPA `finance-spa` (OIDC conformant, PKCE), API `finance-api` (RS256) + `email`/`profile` scopes |
| `infra/auth0/environments/prod` | Same, on a **separate prod tenant**                                                 |
| `infra/aws-ssm`             | Gated (`enabled`, default `false`): 16 SecureString params `/finance-tracker/<NAME>` (plan §6 names) + `finance-tracker-secretsync` IAM policy |

Outputs per Auth0 environment: `issuer` (`https://<tenant domain>`),
`client_id`, `domain`.

## Prerequisites

- Terraform ≥ 1.9 with `auth0/auth0` 1.60.0 / `hashicorp/aws` 6.71.0
  resolvable (auto-downloaded on `init`). No backend is configured — state
  is local by default; add a backend block yourself if you want remote
  state. `*.tfstate*` and `.terraform/` are git-ignored; **never commit
  state or tokens.**
- An Auth0 account (free tier) per environment — two tenants (test, prod)
  for clean isolation.
- AWS credentials with SSM + IAM policy permissions — only for the
  optional `infra/aws-ssm` part.

## Auth0 — test environment

1. **Create the test tenant** in the Auth0 dashboard (sign up / add
   tenant). Note the tenant domain from the URL, e.g.
   `myorg-dev.us.auth0.com`.
2. **Create the API token:** dashboard → *API* → *Auth0 Management API* →
   *Generate API Token*, scope `full` (the module manages tenant settings,
   clients, and APIs). Store it in your secret manager — it goes into
   `TF_VAR_auth0_api_token`, never into a committed file.
3. **Set the real app origin:** edit `app_origin` in
   `infra/auth0/environments/test/main.tf` (placeholder
   `https://test.example.com`) to the test app's origin.
4. **Apply:**

   ```sh
   cd infra/auth0/environments/test
   export TF_VAR_auth0_api_token='...'
   terraform init
   terraform apply
   ```

5. **Copy the outputs** (printed at the end of `apply`; also `terraform
   output`):

   | Output       | Where it goes                                            |
   |--------------|----------------------------------------------------------|
   | `issuer`     | `OIDC_ISSUER` (k8s Secret baseline, or SSM — below)      |
   | `client_id`  | `OIDC_CLIENT_ID`                                         |
   | `domain`     | informational                                            |

   `OIDC_CLIENT_SECRET` (a plan §6 name) stays **empty**: the SPA is
   first-party — PKCE protects the code flow and the backend verifies
   tokens via the tenant JWKS, so no secret exists to use.

6. **Create the user:** dashboard → *Users* → *Create User* (email).
   With the default Auth0 email provider the user confirms the signup
   email. Log in from the test app; the authorization prompt asks for the
   `email` and `profile` scopes on `finance-api` — grant them.

## Auth0 — prod environment

Repeat the test steps against the **prod tenant**: sign up for / create the
prod tenant, generate its own API token, set `app_origin` in
`infra/auth0/environments/prod/main.tf`, then `init` + `apply` in
`environments/prod`. Copy that environment's `issuer`/`client_id` into the
prod deployment's env.

Note: the SPA must pin its `redirect_uri` to the app origin
(`window.location.origin`) so the redirect target always matches the
registered callback URL.

## Optional: AWS SSM Parameter Store

Only if you want the plan §6 "optional hardening" (secrets in SSM instead
of plain k8s Secrets; the opt-in `secretsync` Job fetches them).

1. In `infra/aws-ssm/main.tf` set `enabled = true` and `aws_region`
   (e.g. `ca-central-1`). `kms_key_arn` stays empty unless you have a
   customer-managed key.
2. Apply (AWS credentials in the standard env vars):

   ```sh
   cd infra/aws-ssm
   terraform init
   terraform apply
   ```

   This creates the 16 `/finance-tracker/<NAME>` SecureString parameters
   **without values** plus the `finance-tracker-secretsync` IAM policy
   (exact per-parameter ARNs; `kms:Decrypt`/`GenerateDataKey` only when
   `kms_key_arn` is set).
3. **Set values out-of-band** (one per parameter; never via Terraform —
   the resource carries `ignore_changes = [value]`):

   ```sh
   read -rs KEK_VALUE && printf '\n'   # no echo, no shell history
   aws ssm put-parameter --name /finance-tracker/KEK \
     --type SecureString --value "$KEK_VALUE"
   # ... repeat for DATABASE_URL, OIDC_ISSUER, OIDC_CLIENT_ID,
   # OIDC_CLIENT_SECRET, NR_LICENSE_KEY, NR_APPLICATION_ID, SMTP_*, LLM_*
   ```

   Copy the Auth0 `issuer`/`client_id` outputs into
   `/finance-tracker/OIDC_ISSUER` and `/finance-tracker/OIDC_CLIENT_ID`
   the same way.

   **Note (D-036, round 64):** the SSM parameters were renamed
   `AUTH0_*` → `OIDC_*` to match the app's env names, and
   `NR_APPLICATION_ID` was added (16 total). **MIGRATION STEP** for
   operators who applied the old template: the module now creates NEW
   empty `OIDC_*` parameters (`ignore_changes = [value]` preserves
   operator-set values), so copy your already-set values to the new
   names, then delete the old `AUTH0_*` parameters:

   ```sh
   aws ssm put-parameter --name /finance-tracker/OIDC_ISSUER \
     --type SecureString --value '<old AUTH0_ISSUER value>'
   # ... same for OIDC_CLIENT_ID and OIDC_CLIENT_SECRET
   aws ssm delete-parameter --name /finance-tracker/AUTH0_ISSUER
   aws ssm delete-parameter --name /finance-tracker/AUTH0_CLIENT_ID
   aws ssm delete-parameter --name /finance-tracker/AUTH0_CLIENT_SECRET
   ```

   **Caveat:** whether SSM accepts an *empty* SecureString value on
   creation is not verified (AWS docs don't say). If `apply` is rejected
   on an empty value, either set a distinct placeholder in
   `aws_ssm_parameter` (e.g. `PLACEHOLDER_SET_VIA_CLI`) or create the
   parameter with its real value via `put-parameter` first and
   `terraform import` it.
4. Use the policy: attach `finance-tracker-secretsync` to the node
   instance profile (or the static-creds role) that runs the `secretsync`
   Job. The Job itself lands with the deployment work — the app always
   reads the k8s Secret, never SSM.

## Troubleshooting

- `terraform init` provider errors → the templates pin exact versions
  (`auth0 1.60.0`, `aws 6.71.0`); check network access to the registry.
- Auth0 API 403 → the token lacks the required Management API scopes
  (regenerate with `full`).
- `auth0_tenant` data source fails → the API token's tenant differs from
  the one you intend (each environment has its own token + tenant).

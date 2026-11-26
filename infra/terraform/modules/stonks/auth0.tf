# Auth0 — mirrors the app repo's infra/auth0 template.
#
# The tenant (account) itself is created out-of-band — one-time free signup
# in the dashboard (not possible via the Management API); the API token
# pins it. What Terraform manages:
#   - the tenant's SETTINGS via `auth0_tenant` (below), so a basically
#     blank new tenant gets normalized to the values this module expects;
#   - `data "auth0_tenant"` reads the tenant the token belongs to,
#     including its `domain`, which the resource does not expose.
# Note: applying `auth0_tenant` overwrites the dashboard-managed settings
# it controls with the values below (the rest are left as-is server-side).

# Tenant settings on the (out-of-band) tenant.
resource "auth0_tenant" "this" {
  friendly_name = "Finance Tracker"
}

data "auth0_tenant" "this" {}

# Browser-side first-party client: no client secret is used at runtime —
# the authorization code flow is protected by PKCE, and the backend
# verifies tokens via the tenant JWKS (issuer + audience only).
resource "auth0_client" "spa" {
  name        = "finance-spa"
  app_type    = "spa"
  description = "Single-page web app for the personal finance tracker."

  oidc_conformant             = true
  require_proof_of_possession = true
  cross_origin_auth           = true

  # Callback = app origin. The SPA must pin its redirect_uri to the origin
  # (e.g. window.location.origin) so the redirect target always matches.
  callbacks           = [var.app_origin]
  allowed_origins     = [var.app_origin]
  allowed_logout_urls = [var.app_origin]

  grant_types = ["authorization_code", "refresh_token"]
}

resource "auth0_resource_server" "finance_api" {
  name        = "finance-api"
  identifier  = "finance-api" # = OIDC_AUDIENCE — the backend requires it at boot
  signing_alg = "RS256"
}

resource "auth0_resource_server_scope" "email" {
  resource_server_identifier = auth0_resource_server.finance_api.identifier
  scope                      = "email"
  description                = "Read the user's email address."
}

resource "auth0_resource_server_scope" "profile" {
  resource_server_identifier = auth0_resource_server.finance_api.identifier
  scope                      = "profile"
  description                = "Read the user's profile (name, picture)."
}

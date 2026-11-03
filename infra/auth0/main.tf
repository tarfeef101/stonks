# Shared Auth0 module for the personal finance tracker.
#
# Called by one module instance per environment (environments/test,
# environments/prod) against a DIFFERENT Auth0 tenant each.
#
# Template-only in iteration 1 — this is never applied by repo automation
# (plan §0 / step 1.1; operator runbook: docs/AUTH0-SETUP.md).
#
# The Auth0 tenant (account) itself is created out-of-band in the Auth0
# dashboard; `data "auth0_tenant"` reads the tenant the API token belongs
# to (including its domain, which the resource does not expose as computed).
# This module then manages, per tenant:
#   - tenant settings (friendly name)
#   - SPA client `finance-spa` — OIDC conformant, PKCE enforced
#   - resource server `finance-api` (RS256) + `email` / `profile` scopes

terraform {
  required_version = ">= 1.9"

  required_providers {
    auth0 = {
      source  = "auth0/auth0"
      version = "1.60.0"
    }
  }
}

provider "auth0" {
  api_token = var.api_token
}

variable "api_token" {
  description = "Auth0 Management API token (full admin scope) for the tenant this environment manages. Never commit — pass via TF_VAR_api_token or a git-ignored -var-file."
  type        = string
  sensitive   = true
}

variable "tenant_friendly_name" {
  description = "Friendly (display) name set on the tenant."
  type        = string
}

variable "app_origin" {
  description = "Origin of the web app in this environment (scheme + host, e.g. https://app.example.com — no trailing slash, no path). Used as the SPA callback URL, CORS origin, and post-logout redirect."
  type        = string
}

data "auth0_tenant" "this" {}

resource "auth0_tenant" "this" {
  friendly_name = var.tenant_friendly_name
}

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
  identifier  = "finance-api"
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

output "issuer" {
  description = "OIDC issuer for this environment: https://<tenant domain>."
  value       = "https://${data.auth0_tenant.this.domain}"
}

output "client_id" {
  description = "Client ID of the `finance-spa` application (public value)."
  value       = auth0_client.spa.client_id
}

output "domain" {
  description = "Auth0 tenant domain (e.g. mytenant.us.auth0.com)."
  value       = data.auth0_tenant.this.domain
}

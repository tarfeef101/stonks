# TEST environment — thin caller of the shared Auth0 module (../..).
#
# Template-only in iteration 1: never applied by repo automation. Operator
# flow (create tenant + API token out-of-band, set the real app origin,
# apply): docs/AUTH0-SETUP.md.

terraform {
  required_version = ">= 1.9"

  required_providers {
    auth0 = {
      source  = "auth0/auth0"
      version = "1.60.0"
    }
  }
}

variable "auth0_api_token" {
  description = "Auth0 Management API token (full admin scope) for the TEST tenant. Never commit — pass via TF_VAR_auth0_api_token or a git-ignored -var-file."
  type        = string
  sensitive   = true
}

module "auth0" {
  source = "../.."

  api_token            = var.auth0_api_token
  tenant_friendly_name = "Personal Finance Tracker (Test)"

  # RFC 2606 placeholder — replace with the real test app origin before applying.
  app_origin = "https://test.example.com"
}

output "issuer" {
  description = "OIDC issuer for the test environment (copy to OIDC_ISSUER)."
  value       = module.auth0.issuer
}

output "client_id" {
  description = "SPA client ID for the test environment (copy to OIDC_CLIENT_ID)."
  value       = module.auth0.client_id
}

output "domain" {
  description = "Test tenant domain."
  value       = module.auth0.domain
}

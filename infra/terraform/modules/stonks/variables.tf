# ---------------------------------------------------------------------------
# Auth0 — tenant created out-of-band in the dashboard (app repo runbook:
# docs/AUTH0-SETUP.md). The Management API token that pins the tenant
# lives in the root module: it configures the root's auth0 provider,
# which is passed into this module via `providers = {}`.
# ---------------------------------------------------------------------------

variable "app_origin" {
  description = "Origin of the deployed web app (scheme + host, no trailing slash, e.g. https://stonks.tareef.tech). Used as the SPA callback, CORS origin, and post-logout redirect URL."
  type        = string
}

# ---------------------------------------------------------------------------
# AWS SSM — optional part. Values are ALWAYS set out-of-band (aws ssm
# put-parameter), never via Terraform.
# ---------------------------------------------------------------------------

variable "ssm_enabled" {
  description = "Set false to skip the 7 SSM SecureString parameters + the secretsync IAM policy (the k8s Secret does not depend on them)."
  type        = bool
  default     = true
}

variable "aws_kms_key_arn" {
  description = "ARN of a customer-managed KMS key for the SecureStrings. Empty (default) = the AWS-managed SSM key (aws/ssm)."
  type        = string
  default     = ""
}

# ---------------------------------------------------------------------------
# Kubernetes / Helm
# ---------------------------------------------------------------------------

variable "namespace" {
  description = "Release namespace."
  type        = string
  default     = "stonks"
}

variable "release_name" {
  description = "Helm release name. The chart names its objects <release>-* and expects its Secret at <release>-secrets."
  type        = string
  default     = "stonks"
}

variable "chart_version" {
  description = "Chart version pin (re-pin at each deploy; the digest of 0.1.3 is recorded in the app repo's docs/OPERATOR.md §2.1)."
  type        = string
  default     = "0.1.3"
}

variable "image_tag" {
  description = "Git short SHA the backend/frontend images were built + pushed from. The chart's `latest` defaults are sentinels that never exist in the registry."
  type        = string
}

variable "postgres_image_tag" {
  description = "Postgres image minor pin (re-verify when `postgres:18` moves; keep the backup CronJob in sync — app repo docs/OPERATOR.md §2.1/§4.1)."
  type        = string
  default     = "18.6"
}

# ---------------------------------------------------------------------------
# App config — the chart 0.1.3 contract: the `sensitive` variables below
# land in the k8s Secret; the non-sensitive ones are passed to the chart
# as non-secret values (see local.chart_values in kubernetes.tf). OIDC_
# ISSUER and OIDC_CLIENT_ID are NOT variables: they come from the Auth0
# resources in auth0.tf (chart values, never the Secret). Sensitive
# variables are never committed (TF_VAR_* or a git-ignored -var-file).
# ---------------------------------------------------------------------------

variable "kek" {
  description = "64-hex key-encryption key (generate with `make gen-kek` in the app repo)."
  type        = string
  sensitive   = true
}

variable "postgres_user" {
  description = "Postgres user for the in-cluster database."
  type        = string
  default     = "ftrk"
}

variable "postgres_db" {
  description = "Postgres database name."
  type        = string
  default     = "ftrk"
}

variable "postgres_password" {
  description = "Postgres password for the in-cluster database."
  type        = string
  sensitive   = true
}

variable "oidc_audience" {
  description = "OIDC audience the backend verifies at boot — MUST equal the Auth0 resource server identifier (`finance-api`)."
  type        = string
  default     = "finance-api"
}

variable "oidc_client_secret" {
  description = "Empty (default) for the first-party PKCE SPA — no client secret exists at runtime."
  type        = string
  default     = ""
  sensitive   = true
}

variable "nr_license_key" {
  description = "New Relic license key. Empty (default) = no telemetry (opt-in, off by default)."
  type        = string
  default     = ""
  sensitive   = true
}

variable "nr_application_id" {
  description = "New Relic browser application ID (non-secret chart value `newrelic.applicationId`). The browser agent needs both NR values; empty = no telemetry."
  type        = string
  default     = ""
}

variable "smtp_host" {
  description = "SMTP host. Empty (default) = mail off."
  type        = string
  default     = ""
}

variable "smtp_port" {
  description = "SMTP port. Empty = mail off."
  type        = string
  default     = ""
}

variable "smtp_user" {
  description = "SMTP user (non-secret chart value `smtp.user`). Empty = mail off."
  type        = string
  default     = ""
}

variable "smtp_pass" {
  description = "SMTP password. Empty = mail off."
  type        = string
  default     = ""
  sensitive   = true
}

variable "smtp_from" {
  description = "SMTP From address. Empty = mail off."
  type        = string
  default     = ""
}

variable "smtp_tls_policy" {
  description = "SMTP TLS policy. Empty = mail off."
  type        = string
  default     = ""
}

variable "llm_base_url" {
  description = "LLM endpoint base URL. Empty (default) = LLM off."
  type        = string
  default     = ""
}

variable "llm_api_key" {
  description = "LLM API key. Empty = LLM off."
  type        = string
  default     = ""
  sensitive   = true
}

variable "llm_model" {
  description = "LLM model name. Empty = LLM off."
  type        = string
  default     = ""
}

# ---------------------------------------------------------------------------
# Optional daily Postgres backup CronJob (mirrors the app repo's
# deploy/ops/pg-backup-cronjob.yaml — D-037: operator-owned ops surface).
# ---------------------------------------------------------------------------

variable "pg_backup_enabled" {
  description = "Set true to manage the backup CronJob + PVC with Terraform. Otherwise `kubectl apply -f` the app repo's deploy/ops/pg-backup-cronjob.yaml (and drop pg_backup.tf from here)."
  type        = bool
  default     = false
}

variable "pg_backup_storage_class" {
  description = "Storage class for the backup PVC (the verified cluster uses Longhorn's `longhorn`)."
  type        = string
  default     = "longhorn"
}

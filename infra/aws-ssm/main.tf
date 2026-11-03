# Optional AWS SSM Parameter Store template for the personal finance tracker.
#
# Gated: nothing is created unless `enabled = true` (default false), so the
# module is inert (and validateable) without AWS credentials.
#
# When enabled, creates (plan §6):
#   - one SecureString parameter per §6 canonical name, at
#     /finance-tracker/<NAME> — names only; the VALUES are set out-of-band
#     by the operator (aws ssm put-parameter), never via Terraform
#   - an IAM policy granting the opt-in `secretsync` Job read access to
#     exactly those parameters (plus KMS decrypt, when a KMS key is given)
#
# Template-only in iteration 1 — never applied by repo automation
# (plan §0 / step 1.1; operator runbook: docs/AUTH0-SETUP.md).

terraform {
  required_version = ">= 1.9"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "6.71.0"
    }
  }
}

provider "aws" {
  region = var.aws_region
}

variable "enabled" {
  description = "Set true to create the SSM parameters + IAM policy. Default false so the module stays inert unless opted in."
  type        = bool
  default     = false
}

variable "aws_region" {
  description = "AWS region to create the resources in (e.g. ca-central-1). Required when enabled = true."
  type        = string
}

variable "kms_key_arn" {
  description = "ARN of a customer-managed KMS key for the SecureStrings. Empty (default) = the AWS-managed SSM key (aws/ssm)."
  type        = string
  default     = ""
}

locals {
  parameter_prefix = "/finance-tracker"

  # Plan §6 canonical env names — the app and the secretsync Job expect
  # these exact parameter paths.
  parameter_names = [
    "KEK",
    "DATABASE_URL",
    "AUTH0_ISSUER",
    "AUTH0_CLIENT_ID",
    "AUTH0_CLIENT_SECRET",
    "NR_LICENSE_KEY",
    "SMTP_HOST",
    "SMTP_PORT",
    "SMTP_USER",
    "SMTP_PASS",
    "SMTP_FROM",
    "SMTP_TLS_POLICY",
    "LLM_BASE_URL",
    "LLM_API_KEY",
    "LLM_MODEL",
  ]
}

# Gated so a disabled module never resolves AWS data sources (no
# credentials required for `terraform validate` / planning while inert).
data "aws_region" "current" {
  count = var.enabled ? 1 : 0
}

data "aws_caller_identity" "current" {
  count = var.enabled ? 1 : 0
}

resource "aws_ssm_parameter" "values" {
  for_each = var.enabled ? toset(local.parameter_names) : toset([])

  name   = "${local.parameter_prefix}/${each.value}"
  type   = "SecureString"
  value  = "" # placeholder only — the operator sets the real value out-of-band (docs/AUTH0-SETUP.md)
  key_id = var.kms_key_arn != "" ? var.kms_key_arn : null

  lifecycle {
    # Never clobber operator-set values on subsequent applies.
    ignore_changes = [value]
  }
}

resource "aws_iam_policy" "secretsync" {
  count = var.enabled ? 1 : 0

  name        = "finance-tracker-secretsync"
  description = "Read access to the /finance-tracker SSM parameters for the opt-in secretsync Job (plan §6)."

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat(
      [
        {
          Sid    = "ReadFinanceTrackerParameters"
          Effect = "Allow"
          Action = ["ssm:GetParameters"]
          Resource = [
            for n in local.parameter_names : "arn:aws:ssm:${data.aws_region.current[0].name}:${data.aws_caller_identity.current[0].account_id}:parameter${local.parameter_prefix}/${n}"
          ]
        },
      ],
      var.kms_key_arn != "" ? [
        {
          Sid      = "DecryptSecureStrings"
          Effect   = "Allow"
          Action   = ["kms:Decrypt", "kms:GenerateDataKey"]
          Resource = [var.kms_key_arn]
        },
      ] : []
    )
  })
}

output "parameter_names" {
  description = "The 15 SSM parameter paths the app / secretsync Job expect."
  value = [
    for n in local.parameter_names : "${local.parameter_prefix}/${n}"
  ]
}

output "policy_arn" {
  description = "ARN of the secretsync IAM policy (null while disabled)."
  value       = var.enabled ? aws_iam_policy.secretsync[0].arn : null
}

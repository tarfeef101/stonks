# AWS SSM Parameter Store.
#
# Creates the 7 /finance-tracker/<NAME> SecureString parameters — exactly
# the keys of the k8s Secret (chart 0.1.3: only genuinely secret values) —
# as placeholders (value = "") plus the `finance-tracker-secretsync` IAM
# policy. The VALUES are always set out-of-band by the operator
# (aws ssm put-parameter) — Terraform never clobbers them
# (ignore_changes = [value]).

locals {
  ssm_parameter_prefix = "/finance-tracker"

  # The k8s Secret's keys (chart 0.1.3 contract) — the opt-in secretsync
  # Job reads exactly these parameter paths.
  ssm_parameter_names = [
    "KEK",
    "DATABASE_URL",
    "POSTGRES_PASSWORD",
    "OIDC_CLIENT_SECRET",
    "NR_LICENSE_KEY",
    "SMTP_PASS",
    "LLM_API_KEY",
  ]
}

# Gated so a disabled run never resolves AWS data sources (no credentials
# needed while inert).
data "aws_region" "current" {
  count = var.ssm_enabled ? 1 : 0
}

data "aws_caller_identity" "current" {
  count = var.ssm_enabled ? 1 : 0
}

resource "aws_ssm_parameter" "values" {
  for_each = var.ssm_enabled ? toset(local.ssm_parameter_names) : toset([])

  name   = "${local.ssm_parameter_prefix}/${each.value}"
  type   = "SecureString"
  value  = "" # placeholder only — the operator sets the real value out-of-band
  key_id = var.aws_kms_key_arn != "" ? var.aws_kms_key_arn : null

  lifecycle {
    # Never clobber operator-set values on subsequent applies.
    ignore_changes = [value]
  }
}

resource "aws_iam_policy" "secretsync" {
  count = var.ssm_enabled ? 1 : 0

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
            for n in local.ssm_parameter_names : "arn:aws:ssm:${data.aws_region.current[0].name}:${data.aws_caller_identity.current[0].account_id}:parameter${local.ssm_parameter_prefix}/${n}"
          ]
        },
      ],
      var.aws_kms_key_arn != "" ? [
        {
          Sid      = "DecryptSecureStrings"
          Effect   = "Allow"
          Action   = ["kms:Decrypt", "kms:GenerateDataKey"]
          Resource = [var.aws_kms_key_arn]
        },
      ] : []
    )
  })
}

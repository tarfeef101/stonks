output "oidc_issuer" {
  description = "OIDC issuer (https://<tenant domain>) — passed to the chart as `oidc.issuer`."
  value       = local.oidc_issuer
}

output "oidc_client_id" {
  description = "Client ID of the `finance-spa` application (public value) — passed to the chart as `oidc.clientId`."
  value       = auth0_client.spa.client_id
}

output "tenant_domain" {
  description = "Auth0 tenant domain (informational)."
  value       = data.auth0_tenant.this.domain
}

output "ssm_parameter_paths" {
  description = "The 7 SSM parameter paths (mirroring the Secret's keys) — values are set out-of-band (aws ssm put-parameter), never via Terraform."
  value = [
    for n in local.ssm_parameter_names : "${local.ssm_parameter_prefix}/${n}"
  ]
}

output "secretsync_policy_arn" {
  description = "ARN of the finance-tracker-secretsync IAM policy (null when ssm_enabled = false)."
  value       = var.ssm_enabled ? aws_iam_policy.secretsync[0].arn : null
}

output "secret_name" {
  description = "k8s Secret the chart consumes (in the release namespace)."
  value       = kubernetes_secret_v1.app.metadata[0].name
}

output "helm_release" {
  description = "Installed Helm release (name + chart version)."
  value       = "${helm_release.this.name} @ chart ${helm_release.this.version}"
}

# stonks — child module for your "kubernetes stuff" root module
#
# Generated 2026-11-26 from the personal_finance_tracker repo
# (docs/OPERATOR.md, docs/AUTH0-SETUP.md, deploy/chart/stonks/README.md,
# deploy/ops/pg-backup-cronjob.yaml). This directory is meant to live in
# YOUR private provisioning repo — it is deliberately NOT committed to the
# app repo.
#
# Provider policy: NO pins — floors only, so your root stays in control of
# the exact versions. This module declares NO provider blocks of its own:
# the root configures all four providers (auth0, aws, kubernetes, helm)
# and passes its default instances down via the `providers` meta-argument
# (see README).
#
# The `source` lines below are NOT optional and MUST equal the source
# your root declares for each provider. Verified empirically (2026-11-26):
# a source-less child requirement resolves to hashicorp/<name>, which
# mismatches the instance the root passes ("Provider type mismatch" for
# opentofu/* and auth0/auth0). If your root's sources differ, change them
# here (four lines). Version constraints merge with your root's: e.g.
# root `~> 2.38.0` + child `>= 2.7.0` → 2.38.x.
#
# Floors are the lowest releases that contain everything this module uses,
# verified against provider changelogs and 2.38.0/2.17.0/1.7.0 schema dumps
# (2026-11-26):
#   kubernetes >= 2.7.0  — versioned resource names (*_v1) introduced in
#                          2.7.0; `kubernetes_manifest` GA since 2.6.0
#   helm       >= 2.5.0  — OCI chart support (oci://ghcr.io/...)
#   auth0      >= 1.7.0  — `require_proof_of_possession` on auth0_client;
#                          `domain` computed on the auth0_tenant data source
#   aws        >= 5.0.0  — SSM parameters + IAM policy resources (stable;
#                          5.x and 6.x both fine — v6.0 upgrade guide checked)

terraform {
  # OpenTofu-compatible floor (provider inheritance, modern HCL).
  required_version = ">= 1.5.0"

  required_providers {
    auth0 = {
      source  = "auth0/auth0"
      version = ">= 1.7.0"
    }
    aws = {
      source  = "opentofu/aws"
      version = ">= 5.0.0"
    }
    kubernetes = {
      source  = "opentofu/kubernetes"
      version = ">= 2.7.0"
    }
    helm = {
      source = "opentofu/helm"
      # Ceiling: in helm 3.x the `set`/`set_sensitive` blocks and the
      # provider's `kubernetes` block became attributes — 3.x breaks the
      # HCL in this module. Your root already runs helm 2.x.
      version = ">= 2.5.0, < 3.0.0"
    }
  }

  # No backend configured on purpose: use whatever state backend your
  # root module uses. NEVER commit *.tfstate* files, tokens, or the
  # .terraform/ directory.
}

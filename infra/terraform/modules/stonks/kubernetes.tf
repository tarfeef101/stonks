# Kubernetes — release namespace, the app Secret, and the Helm release.
#
# The chart (oci://ghcr.io/tarfeef101/stonks-chart, 0.1.3+) creates
# everything else in the release namespace; it creates nothing in `envoy`
# (the cluster-level Gateway `global` + ListenerSet `tareef-tech` are
# operator-owned — app repo D-034).
#
# Config split (chart 0.1.3 contract): the Secret carries ONLY genuinely
# secret values (keys, passwords, DSNs); everything non-secret goes to the
# chart as values (local.chart_values).

locals {
  # In-cluster Postgres DSN (the chart's default topology). It embeds the
  # DB password — that is why DATABASE_URL lives in the Secret.
  database_url = "postgres://${var.postgres_user}:${var.postgres_password}@${var.namespace}-postgres:5432/${var.postgres_db}"

  oidc_issuer = "https://${data.auth0_tenant.this.domain}"

  # Optional secret keys — included in the Secret only when non-empty
  # (the chart renders their refs `optional: true`; absent = feature off).
  optional_secret_entries = merge(
    var.oidc_client_secret != "" ? { OIDC_CLIENT_SECRET = var.oidc_client_secret } : {},
    var.nr_license_key != "" ? { NR_LICENSE_KEY = var.nr_license_key } : {},
    var.smtp_pass != "" ? { SMTP_PASS = var.smtp_pass } : {},
    var.llm_api_key != "" ? { LLM_API_KEY = var.llm_api_key } : {},
  )

  # Non-secret chart values (0.1.3+). Everything here is public
  # configuration — nothing in this map may ever carry a secret value.
  # `oidc.discoveryUrl` is deliberately not set: its default (empty) runs
  # discovery against `oidc.issuer`, which is what one-horizon IdPs like
  # Auth0 want.
  chart_values = {
    oidc = {
      issuer   = local.oidc_issuer
      clientId = auth0_client.spa.client_id
      audience = var.oidc_audience
    }
    newrelic = {
      applicationId = var.nr_application_id
    }
    smtp = {
      host      = var.smtp_host
      port      = var.smtp_port
      user      = var.smtp_user
      from      = var.smtp_from
      tlsPolicy = var.smtp_tls_policy
    }
    llm = {
      baseUrl = var.llm_base_url
      model   = var.llm_model
    }
    postgres = {
      user = var.postgres_user
      db   = var.postgres_db
    }
  }
}

resource "kubernetes_namespace_v1" "this" {
  metadata {
    name = var.namespace
  }
}

# The chart requires this Secret to pre-exist (it is never created by the
# chart). Key contract: app repo deploy/chart/stonks/README.md — only
# genuinely secret values (3 required + up to 4 optional).
resource "kubernetes_secret_v1" "app" {
  metadata {
    name      = "${var.release_name}-secrets"
    namespace = kubernetes_namespace_v1.this.metadata[0].name
  }

  data = merge(
    {
      # Required by the backend / chart (their refs have no `optional:`).
      KEK               = var.kek
      DATABASE_URL      = local.database_url
      POSTGRES_PASSWORD = var.postgres_password
    },
    local.optional_secret_entries,
  )
}

resource "helm_release" "this" {
  name             = var.release_name
  chart            = "oci://ghcr.io/tarfeef101/stonks-chart"
  version          = var.chart_version
  create_namespace = true
  namespace        = var.namespace
  depends_on       = [kubernetes_secret_v1.app]

  # Non-secret config (0.1.3+): built from module input variables + Auth0
  # outputs. A single `values` block rather than `set_list`: the Helm
  # provider's `set_list` only produces LIST-typed values and none of
  # these chart keys are lists.
  values = [yamlencode(local.chart_values)]

  # SENTINEL OVERRIDE — both image tags MUST be the git short SHA the
  # images were built + pushed from (the chart's `latest` defaults never
  # exist in the registry).
  # (Helm provider 2.x: `set` is a block, not a list attribute.)
  set {
    name  = "images.backend.tag"
    value = var.image_tag
  }
  set {
    name  = "images.frontend.tag"
    value = var.image_tag
  }
  set {
    name  = "postgres.image.tag"
    value = var.postgres_image_tag
  }
}

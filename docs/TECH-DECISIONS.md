# Technical decisions log

Append-only record of implementation-time decisions. Supplements the canonical
implementation plan (operator planning doc; iteration 1 = steps 0.1–9.2, local-only).

## Decisions

| ID | Date | Decision | Rationale / notes |
|----|------|----------|-------------------|
| D-001 | 2026-11-02 | **Docker-only toolchain:** all build/test/lint/fmt/migrate/tf-validate work runs in container images (`golang:1.27.3`, `node:26.13.0`, `hashicorp/terraform`); no host toolchain installs; nothing lands on the host PATH | User directive (2026-11-02). Host prerequisite: docker + compose (already present). testcontainers integration tests run in the `golang:1.27.3` container with the host `docker.sock` mounted so tests spawn their own ephemeral containers (standard CI pattern; no host modification). |
| D-002 | 2026-11-02 | Go module path = `finance-tracker` (single element) | Repo is self-hosted (gogs; no public VCS URL at execution time); a single-element module path is legal for a never-fetched module and avoids inventing a domain. |
| D-003 | 2026-11-02 | Node pinned to **26.13.0** (Active LTS line 26; latest 26.x as of 2026-11-02 per nodejs.org dist index) | Plan §1 requires the exact 26.x minor be recorded at first use; `node:26.13.0` is the image tag. |
| D-004 | 2026-11-02 | `hashicorp/terraform` image tag stays `latest` until step 1.1 | Exact version verified + pinned at step 1.1 (first `tf-validate` run); recorded here then. |
| D-005 | 2026-11-02 | New Relic `go-agent/v3` pinned **v3.45.0**; init is `app, err := newrelic.NewApplication(opts...)` (returns an error on bad config), options use the `Config*` prefix (`ConfigAppName`, `ConfigLicense`, `ConfigEnabled`, …); HTTP instrumentation is the nil-safe `_, handler := newrelic.WrapHandle(app, pattern, handler)` (returns `(string, http.Handler)`); with no license the agent is disabled explicitly via `ConfigEnabled(false)` | Verified in module source: `Config.validate()` rejects an empty license while `Enabled` is true (default) with `errLicenseLen`, so no-license runs must pass `ConfigEnabled(false)`; `AppHTTPHandler` and `NewLogLogger` no longer exist in v3.45.x. |
| D-006 | 2026-11-02 | `prometheus/client_golang` pinned **v1.25.0**; process collector is `prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{})` | Type renamed from `ProcessOpts` (v1.25); verified in module source before wiring into `/metrics`. |
| D-007 | 2026-11-02 | Backend dependency set at first `go get`: chi **v5.5.0**, pgx/v5 **v5.12.0**, prometheus/client_golang **v1.25.0** (D-006), newrelic/go-agent/v3 **v3.45.0** (D-005) | All latest as of 2026-11-02; minor bumps may land with later `go get`s (goose, cron, testcontainers-go at their first-use steps). |

## Change log

- 2026-11-02: seeded (step 0.1). Verified stack table below re-checked at implementation time per plan §1.
- 2026-11-02: step 0.2 — added D-005…D-007 (NR v3.45.0 API + no-license no-op, client_golang v1.25.0 process collector, first backend dep set).

## Verified stack (plan §1, Nov 2026)

| Choice | Verified version / fact | Notes |
|---|---|---|
| Go | **1.27.3** (latest stable; 1.27 line released Aug 19 2026) | pin in `go.mod` + `.tool-versions` |
| Node.js | **26** (Active LTS since Oct 28 2026; Node 24 entered maintenance Oct 20 2026) | pinned 26.13.0 (D-003) |
| Toolchain | **Docker-only** (D-001) | no host installs, no host PATH changes |
| Frontend | **Svelte 5.57.x + SvelteKit 3.0** (new major, released Oct 1 2026), TypeScript | `@sveltejs/adapter-static` SPA mode — verify 3.x adapter behavior at scaffold (new major) |
| Database | **PostgreSQL 18** (18.0 Sep 25 2025; 18.3 confirmed Feb 26 2026; monthly patches continue) | image tag `postgres:18`; record exact minor when pinning (step 10.1) |
| Cluster | **k3s v1.37.1+k3s1 as installed** (K8s 1.37 "Garhwal", Aug 26 2026) | do not target a different distro (Phase 10 — not iteration 1) |
| Gateway | **Envoy Gateway v1.9.3 + Envoy v1.39.5 as installed** (ns `envoy`); Gateway API v1 (GA), v1.5 stable Apr 2026 | bundled rate-limit service + redis present (Phase 10 — not iteration 1) |
| Storage | **Longhorn v1.13.0 as installed**; SC `longhorn` is cluster **default** | multi-replica across 4 nodes (Phase 10 — not iteration 1) |
| Helm | **4.3.1** (Helm 4 since Nov 2025; Server-Side Apply default, WASM plugins) | pin tool version in Makefile/tool-versions (Phase 10 — not iteration 1) |
| LLM server | **llama.cpp ~b12032** (latest as of Oct 31 2026; OpenAI-compatible REST API) | pin exact release tag in step 5.3 |
| LLM model | **4B-class Qwen instruct, GGUF Q4_K_M ≈ 2.5 GB** (Qwen3-4B-Instruct-2507 confirmed; Qwen3.5-4B appeared on HF late Oct 2026) | pick newest 4B-class Qwen instruct Q4_K_M at step 5.3, record artifact + SHA |
| Auth | **Auth0 free tier: 25,000 MAU**, unlimited connections | SPA app + `finance-api` audience; test + prod tenants via Terraform (templates only in iteration 1; local Keycloak is the IdP) |
| Backend auth lib | `auth0/go-jwt-middleware` v3 with OIDC discovery | issuer-agnostic — Keycloak ↔ Auth0 via env only |
| Monitoring | **New Relic free: 100 GB ingest/month**, 1 full user; `newrelic/go-agent/v3`; `@newrelic/browser-agent` 1.324.1 | NR agent no-ops without license in iteration 1 |
| Email | **Generic SMTP** (no vendor): `jhillyerd/enmime` + stdlib `net/smtp` | config via `SMTP_*` env; Mailpit sink in iteration 1 |
| Secrets (optional) | **AWS SSM Parameter Store, standard tier: free** (10k params, ≤4 KB each, 100k API calls/mo free) | only if operator wants more than k3s SE (Phase 10 — not iteration 1) |
| Go libs | chi v5, pgx v5 (pool), prometheus/client_golang, goose, robfig/cron v3, testcontainers-go | resolved at step 0.2: chi v5.5.0, pgx/v5 v5.12.0, client_golang v1.25.0, go-agent v3.45.0 (D-005/D-006/D-007); remaining libs at first use |
| FE libs | `@tanstack/svelte-query`, `svelte-i18n` (en only), `@auth0/auth0-spa-js`, ECharts (via thin wrapper), Tailwind v4 | verify `svelte-echarts` maintenance at 6.4; roll own ~30-line wrapper if stale |

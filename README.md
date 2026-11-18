# Personal Finance Tracker

Self-hosted personal finance app: import Canadian bank data (CSV/OFX/QFX/JSON),
categorise transactions with a local or cloud LLM, and explore spending with
budgets, dashboards, and a mortgage what-if tool. Per-user encryption at rest.

- Requirements: [DESIGN.md](DESIGN.md)
- Decisions log: [docs/TECH-DECISIONS.md](docs/TECH-DECISIONS.md)
- Dev setup: `docs/DEVELOPMENT.md` (lands in step 0.4)

## Quickstart (once the dev stack lands — step 0.4)

    cp .env.example .env   # dev placeholders only
    make dev               # postgres:18 + backend + frontend + Keycloak + Mailpit
    scripts/verify.sh all  # scripted E2E: OIDC flow + AuthN checks

## Make targets

`make help` lists everything: build / test / lint / fmt / migrate / dev /
images / images-push / tf-validate.

All toolchain work runs inside Docker — nothing is installed on the host.

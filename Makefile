# Personal Finance Tracker — top-level Makefile
#
# Docker-only toolchain (plan §0 hard rule / user directive): every
# build/test/lint/fmt/migrate operation runs inside a container image.
# Nothing is installed on the host, nothing lands on the host PATH.
# Host prerequisite: docker + compose (already present).
#
# Toolchain images (pins mirrored in .tool-versions):
#   golang:1.27.3          backend build/test/lint/fmt
#   node:26.13.0           frontend build/lint/fmt (exact 26.x minor recorded in docs/TECH-DECISIONS.md)
#   hashicorp/terraform:1.17.1   tf-validate (pinned at step 1.1, D-017)
#
# NOTE: every guarded target uses a single-shell `if [ ... ]; then ...; else ...; fi`
# recipe line — a bare `test ... || exit 0` line would NOT stop subsequent recipe
# lines (each line runs in its own shell).

SHELL := /bin/bash
.DEFAULT_GOAL := help

GO_IMG   ?= golang:1.27.3
NODE_IMG ?= node:26.13.0
TF_IMG   ?= hashicorp/terraform:1.17.1 # pinned at step 1.1 (D-017; `latest` was v1.17.1 on 2026-11-03)
COMPOSE  ?= docker compose
ROOT     := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))

# Named cache volumes (Docker-managed storage — not host paths)
GO_BUILD_VOL  := ftrk-go-build
GO_MOD_VOL    := ftrk-go-modcache
NPM_CACHE_VOL := ftrk-npm-cache

DOCKER_GO = docker run --rm -v $(ROOT)/backend:/src -w /src -v $(GO_BUILD_VOL):/root/.cache/go-build -v $(GO_MOD_VOL):/go/pkg/mod $(GO_IMG)
# testcontainers tests spawn ephemeral containers via the host daemon (standard CI
# pattern). NOTE: the extra mount must stay BEFORE the image name — appending it after
# $(DOCKER_GO) would make docker parse it as a command argument.
DOCKER_GO_TC = docker run --rm -v $(ROOT)/backend:/src -w /src -v $(GO_BUILD_VOL):/root/.cache/go-build -v $(GO_MOD_VOL):/go/pkg/mod -v /var/run/docker.sock:/var/run/docker.sock $(GO_IMG)
DOCKER_NODE = docker run --rm -v $(ROOT)/frontend:/app -w /app -v $(NPM_CACHE_VOL):/root/.npm $(NODE_IMG)

.PHONY: help
help: ## List this Makefile's targets
	@grep -E '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

# ---------- build ----------

.PHONY: build
build: backend-build frontend-build ## Build everything (backend + frontend)

.PHONY: backend-build
backend-build: ## Build Go module + cmd/api and cmd/worker (golang:1.27.3)
	@if [ -f backend/go.mod ]; then \
	  $(DOCKER_GO) sh -c 'go build ./... && go build -o bin/api ./cmd/api && go build -o bin/worker ./cmd/worker'; \
	else \
	  echo "skip: backend lands in step 0.2"; \
	fi

.PHONY: frontend-build
frontend-build: ## Build frontend SPA bundle (node:26)
	@if [ -f frontend/package.json ]; then \
	  $(DOCKER_NODE) sh -c 'npm ci && npm run build'; \
	else \
	  echo "skip: frontend lands in step 0.3"; \
	fi

# ---------- test ----------

.PHONY: test
test: backend-test ## Run all tests

.PHONY: backend-test
backend-test: ## Backend unit + testcontainers integration tests (golang:1.27.3)
	@if [ -f backend/go.mod ]; then \
	  $(DOCKER_GO_TC) sh -c 'go test ./...'; \
	else \
	  echo "skip: backend lands in step 0.2"; \
	fi

# ---------- lint / fmt ----------

.PHONY: lint
lint: backend-lint frontend-lint ## Lint everything

.PHONY: backend-lint
backend-lint: ## go vet + gofmt check (golang:1.27.3)
	@if [ -f backend/go.mod ]; then \
	  $(DOCKER_GO) sh -c 'set -e; go vet ./...; bad=$$(gofmt -l .); [ -z "$$bad" ] || { echo "gofmt needed on:"; echo "$$bad"; exit 1; }'; \
	else \
	  echo "skip: backend lands in step 0.2"; \
	fi

.PHONY: frontend-lint
frontend-lint: ## svelte-check + eslint (node:26)
	@if [ -f frontend/package.json ]; then \
	  $(DOCKER_NODE) sh -c 'npm ci && npm run check && npm run lint'; \
	else \
	  echo "skip: frontend lands in step 0.3"; \
	fi

.PHONY: fmt
fmt: backend-fmt frontend-fmt ## Format everything

.PHONY: backend-fmt
backend-fmt: ## gofmt -w (golang:1.27.3)
	@if [ -f backend/go.mod ]; then \
	  $(DOCKER_GO) sh -c 'gofmt -w .'; \
	else \
	  echo "skip: backend lands in step 0.2"; \
	fi

.PHONY: frontend-fmt
frontend-fmt: ## prettier --write (node:26)
	@if [ -f frontend/package.json ]; then \
	  $(DOCKER_NODE) sh -c 'npm ci && npm run format'; \
	else \
	  echo "skip: frontend lands in step 0.3"; \
	fi

# ---------- migrations ----------

.PHONY: migrate
migrate: ## Apply DB migrations (goose up) via the compose migrate service
	@if [ -f docker-compose.dev.yml ] && [ -d backend/migrations ]; then \
	  $(COMPOSE) -f docker-compose.dev.yml run --rm migrate up; \
	else \
	  echo "skip: migrations + compose migrate service land in a later step"; \
	fi

.PHONY: migrate-down
migrate-down: ## Roll back DB migrations (goose down)
	@if [ -f docker-compose.dev.yml ] && [ -d backend/migrations ]; then \
	  $(COMPOSE) -f docker-compose.dev.yml run --rm migrate down; \
	else \
	  echo "skip: migrations + compose migrate service land in a later step"; \
	fi

.PHONY: migrate-status
migrate-status: ## Show migration status (goose status)
	@if [ -f docker-compose.dev.yml ] && [ -d backend/migrations ]; then \
	  $(COMPOSE) -f docker-compose.dev.yml run --rm migrate status; \
	else \
	  echo "skip: migrations + compose migrate service land in a later step"; \
	fi

# ---------- secrets ----------

.PHONY: gen-kek
gen-kek: ## Print a random 64-hex-char (32 B) KEK for .env (D-020; host coreutils only — a dev-credential helper, not a toolchain)
	@head -c 32 /dev/urandom | od -An -v -tx1 | tr -d " \n"; echo

# ---------- dev stack ----------

.PHONY: dev
dev: ## Boot full local stack (postgres, backend, frontend, Keycloak, Mailpit)
	@if [ -f docker-compose.dev.yml ]; then \
	  $(COMPOSE) -f docker-compose.dev.yml up --build -d; \
	  $(COMPOSE) -f docker-compose.dev.yml ps; \
	else \
	  echo "skip: compose stack lands in step 0.4"; \
	fi

.PHONY: dev-ps
dev-ps: ## Show local stack status
	@if [ -f docker-compose.dev.yml ]; then \
	  $(COMPOSE) -f docker-compose.dev.yml ps; \
	else \
	  echo "skip: compose stack lands in step 0.4"; \
	fi

.PHONY: dev-logs
dev-logs: ## Tail local stack logs
	@if [ -f docker-compose.dev.yml ]; then \
	  $(COMPOSE) -f docker-compose.dev.yml logs -f --tail=100; \
	else \
	  echo "skip: compose stack lands in step 0.4"; \
	fi

.PHONY: dev-down
dev-down: ## Stop the local stack
	@if [ -f docker-compose.dev.yml ]; then \
	  $(COMPOSE) -f docker-compose.dev.yml down; \
	else \
	  echo "skip: compose stack lands in step 0.4"; \
	fi

# ---------- images ----------

.PHONY: images
images: ## Build production container images locally (no push)
	@if [ -f backend/Dockerfile ]; then \
	  docker build -t finance-tracker/backend:local backend/; \
	else \
	  echo "skip: backend Dockerfile lands in step 0.2"; \
	fi
	@if [ -f frontend/Dockerfile ]; then \
	  docker build -t finance-tracker/frontend:local frontend/; \
	else \
	  echo "skip: frontend Dockerfile lands in step 0.3"; \
	fi

.PHONY: images-push
images-push: ## Push images to $(REGISTRY) — MANUAL / OPERATOR-GATED (iteration 1 never pushes)
	@test -n "$(REGISTRY)" || { echo "refusing: REGISTRY unset (iteration 1 must not push images)"; exit 1; }
	@if [ -f backend/Dockerfile ] && [ -f frontend/Dockerfile ]; then \
	  docker buildx build --platform linux/amd64 -t $(REGISTRY)/finance-tracker-backend:local --push backend/; \
	  docker buildx build --platform linux/amd64 -t $(REGISTRY)/finance-tracker-frontend:local --push frontend/; \
	else \
	  echo "skip: needs both Dockerfiles (steps 0.2/0.3)"; \
	fi

# ---------- terraform (templates only — never applied) ----------
# `tf-validate` runs `init` per directory; the resulting .terraform.lock.hcl
# files are generated byproducts (exact provider versions are already pinned
# in the configs) and are removed afterwards so the working tree stays clean.

TF_DIRS := $(shell find infra -name '*.tf' -not -path '*/.terraform/*' 2>/dev/null | xargs -r -n1 dirname 2>/dev/null | sort -u)

.PHONY: tf-validate
tf-validate: ## Validate Terraform templates (init -backend=false, validate, fmt -check per module)
	@if [ -n "$(TF_DIRS)" ]; then \
	  for d in $(TF_DIRS); do \
	    echo "== $$d"; \
	    docker run --rm -v $(ROOT):/w -w /w/$$d $(TF_IMG) init -backend=false -input=false && \
	    docker run --rm -v $(ROOT):/w -w /w/$$d $(TF_IMG) validate && \
	    docker run --rm -v $(ROOT):/w -w /w/$$d $(TF_IMG) fmt -check && \
	    rm -f $$d/.terraform.lock.hcl; \
	  done; \
	else \
	  echo "skip: terraform modules land in step 1.1"; \
	fi

# ---------- e2e ----------

.PHONY: e2e
e2e: ## Run the scripted API-level E2E tour against the running local stack
	@if [ -f scripts/e2e.sh ]; then \
	  bash scripts/e2e.sh; \
	else \
	  echo "skip: E2E tour lands in step 4.6"; \
	fi

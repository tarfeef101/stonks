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
#   hashicorp/terraform    tf-validate (exact version verified + pinned at step 1.1)

SHELL := /bin/bash
.DEFAULT_GOAL := help

GO_IMG   ?= golang:1.27.3
NODE_IMG ?= node:26.13.0
TF_IMG   ?= hashicorp/terraform:latest # TODO(step 1.1): verify latest + pin exact version
COMPOSE  ?= docker compose
ROOT     := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))

# Named cache volumes (Docker-managed storage — not host paths)
GO_BUILD_VOL  := ftrk-go-build
GO_MOD_VOL    := ftrk-go-modcache
NPM_CACHE_VOL := ftrk-npm-cache

DOCKER_GO = docker run --rm -v $(ROOT)/backend:/src -w /src -v $(GO_BUILD_VOL):/root/.cache/go-build -v $(GO_MOD_VOL):/go/pkg/mod $(GO_IMG)
# testcontainers tests spawn ephemeral containers via the host daemon (standard CI pattern)
DOCKER_GO_TC = $(DOCKER_GO) -v /var/run/docker.sock:/var/run/docker.sock
DOCKER_NODE = docker run --rm -v $(ROOT)/frontend:/app -w /app -v $(NPM_CACHE_VOL):/root/.npm $(NODE_IMG)

.PHONY: help
help: ## List this Makefile's targets
	@grep -E '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

# ---------- build ----------

.PHONY: build
build: backend-build frontend-build ## Build everything (backend + frontend)

.PHONY: backend-build
backend-build: ## Build Go module + cmd/api and cmd/worker (golang:1.27.3)
	@test -f backend/go.mod || { echo "skip: backend lands in step 0.2"; exit 0; }
	$(DOCKER_GO) sh -c 'go build ./... && go build -o bin/api ./cmd/api && go build -o bin/worker ./cmd/worker'

.PHONY: frontend-build
frontend-build: ## Build frontend SPA bundle (node:26)
	@test -f frontend/package.json || { echo "skip: frontend lands in step 0.3"; exit 0; }
	$(DOCKER_NODE) sh -c 'npm ci && npm run build'

# ---------- test ----------

.PHONY: test
test: backend-test ## Run all tests

.PHONY: backend-test
backend-test: ## Backend unit + testcontainers integration tests (golang:1.27.3)
	@test -f backend/go.mod || { echo "skip: backend lands in step 0.2"; exit 0; }
	$(DOCKER_GO_TC) sh -c 'go test ./...'

# ---------- lint / fmt ----------

.PHONY: lint
lint: backend-lint frontend-lint ## Lint everything

.PHONY: backend-lint
backend-lint: ## go vet + gofmt check (golang:1.27.3)
	@test -f backend/go.mod || { echo "skip: backend lands in step 0.2"; exit 0; }
	$(DOCKER_GO) sh -c 'set -e; go vet ./...; bad=$$(gofmt -l .); [ -z "$$bad" ] || { echo "gofmt needed on:"; echo "$$bad"; exit 1; }'

.PHONY: frontend-lint
frontend-lint: ## svelte-check + eslint (node:26)
	@test -f frontend/package.json || { echo "skip: frontend lands in step 0.3"; exit 0; }
	$(DOCKER_NODE) sh -c 'npm ci && npm run check && npm run lint'

.PHONY: fmt
fmt: backend-fmt frontend-fmt ## Format everything

.PHONY: backend-fmt
backend-fmt: ## gofmt -w (golang:1.27.3)
	@test -f backend/go.mod || { echo "skip: backend lands in step 0.2"; exit 0; }
	$(DOCKER_GO) sh -c 'gofmt -w .'

.PHONY: frontend-fmt
frontend-fmt: ## prettier --write (node:26)
	@test -f frontend/package.json || { echo "skip: frontend lands in step 0.3"; exit 0; }
	$(DOCKER_NODE) sh -c 'npm ci && npm run format'

# ---------- migrations ----------

.PHONY: migrate
migrate: ## Apply DB migrations (goose up) via the compose migrate service
	@test -f docker-compose.dev.yml || { echo "skip: compose stack lands in step 0.4"; exit 0; }
	$(COMPOSE) -f docker-compose.dev.yml run --rm migrate up

.PHONY: migrate-down
migrate-down: ## Roll back DB migrations (goose down)
	@test -f docker-compose.dev.yml || { echo "skip: compose stack lands in step 0.4"; exit 0; }
	$(COMPOSE) -f docker-compose.dev.yml run --rm migrate down

# ---------- dev stack ----------

.PHONY: dev
dev: ## Boot full local stack (postgres, backend, frontend, Keycloak, Mailpit)
	@test -f docker-compose.dev.yml || { echo "skip: compose stack lands in step 0.4"; exit 0; }
	$(COMPOSE) -f docker-compose.dev.yml up --build -d
	@$(COMPOSE) -f docker-compose.dev.yml ps

.PHONY: dev-ps
dev-ps: ## Show local stack status
	@test -f docker-compose.dev.yml || { echo "skip: compose stack lands in step 0.4"; exit 0; }
	$(COMPOSE) -f docker-compose.dev.yml ps

.PHONY: dev-logs
dev-logs: ## Tail local stack logs
	@test -f docker-compose.dev.yml || { echo "skip: compose stack lands in step 0.4"; exit 0; }
	$(COMPOSE) -f docker-compose.dev.yml logs -f --tail=100

.PHONY: dev-down
dev-down: ## Stop the local stack
	@test -f docker-compose.dev.yml || { echo "skip: compose stack lands in step 0.4"; exit 0; }
	$(COMPOSE) -f docker-compose.dev.yml down

# ---------- images ----------

.PHONY: images
images: ## Build production container images locally (no push)
	@test -f backend/Dockerfile || { echo "skip: backend Dockerfile lands in step 0.2"; exit 0; }
	docker build -t finance-tracker/backend:local backend/
	@test -f frontend/Dockerfile || { echo "skip: frontend Dockerfile lands in step 0.3"; exit 0; }
	docker build -t finance-tracker/frontend:local frontend/

.PHONY: images-push
images-push: ## Push images to $(REGISTRY) — MANUAL / OPERATOR-GATED (iteration 1 never pushes)
	@test -n "$(REGISTRY)" || { echo "refusing: REGISTRY unset (iteration 1 must not push images)"; exit 1; }
	@test -f backend/Dockerfile && test -f frontend/Dockerfile || { echo "skip: needs both Dockerfiles (steps 0.2/0.3)"; exit 0; }
	docker buildx build --platform linux/amd64 -t $(REGISTRY)/finance-tracker-backend:local --push backend/
	docker buildx build --platform linux/amd64 -t $(REGISTRY)/finance-tracker-frontend:local --push frontend/

# ---------- terraform (templates only — never applied) ----------

TF_DIRS := $(shell find infra -name '*.tf' -not -path '*/.terraform/*' 2>/dev/null | xargs -r -n1 dirname 2>/dev/null | sort -u)

.PHONY: tf-validate
tf-validate: ## Validate Terraform templates (init -backend=false, validate, fmt -check per module)
	@test -n "$(TF_DIRS)" || { echo "skip: terraform modules land in step 1.1"; exit 0; }
	@for d in $(TF_DIRS); do \
		echo "== $$d"; \
		docker run --rm -v $(ROOT):/w -w /w/$$d $(TF_IMG) init -backend=false -input=false && \
		docker run --rm -v $(ROOT):/w -w /w/$$d $(TF_IMG) validate && \
		docker run --rm -v $(ROOT):/w -w /w/$$d $(TF_IMG) fmt -check; \
	done

# ---------- e2e ----------

.PHONY: e2e
e2e: ## Run the scripted API-level E2E tour against the running local stack
	@test -f scripts/e2e.sh || { echo "skip: E2E tour lands in step 4.6"; exit 0; }
	bash scripts/e2e.sh

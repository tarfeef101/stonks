# AGENTS.md

Agent instructions for this repository.

## Toolchains: containers only (hard rule)

**Nothing may be run or installed on the host system.** No host-side `go`, `node`, `npm`, `go install`, `npm i -g`, or any other package/toolchain installation — ever. All builds, tests, and toolchain invocations run inside Docker containers (the Docker daemon is available).

Pinned versions (do not deviate): Go `1.27.3` (see `backend/go.mod`, `backend/Dockerfile`), Node `26.13.0` (see `.tool-versions`, `frontend/Dockerfile`).

Standard container commands (run from the repo root):

```bash
# Go: vet + test
docker run --rm -v "$PWD/backend:/src" -v gomodcache:/go/pkg/mod -w /src \
  golang:1.27.3 sh -c 'go vet ./... && go test ./...'

# Go: build check (writes nothing to the host)
docker build -f backend/Dockerfile backend/

# Frontend: production build (the gate)
docker build -f frontend/Dockerfile frontend/

# Frontend: typecheck / scripts
docker run --rm -v "$PWD/frontend:/app" -v npmcache:/root/.npm -w /app \
  node:26.13.0 sh -c 'npm ci && npm run check'
```

Notes:
- The `gomodcache` / `npmcache` named volumes keep dependency caches in Docker storage (not the host) for iteration speed.
- Project-local gitignored artifacts under the repo (e.g. `frontend/build/`, `node_modules/`) are acceptable; system-level installs are not.
- Dev services (postgres, keycloak, mailpit, llama) come from `docker-compose.dev.yml`.
- **`mise` exists on the host** (`/usr/bin/mise`, shims at `~/.local/share/mise/shims`) and will silently put `go`/`node`/`java` etc. on PATH when a `.tool-versions` file is present. **Never** use it to install or invoke toolchains in this repo, and ignore any host `go`/`node` on PATH. `/home/tdedhar/go` (Go module cache) and `~/.cache/go-build` must not exist on the host — if they appear, they are agent pollution: delete them.

## Context

- This app stores sensitive personal financial data and runs for users at no profit. **Privacy and minimal risk exposure are first-priority concerns** — treat security review findings accordingly (no telemetry, no unnecessary data exposure, defense-in-depth on XSS/SRF surfaces).
- Crypto model: per-user AES-256-GCM DEKs wrapped by a global KEK (`users.wrapped_dek`); `*_encrypted` bytea columns hold `[nonce‖ciphertext‖tag]` packs (`backend/internal/crypto`).
- Dev IdP is local Keycloak (realm `finance`); Auth0 is the production IdP (`infra/auth0/`). Both are plain OIDC.
- Migrations are goose, in `backend/migrations/`. The app is not deployed to production yet — structural/migration changes are allowed when needed.

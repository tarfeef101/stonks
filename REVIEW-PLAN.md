# personal_finance_tracker — Critical Review: Execution Plan

Generated: 2026-11-15, from four subagent reviews (backend security, backend performance, frontend, architecture) + manual schema/env verification.
Repo state: dev only, test data, NOT deployed → structural/migration changes are free.
Prod target: k8s (dev is docker-compose + local Keycloak).

---

## 1. Decisions (locked by user)

| # | Decision |
|---|----------|
| D1 | **Token storage stays in localStorage** (no forced re-login on tab close). Compensate with **strict CSP + full security headers** as XSS defense-in-depth. |
| D2 | **FX**: convert displayed values to the user's `default_currency` (`users.default_currency` already exists, editable via `GET/PATCH /v1/settings`). Provider: **Frankfurter v2** (`api.frankfurter.dev`) — no API key, no quotas (abuse rate-limits only), 219 currencies, daily history back to 1948, reference/mid-market rates, local caching explicitly encouraged, free for commercial use. **No hardcoding of any currency anywhere.** Doc note to add later: filter by transaction/account currency. |
| D3 | **Budgets restructure**: `budgets` stays a plain object (name; `active` promoted to a **plaintext column**; period; schedule/notes remain encrypted); targets move to a child table `budget_targets(budget_id, category_id, amount, PK(budget_id, category_id))`. **Mortgages: no schema change** — fix the query path instead (see T-P1-2). |
| D4 | **TopMerchants (report): leave as-is** for now. Record the two follow-up options in `TECH-DECISIONS.md`: (a) narrow select to `description_encrypted` only + batched AEAD-cached decrypt; (b) plaintext derived merchant column for pure-SQL GROUP BY (privacy tradeoff: DB dump exposes merchant names). |
| D5 | **IdP**: Auth0 = production vendor (keep `infra/auth0/` + `docs/AUTH0-SETUP.md`); Keycloak = dev only. Rename env vars `AUTH0_*` → `OIDC_*` (verifier is issuer-agnostic; current names are misleading). |
| D6 | **Dropped as invalid for this project** (dev-environment concerns; prod is k8s): compose host-port binding, `/metrics` exposure, endpoint/application rate limiting (handled by k8s API gateway in prod). |

---

## 2. P0 — Security (keep from review)

### T-P0-1 · SSRF via `/v1/llm-credentials/test` (HIGHEST)
Any authenticated user can make the backend POST to an arbitrary endpoint; the response-body snippet in the error (`llm/llm.go:202`) makes it a read oracle against internal services/IMDS.
- `backend/internal/server/llmcredentials.go:265`, `backend/internal/llm/llm.go`.
- Fix: allowlist endpoint schemes (`https` for openai/custom; also allow `http` only for localhost/self-hosted if needed) and **reject private/link-local/loopback/metadata ranges** (resolve hostname, check all returned IPs) before connecting; wrap `resp.Body` in `http.MaxBytesReader` (a few MiB); return a **generic** error to the client (full error already logged server-side).

### T-P0-2 · KEK fail-open
`.env.example` ships a valid all-zeros 64-hex KEK (deploying it = known constant KEK that unwraps every user DEK); an empty KEK passes `config.Load` (fail-fast exists only in `cmd/api` + `cmd/worker`).
- Fix: `config.Load` rejects empty KEK and all-zeros KEK; `.env.example` ships an **empty** placeholder with a comment.

### T-P0-3 · Unbounded inputs (self-DoS)
- `csvparse/parse.go`: no row cap → a 10 MB CSV is fully parsed in memory on every analyze/preview. Add a maximum-rows limit (and an upload-size cap at the handler).
- `server/imports.go:338` `ReadFile` decrypts the whole statement into memory; `jsonparse.go:91`, `ofxparse.go:45` use `io.ReadAll` → stream-parse the decrypted stream where feasible, or enforce a hard size cap before parsing.

### T-P0-4 · JWT leeway
`authn/verifier.go:100` sets no `exp/nbf` leeway → clock skew causes spurious 401s (availability). Add 30–60 s.

### T-P0-5 · Frontend hardening (per D1: compensate for localStorage tokens)
- `frontend/Dockerfile`: nginx server block has **no security headers** → add strict **CSP**, `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY` (or `frame-ancestors 'none'`), `Referrer-Policy`. CSP must be tuned to allow SvelteKit + ECharts + self-hosted assets; test thoroughly.
- `frontend/Dockerfile:26`: production image runs as **root** → add non-root `USER` (nginx:alpine `nginx` user; watch listen port 80 vs 8080 + tmp path ownership).
- `routes/settings/+page.svelte:581`: LLM API key entered in a plain `<input>` → `type="password"` + `autocomplete="new-password"`.
- `lib/auth/client.ts:115-122`: `login()` stashes the full URL search string (account/category IDs, amounts, search terms) in localStorage as `return_to` → store **pathname only**.
- `lib/explore/state.ts:119`: `ftrk-explore-state` persists last financial filter state across sessions → move to `sessionStorage` or memory.
- `routes/dashboards/[id]/+page.svelte:642,1190`: user markdown goes `marked → DOMPurify` (default config) → tighten allowlist (tags + URI schemes; no `data:` images, no arbitrary links).
- `routes/auth/callback/+page.svelte:60`: raw OIDC exchange error text shown verbatim → generic failure message; log details client-side.
- (Already verified good: PKCE S256 + state/nonce, Bearer-header auth [no cookie CSRF surface], no tokens in URLs, all user content rendered via escaped text interpolation.)

---

## 3. P1 — Performance

### T-P1-1 · Crypto caching (multiplies all other gains)
- `server.go:423`: **every request** re-runs `EnsureUser` + `UnwrapDEK`; `EnsureUser` (`bootstrap.go:129`) mints and seals a throwaway fresh DEK each call. → Cache unwrapped DEK per user in memory (TTL); seal only at creation.
- `crypto.go:129`: rebuilds `cipher.NewGCM` on **every** `Seal`/`Open` → cache `cipher.AEAD` in a `sync.Map` keyed by DEK.

### T-P1-2 · Mortgage matching (no schema change, per D3)
- `server/mortgageMatching.go:200-234` `loadMatching`: uses generic full-row `transactions.List` (100/page, repo decrypts description/notes/raw per row) to obtain only `(date_posted, amount)` — **both already plaintext**. → Add narrow repo query `SELECT date_posted, amount FROM transactions WHERE account_id=$1 AND amount<0` (1 round trip, 0 decryption).
- `server/mortgageMatching.go:352-408` `syncAberrantNotifications`: runs on **every** `GET /v1/notifications` and re-runs the full-history match scan to purge stale notifications → move the self-heal to the **worker tick**; the endpoint just reads.

### T-P1-3 · Import pipeline
- `server/importrun.go:279`: per-row upsert in a sync HTTP loop (validate EXISTS + `INSERT…ON CONFLICT` + full-row Get/decrypt) ≈ **300k round trips + ~100k GCM seals per 100k-row import**. → Batched multi-row `INSERT…ON CONFLICT DO NOTHING … RETURNING` with validation folded into SQL (dedup unique constraint handles duplicates).
- `categorize/runner.go:215`: per-suggestion `ApplySuggestion` = 25+ round trips per 25-row batch → one multi-row `UPDATE` per batch.

### T-P1-4 · Offset matching
- `repo/offsets.go:72`: unbounded self-join (mirrored amounts, `a.id<b.id`) over the user's **whole history**, re-run after EVERY import (`importrun.go:326`); `LIMIT 1000` caps output only. → Constrain via the existing `(user_id,amount,date_posted)` index using the import's **date window**; run **asynchronously in the worker** (high-water mark on imported rows).

### T-P1-5 · Report collector
- `report/collector.go:143-157`: `listAll` pages the ENTIRE period (100/page) with full-row decryption just to feed `TopMerchants(rows,10)`. → **Per D4: leave as-is**, but record the follow-up options in `TECH-DECISIONS.md`.
- `collector.go:163-183` `budgetTargets`: decrypts ALL budget rows (all periods, inactive included) to take the first active row → after D3 restructure: `WHERE budget_id IN (SELECT id FROM budgets WHERE active=true AND period=$1)` and read targets directly (no blob decrypt).
- `collector.go`: `CategorySource.List` decrypts all category names → acceptable (small table); optionally cache.
- `schedule/dispatcher.go:166`: all (user, period) reports dispatch **strictly sequentially** → bounded-parallel worker pool.

### T-P1-6 · Query/index fixes
- `server/summary.go:415`: `trend=N` issues N sequential extra `Aggregate` round trips → one `GROUP BY period` query.
- `server/summary.go:295`: `budgets.List` decrypts every budget (inactive, all periods) → select active rows with minimal columns.
- `repo/transactions.go:1019` `ReviewPending`: unbounded (no LIMIT), full decrypt of the whole queue → add LIMIT + pagination + partial index `WHERE review_pending`.
- `repo/transactions.go:927/966` `Uncategorized`/`CountUncategorized`: `category_id IS NULL AND review_pending` has **no supporting index** → repeated rescans across ~4000 batches of a 100k-row job → partial index `(user_id) WHERE category_id IS NULL AND review_pending = false`.
- `server/dashboards.go:611` `SetOrder`: fully decrypts every widget name+content just to build an id-membership set → `SELECT id`.

### T-P1-7 · fisync
- `fisync/job.go:129`: hourly tick upserts (3–4 round trips each) **every row** of the adapter's full output → watermark on `fi_credentials.last_synced_at` + batched inserts.

### T-P1-8 · Pool tuning
- `cmd/api/main.go:95`, `cmd/worker/main.go:57`: `pgxpool.New` with no config (default MaxConns = max(4, NumCPU)) → explicit `pgxpool.Config` with tuned MaxConns.

---

## 4. FX feature (new, per D2)

Provider: **Frankfurter v2** — `api.frankfurter.dev`. Endpoints: `GET /v2/rates` (latest), `?date=YYYY-MM-DD`, `?from=&to=` (time series), `?base=`, `?quotes=`, `GET /v2/rate/{base}/{quote}`, `GET /v2/currencies`. CSV (`.csv` suffix) and NDJSON (Accept header) outputs. v1 is deprecated — use v2 only.

1. **Table**: `fx_rates(fx_date date, currency char(3), rate numeric(20,8), PK(fx_date, currency))` — store Frankfurter's daily blended feed (EUR base) **verbatim**. Convert via `amount × rate[to][d] / rate[from][d]` (base-agnostic; no hardcoded base currency).
2. **Worker jobs**:
   - Daily fetch of the latest day → upsert.
   - **On-demand backfill**: when a display conversion misses `(date, currency)`, fetch the missing range via `?from=&to=&quotes=<currencies>` and upsert. Bounded by actual transaction history; one request per gap.
3. **API**: `fx.Rate(date, from, to)` → table lookup with **nearest-earlier-date fallback**; missing pair (currency not in Frankfurter's 219) → return original currency with `converted: false`, **no error**.
4. **Display points** (convert to `users.default_currency`): reports, summary/trend, explore DTOs (add `amount_converted` + `converted_currency` fields alongside original `amount`/`currency`), cross-account balance/net worth, budget actuals vs targets.
5. **Optional**: at account creation, cross-check `^[A-Z]{3}$` currency against a cached `/v2/currencies` list to **warn** (not block) when the code is not convertible.
6. **Docs**: add a note (DESIGN.md / a docs/ page) that per-currency filtering by transaction/account currency is future work; users can change their default currency in Settings.
7. No hardcoded currency constants anywhere in code (replace any `'USD'` defaults found during implementation with config or the user's default).

---

## 5. P2 — Structure, redundancy, extensibility

1. **`budget_targets` restructure** (per D3): new migration; `budgets` loses `targets_encrypted`, gains `active boolean NOT NULL DEFAULT true`; child table `budget_targets`; update repo, server handlers (create/patch), report collector (T-P1-5), and frontend budget pages.
2. **Single source of presets**: delete DB-builtin `import_schemas` rows + the `EnsureBuiltins` path in `repo/schemas.go` (they shadow the 8 code builtins in `internal/presets` and are hand-excluded from `/v1/imports/presets`); keep `internal/presets` as the sole source.
3. **Identity fields decision**: `users.email`, `display_name`, `auth0_sub` are plaintext despite DESIGN.md's per-user-encryption goal, with no recorded decision → add a `TECH-DECISIONS.md` entry formally **exempting identity fields** (they are queryable by design: login lookup, email contact, audit trail).
4. **Collapse triple budgets validation**: period + targets validation runs 3× per request (server Create, server Patch, repo Create/Update) → one canonical validation in repo; keep a light shape check in server.
5. **Shared schedule/report types**: `schedule/adapters.go` field-for-field copies of repo types create a parallel data plane that will drift → define the contract in a shared types package.
6. **Env rename (per D5)**: `AUTH0_*` → `OIDC_*` across backend config, compose, Makefile, scripts, `.env.example`. Keep `infra/auth0/` + `docs/AUTH0-SETUP.md` as the prod path (update doc to note Keycloak is dev-only).
7. **Housekeeping**:
   - Makefile: `e2e` target references non-existent `scripts/e2e.sh` → implement or drop; remove stale always-passing "skip: … lands in step 0.x" guards.
   - `scripts/verify-authn.sh` + `scripts/verify-oidc.sh` duplicate ~70% of plumbing → extract a shared helper or merge into one script with subcommands.
   - `docker-compose.dev.yml`: worker + backend duplicate the full env block → `x-env` YAML anchor.
   - Delete stray empty `frontend/frontend/` directory.
   - `docs/ITERATION-1-REPORT.md` ends at step 9.2 with 33+ later rounds since → mark as point-in-time, point at the latest round.
   - `deploy/keycloak/finance-realm.json` committed with a test user + password (`test1234`) → strip users/credentials from the export (dev-init.sh can create them).
   - Dev default `FTRK_ACCESS_TOKEN_LIFESPAN=604800` → shorten to 3600.
   - `TECH-DECISIONS.md`: add TopMerchants follow-up note (D4) + identity-field exemption + FX rationale.
8. **frontend/build/**: verified NOT git-tracked (gitignored, on-disk only) → no action.

---

## 6. Frontend performance (P2)

1. **Unbounded lists**: `/v1/review`, `/v1/notifications`, `/v1/offsets/suggestions`, `/v1/offsets/groups` send no limit/offset — one bad import can pull thousands of rows into the DOM → add limit/offset params and paginate (pairs with T-P1-6).
2. **Widget grid**: `routes/dashboards/[id]/+page.svelte:255` — each widget's ExploreView independently walks up to 10×100 rows of `/v1/transactions`; an 8-tile grid fires up to 80 requests → dedupe identical filter queries client-side (or serve server-side aggregation later).
3. **ECharts**: `lib/explore/ExploreView.svelte:2` `import * as echarts` pulls the full ~1 MB build into the shared chunk → modular `echarts/core` imports (register only used charts/series/components) + lazy-load the component.
4. **svelte-query**: `@tanstack/svelte-query` is bundled and wraps the whole app, yet **no component calls `createQuery`** — dead weight, and accounts/categories/dashboards/saved-views are re-fetched on every route navigation → **adopt it** (cache shared lookups + fixes re-fetching) or drop the dependency.
5. **Polling**: `AppShell.svelte:60-78` 60 s unread-count poll continues while the tab is hidden → gate on `document.visibilityState`.

---

## 7. Subagent split & execution order

Gate per subagent — **container-only (user rule: nothing runs or installs on the host system; no host go/npm/toolchains)**:
- backend: `docker run --rm -v "$PWD/backend:/src" -v gomodcache:/go/pkg/mod -w /src golang:1.27.3 sh -c 'go vet ./... && go test ./...'` (from repo root; go.mod = go 1.27.3 = backend/Dockerfile build stage)
- frontend: `docker build -f frontend/Dockerfile frontend/` (production build gate; node:26.13.0 build stage); typecheck via `docker run --rm -v "$PWD/frontend:/app" -v npmcache:/root/.npm -w /app node:26.13.0 sh -c 'npm ci && npm run check'`
No production data exists; migrations may rewrite schema freely.

**Wave 1 — parallel** (independent files):
- **A** — T-P0-1…T-P0-4 (backend security).
- **B** — T-P0-5 (frontend hardening) + housekeeping item 7 realm-export strip + token-lifespan.
- **C** — T-P1-1 (crypto caching) + T-P1-3 (import pipeline).

**Wave 2 — parallel** (after C; both touch repo/transactions.go regions):
- **D** — T-P1-2, T-P1-4, T-P1-5 (dispatch part), T-P1-6, T-P1-7, T-P1-8.
- **E** — FX feature (section 4) + `TECH-DECISIONS.md` entries.

**Wave 3 — parallel**:
- **F** — P2 items 1–6 (budgets restructure, presets, validation collapse, shared types, env rename, docs).
- **G** — Frontend perf (section 6) + remaining housekeeping (Makefile, scripts, compose anchor, stray dir).

Suggested commit cadence: one commit per subagent task group (A…G), following the repo's existing "round" commit convention.

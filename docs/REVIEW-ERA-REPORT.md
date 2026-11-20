# Consolidated Review Report — Review Era + Savings Feature

> **Point in time.** Snapshot as of 2026-11-20 (HEAD `3606791`, round 55). The
> per-round record lives in `git log` (rounds 29–49 are bare `(round N)`
> subjects) and in the `docs/TECH-DECISIONS.md` change log — this report
> consolidates the review-era findings, their dispositions, the accepted
> risks, and the savings-feature spec deviations into one place. It is not
> kept up to date; later work records itself in `git log` + the decisions log
> (same convention as `docs/ITERATION-1-REPORT.md`).

## Scope

- **Review era:** rounds 1–49 (`c7b8d43` → `6656fc9`, 2026-11-09 → 2026-11-18) — the post-iteration-1 review/fix rounds after the iteration-1 stop gate (`fbd5119`).
- **Savings / registered plans:** rounds 50–55 (`9b1baed` → `3606791`, 2026-11-18 → 2026-11-20); decision **D-032** in `docs/TECH-DECISIONS.md`.
- Privacy posture held throughout: user-entered data only, no telemetry, no external data sources — the statutory plan figures are embedded in `backend/internal/plans`, verified on canada.ca as at 2026.

## Findings — dispositions (carried)

| # | Finding | Disposition |
|---|---|---|
| (f)-2 | FX conversion placement | **Closed by decision — handler-side.** The `internal/fx` package converts transaction LIST rows only; the savings room endpoint does no FX. `currency`/`amount_converted`/`converted_currency` are non-nullable strings ("" when unconverted); the appreciation detail page renders the `→ <converted>` suffix only when present. |
| (f)-3 | — | **N/A** — closed as not applicable; no action. |

## Accepted risks (carried)

**Risk B — the auth-callback catch path clears shared session state.** A
failed code exchange on `/auth/callback` calls `c.removeUser()`
(`frontend/src/lib/auth/client.ts:227–236`) to clear the stale persisted user
— the round-10 fix for the zombie "session expired" dialog. The accepted
consequence: that clear acts on the origin's shared localStorage, so a failing
exchange in one tab also drops the persisted session other tabs hold — there
is no per-tab isolation of auth state. Left as-is: the stale-user dialog loop
it prevents is strictly worse, and multi-tab silent-renew edges are low-value
in a single-user app.

**Unverified in a real browser (pre-deploy):**

- **CSP enforcement** — the headers are configured server-side, but enforcement was never exercised against a real browser; "no violation observed" is untested, not proven.
- **Silent renewal e2e** — the scripted E2E covers the full PKCE sign-in exchange; the silent-renew path has no scripted coverage (exercised only incidentally in a dev-server browser).

## Known inconsistencies (savings feature, accepted)

- **`SavingsSums` vs `EquitySeries` offset-row treatment (verified 2026-11-20).** `SavingsSums` (`backend/internal/repo/transactions.go:990`) sums ALL of the caller's posted rows per (account, year, external-origin) — no offset filter. `EquitySeries` (`backend/internal/repo/snapshots.go:316`) excludes offset-grouped transactions everywhere ("same rule as `TransactionStore.List`"). Product effect: a transfer between the user's own accounts into a plan account counts toward that account's room/contributions, but does not move the derived equity between snapshots. Accepted: the two endpoints answer different questions (money-in vs balance/value); no fix scheduled pre-deploy.
- **Beneficiary fields server-side (52b1):** accepted on ANY account type; the plan-type gating is frontend-only (a product rule in the settings forms). Accepted: the model stays permissive; the UI enforces the rule.
- **Snapshot note length limit is byte-based (52a2)** — a UTF-8 multibyte note hits the cap in fewer characters. Accepted (the limit applies to the stored form).
- **`date_executed` added to the immutable set (52b2b)** — spec deviation, accepted: the executed date is part of a realized item's identity.
- **Test-fake `Create` stores `ExternalOrigin` (52b2a)** — the repo mirror gained the field to match real behavior; test-only.
- **52b3 spec bug:** the inlined spec omitted the appreciation `account_id`-required guard; the child added it. The spec was wrong, not the child — carried as evidence for the process note below.

## Open items (low priority, pre-deploy)

- Stale pre-D-030 `AUTH0_*` names in `docs/DEVELOPMENT.md:148` and `docs/BLOCKERS.md:37–38` (verified 2026-11-20). The `AUTH0_*` mentions in `docs/AUTH0-SETUP.md` and `docs/TECH-DECISIONS.md` are INTENTIONAL (D-030: SSM parameter names keep the legacy spelling; the decisions log is historical).
- Local git-ignored `.env` on the host — expected (dev credentials; never committed).
- Host `~/.local/share/mise/` audit — **CLOSED 2026-11-20**: user confirmed the directory is legitimate mise-managed installs shared with other projects on the machine, not agent pollution. (The repo rule stands: no mise toolchains used IN this repo.)

## Resolved (this session)

- **Round-49 console warnings — RESOLVED.** User confirmation 2026-11-20 after a hard reload: the app's console is clean; the only residual noise is a browser extension (Bitwarden), not the app.

## Process notes (subagent dispatch series)

- **7 subagent tail crashes** across the era (6 pre-feature + round 54: "Compaction summary reached the output token limit", AFTER all file writes had completed). All 7 were recovered orchestrator-side: the child's work was reviewed, gated, and committed by the orchestrator. The crash-tolerance pattern (sequential children, ≤ ~1 new file + narrow edits, signatures pre-extracted and inlined, explicit read lists, ≤ 2 fix cycles, orchestrator-side gates + commits) held — no round was lost.
- **Round-54 child defects (all fixed orchestrator-side, pre-gate):** (1) a line-wrap dropped `filter`'s closing paren in the dashboards page's `planAccounts` derived — a syntax error; (2) the widget's `cfg` derived returned `parsed.account_id` while still `unknown` — TS narrowing does not survive across a boolean const; restructured around a local const; (3) the child changed the detail page's `chartOption` empty-series guard (`length === 0`) to `points === null` — broke both the empty state and the svelte-check template narrowing; house semantics restored.
- **Inlined specs are not gospel (×2):** round 54's spec stated `getEquity` returns a bare array (it returns `{ points }`) — the child read the real API file and implemented correctly, and improved on the spec (beneficiary-aware table keys; a 3-column room table). See the 52b3 entry above for the other case. Standing rule: children read real files over inlined signatures.
- **Smaller process notes:** 53b — the child hit the 2-cycle fix cap on an invented `base` import; orchestrator fixed. 53c — the child self-found and fixed its own adjErrors bug. 53d — a self-contradictory delete-label paragraph in the dispatch prompt cost a 3-line follow-up + a forced one-line `auth/callback` cast extension. 53f, 55 — clean rounds.
- **Orchestrator shell bug:** an assigned docker-command string (`D='docker run …'` then `$D …`) does not re-expand `$PWD` and keeps literal quotes → 0-byte outputs that read as "prettier crashes"; a full gate re-run with inline commands was required. Standing rule: docker commands are always written inline.
- **Pre-existing prettier/tailwind-plugin class-order drift** on several house svelte files — repo-wide, OUT of scope; the real frontend gate is `svelte-check` (`npm run check`). New files from this era: apply prettier, restore the house-ordered `dark:`-variant class attributes, and verify the residual drift is EXACTLY those restorations (round-54 widget: exactly 4 lines, verified).

## Gates (final state, 2026-11-20)

- Backend: `go vet ./... && go test ./...` green (golang:1.27.3 container) — rounds 50–52.
- Frontend: `svelte-check` 0 errors / 0 warnings; `en.json` prettier-clean (node:26.13.0 container) — rounds 53a–54.
- All gates run in the pinned Docker containers (the repo's Docker-only rule); nothing was installed on the host.

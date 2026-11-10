# Dashboards rework (round 6) — plan

**User intent (verbatim, 2026-11-09):** "dashboards should be a place to browse dashboards, not
little 'cards'. dashboards should be a place where one can add tables, graphs, etc. each with
their own saved queries, resize them, move them around, add custom text items in markdown, etc.
you could consider a single dashboard to be comprised of cards, if cards could be made that
flexible. but the dashboards tab should allow one to create+manage many dashboards, not just
one."

## Goal

Replace the ONE implicit dashboard (the grid of `dashboard_cards`) with a browsable
**multi-dashboard** area:

- `/dashboards` → list of dashboards: create, rename, delete, open (each shows its widget count).
- `/dashboards/[id]` → one dashboard: a grid of **widgets** the user can add, rename,
  resize (column span), reorder (move left/right), and delete.

### Widget kinds (v1)

| kind       | renders                                                                 | `content` stores                                          |
| ---------- | ----------------------------------------------------------------------- | --------------------------------------------------------- |
| `explore`  | the existing `ExploreView` (table + line/bar/pie = "tables, graphs, …") | the canonical `explore_state` query string ("" = default all-time view) — the widget's **saved query** |
| `markdown` | rendered markdown (user's custom text)                                  | the markdown source                                       |

"Tables and graphs, each with their own saved query" = `explore` widgets: every widget carries
its own `explore_state`, so each has its own filters/grouping/chart. No new rendering is needed —
`ExploreView.svelte` is already the shared renderer.

### Layout: resize + move (no drag-and-drop library)

- Grid: `grid grid-cols-1 gap-4 lg:grid-cols-12`; each widget takes `lg:col-span-N`, `span ∈ 1..12`
  (DB column, default 6). Mobile (`grid-cols-1`) renders every widget full-width.
- Tailwind JIT: dynamic `lg:col-span-N` strings are only generated if the literal appears in
  source — the widget component keeps a `SPAN_CLASSES` const array holding all 12 literals and
  indexes into it (Tailwind scans file text, so all classes are emitted).
- **Resize** UI: a per-widget size select with presets — `3` = ¼, `4` = ⅓, `6` = ½ (default),
  `8` = ⅔, `12` = full.
- **Move** UI: move-left / move-right buttons that swap the widget with its neighbour in
  `sort_order` (same swap+PUT-order pattern the current cards page already uses). DOM order in
  the 12-col grid IS the display order, so swapping covers free repositioning for v1.
  (Drag-and-drop deliberately out of scope: new dependency, a11y, and testing cost; the lean
  house pattern is button-driven reorder, like the existing cards.)

### Data migration (existing cards preserved)

`dashboard_cards` rows are **not** lost: a one-time **app-level backfill** at server startup
(after goose) moves every card into a per-user dashboard named **Default** as `explore` widgets
(`content` = the card's `explore_state`, `span` = 4 — the old grid was 3-up), in sort order,
then clears and `DROP TABLE dashboard_cards`. The backfill is a no-op when the table is already
empty or absent (idempotent; logs a line when it moves data). 0013's SQL creates the new tables
but does NOT drop the old one (the backfill owns the drop, because DEK encryption is app-layer —
SQL can't produce the ciphertext).

Consequence: `0008`'s down-migration becomes unreachable on post-backfill DBs (the table is
already gone at runtime). The house one-step-down test is updated to match (it only tests
one-step-down from the tip; 0013's down drops the new tables).

## Data model — migration `0013_dashboards.sql`

House conventions (0008 header): `user_id` on both tables (stores scope SQL on it), DEK-encrypted
packs for user-authored fields, no timestamps on the wire.

```sql
CREATE TABLE dashboards (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name_encrypted  bytea       NOT NULL,          -- house: user-authored names are DEK-encrypted
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX dashboards_user_idx ON dashboards (user_id);

CREATE TABLE dashboard_widgets (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id            uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    dashboard_id       uuid        NOT NULL REFERENCES dashboards (id) ON DELETE CASCADE,
    kind               text        NOT NULL CHECK (kind IN ('explore', 'markdown')),
    name_encrypted     bytea       NOT NULL,
    content_encrypted  bytea       NOT NULL,       -- explore: explore_state string; markdown: source
    span               integer     NOT NULL CHECK (span >= 1 AND span <= 12) DEFAULT 6,
    sort_order         integer     NOT NULL,       -- new rows take MAX(sort_order)+1 over the dashboard
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX dashboard_widgets_dashboard_idx ON dashboard_widgets (user_id, dashboard_id, sort_order);
```

No unique constraint on names (house: duplicate labels allowed, like budgets). Widget deletion is
a plain row delete; dashboard deletion cascades (FK). `kind` is CHECK-enum'd in SQL.

## API (replaces `/v1/dashboard-cards`)

Wire convention (3.1): success bodies RAW (no envelope), NO timestamps; errors are the
`{error: {code, message}}` envelope; 400 messages are `"; "`-joined problem lists; 404s never
disclose existence. Auth via the existing bearer path; every store call scoped to the caller.

| # | Method + path                                | Body                                          | Success                       | Errors |
|---|----------------------------------------------|-----------------------------------------------|-------------------------------|--------|
| 1 | `GET /v1/dashboards`                         | —                                             | `[{id, name, widget_count}]`, created_at asc (id asc tiebreak) | — |
| 2 | `POST /v1/dashboards`                        | `{name}`                                      | 201 `{id, name, widget_count: 0}` | 400 bad name |
| 3 | `PATCH /v1/dashboards/{id}`                  | `{name?}` (pointer; absent = untouched)       | 200 `{id, name, widget_count}` | 404; 400 bad name / empty patch |
| 4 | `DELETE /v1/dashboards/{id}`                 | —                                             | 204                           | 404 |
| 5 | `GET /v1/dashboards/{id}/widgets`            | —                                             | `[{id, name, kind, content, span, sort_order}]`, sort_order asc | 404 |
| 6 | `POST /v1/dashboards/{id}/widgets`           | `{name, kind, content, span?}`                | 201 created row               | 404; 400 (see rules) |
| 7 | `PATCH /v1/dashboards/{id}/widgets/{wid}`    | `{name?, kind?, content?, span?}` (pointers)  | 200 updated row               | 404; 400 (see rules) |
| 8 | `DELETE /v1/dashboards/{id}/widgets/{wid}`   | —                                             | 204                           | 404 |
| 9 | `PUT /v1/dashboards/{id}/widgets/order`      | **bare JSON array** `["uuid", ...]` of every widget in the dashboard, new order | 204 | 404; 400 not a complete 1:1 permutation |

Validation rules (mirrors `dashboard_cards.go` + `envelope.go` helpers):

- `name`: shared `checkName` (non-empty, ≤ 255 runes).
- `kind`: must be `explore` or `markdown` else 400.
- `content` caps, by the kind in effect: `explore` ≤ **1024** runes (server does NOT re-validate
  canonical form — same stance as the old cards endpoint); `markdown` ≤ **16 384** runes.
  On a PATCH that sets `kind` but not `content`, the EXISTING content is re-validated against
  the new kind's cap (fetch-then-check).
- `span`: integer 1..12 else 400; absent on POST = 6.
- Order endpoint: body must be a bare array containing each widget of that dashboard exactly
  once; `ids[i]` → `sort_order = i+1`, atomic (same pattern as `SetOrder` for cards).
- Widget endpoints verify the widget belongs to the named dashboard (else 404, not 400).
- Sentinel errors (`ErrDashboardNotFound`, `ErrDashboardWidgetNotFound`) → 404 with fixed
  messages; everything else → 500 with details only in the log (house `cardError` pattern).

## Backend file map (phase 1)

- `backend/migrations/0013_dashboards.sql` — up: create both tables (no data migration in SQL);
  down: drop both (widgets first).
- `backend/internal/repo/dashboards.go` — replaces `dashboard_cards.go`:
  `DashboardStore` (List with widget_count, Create, Update, Delete) +
  `DashboardWidgetStore` (List, Create, Update, Delete, SetOrder), same pool/DEK/dek style as the
  cards repo. `ErrDashboardNotFound` / `ErrDashboardWidgetNotFound` sentinels.
  Plus `Backfill(ctx, pool, kek)` (or a `migrate`-package hook — subagent picks the cleanest
  existing hook point after goose runs): for each user with cards, create the `Default`
  dashboard + copy cards as `explore` widgets (span 4, sort preserved), delete the cards, and
  finally `DROP TABLE dashboard_cards` (all inside a transaction; no-op when empty/absent).
- `backend/internal/server/dashboards.go` — replaces `dashboard_cards.go`: handlers + DTOs
  (`dashboardOut`, `widgetOut`, `dashboardIn`, `dashboardPatch`, `widgetIn`, `widgetPatch`) +
  validation, registered in `server.go` (the six `dashboard-cards` routes at :264-269 are
  replaced by the nine above; `New()`'s `cards CardStore` param is replaced by the new store(s)).
- Delete: `backend/internal/repo/dashboard_cards.go`,
  `backend/internal/server/dashboard_cards.go`, `dashboard_cards_test.go` (replaced by
  `dashboards_test.go` with the in-memory store impl, covering all 9 routes: 400s, 404s,
  ordering, kind/content caps, order-endpoint permutation check, cascade delete).
- `backend/internal/migrate/migrate_test.go` — table-list + one-step-down assertions updated
  (drop `dashboard_cards` references; assert the two new tables remain after one-step-down from
  the tip; 0013's down drops them).
- Update every `New(...)`/bootstrap call site that passes the cards store (grep `CardStore`).

**Verify (phase 1):** `go build ./... && go vet ./... && go test ./...` (house pattern — tests
run against the running dev compose Postgres; restart the backend container first so goose
applies 0013 + the backfill runs; expect the existing `Test Card` to land in a `Default`
dashboard). Live probe: the 9 routes via curl with a Keycloak token
(`POST /realms/finance/protocol/openid-connect/token`, grant_type=password, test/test1234,
client_id finance-spa; base `http://localhost:18080/api/v1`): create dashboard, create explore
+ markdown widgets, PATCH span/kind/content, PUT order, DELETEs, 404/400 paths.

## Frontend file map (phase 2)

- **Deps:** add `marked` + `dompurify` to `frontend/package.json` (markdown rendering +
  sanitization; both ship their own types).
- `frontend/src/lib/api/dashboards.ts` — NEW, replaces `dashboardCards.ts` (deleted):
  `Dashboard {id, name, widget_count}`, `WidgetKind = 'explore' | 'markdown'`,
  `DashboardWidget {id, name, kind, content, span, sort_order}`, and the 9 client functions.
- `frontend/src/routes/dashboards/+page.svelte` — REWRITTEN as the **list** page (keeps the
  sign-in gate, loading/error/empty patterns, inline rename pattern, confirm-on-delete,
  `resolve()` links): one row per dashboard (name, widget count, Open link, rename, delete) +
  a new-dashboard add row.
- `frontend/src/routes/dashboards/[id]/+page.svelte` — NEW dashboard editor: loads dashboard +
  widgets + `loadCatalogs()` name maps; the 12-col grid; widget card = header (name + controls:
  size select, move left/right, edit, open-in-Explore link for `explore` widgets via
  `serializeStateForLink`, delete w/ confirm) + body (`<ExploreView state={parseState(w.content)}
  names={names}/>` or sanitized markdown via `marked.parse` → `DOMPurify.sanitize` → `@html`,
  `$derived`); add-widget row (name + kind buttons: "Explore view" seeds `content ''`,
  "Text (markdown)" opens a textarea); edit = rename for explore widgets, rename+textarea for
  markdown widgets (Save/Cancel, Enter/Escape like the settings cards). Dynamic span classes via
  the `SPAN_CLASSES` literal array.
- `frontend/src/routes/explore/+page.svelte` — `saveAsCard` → **save-to-dashboard**: replace
  `window.prompt` with a small inline panel (dashboard `<select>` from `listDashboards`, name
  input, Save/Cancel; when there are zero dashboards the panel offers a create-name field and
  creates first, then saves). Calls `createWidget(dashboardId, {name, kind: 'explore', content:
  serializeState(explore), span: 4})`.
- `frontend/src/lib/i18n/locales/en.json` — rework the `dashboards` group for list+editor+widgets
  (new keys for dashboards CRUD, widgets CRUD, sizes, move, markdown, save-to-dashboard panel;
  drop the card-only keys; keep `explore.*` save keys renamed to the dashboard wording); update
  any comments quoting removed strings.

**Verify (phase 2):** docker node:26 one-shot `npx svelte-kit sync && npx svelte-check --output
human && npx vite build` (0/0); dev server on :18082 serving the routes (SPA fallback handles
`/dashboards/<uuid>` deep links); manual page walk.

## Phasing & execution

- **P1 — backend subagent** (own context): everything in the backend file map + its verify
  section. Must NOT touch `frontend/` or `docs/TECH-DECISIONS.md`. Reports the exact contract
  deltas if any (there should be none — this doc is the contract).
- **P2 — frontend subagent** (own context, after P1 is verified): everything in the frontend
  file map + its verify section. Must NOT touch `backend/` or `docs/TECH-DECISIONS.md`.
- **P3 — parent:** re-verify subagent claims on disk + live (the house pattern), one cumulative
  commit per phase (one-line subjects), `docs/TECH-DECISIONS.md` round-6 bullet, report.

## Explicit non-goals (v1)

- Drag-and-drop repositioning (button reorder + span presets cover resize/move).
- Editing an `explore` widget's saved query in place — the widget's "Open in Explore" link loads
  its state; a changed view is saved as a NEW widget via the Explore save panel (delete the old
  one after). (In-place query editing = rebuilding the explore filter bar inside the widget
  editor; deferred.)
- New widget kinds beyond `explore` + `markdown` (the kind enum + content column make adding one
  a follow-up, not a rework).
- Per-widget data caching/sharing across widgets (each `explore` widget runs its own paged fetch,
  same as today's cards).

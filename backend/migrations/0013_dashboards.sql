-- 0013_dashboards.sql — dashboards rework (round 6, phase 1).
--
-- Replaces the single implicit dashboard (0008's dashboard_cards grid) with a
-- browsable multi-dashboard area (DASHBOARDS-REWORK.md):
--   dashboards         named, user-owned containers of widgets. One
--                      DEK-encrypted pack for the user-authored name, same
--                      convention as accounts/categories/budgets.
--   dashboard_widgets  the grid items: kind 'explore' (content = the
--                      canonical explore_state query string, '' = default
--                      all-time view) or 'markdown' (content = the markdown
--                      source). Both name and content are ONE
--                      DEK-encrypted pack each (content can carry the
--                      free-text q search term, like explore_state did).
--                      span is the 1..12 grid column span (default 6);
--                      sort_order is the display order (the PUT /order
--                      endpoint rewrites it; new rows take
--                      MAX(sort_order)+1 over the dashboard). Like the
--                      old cards, names have NO unique constraint
--                      (duplicate labels allowed).
--
-- No data migration in SQL: the old dashboard_cards rows are moved by the
-- app-level Backfill at startup (DEK encryption is app-layer — SQL can't
-- produce the ciphertext). That backfill owns the `DROP TABLE
-- dashboard_cards`; this file does NOT drop the old table.
--
-- *_encrypted bytea columns hold [nonce ‖ ciphertext ‖ tag] packs produced
-- by internal/crypto (AES-256-GCM, step 2.4); see the 0001 header.

-- +goose Up

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

-- +goose Down

-- Widgets reference dashboards (FK), so they must go first.
DROP TABLE dashboard_widgets;
DROP TABLE dashboards;

-- 0015_saved_views.sql — saved Explore views (round 16).
--
-- saved_views are per-user named snapshots of the Explore page's URL
-- state: the canonical explore_state query string (the frontend codec's
-- serializeState() output; '' = the default view). The Explore view bar
-- saves, opens, updates, renames and deletes them; the dashboard
-- add-widget flow picks one (or "none") and the widget stores its OWN
-- copy of the state — a copy, not a reference — so deleting a saved
-- view never breaks a widget.
--
-- Both name and state are ONE DEK-encrypted pack each (the state can
-- carry the free-text q search term, like explore_state did in the old
-- dashboard_cards). Like the dashboards, names have NO unique
-- constraint (duplicate labels allowed).

-- +goose Up

CREATE TABLE saved_views (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name_encrypted  bytea       NOT NULL,          -- house: user-authored names are DEK-encrypted
    state_encrypted bytea       NOT NULL,          -- canonical explore_state string; '' = the default view
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX saved_views_user_idx ON saved_views (user_id);

-- +goose Down

DROP TABLE saved_views;

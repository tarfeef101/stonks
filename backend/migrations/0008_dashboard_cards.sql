-- 0008_dashboard_cards.sql — dashboard cards (plan step 6.5).
--
-- Tables per plan §5 (encryption field inventory) + DESIGN.md "Database":
--   dashboard_cards  saved Explore views: a user-authored name plus the
--                    codec's canonical explore_state query string, rendered
--                    as a read-only card on the Dashboards page and
--                    click-through to /explore with that state. name AND
--                    explore_state are each ONE DEK-encrypted pack: the
--                    table is not enumerated in the §5 inventory, but the
--                    house convention (user-authored names are DEK-
--                    encrypted: accounts, categories, budgets) applies to
--                    name, and explore_state can carry the free-text q
--                    search term, so both are encrypted. sort_order is the
--                    per-user display order (the PUT /order endpoint rewrites
--                    it); like budgets there is NO unique constraint on the
--                    name (duplicate labels allowed).
--
-- *_encrypted bytea columns hold [nonce ‖ ciphertext ‖ tag] packs produced
-- by internal/crypto (AES-256-GCM, step 2.4); see the 0001 header.

-- +goose Up

CREATE TABLE dashboard_cards (
    id                    uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id               uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name_encrypted        bytea       NOT NULL,
    -- The codec's canonical explore_state query string ('' = the all-time
    -- default view), one pack (see the header).
    explore_state_encrypted bytea     NOT NULL,
    sort_order            integer     NOT NULL,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX dashboard_cards_user_idx ON dashboard_cards (user_id, sort_order);

-- +goose Down

DROP TABLE dashboard_cards;

-- 0001_core.sql — core schema (step 2.1).
--
-- Tables per plan §5 (encryption field inventory):
--   users           identity + settings + wrapped per-user DEK
--   user_categories per-user categories (name DEK-encrypted; name_hash for
--                   uniqueness/lookup — §5 keeps it plaintext)
--   import_schemas  builtin (global, public) + per-user custom presets;
--                   custom names DEK-encrypted (§5: "builtins are public
--                   knowledge")
--   notifications   app-generated text — all plaintext per §5
--
-- *_encrypted bytea columns hold [nonce ‖ ciphertext ‖ tag] packs produced
-- by internal/crypto (AES-256-GCM, step 2.4); the column stores nothing
-- else. users.wrapped_dek holds the KEK-wrapped per-user DEK (step 2.4).
--
-- PG18: gen_random_uuid() is a core builtin (no extension required).
--
-- Period vocabulary ('month' | 'quarter' | 'year') is pinned here once so
-- users.default_period (2.1), budgets.period (2.3) and report periods
-- (8.x) share one CHECK-defined set.

-- +goose Up

CREATE TABLE users (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    auth0_sub        text        NOT NULL,
    email            text        NOT NULL,
    display_name     text        NOT NULL,
    language         text        NOT NULL DEFAULT 'en',
    default_currency text        NOT NULL DEFAULT 'USD',
    default_period   text        NOT NULL DEFAULT 'month',
    notify_monthly   boolean     NOT NULL DEFAULT false,
    notify_quarterly boolean     NOT NULL DEFAULT false,
    notify_annual    boolean     NOT NULL DEFAULT false,
    wrapped_dek      bytea       NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT users_auth0_sub_key UNIQUE (auth0_sub),
    CONSTRAINT users_default_period_check
        CHECK (default_period IN ('month', 'quarter', 'year'))
);

CREATE TABLE user_categories (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name_encrypted bytea      NOT NULL,
    name_hash     text        NOT NULL,
    parent_id     uuid        REFERENCES user_categories (id) ON DELETE CASCADE,
    active        boolean     NOT NULL DEFAULT true,
    sort_order    integer     NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT user_categories_name_key UNIQUE (user_id, name_hash),
    CONSTRAINT user_categories_parent_check
        CHECK (parent_id IS NULL OR parent_id <> id)
);
CREATE INDEX user_categories_user_idx ON user_categories (user_id, sort_order);

CREATE TABLE import_schemas (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id        uuid        REFERENCES users (id) ON DELETE CASCADE,
    -- Builtins are global and public: user_id IS NULL, builtin = true,
    -- name plaintext. Customs are per-user: user_id NOT NULL, builtin =
    -- false, name_encrypted = DEK pack. The CHECK below makes that
    -- invariant explicit (no mixed-format single column).
    name           text,
    name_encrypted bytea,
    kind           text        NOT NULL,
    builtin        boolean     NOT NULL DEFAULT false,
    spec           jsonb       NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT import_schemas_kind_check CHECK (kind IN ('csv', 'ofx', 'json')),
    CONSTRAINT import_schemas_shape_check
        CHECK ((builtin AND user_id IS NULL AND name IS NOT NULL
                AND name_encrypted IS NULL)
             OR (NOT builtin AND user_id IS NOT NULL AND name IS NULL
                AND name_encrypted IS NOT NULL))
);
CREATE INDEX import_schemas_user_idx ON import_schemas (user_id) WHERE user_id IS NOT NULL;

CREATE TABLE notifications (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind       text        NOT NULL,
    title      text        NOT NULL,
    body       text        NOT NULL,
    ref        jsonb,
    read_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT notifications_read_at_check CHECK (read_at IS NULL OR read_at >= created_at)
);
CREATE INDEX notifications_unread_idx
    ON notifications (user_id, created_at DESC)
    WHERE read_at IS NULL;

-- +goose Down

DROP TABLE notifications;
DROP TABLE import_schemas;
DROP TABLE user_categories;
DROP TABLE users;

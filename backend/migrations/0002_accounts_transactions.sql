-- 0002_accounts_transactions.sql — accounts + transactions (step 2.2).
--
-- Tables per plan §5 (encryption field inventory) + DESIGN.md "Database":
--   accounts      per-user accounts (name DEK-encrypted; name_hash for
--                 uniqueness/lookup — §5 keeps it plaintext); type
--                 vocabulary pinned below; default_schema_id -> the
--                 import_schemas preset applied to that account's imports
--   transactions  one row per financial event; amount is PLAINTEXT by
--                 decision (SQL must sum/range/sort it at ~100K rows);
--                 description/notes/raw are DEK-encrypted so merchant
--                 identity stays private in a dump; dedup_hash one-way
--
-- dedup_hash = sha256(account_id ‖ date_posted ‖ amount ‖ normalized
-- description), computed in-app pre-encryption (step 2.6 upserts on it);
-- UNIQUE (account_id, date_posted, dedup_hash) makes re-importing
-- overlapping statement data a no-op (DESIGN.md "gracefully de-duplicate").
--
-- date_posted is the dedup anchor: NOT NULL. date_executed is nullable
-- (some accounts only carry one date) and may not post before it executed.
--
-- *_encrypted bytea columns hold [nonce ‖ ciphertext ‖ tag] packs produced
-- by internal/crypto (AES-256-GCM, step 2.4); see the 0001 header.

-- +goose Up

CREATE TABLE accounts (
    id                uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id           uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name_encrypted    bytea       NOT NULL,
    name_hash         text        NOT NULL,
    -- DESIGN.md "Database": type (bank/cash vs credit card vs mortgage,
    -- "etc" — 'other' is the escape hatch), subtype (chequing/savings;
    -- variable/fixed mortgage) free text.
    type              text        NOT NULL,
    subtype           text,
    currency          text        NOT NULL,
    -- Preset applied to this account's imports (0001). SET NULL: a
    -- deleted schema orphans the setting, not the account.
    default_schema_id uuid        REFERENCES import_schemas (id) ON DELETE SET NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT accounts_name_key UNIQUE (user_id, name_hash),
    CONSTRAINT accounts_type_check
        CHECK (type IN ('bank', 'cash', 'credit_card', 'mortgage', 'investment', 'other'))
);
CREATE INDEX accounts_user_idx ON accounts (user_id);

CREATE TABLE transactions (
    id                    uuid         PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id               uuid         NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    account_id            uuid         NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    date_executed         date,
    date_posted           date         NOT NULL,
    -- Plaintext by decision (plan §5): SQL sums/ranges/sorts this column.
    amount                numeric(14,2) NOT NULL,
    -- SET NULL: deleting a category uncategorises the event; it never
    -- deletes it.
    category_id           uuid         REFERENCES user_categories (id) ON DELETE SET NULL,
    description_encrypted bytea        NOT NULL,
    notes_encrypted       bytea,
    raw_encrypted         bytea,
    dedup_hash            text         NOT NULL,
    review_pending        boolean      NOT NULL DEFAULT false,
    created_at            timestamptz  NOT NULL DEFAULT now(),
    updated_at            timestamptz  NOT NULL DEFAULT now(),

    CONSTRAINT transactions_dedup_key
        UNIQUE (account_id, date_posted, dedup_hash),
    CONSTRAINT transactions_dates_check
        CHECK (date_executed IS NULL OR date_executed <= date_posted)
);
-- Explore-page default view: this user's events over a date range. The
-- dedup unique index also serves (account_id, date_posted) prefix queries.
CREATE INDEX transactions_user_posted_idx ON transactions (user_id, date_posted);

-- +goose Down

DROP TABLE transactions;
DROP TABLE accounts;

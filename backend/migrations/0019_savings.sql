-- 0019_savings.sql — savings / registered-plan area (round 50).
--
-- Schema support for the Canadian registered-plan room engine
-- (backend/internal/plans): new account types (rrsp/tfsa/fhsa/rrif/resp/
-- rdsp), per-plan beneficiary + opening metadata, transfer grouping, and
-- external origin for contribution rows (grants/inherited never count
-- toward room; employer matches do).
--
-- Constraint names VERIFIED in the dev DB before writing:
--   accounts.type            -> accounts_type_check         (named in 0002)
--   dashboard_widgets.kind   -> dashboard_widgets_kind_check (inline in 0013,
--                               auto-named per the <table>_<column>_check rule)
--
-- *_encrypted bytea columns hold [nonce ‖ ciphertext ‖ tag] packs produced
-- by internal/crypto (AES-256-GCM, step 2.4); see the 0001 header. No
-- plaintext years are stored anywhere: birth years travel as DEK-encrypted
-- decimal strings.
--
-- The new account_snapshots table lets the UI pin a known balance per
-- account/date (e.g. the RRIF fair market value as of Jan 1) without
-- trusting only the latest imported statement.

-- +goose Up

-- 1) users: birth year as a DEK-encrypted decimal string (no plaintext year
--    anywhere). NULL = unknown.
ALTER TABLE users ADD COLUMN birth_year_encrypted bytea NULL;

-- 2) accounts: plan vocabulary + plan metadata.
ALTER TABLE accounts DROP CONSTRAINT accounts_type_check;
ALTER TABLE accounts ADD CONSTRAINT accounts_type_check
    CHECK (type IN ('bank', 'cash', 'credit_card', 'mortgage', 'investment',
                    'other', 'rrsp', 'tfsa', 'fhsa', 'rrif', 'resp', 'rdsp'));
ALTER TABLE accounts ADD COLUMN beneficiary_name_encrypted bytea NULL;
ALTER TABLE accounts ADD COLUMN beneficiary_birth_year_encrypted bytea NULL;
ALTER TABLE accounts ADD COLUMN opening_balance numeric(14,2) NOT NULL DEFAULT 0;
ALTER TABLE accounts ADD COLUMN opened_at date NULL;

-- 3) transactions: transfer grouping (two legs share a group id so the UI
--    can render them as one transfer) + external origin for contribution
--    rows. NULL external_origin = a normal user contribution.
ALTER TABLE transactions ADD COLUMN transfer_group_id uuid NULL;
CREATE INDEX idx_transactions_transfer_group
    ON transactions (transfer_group_id) WHERE transfer_group_id IS NOT NULL;
ALTER TABLE transactions ADD COLUMN external_origin text NULL;
ALTER TABLE transactions ADD CONSTRAINT transactions_external_origin_check
    CHECK (external_origin IN ('employer', 'government_grant', 'inherited', 'other'));

-- 4) Plan room tables.
CREATE TABLE plan_limit_overrides (
    id           uuid         PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid         NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    plan_type    text         NOT NULL,
    year         integer      NOT NULL,
    annual_limit numeric(14,2) NOT NULL,
    created_at   timestamptz  NOT NULL DEFAULT now(),
    updated_at   timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT plan_limit_overrides_user_plan_year_uq
        UNIQUE (user_id, plan_type, year)
);

CREATE TABLE plan_room_adjustments (
    id        uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id   uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    plan_type text        NOT NULL,
    year      integer     NOT NULL,
    -- Signed: positive adds room, negative removes it.
    amount    numeric(14,2) NOT NULL,
    note_encrypted bytea   NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
-- No unique constraint: a user may make several adjustments per plan-year.
CREATE INDEX plan_room_adjustments_user_idx ON plan_room_adjustments (user_id);

CREATE TABLE account_snapshots (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id          uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    account_id       uuid        NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    as_of            date        NOT NULL,
    balance          numeric(14,2) NOT NULL,
    note_encrypted   bytea       NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT account_snapshots_account_asof_uq
        UNIQUE (account_id, as_of)
);
CREATE INDEX account_snapshots_account_idx ON account_snapshots (account_id, as_of);

-- 5) dashboard_widgets: the savings widget kind.
ALTER TABLE dashboard_widgets DROP CONSTRAINT dashboard_widgets_kind_check;
ALTER TABLE dashboard_widgets ADD CONSTRAINT dashboard_widgets_kind_check
    CHECK (kind IN ('explore', 'markdown', 'savings'));

-- +goose Down

ALTER TABLE dashboard_widgets DROP CONSTRAINT dashboard_widgets_kind_check;
ALTER TABLE dashboard_widgets ADD CONSTRAINT dashboard_widgets_kind_check
    CHECK (kind IN ('explore', 'markdown'));

DROP TABLE account_snapshots;
DROP TABLE plan_room_adjustments;
DROP TABLE plan_limit_overrides;

ALTER TABLE transactions DROP CONSTRAINT transactions_external_origin_check;
ALTER TABLE transactions DROP COLUMN external_origin;
DROP INDEX idx_transactions_transfer_group;
ALTER TABLE transactions DROP COLUMN transfer_group_id;

ALTER TABLE accounts DROP COLUMN opened_at;
ALTER TABLE accounts DROP COLUMN opening_balance;
ALTER TABLE accounts DROP COLUMN beneficiary_birth_year_encrypted;
ALTER TABLE accounts DROP COLUMN beneficiary_name_encrypted;
ALTER TABLE accounts DROP CONSTRAINT accounts_type_check;
ALTER TABLE accounts ADD CONSTRAINT accounts_type_check
    CHECK (type IN ('bank', 'cash', 'credit_card', 'mortgage', 'investment', 'other'));

ALTER TABLE users DROP COLUMN birth_year_encrypted;

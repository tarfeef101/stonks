-- 0009_mortgages.sql — mortgages (plan step 7.1).
--
-- Table per plan §5 (encryption field inventory):
--   mortgages  the ENTIRE mortgage spec (principal, annual_rate,
--              amortization_years, start_date, payment_frequency,
--              extra_payment_amount/frequency, extra_prepayment_type)
--              is stored as ONE DEK-encrypted jsonb blob per §5: the
--              marshalled spec sealed as a single crypto.Seal pack in
--              spec_encrypted. There are NO plaintext spec columns:
--              the amortisation engine (7.2) and the UI (7.3) decrypt
--              in-app, and no mortgage data is ever queryable in SQL.
--              account_id is immutable (a mortgage belongs to exactly
--              one account — the server layer requires a 'mortgage'
--              type per 0002; the store itself does not validate it).
--
-- *_encrypted bytea columns hold [nonce ‖ ciphertext ‖ tag] packs produced
-- by internal/crypto (AES-256-GCM, step 2.4); see the 0001 header.

-- +goose Up

CREATE TABLE mortgages (
    id                uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id           uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    account_id        uuid        NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    spec_encrypted    bytea       NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX mortgages_user_idx ON mortgages (user_id);
CREATE INDEX mortgages_account_idx ON mortgages (account_id);

-- +goose Down

DROP TABLE mortgages;

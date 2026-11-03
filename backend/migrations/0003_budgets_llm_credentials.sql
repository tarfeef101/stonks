-- 0003_budgets_llm_credentials.sql — budgets + LLM credentials (step 2.3).
--
-- Tables per plan §5 (encryption field inventory) + DESIGN.md "Database":
--   budgets          per-user spending targets; name + the ENTIRE targets
--                    jsonb stored as ONE DEK-encrypted blob (§5: budget-
--                    vs-actual compares decrypted rows in-app — zero SQL
--                    cost); period shares the vocabulary pinned in 0001
--   llm_credentials  the user's own LLM provider credential (categorisation,
--                    step 5.x); endpoint + api_key DEK-encrypted, the
--                    provider enum stays plaintext (routing only, §5);
--                    at most one row per (user, provider)
--
-- *_encrypted bytea columns hold [nonce ‖ ciphertext ‖ tag] packs produced
-- by internal/crypto (AES-256-GCM, step 2.4); see the 0001 header.

-- +goose Up

CREATE TABLE budgets (
    id                uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id           uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    period            text        NOT NULL,
    active            boolean     NOT NULL DEFAULT true,
    name_encrypted    bytea       NOT NULL,
    -- The whole per-category targets jsonb, one pack (plan §5).
    targets_encrypted bytea       NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),

    -- Same CHECK-defined set as users.default_period (0001) and the
    -- report periods (8.x).
    CONSTRAINT budgets_period_check
        CHECK (period IN ('month', 'quarter', 'year'))
);
CREATE INDEX budgets_user_idx ON budgets (user_id);

CREATE TABLE llm_credentials (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id            uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- Routing only (plan §5: "provider enum stays"). One OpenAI-compatible
    -- implementation (step 5.1) covers all three: self-hosted llama.cpp
    -- (the 5.3 default), OpenAI, or any other compatible custom endpoint
    -- (5.6 picker vocabulary).
    provider           text        NOT NULL,
    endpoint_encrypted bytea       NOT NULL,
    -- Nullable: self-hosted llama.cpp takes no key. The app enforces the
    -- key requirement for cloud providers (2.6 / 5.6), not the schema.
    api_key_encrypted  bytea,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT llm_credentials_provider_key UNIQUE (user_id, provider),
    CONSTRAINT llm_credentials_provider_check
        CHECK (provider IN ('self_hosted', 'openai', 'custom'))
);

-- +goose Down

DROP TABLE llm_credentials;
DROP TABLE budgets;

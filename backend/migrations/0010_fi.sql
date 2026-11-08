-- 0010_fi.sql — FI institutions + credentials (plan step 9.1).
--
-- Tables per plan §5 (encryption field inventory):
--   fi_institutions  GLOBAL institution registry (code, name, status).
--              No user scoping, no encryption: institutions are shared
--              vocabulary, not per-user data. There is deliberately NO
--              institution-delete API (9.1), so rows are not deletable
--              from the application at all.
--   fi_credentials   per-user institution tokens. BOTH secrets are
--              stored as DEK-encrypted packs per §5: token_encrypted
--              and refresh_token_encrypted are TWO separate
--              crypto.Seal packs (one pack per secret, two packs per
--              row). The 9.2 sync worker decrypts in-app; no secret
--              data is ever queryable in SQL. last_synced_at is NULL
--              until the first sync (the 9.2 job stamps it via
--              FICredentialStore.MarkSynced). institution_id has NO
--              ON DELETE cascade — with no institution-delete path
--              there is no orphan path.
--
-- *_encrypted bytea columns hold [nonce ‖ ciphertext ‖ tag] packs produced
-- by internal/crypto (AES-256-GCM, step 2.4); see the 0001 header.

-- +goose Up

CREATE TABLE fi_institutions (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    code       text NOT NULL UNIQUE,
    name       text NOT NULL,
    status     text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE fi_credentials (
    id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id                 uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    institution_id          uuid NOT NULL REFERENCES fi_institutions(id),
    token_encrypted         bytea NOT NULL,
    refresh_token_encrypted bytea NOT NULL,
    last_synced_at          timestamptz,
    created_at              timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, institution_id)
);

CREATE INDEX fi_credentials_user_idx ON fi_credentials (user_id);

-- +goose Down

DROP TABLE fi_credentials;
DROP TABLE fi_institutions;

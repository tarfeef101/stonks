-- 0005_import_jobs.sql — import job lifecycle (step 4.1).
--
-- import_jobs is the upload/parse/import state machine: one row per
-- uploaded statement file. Per plan §5, file_encrypted (the raw
-- statement bytes) and file_name_encrypted (the original filename) are
-- DEK-encrypted packs — the statement content is the most sensitive
-- data the app holds. status walks the pipeline
-- uploaded -> mapping -> importing -> done|failed; result (jsonb) and
-- error (text) stay NULL until a later step (4.3+) reaches a terminal
-- state.
--
-- account_id mirrors transactions.account_id (0002): a job belongs to
-- the account its transactions land in, and deleting the account
-- cascades to its jobs (like its transactions).
--
-- Numbering note: the plan pins import_jobs at 0004; step 1.4 took 0004
-- for users.onboarded_at (recorded in 0004's header), so this is 0005.

-- +goose Up

CREATE TABLE import_jobs (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id             uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    account_id          uuid        NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    status              text        NOT NULL,
    file_encrypted      bytea       NOT NULL,
    file_name_encrypted bytea       NOT NULL,
    result              jsonb,
    error               text,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT import_jobs_status_check
        CHECK (status IN ('uploaded', 'mapping', 'importing', 'done', 'failed'))
);
CREATE INDEX import_jobs_user_idx ON import_jobs (user_id, created_at DESC);

-- +goose Down

DROP TABLE import_jobs;

-- 0007_categorise_jobs.sql — categorisation job queue (step 5.4).
--
-- categorise_jobs is the queue behind the LLM categorisation worker: one
-- row per categorisation request, created either by the manual
-- "categorise" endpoint (source = 'manual') or chained from an import
-- job that finished with inserted transactions (source = 'import').
-- status walks pending -> running -> done|failed; total/done track
-- progress, auto_applied/to_review record how suggestions were applied
-- (category_id set vs review_pending set), and llm_model plus the
-- llm_tokens_in/out pair give per-job usage visibility.
--
-- llm_cost_cents is reserved and stays 0 in iteration 1: token tracking
-- satisfies the plan's "token/cost tracking" requirement pragmatically
-- without a per-model price table (no price table ships in this repo).
--
-- The single worker node claims jobs with FOR UPDATE SKIP LOCKED; on
-- startup it re-queues 'running' rows left behind by a crash.

-- +goose Up

CREATE TABLE categorise_jobs (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id          uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    source           text        NOT NULL CHECK (source IN ('import', 'manual')),
    import_job_id    uuid        REFERENCES import_jobs (id) ON DELETE SET NULL,
    status           text        NOT NULL CHECK (status IN ('pending', 'running', 'done', 'failed')),
    total            integer     NOT NULL DEFAULT 0,
    done             integer     NOT NULL DEFAULT 0,
    auto_applied     integer     NOT NULL DEFAULT 0,
    to_review        integer     NOT NULL DEFAULT 0,
    llm_model        text,
    llm_tokens_in    integer     NOT NULL DEFAULT 0,
    llm_tokens_out   integer     NOT NULL DEFAULT 0,
    llm_cost_cents   integer     NOT NULL DEFAULT 0, -- reserved: iteration 1 always 0 (no per-model price table; tokens ARE tracked)
    error            text,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT categorise_jobs_import_fk_check
        CHECK (source <> 'import' OR import_job_id IS NOT NULL)
);
CREATE INDEX categorise_jobs_user_idx ON categorise_jobs (user_id, created_at DESC);
CREATE INDEX categorise_jobs_queue_idx ON categorise_jobs (status, created_at) WHERE status IN ('pending', 'running');

-- +goose Down

DROP TABLE categorise_jobs;

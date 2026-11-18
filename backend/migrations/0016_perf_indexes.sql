-- 0016_perf_indexes.sql — partial indexes for the review queue, the
-- uncategorized scan, and the worker offset tick's recent-import
-- enumeration (REVIEW-PLAN T-P1-6 + T-P1-4). Indexes only: no tables,
-- no columns, no data. 0017 (fx_rates) is untouched.

-- +goose Up

-- Review queue (GET /v1/review, T-P1-6c): ReviewPending and
-- CountReviewPending filter `user_id = $1 AND review_pending = true AND
-- category_id IS NULL`. The queue is small by design, so (user_id)
-- under the partial predicate suffices; category_id IS NULL is implied
-- while the flag is set (accept/reject clear the flag), and ORDER BY id
-- is a heap fetch over the few matched rows.
CREATE INDEX transactions_review_pending_idx
    ON transactions (user_id)
    WHERE review_pending = true;

-- Uncategorized scan (T-P1-6d): `category_id IS NULL AND review_pending
-- = false` had NO supporting index — the categorise runner rescanned it
-- across ~4000 batches of a 100k-row job.
CREATE INDEX transactions_uncategorized_idx
    ON transactions (user_id)
    WHERE category_id IS NULL AND review_pending = false;

-- Worker offset tick (T-P1-4, D2): enumerate users with recently
-- IMPORTED rows, then find pairs whose newest leg arrived after a
-- created_at watermark. Posting-date windows cannot catch late imports
-- of old statements, so the tick watermarks on created_at; this index
-- keeps both the enumeration and the per-user recent-rows scan
-- index-driven.
CREATE INDEX transactions_user_created_idx
    ON transactions (user_id, created_at);

-- +goose Down

DROP INDEX transactions_user_created_idx;
DROP INDEX transactions_uncategorized_idx;
DROP INDEX transactions_review_pending_idx;

-- 0012_offsets.sql — offset groups: transactions that net each other out.
--
-- Some rows are internal movements between the user's own accounts —
-- paying a credit card from chequing, moving money between two
-- chequing accounts — and must not count as spend in one account and
-- income in the other. transactions.offset_group_id ties the members of
-- one such group together: a group is a set of the caller's rows whose
-- amounts sum to exactly zero.
--
-- Grouped rows are hidden from every money view — the transaction List
-- and the Aggregate (explore, dashboard summary, reports) — while the
-- review queue and the uncategorized view keep them (they may still
-- need a category).
--
-- offset_suggestions records the pairs the matcher proposed so the user
-- can accept or dismiss them. tx_a < tx_b is the canonical pair order;
-- the UNIQUE (user_id, tx_a, tx_b) makes the matcher idempotent: once a
-- pair has a suggestion row of ANY status it is never re-suggested, so
-- dismissals stick until the group work is undone.

-- +goose Up
ALTER TABLE transactions
    ADD COLUMN offset_group_id uuid NULL;

CREATE INDEX transactions_offset_group_idx ON transactions (offset_group_id)
    WHERE offset_group_id IS NOT NULL;

-- Matcher join: same user, mirrored amounts, close posting dates.
CREATE INDEX transactions_offset_match_idx ON transactions (user_id, amount, date_posted);

CREATE TABLE offset_suggestions (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    tx_a       uuid        NOT NULL REFERENCES transactions (id) ON DELETE CASCADE,
    tx_b       uuid        NOT NULL REFERENCES transactions (id) ON DELETE CASCADE,
    status     text        NOT NULL DEFAULT 'pending'
               CHECK (status IN ('pending', 'accepted', 'dismissed')),
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (tx_a < tx_b),
    UNIQUE (user_id, tx_a, tx_b)
);

CREATE INDEX offset_suggestions_user_status_idx ON offset_suggestions (user_id, status);

-- +goose Down
DROP TABLE offset_suggestions;
DROP INDEX transactions_offset_match_idx;
DROP INDEX transactions_offset_group_idx;
ALTER TABLE transactions
    DROP COLUMN offset_group_id;

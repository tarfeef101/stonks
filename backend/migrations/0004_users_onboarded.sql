-- 0004_users_onboarded.sql — onboarding completion marker (step 1.4).
--
-- users.onboarded_at is the server-authoritative onboarding gate: the
-- settings endpoints (GET/PATCH /api/v1/settings) read and write it, and
-- the frontend (callback redirect, UserChip prompt, onboarding page state)
-- routes on it. A derivation-based marker does not work: every settings
-- column has a DB default (0001), so "all defaults" is indistinguishable
-- from "never visited the form". NULL = not onboarded; set once (the
-- `onboarded: true` patch flag maps to COALESCE(onboarded_at, now()) —
-- only-if-unset, never cleared, never overwritten).
--
-- Numbering note (recorded in docs/TECH-DECISIONS.md Change log): step 1.4
-- needs a migration now, so the plan-pinned migrations after 0003 shift by
-- one: import_jobs 0004->0005, transactions.review_pending 0005->0006,
-- dashboard_cards 0006->0007, mortgages 0007->0008, fi_institutions +
-- fi_credentials 0008->0009.

-- +goose Up
ALTER TABLE users ADD COLUMN onboarded_at timestamptz;

-- +goose Down
ALTER TABLE users DROP COLUMN onboarded_at;

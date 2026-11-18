-- 0018_budget_targets.sql — budget targets move to a child table; import
-- presets single-sourced to code (REVIEW-PLAN §5).
--
-- Two concerns, one migration:
--
--   budget_targets   Per-category budget targets as plain rows. Before, the
--                    whole targets jsonb lived in budgets.targets_encrypted
--                    as ONE DEK-encrypted blob; per-category rows are
--                    constraint-checkable (range, FK) and cascade-safe (a
--                    deleted category drops its target row, not the budget).
--                    The wire format is deliberately UNCHANGED — the same
--                    {category_id: number} JSON goes in and out of every
--                    endpoint; the repo hydrates/marshals the child rows.
--
--   import_schemas   The DB-maintained builtins (3 global rows) are deleted;
--                    the code builtins in internal/presets (served by
--                    GET /v1/imports/presets) are the SOLE preset source.
--                    The table is custom-only, so the shape CHECK shrinks to
--                    the custom half. `name` STAYS (always-NULL now): the
--                    repo's scans and DTO keep working with minimal churn.
--
-- Data loss (ACCEPTED): test data is disposable (pre-deploy). The old target
-- values were DEK-encrypted packs and are unrecoverable in SQL; no Go data
-- migration exists by design (the migrate service has no KEK context).
--
-- No extra indexes: the UNIQUE (budget_id, category_id) leftmost prefix
-- covers budget-id lookups; both FKs cover the cascade paths. budgets.active
-- already exists (0003).

-- +goose Up

-- 1) New child table: per-category targets.
CREATE TABLE budget_targets (
    id          uuid         PRIMARY KEY DEFAULT gen_random_uuid(),
    budget_id   uuid         NOT NULL REFERENCES budgets (id) ON DELETE CASCADE,
    category_id uuid         NOT NULL REFERENCES user_categories (id) ON DELETE CASCADE,
    target      numeric(14,2) NOT NULL,
    CONSTRAINT budget_targets_target_check
        CHECK (target BETWEEN 0 AND 999999999999.99),
    CONSTRAINT budget_targets_budget_category_uq
        UNIQUE (budget_id, category_id)
);

-- 2) Presets single-source: the DB builtins are deleted; the code builtins
--    in internal/presets are the sole source.
DELETE FROM import_schemas WHERE builtin = true;
ALTER TABLE import_schemas DROP CONSTRAINT import_schemas_shape_check;
ALTER TABLE import_schemas DROP COLUMN builtin;
ALTER TABLE import_schemas ADD CONSTRAINT import_schemas_shape_check
    CHECK (user_id IS NOT NULL AND name IS NULL AND name_encrypted IS NOT NULL);

-- 3) Budget targets move to the child table. Old values were DEK-encrypted
--    packs and are unrecoverable in SQL; test data is disposable, loss
--    accepted — see header.
ALTER TABLE budgets DROP COLUMN targets_encrypted;

-- +goose Down

ALTER TABLE budgets ADD COLUMN targets_encrypted bytea NOT NULL DEFAULT ''::bytea;
-- Old values were DEK-encrypted packs and are unrecoverable in SQL;
-- test-data-only deploy, loss accepted — see the Up comment above.
DROP TABLE budget_targets;
-- The narrowed custom-only shape CHECK from Up would reject the builtin
-- rows, so restore the 0001 shape AROUND the re-seed: drop it first,
-- re-add the column + rows, then re-add the original constraint.
ALTER TABLE import_schemas DROP CONSTRAINT import_schemas_shape_check;
ALTER TABLE import_schemas ADD COLUMN builtin boolean NOT NULL DEFAULT false;
INSERT INTO import_schemas (name, kind, builtin, spec) VALUES
    ('Generic CSV',   'csv',  true, '{}'),
    ('OFX Statement', 'ofx',  true, '{}'),
    ('Generic JSON',  'json', true, '{}');
ALTER TABLE import_schemas ADD CONSTRAINT import_schemas_shape_check
    CHECK ((builtin AND user_id IS NULL AND name IS NOT NULL
            AND name_encrypted IS NULL)
         OR (NOT builtin AND user_id IS NOT NULL AND name IS NULL
            AND name_encrypted IS NOT NULL));

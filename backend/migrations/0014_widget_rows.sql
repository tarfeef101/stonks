-- 0014_widget_rows.sql — dashboard widget row spans (round 7, drag resize).
--
-- Adds row_span to dashboard_widgets: the number of fixed-height grid rows
-- a widget occupies (span = the column count, 0013). The editor's grid is
-- 12 columns of fixed-height rows (md:auto-rows), so row_span 1..8 is a
-- fixed pixel height; a widget whose content overflows scrolls inside the
-- card. The drag handles (right edge = width, bottom edge = height, corner
-- = both) patch span/row_span on release.
--
-- Backfill: existing widgets take the same per-kind defaults a fresh create
-- gets — explore 4, markdown 2 (repo.Create, server widgetsCreate).

-- +goose Up

ALTER TABLE dashboard_widgets
  ADD COLUMN row_span integer NOT NULL CHECK (row_span >= 1 AND row_span <= 8) DEFAULT 4;

UPDATE dashboard_widgets SET row_span = 2 WHERE kind = 'markdown';

-- +goose Down

ALTER TABLE dashboard_widgets DROP COLUMN row_span;

-- 0011_users_theme.sql — per-user appearance theme (dark / light / system).
--
-- users.theme is the server-side record of the user's appearance choice:
-- 'light' forces the light palette, 'dark' forces the dark palette, and
-- 'system' (the default) defers to the browser's prefers-color-scheme.
-- The frontend applies it to the document on load and keeps the <html>
-- class in sync with this store; with 'system' it also follows the OS /
-- browser preference live (matchMedia change events).
--
-- The CHECK mirrors the frontend's select so an invalid value 400s in
-- validatePatch (backend/internal/server/settings.go) before it can reach
-- the DB constraint.

-- +goose Up
ALTER TABLE users
    ADD COLUMN theme text NOT NULL DEFAULT 'system',
    ADD CONSTRAINT users_theme_check CHECK (theme IN ('light', 'dark', 'system'));

-- +goose Down
ALTER TABLE users
    DROP CONSTRAINT users_theme_check,
    DROP COLUMN theme;

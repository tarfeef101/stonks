-- 0006_import_jobs_mapping.sql — import job mapping document (step 4.5).
--
-- mapping (jsonb, NULL while unset) holds the field-to-column mapping
-- the user confirmed for the uploaded file:
--
--     {"kind":"csv|ofx|json",
--      "array_pointers":["/transactions"],   -- json files only
--      "map":{"date_posted":{"headers":["date"],"index":1},
--              "amount":{"index":2}}}
--
-- kind must match the uploaded file's extension; the per-field
-- shape is csv = headers (candidate names, first match wins) + index
-- (0-based fallback column, -1 = unmapped), json = pointers (RFC 6901,
-- tried in order), ofx = {} (ofxparse already emits the app fields,
-- so the mapping is the identity). The same document is stored verbatim
-- in import_schemas.spec when the user saves the mapping as a preset
-- (POST /api/v1/import-schemas), so the 4.6 runner can read one job's
-- mapping and apply a saved preset with the same code path.

-- +goose Up

ALTER TABLE import_jobs ADD COLUMN mapping jsonb;

-- +goose Down

ALTER TABLE import_jobs DROP COLUMN mapping;

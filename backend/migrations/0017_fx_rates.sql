-- 0017_fx_rates.sql — FX rates from the Frankfurter API (rate lookup feature).
--
-- fx_rates is GLOBAL (no user_id, no DEK): exchange rates are shared by
-- every user, so the table is plain columns, not per-user encrypted
-- packs. One row per (fx_date, currency): the rate of `currency` against
-- the upstream's base currency for that day. The base currency is NOT
-- stored and must not be assumed — the fx package converts between any
-- two currencies as rate[to]/rate[from], which is base-independent.
--
-- Populated by the fx worker entry (RunDaily, the latest day, upserted)
-- and by on-demand backfill when a lookup misses. Rates lag for some
-- codes (crypto, metals), so lookups fall back to the nearest EARLIER
-- stored date — this table is append-mostly and never needs backdating.

-- +goose Up

CREATE TABLE fx_rates (
    fx_date date         NOT NULL,
    currency char(3)     NOT NULL,
    rate    numeric(20,8) NOT NULL,
    PRIMARY KEY (fx_date, currency)
);

-- +goose Down

DROP TABLE fx_rates;

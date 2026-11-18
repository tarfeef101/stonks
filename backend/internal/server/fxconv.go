// Display-only FX conversion for the summary and transaction routes
// (REVIEW-PLAN §4.4): amounts are converted into the user's default
// currency (users.default_currency) before the response is assembled.
// Every failure mode — no client configured, an empty currency, an
// unsupported pair, a missing stored rate, a lookup error — degrades to
// "show the amount in its own currency" (identity factor, converted
// false); conversion never fails a request. Rounding is half-away-from-
// zero at the cent: int64(math.Round(cents * factor)), the same
// convention the report dispatch uses.
package server

import (
	"context"
	"log/slog"
	"math"
	"time"

	"finance-tracker/internal/fx"
	"finance-tracker/internal/repo"
)

// FX resolves display-only FX rates for a user's base currency. The
// production value is *fx.Client; tests use a fake or nil (nil disables
// conversion). Rate mirrors fx.Client.Rate.
type FX interface {
	Rate(ctx context.Context, date time.Time, from, to string) (fx.Rate, error)
}

// fxRateEntry is one memoized (day, currency -> base) resolution.
type fxRateEntry struct {
	factor    float64
	converted bool
}

// fxCache memoizes rate resolutions for the lifetime of one request.
// The handlers drive it sequentially (one goroutine per request), so no
// locking is needed. base is the user's default currency; an empty base
// or a nil fx disables conversion entirely.
type fxCache struct {
	fx    FX
	base  string
	rates map[string]fxRateEntry
}

// newFXCache returns a cache for one request; either field may be nil
// or empty (conversion off, pre-change behavior).
func newFXCache(fxClient FX, base string) *fxCache {
	return &fxCache{fx: fxClient, base: base, rates: map[string]fxRateEntry{}}
}

// convert resolves the factor for cur -> base on the UTC day of date.
// It returns (1, false) — "display as-is" — when conversion is
// disabled, cur equals base, the pair is unsupported by upstream, or
// the lookup failed (a warning is logged; ErrNoRate is the common case
// before the fx-rates worker has stored a day).
func (c *fxCache) convert(ctx context.Context, date time.Time, cur string) (factor float64, converted bool) {
	if c == nil || c.fx == nil || c.base == "" || cur == "" || cur == c.base {
		return 1, false
	}
	day := date.UTC().Format("2006-01-02")
	key := day + "|" + cur + "->" + c.base
	if e, ok := c.rates[key]; ok {
		return e.factor, e.converted
	}
	e := fxRateEntry{factor: 1}
	if r, err := c.fx.Rate(ctx, date, cur, c.base); err != nil {
		slog.Warn("fx lookup failed; showing amount unconverted",
			"date", day, "from", cur, "to", c.base, "error", err)
	} else if r.Converted {
		e = fxRateEntry{factor: r.Factor, converted: true}
		// !Converted: upstream does not support the pair — keep the
		// original currency, no error (display-only).
	}
	c.rates[key] = e
	return e.factor, e.converted
}

// convertRow converts one aggregate group's canonical cent strings to
// base-currency cents (spend, income). With conversion disabled — or
// for a group whose currency already is the base — the original cents
// pass through unchanged.
func (c *fxCache) convertRow(ctx context.Context, date time.Time, g repo.CategorySpend) (spend, income int64, err error) {
	sp, err := parseCents(g.Spend)
	if err != nil {
		return 0, 0, err
	}
	inc, err := parseCents(g.Income)
	if err != nil {
		return 0, 0, err
	}
	if factor, ok := c.convert(ctx, date, g.Currency); ok {
		sp = int64(math.Round(float64(sp) * factor))
		inc = int64(math.Round(float64(inc) * factor))
	}
	return sp, inc, nil
}

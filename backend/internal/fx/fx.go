// Package fx resolves daily FX rates for arbitrary currency pairs,
// backed by the Frankfurter API (v2, no API key).
//
// Rates are stored globally in fx_rates (one row per (fx_date,
// currency), against the upstream's base currency for that day) and are
// populated by RunDaily (the worker's daily entry) plus an on-demand
// backfill when a lookup misses. A lookup converts between ANY two
// supported currencies as rate[to]/rate[from]; no base currency is
// assumed or hardcoded anywhere in this package.
//
// Upstream shape (verified against the live API, unlike the v1-style
// docs): GET /v2/rates returns a FLAT JSON array of {date, base, quote,
// rate} records — one per (date, quote) pair — and the latest-day
// response may span several dates (crypto lags fiat by a day). The
// base's own 1.0 row is often omitted from the records; the upsert
// inserts it explicitly, reading the base code from the response.
package fx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// defaultBaseURL is the production Frankfurter v2 endpoint.
	defaultBaseURL = "https://api.frankfurter.dev"
	// backfillWindowDays is how far back an on-demand backfill reaches
	// in one upstream request. 35 days covers the longest observed
	// upstream gaps (crypto holiday lags) with headroom; anything older
	// is out of scope for rate conversion by design.
	backfillWindowDays = 35
	// backfillCooldown is how long after ANY attempt (success or
	// failure) the same (from, to) pair will not trigger another
	// backfill fetch. A few minutes is enough to coalesce concurrent
	// misses and survive a caller retry loop without hammering the
	// upstream; a failed attempt records the cooldown too — the gap
	// will re-fill on the next attempt outside it, and a persistently
	// broken upstream should surface via the daily run, not a hot loop.
	backfillCooldown = 5 * time.Minute
	// maxResponseBytes bounds every upstream read (defense against a
	// runaway or hostile response body).
	maxResponseBytes = 10 << 20
)

// errNoRate is the internal "no stored rate on or before the date"
// sentinel; Rate turns it into the caller-facing error after the
// backfill retry.
var errNoRate = errors.New("fx: no stored rate")

// Rate is the result of one lookup. The caller converts with
// amount*Factor; when Converted is false the amount was NOT converted
// (an unsupported currency) and stays in the original currency.
type Rate struct {
	// Converted reports whether the conversion was performed.
	Converted bool
	// Factor is rate[to]/rate[from] (1 when !Converted).
	Factor float64
	// FromDate and ToDate are the days the two rates were actually
	// drawn from: the requested date when stored there, otherwise the
	// nearest stored date EARLIER (upstream data lags for some codes).
	FromDate time.Time
	ToDate   time.Time
}

// Client resolves FX rates against a pool. It owns the per-process
// caches (the /v2/currencies support list, the backfill cooldown);
// construct one per process and share it.
type Client struct {
	pool    *pgxpool.Pool
	baseURL string
	http    *http.Client
	// Log receives the summary lines RunDaily emits; defaults to
	// slog.Default() (schedule-style).
	Log *slog.Logger

	supportedMu sync.Mutex
	supported   map[string]bool // ISO codes from /v2/currencies; nil until loaded

	backfillMu   sync.Mutex
	backfillLast map[string]time.Time // "FROM|TO" -> instant of last attempt
}

// New returns a Client for the production Frankfurter endpoint.
func New(pool *pgxpool.Pool) *Client {
	return NewForBaseURL(pool, defaultBaseURL)
}

// NewForBaseURL returns a Client rooted at baseURL (tests point this at
// an httptest server).
func NewForBaseURL(pool *pgxpool.Pool, baseURL string) *Client {
	return &Client{
		pool:         pool,
		baseURL:      strings.TrimSuffix(baseURL, "/"),
		http:         &http.Client{Timeout: 30 * time.Second},
		Log:          slog.Default(),
		backfillLast: map[string]time.Time{},
	}
}

// Rate resolves the conversion factor from -> to on the given day
// (the time-of-day is ignored; the day is taken in UTC). It falls back
// to the nearest stored date EARLIER than the requested one on each
// side, backfills on demand when a supported currency is missing, and
// returns Converted=false (no error) when either currency is not
// supported by the upstream at all — the caller then keeps the amount
// in the original currency.
func (c *Client) Rate(ctx context.Context, date time.Time, from, to string) (Rate, error) {
	from = strings.ToUpper(from)
	to = strings.ToUpper(to)
	if from == "" || to == "" {
		return Rate{}, fmt.Errorf("fx: currency code must be non-empty")
	}
	day := time.Date(date.UTC().Year(), date.UTC().Month(), date.UTC().Day(), 0, 0, 0, 0, time.UTC)
	if from == to {
		// Identity: no DB or upstream round trip at all.
		return Rate{Converted: true, Factor: 1, FromDate: day, ToDate: day}, nil
	}

	fromOK, err := c.isSupported(ctx, from)
	if err != nil {
		return Rate{}, err
	}
	toOK, err := c.isSupported(ctx, to)
	if err != nil {
		return Rate{}, err
	}
	if !fromOK || !toOK {
		// Unsupported currency: report "not converted" and let the
		// caller keep the original currency — deliberately NOT an
		// error, and no backfill is attempted for codes the upstream
		// does not know.
		return Rate{Converted: false, Factor: 1, FromDate: day, ToDate: day}, nil
	}

	fromDate, fromRateVal, fromErr := c.latestRate(ctx, day, from)
	toDate, toRateVal, toErr := c.latestRate(ctx, day, to)
	if errors.Is(fromErr, errNoRate) || errors.Is(toErr, errNoRate) {
		var missing []string
		if errors.Is(fromErr, errNoRate) {
			missing = append(missing, from)
		}
		if errors.Is(toErr, errNoRate) {
			missing = append(missing, to)
		}
		if berr := c.backfill(ctx, day, from, to, missing); berr != nil {
			return Rate{}, berr
		}
		if errors.Is(fromErr, errNoRate) {
			fromDate, fromRateVal, fromErr = c.latestRate(ctx, day, from)
		}
		if errors.Is(toErr, errNoRate) {
			toDate, toRateVal, toErr = c.latestRate(ctx, day, to)
		}
	}
	if fromErr != nil {
		return Rate{}, fromErr
	}
	if toErr != nil {
		return Rate{}, toErr
	}
	return Rate{
		Converted: true,
		Factor:    toRateVal / fromRateVal,
		FromDate:  fromDate,
		ToDate:    toDate,
	}, nil
}

// RunDaily fetches the upstream's latest day (which may span a couple
// of dates: crypto lags fiat) and upserts every record, then logs a
// one-line summary. It is the worker's scheduled entry; errors are
// logged by the scheduler and retried on the next fire.
func (c *Client) RunDaily(ctx context.Context) error {
	records, err := c.fetchRates(ctx, "/v2/rates", nil)
	if err != nil {
		return err
	}
	if err := c.upsert(ctx, records); err != nil {
		return err
	}
	dates := map[string]bool{}
	for _, r := range records {
		dates[r.Date] = true
	}
	c.Log.InfoContext(ctx, "fx daily rates stored",
		"dates", len(dates), "records", len(records))
	return nil
}

// latestRate returns the stored rate for code on or before onOrBefore
// (the nearest earlier date), or errNoRate when nothing is stored yet.
func (c *Client) latestRate(ctx context.Context, onOrBefore time.Time, code string) (time.Time, float64, error) {
	var (
		day  time.Time
		rate float64
	)
	err := c.pool.QueryRow(ctx, `
		SELECT fx_date, rate
		FROM fx_rates
		WHERE fx_date <= $1 AND currency = $2
		ORDER BY fx_date DESC
		LIMIT 1`, onOrBefore.Format("2006-01-02"), code).Scan(&day, &rate)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, 0, errNoRate
		}
		return time.Time{}, 0, fmt.Errorf("fx: lookup %s on/before %s: %w",
			code, onOrBefore.Format("2006-01-02"), err)
	}
	return day, rate, nil
}

// backfill fetches the window [day-35d, day] in ONE upstream request,
// with quotes limited to the codes that are actually missing, and
// upserts the response. Cooldown + mutex: the check-and-set of the
// attempt instant happens under the lock, the fetch outside it, so
// concurrent misses for the same pair coalesce into one request — the
// first caller fetches, the rest skip to the retry (if the fetch is
// still in flight when they retry they get "no rate" for this call and
// succeed on the next).
func (c *Client) backfill(ctx context.Context, day time.Time, from, to string, missing []string) error {
	key := from + "|" + to
	now := time.Now()
	c.backfillMu.Lock()
	last, seen := c.backfillLast[key]
	if seen && now.Sub(last) < backfillCooldown {
		c.backfillMu.Unlock()
		c.Log.DebugContext(ctx, "backfill in cooldown, skipping upstream fetch",
			"from", from, "to", to,
			"retry_in", time.Until(last.Add(backfillCooldown)).Round(time.Second).String())
		return nil
	}
	c.backfillLast[key] = now
	c.backfillMu.Unlock()

	q := url.Values{}
	q.Set("start_date", day.AddDate(0, 0, -backfillWindowDays).Format("2006-01-02"))
	q.Set("end_date", day.Format("2006-01-02"))
	q.Set("quotes", strings.Join(missing, ","))
	records, err := c.fetchRates(ctx, "/v2/rates", q)
	if err != nil {
		return err
	}
	return c.upsert(ctx, records)
}

// upsert stores records in one statement. Dates are bound as text[]
// and cast on the server side (the repo's date-binding convention:
// never let a timestamptz session TZ interpret a date literal).
func (c *Client) upsert(ctx context.Context, records []rateRecord) error {
	if len(records) == 0 {
		return nil
	}
	dates, codes, rates := normalize(records)
	_, err := c.pool.Exec(ctx, `
		INSERT INTO fx_rates (fx_date, currency, rate)
		SELECT d::date, c, r
		FROM unnest($1::text[], $2::text[], $3::float8[]) AS t(d, c, r)
		ON CONFLICT (fx_date, currency) DO UPDATE SET rate = EXCLUDED.rate`,
		dates, codes, rates)
	if err != nil {
		return fmt.Errorf("fx: upsert %d rate rows: %w", len(dates), err)
	}
	return nil
}

// isSupported reports whether code is in the upstream's /v2/currencies
// list, loaded once per Client (a per-process cache; a load failure is
// NOT cached, so a transient upstream blip does not wedge the process).
func (c *Client) isSupported(ctx context.Context, code string) (bool, error) {
	c.supportedMu.Lock()
	defer c.supportedMu.Unlock()
	if c.supported != nil {
		return c.supported[code], nil
	}
	var list []currencyInfo
	body, err := c.get(ctx, "/v2/currencies", nil)
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return false, fmt.Errorf("fx: decode /v2/currencies: %w", err)
	}
	c.supported = make(map[string]bool, len(list))
	for _, cur := range list {
		c.supported[strings.ToUpper(cur.ISOCode)] = true
	}
	return c.supported[code], nil
}

// fetchRates performs GET path?q and decodes the flat v2 rate records.
func (c *Client) fetchRates(ctx context.Context, path string, q url.Values) ([]rateRecord, error) {
	body, err := c.get(ctx, path, q)
	if err != nil {
		return nil, err
	}
	var records []rateRecord
	if err := json.Unmarshal(body, &records); err != nil {
		return nil, fmt.Errorf("fx: decode %s: %w", path, err)
	}
	return records, nil
}

// get performs the HTTP GET shared by both endpoints (plain client,
// 30 s timeout, no API key, bounded body).
func (c *Client) get(ctx context.Context, path string, q url.Values) ([]byte, error) {
	target := c.baseURL + path
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("fx: build %s request: %w", path, err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fx: GET %s: %w", target, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("fx: read %s: %w", target, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fx: GET %s: status %s", target, resp.Status)
	}
	return body, nil
}

// rateRecord is one element of the /v2/rates flat array: the rate of
// Quote against Base on Date.
type rateRecord struct {
	Date  string  `json:"date"`
	Base  string  `json:"base"`
	Quote string  `json:"quote"`
	Rate  float64 `json:"rate"`
}

// currencyInfo is the field of a /v2/currencies entry this package
// uses (the rest — name, symbol, start/end dates — is ignored).
type currencyInfo struct {
	ISOCode string `json:"iso_code"`
}

// normalize flattens records into the three parallel arrays the upsert
// binds: de-duplicated per (date, quote), with the feed's base
// currency explicitly present at 1.0 for every date it covers (the
// base code is read from the records, never assumed), and sorted so
// the statement is deterministic.
func normalize(records []rateRecord) (dates, codes []string, rates []float64) {
	byDate := map[string]map[string]float64{}
	for _, rec := range records {
		row := byDate[rec.Date]
		if row == nil {
			row = map[string]float64{}
			byDate[rec.Date] = row
		}
		if _, ok := row[rec.Quote]; !ok {
			row[rec.Quote] = rec.Rate
		}
		if rec.Base != "" {
			if _, ok := row[rec.Base]; !ok {
				row[rec.Base] = 1.0
			}
		}
	}
	type rateRow struct {
		date string
		code string
		rate float64
	}
	rows := make([]rateRow, 0, len(records))
	for date, row := range byDate {
		for code, rate := range row {
			rows = append(rows, rateRow{date, code, rate})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].date != rows[j].date {
			return rows[i].date < rows[j].date
		}
		return rows[i].code < rows[j].code
	})
	for _, r := range rows {
		dates = append(dates, r.date)
		codes = append(codes, r.code)
		rates = append(rates, r.rate)
	}
	return dates, codes, rates
}

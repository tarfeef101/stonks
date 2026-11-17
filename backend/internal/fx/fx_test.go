// Tests for the fx package. One postgres:18 testcontainer with all
// goose migrations applied serves the WHOLE test binary (TestMain —
// the container is the expensive part, ~7-15 s); every test wipes
// fx_rates first, so no seeding or cleanup races. migratedPool/
// pingUntilReady are copies of the repo package's helpers (per-package
// copy, D-019); fx_rates is global, so no users rows are seeded.
//
// The upstream is an httptest fake recording every /v2/rates request,
// so backfill range construction, quote scoping and the cooldown are
// asserted on the actual wire.
package fx

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// Package-level environment started by TestMain.
var (
	testPool *pgxpool.Pool
	testCtx  = context.Background()
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	ctr, err := postgres.Run(ctx, "postgres:18", postgres.BasicWaitStrategies())
	if err != nil {
		fmt.Fprintf(os.Stderr, "fx test: testcontainers postgres: %v\n", err)
		os.Exit(1)
	}
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "fx test: connection string: %v\n", err)
		os.Exit(1)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fx test: open db: %v\n", err)
		os.Exit(1)
	}
	if err := pingUntilReady(ctx, db, 60*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "fx test: postgres readiness: %v\n", err)
		os.Exit(1)
	}
	if err := goose.SetDialect("postgres"); err != nil {
		fmt.Fprintf(os.Stderr, "fx test: goose dialect: %v\n", err)
		os.Exit(1)
	}
	if err := goose.UpContext(ctx, db, "../../migrations"); err != nil {
		fmt.Fprintf(os.Stderr, "fx test: goose up: %v\n", err)
		os.Exit(1)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fx test: pgx pool: %v\n", err)
		os.Exit(1)
	}
	if err := pool.Ping(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "fx test: pool ping: %v\n", err)
		os.Exit(1)
	}
	testPool = pool

	code := m.Run()

	pool.Close()
	_ = db.Close()
	sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = ctr.Stop(sctx, nil)
	os.Exit(code)
}

// pingUntilReady polls db.PingContext until it succeeds or the budget
// runs out (copy of the repo/server helpers: the container's built-in
// wait alone is not trusted — postgres can accept connections before
// it is ready to serve them).
func pingUntilReady(ctx context.Context, db *sql.DB, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	var lastErr error
	for {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		lastErr = db.PingContext(pctx)
		cancel()
		if lastErr == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("after %v: %w", budget, lastErr)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// reset wipes fx_rates: the table is global and the tests run
// sequentially, so a fresh table per test is full isolation.
func reset(t *testing.T) {
	t.Helper()
	if _, err := testPool.Exec(testCtx, `DELETE FROM fx_rates`); err != nil {
		t.Fatalf("reset fx_rates: %v", err)
	}
}

// d parses a "2006-01-02" test date (UTC midnight).
func d(s string) time.Time {
	tm, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return tm
}

type seedRow struct {
	when string
	code string
	rate float64
}

func seed(t *testing.T, rows ...seedRow) {
	t.Helper()
	for _, r := range rows {
		if _, err := testPool.Exec(testCtx,
			`INSERT INTO fx_rates (fx_date, currency, rate) VALUES ($1, $2, $3)`,
			r.when, r.code, r.rate); err != nil {
			t.Fatalf("seed %s %s: %v", r.when, r.code, err)
		}
	}
}

// fakeFrankfurter is the httptest upstream. It serves /v2/currencies
// (fixed codes) and /v2/rates (series, called with the request query)
// and records every /v2/rates query for assertions.
type fakeFrankfurter struct {
	mu         sync.Mutex
	rateReq    []url.Values // one entry per /v2/rates request, in order
	currencies []string
	series     func(url.Values) []rateRecord
	server     *httptest.Server
}

func newFakeFrankfurter(t *testing.T, currencies []string, series func(url.Values) []rateRecord) *fakeFrankfurter {
	t.Helper()
	f := &fakeFrankfurter{currencies: currencies, series: series}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/currencies":
			out := make([]currencyInfo, 0, len(f.currencies))
			for _, code := range f.currencies {
				out = append(out, currencyInfo{ISOCode: code})
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(out); err != nil {
				t.Errorf("encode currencies: %v", err)
			}
		case "/v2/rates":
			f.mu.Lock()
			f.rateReq = append(f.rateReq, r.URL.Query())
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(f.series(r.URL.Query())); err != nil {
				t.Errorf("encode rates: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeFrankfurter) rateRequests() []url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]url.Values, len(f.rateReq))
	copy(out, f.rateReq)
	return out
}

// newClient builds a Client against the fake with a quiet logger.
// Every test gets a FRESH client: the support-list and cooldown caches
// are per-Client, so a fresh one is full cache isolation.
func newClient(t *testing.T, f *fakeFrankfurter) *Client {
	t.Helper()
	c := NewForBaseURL(testPool, f.server.URL)
	c.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	return c
}

func wantRate(t *testing.T, got Rate, err error, want Rate) {
	t.Helper()
	if err != nil {
		t.Fatalf("Rate: %v", err)
	}
	if got.Converted != want.Converted {
		t.Fatalf("Converted = %v, want %v (+%v)", got.Converted, want.Converted, got)
	}
	if math.Abs(got.Factor-want.Factor) > 1e-9 {
		t.Errorf("Factor = %v, want %v", got.Factor, want.Factor)
	}
	if !got.FromDate.Equal(want.FromDate) {
		t.Errorf("FromDate = %s, want %s", got.FromDate.Format("2006-01-02"), want.FromDate.Format("2006-01-02"))
	}
	if !got.ToDate.Equal(want.ToDate) {
		t.Errorf("ToDate = %s, want %s", got.ToDate.Format("2006-01-02"), want.ToDate.Format("2006-01-02"))
	}
}

// TestRateLookup covers the nearest-earlier-date fallback on BOTH
// sides, the identity short-circuit, the conversion math on a
// EUR-base feed, and the empty-code error — all from seeded rows, with
// NO backfill expected (the fake's series is never allowed to be
// called into a stored state).
func TestRateLookup(t *testing.T) {
	reset(t)
	f := newFakeFrankfurter(t, []string{"EUR", "USD", "JPY"}, func(q url.Values) []rateRecord {
		t.Fatalf("backfill must not fire: everything is seeded")
		return nil
	})
	c := newClient(t, f)
	seed(t,
		seedRow{"2026-10-01", "EUR", 1.0},
		seedRow{"2026-10-01", "USD", 1.10},
		seedRow{"2026-11-09", "EUR", 1.0},
		seedRow{"2026-11-09", "USD", 1.25},
		seedRow{"2026-11-09", "JPY", 200.0},
	)

	cases := []struct {
		name      string
		date      time.Time
		from, to  string
		want      Rate
		wantError bool
	}{
		{
			name: "exact dates both sides",
			date: d("2026-11-09"),
			from: "usd", to: "jpy", // lowercase in, case-insensitive
			want: Rate{Converted: true, Factor: 160.0, FromDate: d("2026-11-09"), ToDate: d("2026-11-09")},
		},
		{
			name: "fallback both sides to same earlier day",
			date: d("2026-11-12"),
			from: "USD", to: "JPY",
			want: Rate{Converted: true, Factor: 160.0, FromDate: d("2026-11-09"), ToDate: d("2026-11-09")},
		},
		{
			name: "fallback into earlier month",
			date: d("2026-11-05"),
			from: "EUR", to: "USD",
			want: Rate{Converted: true, Factor: 1.10, FromDate: d("2026-10-01"), ToDate: d("2026-10-01")},
		},
		{
			name: "reverse direction math",
			date: d("2026-11-09"),
			from: "JPY", to: "USD",
			want: Rate{Converted: true, Factor: 1.25 / 200.0, FromDate: d("2026-11-09"), ToDate: d("2026-11-09")},
		},
		{
			name: "time of day ignored (UTC day)",
			date: time.Date(2026, 11, 9, 13, 45, 0, 0, time.UTC),
			from: "USD", to: "JPY",
			want: Rate{Converted: true, Factor: 160.0, FromDate: d("2026-11-09"), ToDate: d("2026-11-09")},
		},
		{
			name: "identity short-circuits",
			date: d("2026-11-09"),
			from: "usd", to: "USD",
			want: Rate{Converted: true, Factor: 1, FromDate: d("2026-11-09"), ToDate: d("2026-11-09")},
		},
		{
			name: "empty to code",
			date: d("2026-11-09"),
			from: "USD", to: "",
			wantError: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := c.Rate(testCtx, tc.date, tc.from, tc.to)
			if tc.wantError {
				if err == nil {
					t.Fatalf("want error, got %+v", got)
				}
				return
			}
			wantRate(t, got, err, tc.want)
		})
	}

	// Nothing above should have touched /v2/rates (only the cached
	// /v2/currencies probe).
	if reqs := f.rateRequests(); len(reqs) != 0 {
		t.Fatalf("expected no /v2/rates requests, got %d: %v", len(reqs), reqs)
	}
}

// TestRateUnsupported: a currency absent from /v2/currencies yields
// Converted=false with NO error and NO backfill attempt.
func TestRateUnsupported(t *testing.T) {
	reset(t)
	f := newFakeFrankfurter(t, []string{"EUR", "USD"}, func(q url.Values) []rateRecord {
		t.Fatalf("backfill must not fire for an unsupported currency")
		return nil
	})
	c := newClient(t, f)

	day := d("2026-11-15")
	for _, tc := range []struct{ name, from, to string }{
		{"unsupported to", "USD", "ZZZ"},
		{"unsupported from", "ZZZ", "USD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := c.Rate(testCtx, day, tc.from, tc.to)
			if err != nil {
				t.Fatalf("unsupported currency must not error: %v", err)
			}
			if got.Converted {
				t.Fatalf("want Converted=false, got %+v", got)
			}
			if got.Factor != 1 {
				t.Errorf("Factor = %v, want 1", got.Factor)
			}
		})
	}
	if reqs := f.rateRequests(); len(reqs) != 0 {
		t.Fatalf("expected no /v2/rates requests, got %d", len(reqs))
	}
}

// TestRateBackfill: an empty table triggers ONE upstream request whose
// range is [day-35d, day] and whose quotes are EXACTLY the missing
// currencies (never the base); the omitted base row must be stored at
// 1.0 by the upsert.
func TestRateBackfill(t *testing.T) {
	reset(t)
	// The fake honors the quotes param the way the real API does: it
	// answers only the requested quotes, base EUR, omitting the base's
	// own 1.0 row (which is common in the live API).
	f := newFakeFrankfurter(t, []string{"EUR", "USD", "JPY"}, func(q url.Values) []rateRecord {
		var out []rateRecord
		for _, code := range strings.Split(q.Get("quotes"), ",") {
			var rate float64
			switch code {
			case "USD":
				rate = 1.25
			case "JPY":
				rate = 200.0
			}
			for _, day := range []string{"2026-11-14", "2026-11-15"} {
				out = append(out, rateRecord{Date: day, Base: "EUR", Quote: code, Rate: rate})
			}
		}
		return out
	})
	c := newClient(t, f)

	got, err := c.Rate(testCtx, d("2026-11-15"), "usd", "jpy")
	wantRate(t, got, err, Rate{Converted: true, Factor: 160.0, FromDate: d("2026-11-15"), ToDate: d("2026-11-15")})

	reqs := f.rateRequests()
	if len(reqs) != 1 {
		t.Fatalf("want exactly one backfill request for the whole gap, got %d: %v", len(reqs), reqs)
	}
	q := reqs[0]
	if got := q.Get("start_date"); got != "2026-10-11" {
		t.Errorf("start_date = %q, want %q (day - 35 days)", got, "2026-10-11")
	}
	if got := q.Get("end_date"); got != "2026-11-15" {
		t.Errorf("end_date = %q, want %q (the requested day)", got, "2026-11-15")
	}
	if got := q.Get("quotes"); got != "USD,JPY" {
		t.Errorf("quotes = %q, want %q (only the missing currencies)", got, "USD,JPY")
	}

	// The feed omitted the base row; the upsert must have inserted it.
	var n int
	if err := testPool.QueryRow(testCtx, `
		SELECT count(*) FROM fx_rates
		WHERE fx_date = '2026-11-15' AND currency = 'EUR' AND rate = 1.0`).Scan(&n); err != nil {
		t.Fatalf("count base row: %v", err)
	}
	if n != 1 {
		t.Errorf("implicit base 1.0 row for 2026-11-15: got %d, want 1", n)
	}
}

// TestBackfillCooldown: while a (from, to) pair is inside the cooldown
// after a (failed) attempt, further misses do NOT re-fetch — but a
// DIFFERENT pair still does.
func TestBackfillCooldown(t *testing.T) {
	reset(t)
	f := newFakeFrankfurter(t, []string{"EUR", "USD", "JPY"}, func(q url.Values) []rateRecord {
		return nil // the gap stays unfilled, so the retries keep missing
	})
	c := newClient(t, f)
	day := d("2026-11-15")

	if _, err := c.Rate(testCtx, day, "USD", "JPY"); err == nil {
		t.Fatal("want error while the gap is unfilled")
	}
	if n := len(f.rateRequests()); n != 1 {
		t.Fatalf("first miss must fetch once, got %d requests", n)
	}

	if _, err := c.Rate(testCtx, day, "USD", "JPY"); err == nil {
		t.Fatal("want error while the gap is unfilled")
	}
	if n := len(f.rateRequests()); n != 1 {
		t.Fatalf("cooldown must suppress the second fetch, got %d requests", n)
	}

	// A different pair is under a different cooldown key.
	if _, err := c.Rate(testCtx, day, "USD", "EUR"); err == nil {
		t.Fatal("want error while the gap is unfilled")
	}
	if n := len(f.rateRequests()); n != 2 {
		t.Fatalf("a different pair must fetch on its own, got %d requests", n)
	}
}

// TestRunDaily: the latest-day fetch (which spans two dates here: a
// lagging day without an explicit base row plus a day with one) is
// upserted; a second run with changed data updates in place.
func TestRunDaily(t *testing.T) {
	reset(t)
	usdRate := 1.09
	f := newFakeFrankfurter(t, nil, func(q url.Values) []rateRecord {
		return []rateRecord{
			{Date: "2026-11-16", Base: "EUR", Quote: "USD", Rate: usdRate}, // lagging day, base row omitted
			{Date: "2026-11-17", Base: "EUR", Quote: "EUR", Rate: 1.0},     // explicit base row
			{Date: "2026-11-17", Base: "EUR", Quote: "JPY", Rate: 162.0},
		}
	})
	c := newClient(t, f)

	if err := c.RunDaily(testCtx); err != nil {
		t.Fatalf("RunDaily: %v", err)
	}
	rows, err := storedRates(t)
	if err != nil {
		t.Fatal(err)
	}
	assertRows(t, rows, map[string]float64{
		"2026-11-16|EUR": 1.0, // implicit base row inserted by the upsert
		"2026-11-16|USD": 1.09,
		"2026-11-17|EUR": 1.0,
		"2026-11-17|JPY": 162.0,
	})

	// Second run: same keys, one changed rate — upsert, not duplicate.
	usdRate = 1.10
	if err := c.RunDaily(testCtx); err != nil {
		t.Fatalf("RunDaily (2nd): %v", err)
	}
	rows, err = storedRates(t)
	if err != nil {
		t.Fatal(err)
	}
	assertRows(t, rows, map[string]float64{
		"2026-11-16|EUR": 1.0,
		"2026-11-16|USD": 1.10,
		"2026-11-17|EUR": 1.0,
		"2026-11-17|JPY": 162.0,
	})
}

// storedRates returns "fx_date|currency" -> rate for everything stored.
func storedRates(t *testing.T) (map[string]float64, error) {
	t.Helper()
	rows, err := testPool.Query(testCtx, `SELECT fx_date, currency, rate FROM fx_rates`)
	if err != nil {
		return nil, fmt.Errorf("query fx_rates: %w", err)
	}
	defer rows.Close()
	out := map[string]float64{}
	for rows.Next() {
		var (
			day  time.Time
			code string
			rate float64
		)
		if err := rows.Scan(&day, &code, &rate); err != nil {
			return nil, fmt.Errorf("scan fx_rates: %w", err)
		}
		out[day.Format("2006-01-02")+"|"+code] = rate
	}
	return out, rows.Err()
}

func assertRows(t *testing.T, got map[string]float64, want map[string]float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("stored %d rows (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for k, w := range want {
		if g, ok := got[k]; !ok || math.Abs(g-w) > 1e-9 {
			t.Errorf("row %s = %v, want %v", k, got[k], w)
		}
	}
}

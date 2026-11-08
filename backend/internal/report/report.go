// Package report builds the personal-finance reports (plan step 8.2):
// spend by category, income, net, budget adherence, top merchants, and
// a comparison against the prior period. The package is database-free on
// purpose — Generate and the Render functions are pure over a Data
// snapshot, and Collector gathers that snapshot through the narrow
// TxSource/BudgetSource/CategorySource interfaces. Step 8.3 (the cron
// worker) will call Collector.Collect, Generate, and RenderText/
// RenderHTML and hand the two renderings to the step 8.1 mailer
// (multipart/alternative text + inline-CSS HTML).
//
// Money is canonical 2-decimal strings on every boundary and int64 at
// scale 100 internally; float64 never touches a money value.
package report

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Period is the report period kind.
type Period string

const (
	PeriodMonthly   Period = "monthly"
	PeriodQuarterly Period = "quarterly"
	PeriodAnnual    Period = "annual"
)

// maxWholeCents is the largest whole-dollar magnitude parse2dec accepts:
// cents must fit int64.
const maxWholeCents = (math.MaxInt64 - 99) / 100

// Range returns the calendar period containing asOf at date precision in
// UTC: from/to are 00:00 UTC of the period's first and last day, and
// label its display name ("October 2026", "Q4 2026", "2026"). Monthly is
// asOf's month, quarterly the calendar quarter (Jan-Mar / Apr-Jun /
// Jul-Sep / Oct-Dec), annual the calendar year. [from, to] matches the
// inclusive, date-precision window repo.Aggregate consumes. An unknown
// period panics — the three constants are the only values the app
// produces.
func Range(asOf time.Time, p Period) (from, to time.Time, label string) {
	u := asOf.UTC()
	switch p {
	case PeriodMonthly:
		from = time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
		to = time.Date(u.Year(), u.Month()+1, 0, 0, 0, 0, 0, time.UTC)
		return from, to, from.Format("January 2006")
	case PeriodQuarterly:
		q := int(u.Month()-1)/3 + 1
		first := time.Month((q-1)*3 + 1)
		from = time.Date(u.Year(), first, 1, 0, 0, 0, 0, time.UTC)
		to = time.Date(u.Year(), first+3, 0, 0, 0, 0, 0, time.UTC)
		return from, to, fmt.Sprintf("Q%d %d", q, u.Year())
	case PeriodAnnual:
		from = time.Date(u.Year(), time.January, 1, 0, 0, 0, 0, time.UTC)
		to = time.Date(u.Year(), time.December, 31, 0, 0, 0, 0, time.UTC)
		return from, to, strconv.Itoa(u.Year())
	}
	panic(fmt.Sprintf("report: unknown period %q", p))
}

// priorAsOf shifts asOf back one calendar period of kind p (the input
// whose Range is the prior period). An unknown period panics, as Range.
func priorAsOf(asOf time.Time, p Period) time.Time {
	u := asOf.UTC()
	switch p {
	case PeriodMonthly:
		return u.AddDate(0, -1, 0)
	case PeriodQuarterly:
		return u.AddDate(0, -3, 0)
	case PeriodAnnual:
		return u.AddDate(-1, 0, 0)
	}
	panic(fmt.Sprintf("report: unknown period %q", p))
}

// priorLabel is the label of the period immediately before the one
// containing asOf (via Range, the pinned prior-period semantics).
func priorLabel(asOf time.Time, p Period) string {
	_, _, label := Range(priorAsOf(asOf, p), p)
	return label
}

// dateStr renders a date as "2006-01-02" (UTC).
func dateStr(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

// CategorySpend is one (category, period) row of spend and income. Spend
// and Income are non-negative magnitudes (spend = the absolute sum of
// the negative amounts, income = the sum of the positive ones); Net is
// income minus spend (negative when the spend wins). All three are
// canonical 2-decimal strings. CategoryID "" = the uncategorized group.
//
// This mirrors repo.CategorySpend without importing internal/repo: the
// package must stay importable by step 8.3 without a database, and a
// small adapter there copies the repo type in.
type CategorySpend struct {
	CategoryID string
	Spend      string
	Income     string
	Net        string
}

// MerchantCount is one row of the top-merchants table: Name is the
// transaction description as-is (the app has no separate merchant
// column), Count its spending-row count, Total the canonical 2-decimal
// magnitude of the summed spend (e.g. "42.13").
type MerchantCount struct {
	Name  string
	Count int
	Total string
}

// CategoryLine is one row of the per-category table. BudgetTarget is the
// canonical target string, or "" when the category has no target;
// WithinBudget is nil in that case, otherwise it points to
// (Spend <= BudgetTarget).
type CategoryLine struct {
	CategoryID   string
	Name         string
	Spend        string
	Income       string
	Net          string
	BudgetTarget string
	WithinBudget *bool
}

// PeriodTotals is the income/spend/net summary of one period (the
// prior period's box in Report).
type PeriodTotals struct {
	Label  string
	Income string
	Spend  string
	Net    string
}

// Report is one generated report. ByCategory keeps the caller's order
// (Generate preserves it); TopMerchants is passed through as given;
// Prior is nil when the caller has no prior-period data.
type Report struct {
	Label        string
	From, To     time.Time
	Income       string
	Spend        string
	Net          string
	ByCategory   []CategoryLine
	TopMerchants []MerchantCount
	Prior        *PeriodTotals
}

// Data is the snapshot Generate works from. CategorySpend and
// PriorCategorySpend come from a TransactionStore.Aggregate pair (the
// caller sorts; Generate preserves the order); Merchants is the
// caller's top-N (TopMerchants computes it from rows when wanted);
// BudgetTargets maps category id -> canonical 2-decimal target (absent
// = no target); CategoryNames maps category id -> name.
type Data struct {
	Period             Period
	AsOf               time.Time
	CategorySpend      []CategorySpend
	Merchants          []MerchantCount
	PriorCategorySpend []CategorySpend
	BudgetTargets      map[string]string
	CategoryNames      map[string]string
}

// Row is one transaction row for TopMerchants: Description is the
// decrypted memo and Amount the canonical 2-decimal signed amount
// (negative = spend).
type Row struct {
	Description string
	Amount      string
}

// Generate builds a Report from d. It is pure (no ctx, no DB, no
// now()): every time-dependent input arrives via d. All money math is
// int64 cents, and every money field in the output is re-canonicalized
// via fmt2dec, so the Report speaks canonical 2-decimal strings
// regardless of the input spelling. A non-canonical money string in d
// panics (the pinned signature has no error return): every producer of
// Data re-canonicalizes its strings (the repo re-canonicalizes on scan
// and the collector passes them through), so a parse failure means a
// caller bug — it must fail loudly, never zero a money field.
func Generate(d Data) Report {
	from, to, label := Range(d.AsOf, d.Period)

	income := int64(0)
	spend := int64(0)
	lines := make([]CategoryLine, 0, len(d.CategorySpend))
	for _, g := range d.CategorySpend {
		s := money(g.Spend, "CategorySpend.Spend")
		i := money(g.Income, "CategorySpend.Income")
		income += i
		spend += s
		line := CategoryLine{
			CategoryID: g.CategoryID,
			Name:       d.CategoryNames[g.CategoryID],
			Spend:      fmt2dec(s),
			Income:     fmt2dec(i),
			Net:        fmt2dec(i - s),
		}
		if t, ok := d.BudgetTargets[g.CategoryID]; ok {
			tc := money(t, "BudgetTargets")
			line.BudgetTarget = fmt2dec(tc)
			w := s <= tc
			line.WithinBudget = &w
		}
		lines = append(lines, line)
	}

	r := Report{
		Label:        label,
		From:         from,
		To:           to,
		Income:       fmt2dec(income),
		Spend:        fmt2dec(spend),
		Net:          fmt2dec(income - spend),
		ByCategory:   lines,
		TopMerchants: d.Merchants,
	}

	// nil = no prior data at all (no comparison section); an empty
	// non-nil slice renders the prior box with zero totals.
	if d.PriorCategorySpend != nil {
		pIn, pSp := periodTotals(d.PriorCategorySpend)
		r.Prior = &PeriodTotals{
			Label:  priorLabel(d.AsOf, d.Period),
			Income: fmt2dec(pIn),
			Spend:  fmt2dec(pSp),
			Net:    fmt2dec(pIn - pSp),
		}
	}
	return r
}

// periodTotals sums the income and spend magnitudes of rows (the same
// totals rules as Generate's current period).
func periodTotals(rows []CategorySpend) (income, spend int64) {
	for _, g := range rows {
		income += money(g.Income, "CategorySpend.Income")
		spend += money(g.Spend, "CategorySpend.Spend")
	}
	return
}

// TopMerchants counts the spending rows of rows (Amount < 0) grouped by
// their Description as-is (no separate merchant column exists in this
// app — DESIGN.md "top-10 merchants by frequency (personal scale)"),
// sums the magnitudes, and returns at most n merchants ordered by count
// DESC, then Name ASC. Rows with an empty description are skipped.
// n <= 0 means 10 (the pinned default; callers pass 10).
func TopMerchants(rows []Row, n int) []MerchantCount {
	if n <= 0 {
		n = 10
	}
	type agg struct {
		count int
		total int64
	}
	by := make(map[string]*agg)
	for _, r := range rows {
		if r.Description == "" {
			continue
		}
		amt := money(r.Amount, "Row.Amount")
		if amt >= 0 {
			continue // spending rows only
		}
		a := by[r.Description]
		if a == nil {
			a = &agg{}
			by[r.Description] = a
		}
		a.count++
		a.total += -amt
	}
	out := make([]MerchantCount, 0, len(by))
	for name, a := range by {
		out = append(out, MerchantCount{Name: name, Count: a.count, Total: fmt2dec(a.total)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// parse2dec parses a decimal amount into int64 units of 1/100 (scale
// 100). Accepted: optional sign, digits only otherwise, at most 2
// fractional digits ("5" and "5.5" are padded). It rejects empty input,
// non-digits, more than 2 fractional digits, and values whose cents do
// not fit int64.
func parse2dec(s string) (int64, error) {
	rest := s
	neg := false
	if rest != "" && (rest[0] == '-' || rest[0] == '+') {
		neg = rest[0] == '-'
		rest = rest[1:]
	}
	intPart, fracPart := rest, ""
	if i := strings.IndexByte(rest, '.'); i >= 0 {
		intPart, fracPart = rest[:i], rest[i+1:]
	}
	if intPart == "" && fracPart == "" {
		return 0, fmt.Errorf("report: amount %q is empty", s)
	}
	if !isDigits(intPart) || !isDigits(fracPart) {
		return 0, fmt.Errorf("report: amount %q is not a valid decimal", s)
	}
	if len(fracPart) > 2 {
		return 0, fmt.Errorf("report: amount %q has more than 2 decimal places", s)
	}
	cents := int64(0)
	if intPart != "" {
		whole, err := strconv.ParseInt(intPart, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("report: amount %q does not fit", s)
		}
		if whole > maxWholeCents {
			return 0, fmt.Errorf("report: amount %q is out of range", s)
		}
		cents = whole * 100
	}
	switch len(fracPart) {
	case 1:
		cents += int64(fracPart[0]-'0') * 10
	case 2:
		cents += int64(fracPart[0]-'0')*10 + int64(fracPart[1]-'0')
	}
	if neg {
		cents = -cents
	}
	return cents, nil
}

// fmt2dec renders int64 cents in canonical 2-decimal form ("12.30",
// "-5.00", "0.00") — the inverse of parse2dec for every value parse2dec
// accepts. math.MinInt64 is unreachable through parse2dec, so the negate
// below cannot overflow.
func fmt2dec(cents int64) string {
	neg := cents < 0
	if neg {
		cents = -cents
	}
	frac := cents % 100
	out := strconv.FormatInt(cents/100, 10) + "." + strconv.FormatInt(frac, 10)
	if frac < 10 {
		out += "0"
	}
	if neg {
		out = "-" + out
	}
	return out
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// money parses one money string into cents, panicking on a
// non-canonical value. See Generate for why this is a panic instead of
// an error (the pinned signatures carry no error return).
func money(s, field string) int64 {
	cents, err := parse2dec(s)
	if err != nil {
		panic(fmt.Sprintf("report: %s %q: %s", field, s, strings.TrimPrefix(err.Error(), "report: ")))
	}
	return cents
}

// centsOf parses s; it must only be called on a string already validated
// by checkReport (or on Generate's output, which always passes).
func centsOf(s string) int64 {
	cents, err := parse2dec(s)
	if err != nil {
		panic(fmt.Sprintf("report: money %q: %v", s, err))
	}
	return cents
}

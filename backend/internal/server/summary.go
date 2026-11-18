// GET /api/v1/summary (plan step 3.2): the per-period rollup the
// frontend dashboard renders. It aggregates the caller's transactions
// for the period (repo Aggregate: per-category spend / income / net),
// merges the active budget targets of the period (the repo's
// ActiveTargets, narrowed in SQL to the period's active rows, is
// decrypted in-app and summed per category — more than one active
// budget may name the same category), and joins category names. Groups are the
// UNION of the aggregation and the target keys, so a category with a
// target but no transactions still shows up (0.00s). The uncategorized
// group carries null category_id and category_name; a target key no
// category row answers (dangling) carries a null category_name too.
//
// Optional parameters (6.2): trend=N (integer 1..24) appends a "trend"
// array of the last N period windows, oldest first, each carrying the
// window's inclusive from/to bounds and spend / income / net; without
// the parameter the response carries no trend key at all. A repeatable
// account_id=UUID restricts the aggregation to those accounts: an
// empty value is skipped, a malformed value is a 400, an unknown id
// simply contributes nothing (200 with the unfiltered shape). 6.3:
// each trend point also carries its window's per-category breakdown
// (the point's groups — summaryGroup's shape without the target,
// sorted like the top-level groups), so a caller tracking a budget
// joins a point's groups against its own targets document.
package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"finance-tracker/internal/repo"
)

// summaryOut is the response body. From/To are the inclusive
// "YYYY-MM-DD" bounds of the computed period; Totals cover the
// transaction groups only (target-only groups contribute nothing).
// Trend is nil (and the key omitted) unless trend=N was requested.
type summaryOut struct {
	Period string         `json:"period"`
	From   string         `json:"from"`
	To     string         `json:"to"`
	Groups []summaryGroup `json:"groups"`
	Totals summaryTotals  `json:"totals"`
	Trend  []trendPoint   `json:"trend,omitempty"`
}

// trendPoint is one window of summaryOut.Trend: the inclusive
// "YYYY-MM-DD" bounds of a past period window, its totals in canonical
// 2-decimal strings (computed like Totals), and its per-category
// breakdown (Groups — see trendGroup). Groups has no omitempty, like
// the top-level groups: an empty window marshals as [], never null.
type trendPoint struct {
	From   string       `json:"from"`
	To     string       `json:"to"`
	Spend  string       `json:"spend"`
	Income string       `json:"income"`
	Net    string       `json:"net"`
	Groups []trendGroup `json:"groups"`
}

// trendGroup is a trend point's per-category breakdown — the same shape as
// summaryGroup WITHOUT Target: a budget's activeness is a present-tense flag
// with no dates, so no past window has well-defined targets; callers that
// track a budget join this against their own targets document.
type trendGroup struct {
	CategoryID   *string `json:"category_id"`
	CategoryName *string `json:"category_name"`
	Spend        string  `json:"spend"`
	Income       string  `json:"income"`
	Net          string  `json:"net"`
}

type summaryGroup struct {
	CategoryID   *string `json:"category_id"`
	CategoryName *string `json:"category_name"`
	Spend        string  `json:"spend"`
	Income       string  `json:"income"`
	Net          string  `json:"net"`
	Target       *string `json:"target"`
}

type summaryTotals struct {
	Spend  string `json:"spend"`
	Income string `json:"income"`
	Net    string `json:"net"`
}

// periodOK loops repo.BudgetPeriods — the one shared period vocabulary
// (0003 budgets.period, users.default_period, the report periods).
func periodOK(p string) bool {
	for _, v := range repo.BudgetPeriods {
		if v == p {
			return true
		}
	}
	return false
}

// periodBounds computes the inclusive, date-precision calendar range of
// the period containing now, in UTC. Month = the calendar month;
// quarter = the 3-month block starting at month 1 + 3*((m-1)/3);
// year = the calendar year.
func periodBounds(period string, now time.Time) (time.Time, time.Time) {
	now = now.UTC()
	y, m, _ := now.Date()
	switch period {
	case "quarter":
		start := time.Month(1 + 3*((int(m)-1)/3))
		from := time.Date(y, start, 1, 0, 0, 0, 0, time.UTC)
		return from, from.AddDate(0, 3, -1)
	case "year":
		from := time.Date(y, time.January, 1, 0, 0, 0, 0, time.UTC)
		return from, from.AddDate(1, 0, -1) // Dec 31, inclusive
	default: // month (the only remaining valid value)
		from := time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
		return from, from.AddDate(0, 1, -1)
	}
}

// trendShift returns now pulled back by k periods of the given kind,
// so that periodBounds(period, trendShift(period, now, k)) is the
// k-th window before the current one (k = 0 is the current window
// itself). Shifting by whole periods keeps the window shape: month
// windows are calendar months, quarter windows stay inside the same
// quarter shape one block back, year windows stay calendar years.
func trendShift(period string, now time.Time, k int) time.Time {
	switch period {
	case "quarter":
		return now.AddDate(0, -3*k, 0)
	case "year":
		return now.AddDate(-k, 0, 0)
	default: // month
		return now.AddDate(0, -k, 0)
	}
}

// parseCents parses a canonical 2-decimal amount string to int64 cents
// exactly (no float involved). Canonical strings always carry two
// fractional digits; the pad/trim is belt-and-braces.
func parseCents(s string) (int64, error) {
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	intPart, fracPart := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, fracPart = s[:i], s[i+1:]
	}
	for len(fracPart) < 2 {
		fracPart += "0"
	}
	v, err := strconv.ParseInt(intPart+fracPart[:2], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("summary: unparseable amount %q: %w", s, err)
	}
	if neg {
		v = -v
	}
	return v, nil
}

// formatCents renders int64 cents as a canonical 2-decimal string.
// int64 cents would only overflow beyond ~9.2e14 USD — far past the
// numeric(14,2) ceiling the DB enforces on any single amount.
func formatCents(c int64) string {
	sign := ""
	if c < 0 {
		sign = "-"
		c = -c
	}
	return fmt.Sprintf("%s%d.%02d", sign, c/100, c%100)
}

// groupCents parses one aggregation group's three canonical amount
// strings into int64 cents (the shared parse behind the top-level
// Totals and each trend point).
func groupCents(g repo.CategorySpend) (spend, income, net int64, err error) {
	if spend, err = parseCents(g.Spend); err != nil {
		return 0, 0, 0, err
	}
	if income, err = parseCents(g.Income); err != nil {
		return 0, 0, 0, err
	}
	if net, err = parseCents(g.Net); err != nil {
		return 0, 0, 0, err
	}
	return spend, income, net, nil
}

// groupKey is the (category id, category name) pair a group's order is
// keyed on. The top-level summaryGroup list and each trend point's
// trendGroup list both sort through it, so the two listings always
// agree.
type groupKey struct {
	id   *string
	name *string
}

// sortGroups orders groups by category name asc with null names last;
// ties break by category id asc (a nil id sorts after a non-nil one,
// so the uncategorized null id is last among the unnamed).
func sortGroups[T any](groups []T, key func(T) groupKey) {
	sort.Slice(groups, func(i, j int) bool {
		ki, kj := key(groups[i]), key(groups[j])
		ni, nj := ki.name == nil, kj.name == nil
		if ni != nj {
			return !ni
		}
		if ni {
			iID, jID := ki.id == nil, kj.id == nil
			if iID != jID {
				return !iID
			}
			return !iID || *ki.id < *kj.id
		}
		if *ki.name != *kj.name {
			return *ki.name < *kj.name
		}
		iID, jID := ki.id == nil, kj.id == nil
		if iID != jID {
			return !iID
		}
		return !iID || *ki.id < *kj.id
	})
}

// summary serves GET /api/v1/summary?period=month|quarter|year&group_by=category
// with the optional 6.2 parameters trend=1..24 and repeatable
// account_id=UUID (see the package doc).
func (h *handler) summary(w http.ResponseWriter, r *http.Request) {
	if h.transactions == nil || h.budgets == nil || h.categories == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	period := q.Get("period")
	if period == "" {
		period = "month"
	}
	if !periodOK(period) {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest,
			"period must be one of "+strings.Join(repo.BudgetPeriods, ", "))
		return
	}
	if gb := q.Get("group_by"); gb != "" && gb != "category" {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "group_by must be category")
		return
	}
	trend := 0
	if s := q.Get("trend"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "trend must be an integer")
			return
		}
		if n < 1 || n > 24 {
			writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "trend must be between 1 and 24")
			return
		}
		trend = n
	}
	// account_id is repeatable, exactly like the transactions list
	// filter: empty values are skipped, malformed values are a 400,
	// unknown ids stay a 200 (they just match no rows).
	var accountIDs []string
	for _, a := range q["account_id"] {
		if a == "" {
			continue
		}
		if !uuidShapeOK(a) {
			writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "account_id must be a valid UUID")
			return
		}
		accountIDs = append(accountIDs, a)
	}

	// One clock read, shared by the top-level window and every trend
	// shift: the k=0 trend point must be exactly the top-level from/to.
	now := time.Now()
	from, to := periodBounds(period, now)
	groups, err := h.transactions.Aggregate(r.Context(), c.row.ID, from, to, accountIDs)
	if err != nil {
		h.transactionError(w, "aggregate", err)
		return
	}
	budgets, err := h.budgets.ActiveTargets(r.Context(), c.row.ID, period, c.dek)
	if err != nil {
		slog.Error("budget operation failed", "op", "list", "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "budget operation failed")
		return
	}
	cats, err := h.categories.List(r.Context(), c.row.ID, c.dek)
	if err != nil {
		slog.Error("category operation failed", "op", "list", "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "category operation failed")
		return
	}

	// Active budgets of the period: sum their targets per category in
	// float64. validateTargetsShape does not cap decimal places, so
	// sub-cent targets are possible — render at 2 decimals, don't
	// pretend exactness.
	targets := make(map[string]float64)
	for _, b := range budgets {
		var m map[string]json.Number
		if err := json.Unmarshal(b.Targets, &m); err != nil {
			slog.Error("budget targets could not be decoded", "budget", b.ID, "error", err)
			writeAPIError(w, http.StatusInternalServerError, codeInternal, "budget targets could not be decoded")
			return
		}
		for id, n := range m {
			f, err := n.Float64()
			if err != nil {
				slog.Error("budget target value could not be decoded", "budget", b.ID, "category", id, "error", err)
				writeAPIError(w, http.StatusInternalServerError, codeInternal, "budget targets could not be decoded")
				return
			}
			targets[id] += f
		}
	}
	names := make(map[string]string, len(cats))
	for _, cat := range cats {
		names[cat.ID] = cat.Name
	}

	// Union of the aggregation groups and the target keys.
	type acc struct {
		spend, income, net string
		hasTx              bool
		target             float64
		hasTarget          bool
	}
	byCat := make(map[string]*acc)
	get := func(id string) *acc {
		a, ok := byCat[id]
		if !ok {
			a = &acc{}
			byCat[id] = a
		}
		return a
	}
	for _, g := range groups {
		a := get(g.CategoryID)
		a.spend, a.income, a.net = g.Spend, g.Income, g.Net
		a.hasTx = true
	}
	for id, f := range targets {
		a := get(id)
		a.target, a.hasTarget = f, true
	}

	out := make([]summaryGroup, 0, len(byCat))
	for id, a := range byCat {
		sg := summaryGroup{}
		if id != "" {
			sg.CategoryID = &id
		}
		if name, ok := names[id]; ok {
			sg.CategoryName = &name
		}
		if a.hasTx {
			sg.Spend, sg.Income, sg.Net = a.spend, a.income, a.net
		} else {
			sg.Spend, sg.Income, sg.Net = "0.00", "0.00", "0.00"
		}
		if a.hasTarget {
			s := strconv.FormatFloat(a.target, 'f', 2, 64)
			sg.Target = &s
		}
		out = append(out, sg)
	}
	// Sort: category name asc, nulls last; ties break by category id
	// asc (the uncategorized null id sorts last among the unnamed) —
	// the shared comparator the trend points' groups use too.
	sortGroups(out, func(g summaryGroup) groupKey {
		return groupKey{id: g.CategoryID, name: g.CategoryName}
	})

	// Totals in int64 cents over the transaction groups only.
	var tSpend, tIncome, tNet int64
	for _, g := range groups {
		sp, inc, net, err := groupCents(g)
		if err != nil {
			slog.Error("summary totals could not be computed", "error", err)
			writeAPIError(w, http.StatusInternalServerError, codeInternal, "summary totals could not be computed")
			return
		}
		tSpend, tIncome, tNet = tSpend+sp, tIncome+inc, tNet+net
	}

	// Trend: the last N windows, oldest first (k = N-1 down to 0).
	// One bucketed TrendAggregate query over the whole trend span
	// replaces the per-window Aggregate calls; the buckets are keyed
	// by their UTC-midnight start, which is exactly each window's
	// periodBounds from date. Same account filter as the top-level
	// window; totals computed the same way. Each point also carries
	// its window's per-category breakdown (6.3): the same bucket rows
	// the totals were summed from, names joined from the shared names
	// map and sorted with the shared group comparator. A nil Trend
	// stays nil and drops out of the JSON.
	var trendPts []trendPoint
	if trend > 0 {
		wf0, _ := periodBounds(period, trendShift(period, now, trend-1))
		_, wtN := periodBounds(period, now)
		buckets, err := h.transactions.TrendAggregate(r.Context(), c.row.ID, period, wf0, wtN, accountIDs)
		if err != nil {
			h.transactionError(w, "aggregate", err)
			return
		}
		byWindow := make(map[string][]repo.CategorySpend, len(buckets))
		for _, b := range buckets {
			key := b.Bucket.Format("2006-01-02")
			byWindow[key] = append(byWindow[key], b.Groups...)
		}
		trendPts = make([]trendPoint, 0, trend)
		for k := trend - 1; k >= 0; k-- {
			tf, tt := periodBounds(period, trendShift(period, now, k))
			tg := byWindow[tf.Format("2006-01-02")]
			var ptSpend, ptIncome, ptNet int64
			tgs := make([]trendGroup, 0, len(tg))
			for _, g := range tg {
				sp, inc, net, err := groupCents(g)
				if err != nil {
					slog.Error("summary trend could not be computed", "error", err)
					writeAPIError(w, http.StatusInternalServerError, codeInternal, "summary trend could not be computed")
					return
				}
				ptSpend, ptIncome, ptNet = ptSpend+sp, ptIncome+inc, ptNet+net
				grp := trendGroup{Spend: g.Spend, Income: g.Income, Net: g.Net}
				if g.CategoryID != "" {
					id := g.CategoryID
					grp.CategoryID = &id
				}
				if name, ok := names[g.CategoryID]; ok {
					n := name
					grp.CategoryName = &n
				}
				tgs = append(tgs, grp)
			}
			sortGroups(tgs, func(g trendGroup) groupKey {
				return groupKey{id: g.CategoryID, name: g.CategoryName}
			})
			trendPts = append(trendPts, trendPoint{
				From:   dateOnly(tf),
				To:     dateOnly(tt),
				Spend:  formatCents(ptSpend),
				Income: formatCents(ptIncome),
				Net:    formatCents(ptNet),
				Groups: tgs,
			})
		}
	}

	resp := summaryOut{
		Period: period,
		From:   dateOnly(from),
		To:     dateOnly(to),
		Groups: out,
		Totals: summaryTotals{
			Spend:  formatCents(tSpend),
			Income: formatCents(tIncome),
			Net:    formatCents(tNet),
		},
		Trend: trendPts,
	}
	writeJSON(w, http.StatusOK, resp)
}

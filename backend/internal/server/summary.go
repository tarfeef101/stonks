// GET /api/v1/summary (plan step 3.2): the per-period rollup the
// frontend dashboard renders. It aggregates the caller's transactions
// for the period (repo Aggregate: per-category spend / income / net),
// merges the active budget targets of the period (decrypted in-app by
// the repo List, summed per category — more than one active budget may
// name the same category), and joins category names. Groups are the
// UNION of the aggregation and the target keys, so a category with a
// target but no transactions still shows up (0.00s). The uncategorized
// group carries null category_id and category_name; a target key no
// category row answers (dangling) carries a null category_name too.
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
type summaryOut struct {
	Period string         `json:"period"`
	From   string         `json:"from"`
	To     string         `json:"to"`
	Groups []summaryGroup `json:"groups"`
	Totals summaryTotals  `json:"totals"`
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

// summary serves GET /api/v1/summary?period=month|quarter|year&group_by=category.
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

	from, to := periodBounds(period, time.Now())
	groups, err := h.transactions.Aggregate(r.Context(), c.row.ID, from, to)
	if err != nil {
		h.transactionError(w, "aggregate", err)
		return
	}
	budgets, err := h.budgets.List(r.Context(), c.row.ID, c.dek)
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
		if !b.Active || b.Period != period {
			continue
		}
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
	// asc (the uncategorized null id sorts last among the unnamed).
	sort.Slice(out, func(i, j int) bool {
		ni, nj := out[i].CategoryName == nil, out[j].CategoryName == nil
		if ni != nj {
			return !ni
		}
		if ni {
			iID, jID := out[i].CategoryID == nil, out[j].CategoryID == nil
			if iID != jID {
				return !iID
			}
			return !iID || *out[i].CategoryID < *out[j].CategoryID
		}
		if *out[i].CategoryName != *out[j].CategoryName {
			return *out[i].CategoryName < *out[j].CategoryName
		}
		iID, jID := out[i].CategoryID == nil, out[j].CategoryID == nil
		if iID != jID {
			return !iID
		}
		return !iID || *out[i].CategoryID < *out[j].CategoryID
	})

	// Totals in int64 cents over the transaction groups only.
	var tSpend, tIncome, tNet int64
	for _, g := range groups {
		sp, err := parseCents(g.Spend)
		if err != nil {
			slog.Error("summary totals could not be computed", "error", err)
			writeAPIError(w, http.StatusInternalServerError, codeInternal, "summary totals could not be computed")
			return
		}
		inc, err := parseCents(g.Income)
		if err != nil {
			slog.Error("summary totals could not be computed", "error", err)
			writeAPIError(w, http.StatusInternalServerError, codeInternal, "summary totals could not be computed")
			return
		}
		net, err := parseCents(g.Net)
		if err != nil {
			slog.Error("summary totals could not be computed", "error", err)
			writeAPIError(w, http.StatusInternalServerError, codeInternal, "summary totals could not be computed")
			return
		}
		tSpend, tIncome, tNet = tSpend+sp, tIncome+inc, tNet+net
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
	}
	writeJSON(w, http.StatusOK, resp)
}

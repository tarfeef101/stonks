package report

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// txPageSize is the page size the collector walks Tx.List with: the
// repo's hard cap, so every page except possibly the last is full and a
// short page terminates the walk. ListFilter carries no limit of its
// own — the source adapter must page at this size (8.3's adapter over
// repo.TransactionStore pins List's limit to 100; its default is 50).
const txPageSize = 100

// ListFilter selects one page of transaction rows for TxSource.List:
// [From, To] inclusive at date precision (the same windows Range
// produces, passed through unchanged), Offset the zero-based row
// offset. Ordering is the source's (repo: date_posted DESC, id DESC);
// the collector advances Offset by each page's length until a short
// page.
type ListFilter struct {
	From   time.Time
	To     time.Time
	Offset int
}

// TxSource is the transaction half of the collector's data plane. The
// collector defines its own narrow interface (it does not reuse
// internal/server or internal/repo types) so step 8.3 can implement it
// with a small adapter over *repo.TransactionStore. CategorySpend and
// Row are the report-local structs; the adapter copies the repo types.
// The per-category math happens here (categorySpend), not in the
// source: both periods share one code path, and the optional per-row
// FX conversion runs before the sums (see FxSource).
type TxSource interface {
	List(ctx context.Context, userID string, f ListFilter, dek []byte) ([]Row, error)
}

// FxSource resolves one conversion factor per row: Factor returns the
// factor that turns an amount in from (ISO-4217) into to as of date
// (the row's date_posted). converted is false when the source cannot
// resolve the pair (no rate on or before date for it): the collector
// keeps the row's original amount and does NOT fail the collection.
// Any other failure is an error and aborts the collection (wrapped as
// "report: fx rate: %w").
//
// This package must not import internal/fx: the worker's adapter
// implements FxSource over the fx.Client (TECH-DECISIONS.md D-027).
type FxSource interface {
	Factor(ctx context.Context, date time.Time, from, to string) (factor float64, converted bool, err error)
}

// Budget is one budget row as the collector consumes it; the adapter
// decrypts Targets (a JSON object keyed by category id -> canonical
// 2-decimal string, exactly repo.Budget.Targets after decryption).
//
// Order contract: BudgetSource.List returns rows in a stable,
// deterministic order (repo.BudgetStore.List orders by created_at, id),
// and Collect takes the FIRST active row whose Period matches. (The
// handoff's "lexicographically first ID" is not expressible here: the
// pinned struct has no ID field — see REPORT.md, PINCORR.)
type Budget struct {
	Period  string
	Active  bool
	Targets map[string]string
}

// BudgetSource lists the user's budgets; see Budget for the order
// contract Collect relies on.
type BudgetSource interface {
	List(ctx context.Context, userID string, dek []byte) ([]Budget, error)
}

// Name is one category row as the collector consumes it.
type Name struct {
	ID   string
	Name string
}

// CategorySource lists the user's categories (the adapter copies
// repo.Category rows; dek decrypts names).
type CategorySource interface {
	List(ctx context.Context, userID string, dek []byte) ([]Name, error)
}

// Collector gathers a Data snapshot from the narrow sources. All
// time-dependent input is explicit (asOf); Collect never calls now().
// Fx nil = no conversion: rows stay in their account's own currency.
type Collector struct {
	Tx       TxSource
	Budget   BudgetSource
	Category CategorySource
	Fx       FxSource
}

// Collect gathers the Data snapshot for period p as of asOf without FX
// conversion (each row stays in its account's own currency). It
// delegates to CollectFor with an empty base, which is numerically the
// pre-change behavior: per-category sums over the walked rows plus
// current-period merchants, the first active period-matching budget's
// targets (empty map when none — not an error), and category names. An
// empty prior window normalizes to a nil slice, so Generate renders no
// comparison section for a user's first period. dek is passed through
// to the sources for field decryption.
func (c *Collector) Collect(ctx context.Context, userID string, p Period, asOf time.Time, dek []byte) (Data, error) {
	return c.CollectFor(ctx, userID, p, asOf, dek, "")
}

// CollectFor gathers the Data snapshot for period p as of asOf, with
// optional FX conversion: when base (ISO-4217) is non-empty and Fx is
// set, each row whose Currency is non-empty and differs from base is
// converted into base at its Date (an unresolvable pair keeps the
// original amount — see FxSource). An empty base means no conversion
// even when Fx is set.
//
// Both windows are walked (current first, then prior) and their
// CategorySpend computed by the same categorySpend; an empty prior
// normalizes to a nil slice. Merchants is the current period only,
// always from the raw (unconverted) row amounts. An unknown period is
// an error before any source is touched. dek is passed through to the
// sources for field decryption.
func (c *Collector) CollectFor(ctx context.Context, userID string, p Period, asOf time.Time, dek []byte, base string) (Data, error) {
	// Validate p before touching the sources: an unknown period is a
	// caller bug surfaced as the documented error rather than Range's
	// panic.
	if _, ok := budgetPeriod(p); !ok {
		return Data{}, fmt.Errorf("report: unknown period %q", p)
	}
	from, to, _ := Range(asOf, p)
	priorFrom, priorTo, _ := Range(priorAsOf(asOf, p), p)

	currentRows, err := c.listAll(ctx, userID, from, to, dek)
	if err != nil {
		return Data{}, err
	}
	priorRows, err := c.listAll(ctx, userID, priorFrom, priorTo, dek)
	if err != nil {
		return Data{}, err
	}

	current, err := categorySpend(ctx, currentRows, base, c.Fx)
	if err != nil {
		return Data{}, err
	}
	prior, err := categorySpend(ctx, priorRows, base, c.Fx)
	if err != nil {
		return Data{}, err
	}
	if len(prior) == 0 {
		prior = nil
	}

	targets, err := c.budgetTargets(ctx, userID, p, dek)
	if err != nil {
		return Data{}, err
	}

	cats, err := c.Category.List(ctx, userID, dek)
	if err != nil {
		return Data{}, fmt.Errorf("report: list categories: %w", err)
	}
	names := make(map[string]string, len(cats))
	for _, cat := range cats {
		names[cat.ID] = cat.Name
	}

	return Data{
		Period:             p,
		AsOf:               asOf,
		CategorySpend:      current,
		Merchants:          TopMerchants(currentRows, 10),
		PriorCategorySpend: prior,
		BudgetTargets:      targets,
		CategoryNames:      names,
	}, nil
}

// listAll walks Tx.List in pages of txPageSize over [from, to] until a
// short page and returns every row (Offset advances by each page's
// length; an empty first page yields an empty slice).
func (c *Collector) listAll(ctx context.Context, userID string, from, to time.Time, dek []byte) ([]Row, error) {
	rows := []Row{}
	offset := 0
	for {
		page, err := c.Tx.List(ctx, userID, ListFilter{From: from, To: to, Offset: offset}, dek)
		if err != nil {
			return nil, fmt.Errorf("report: list transactions: %w", err)
		}
		rows = append(rows, page...)
		if len(page) < txPageSize {
			return rows, nil
		}
		offset += len(page)
	}
}

// categorySpend sums one window's rows into per-category spend, income
// and net. A row is converted into base only when base != "", fx !=
// nil, and the row's Currency is non-empty and != base: the converted
// value is round(originalCents × factor) (half away from zero) and the
// sign is the original cents' (a factor is always positive). An
// unresolvable pair (converted == false) keeps the original amount.
// The output is sorted by CategoryID ("" = uncategorized first),
// matching the old SQL aggregate's ORDER BY category_id NULLS FIRST. A
// non-canonical row Amount fails the collection (the adapter
// re-canonicalizes on scan, so a parse failure is a caller bug). Empty
// input yields an empty non-nil slice.
func categorySpend(ctx context.Context, rows []Row, base string, fx FxSource) ([]CategorySpend, error) {
	type acc struct{ spend, income int64 }
	by := make(map[string]*acc)
	for _, r := range rows {
		cents, err := parse2dec(r.Amount)
		if err != nil {
			return nil, fmt.Errorf("report: row %q: %s", r.Amount, strings.TrimPrefix(err.Error(), "report: "))
		}
		if base != "" && fx != nil && r.Currency != "" && r.Currency != base {
			factor, converted, err := fx.Factor(ctx, r.Date, r.Currency, base)
			if err != nil {
				return nil, fmt.Errorf("report: fx rate: %w", err)
			}
			if converted {
				cents = int64(math.Round(float64(cents) * factor))
			}
		}
		a := by[r.CategoryID]
		if a == nil {
			a = &acc{}
			by[r.CategoryID] = a
		}
		if cents < 0 {
			a.spend += -cents
		} else {
			a.income += cents
		}
	}
	out := make([]CategorySpend, 0, len(by))
	for id, a := range by {
		out = append(out, CategorySpend{
			CategoryID: id,
			Spend:      fmt2dec(a.spend),
			Income:     fmt2dec(a.income),
			Net:        fmt2dec(a.income - a.spend),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CategoryID < out[j].CategoryID })
	return out, nil
}

// budgetTargets returns the targets of the first active budget whose
// period matches p (see Budget for the order contract), copied into a
// fresh map. No matching active budget yields an empty map, not an
// error: the report simply renders without budget values.
func (c *Collector) budgetTargets(ctx context.Context, userID string, p Period, dek []byte) (map[string]string, error) {
	period, ok := budgetPeriod(p)
	if !ok {
		return nil, fmt.Errorf("report: unknown period %q", p)
	}
	budgets, err := c.Budget.List(ctx, userID, dek)
	if err != nil {
		return nil, fmt.Errorf("report: list budgets: %w", err)
	}
	targets := make(map[string]string)
	for _, b := range budgets {
		if !b.Active || b.Period != period {
			continue
		}
		for k, v := range b.Targets {
			targets[k] = v
		}
		break // first match wins (deterministic: source order)
	}
	return targets, nil
}

// budgetPeriod maps a report period to the repo budget period spelling
// (monthly->"month", quarterly->"quarter", annual->"year").
func budgetPeriod(p Period) (string, bool) {
	switch p {
	case PeriodMonthly:
		return "month", true
	case PeriodQuarterly:
		return "quarter", true
	case PeriodAnnual:
		return "year", true
	}
	return "", false
}

package report

import (
	"context"
	"fmt"
	"time"
)

// txPageSize is the page size the collector walks Tx.List with: the
// repo's hard cap, so every page except possibly the last is full and a
// short page terminates the walk. ListFilter carries no limit of its
// own — the source adapter must page at this size (8.3's adapter over
// repo.TransactionStore pins List's limit to 100; its default is 50).
const txPageSize = 100

// ListFilter selects one page of transaction rows for TxSource.List:
// [From, To] inclusive at date precision (the same window semantics
// TxSource.Aggregate uses), Offset the zero-based row offset. Ordering
// is the source's (repo: date_posted DESC, id DESC); the collector
// advances Offset by each page's length until a short page.
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
// Aggregate's accountIDs is the account filter (nil = all accounts).
type TxSource interface {
	Aggregate(ctx context.Context, userID string, from, to time.Time, accountIDs []string) ([]CategorySpend, error)
	List(ctx context.Context, userID string, f ListFilter, dek []byte) ([]Row, error)
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

// Collector gathers a Data snapshot from the three narrow sources. All
// time-dependent input is explicit (asOf); Collect never calls now().
type Collector struct {
	Tx       TxSource
	Budget   BudgetSource
	Category CategorySource
}

// Collect gathers the Data snapshot for period p as of asOf:
// Aggregates the current and prior period, walks Tx.List for the
// CURRENT period only (merchant counts are current-period only) and
// derives TopMerchants from those rows, takes the first active
// period-matching budget's targets (empty map when none — not an
// error), and maps category ids to names. An empty prior aggregate
// normalizes to a nil slice, so Generate renders no comparison section
// for a user's first period. dek is passed through to the sources for
// field decryption.
func (c *Collector) Collect(ctx context.Context, userID string, p Period, asOf time.Time, dek []byte) (Data, error) {
	// Validate p before touching the sources: an unknown period is a
	// caller bug surfaced as the documented error rather than Range's
	// panic.
	if _, ok := budgetPeriod(p); !ok {
		return Data{}, fmt.Errorf("report: unknown period %q", p)
	}
	from, to, _ := Range(asOf, p)
	priorFrom, priorTo, _ := Range(priorAsOf(asOf, p), p)

	current, err := c.Tx.Aggregate(ctx, userID, from, to, nil)
	if err != nil {
		return Data{}, fmt.Errorf("report: aggregate current period: %w", err)
	}
	prior, err := c.Tx.Aggregate(ctx, userID, priorFrom, priorTo, nil)
	if err != nil {
		return Data{}, fmt.Errorf("report: aggregate prior period: %w", err)
	}
	if len(prior) == 0 {
		prior = nil
	}

	rows, err := c.listAll(ctx, userID, from, to, dek)
	if err != nil {
		return Data{}, err
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
		Merchants:          TopMerchants(rows, 10),
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

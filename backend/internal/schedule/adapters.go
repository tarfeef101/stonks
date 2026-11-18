package schedule

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"finance-tracker/internal/repo"
	"finance-tracker/internal/report"

	"github.com/jackc/pgx/v5/pgxpool"
)

// dispatchUserQuery is the pinned user listing: every ONBOARDED user
// with at least one notify flag (the period filtering happens in Go —
// one query, once per dispatch execution).
const dispatchUserQuery = `
	SELECT id, email, onboarded_at, notify_monthly, notify_quarterly, notify_annual
	FROM users
	WHERE onboarded_at IS NOT NULL
	  AND (notify_monthly OR notify_quarterly OR notify_annual)`

// PoolUserLister is the production UserLister: the pinned query over
// the pool.
type PoolUserLister struct {
	pool *pgxpool.Pool
}

// NewPoolUserLister returns a PoolUserLister over pool.
func NewPoolUserLister(pool *pgxpool.Pool) *PoolUserLister {
	return &PoolUserLister{pool: pool}
}

// List runs the pinned query; onboarded_at is non-NULL by the WHERE
// clause, so Onboarded comes back true for every row.
func (l *PoolUserLister) List(ctx context.Context) ([]User, error) {
	rows, err := l.pool.Query(ctx, dispatchUserQuery)
	if err != nil {
		return nil, fmt.Errorf("schedule: list dispatch users: %w", err)
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var (
			u         User
			onboarded *time.Time
		)
		if err := rows.Scan(&u.ID, &u.Email, &onboarded, &u.Monthly, &u.Quarterly, &u.Annual); err != nil {
			return nil, fmt.Errorf("schedule: scan dispatch user: %w", err)
		}
		u.Onboarded = onboarded != nil
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("schedule: iterate dispatch users: %w", err)
	}
	return out, nil
}

// PoolUserIDLister is a UserIDLister backed by *pgxpool.Pool. The
// self-heal walks every user, so this is the plain id listing (no
// notification preferences, no onboarding filter).
type PoolUserIDLister struct {
	pool *pgxpool.Pool
}

// NewPoolUserIDLister wraps a pool for schedule.UserIDLister.
func NewPoolUserIDLister(pool *pgxpool.Pool) *PoolUserIDLister {
	return &PoolUserIDLister{pool: pool}
}

// ListUserIDs returns every user id, ordered for determinism.
func (l *PoolUserIDLister) ListUserIDs(ctx context.Context) ([]string, error) {
	rows, err := l.pool.Query(ctx, `SELECT id FROM users ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("schedule: list user ids: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("schedule: scan user id: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("schedule: iterate user ids: %w", err)
	}
	return out, nil
}

// txPageLimit pins repo.TransactionStore.List to its hard cap: the
// collector pages at report.txPageSize (100) and terminates on a short
// page; the store's own default is 50, which would underfill the pages.
const txPageLimit = 100

// TransactionAdapter adapts *repo.TransactionStore to report.TxSource
// (a 1:1 copy of the repo types into the report-local structs).
type TransactionAdapter struct {
	Store *repo.TransactionStore
}

// NewTransactionAdapter returns a TransactionAdapter over store.
func NewTransactionAdapter(store *repo.TransactionStore) *TransactionAdapter {
	return &TransactionAdapter{Store: store}
}

// Aggregate is a 1:1 copy of repo.TransactionStore.Aggregate's rows
// (repo.CategorySpend -> report.CategorySpend, field for field).
func (a *TransactionAdapter) Aggregate(ctx context.Context, userID string, from, to time.Time, accountIDs []string) ([]report.CategorySpend, error) {
	rows, err := a.Store.Aggregate(ctx, userID, from, to, accountIDs)
	if err != nil {
		return nil, err
	}
	out := make([]report.CategorySpend, 0, len(rows))
	for _, g := range rows {
		out = append(out, report.CategorySpend{
			CategoryID: g.CategoryID,
			Spend:      g.Spend,
			Income:     g.Income,
			Net:        g.Net,
		})
	}
	return out, nil
}

// List pages one report.ListFilter window (the limit pinned to the
// repo hard cap) and copies the rows to report.Row.
func (a *TransactionAdapter) List(ctx context.Context, userID string, f report.ListFilter, dek []byte) ([]report.Row, error) {
	from, to := f.From, f.To
	rows, err := a.Store.List(ctx, userID, repo.TransactionFilter{
		DateFrom: &from,
		DateTo:   &to,
		Limit:    txPageLimit,
		Offset:   f.Offset,
	}, dek)
	if err != nil {
		return nil, err
	}
	out := make([]report.Row, 0, len(rows))
	for _, tx := range rows {
		out = append(out, report.Row{Description: tx.Description, Amount: tx.Amount})
	}
	return out, nil
}

// BudgetAdapter adapts *repo.BudgetStore to report.BudgetSource: it
// unmarshals the decrypted Targets blob (a JSON object keyed by
// category id -> canonical 2-decimal string) and passes the period
// spelling through untouched (the repo speaks "month"/"quarter"/
// "year", exactly what report.budgetPeriod compares).
type BudgetAdapter struct {
	Store *repo.BudgetStore
}

// NewBudgetAdapter returns a BudgetAdapter over store.
func NewBudgetAdapter(store *repo.BudgetStore) *BudgetAdapter {
	return &BudgetAdapter{Store: store}
}

// List copies the repo budget rows, unmarshalling Targets (an empty
// blob is impossible in practice — Create validates non-empty — but a
// nil/empty blob yields an empty target map).
func (a *BudgetAdapter) List(ctx context.Context, userID string, dek []byte) ([]report.Budget, error) {
	rows, err := a.Store.List(ctx, userID, dek)
	if err != nil {
		return nil, err
	}
	out := make([]report.Budget, 0, len(rows))
	for _, b := range rows {
		var targets map[string]string
		if len(b.Targets) > 0 {
			if err := json.Unmarshal(b.Targets, &targets); err != nil {
				return nil, fmt.Errorf("schedule: budget %s targets: %w", b.ID, err)
			}
		}
		out = append(out, report.Budget{Period: b.Period, Active: b.Active, Targets: targets})
	}
	return out, nil
}

// CategoryAdapter adapts *repo.CategoryStore to report.CategorySource
// (repo.Category -> report.Name, decrypted names).
type CategoryAdapter struct {
	Store *repo.CategoryStore
}

// NewCategoryAdapter returns a CategoryAdapter over store.
func NewCategoryAdapter(store *repo.CategoryStore) *CategoryAdapter {
	return &CategoryAdapter{Store: store}
}

// List copies the repo category rows.
func (a *CategoryAdapter) List(ctx context.Context, userID string, dek []byte) ([]report.Name, error) {
	rows, err := a.Store.List(ctx, userID, dek)
	if err != nil {
		return nil, err
	}
	out := make([]report.Name, 0, len(rows))
	for _, c := range rows {
		out = append(out, report.Name{ID: c.ID, Name: c.Name})
	}
	return out, nil
}

// PoolNotifier is the production Notifier: repo.NotificationStore.
// Create.
type PoolNotifier struct {
	Store *repo.NotificationStore
}

// NewPoolNotifier returns a PoolNotifier over store.
func NewPoolNotifier(store *repo.NotificationStore) *PoolNotifier {
	return &PoolNotifier{Store: store}
}

// Notify creates the notification row for the user.
func (n *PoolNotifier) Notify(ctx context.Context, userID string, in NotificationIn) error {
	_, err := n.Store.Create(ctx, userID, repo.NotificationIn{
		Kind:  in.Kind,
		Title: in.Title,
		Body:  in.Body,
		Ref:   in.Ref,
	})
	return err
}

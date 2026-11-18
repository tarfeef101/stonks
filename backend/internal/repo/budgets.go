// Per-user budget repository (plan step 2.6, migration 0003; targets
// restructured to a child table in 0018): spending targets by category.
// The name is one DEK-encrypted pack (plan §5). The per-category targets
// are PLAIN rows in the budget_targets child table (0018): per-category
// rows are constraint-checkable (a range CHECK and the FK to
// user_categories) and cascade-safe (a deleted category drops its target
// row, not the budget). The wire format is UNCHANGED by the restructure:
// `targets` is the same {category_id: number} JSON object in and out of
// every endpoint; the store hydrates/marshals the child rows into it,
// building the document by string construction (keys marshalled, values
// the PG numeric text inserted unquoted) so a value like 60.00 comes
// back verbatim — no float64 re-encoding.
//
// Period is immutable (absent from BudgetPatch, like the import-schemas
// kind): it is part of the row's identity for reporting (plan 8.x) and
// the vocabulary mirrors the 0003 CHECK (month|quarter|year) so a bad
// value is a 400 at the repo, not a 23514 surfacing as a 500.
package repo

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"finance-tracker/internal/crypto"

	"github.com/jackc/pgx/v5/pgxpool"
)

// BudgetPeriods is the 0003 budgets.period vocabulary (the same set as
// users.default_period in 0001 and the report periods, plan 8.x).
var BudgetPeriods = []string{"month", "quarter", "year"}

var budgetPeriodOK = func() map[string]bool {
	m := make(map[string]bool, len(BudgetPeriods))
	for _, p := range BudgetPeriods {
		m[p] = true
	}
	return m
}()

// maxTargetValue caps one per-category target at the numeric(14,2)
// ceiling, the same range as transaction amounts (a target larger than
// any possible amount is noise).
const maxTargetValue = 999999999999.99

// Budget is one budgets row with its name decrypted and its targets
// hydrated from the budget_targets child table (0018) into the wire
// format: a JSON object keyed by category id.
type Budget struct {
	ID        string
	Name      string
	Period    string
	Active    bool
	Targets   json.RawMessage
	CreatedAt time.Time
	UpdatedAt time.Time
}

// BudgetIn is the Create input. Active is nil = default true. Targets
// is a non-empty JSON object keyed by the user's own category ids
// (see validateTargets).
type BudgetIn struct {
	Name    string
	Period  string
	Active  *bool
	Targets json.RawMessage
}

// BudgetPatch is the Update input; nil = untouched. Period is
// deliberately absent — it is immutable (see package header).
type BudgetPatch struct {
	Name    *string
	Active  *bool
	Targets *json.RawMessage
}

// BudgetStore persists budgets. One store serves every user; each
// method takes a userID and scopes its SQL on it.
type BudgetStore struct {
	pool *pgxpool.Pool
}

// NewBudgetStore returns a BudgetStore backed by pool (nil for a nil
// pool, mirroring the 1.3c UserStore constructor).
func NewBudgetStore(pool *pgxpool.Pool) *BudgetStore {
	if pool == nil {
		return nil
	}
	return &BudgetStore{pool: pool}
}

// uuidOK accepts the canonical 8-4-4-4-12 uuid text form (any hex
// case). It keeps a malformed target key a 400 at the repo instead of a
// 22P02 (invalid_text_representation) from the id = ANY/IN cast.
func uuidOK(s string) bool {
	if len(s) != 36 {
		return false
	}
	isDash := func(i int) bool { return i == 8 || i == 13 || i == 18 || i == 23 }
	for i := 0; i < 36; i++ {
		b := s[i]
		if isDash(i) {
			if b != '-' {
				return false
			}
		} else if !(b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F') {
			return false
		}
	}
	return true
}

// ValidatePeriod checks the period against the BudgetPeriods
// vocabulary. The 3.3 budget handlers run user-supplied periods through
// it before any store call, so a bad value is a 400 carrying this exact
// message; the repo re-runs it (defense-in-depth, as with validateName).
func ValidatePeriod(period string) error {
	if !budgetPeriodOK[period] {
		return fmt.Errorf("period must be one of %s", strings.Join(BudgetPeriods, ", "))
	}
	return nil
}

// ValidateTargets checks a targets document against the shape rules
// (validateTargetsShape) without a database, for the same 400-before-
// the-repo reason as ValidatePeriod. Ownership (every key is the
// user's own category) is NOT checked here — that needs the user's
// category rows; the repo re-runs the shape rules and then checks
// ownership.
func ValidateTargets(targets json.RawMessage) error {
	_, err := validateTargetsShape(targets)
	return err
}

// validateBudgetFields checks the plaintext input fields; the name goes
// through the shared validateName.
func validateBudgetFields(name, period string) error {
	if err := validateName(name); err != nil {
		return err
	}
	return ValidatePeriod(period)
}

// validateTargetsShape checks a targets document WITHOUT a database:
// a non-empty JSON object, keys in canonical uuid form (so a malformed
// key is a 400 here, not a 22P02 from the id = ANY/IN cast), values
// numbers in [0, maxTargetValue]. It returns the document as
// category id -> numeric LITERAL TEXT (json.Number keeps the source
// digits) so Create/Update insert the exact literal and PG normalizes
// it to 2 decimals — no float64 round trip.
func validateTargetsShape(targets json.RawMessage) (map[string]json.Number, error) {
	if len(targets) == 0 || !json.Valid(targets) {
		return nil, errors.New("targets must be a non-empty JSON document")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(targets, &raw); err != nil {
		return nil, errors.New("targets must be a JSON object")
	}
	if len(raw) == 0 {
		return nil, errors.New("targets must name at least one category")
	}
	out := make(map[string]json.Number, len(raw))
	for k, v := range raw {
		if !uuidOK(k) {
			return nil, fmt.Errorf("target key %q is not a category id", k)
		}
		// A JSON number literal starts with a digit or '-'; strings,
		// booleans, null, and nested documents start with another byte.
		// The check is load-bearing: json.Number is a string type, so
		// json.Unmarshal alone would ACCEPT a JSON string ("50") and the
		// bogus literal would reach the INSERT.
		t := bytes.TrimSpace(v)
		if len(t) == 0 || (t[0] != '-' && (t[0] < '0' || t[0] > '9')) {
			return nil, fmt.Errorf("target for %s must be a number", k)
		}
		var n json.Number
		if err := json.Unmarshal(v, &n); err != nil {
			return nil, fmt.Errorf("target for %s must be a number", k)
		}
		f, err := n.Float64()
		if err != nil || f < 0 || f > maxTargetValue {
			return nil, fmt.Errorf("target for %s must be between 0 and 999999999999.99", k)
		}
		out[k] = n
	}
	return out, nil
}

// validateTargets = the shape rules (validateTargetsShape) plus the
// ownership rule: every key must be the user's OWN category (scoped IN
// check, existence not disclosed — a foreign or missing key is
// ErrCategoryNotFound). It returns the parsed document (category id ->
// numeric literal text) so Create/Update insert without re-parsing.
func (s *BudgetStore) validateTargets(ctx context.Context, userID string, targets json.RawMessage) (map[string]json.Number, error) {
	parsed, err := validateTargetsShape(targets)
	if err != nil {
		return nil, err
	}
	ph := make([]string, 0, len(parsed))
	args := []any{userID}
	for k := range parsed {
		args = append(args, k)
		ph = append(ph, "$"+strconv.Itoa(len(args)))
	}
	var n int
	if err := s.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM user_categories WHERE user_id = $1 AND id IN ("+strings.Join(ph, ", ")+")",
		args...).Scan(&n); err != nil {
		return nil, fmt.Errorf("repo: check budget targets: %w", err)
	}
	if n != len(parsed) {
		return nil, ErrCategoryNotFound
	}
	return parsed, nil
}

// budgetTargetsInsert builds the one-statement multi-row INSERT for the
// budget's target rows (budgets are small; a single statement, no
// batching). The target value is the parsed numeric literal text — PG
// normalizes it to the numeric(14,2) 2-decimal form.
func budgetTargetsInsert(budgetID string, targets map[string]json.Number) (string, []any) {
	vals := make([]string, 0, len(targets))
	args := make([]any, 0, 3*len(targets))
	i := 0
	for cat, n := range targets {
		i++
		args = append(args, budgetID, cat, n.String())
		vals = append(vals, fmt.Sprintf("($%d, $%d, $%d)", 3*i-2, 3*i-1, 3*i))
	}
	return "INSERT INTO budget_targets (budget_id, category_id, target) VALUES " +
		strings.Join(vals, ", "), args
}

// hydrateTargets fetches the budget_targets child rows of the given
// budgets in ONE query ($1::uuid[] — pgx encodes the []string and the
// cast makes the comparison valid) and groups them into the wire-format
// documents: one {category_id: target-text} object per budget. Keys are
// marshalled defensively (uuid-shaped by construction); values are the
// PG numeric text inserted UNQUOTED, so the canonical 2-decimal form
// (e.g. 60.00) survives to the client. A budget with no rows gets no
// entry — the callers default it to {}.
func (s *BudgetStore) hydrateTargets(ctx context.Context, budgetIDs []string) (map[string]json.RawMessage, error) {
	out := make(map[string]json.RawMessage, len(budgetIDs))
	if len(budgetIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT bt.budget_id::text, bt.category_id::text, bt.target::text
		FROM budget_targets bt
		WHERE bt.budget_id = ANY($1::uuid[])
		ORDER BY bt.category_id`, budgetIDs)
	if err != nil {
		return nil, fmt.Errorf("repo: hydrate budget targets: %w", err)
	}
	defer rows.Close()
	parts := make(map[string][]string, len(budgetIDs))
	for rows.Next() {
		var bid, cat, val string
		if err := rows.Scan(&bid, &cat, &val); err != nil {
			return nil, fmt.Errorf("repo: scan budget target: %w", err)
		}
		key, err := json.Marshal(cat)
		if err != nil {
			return nil, fmt.Errorf("repo: marshal budget target key: %w", err)
		}
		parts[bid] = append(parts[bid], string(key)+":"+val)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo: hydrate budget targets: %w", err)
	}
	for id, ps := range parts {
		out[id] = json.RawMessage("{" + strings.Join(ps, ",") + "}")
	}
	return out, nil
}

// targetDoc picks the hydrated document for id, defaulting to {} when
// the budget has no target rows (a transient state the schema allows).
func targetDoc(doc map[string]json.RawMessage, id string) json.RawMessage {
	if d, ok := doc[id]; ok {
		return d
	}
	return json.RawMessage("{}")
}

// List returns the user's budgets ordered by created_at (ties: id). dek
// decrypts the names; targets are hydrated from budget_targets (0018).
func (s *BudgetStore) List(ctx context.Context, userID string, dek []byte) ([]Budget, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, period, active, name_encrypted, created_at, updated_at
		FROM budgets
		WHERE user_id = $1
		ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, fmt.Errorf("repo: list budgets: %w", err)
	}
	defer rows.Close()
	out := make([]Budget, 0, 4)
	ids := make([]string, 0, 4)
	for rows.Next() {
		var (
			b        Budget
			namePack []byte
		)
		if err := rows.Scan(&b.ID, &b.Period, &b.Active, &namePack, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, fmt.Errorf("repo: scan budget: %w", err)
		}
		name, err := crypto.Open(dek, namePack)
		if err != nil {
			return nil, fmt.Errorf("repo: decrypt budget %s name: %w", b.ID, err)
		}
		b.Name = string(name)
		out = append(out, b)
		ids = append(ids, b.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo: list budgets: %w", err)
	}
	doc, err := s.hydrateTargets(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Targets = targetDoc(doc, out[i].ID)
	}
	return out, nil
}

// ActiveBudget is one active budget's targets, hydrated from the child
// table. The name is deliberately not selected: the summary response
// only needs the targets, and skipping the name column keeps the
// response surface minimal.
type ActiveBudget struct {
	ID      string
	Targets json.RawMessage
}

// ActiveTargets returns the targets of every ACTIVE budget of the
// user's for one period. The period and active filters run in SQL
// (both columns are plaintext) so the wide List result set is never
// materialized for the summary; the targets then come from
// budget_targets (0018) via the same hydration as List/Get. The dek
// parameter is kept for interface stability (the server's BudgetStore
// interface) — targets are plaintext child rows now, so it is unused.
func (s *BudgetStore) ActiveTargets(ctx context.Context, userID, period string, dek []byte) ([]ActiveBudget, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id
		FROM budgets
		WHERE user_id = $1 AND period = $2 AND active = true
		ORDER BY created_at, id`, userID, period)
	if err != nil {
		return nil, fmt.Errorf("repo: list active budgets: %w", err)
	}
	defer rows.Close()
	out := make([]ActiveBudget, 0, 4)
	ids := make([]string, 0, 4)
	for rows.Next() {
		var b ActiveBudget
		if err := rows.Scan(&b.ID); err != nil {
			return nil, fmt.Errorf("repo: scan budget: %w", err)
		}
		out = append(out, b)
		ids = append(ids, b.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo: list active budgets: %w", err)
	}
	doc, err := s.hydrateTargets(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Targets = targetDoc(doc, out[i].ID)
	}
	return out, nil
}

// Get returns one of the user's budgets, or ErrBudgetNotFound when the
// id is not the user's (existence is not disclosed across users). dek
// decrypts the name; targets are hydrated from budget_targets (0018).
func (s *BudgetStore) Get(ctx context.Context, userID, id string, dek []byte) (Budget, error) {
	var (
		b        Budget
		namePack []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT period, active, name_encrypted, created_at, updated_at
		FROM budgets
		WHERE user_id = $1 AND id = $2`, userID, id).
		Scan(&b.Period, &b.Active, &namePack, &b.CreatedAt, &b.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Budget{}, ErrBudgetNotFound
	}
	if err != nil {
		return Budget{}, fmt.Errorf("repo: get budget: %w", err)
	}
	b.ID = id
	name, err := crypto.Open(dek, namePack)
	if err != nil {
		return Budget{}, fmt.Errorf("repo: decrypt budget %s name: %w", b.ID, err)
	}
	b.Name = string(name)
	doc, err := s.hydrateTargets(ctx, []string{id})
	if err != nil {
		return Budget{}, err
	}
	b.Targets = targetDoc(doc, id)
	return b, nil
}

// Create inserts a budget for the user and returns it. A target key
// that is missing or owned by another user is ErrCategoryNotFound. The
// budget row and its target rows land in ONE transaction, so a budget
// never exists with a partial or missing target set.
func (s *BudgetStore) Create(ctx context.Context, userID string, in BudgetIn, dek []byte) (Budget, error) {
	if err := validateBudgetFields(in.Name, in.Period); err != nil {
		return Budget{}, fmt.Errorf("repo: %w", err)
	}
	targets, err := s.validateTargets(ctx, userID, in.Targets)
	if err != nil {
		return Budget{}, err
	}
	name := strings.TrimSpace(in.Name)
	namePack, err := crypto.Seal(dek, []byte(name))
	if err != nil {
		return Budget{}, fmt.Errorf("repo: seal budget name: %w", err)
	}
	active := true
	if in.Active != nil {
		active = *in.Active
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Budget{}, fmt.Errorf("repo: create budget: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = tx.Rollback(rctx)
		}
	}()

	var id string
	if err := tx.QueryRow(ctx, `
		INSERT INTO budgets (user_id, period, active, name_encrypted)
		VALUES ($1, $2, $3, $4)
		RETURNING id`,
		userID, in.Period, active, namePack).Scan(&id); err != nil {
		return Budget{}, fmt.Errorf("repo: create budget: %w", err)
	}
	stmt, args := budgetTargetsInsert(id, targets)
	if _, err := tx.Exec(ctx, stmt, args...); err != nil {
		return Budget{}, fmt.Errorf("repo: create budget: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Budget{}, fmt.Errorf("repo: create budget: %w", err)
	}
	committed = true
	return s.Get(ctx, userID, id, dek)
}

// Update applies the non-nil fields of p to the user's budget and
// returns the updated row. A renamed name is re-sealed under the same
// DEK; a targets patch is validated (shape + ownership) and then
// rewritten WHOLESALE — DELETE of the child rows plus a bulk re-INSERT
// (budgets are small; no diffing) — in ONE transaction with the budgets
// UPDATE, which runs for a targets patch too: even a re-sent identical
// document is a rewrite, and updated_at moves with it. A patch that
// changes nothing — name re-trimmed to the same string, same active, no
// targets — is a no-op: the row comes back without an UPDATE, so
// updated_at stays put.
func (s *BudgetStore) Update(ctx context.Context, userID, id string, p BudgetPatch, dek []byte) (Budget, error) {
	var (
		curActive bool
		namePack  []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT active, name_encrypted
		FROM budgets
		WHERE user_id = $1 AND id = $2`, userID, id).
		Scan(&curActive, &namePack)
	if errors.Is(err, sql.ErrNoRows) {
		return Budget{}, ErrBudgetNotFound
	}
	if err != nil {
		return Budget{}, fmt.Errorf("repo: get budget: %w", err)
	}
	curName, err := crypto.Open(dek, namePack)
	if err != nil {
		return Budget{}, fmt.Errorf("repo: decrypt budget %s name: %w", id, err)
	}

	nameChanged := false
	if p.Name != nil {
		if err := validateName(*p.Name); err != nil {
			return Budget{}, fmt.Errorf("repo: %w", err)
		}
		if n := strings.TrimSpace(*p.Name); n != string(curName) {
			pack, err := crypto.Seal(dek, []byte(n))
			if err != nil {
				return Budget{}, fmt.Errorf("repo: seal budget name: %w", err)
			}
			namePack, nameChanged = pack, true
		}
	}
	activeChanged := p.Active != nil && *p.Active != curActive
	var targets map[string]json.Number
	targetsChanged := false
	if p.Targets != nil {
		if targets, err = s.validateTargets(ctx, userID, *p.Targets); err != nil {
			return Budget{}, err
		}
		targetsChanged = true
	}

	if !nameChanged && !activeChanged && !targetsChanged {
		// No-op patch: return the current row without an UPDATE.
		return s.Get(ctx, userID, id, dek)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Budget{}, fmt.Errorf("repo: update budget: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = tx.Rollback(rctx)
		}
	}()

	if nameChanged || activeChanged || targetsChanged {
		sets := make([]string, 0, 4)
		args := []any{userID, id}
		addSet := func(col string, val any) {
			args = append(args, val)
			sets = append(sets, col+" = $"+strconv.Itoa(len(args)))
		}
		if nameChanged {
			addSet("name_encrypted", namePack)
		}
		if activeChanged {
			addSet("active", *p.Active)
		}
		sets = append(sets, "updated_at = now()")
		cmdTag, err := tx.Exec(ctx,
			"UPDATE budgets SET "+strings.Join(sets, ", ")+" WHERE user_id = $1 AND id = $2", args...)
		if err != nil {
			return Budget{}, fmt.Errorf("repo: update budget: %w", err)
		}
		if cmdTag.RowsAffected() == 0 {
			return Budget{}, ErrBudgetNotFound // deleted after the SELECT above
		}
	}
	if targetsChanged {
		if _, err := tx.Exec(ctx, "DELETE FROM budget_targets WHERE budget_id = $1", id); err != nil {
			return Budget{}, fmt.Errorf("repo: update budget: %w", err)
		}
		stmt, targs := budgetTargetsInsert(id, targets)
		if _, err := tx.Exec(ctx, stmt, targs...); err != nil {
			return Budget{}, fmt.Errorf("repo: update budget: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Budget{}, fmt.Errorf("repo: update budget: %w", err)
	}
	committed = true
	return s.Get(ctx, userID, id, dek)
}

// Delete removes the user's budget; its budget_targets child rows
// cascade at the DB level (0018 ON DELETE CASCADE). An unknown or other
// user's id is ErrBudgetNotFound; the row is left untouched.
func (s *BudgetStore) Delete(ctx context.Context, userID, id string) error {
	cmdTag, err := s.pool.Exec(ctx, `
		DELETE FROM budgets
		WHERE user_id = $1 AND id = $2`, userID, id)
	if err != nil {
		return fmt.Errorf("repo: delete budget: %w", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return ErrBudgetNotFound
	}
	return nil
}

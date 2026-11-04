// Per-user budget repository (plan step 2.6, migration 0003): spending
// targets by category. name + the ENTIRE targets jsonb are each one
// DEK-encrypted pack (plan §5: budget-vs-actual compares decrypted rows
// in-app — zero SQL cost, so the table has no name_hash and no
// uniqueness constraint). Targets is a JSON object keyed by the user's
// own category ids, values numeric >= 0.
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

// Budget is one budgets row with its encrypted fields opened. Name is
// decrypted; Targets is the decrypted blob (a JSON object keyed by
// category id).
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
// numbers in [0, maxTargetValue]. It returns the keys (map order — the
// callers don't depend on it).
func validateTargetsShape(targets json.RawMessage) ([]string, error) {
	if len(targets) == 0 || !json.Valid(targets) {
		return nil, errors.New("targets must be a non-empty JSON document")
	}
	var obj map[string]any
	if err := json.Unmarshal(targets, &obj); err != nil {
		return nil, errors.New("targets must be a JSON object")
	}
	if len(obj) == 0 {
		return nil, errors.New("targets must name at least one category")
	}
	keys := make([]string, 0, len(obj))
	for k, v := range obj {
		if !uuidOK(k) {
			return nil, fmt.Errorf("target key %q is not a category id", k)
		}
		n, ok := v.(float64)
		if !ok {
			return nil, fmt.Errorf("target for %s must be a number", k)
		}
		if n < 0 || n > maxTargetValue {
			return nil, fmt.Errorf("target for %s must be between 0 and 999999999999.99", k)
		}
		keys = append(keys, k)
	}
	return keys, nil
}

// validateTargets = the shape rules (validateTargetsShape) plus the
// ownership rule: every key must be the user's OWN category (scoped IN
// check, existence not disclosed — a foreign or missing key is
// ErrCategoryNotFound).
func (s *BudgetStore) validateTargets(ctx context.Context, userID string, targets json.RawMessage) error {
	keys, err := validateTargetsShape(targets)
	if err != nil {
		return err
	}
	ph := make([]string, 0, len(keys))
	args := []any{userID}
	for _, k := range keys {
		args = append(args, k)
		ph = append(ph, "$"+strconv.Itoa(len(args)))
	}
	var n int
	if err := s.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM user_categories WHERE user_id = $1 AND id IN ("+strings.Join(ph, ", ")+")",
		args...).Scan(&n); err != nil {
		return fmt.Errorf("repo: check budget targets: %w", err)
	}
	if n != len(keys) {
		return ErrCategoryNotFound
	}
	return nil
}

// decrypt opens the name + targets packs. A pack that does not open is a
// key/data mismatch the API surfaces as 500 (the DEK came from the
// user's own wrapped_dek, so this is corruption, not a client error) —
// same rule as the other stores.
func (b *Budget) decrypt(namePack, targetsPack []byte, dek []byte) error {
	name, err := crypto.Open(dek, namePack)
	if err != nil {
		return fmt.Errorf("repo: decrypt budget %s name: %w", b.ID, err)
	}
	b.Name = string(name)
	tgt, err := crypto.Open(dek, targetsPack)
	if err != nil {
		return fmt.Errorf("repo: decrypt budget %s targets: %w", b.ID, err)
	}
	b.Targets = json.RawMessage(tgt)
	return nil
}

// List returns the user's budgets ordered by created_at (ties: id).
// dek decrypts names + targets.
func (s *BudgetStore) List(ctx context.Context, userID string, dek []byte) ([]Budget, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, period, active, name_encrypted, targets_encrypted, created_at, updated_at
		FROM budgets
		WHERE user_id = $1
		ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, fmt.Errorf("repo: list budgets: %w", err)
	}
	defer rows.Close()
	out := make([]Budget, 0, 4)
	for rows.Next() {
		var (
			b        Budget
			namePack []byte
			tgtPack  []byte
		)
		if err := rows.Scan(&b.ID, &b.Period, &b.Active, &namePack, &tgtPack, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, fmt.Errorf("repo: scan budget: %w", err)
		}
		if err := b.decrypt(namePack, tgtPack, dek); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo: list budgets: %w", err)
	}
	return out, nil
}

// Get returns one of the user's budgets, or ErrBudgetNotFound when the
// id is not the user's (existence is not disclosed across users).
func (s *BudgetStore) Get(ctx context.Context, userID, id string, dek []byte) (Budget, error) {
	var (
		b        Budget
		namePack []byte
		tgtPack  []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT period, active, name_encrypted, targets_encrypted, created_at, updated_at
		FROM budgets
		WHERE user_id = $1 AND id = $2`, userID, id).
		Scan(&b.Period, &b.Active, &namePack, &tgtPack, &b.CreatedAt, &b.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Budget{}, ErrBudgetNotFound
	}
	if err != nil {
		return Budget{}, fmt.Errorf("repo: get budget: %w", err)
	}
	b.ID = id
	if err := b.decrypt(namePack, tgtPack, dek); err != nil {
		return Budget{}, err
	}
	return b, nil
}

// Create inserts a budget for the user and returns it. A target key
// that is missing or owned by another user is ErrCategoryNotFound.
func (s *BudgetStore) Create(ctx context.Context, userID string, in BudgetIn, dek []byte) (Budget, error) {
	if err := validateBudgetFields(in.Name, in.Period); err != nil {
		return Budget{}, fmt.Errorf("repo: %w", err)
	}
	if err := s.validateTargets(ctx, userID, in.Targets); err != nil {
		return Budget{}, err
	}
	name := strings.TrimSpace(in.Name)
	namePack, err := crypto.Seal(dek, []byte(name))
	if err != nil {
		return Budget{}, fmt.Errorf("repo: seal budget name: %w", err)
	}
	tgtPack, err := crypto.Seal(dek, in.Targets)
	if err != nil {
		return Budget{}, fmt.Errorf("repo: seal budget targets: %w", err)
	}
	active := true
	if in.Active != nil {
		active = *in.Active
	}
	var id string
	err = s.pool.QueryRow(ctx, `
		INSERT INTO budgets (user_id, period, active, name_encrypted, targets_encrypted)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id`,
		userID, in.Period, active, namePack, tgtPack).Scan(&id)
	if err != nil {
		return Budget{}, fmt.Errorf("repo: create budget: %w", err)
	}
	return s.Get(ctx, userID, id, dek)
}

// Update applies the non-nil fields of p to the user's budget and
// returns the updated row. Renamed names / changed targets are
// re-sealed under the same DEK. An unchanged patch (same name, active,
// and target values) is a no-op: the row comes back without an UPDATE,
// so updated_at stays put.
func (s *BudgetStore) Update(ctx context.Context, userID, id string, p BudgetPatch, dek []byte) (Budget, error) {
	var (
		curPeriod string
		curActive bool
		namePack  []byte
		tgtPack   []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT period, active, name_encrypted, targets_encrypted
		FROM budgets
		WHERE user_id = $1 AND id = $2`, userID, id).
		Scan(&curPeriod, &curActive, &namePack, &tgtPack)
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
	curTargets, err := crypto.Open(dek, tgtPack)
	if err != nil {
		return Budget{}, fmt.Errorf("repo: decrypt budget %s targets: %w", id, err)
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
	var tgtArg []byte
	targetsChanged := false
	if p.Targets != nil {
		if err := s.validateTargets(ctx, userID, *p.Targets); err != nil {
			return Budget{}, err
		}
		if !bytes.Equal(*p.Targets, curTargets) {
			pack, err := crypto.Seal(dek, *p.Targets)
			if err != nil {
				return Budget{}, fmt.Errorf("repo: seal budget targets: %w", err)
			}
			tgtArg, targetsChanged = pack, true
		}
	}

	if !nameChanged && !activeChanged && !targetsChanged {
		// No-op patch: return the current row without an UPDATE.
		return s.Get(ctx, userID, id, dek)
	}

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
	if targetsChanged {
		addSet("targets_encrypted", tgtArg)
	}
	sets = append(sets, "updated_at = now()")

	cmdTag, err := s.pool.Exec(ctx,
		"UPDATE budgets SET "+strings.Join(sets, ", ")+" WHERE user_id = $1 AND id = $2",
		args...)
	if err != nil {
		return Budget{}, fmt.Errorf("repo: update budget: %w", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return Budget{}, ErrBudgetNotFound // deleted after the SELECT above
	}
	return s.Get(ctx, userID, id, dek)
}

// Delete removes the user's budget (nothing references budgets; 0003 has
// no inbound FKs). An unknown or other user's id is ErrBudgetNotFound;
// the row is left untouched.
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

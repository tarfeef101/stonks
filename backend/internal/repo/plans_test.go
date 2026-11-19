// Plan limit override + room adjustment tests (round 51c): one
// postgres:18 testcontainer per test (newTestEnv) covering
// UpsertLimitOverride (in-place update, updated_at advance, the list
// filter matrix, the delete not-found rule), CreateRoomAdjustment
// (signed amounts, sealed-note round trip with raw-column checks,
// plan_type/year ordering, the delete not-found rule), the shared
// validation (vocabulary, year window, limit >= 0, amount grammar, no
// partial persistence), and user isolation across both tables.
package repo

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// rawNotePack returns the plan_room_adjustments.note_encrypted pack for
// id (present=false when the column is NULL or the row is gone).
func rawNotePack(t *testing.T, pool *pgxpool.Pool, ctx context.Context, id string) ([]byte, bool) {
	t.Helper()
	var pack *[]byte
	err := pool.QueryRow(ctx, `
		SELECT note_encrypted
		FROM plan_room_adjustments
		WHERE id = $1`, id).Scan(&pack)
	if err != nil {
		t.Fatalf("raw note query: %v", err)
	}
	if pack == nil {
		return nil, false
	}
	return *pack, true
}

// planTableCounts returns the (plan_limit_overrides,
// plan_room_adjustments) row counts; the testcontainer DB is isolated
// per test, so whole-table counts are per-test counters.
func planTableCounts(t *testing.T, pool *pgxpool.Pool, ctx context.Context) (int, int) {
	t.Helper()
	var o, a int
	err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM plan_limit_overrides),
		       (SELECT count(*) FROM plan_room_adjustments)`).Scan(&o, &a)
	if err != nil {
		t.Fatalf("count plan tables: %v", err)
	}
	return o, a
}

func TestLimitOverrideUpsertListDelete(t *testing.T) {
	env := newTestEnv(t, "lo-a", "lo-b")
	pl := NewPlanStore(env.pool)
	a := env.user(t, "lo-a")
	b := env.user(t, "lo-b")
	ctx := env.ctx

	first, err := pl.UpsertLimitOverride(ctx, a.row.ID, "rrsp", 2026, "33810.00")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if first.ID == "" || first.PlanType != "rrsp" || first.Year != 2026 ||
		first.AnnualLimit != "33810.00" {
		t.Fatalf("override row: %+v", first)
	}

	// Re-upsert the same key: one row, limit refreshed, updated_at
	// advanced, created_at untouched.
	second, err := pl.UpsertLimitOverride(ctx, a.row.ID, "rrsp", 2026, "40000.00")
	if err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("upsert must update in place: %s then %s", first.ID, second.ID)
	}
	if second.AnnualLimit != "40000.00" {
		t.Fatalf("limit: want 40000.00, got %q", second.AnnualLimit)
	}
	if !second.UpdatedAt.After(first.UpdatedAt) {
		t.Fatalf("updated_at must advance: %s then %s", first.UpdatedAt, second.UpdatedAt)
	}
	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("created_at must be preserved: %s then %s", first.CreatedAt, second.CreatedAt)
	}
	if o, _ := planTableCounts(t, env.pool, ctx); o != 1 {
		t.Fatalf("override rows: want 1, got %d", o)
	}

	// Filter-matrix seeds: rrsp/2025 and tfsa/2026.
	if _, err := pl.UpsertLimitOverride(ctx, a.row.ID, "rrsp", 2025, "31560.00"); err != nil {
		t.Fatalf("seed rrsp 2025: %v", err)
	}
	tfsa26, err := pl.UpsertLimitOverride(ctx, a.row.ID, "tfsa", 2026, "7000.00")
	if err != nil {
		t.Fatalf("seed tfsa 2026: %v", err)
	}

	byPlan, err := pl.ListLimitOverrides(ctx, a.row.ID, "rrsp", 0)
	if err != nil {
		t.Fatalf("list by plan: %v", err)
	}
	if len(byPlan) != 2 || byPlan[0].Year != 2025 || byPlan[1].Year != 2026 ||
		byPlan[1].AnnualLimit != "40000.00" {
		t.Fatalf("by plan: want rrsp 2025+2026 (40000.00), got %+v", byPlan)
	}
	byYear, err := pl.ListLimitOverrides(ctx, a.row.ID, "", 2026)
	if err != nil {
		t.Fatalf("list by year: %v", err)
	}
	if len(byYear) != 2 || byYear[0].PlanType != "rrsp" || byYear[1].PlanType != "tfsa" {
		t.Fatalf("by year: want rrsp then tfsa, got %+v", byYear)
	}
	both, err := pl.ListLimitOverrides(ctx, a.row.ID, "rrsp", 2026)
	if err != nil {
		t.Fatalf("list both: %v", err)
	}
	if len(both) != 1 || both[0].ID != second.ID {
		t.Fatalf("both: want the rrsp/2026 row, got %+v", both)
	}
	all, err := pl.ListLimitOverrides(ctx, a.row.ID, "", 0)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("all: want 3, got %d: %+v", len(all), all)
	}

	// Delete: gone, second delete not-found, foreign id not-found
	// (existence not disclosed).
	if err := pl.DeleteLimitOverride(ctx, a.row.ID, second.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	gone, err := pl.ListLimitOverrides(ctx, a.row.ID, "rrsp", 2026)
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(gone) != 0 {
		t.Fatalf("deleted row must be gone: %+v", gone)
	}
	if err := pl.DeleteLimitOverride(ctx, a.row.ID, second.ID); !errors.Is(err, ErrLimitOverrideNotFound) {
		t.Fatalf("second delete: want ErrLimitOverrideNotFound, got %v", err)
	}
	if err := pl.DeleteLimitOverride(ctx, b.row.ID, tfsa26.ID); !errors.Is(err, ErrLimitOverrideNotFound) {
		t.Fatalf("foreign delete: want ErrLimitOverrideNotFound, got %v", err)
	}
}

func TestRoomAdjustmentCreateListDelete(t *testing.T) {
	env := newTestEnv(t, "ra-a", "ra-b")
	pl := NewPlanStore(env.pool)
	a := env.user(t, "ra-a")
	b := env.user(t, "ra-b")
	ctx := env.ctx

	withNote, err := pl.CreateRoomAdjustment(ctx, a.row.ID, "rrsp", 2026, "250.00", "legacy room", a.dek)
	if err != nil {
		t.Fatalf("create positive: %v", err)
	}
	noNote, err := pl.CreateRoomAdjustment(ctx, a.row.ID, "tfsa", 2025, "-100.00", "", a.dek)
	if err != nil {
		t.Fatalf("create negative: %v", err)
	}
	earlier, err := pl.CreateRoomAdjustment(ctx, a.row.ID, "rrsp", 2025, "-50.00", "one-off grant", a.dek)
	if err != nil {
		t.Fatalf("create third: %v", err)
	}
	if withNote.ID == "" || withNote.PlanType != "rrsp" || withNote.Year != 2026 ||
		withNote.Amount != "250.00" || withNote.Note != "legacy room" {
		t.Fatalf("positive adjustment: %+v", withNote)
	}
	if noNote.Amount != "-100.00" || noNote.Note != "" ||
		noNote.PlanType != "tfsa" || noNote.Year != 2025 {
		t.Fatalf("negative adjustment: %+v", noNote)
	}
	if earlier.Amount != "-50.00" || earlier.Note != "one-off grant" {
		t.Fatalf("third adjustment: %+v", earlier)
	}

	// Raw-column checks: a non-empty note is a sealed pack (no
	// plaintext); an empty note is NULL.
	pack, present := rawNotePack(t, env.pool, ctx, withNote.ID)
	if !present || len(pack) == 0 {
		t.Fatalf("positive note must be sealed, pack %q (present=%v)", pack, present)
	}
	if strings.Contains(string(pack), "legacy room") {
		t.Fatal("plaintext note must not appear in note_encrypted")
	}
	if pack, present := rawNotePack(t, env.pool, ctx, noNote.ID); present || len(pack) != 0 {
		t.Fatalf("empty note must be NULL, got %q (present=%v)", pack, present)
	}

	// Ordered plan_type, then year: rrsp/2025, rrsp/2026, tfsa/2025.
	all, err := pl.ListRoomAdjustments(ctx, a.row.ID, "", a.dek)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	wantIDs := []string{earlier.ID, withNote.ID, noNote.ID}
	if len(all) != len(wantIDs) {
		t.Fatalf("rows: want %d, got %d: %+v", len(wantIDs), len(all), all)
	}
	for i, id := range wantIDs {
		if all[i].ID != id {
			t.Fatalf("order[%d]: want %s, got %s (all %+v)", i, id, all[i].ID, all)
		}
	}
	byPlan, err := pl.ListRoomAdjustments(ctx, a.row.ID, "rrsp", a.dek)
	if err != nil {
		t.Fatalf("list by plan: %v", err)
	}
	if len(byPlan) != 2 || byPlan[0].ID != earlier.ID || byPlan[1].ID != withNote.ID {
		t.Fatalf("by plan: want rrsp 2025 then 2026, got %+v", byPlan)
	}

	// Delete: gone, again not-found, foreign not-found.
	if err := pl.DeleteRoomAdjustment(ctx, a.row.ID, noNote.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	gone, err := pl.ListRoomAdjustments(ctx, a.row.ID, "tfsa", a.dek)
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(gone) != 0 {
		t.Fatalf("deleted row must be gone: %+v", gone)
	}
	if err := pl.DeleteRoomAdjustment(ctx, a.row.ID, noNote.ID); !errors.Is(err, ErrAdjustmentNotFound) {
		t.Fatalf("second delete: want ErrAdjustmentNotFound, got %v", err)
	}
	if err := pl.DeleteRoomAdjustment(ctx, b.row.ID, withNote.ID); !errors.Is(err, ErrAdjustmentNotFound) {
		t.Fatalf("foreign delete: want ErrAdjustmentNotFound, got %v", err)
	}
}

func TestPlanValidation(t *testing.T) {
	env := newTestEnv(t, "pv")
	pl := NewPlanStore(env.pool)
	a := env.user(t, "pv")
	ctx := env.ctx

	// Plan type must be in the plans vocabulary ("" is not one of the
	// six; the list methods use "" as a wildcard but writes must not).
	for _, pt := range []string{"tfsaa", "bank", ""} {
		if _, err := pl.UpsertLimitOverride(ctx, a.row.ID, pt, 2026, "1.00"); err == nil {
			t.Fatalf("plan type %q must fail (upsert)", pt)
		}
		if _, err := pl.CreateRoomAdjustment(ctx, a.row.ID, pt, 2026, "1.00", "", a.dek); err == nil {
			t.Fatalf("plan type %q must fail (adjustment)", pt)
		}
	}
	// Year window is 1900..2100.
	for _, y := range []int{1899, 2101} {
		if _, err := pl.UpsertLimitOverride(ctx, a.row.ID, "rrsp", y, "1.00"); err == nil {
			t.Fatalf("year %d must fail (upsert)", y)
		}
		if _, err := pl.CreateRoomAdjustment(ctx, a.row.ID, "rrsp", y, "1.00", "", a.dek); err == nil {
			t.Fatalf("year %d must fail (adjustment)", y)
		}
	}
	// Annual limit: canonical grammar and >= 0 (a leading '-' is
	// canonical-valid but must be rejected by the >= 0 check).
	for _, lim := range []string{"-5", "1.234"} {
		if _, err := pl.UpsertLimitOverride(ctx, a.row.ID, "rrsp", 2026, lim); err == nil {
			t.Fatalf("annual limit %q must fail", lim)
		}
	}
	// Adjustment amount: canonical grammar, sign allowed.
	for _, amt := range []string{"1.234"} {
		if _, err := pl.CreateRoomAdjustment(ctx, a.row.ID, "rrsp", 2026, amt, "", a.dek); err == nil {
			t.Fatalf("amount %q must fail", amt)
		}
	}

	if o, aj := planTableCounts(t, env.pool, ctx); o != 0 || aj != 0 {
		t.Fatalf("validation failures must persist nothing: overrides %d, adjustments %d", o, aj)
	}
}

func TestPlanUserIsolation(t *testing.T) {
	env := newTestEnv(t, "pi-a", "pi-b")
	pl := NewPlanStore(env.pool)
	a := env.user(t, "pi-a")
	b := env.user(t, "pi-b")
	ctx := env.ctx

	ovA, err := pl.UpsertLimitOverride(ctx, a.row.ID, "fhsa", 2027, "15000.00")
	if err != nil {
		t.Fatalf("a upsert: %v", err)
	}
	adjA, err := pl.CreateRoomAdjustment(ctx, a.row.ID, "rdsp", 2024, "75.25", "isolated note", a.dek)
	if err != nil {
		t.Fatalf("a adjust: %v", err)
	}

	// b sees none of a's rows (empty lists, not errors).
	ovs, err := pl.ListLimitOverrides(ctx, b.row.ID, "", 0)
	if err != nil {
		t.Fatalf("b list overrides: %v", err)
	}
	if len(ovs) != 0 {
		t.Fatalf("b must see no overrides: %+v", ovs)
	}
	adj, err := pl.ListRoomAdjustments(ctx, b.row.ID, "", b.dek)
	if err != nil {
		t.Fatalf("b list adjustments: %v", err)
	}
	if len(adj) != 0 {
		t.Fatalf("b must see no adjustments: %+v", adj)
	}

	// ...and cannot delete them (not-found, not forbidden).
	if err := pl.DeleteLimitOverride(ctx, b.row.ID, ovA.ID); !errors.Is(err, ErrLimitOverrideNotFound) {
		t.Fatalf("b delete a override: want ErrLimitOverrideNotFound, got %v", err)
	}
	if err := pl.DeleteRoomAdjustment(ctx, b.row.ID, adjA.ID); !errors.Is(err, ErrAdjustmentNotFound) {
		t.Fatalf("b delete a adjustment: want ErrAdjustmentNotFound, got %v", err)
	}

	// a's rows survive untouched.
	got, err := pl.ListLimitOverrides(ctx, a.row.ID, "fhsa", 2027)
	if err != nil {
		t.Fatalf("a list after: %v", err)
	}
	if len(got) != 1 || got[0].ID != ovA.ID || got[0].AnnualLimit != "15000.00" {
		t.Fatalf("a override must survive: %+v", got)
	}
	gotAdj, err := pl.ListRoomAdjustments(ctx, a.row.ID, "rdsp", a.dek)
	if err != nil {
		t.Fatalf("a list adjustments after: %v", err)
	}
	if len(gotAdj) != 1 || gotAdj[0].ID != adjA.ID || gotAdj[0].Amount != "75.25" ||
		gotAdj[0].Note != "isolated note" {
		t.Fatalf("a adjustment must survive: %+v", gotAdj)
	}
}

// Budget store tests (plan step 2.6, second half of the 2.6 split):
// one shared postgres:18 testcontainer (newTestEnv) with subtests
// covering the CRUD round trip (name + targets blob decrypt),
// validation (period vocabulary, targets shape, per-user category keys,
// value range), the no-op update rule, and per-user isolation.
package repo

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// rawPtr points at a json.RawMessage for BudgetPatch.Targets.
func rawPtr(r json.RawMessage) *json.RawMessage { return &r }

// jsonDocsEqual reports whether two JSON documents parse to the same
// value (jsonb normalizes whitespace, so byte equality is the wrong
// test — same pin as the import-schemas spec tests).
func jsonDocsEqual(a, b []byte) bool {
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &y); err != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

func TestBudgetStore(t *testing.T) {
	env := newTestEnv(t, "bud-a", "bud-b")
	cat := NewCategoryStore(env.pool)
	bud := NewBudgetStore(env.pool)
	if bud == nil || cat == nil {
		t.Fatal("non-nil pool must give non-nil stores")
	}
	if NewBudgetStore(nil) != nil {
		t.Fatal("nil pool must give nil store")
	}
	a := env.user(t, "bud-a")
	b := env.user(t, "bud-b")
	ctx := env.ctx
	aid, bid := a.row.ID, b.row.ID

	// Target keys must be the user's OWN categories.
	fa, err := cat.Create(ctx, aid, CategoryIn{Name: "Food"}, a.dek)
	if err != nil {
		t.Fatalf("create category: %v", err)
	}
	fu, err := cat.Create(ctx, aid, CategoryIn{Name: "Fun"}, a.dek)
	if err != nil {
		t.Fatalf("create category: %v", err)
	}
	fb, err := cat.Create(ctx, bid, CategoryIn{Name: "Food"}, b.dek)
	if err != nil {
		t.Fatalf("create other user's category: %v", err)
	}

	targets := func(vals map[string]any) json.RawMessage {
		t.Helper()
		out, err := json.Marshal(vals)
		if err != nil {
			t.Fatalf("marshal targets: %v", err)
		}
		return out
	}

	t.Run("create + get round trip", func(t *testing.T) {
		in := BudgetIn{
			Name: "  Dining  ", Period: "month",
			Targets: targets(map[string]any{fa.ID: 100.5, fu.ID: 200}),
		}
		created, err := bud.Create(ctx, aid, in, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if created.Name != "Dining" || created.Period != "month" || !created.Active {
			t.Fatalf("created: %+v", created)
		}
		if !jsonDocsEqual(created.Targets, in.Targets) {
			t.Fatalf("targets: want %s, got %s", in.Targets, created.Targets)
		}
		got, err := bud.Get(ctx, aid, created.ID, a.dek)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.ID != created.ID || got.Name != created.Name || got.Period != created.Period ||
			!got.Active || !got.CreatedAt.Equal(created.CreatedAt) || !got.UpdatedAt.Equal(created.UpdatedAt) {
			t.Fatalf("get: want %+v, got %+v", created, got)
		}
		if !jsonDocsEqual(got.Targets, in.Targets) {
			t.Fatalf("get targets: want %s, got %s", in.Targets, got.Targets)
		}
		if err := bud.Delete(ctx, aid, created.ID); err != nil {
			t.Fatalf("cleanup delete: %v", err)
		}
	})

	t.Run("create with explicit active false", func(t *testing.T) {
		off := false
		created, err := bud.Create(ctx, aid, BudgetIn{
			Name: "Paused", Period: "year", Active: &off,
			Targets: targets(map[string]any{fa.ID: 1}),
		}, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if created.Active {
			t.Fatalf("active must be false, got %+v", created)
		}
		if err := bud.Delete(ctx, aid, created.ID); err != nil {
			t.Fatalf("cleanup delete: %v", err)
		}
	})

	t.Run("validation", func(t *testing.T) {
		base := func() BudgetIn {
			return BudgetIn{
				Name: "Valid", Period: "quarter",
				Targets: targets(map[string]any{fa.ID: 50}),
			}
		}
		cases := []struct {
			name    string
			mut     func(*BudgetIn)
			wantErr error
		}{
			{"blank name", func(in *BudgetIn) { in.Name = "   " }, nil},
			{"name too long", func(in *BudgetIn) { in.Name = strings.Repeat("n", 256) }, nil},
			{"bad period", func(in *BudgetIn) { in.Period = "week" }, nil},
			{"targets not JSON", func(in *BudgetIn) { in.Targets = json.RawMessage(`{`) }, nil},
			{"targets an array", func(in *BudgetIn) { in.Targets = json.RawMessage(`[1]`) }, nil},
			{"targets a scalar", func(in *BudgetIn) { in.Targets = json.RawMessage(`5`) }, nil},
			{"targets empty object", func(in *BudgetIn) { in.Targets = json.RawMessage(`{}`) }, nil},
			{"target key not a uuid", func(in *BudgetIn) {
				in.Targets = targets(map[string]any{"not-a-uuid": 1})
			}, nil},
			{"target key is another user's category", func(in *BudgetIn) {
				in.Targets = targets(map[string]any{fb.ID: 1})
			}, ErrCategoryNotFound},
			{"target key is a stranger uuid", func(in *BudgetIn) {
				in.Targets = targets(map[string]any{zeroUUID: 1})
			}, ErrCategoryNotFound},
			{"target value negative", func(in *BudgetIn) {
				in.Targets = targets(map[string]any{fa.ID: -1})
			}, nil},
			{"target value a string", func(in *BudgetIn) {
				in.Targets = targets(map[string]any{fa.ID: "50"})
			}, nil},
			{"target value above the numeric(14,2) ceiling", func(in *BudgetIn) {
				in.Targets = targets(map[string]any{fa.ID: 1000000000000.0})
			}, nil},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				in := base()
				tc.mut(&in)
				_, err := bud.Create(ctx, aid, in, a.dek)
				if tc.wantErr != nil {
					if !errors.Is(err, tc.wantErr) {
						t.Fatalf("got %v, want %v", err, tc.wantErr)
					}
				} else if err == nil {
					t.Fatal("expected a validation error, got nil")
				}
			})
		}
		// The boundary value that DOES fit is accepted.
		ok, err := bud.Create(ctx, aid, BudgetIn{
			Name: "Boundary", Period: "year",
			Targets: targets(map[string]any{fu.ID: 999999999999.99}),
		}, a.dek)
		if err != nil {
			t.Fatalf("boundary target must be accepted: %v", err)
		}
		if err := bud.Delete(ctx, aid, ok.ID); err != nil {
			t.Fatalf("cleanup delete: %v", err)
		}
	})

	t.Run("update name, active, targets", func(t *testing.T) {
		created, err := bud.Create(ctx, aid, BudgetIn{
			Name: "Before", Period: "month",
			Targets: targets(map[string]any{fa.ID: 10}),
		}, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		off := false
		newTargets := targets(map[string]any{fa.ID: 10, fu.ID: 20})
		updated, err := bud.Update(ctx, aid, created.ID, BudgetPatch{
			Name: strPtr("After"), Active: &off,
			Targets: rawPtr(newTargets),
		}, a.dek)
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if updated.Name != "After" || updated.Period != "month" || updated.Active {
			t.Fatalf("updated: %+v", updated)
		}
		if !jsonDocsEqual(updated.Targets, targets(map[string]any{fa.ID: 10, fu.ID: 20})) {
			t.Fatalf("targets: %s", updated.Targets)
		}
		if !updated.UpdatedAt.After(created.UpdatedAt) {
			t.Fatalf("updated_at must move: %v -> %v", created.UpdatedAt, updated.UpdatedAt)
		}
		if err := bud.Delete(ctx, aid, created.ID); err != nil {
			t.Fatalf("cleanup delete: %v", err)
		}
	})

	t.Run("update no-op leaves updated_at put", func(t *testing.T) {
		created, err := bud.Create(ctx, aid, BudgetIn{
			Name: "Stable", Period: "quarter",
			Targets: targets(map[string]any{fa.ID: 5}),
		}, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		// Same name (re-trimmed), same active, byte-identical targets.
		sameTargets := targets(map[string]any{fa.ID: 5})
		got, err := bud.Update(ctx, aid, created.ID, BudgetPatch{
			Name: strPtr(" Stable "), Active: boolPtr(true),
			Targets: rawPtr(sameTargets),
		}, a.dek)
		if err != nil {
			t.Fatalf("no-op update: %v", err)
		}
		if !got.UpdatedAt.Equal(created.UpdatedAt) {
			t.Fatalf("no-op must not touch updated_at: %v -> %v", created.UpdatedAt, got.UpdatedAt)
		}
		if got.Name != "Stable" || !got.Active || !jsonDocsEqual(got.Targets, targets(map[string]any{fa.ID: 5})) {
			t.Fatalf("row must be unchanged: %+v", got)
		}
		if err := bud.Delete(ctx, aid, created.ID); err != nil {
			t.Fatalf("cleanup delete: %v", err)
		}
	})

	t.Run("per-user scoping", func(t *testing.T) {
		created, err := bud.Create(ctx, aid, BudgetIn{
			Name: "Mine", Period: "month",
			Targets: targets(map[string]any{fa.ID: 1}),
		}, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := bud.Get(ctx, bid, created.ID, b.dek); !errors.Is(err, ErrBudgetNotFound) {
			t.Fatalf("other user's get: got %v, want ErrBudgetNotFound", err)
		}
		off := false
		if _, err := bud.Update(ctx, bid, created.ID, BudgetPatch{Active: &off}, b.dek); !errors.Is(err, ErrBudgetNotFound) {
			t.Fatalf("other user's update: got %v, want ErrBudgetNotFound", err)
		}
		if err := bud.Delete(ctx, bid, created.ID); !errors.Is(err, ErrBudgetNotFound) {
			t.Fatalf("other user's delete: got %v, want ErrBudgetNotFound", err)
		}
		// The other user's create is scoped the same way: a target key
		// that is user A's category must be rejected for user B.
		if _, err := bud.Create(ctx, bid, BudgetIn{
			Name: "Sneaky", Period: "month",
			Targets: targets(map[string]any{fa.ID: 1}),
		}, b.dek); !errors.Is(err, ErrCategoryNotFound) {
			t.Fatalf("cross-user target key: got %v, want ErrCategoryNotFound", err)
		}
		// List isolation.
		lb, err := bud.List(ctx, bid, b.dek)
		if err != nil {
			t.Fatalf("list b: %v", err)
		}
		for _, x := range lb {
			if x.ID == created.ID {
				t.Fatal("other user's budget leaked into the list")
			}
		}
		// The row is intact.
		if _, err := bud.Get(ctx, aid, created.ID, a.dek); err != nil {
			t.Fatalf("row must be intact: %v", err)
		}
		if err := bud.Delete(ctx, aid, created.ID); err != nil {
			t.Fatalf("cleanup delete: %v", err)
		}
	})

	t.Run("list + delete", func(t *testing.T) {
		ids := map[string]bool{}
		for _, name := range []string{"L1", "L2"} {
			created, err := bud.Create(ctx, aid, BudgetIn{
				Name: name, Period: "year",
				Targets: targets(map[string]any{fu.ID: 2}),
			}, a.dek)
			if err != nil {
				t.Fatalf("create %s: %v", name, err)
			}
			ids[created.ID] = true
		}
		got, err := bud.List(ctx, aid, a.dek)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("list: got %d rows, want 2", len(got))
		}
		for _, x := range got {
			if !ids[x.ID] {
				t.Fatalf("unexpected row %s in list", x.ID)
			}
		}
		if err := bud.Delete(ctx, aid, got[0].ID); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := bud.Get(ctx, aid, got[0].ID, a.dek); !errors.Is(err, ErrBudgetNotFound) {
			t.Fatalf("deleted get: got %v, want ErrBudgetNotFound", err)
		}
		if err := bud.Delete(ctx, aid, got[1].ID); err != nil {
			t.Fatalf("cleanup delete: %v", err)
		}
	})
}

// TestBudgetActiveTargets covers the summary's narrow select: only the
// ACTIVE budgets of the requested period come back, names are never
// decrypted (not selected at all), targets are decrypted, order is
// created_at, id, and foreign users' rows never leak. One fresh
// testcontainer (the per-Test-fn convention).
func TestBudgetActiveTargets(t *testing.T) {
	env := newTestEnv(t, "budx-a", "budx-b")
	cat := NewCategoryStore(env.pool)
	bud := NewBudgetStore(env.pool)
	a := env.user(t, "budx-a")
	b := env.user(t, "budx-b")
	ctx := env.ctx
	aid, bid := a.row.ID, b.row.ID

	fa, err := cat.Create(ctx, aid, CategoryIn{Name: "Food"}, a.dek)
	if err != nil {
		t.Fatalf("create category: %v", err)
	}
	fb, err := cat.Create(ctx, bid, CategoryIn{Name: "Food"}, b.dek)
	if err != nil {
		t.Fatalf("create other user's category: %v", err)
	}
	targets := func(vals map[string]any) json.RawMessage {
		t.Helper()
		out, err := json.Marshal(vals)
		if err != nil {
			t.Fatalf("marshal targets: %v", err)
		}
		return out
	}

	monthA, err := bud.Create(ctx, aid, BudgetIn{Name: "Month A", Period: "month", Targets: targets(map[string]any{fa.ID: 10})}, a.dek)
	if err != nil {
		t.Fatalf("create month A: %v", err)
	}
	monthA2, err := bud.Create(ctx, aid, BudgetIn{Name: "Month A2", Period: "month", Targets: targets(map[string]any{fa.ID: 5})}, a.dek)
	if err != nil {
		t.Fatalf("create month A2: %v", err)
	}
	off := false
	monthOff, err := bud.Create(ctx, aid, BudgetIn{Name: "Month Off", Period: "month", Active: &off, Targets: targets(map[string]any{fa.ID: 1})}, a.dek)
	if err != nil {
		t.Fatalf("create month off: %v", err)
	}
	quarterA, err := bud.Create(ctx, aid, BudgetIn{Name: "Quarter A", Period: "quarter", Targets: targets(map[string]any{fa.ID: 7})}, a.dek)
	if err != nil {
		t.Fatalf("create quarter A: %v", err)
	}
	yearA, err := bud.Create(ctx, aid, BudgetIn{Name: "Year A", Period: "year", Targets: targets(map[string]any{fa.ID: 3})}, a.dek)
	if err != nil {
		t.Fatalf("create year A: %v", err)
	}
	monthB, err := bud.Create(ctx, bid, BudgetIn{Name: "Month B", Period: "month", Targets: targets(map[string]any{fb.ID: 9})}, b.dek)
	if err != nil {
		t.Fatalf("create month B: %v", err)
	}
	cleanup := func(u, id string) {
		t.Helper()
		if err := bud.Delete(ctx, u, id); err != nil {
			t.Fatalf("cleanup delete %s: %v", id, err)
		}
	}

	// Month: the two active month budgets, in created_at, id order;
	// the inactive one, the other periods, and user b's row are all
	// filtered out in SQL.
	got, err := bud.ActiveTargets(ctx, aid, "month", a.dek)
	if err != nil {
		t.Fatalf("active targets month: %v", err)
	}
	wantIDs := []string{monthA.ID, monthA2.ID}
	if len(got) != len(wantIDs) {
		t.Fatalf("month: got %d rows %+v, want %d", len(got), got, len(wantIDs))
	}
	for i, id := range wantIDs {
		if got[i].ID != id {
			t.Fatalf("month row %d = %s, want %s", i, got[i].ID, id)
		}
	}
	if !jsonDocsEqual(got[0].Targets, monthA.Targets) || !jsonDocsEqual(got[1].Targets, monthA2.Targets) {
		t.Fatalf("month targets: got %+v", got)
	}

	// Quarter: only the quarter row.
	got, err = bud.ActiveTargets(ctx, aid, "quarter", a.dek)
	if err != nil {
		t.Fatalf("active targets quarter: %v", err)
	}
	if len(got) != 1 || got[0].ID != quarterA.ID {
		t.Fatalf("quarter: got %+v, want the single %s", got, quarterA.ID)
	}
	if !jsonDocsEqual(got[0].Targets, quarterA.Targets) {
		t.Fatalf("quarter targets: got %s, want %s", got[0].Targets, quarterA.Targets)
	}

	// Year: only the year row.
	got, err = bud.ActiveTargets(ctx, aid, "year", a.dek)
	if err != nil {
		t.Fatalf("active targets year: %v", err)
	}
	if len(got) != 1 || got[0].ID != yearA.ID {
		t.Fatalf("year: got %+v, want the single %s", got, yearA.ID)
	}

	// User b's own month budget: isolated from user a's set.
	got, err = bud.ActiveTargets(ctx, bid, "month", b.dek)
	if err != nil {
		t.Fatalf("active targets user b: %v", err)
	}
	if len(got) != 1 || got[0].ID != monthB.ID {
		t.Fatalf("user b month: got %+v, want the single %s", got, monthB.ID)
	}

	// Deactivating the row removes it from the set (the filter is SQL,
	// not Go).
	if _, err := bud.Update(ctx, aid, monthA2.ID, BudgetPatch{Active: &off}, a.dek); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	got, err = bud.ActiveTargets(ctx, aid, "month", a.dek)
	if err != nil {
		t.Fatalf("active targets after deactivate: %v", err)
	}
	if len(got) != 1 || got[0].ID != monthA.ID {
		t.Fatalf("after deactivate: got %+v, want the single %s", got, monthA.ID)
	}

	cleanup(aid, monthA.ID)
	got, err = bud.ActiveTargets(ctx, aid, "month", a.dek)
	if err != nil {
		t.Fatalf("active targets empty: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty: got %+v, want none", got)
	}
	cleanup(aid, monthA2.ID)
	cleanup(aid, monthOff.ID)
	cleanup(aid, quarterA.ID)
	cleanup(aid, yearA.ID)
	cleanup(bid, monthB.ID)
}

func TestBudgetValidateTargetsPure(t *testing.T) {
	// The shape rules (no DB): uuid keys, numeric non-negative values.
	// The keys are syntactically valid uuids — ownership (whether the
	// key is the caller's category) is the DB half, tested in
	// TestBudgetStore.
	uk := "3f2c1a9e-6b4d-4c8a-9e2f-1d5b7a3c8e0f"
	cases := []struct {
		name    string
		targets string
		wantErr bool
	}{
		{"valid object", `{"` + uk + `": 1}`, false},
		{"zero value ok", `{"` + uk + `": 0}`, false},
		{"fractional ok", `{"` + uk + `": 10.25}`, false},
		{"not JSON", `{`, true},
		{"array", `[1]`, true},
		{"scalar", `"x"`, true},
		{"empty object", `{}`, true},
		{"bad key", `{"nope": 1}`, true},
		{"short key", `{"a-b": 1}`, true},
		{"string value", `{"` + uk + `": "1"}`, true},
		{"null value", `{"` + uk + `": null}`, true},
		{"bool value", `{"` + uk + `": true}`, true},
		{"negative", `{"` + uk + `": -0.01}`, true},
		{"above ceiling", `{"` + uk + `": 1e12}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keys, err := validateTargetsShape(json.RawMessage(tc.targets))
			if tc.wantErr && err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("got %v, want nil", err)
			}
			if !tc.wantErr && (len(keys) != 1 || keys[0] != uk) {
				t.Fatalf("keys: got %v, want [%s]", keys, uk)
			}
		})
	}
}

func TestUUIDOK(t *testing.T) {
	ok := "3f2c1a9e-6b4d-4c8a-9e2f-1d5b7a3c8e0f"
	if !uuidOK(ok) {
		t.Fatalf("must accept %q", ok)
	}
	if !uuidOK(strings.ToUpper(ok)) {
		t.Fatalf("must accept uppercase hex %q", strings.ToUpper(ok))
	}
	for _, bad := range []string{
		"",
		"3f2c1a9e-6b4d-4c8a-9e2f-1d5b7a3c8e0",   // 35
		"3f2c1a9e-6b4d-4c8a-9e2f-1d5b7a3c8e0ff", // 37
		"3f2c1a9e6b4d4c8a9e2f1d5b7a3c8e0f",      // no dashes
		"3f2c1a9e_6b4d-4c8a-9e2f-1d5b7a3c8e0f",  // bad separator
		"3f2c1a9e-6b4d-4c8a-9e2f-1d5b7a3c8e0g",  // non-hex
	} {
		if uuidOK(bad) {
			t.Fatalf("must reject %q", bad)
		}
	}
}

func TestBudgetPeriodsVocabulary(t *testing.T) {
	if !reflect.DeepEqual(BudgetPeriods, []string{"month", "quarter", "year"}) {
		t.Fatalf("BudgetPeriods must mirror the 0003 CHECK, got %v", BudgetPeriods)
	}
}

// Schema store tests (plan step 2.5b): one shared postgres:18
// testcontainer (newTestEnv) with subtests covering builtin seeding +
// read-only, custom CRUD round trips, validation, and per-user
// scoping. The concurrency guard — the advisory lock, which is the
// ONLY guard (no unique constraint on builtin names) — is proven in
// TestEnsureBuiltinsRace with a second, fresh env.
package repo

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// specEqual compares two specs as JSON VALUES, not bytes: postgres
// jsonb normalizes whitespace on storage, so a spec round-trips with
// different formatting ({"a":1} comes back {"a": 1}).
func specEqual(a, b json.RawMessage) bool {
	av, bv := map[string]any{}, map[string]any{}
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

func TestSchemaStore(t *testing.T) {
	env := newTestEnv(t, "sch-a", "sch-b")
	store := NewSchemaStore(env.pool)
	if store == nil {
		t.Fatal("NewSchemaStore(nil-check): non-nil pool must give a store")
	}
	a := env.user(t, "sch-a")
	b := env.user(t, "sch-b")
	ctx := env.ctx
	aid, bid := a.row.ID, b.row.ID

	t.Run("ensure builtins idempotent, visible to all users", func(t *testing.T) {
		n, err := store.EnsureBuiltins(ctx)
		if err != nil {
			t.Fatalf("ensure: %v", err)
		}
		if n != 3 {
			t.Fatalf("first ensure: want 3 inserted, got %d", n)
		}
		// Re-ensuring is a no-op: the (name, kind) pairs are all present.
		if n, err = store.EnsureBuiltins(ctx); err != nil || n != 0 {
			t.Fatalf("re-ensure: want 0 inserted, got %d (err %v)", n, err)
		}

		list, err := store.List(ctx, aid, a.dek)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(list) != 3 {
			t.Fatalf("rows: want 3 builtins, got %d", len(list))
		}
		want := map[string]string{"Generic CSV": "csv", "OFX Statement": "ofx", "Generic JSON": "json"}
		var ids []string
		for _, s := range list {
			if !s.Builtin {
				t.Fatalf("unexpected custom in freshly seeded list: %+v", s)
			}
			if got, ok := want[s.Name]; !ok || got != s.Kind {
				t.Fatalf("builtin: want %q kind %q, got %q kind %q", s.Name, want[s.Name], s.Name, s.Kind)
			}
			if string(s.Spec) != "{}" {
				t.Fatalf("builtin %q spec: want {}, got %s", s.Name, s.Spec)
			}
			ids = append(ids, s.ID)
		}
		// The SAME 3 rows are visible to the other user (builtins are
		// global; the ordering is deterministic for a fixed row set).
		lb, err := store.List(ctx, bid, b.dek)
		if err != nil {
			t.Fatalf("list b: %v", err)
		}
		if len(lb) != 3 {
			t.Fatalf("b: want 3 rows, got %d", len(lb))
		}
		for i, s := range lb {
			if s.ID != ids[i] {
				t.Fatalf("b sees different builtins: %+v", lb)
			}
		}
	})

	t.Run("builtins are read-only", func(t *testing.T) {
		list, err := store.List(ctx, aid, a.dek)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		var builtinID string
		for _, s := range list {
			if s.Builtin {
				builtinID = s.ID
				break
			}
		}
		if builtinID == "" {
			t.Fatal("no builtin found")
		}
		if _, err := store.Update(ctx, aid, builtinID, SchemaPatch{Name: strPtr("Hacked")}, a.dek); !errors.Is(err, ErrBuiltinReadOnly) {
			t.Fatalf("update builtin: want ErrBuiltinReadOnly, got %v", err)
		}
		if err := store.Delete(ctx, aid, builtinID); !errors.Is(err, ErrBuiltinReadOnly) {
			t.Fatalf("delete builtin: want ErrBuiltinReadOnly, got %v", err)
		}
		// Read-only for EVERY user, not just the one who seeded them.
		if _, err := store.Update(ctx, bid, builtinID, SchemaPatch{Name: strPtr("Hacked")}, b.dek); !errors.Is(err, ErrBuiltinReadOnly) {
			t.Fatalf("b updates builtin: want ErrBuiltinReadOnly, got %v", err)
		}
		if got, err := store.Get(ctx, aid, builtinID, a.dek); err != nil || got.Name == "Hacked" {
			t.Fatalf("builtin must be untouched: %+v (err %v)", got, err)
		}
	})

	t.Run("custom crud round trip", func(t *testing.T) {
		spec := json.RawMessage(`{"columns":{"date":"1"}}`)
		sc, err := store.Create(ctx, aid, SchemaIn{Name: "  A-import  ", Kind: "csv", Spec: spec}, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if sc.Name != "A-import" || sc.Kind != "csv" || sc.Builtin || string(sc.Spec) != `{"columns":{"date":"1"}}` {
			t.Fatalf("created: %+v", sc)
		}
		got, err := store.Get(ctx, aid, sc.ID, a.dek)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		// specEqual: got came back from the DB, which normalizes jsonb
		// whitespace; sc is Create's in-memory return.
		if got.Name != sc.Name || got.Kind != sc.Kind || got.Builtin != sc.Builtin || !specEqual(got.Spec, sc.Spec) {
			t.Fatalf("get: want %+v, got %+v", sc, got)
		}

		// List: builtins first, the user's customs after.
		list, err := store.List(ctx, aid, a.dek)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(list) != 4 || list[3].ID != sc.ID {
			t.Fatalf("list: want 3 builtins then the custom, got %+v", list)
		}

		// Update name + spec. kind is not in SchemaPatch — it is
		// immutable (the importer compiles against it).
		spec2 := json.RawMessage(`{"columns":{"date":"2"}}`)
		updated, err := store.Update(ctx, aid, sc.ID, SchemaPatch{Name: strPtr("A-import v2"), Spec: &spec2}, a.dek)
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		// updated comes back through Get: DB-normalized jsonb, so compare
		// the spec as a JSON value, not bytes.
		if updated.Name != "A-import v2" || !specEqual(updated.Spec, json.RawMessage(`{"columns":{"date":"2"}}`)) || updated.Kind != "csv" {
			t.Fatalf("updated: %+v", updated)
		}

		// A no-op patch returns the row without an UPDATE.
		if _, err := store.Update(ctx, aid, sc.ID, SchemaPatch{}, a.dek); err != nil {
			t.Fatalf("no-op update: %v", err)
		}

		if err := store.Delete(ctx, aid, sc.ID); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := store.Get(ctx, aid, sc.ID, a.dek); !errors.Is(err, ErrSchemaNotFound) {
			t.Fatalf("get after delete: want ErrSchemaNotFound, got %v", err)
		}
	})

	t.Run("validation, rejected before the DB", func(t *testing.T) {
		bad := []struct {
			name string
			in   SchemaIn
		}{
			{"bad kind", SchemaIn{Name: "V-Kind", Kind: "xml", Spec: json.RawMessage(`{}`)}},
			{"invalid json spec", SchemaIn{Name: "V-J1", Kind: "csv", Spec: json.RawMessage(`{nope`)}},
			{"array spec", SchemaIn{Name: "V-J2", Kind: "csv", Spec: json.RawMessage(`[1,2]`)}},
			{"string spec", SchemaIn{Name: "V-J3", Kind: "csv", Spec: json.RawMessage(`"str"`)}},
			{"number spec", SchemaIn{Name: "V-J4", Kind: "csv", Spec: json.RawMessage(`42`)}},
			{"empty spec", SchemaIn{Name: "V-J5", Kind: "csv", Spec: json.RawMessage{}}},
			{"blank name", SchemaIn{Name: "   ", Kind: "csv", Spec: json.RawMessage(`{}`)}},
			{"256-char name", SchemaIn{Name: strings.Repeat("x", 256), Kind: "csv", Spec: json.RawMessage(`{}`)}},
		}
		for _, tc := range bad {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := store.Create(ctx, aid, tc.in, a.dek); err == nil {
					t.Fatal("want rejection, got nil")
				}
			})
		}
		// Rejected on update too.
		v, err := store.Create(ctx, aid, SchemaIn{Name: "V-Upd", Kind: "csv", Spec: json.RawMessage(`{}`)}, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		badSpec := json.RawMessage(`[1,2]`)
		if _, err := store.Update(ctx, aid, v.ID, SchemaPatch{Spec: &badSpec}, a.dek); err == nil {
			t.Fatal("bad spec on update: want rejection, got nil")
		}
		_ = store.Delete(ctx, aid, v.ID)
	})

	t.Run("per-user scoping", func(t *testing.T) {
		mine, err := store.Create(ctx, aid, SchemaIn{Name: "Private", Kind: "json", Spec: json.RawMessage(`{}`)}, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		// User b must not see, update, or delete a schema owned by a —
		// and none of b's attempts may touch the row. "No such id" and
		// "somebody else's custom" are indistinguishable.
		if _, err := store.Get(ctx, bid, mine.ID, b.dek); !errors.Is(err, ErrSchemaNotFound) {
			t.Fatalf("b reads a's schema: want ErrSchemaNotFound, got %v", err)
		}
		if _, err := store.Update(ctx, bid, mine.ID, SchemaPatch{Name: strPtr("Stolen")}, b.dek); !errors.Is(err, ErrSchemaNotFound) {
			t.Fatalf("b updates a's schema: want ErrSchemaNotFound, got %v", err)
		}
		if err := store.Delete(ctx, bid, mine.ID); !errors.Is(err, ErrSchemaNotFound) {
			t.Fatalf("b deletes a's schema: want ErrSchemaNotFound, got %v", err)
		}
		if got, err := store.Get(ctx, aid, mine.ID, a.dek); err != nil || got.Name != "Private" {
			t.Fatalf("row must survive b's attempts: %+v (err %v)", got, err)
		}
		list, err := store.List(ctx, bid, b.dek)
		if err != nil {
			t.Fatalf("list b: %v", err)
		}
		for _, s := range list {
			if s.ID == mine.ID {
				t.Fatal("b's list contains a's schema")
			}
			if !s.Builtin {
				t.Fatalf("b sees a non-builtin schema: %+v", s)
			}
		}
	})
}

// TestEnsureBuiltinsRace (own env: the race only exists on an empty
// database) proves the advisory lock: two concurrent first bootstraps
// must insert exactly the 3 builtins. Without the lock the
// check-then-insert could race into 6 rows — the table has no unique
// constraint on builtin names, so the lock IS the concurrency guard.
func TestEnsureBuiltinsRace(t *testing.T) {
	env := newTestEnv(t, "race-a")
	store := NewSchemaStore(env.pool)
	if store == nil {
		t.Fatal("NewSchemaStore(nil-check): non-nil pool must give a store")
	}
	ctx := env.ctx
	a := env.user(t, "race-a")

	const goroutines = 2
	counts := make([]int, goroutines)
	errs := make([]error, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			counts[i], errs[i] = store.EnsureBuiltins(ctx)
		}(i)
	}
	wg.Wait()

	total := 0
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		total += counts[i]
	}
	if total != 3 {
		t.Fatalf("concurrent seed: want 3 rows inserted in total, got %d (lock race?)", total)
	}
	list, err := store.List(ctx, a.row.ID, a.dek)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("builtins after race: want exactly 3, got %d", len(list))
	}
}

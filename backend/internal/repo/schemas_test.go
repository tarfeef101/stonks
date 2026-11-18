// Schema store tests (plan step 2.5b): one shared postgres:18
// testcontainer (newTestEnv) with subtests covering the custom CRUD
// round trips, validation, and per-user scoping. Since 0018 the table
// is customs only (the global builtins are deleted; the code builtins
// in internal/presets are the sole preset source, D-029), so there is
// no seeding surface to test here.
package repo

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
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

	t.Run("custom crud round trip", func(t *testing.T) {
		spec := json.RawMessage(`{"columns":{"date":"1"}}`)
		sc, err := store.Create(ctx, aid, SchemaIn{Name: "  A-import  ", Kind: "csv", Spec: spec}, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if sc.Name != "A-import" || sc.Kind != "csv" || string(sc.Spec) != `{"columns":{"date":"1"}}` {
			t.Fatalf("created: %+v", sc)
		}
		got, err := store.Get(ctx, aid, sc.ID, a.dek)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		// specEqual: got came back from the DB, which normalizes jsonb
		// whitespace; sc is Create's in-memory return.
		if got.Name != sc.Name || got.Kind != sc.Kind || !specEqual(got.Spec, sc.Spec) {
			t.Fatalf("get: want %+v, got %+v", sc, got)
		}

		// List: the user's customs only (customs only since 0018), in
		// created_at, id order.
		list, err := store.List(ctx, aid, a.dek)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(list) != 1 || list[0].ID != sc.ID {
			t.Fatalf("list: want the single custom, got %+v", list)
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
		}
	})
}

// Dashboard card store tests (plan step 6.5, migration 0008): one
// shared postgres:18 testcontainer (newTestEnv) with subtests covering
// the CRUD round trip (trimmed name + explore_state decrypt), sort_order
// ordering, the MAX(sort_order)+1 rule over REMAINING rows after a
// delete, the no-op update rule, SetOrder (success + rollback), the
// not-found sentinels, and per-user isolation.
package repo

import (
	"errors"
	"strings"
	"testing"
)

func TestDashboardCardStore(t *testing.T) {
	env := newTestEnv(t, "dca-a", "dca-b")
	dcs := NewDashboardCardStore(env.pool)
	if dcs == nil {
		t.Fatal("non-nil pool must give non-nil store")
	}
	if NewDashboardCardStore(nil) != nil {
		t.Fatal("nil pool must give nil store")
	}
	a := env.user(t, "dca-a")
	b := env.user(t, "dca-b")
	ctx := env.ctx
	aid, bid := a.row.ID, b.row.ID

	t.Run("create + get round trip", func(t *testing.T) {
		created, err := dcs.Create(ctx, aid, DashboardCardIn{
			Name: "  Saved view  ", ExploreState: "range=30d&q=coffee",
		}, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if created.Name != "Saved view" || created.ExploreState != "range=30d&q=coffee" || created.SortOrder != 1 {
			t.Fatalf("created: %+v", created)
		}
		got, err := dcs.Get(ctx, aid, created.ID, a.dek)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.ID != created.ID || got.Name != created.Name || got.ExploreState != created.ExploreState ||
			got.SortOrder != created.SortOrder || !got.CreatedAt.Equal(created.CreatedAt) || !got.UpdatedAt.Equal(created.UpdatedAt) {
			t.Fatalf("get: want %+v, got %+v", created, got)
		}
		// "" IS the default (all-time) view and must round-trip as "".
		dflt, err := dcs.Create(ctx, aid, DashboardCardIn{Name: "All"}, a.dek)
		if err != nil {
			t.Fatalf("create default: %v", err)
		}
		if dflt.ExploreState != "" || dflt.SortOrder != 2 {
			t.Fatalf("default card: %+v", dflt)
		}
		if err := dcs.Delete(ctx, aid, created.ID); err != nil {
			t.Fatalf("cleanup delete: %v", err)
		}
		if err := dcs.Delete(ctx, aid, dflt.ID); err != nil {
			t.Fatalf("cleanup delete: %v", err)
		}
	})

	t.Run("validation", func(t *testing.T) {
		cases := []struct {
			name string
			in   DashboardCardIn
		}{
			{"blank name", DashboardCardIn{Name: "   ", ExploreState: "x"}},
			{"name too long", DashboardCardIn{Name: strings.Repeat("n", 256)}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := dcs.Create(ctx, aid, tc.in, a.dek); err == nil {
					t.Fatal("expected a validation error, got nil")
				}
			})
		}
		// The 255-character boundary is accepted. The explore_state
		// length cap is the SERVER's rule, not the repo's: a 1024-char
		// state seals and round-trips fine here.
		ok, err := dcs.Create(ctx, aid, DashboardCardIn{
			Name: strings.Repeat("n", 255), ExploreState: strings.Repeat("q", 1024),
		}, a.dek)
		if err != nil {
			t.Fatalf("boundary row must be accepted: %v", err)
		}
		if err := dcs.Delete(ctx, aid, ok.ID); err != nil {
			t.Fatalf("cleanup delete: %v", err)
		}
	})

	t.Run("sort order: list, max+1 after delete, setorder", func(t *testing.T) {
		var ids []string
		for _, name := range []string{"C1", "C2", "C3"} {
			created, err := dcs.Create(ctx, aid, DashboardCardIn{Name: name}, a.dek)
			if err != nil {
				t.Fatalf("create %s: %v", name, err)
			}
			ids = append(ids, created.ID)
		}
		got, err := dcs.List(ctx, aid, a.dek)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("list: got %d rows, want 3", len(got))
		}
		for i, id := range ids {
			if got[i].ID != id || got[i].SortOrder != i+1 {
				t.Fatalf("list order: %+v", got)
			}
		}
		// Delete the first row; the next create takes MAX(sort_order)+1
		// over REMAINING rows: C3 holds 3, so the new row gets 4.
		if err := dcs.Delete(ctx, aid, ids[0]); err != nil {
			t.Fatalf("delete c1: %v", err)
		}
		nxt, err := dcs.Create(ctx, aid, DashboardCardIn{Name: "C4"}, a.dek)
		if err != nil {
			t.Fatalf("create c4: %v", err)
		}
		if nxt.SortOrder != 4 {
			t.Fatalf("next sort_order: got %d, want 4 (remaining rows hold 2 and 3)", nxt.SortOrder)
		}
		// SetOrder rewrites all three rows: C3, C2, C4.
		if err := dcs.SetOrder(ctx, aid, []string{ids[2], ids[1], nxt.ID}); err != nil {
			t.Fatalf("setorder: %v", err)
		}
		got, err = dcs.List(ctx, aid, a.dek)
		if err != nil {
			t.Fatalf("list after setorder: %v", err)
		}
		for i, id := range []string{ids[2], ids[1], nxt.ID} {
			if got[i].ID != id || got[i].SortOrder != i+1 {
				t.Fatalf("post-setorder list: %+v", got)
			}
		}
		for _, id := range append(ids[1:], nxt.ID) {
			if err := dcs.Delete(ctx, aid, id); err != nil {
				t.Fatalf("cleanup delete: %v", err)
			}
		}
	})

	t.Run("update name and explore state", func(t *testing.T) {
		created, err := dcs.Create(ctx, aid, DashboardCardIn{Name: "Before"}, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		updated, err := dcs.Update(ctx, aid, created.ID, DashboardCardPatch{
			Name: strPtr("  After  "), ExploreState: strPtr("range=7d"),
		}, a.dek)
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if updated.Name != "After" || updated.ExploreState != "range=7d" || updated.SortOrder != 1 {
			t.Fatalf("updated: %+v", updated)
		}
		if !updated.UpdatedAt.After(created.UpdatedAt) {
			t.Fatalf("updated_at must move: %v -> %v", created.UpdatedAt, updated.UpdatedAt)
		}
		// Partial patch: only explore_state changes (to the "" default),
		// the name stays.
		partial, err := dcs.Update(ctx, aid, created.ID, DashboardCardPatch{ExploreState: strPtr("")}, a.dek)
		if err != nil {
			t.Fatalf("partial update: %v", err)
		}
		if partial.Name != "After" || partial.ExploreState != "" {
			t.Fatalf("partial: %+v", partial)
		}
		if err := dcs.Delete(ctx, aid, created.ID); err != nil {
			t.Fatalf("cleanup delete: %v", err)
		}
	})

	t.Run("update no-op leaves updated_at put", func(t *testing.T) {
		created, err := dcs.Create(ctx, aid, DashboardCardIn{
			Name: "Stable", ExploreState: "range=90d",
		}, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		// Same name (re-trimmed) and byte-identical explore state.
		got, err := dcs.Update(ctx, aid, created.ID, DashboardCardPatch{
			Name: strPtr(" Stable "), ExploreState: strPtr("range=90d"),
		}, a.dek)
		if err != nil {
			t.Fatalf("no-op update: %v", err)
		}
		if !got.UpdatedAt.Equal(created.UpdatedAt) {
			t.Fatalf("no-op must not touch updated_at: %v -> %v", created.UpdatedAt, got.UpdatedAt)
		}
		if got.Name != "Stable" || got.ExploreState != "range=90d" {
			t.Fatalf("row must be unchanged: %+v", got)
		}
		if err := dcs.Delete(ctx, aid, created.ID); err != nil {
			t.Fatalf("cleanup delete: %v", err)
		}
	})

	t.Run("setorder rejects foreign and unknown ids and rolls back", func(t *testing.T) {
		ca, err := dcs.Create(ctx, aid, DashboardCardIn{Name: "A1"}, a.dek)
		if err != nil {
			t.Fatalf("create a: %v", err)
		}
		cb, err := dcs.Create(ctx, bid, DashboardCardIn{Name: "B1"}, b.dek)
		if err != nil {
			t.Fatalf("create b: %v", err)
		}
		if err := dcs.SetOrder(ctx, aid, []string{ca.ID, cb.ID}); !errors.Is(err, ErrDashboardCardNotFound) {
			t.Fatalf("foreign id: got %v, want ErrDashboardCardNotFound", err)
		}
		if err := dcs.SetOrder(ctx, aid, []string{ca.ID, zeroUUID}); !errors.Is(err, ErrDashboardCardNotFound) {
			t.Fatalf("unknown id: got %v, want ErrDashboardCardNotFound", err)
		}
		// Rollback: A1 keeps its original sort_order.
		got, err := dcs.Get(ctx, aid, ca.ID, a.dek)
		if err != nil {
			t.Fatalf("get after failed setorder: %v", err)
		}
		if got.SortOrder != 1 {
			t.Fatalf("failed setorder must roll back: sort_order %d, want 1", got.SortOrder)
		}
		if err := dcs.Delete(ctx, aid, ca.ID); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
		if err := dcs.Delete(ctx, bid, cb.ID); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
	})

	t.Run("not-found sentinels", func(t *testing.T) {
		if _, err := dcs.Get(ctx, aid, zeroUUID, a.dek); !errors.Is(err, ErrDashboardCardNotFound) {
			t.Fatalf("get: got %v, want ErrDashboardCardNotFound", err)
		}
		if _, err := dcs.Update(ctx, aid, zeroUUID, DashboardCardPatch{Name: strPtr("X")}, a.dek); !errors.Is(err, ErrDashboardCardNotFound) {
			t.Fatalf("update: got %v, want ErrDashboardCardNotFound", err)
		}
		if err := dcs.Delete(ctx, aid, zeroUUID); !errors.Is(err, ErrDashboardCardNotFound) {
			t.Fatalf("delete: got %v, want ErrDashboardCardNotFound", err)
		}
	})

	t.Run("per-user scoping", func(t *testing.T) {
		created, err := dcs.Create(ctx, aid, DashboardCardIn{Name: "Mine", ExploreState: "range=30d"}, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := dcs.Get(ctx, bid, created.ID, b.dek); !errors.Is(err, ErrDashboardCardNotFound) {
			t.Fatalf("other user's get: got %v, want ErrDashboardCardNotFound", err)
		}
		if _, err := dcs.Update(ctx, bid, created.ID, DashboardCardPatch{Name: strPtr("Stolen")}, b.dek); !errors.Is(err, ErrDashboardCardNotFound) {
			t.Fatalf("other user's update: got %v, want ErrDashboardCardNotFound", err)
		}
		if err := dcs.Delete(ctx, bid, created.ID); !errors.Is(err, ErrDashboardCardNotFound) {
			t.Fatalf("other user's delete: got %v, want ErrDashboardCardNotFound", err)
		}
		lb, err := dcs.List(ctx, bid, b.dek)
		if err != nil {
			t.Fatalf("list b: %v", err)
		}
		for _, x := range lb {
			if x.ID == created.ID {
				t.Fatal("other user's card leaked into the list")
			}
		}
		if _, err := dcs.Get(ctx, aid, created.ID, a.dek); err != nil {
			t.Fatalf("row must be intact: %v", err)
		}
		if err := dcs.Delete(ctx, aid, created.ID); err != nil {
			t.Fatalf("cleanup delete: %v", err)
		}
	})
}

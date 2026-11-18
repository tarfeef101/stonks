// Dashboard repository tests (round 6 rework, migration 0013): the
// narrow WidgetIDs select the set-order endpoint uses instead of the
// full (decrypting) List, with per-user isolation. One fresh
// testcontainer per Test function (the per-Test-fn convention).
package repo

import (
	"errors"
	"testing"
)

// TestDashboardWidgetIDs covers WidgetIDs: the dashboard's widget ids
// come back in sort_order, id order with no DEK involved at all (the
// method never sees a key), an empty dashboard yields an empty (non-nil)
// result, deleting a widget or the dashboard (cascading its widgets)
// shrinks the set, and a dashboard id that is not the caller's is
// ErrDashboardNotFound (existence is not disclosed).
func TestDashboardWidgetIDs(t *testing.T) {
	env := newTestEnv(t, "dsh-a", "dsh-b")
	dash := NewDashboardStore(env.pool)
	wid := NewDashboardWidgetStore(env.pool)
	a := env.user(t, "dsh-a")
	b := env.user(t, "dsh-b")
	ctx := env.ctx
	aid, bid := a.row.ID, b.row.ID

	// User a: a dashboard with two widgets, created in order so the
	// append rule (sort_order = MAX+1) gives them sort order 1, 2.
	d, err := dash.Create(ctx, aid, DashboardIn{Name: "Main"}, a.dek)
	if err != nil {
		t.Fatalf("create dashboard: %v", err)
	}
	w1, err := wid.Create(ctx, aid, d.ID, DashboardWidgetIn{Name: "W1", Kind: "explore", Content: ""}, a.dek)
	if err != nil {
		t.Fatalf("create widget 1: %v", err)
	}
	w2, err := wid.Create(ctx, aid, d.ID, DashboardWidgetIn{Name: "W2", Kind: "markdown", Content: "# hi"}, a.dek)
	if err != nil {
		t.Fatalf("create widget 2: %v", err)
	}

	got, err := wid.WidgetIDs(ctx, aid, d.ID)
	if err != nil {
		t.Fatalf("widget ids: %v", err)
	}
	if len(got) != 2 || got[0] != w1.ID || got[1] != w2.ID {
		t.Fatalf("widget ids: got %v, want [%s %s]", got, w1.ID, w2.ID)
	}

	// A dashboard id that is not the caller's is not-found.
	if _, err := wid.WidgetIDs(ctx, bid, d.ID); !errors.Is(err, ErrDashboardNotFound) {
		t.Fatalf("foreign dashboard: got %v, want ErrDashboardNotFound", err)
	}
	// A plain unknown id is not-found too.
	if _, err := wid.WidgetIDs(ctx, aid, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrDashboardNotFound) {
		t.Fatalf("unknown dashboard: got %v, want ErrDashboardNotFound", err)
	}

	// User a: an empty dashboard yields an empty (non-nil) result.
	empty, err := dash.Create(ctx, aid, DashboardIn{Name: "Empty"}, a.dek)
	if err != nil {
		t.Fatalf("create empty dashboard: %v", err)
	}
	got, err = wid.WidgetIDs(ctx, aid, empty.ID)
	if err != nil {
		t.Fatalf("widget ids empty dashboard: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("empty dashboard: got %v, want an empty non-nil slice", got)
	}

	// Deleting a widget shrinks the set.
	if err := wid.Delete(ctx, aid, d.ID, w2.ID); err != nil {
		t.Fatalf("delete widget: %v", err)
	}
	got, err = wid.WidgetIDs(ctx, aid, d.ID)
	if err != nil {
		t.Fatalf("widget ids after delete: %v", err)
	}
	if len(got) != 1 || got[0] != w1.ID {
		t.Fatalf("after delete: got %v, want [%s]", got, w1.ID)
	}

	// Deleting the dashboard cascades its widgets and the id stops
	// existing for the caller.
	if err := dash.Delete(ctx, aid, d.ID); err != nil {
		t.Fatalf("delete dashboard: %v", err)
	}
	if _, err := wid.WidgetIDs(ctx, aid, d.ID); !errors.Is(err, ErrDashboardNotFound) {
		t.Fatalf("after dashboard delete: got %v, want ErrDashboardNotFound", err)
	}

	// Cleanup: the empty dashboard is left over from the cascade test
	// above (d is gone).
	if err := dash.Delete(ctx, aid, empty.ID); err != nil {
		t.Fatalf("cleanup delete empty dashboard: %v", err)
	}
}

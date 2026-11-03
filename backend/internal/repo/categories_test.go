// Category store tests (plan step 2.5a): one shared postgres:18
// testcontainer (newTestEnv) with subtests covering default seeding,
// CRUD round trips, uniqueness, and per-user scoping. The drift pin
// (TestDefaultsMatchFrontend) needs no database: it reads the frontend
// source the Makefile backend-test target mounts at the repo root.
package repo

import (
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
)

// zeroUUID is a syntactically valid uuid that exists in no test database.
const zeroUUID = "00000000-0000-0000-0000-000000000000"

func boolPtr(b bool) *bool { return &b }

func TestCategoryStore(t *testing.T) {
	env := newTestEnv(t, "cat-a", "cat-b")
	store := NewCategoryStore(env.pool)
	if store == nil {
		t.Fatal("NewCategoryStore(nil-check): non-nil pool must give a store")
	}
	a := env.user(t, "cat-a")
	b := env.user(t, "cat-b")
	ctx := env.ctx
	aid, bid := a.row.ID, b.row.ID

	t.Run("seed defaults idempotent and per-user", func(t *testing.T) {
		n, err := store.SeedDefaults(ctx, aid, DefaultCategoryNames, a.dek)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		if n != len(DefaultCategoryNames) {
			t.Fatalf("first seed: want %d inserted, got %d", len(DefaultCategoryNames), n)
		}
		// Re-seeding the same list is a no-op.
		if n, err = store.SeedDefaults(ctx, aid, DefaultCategoryNames, a.dek); err != nil || n != 0 {
			t.Fatalf("re-seed: want 0 inserted, got %d (err %v)", n, err)
		}

		list, err := store.List(ctx, aid, a.dek)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(list) != len(DefaultCategoryNames) {
			t.Fatalf("rows: want %d, got %d", len(DefaultCategoryNames), len(list))
		}
		for i, want := range DefaultCategoryNames {
			if list[i].Name != want {
				t.Fatalf("row %d: want %q, got %q", i, want, list[i].Name)
			}
			if list[i].ParentID != "" || !list[i].Active {
				t.Fatalf("row %d: want root+active, got %+v", i, list[i])
			}
		}

		// Partial re-seed: the existing name is skipped, new names
		// continue sort_order after the user's maximum.
		n, err = store.SeedDefaults(ctx, aid, []string{DefaultCategoryNames[0], "Extra one", "Extra two"}, a.dek)
		if err != nil {
			t.Fatalf("partial seed: %v", err)
		}
		if n != 2 {
			t.Fatalf("partial seed: want 2 inserted, got %d", n)
		}
		list, err = store.List(ctx, aid, a.dek)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(list) != 14 || list[12].Name != "Extra one" || list[13].Name != "Extra two" {
			t.Fatalf("extras out of place: %+v", list[len(list)-2:])
		}
		if list[13].SortOrder <= list[11].SortOrder {
			t.Fatalf("sort_order did not continue past the seed: %+v", list[13])
		}

		// The other user is untouched by the seed.
		lb, err := store.List(ctx, bid, b.dek)
		if err != nil {
			t.Fatalf("list b: %v", err)
		}
		if len(lb) != 0 {
			t.Fatalf("user b seeded: got %d rows", len(lb))
		}
	})

	t.Run("crud round trip", func(t *testing.T) {
		// Surrounding whitespace is trimmed before hashing and storing.
		c, err := store.Create(ctx, aid, CategoryIn{Name: "  Alpha  "}, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if c.Name != "Alpha" || c.ParentID != "" || !c.Active || c.SortOrder != 0 {
			t.Fatalf("created: %+v", c)
		}
		got, err := store.Get(ctx, aid, c.ID, a.dek)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got != c {
			t.Fatalf("get: want %+v, got %+v", c, got)
		}

		// Rename: re-encrypted under the same DEK, old name freed.
		renamed := "Beta"
		updated, err := store.Update(ctx, aid, c.ID, CategoryPatch{Name: &renamed}, a.dek)
		if err != nil {
			t.Fatalf("rename: %v", err)
		}
		if updated.Name != "Beta" {
			t.Fatalf("renamed: %+v", updated)
		}
		alphaAgain, err := store.Create(ctx, aid, CategoryIn{Name: "Alpha"}, a.dek)
		if err != nil {
			t.Fatalf("old name must be free after rename: %v", err)
		}
		if err := store.Delete(ctx, aid, alphaAgain.ID); err != nil {
			t.Fatalf("cleanup delete: %v", err)
		}

		if _, err = store.Update(ctx, aid, c.ID, CategoryPatch{Active: boolPtr(false)}, a.dek); err != nil {
			t.Fatalf("deactivate: %v", err)
		}
		got, err = store.Get(ctx, aid, c.ID, a.dek)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Active {
			t.Fatalf("still active after deactivate: %+v", got)
		}

		// A no-op patch returns the row and changes nothing.
		if _, err = store.Update(ctx, aid, c.ID, CategoryPatch{}, a.dek); err != nil {
			t.Fatalf("no-op update: %v", err)
		}

		if err := store.Delete(ctx, aid, c.ID); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err = store.Get(ctx, aid, c.ID, a.dek); !errors.Is(err, ErrCategoryNotFound) {
			t.Fatalf("get after delete: want ErrCategoryNotFound, got %v", err)
		}
	})

	t.Run("uniqueness per user, case-sensitive names", func(t *testing.T) {
		if _, err := store.Create(ctx, aid, CategoryIn{Name: "Shared"}, a.dek); err != nil {
			t.Fatalf("create for a: %v", err)
		}
		// The same name for a DIFFERENT user is fine: uniqueness is
		// (user_id, name_hash), not name_hash alone.
		if _, err := store.Create(ctx, bid, CategoryIn{Name: "Shared"}, b.dek); err != nil {
			t.Fatalf("same name across users: %v", err)
		}
		if _, err := store.Create(ctx, aid, CategoryIn{Name: "Shared"}, a.dek); !errors.Is(err, ErrNameTaken) {
			t.Fatalf("duplicate for same user: want ErrNameTaken, got %v", err)
		}
		// Pinned in the package header: the hash is exact-case sensitive.
		lower := "shared"
		if _, err := store.Create(ctx, aid, CategoryIn{Name: lower}, a.dek); err != nil {
			t.Fatalf("different case is a distinct name: %v", err)
		}
		// Blank and over-long names are rejected before they reach the DB.
		if _, err := store.Create(ctx, aid, CategoryIn{Name: "   "}, a.dek); err == nil {
			t.Fatal("blank name: want error, got nil")
		}
		if _, err := store.Create(ctx, aid, CategoryIn{Name: strings.Repeat("x", 256)}, a.dek); err == nil {
			t.Fatal("256-char name: want error, got nil")
		}
	})

	t.Run("parenting, scoped to the user", func(t *testing.T) {
		parent, err := store.Create(ctx, aid, CategoryIn{Name: "Parent"}, a.dek)
		if err != nil {
			t.Fatalf("create parent: %v", err)
		}
		pid := parent.ID
		child, err := store.Create(ctx, aid, CategoryIn{Name: "Child", ParentID: &pid}, a.dek)
		if err != nil {
			t.Fatalf("create child: %v", err)
		}
		if child.ParentID != pid {
			t.Fatalf("child: %+v", child)
		}
		if got, err := store.Get(ctx, aid, child.ID, a.dek); err != nil || got.ParentID != pid {
			t.Fatalf("get child: parent %q (err %v), want %q", got.ParentID, err, pid)
		}

		// A well-formed but nonexistent parent id.
		missing := zeroUUID
		if _, err := store.Create(ctx, aid, CategoryIn{Name: "Orphan", ParentID: &missing}, a.dek); !errors.Is(err, ErrParentNotFound) {
			t.Fatalf("missing parent: want ErrParentNotFound, got %v", err)
		}
		// Another user's category is not a valid parent for this user
		// (the FK would accept it; the scoped check must not).
		bcat, err := store.Create(ctx, bid, CategoryIn{Name: "B-side"}, b.dek)
		if err != nil {
			t.Fatalf("create b category: %v", err)
		}
		bidCat := bcat.ID
		if _, err := store.Create(ctx, aid, CategoryIn{Name: "Cross", ParentID: &bidCat}, a.dek); !errors.Is(err, ErrParentNotFound) {
			t.Fatalf("cross-user parent: want ErrParentNotFound, got %v", err)
		}

		// The **string idiom: &nil demotes to root; a parent id re-attaches.
		root := (*string)(nil)
		if _, err := store.Update(ctx, aid, child.ID, CategoryPatch{ParentID: &root}, a.dek); err != nil {
			t.Fatalf("demote to root: %v", err)
		}
		if got, err := store.Get(ctx, aid, child.ID, a.dek); err != nil || got.ParentID != "" {
			t.Fatalf("demoted child: parent %q (err %v), want root", got.ParentID, err)
		}
		reparent := &pid
		if _, err := store.Update(ctx, aid, child.ID, CategoryPatch{ParentID: &reparent}, a.dek); err != nil {
			t.Fatalf("re-parent: %v", err)
		}
		// Re-parenting to another user's category is rejected the same way.
		crossParent := &bidCat
		if _, err := store.Update(ctx, aid, child.ID, CategoryPatch{ParentID: &crossParent}, a.dek); !errors.Is(err, ErrParentNotFound) {
			t.Fatalf("cross-user re-parent: want ErrParentNotFound, got %v", err)
		}
		missingParent := &missing
		if _, err := store.Update(ctx, aid, child.ID, CategoryPatch{ParentID: &missingParent}, a.dek); !errors.Is(err, ErrParentNotFound) {
			t.Fatalf("missing re-parent: want ErrParentNotFound, got %v", err)
		}
	})

	t.Run("not found and per-user scoping", func(t *testing.T) {
		if _, err := store.Get(ctx, aid, zeroUUID, a.dek); !errors.Is(err, ErrCategoryNotFound) {
			t.Fatalf("get unknown id: want ErrCategoryNotFound, got %v", err)
		}
		if _, err := store.Update(ctx, aid, zeroUUID, CategoryPatch{Active: boolPtr(true)}, a.dek); !errors.Is(err, ErrCategoryNotFound) {
			t.Fatalf("update unknown id: want ErrCategoryNotFound, got %v", err)
		}
		if err := store.Delete(ctx, aid, zeroUUID); !errors.Is(err, ErrCategoryNotFound) {
			t.Fatalf("delete unknown id: want ErrCategoryNotFound, got %v", err)
		}

		// User b must not see, update, or delete a row owned by a — and
		// none of b's attempts may touch the row.
		mine, err := store.Create(ctx, aid, CategoryIn{Name: "Scoped"}, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := store.Get(ctx, bid, mine.ID, b.dek); !errors.Is(err, ErrCategoryNotFound) {
			t.Fatalf("b reads a's row: want ErrCategoryNotFound, got %v", err)
		}
		if _, err := store.Update(ctx, bid, mine.ID, CategoryPatch{Active: boolPtr(false)}, b.dek); !errors.Is(err, ErrCategoryNotFound) {
			t.Fatalf("b updates a's row: want ErrCategoryNotFound, got %v", err)
		}
		if err := store.Delete(ctx, bid, mine.ID); !errors.Is(err, ErrCategoryNotFound) {
			t.Fatalf("b deletes a's row: want ErrCategoryNotFound, got %v", err)
		}
		if _, err := store.Get(ctx, aid, mine.ID, a.dek); err != nil {
			t.Fatalf("row must survive b's attempts: %v", err)
		}
		list, err := store.List(ctx, bid, b.dek)
		if err != nil {
			t.Fatalf("list b: %v", err)
		}
		for _, c := range list {
			if c.ID == mine.ID {
				t.Fatal("b's list contains a's row")
			}
		}
	})
}

// TestDefaultsMatchFrontend (no DB) pins DefaultCategoryNames against the
// onboarding preview in frontend/src/lib/settings.ts. The backend-test
// target mounts the repo root so the frontend source is visible; running
// `go test` without that mount fails loudly here rather than skipping.
func TestDefaultsMatchFrontend(t *testing.T) {
	data, err := os.ReadFile("../../../frontend/src/lib/settings.ts")
	if err != nil {
		t.Fatalf("read frontend settings.ts (run via `make backend-test`): %v", err)
	}
	m := regexp.MustCompile(`(?s)DEFAULT_CATEGORY_NAMES: string\[\] = \[([^\]]*)\]`).FindSubmatch(data)
	if m == nil {
		t.Fatal("DEFAULT_CATEGORY_NAMES array not found in frontend/src/lib/settings.ts")
	}
	entries := regexp.MustCompile(`'([^']*)'`).FindAllSubmatch(m[1], -1)
	if len(entries) != len(DefaultCategoryNames) {
		t.Fatalf("frontend declares %d default categories, Go declares %d", len(entries), len(DefaultCategoryNames))
	}
	for i, e := range entries {
		if string(e[1]) != DefaultCategoryNames[i] {
			t.Fatalf("default %d: frontend %q, Go %q", i, e[1], DefaultCategoryNames[i])
		}
	}
}

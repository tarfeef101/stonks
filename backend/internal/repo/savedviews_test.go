// Saved Explore view store tests (round 16, migration 0015): one shared
// postgres:18 testcontainer (newTestEnv) with subtests covering the
// create/list round trip (name trimmed, state verbatim, both packs
// sealed), created_at ordering, the single-field update that re-seals
// both packs, the no-op update rule, the not-found sentinels, per-user
// isolation, wrong-DEK decryption failure, and the house name rule.
// There is no Get method (house convention, like dashboards): every
// read goes through List.
package repo

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"finance-tracker/internal/crypto"

	"github.com/jackc/pgx/v5/pgxpool"
)

// svRowUpdatedAt reads the raw updated_at (the wire DTO omits the
// timestamps; the no-op rule is about the ROW's column).
func svRowUpdatedAt(t *testing.T, pool *pgxpool.Pool, ctx context.Context, id string) time.Time {
	t.Helper()
	var ts time.Time
	if err := pool.QueryRow(ctx, `SELECT updated_at FROM saved_views WHERE id = $1`, id).Scan(&ts); err != nil {
		t.Fatalf("read saved view updated_at: %v", err)
	}
	return ts
}

func TestSavedViewStore(t *testing.T) {
	env := newTestEnv(t, "sva-a", "sva-b")
	svs := NewSavedViewStore(env.pool)
	if svs == nil {
		t.Fatal("non-nil pool must give non-nil store")
	}
	if NewSavedViewStore(nil) != nil {
		t.Fatal("nil pool must give nil store")
	}
	a := env.user(t, "sva-a")
	b := env.user(t, "sva-b")
	ctx := env.ctx
	aid, bid := a.row.ID, b.row.ID

	t.Run("create + list round trip", func(t *testing.T) {
		created, err := svs.Create(ctx, aid,
			SavedViewIn{Name: "  Sv One  ", State: "chart=bar&group=month"}, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if created.ID == "" || created.Name != "Sv One" || created.State != "chart=bar&group=month" {
			t.Fatalf("created: %+v", created)
		}
		if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
			t.Fatalf("created timestamps: %+v", created)
		}

		// An absent state is the default view ("") and round-trips.
		zero, err := svs.Create(ctx, aid, SavedViewIn{Name: "Sv Zero"}, a.dek)
		if err != nil {
			t.Fatalf("create zero-state: %v", err)
		}
		if zero.State != "" {
			t.Fatalf("absent state: %q, want \"\"", zero.State)
		}

		got, err := svs.List(ctx, aid, a.dek)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("list: got %d rows, want 2", len(got))
		}
		if got[0].ID != created.ID || got[0].Name != "Sv One" || got[0].State != "chart=bar&group=month" ||
			got[1].ID != zero.ID || got[1].Name != "Sv Zero" || got[1].State != "" {
			t.Fatalf("list rows: %+v", got)
		}

		// No plaintext in the DB: both columns are sealed packs and
		// must not contain the name's or state's bytes.
		var nameHex, stateHex string
		err = env.pool.QueryRow(ctx,
			`SELECT name_encrypted::text, state_encrypted::text FROM saved_views WHERE id = $1`,
			created.ID).Scan(&nameHex, &stateHex)
		if err != nil {
			t.Fatalf("read packs: %v", err)
		}
		if strings.Contains(nameHex, "Sv One") {
			t.Fatal("name_encrypted contains the plaintext name")
		}
		if strings.Contains(stateHex, "chart=bar&group=month") {
			t.Fatal("state_encrypted contains the plaintext state")
		}

		if err := svs.Delete(ctx, aid, created.ID); err != nil {
			t.Fatalf("cleanup delete 1: %v", err)
		}
		if err := svs.Delete(ctx, aid, zero.ID); err != nil {
			t.Fatalf("cleanup delete 2: %v", err)
		}
	})

	t.Run("list orders by created_at", func(t *testing.T) {
		first, err := svs.Create(ctx, aid, SavedViewIn{Name: "Sv First"}, a.dek)
		if err != nil {
			t.Fatalf("create 1: %v", err)
		}
		// created_at is timestamptz (microsecond resolution); the
		// second row must land strictly later for the ordering to be
		// deterministic.
		time.Sleep(10 * time.Millisecond)
		second, err := svs.Create(ctx, aid, SavedViewIn{Name: "Sv Second"}, a.dek)
		if err != nil {
			t.Fatalf("create 2: %v", err)
		}
		got, err := svs.List(ctx, aid, a.dek)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("list: got %d rows, want 2", len(got))
		}
		if got[0].ID != first.ID || got[1].ID != second.ID {
			t.Fatalf("list order: %+v", got)
		}
		if err := svs.Delete(ctx, aid, first.ID); err != nil {
			t.Fatalf("cleanup delete 1: %v", err)
		}
		if err := svs.Delete(ctx, aid, second.ID); err != nil {
			t.Fatalf("cleanup delete 2: %v", err)
		}
	})

	t.Run("update changes one field, re-seals both packs", func(t *testing.T) {
		created, err := svs.Create(ctx, aid,
			SavedViewIn{Name: "Sv Up", State: "chart=line"}, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		tsBefore := svRowUpdatedAt(t, env.pool, ctx, created.ID)

		// Rename only: the state must come back untouched (the UPDATE
		// re-seals both packs, so the untouched one is preserved).
		updated, err := svs.Update(ctx, aid, created.ID,
			SavedViewPatch{Name: strPtr("Sv Up2")}, a.dek)
		if err != nil {
			t.Fatalf("rename: %v", err)
		}
		if updated.Name != "Sv Up2" || updated.State != "chart=line" {
			t.Fatalf("updated: %+v", updated)
		}
		if ts := svRowUpdatedAt(t, env.pool, ctx, created.ID); !ts.After(tsBefore) {
			t.Fatalf("updated_at must move: %v -> %v", tsBefore, ts)
		}
		// Re-list: the re-sealed packs decrypt to the merged row.
		got, err := svs.List(ctx, aid, a.dek)
		if err != nil {
			t.Fatalf("list after rename: %v", err)
		}
		if len(got) != 1 || got[0].Name != "Sv Up2" || got[0].State != "chart=line" {
			t.Fatalf("list after rename: %+v", got)
		}

		// State update only: the name must come back untouched.
		updated, err = svs.Update(ctx, aid, created.ID,
			SavedViewPatch{State: strPtr("q=coffee&from=2026-01-01")}, a.dek)
		if err != nil {
			t.Fatalf("state update: %v", err)
		}
		if updated.Name != "Sv Up2" || updated.State != "q=coffee&from=2026-01-01" {
			t.Fatalf("updated: %+v", updated)
		}
		got, err = svs.List(ctx, aid, a.dek)
		if err != nil {
			t.Fatalf("list after state update: %v", err)
		}
		if len(got) != 1 || got[0].Name != "Sv Up2" || got[0].State != "q=coffee&from=2026-01-01" {
			t.Fatalf("list after state update: %+v", got)
		}

		if err := svs.Delete(ctx, aid, created.ID); err != nil {
			t.Fatalf("cleanup delete: %v", err)
		}
	})

	t.Run("update no-op leaves updated_at put", func(t *testing.T) {
		created, err := svs.Create(ctx, aid,
			SavedViewIn{Name: "Sv Noop", State: "chart=bar"}, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		tsBefore := svRowUpdatedAt(t, env.pool, ctx, created.ID)

		// All-nil patch: nothing changes.
		got, err := svs.Update(ctx, aid, created.ID, SavedViewPatch{}, a.dek)
		if err != nil {
			t.Fatalf("no-op (nil) update: %v", err)
		}
		if ts := svRowUpdatedAt(t, env.pool, ctx, created.ID); !ts.Equal(tsBefore) {
			t.Fatalf("nil patch must not touch updated_at: %v -> %v", tsBefore, ts)
		}

		// Both fields present but equal to the current row (the name
		// compared trimmed, like the no-op rule in the budgets store).
		got, err = svs.Update(ctx, aid, created.ID, SavedViewPatch{
			Name:  strPtr("  Sv Noop  "),
			State: strPtr("chart=bar"),
		}, a.dek)
		if err != nil {
			t.Fatalf("no-op (unchanged) update: %v", err)
		}
		if ts := svRowUpdatedAt(t, env.pool, ctx, created.ID); !ts.Equal(tsBefore) {
			t.Fatalf("unchanged patch must not touch updated_at: %v -> %v", tsBefore, ts)
		}
		if got.Name != "Sv Noop" || got.State != "chart=bar" {
			t.Fatalf("row must be unchanged: %+v", got)
		}

		if err := svs.Delete(ctx, aid, created.ID); err != nil {
			t.Fatalf("cleanup delete: %v", err)
		}
	})

	t.Run("not-found sentinels", func(t *testing.T) {
		ca, err := svs.Create(ctx, aid, SavedViewIn{Name: "Sv NF"}, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		// Foreign id (a's row, seen as b) and bogus id.
		if _, err := svs.Update(ctx, bid, ca.ID, SavedViewPatch{Name: strPtr("hijack")}, b.dek); !errors.Is(err, ErrSavedViewNotFound) {
			t.Fatalf("update foreign: got %v, want ErrSavedViewNotFound", err)
		}
		if err := svs.Delete(ctx, bid, ca.ID); !errors.Is(err, ErrSavedViewNotFound) {
			t.Fatalf("delete foreign: got %v, want ErrSavedViewNotFound", err)
		}
		if _, err := svs.Update(ctx, aid, zeroUUID, SavedViewPatch{Name: strPtr("x")}, a.dek); !errors.Is(err, ErrSavedViewNotFound) {
			t.Fatalf("update bogus: got %v, want ErrSavedViewNotFound", err)
		}
		if err := svs.Delete(ctx, aid, zeroUUID); !errors.Is(err, ErrSavedViewNotFound) {
			t.Fatalf("delete bogus: got %v, want ErrSavedViewNotFound", err)
		}
		// The row must be intact through all the failed attempts.
		got, err := svs.List(ctx, aid, a.dek)
		if err != nil {
			t.Fatalf("list after failed attempts: %v", err)
		}
		if len(got) != 1 || got[0].ID != ca.ID || got[0].Name != "Sv NF" {
			t.Fatalf("row must be intact: %+v", got)
		}
		if err := svs.Delete(ctx, aid, ca.ID); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
	})

	t.Run("per-user scoping", func(t *testing.T) {
		created, err := svs.Create(ctx, aid, SavedViewIn{Name: "Sv A only"}, a.dek)
		if err != nil {
			t.Fatalf("create a: %v", err)
		}
		if _, err := svs.Update(ctx, bid, created.ID, SavedViewPatch{Name: strPtr("hijack")}, b.dek); !errors.Is(err, ErrSavedViewNotFound) {
			t.Fatalf("other user's update: got %v, want ErrSavedViewNotFound", err)
		}
		if err := svs.Delete(ctx, bid, created.ID); !errors.Is(err, ErrSavedViewNotFound) {
			t.Fatalf("other user's delete: got %v, want ErrSavedViewNotFound", err)
		}
		// b's own row must not appear in a's list, and a's row must not
		// appear in b's.
		mine, err := svs.Create(ctx, bid, SavedViewIn{Name: "Sv B only"}, b.dek)
		if err != nil {
			t.Fatalf("create b: %v", err)
		}
		la, err := svs.List(ctx, aid, a.dek)
		if err != nil {
			t.Fatalf("list a: %v", err)
		}
		lb, err := svs.List(ctx, bid, b.dek)
		if err != nil {
			t.Fatalf("list b: %v", err)
		}
		for _, x := range la {
			if x.ID == mine.ID {
				t.Fatal("other user's saved view leaked into a's list")
			}
		}
		for _, x := range lb {
			if x.ID == created.ID {
				t.Fatal("other user's saved view leaked into b's list")
			}
		}
		if err := svs.Delete(ctx, aid, created.ID); err != nil {
			t.Fatalf("cleanup delete a: %v", err)
		}
		if err := svs.Delete(ctx, bid, mine.ID); err != nil {
			t.Fatalf("cleanup delete b: %v", err)
		}
	})

	t.Run("wrong DEK: decryption failure is an error, not not-found", func(t *testing.T) {
		created, err := svs.Create(ctx, aid,
			SavedViewIn{Name: "Sv Secret", State: "q=hidden"}, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		// The other user's DEK cannot open this row's packs.
		if _, err := svs.List(ctx, aid, b.dek); err == nil {
			t.Fatal("wrong-DEK list must fail")
		} else if errors.Is(err, ErrSavedViewNotFound) {
			t.Fatalf("wrong-DEK list: got the not-found sentinel, want a decryption error: %v", err)
		}
		if _, err := svs.Update(ctx, aid, created.ID, SavedViewPatch{Name: strPtr("x")}, b.dek); err == nil {
			t.Fatal("wrong-DEK update must fail")
		} else if errors.Is(err, ErrSavedViewNotFound) {
			t.Fatalf("wrong-DEK update: got the not-found sentinel, want a decryption error: %v", err)
		}
		// A fresh DEK never wrapped for this user fails the same way.
		fresh, err := crypto.NewDEK()
		if err != nil {
			t.Fatalf("fresh DEK: %v", err)
		}
		if _, err := svs.List(ctx, aid, fresh); err == nil {
			t.Fatal("fresh-DEK list must fail")
		} else if errors.Is(err, ErrSavedViewNotFound) {
			t.Fatalf("fresh-DEK list: got the not-found sentinel, want a decryption error: %v", err)
		}
		// The row is still intact under the right DEK.
		got, err := svs.List(ctx, aid, a.dek)
		if err != nil {
			t.Fatalf("row must be intact: %v", err)
		}
		if len(got) != 1 || got[0].ID != created.ID || got[0].Name != "Sv Secret" || got[0].State != "q=hidden" {
			t.Fatalf("row must be intact: %+v", got)
		}
		// Delete does not decrypt, so it works with any caller DEK.
		if err := svs.Delete(ctx, aid, created.ID); err != nil {
			t.Fatalf("cleanup delete: %v", err)
		}
	})

	t.Run("name rule", func(t *testing.T) {
		// Blank and over-long names are rejected before the DB sees them
		// (the house name rule, owned by the repo layer).
		if _, err := svs.Create(ctx, aid, SavedViewIn{Name: "   "}, a.dek); err == nil {
			t.Fatal("blank name must fail")
		} else if !strings.Contains(err.Error(), "name must not be empty") {
			t.Fatalf("blank name: want the empty-name error, got %v", err)
		}
		long := strings.Repeat("n", 256)
		if _, err := svs.Create(ctx, aid, SavedViewIn{Name: long}, a.dek); err == nil {
			t.Fatal("256-rune name must fail")
		} else if !strings.Contains(err.Error(), "name must be at most 255 characters") {
			t.Fatalf("long name: want the length error, got %v", err)
		}
		// The same rule on update: a blank patch name errors, the row
		// is untouched.
		created, err := svs.Create(ctx, aid, SavedViewIn{Name: "Sv Rule"}, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := svs.Update(ctx, aid, created.ID, SavedViewPatch{Name: strPtr("  ")}, a.dek); err == nil {
			t.Fatal("blank patch name must fail")
		} else if !strings.Contains(err.Error(), "name must not be empty") {
			t.Fatalf("blank patch name: want the empty-name error, got %v", err)
		}
		got, err := svs.List(ctx, aid, a.dek)
		if err != nil {
			t.Fatalf("list after rejected patches: %v", err)
		}
		if len(got) != 1 || got[0].Name != "Sv Rule" {
			t.Fatalf("row must be intact: %+v", got)
		}
		if err := svs.Delete(ctx, aid, created.ID); err != nil {
			t.Fatalf("cleanup delete: %v", err)
		}
	})
}

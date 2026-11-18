// Account store tests (plan step 2.5b): one shared postgres:18
// testcontainer (newTestEnv) with subtests covering CRUD round trips
// (incl. the **string clear idiom), validation, per-user uniqueness,
// schema-reference scoping (customs only since 0018, FK SET NULL on
// schema delete), and per-user isolation.
package repo

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func strPtr(s string) *string { return &s }

func TestAccountStore(t *testing.T) {
	env := newTestEnv(t, "acct-a", "acct-b")
	acct := NewAccountStore(env.pool)
	schema := NewSchemaStore(env.pool)
	if acct == nil || schema == nil {
		t.Fatal("non-nil pool must give non-nil stores")
	}
	a := env.user(t, "acct-a")
	b := env.user(t, "acct-b")
	ctx := env.ctx
	aid, bid := a.row.ID, b.row.ID

	// One of a's own custom schemas for the schema-reference subtests
	// (0018 dropped the builtins; a schema id is all an account's
	// default_schema_id needs).
	asc, err := schema.Create(ctx, aid, SchemaIn{
		Name: "A-import", Kind: "csv", Spec: json.RawMessage(`{"columns":{"date":"1"}}`),
	}, a.dek)
	if err != nil {
		t.Fatalf("create schema: %v", err)
	}

	t.Run("crud round trip, subtype + schema ref", func(t *testing.T) {
		sub := "chequing"
		acc, err := acct.Create(ctx, aid, AccountIn{
			Name: "  Main Bank  ", Type: "bank", Subtype: &sub,
			Currency: "USD", DefaultSchemaID: &asc.ID,
		}, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if acc.Name != "Main Bank" || acc.Type != "bank" || acc.Subtype != "chequing" ||
			acc.Currency != "USD" || acc.DefaultSchemaID != asc.ID {
			t.Fatalf("created: %+v", acc)
		}
		got, err := acct.Get(ctx, aid, acc.ID, a.dek)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got != acc {
			t.Fatalf("get: want %+v, got %+v", acc, got)
		}

		// Update name + type + currency; the untouched fields stay.
		renamed, err := acct.Update(ctx, aid, acc.ID, AccountPatch{
			Name: strPtr("Second Bank"), Type: strPtr("investment"), Currency: strPtr("CAD"),
		}, a.dek)
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if renamed.Name != "Second Bank" || renamed.Type != "investment" || renamed.Currency != "CAD" ||
			renamed.Subtype != "chequing" || renamed.DefaultSchemaID != asc.ID {
			t.Fatalf("updated: %+v", renamed)
		}
		// The old name is free again after the rename.
		again, err := acct.Create(ctx, aid, AccountIn{Name: "Main Bank", Type: "cash", Currency: "USD"}, a.dek)
		if err != nil {
			t.Fatalf("old name must be free after rename: %v", err)
		}
		if err := acct.Delete(ctx, aid, again.ID); err != nil {
			t.Fatalf("cleanup delete: %v", err)
		}

		// The **string clear idiom: &nil sets the column to NULL.
		empty := (*string)(nil)
		cleared, err := acct.Update(ctx, aid, acc.ID, AccountPatch{Subtype: &empty, DefaultSchemaID: &empty}, a.dek)
		if err != nil {
			t.Fatalf("clear: %v", err)
		}
		if cleared.Subtype != "" || cleared.DefaultSchemaID != "" {
			t.Fatalf("cleared: %+v", cleared)
		}

		// A no-op patch returns the row and changes nothing.
		if _, err := acct.Update(ctx, aid, acc.ID, AccountPatch{}, a.dek); err != nil {
			t.Fatalf("no-op update: %v", err)
		}

		if err := acct.Delete(ctx, aid, acc.ID); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := acct.Get(ctx, aid, acc.ID, a.dek); !errors.Is(err, ErrAccountNotFound) {
			t.Fatalf("get after delete: want ErrAccountNotFound, got %v", err)
		}
	})

	t.Run("validation, rejected before the DB", func(t *testing.T) {
		bad := []struct {
			name string
			in   AccountIn
		}{
			{"bad type", AccountIn{Name: "V-Type", Type: "crypto", Currency: "USD"}},
			{"lowercase currency", AccountIn{Name: "V-Cur1", Type: "bank", Currency: "usd"}},
			{"4-char currency", AccountIn{Name: "V-Cur2", Type: "bank", Currency: "USDI"}},
			{"empty currency", AccountIn{Name: "V-Cur3", Type: "bank", Currency: ""}},
			{"subtype too long", AccountIn{Name: "V-Sub", Type: "bank", Currency: "USD", Subtype: strPtr(strings.Repeat("s", 65))}},
			{"blank name", AccountIn{Name: "   ", Type: "bank", Currency: "USD"}},
			{"256-char name", AccountIn{Name: strings.Repeat("x", 256), Type: "bank", Currency: "USD"}},
		}
		for _, tc := range bad {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := acct.Create(ctx, aid, tc.in, a.dek); err == nil {
					t.Fatal("want rejection, got nil")
				}
			})
		}
	})

	t.Run("uniqueness per user, case-sensitive", func(t *testing.T) {
		if _, err := acct.Create(ctx, aid, AccountIn{Name: "Main Savings", Type: "bank", Currency: "USD"}, a.dek); err != nil {
			t.Fatalf("create for a: %v", err)
		}
		// The same name for a DIFFERENT user is fine: uniqueness is
		// (user_id, name_hash), not name_hash alone.
		if _, err := acct.Create(ctx, bid, AccountIn{Name: "Main Savings", Type: "bank", Currency: "USD"}, b.dek); err != nil {
			t.Fatalf("same name across users: %v", err)
		}
		if _, err := acct.Create(ctx, aid, AccountIn{Name: "Main Savings", Type: "bank", Currency: "USD"}, a.dek); !errors.Is(err, ErrNameTaken) {
			t.Fatalf("duplicate for same user: want ErrNameTaken, got %v", err)
		}
		// Pinned in the package header: the hash is exact-case sensitive.
		if _, err := acct.Create(ctx, aid, AccountIn{Name: "main savings", Type: "bank", Currency: "USD"}, a.dek); err != nil {
			t.Fatalf("different case is a distinct name: %v", err)
		}
		// Whitespace is trimmed before hashing: a padded duplicate is
		// still a duplicate.
		if _, err := acct.Create(ctx, aid, AccountIn{Name: "  main savings  ", Type: "bank", Currency: "USD"}, a.dek); !errors.Is(err, ErrNameTaken) {
			t.Fatalf("trimmed duplicate: want ErrNameTaken, got %v", err)
		}
	})

	t.Run("schema ref: customs only, FK SET NULL", func(t *testing.T) {
		// A well-formed but nonexistent schema id.
		missing := zeroUUID
		if _, err := acct.Create(ctx, aid, AccountIn{Name: "NoSchema", Type: "bank", Currency: "USD", DefaultSchemaID: &missing}, a.dek); !errors.Is(err, ErrSchemaNotFound) {
			t.Fatalf("missing schema: want ErrSchemaNotFound, got %v", err)
		}
		// Another user's custom schema is not usable here (the FK alone
		// would accept it; the scoped check must not).
		if _, err := acct.Create(ctx, bid, AccountIn{Name: "B-with-A-schema", Type: "bank", Currency: "USD", DefaultSchemaID: &asc.ID}, b.dek); !errors.Is(err, ErrSchemaNotFound) {
			t.Fatalf("cross-user schema on create: want ErrSchemaNotFound, got %v", err)
		}
		// b's own account (no schema ref) is what the cross-user
		// update check bounces against.
		bacct, err := acct.Create(ctx, bid, AccountIn{Name: "B-bare", Type: "bank", Currency: "USD"}, b.dek)
		if err != nil {
			t.Fatalf("create b account: %v", err)
		}
		cross := &asc.ID
		if _, err := acct.Update(ctx, bid, bacct.ID, AccountPatch{DefaultSchemaID: &cross}, b.dek); !errors.Is(err, ErrSchemaNotFound) {
			t.Fatalf("cross-user schema on update: want ErrSchemaNotFound, got %v", err)
		}

		// FK SET NULL: deleting the schema orphans the setting, never
		// the account (0002 default_schema_id ON DELETE SET NULL).
		bown, err := schema.Create(ctx, bid, SchemaIn{Name: "B-import", Kind: "json", Spec: json.RawMessage(`{}`)}, b.dek)
		if err != nil {
			t.Fatalf("create b schema: %v", err)
		}
		bacct2, err := acct.Create(ctx, bid, AccountIn{Name: "B-custom-schema", Type: "bank", Currency: "USD", DefaultSchemaID: &bown.ID}, b.dek)
		if err != nil {
			t.Fatalf("create b account: %v", err)
		}
		if err := schema.Delete(ctx, bid, bown.ID); err != nil {
			t.Fatalf("delete b schema: %v", err)
		}
		if got, err := acct.Get(ctx, bid, bacct2.ID, b.dek); err != nil || got.DefaultSchemaID != "" {
			t.Fatalf("account must survive the schema delete with a NULL ref: %+v (err %v)", got, err)
		}
	})

	t.Run("per-user scoping", func(t *testing.T) {
		mine, err := acct.Create(ctx, aid, AccountIn{Name: "Scoped", Type: "cash", Currency: "USD"}, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		// User b must not see, update, or delete a row owned by a — and
		// none of b's attempts may touch the row.
		if _, err := acct.Get(ctx, bid, mine.ID, b.dek); !errors.Is(err, ErrAccountNotFound) {
			t.Fatalf("b reads a's row: want ErrAccountNotFound, got %v", err)
		}
		if _, err := acct.Update(ctx, bid, mine.ID, AccountPatch{Type: strPtr("other")}, b.dek); !errors.Is(err, ErrAccountNotFound) {
			t.Fatalf("b updates a's row: want ErrAccountNotFound, got %v", err)
		}
		if err := acct.Delete(ctx, bid, mine.ID); !errors.Is(err, ErrAccountNotFound) {
			t.Fatalf("b deletes a's row: want ErrAccountNotFound, got %v", err)
		}
		if _, err := acct.Get(ctx, aid, mine.ID, a.dek); err != nil {
			t.Fatalf("row must survive b's attempts: %v", err)
		}
		list, err := acct.List(ctx, bid, b.dek)
		if err != nil {
			t.Fatalf("list b: %v", err)
		}
		for _, x := range list {
			if x.ID == mine.ID {
				t.Fatal("b's list contains a's row")
			}
		}
	})
}

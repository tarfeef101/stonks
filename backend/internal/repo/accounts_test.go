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
	"time"
)

func strPtr(s string) *string { return &s }

// accountsEqual compares two Accounts value-wise: the pointer fields
// (*int birth year, *time.Time opened date) are compared through their
// values, so a DB round trip is checked without pointer identity.
func accountsEqual(x, y Account) bool {
	xn, yn := x.BeneficiaryBirthYear == nil, y.BeneficiaryBirthYear == nil
	if xn != yn || (!xn && *x.BeneficiaryBirthYear != *y.BeneficiaryBirthYear) {
		return false
	}
	xo, yo := x.OpenedAt == nil, y.OpenedAt == nil
	if xo != yo || (!xo && !x.OpenedAt.Equal(*y.OpenedAt)) {
		return false
	}
	x.BeneficiaryBirthYear, y.BeneficiaryBirthYear = nil, nil
	x.OpenedAt, y.OpenedAt = nil, nil
	return x == y
}

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

	t.Run("0019 plan fields: round trip, sealed at rest", func(t *testing.T) {
		opened := time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC)
		acc, err := acct.Create(ctx, aid, AccountIn{
			Name: "RRSP One", Type: "rrsp", Currency: "CAD",
			BeneficiaryName:      "Test Beneficiary",
			BeneficiaryBirthYear: strPtr("1990"),
			OpeningBalance:       strPtr("1234.56"),
			OpenedAt:             &opened,
		}, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if acc.BeneficiaryName != "Test Beneficiary" || acc.BeneficiaryBirthYear == nil || *acc.BeneficiaryBirthYear != 1990 ||
			acc.OpeningBalance != "1234.56" || acc.OpenedAt == nil || !acc.OpenedAt.Equal(opened) {
			t.Fatalf("created: %+v", acc)
		}
		if got, err := acct.Get(ctx, aid, acc.ID, a.dek); err != nil || !accountsEqual(got, acc) {
			t.Fatalf("get: want %+v, got %+v (err %v)", acc, got, err)
		}
		list, err := acct.List(ctx, aid, a.dek)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		var inList bool
		for _, x := range list {
			if x.ID == acc.ID {
				inList = true
				if !accountsEqual(x, acc) {
					t.Fatalf("list: want %+v, got %+v", acc, x)
				}
			}
		}
		if !inList {
			t.Fatal("created account missing from list")
		}

		// At rest the packs are AES-GCM ciphertext: neither column may
		// contain the plaintext name or the plaintext year.
		var bn, by []byte
		if err := env.pool.QueryRow(ctx, `
			SELECT beneficiary_name_encrypted, beneficiary_birth_year_encrypted
			FROM accounts WHERE id = $1`, acc.ID).Scan(&bn, &by); err != nil {
			t.Fatalf("raw columns: %v", err)
		}
		if strings.Contains(string(bn), "Test Beneficiary") {
			t.Fatal("raw beneficiary name pack contains the plaintext")
		}
		if strings.Contains(string(by), "1990") {
			t.Fatal("raw beneficiary birth year pack contains the plaintext year")
		}

		// A bare plan account: the 0019 fields default (NULLs, 0.00).
		bare, err := acct.Create(ctx, aid, AccountIn{Name: "Bare Plan", Type: "tfsa", Currency: "CAD"}, a.dek)
		if err != nil {
			t.Fatalf("create bare: %v", err)
		}
		if bare.BeneficiaryName != "" || bare.BeneficiaryBirthYear != nil ||
			bare.OpeningBalance != "0.00" || bare.OpenedAt != nil {
			t.Fatalf("bare: %+v", bare)
		}
		if err := acct.Delete(ctx, aid, bare.ID); err != nil {
			t.Fatalf("cleanup bare: %v", err)
		}
		if err := acct.Delete(ctx, aid, acc.ID); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
	})

	t.Run("0019 plan fields: tri-state patch", func(t *testing.T) {
		acc, err := acct.Create(ctx, aid, AccountIn{Name: "Patch Plan", Type: "rrsp", Currency: "CAD"}, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}

		// Set the beneficiary name (padded, trimmed on the way in) and
		// birth year.
		got, err := acct.Update(ctx, aid, acc.ID, AccountPatch{
			BeneficiaryName: strPtr("  Spouse  "), BeneficiaryBirthYear: strPtr("1988"),
		}, a.dek)
		if err != nil {
			t.Fatalf("set: %v", err)
		}
		if got.BeneficiaryName != "Spouse" || got.BeneficiaryBirthYear == nil || *got.BeneficiaryBirthYear != 1988 {
			t.Fatalf("after set: %+v", got)
		}

		// Clear the birth year (&""), keep the name: the year column
		// goes back to NULL.
		got, err = acct.Update(ctx, aid, acc.ID, AccountPatch{BeneficiaryBirthYear: strPtr("")}, a.dek)
		if err != nil {
			t.Fatalf("clear year: %v", err)
		}
		if got.BeneficiaryName != "Spouse" || got.BeneficiaryBirthYear != nil {
			t.Fatalf("after clear year: %+v", got)
		}
		var rawYear *[]byte
		if err := env.pool.QueryRow(ctx,
			`SELECT beneficiary_birth_year_encrypted FROM accounts WHERE id = $1`, acc.ID).Scan(&rawYear); err != nil {
			t.Fatalf("raw year column: %v", err)
		}
		if rawYear != nil {
			t.Fatalf("cleared birth year column must be NULL, got a %d-byte pack", len(*rawYear))
		}

		// Set the opening balance (canonicalized on the way out) and
		// opened date.
		opened := time.Date(2022, 1, 15, 0, 0, 0, 0, time.UTC)
		openedPtr := &opened
		got, err = acct.Update(ctx, aid, acc.ID, AccountPatch{
			OpeningBalance: strPtr("99.9"), OpenedAt: &openedPtr,
		}, a.dek)
		if err != nil {
			t.Fatalf("set opening: %v", err)
		}
		if got.OpeningBalance != "99.90" || got.OpenedAt == nil || !got.OpenedAt.Equal(opened) {
			t.Fatalf("after set opening: %+v", got)
		}

		// Clear both back: the balance resets to 0, the date to NULL.
		var nilTime *time.Time
		got, err = acct.Update(ctx, aid, acc.ID, AccountPatch{
			OpeningBalance: strPtr(""), OpenedAt: &nilTime,
		}, a.dek)
		if err != nil {
			t.Fatalf("clear opening: %v", err)
		}
		if got.OpeningBalance != "0.00" || got.OpenedAt != nil {
			t.Fatalf("after clear opening: %+v", got)
		}

		// An all-nil patch is a no-op that returns the current row.
		got, err = acct.Update(ctx, aid, acc.ID, AccountPatch{}, a.dek)
		if err != nil {
			t.Fatalf("no-op: %v", err)
		}
		if got.BeneficiaryName != "Spouse" || got.BeneficiaryBirthYear != nil ||
			got.OpeningBalance != "0.00" || got.OpenedAt != nil {
			t.Fatalf("no-op must change nothing: %+v", got)
		}
		if err := acct.Delete(ctx, aid, acc.ID); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
	})

	t.Run("0019 plan fields: invalid input, no partial write", func(t *testing.T) {
		before, err := acct.List(ctx, aid, a.dek)
		if err != nil {
			t.Fatalf("list: %v", err)
		}

		// Birth years: wrong length, out of range, non-digit.
		for _, y := range []string{"199", "20271", "2999", "1899", "19a0"} {
			if _, err := acct.Create(ctx, aid, AccountIn{
				Name: "BadYear", Type: "rrsp", Currency: "CAD", BeneficiaryBirthYear: strPtr(y),
			}, a.dek); err == nil {
				t.Fatalf("birth year %q: want rejection, got nil", y)
			}
		}
		// Opening balances: negative, more than two fractional digits.
		for _, o := range []string{"-5", "1.234"} {
			if _, err := acct.Create(ctx, aid, AccountIn{
				Name: "BadBalance", Type: "rrsp", Currency: "CAD", OpeningBalance: strPtr(o),
			}, a.dek); err == nil {
				t.Fatalf("opening balance %q: want rejection, got nil", o)
			}
		}

		after, err := acct.List(ctx, aid, a.dek)
		if err != nil {
			t.Fatalf("list after: %v", err)
		}
		if len(after) != len(before) {
			t.Fatalf("rejected creates must not write rows: %d before, %d after", len(before), len(after))
		}

		// A combined patch with one valid and one invalid field applies
		// nothing (validation happens before the UPDATE).
		acc, err := acct.Create(ctx, aid, AccountIn{
			Name: "Combo Plan", Type: "rrsp", Currency: "CAD", BeneficiaryBirthYear: strPtr("1980"),
		}, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := acct.Update(ctx, aid, acc.ID, AccountPatch{
			BeneficiaryName: strPtr("Somebody"), BeneficiaryBirthYear: strPtr("12ab"),
		}, a.dek); err == nil {
			t.Fatal("patch with invalid birth year: want rejection, got nil")
		}
		if got, err := acct.Get(ctx, aid, acc.ID, a.dek); err != nil ||
			got.BeneficiaryName != "" || got.BeneficiaryBirthYear == nil || *got.BeneficiaryBirthYear != 1980 {
			t.Fatalf("invalid patch must not apply the valid field either: %+v (err %v)", got, err)
		}
		if err := acct.Delete(ctx, aid, acc.ID); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
	})

	t.Run("0019 plan account types accepted, unknown rejected", func(t *testing.T) {
		for _, typ := range []string{"rrsp", "tfsa", "fhsa", "rrif", "resp", "rdsp"} {
			acc, err := acct.Create(ctx, aid, AccountIn{Name: "Plan-" + typ, Type: typ, Currency: "CAD"}, a.dek)
			if err != nil {
				t.Fatalf("create %s: %v", typ, err)
			}
			if err := acct.Delete(ctx, aid, acc.ID); err != nil {
				t.Fatalf("cleanup %s: %v", typ, err)
			}
		}
		// An unknown type is a 400 at the repo, not a 23514 from the
		// CHECK constraint.
		if _, err := acct.Create(ctx, aid, AccountIn{Name: "Plan-CEPP", Type: "cepp", Currency: "CAD"}, a.dek); err == nil {
			t.Fatal("unknown plan type must be rejected")
		}
	})

	t.Run("user store: birth year set / clear / unknown user", func(t *testing.T) {
		users := NewUserStore(env.pool)
		if users == nil {
			t.Fatal("non-nil pool must give a non-nil user store")
		}
		rawBirthYear := func(t *testing.T, id string) *[]byte {
			t.Helper()
			var raw *[]byte
			if err := env.pool.QueryRow(ctx,
				`SELECT birth_year_encrypted FROM users WHERE id = $1`, id).Scan(&raw); err != nil {
				t.Fatalf("raw birth year column: %v", err)
			}
			return raw
		}

		// Unknown ids fail on both sides.
		if _, err := users.Get(ctx, zeroUUID, a.dek); !errors.Is(err, ErrUserNotFound) {
			t.Fatalf("get unknown: want ErrUserNotFound, got %v", err)
		}
		if err := users.SetBirthYear(ctx, zeroUUID, strPtr("1985"), a.dek); !errors.Is(err, ErrUserNotFound) {
			t.Fatalf("set unknown: want ErrUserNotFound, got %v", err)
		}

		// nil is a no-op: the column stays NULL.
		if err := users.SetBirthYear(ctx, aid, nil, a.dek); err != nil {
			t.Fatalf("nil no-op: %v", err)
		}
		if raw := rawBirthYear(t, aid); raw != nil {
			t.Fatalf("nil no-op must leave the column NULL, got a %d-byte pack", len(*raw))
		}

		// Set: the year comes back decrypted, and the at-rest pack
		// carries no plaintext.
		if err := users.SetBirthYear(ctx, aid, strPtr("1985"), a.dek); err != nil {
			t.Fatalf("set: %v", err)
		}
		u, err := users.Get(ctx, aid, a.dek)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if u.ID != aid || u.Auth0Sub != "acct-a" || u.Email != "acct-a@localhost" || u.DisplayName != "acct-a" ||
			u.BirthYear == nil || *u.BirthYear != 1985 {
			t.Fatalf("user: %+v", u)
		}
		if raw := rawBirthYear(t, aid); raw == nil {
			t.Fatal("set birth year must store a pack")
		} else if strings.Contains(string(*raw), "1985") {
			t.Fatal("birth year pack contains the plaintext year")
		}

		// Invalid input is rejected and leaves the stored year intact.
		if err := users.SetBirthYear(ctx, aid, strPtr("12ab"), a.dek); err == nil {
			t.Fatal("invalid birth year: want rejection, got nil")
		}
		if u2, err := users.Get(ctx, aid, a.dek); err != nil || u2.BirthYear == nil || *u2.BirthYear != 1985 {
			t.Fatalf("invalid set must not touch the column: %+v (err %v)", u2, err)
		}

		// Clear: &"" sets the column back to NULL.
		if err := users.SetBirthYear(ctx, aid, strPtr(""), a.dek); err != nil {
			t.Fatalf("clear: %v", err)
		}
		if u3, err := users.Get(ctx, aid, a.dek); err != nil || u3.BirthYear != nil {
			t.Fatalf("cleared: %+v (err %v)", u3, err)
		}
		if raw := rawBirthYear(t, aid); raw != nil {
			t.Fatalf("cleared column must be NULL, got a %d-byte pack", len(*raw))
		}
	})
}

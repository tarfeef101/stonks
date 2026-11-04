// Transaction store tests (plan step 2.6, first half of the 2.6 split):
// one shared postgres:18 testcontainer (newTestEnv) with subtests
// covering the dedup upsert (0002 re-import rule), CRUD + patch round
// trips (incl. the **string clear idiom), validation sentinels,
// filters/pagination/search, per-user scoping, plus DB-free tests of
// canonicalAmount / normalizeDescription / dedupHash.
package repo

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// dateOf parses a "2006-01-02" string to a UTC time.Time fixture.
func dateOf(s string) time.Time {
	tm, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return tm
}

// txEqual compares the stable fields of two transactions. CreatedAt/
// UpdatedAt are excluded; *time.Time fields compare by Equal.
func txEqual(got, want Transaction) bool {
	if got.ID != want.ID || got.AccountID != want.AccountID ||
		got.Amount != want.Amount || got.CategoryID != want.CategoryID ||
		got.Description != want.Description || got.Notes != want.Notes ||
		got.Raw != want.Raw || got.DedupHash != want.DedupHash ||
		got.ReviewPending != want.ReviewPending {
		return false
	}
	if (got.DateExecuted == nil) != (want.DateExecuted == nil) {
		return false
	}
	if got.DateExecuted != nil && !got.DateExecuted.Equal(*want.DateExecuted) {
		return false
	}
	return got.DatePosted.Equal(want.DatePosted)
}

func txIDs(txs []Transaction) []string {
	ids := make([]string, 0, len(txs))
	for _, tx := range txs {
		ids = append(ids, tx.ID)
	}
	return ids
}

func ptrDate(s string) *time.Time {
	d := dateOf(s)
	return &d
}

func ptrBool(b bool) *bool { return &b }

func TestTransactionStore(t *testing.T) {
	env := newTestEnv(t, "tx-a", "tx-b")
	tx := NewTransactionStore(env.pool)
	acct := NewAccountStore(env.pool)
	cat := NewCategoryStore(env.pool)
	if tx == nil || acct == nil || cat == nil {
		t.Fatal("non-nil pool must give non-nil stores")
	}
	a := env.user(t, "tx-a")
	b := env.user(t, "tx-b")
	ctx := env.ctx
	aid, bid := a.row.ID, b.row.ID

	// One account + one category per user: the dedup identity and the
	// scoped-reference fixtures.
	aAcct, err := acct.Create(ctx, aid, AccountIn{Name: "Tx Bank", Type: "bank", Currency: "USD"}, a.dek)
	if err != nil {
		t.Fatalf("create a account: %v", err)
	}
	bAcct, err := acct.Create(ctx, bid, AccountIn{Name: "Tx Bank B", Type: "bank", Currency: "USD"}, b.dek)
	if err != nil {
		t.Fatalf("create b account: %v", err)
	}
	aCat, err := cat.Create(ctx, aid, CategoryIn{Name: "Tx Groceries"}, a.dek)
	if err != nil {
		t.Fatalf("create a category: %v", err)
	}
	bCat, err := cat.Create(ctx, bid, CategoryIn{Name: "Tx Coffee"}, b.dek)
	if err != nil {
		t.Fatalf("create b category: %v", err)
	}

	var rtID, rtHash string // round-trip row (created below, used later)

	t.Run("dedup upsert", func(t *testing.T) {
		d := dateOf("2026-01-05")
		first, inserted, err := tx.Upsert(ctx, aid, TransactionIn{
			AccountID: aAcct.ID, DatePosted: d, Amount: "10.0", Description: "  Coffee   Shop ",
		}, a.dek)
		if err != nil || !inserted {
			t.Fatalf("first upsert: inserted=%v err=%v", inserted, err)
		}
		if first.Amount != "10.00" {
			t.Fatalf("stored amount %q, want canonical 10.00", first.Amount)
		}

		// Same dedup key, different description spacing/case and a
		// different amount spelling: no-op returning the existing row.
		again, inserted, err := tx.Upsert(ctx, aid, TransactionIn{
			AccountID: aAcct.ID, DatePosted: d, Amount: "10.00", Description: "coffee shop",
		}, a.dek)
		if err != nil || inserted {
			t.Fatalf("dup upsert: inserted=%v err=%v, want no-op", inserted, err)
		}
		if again.ID != first.ID {
			t.Fatalf("dup upsert returned %s, want existing %s", again.ID, first.ID)
		}
		if again.Description != "  Coffee   Shop " {
			t.Fatalf("existing row must keep the first writer's raw description, got %q", again.Description)
		}

		// Different date → new row.
		next, inserted, err := tx.Upsert(ctx, aid, TransactionIn{
			AccountID: aAcct.ID, DatePosted: dateOf("2026-01-06"), Amount: "10.00", Description: "coffee shop",
		}, a.dek)
		if err != nil || !inserted || next.ID == first.ID {
			t.Fatalf("different-date upsert: inserted=%v id=%s, want new row", inserted, next.ID)
		}

		// Different account → new row (dedup is per-account).
		cross, inserted, err := tx.Upsert(ctx, bid, TransactionIn{
			AccountID: bAcct.ID, DatePosted: d, Amount: "10.00", Description: "coffee shop",
		}, b.dek)
		if err != nil || !inserted || cross.ID == first.ID {
			t.Fatalf("different-account upsert: inserted=%v, want new row", inserted)
		}

		// Create on an existing dedup key → ErrDedupConflict.
		if _, err := tx.Create(ctx, aid, TransactionIn{
			AccountID: aAcct.ID, DatePosted: d, Amount: "10", Description: "coffee shop",
		}, a.dek); !errors.Is(err, ErrDedupConflict) {
			t.Fatalf("Create on dup: got %v, want ErrDedupConflict", err)
		}
	})

	t.Run("create-and-get round trip", func(t *testing.T) {
		exec := dateOf("2026-01-04")
		created, err := tx.Create(ctx, aid, TransactionIn{
			AccountID:     aAcct.ID,
			DateExecuted:  &exec,
			DatePosted:    dateOf("2026-01-05"),
			Amount:        "-12.34",
			CategoryID:    &aCat.ID,
			Description:   "Round Trip Row",
			Notes:         strPtr("a note"),
			Raw:           strPtr(`{"raw":true}`),
			ReviewPending: true,
		}, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		rtID, rtHash = created.ID, created.DedupHash

		got, err := tx.Get(ctx, aid, created.ID, a.dek)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		want := Transaction{
			ID:            created.ID,
			AccountID:     aAcct.ID,
			DateExecuted:  &exec,
			DatePosted:    dateOf("2026-01-05"),
			Amount:        "-12.34",
			CategoryID:    aCat.ID,
			Description:   "Round Trip Row",
			Notes:         "a note",
			Raw:           `{"raw":true}`,
			DedupHash:     created.DedupHash,
			ReviewPending: true,
		}
		if !txEqual(got, want) {
			t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, want)
		}
		if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
			t.Fatal("created_at/updated_at must be set")
		}

		// Nil/empty optionals land as NULL / "".
		bare, err := tx.Create(ctx, aid, TransactionIn{
			AccountID: aAcct.ID, DatePosted: dateOf("2026-01-06"), Amount: "0",
		}, a.dek)
		if err != nil {
			t.Fatalf("create bare: %v", err)
		}
		if bare.Description != "" || bare.Notes != "" || bare.Raw != "" ||
			bare.CategoryID != "" || bare.DateExecuted != nil || bare.Amount != "0.00" {
			t.Fatalf("bare row must have empty optionals, got %+v", bare)
		}

		if _, err := tx.Get(ctx, aid, zeroUUID, a.dek); !errors.Is(err, ErrTransactionNotFound) {
			t.Fatalf("get unknown id: got %v, want ErrTransactionNotFound", err)
		}
	})

	t.Run("patch round trip and clears", func(t *testing.T) {
		if rtID == "" {
			t.Fatal("round-trip subtest did not run")
		}

		// Mixed set/clear patch: &"" clears the category, the rest set.
		newNotes := "new note"
		exec2 := dateOf("2026-01-05")
		exec2Ptr := &exec2
		emptyStr := ""
		emptyStrPtr := &emptyStr
		newNotesPtr := &newNotes
		patched, err := tx.Update(ctx, aid, rtID, TransactionPatch{
			Description:   strPtr("Patched Desc"),
			Amount:        strPtr("99.9"),
			DatePosted:    ptrDate("2026-01-07"),
			DateExecuted:  &exec2Ptr,
			CategoryID:    &emptyStrPtr, // &"" clear form
			Notes:         &newNotesPtr,
			ReviewPending: ptrBool(false),
		}, a.dek)
		if err != nil {
			t.Fatalf("patch: %v", err)
		}
		if patched.Description != "Patched Desc" || patched.Amount != "99.90" ||
			!patched.DatePosted.Equal(dateOf("2026-01-07")) ||
			patched.CategoryID != "" || patched.Notes != "new note" ||
			patched.Raw != `{"raw":true}` || patched.ReviewPending ||
			patched.DateExecuted == nil || !patched.DateExecuted.Equal(dateOf("2026-01-05")) {
			t.Fatalf("patched row mismatch: %+v", patched)
		}
		if patched.DedupHash == rtHash {
			t.Fatal("desc+amount+date_posted all changed: dedup_hash must be recomputed")
		}

		// No-op patch: current row back, no UPDATE (timestamps untouched).
		noop, err := tx.Update(ctx, aid, rtID, TransactionPatch{}, a.dek)
		if err != nil {
			t.Fatalf("no-op patch: %v", err)
		}
		if !txEqual(noop, patched) {
			t.Fatalf("no-op patch must return the current row, got %+v", noop)
		}
		if !noop.UpdatedAt.Equal(patched.UpdatedAt) {
			t.Fatal("no-op patch must not touch updated_at")
		}

		// **string clears, both forms: &nil (notes) and &"" (raw),
		// plus date_executed → NULL.
		var nilStr *string
		emptyStr2 := ""
		emptyStr2Ptr := &emptyStr2
		var nilTime *time.Time
		cleared, err := tx.Update(ctx, aid, rtID, TransactionPatch{
			Notes:        &nilStr, // &nil clear form
			Raw:          &emptyStr2Ptr,
			DateExecuted: &nilTime,
		}, a.dek)
		if err != nil {
			t.Fatalf("clear patch: %v", err)
		}
		if cleared.Notes != "" || cleared.Raw != "" || cleared.DateExecuted != nil {
			t.Fatalf("clears must NULL notes/raw/date_executed, got %+v", cleared)
		}
	})

	t.Run("rehash collision", func(t *testing.T) {
		d := dateOf("2026-02-01")
		if _, err := tx.Create(ctx, aid, TransactionIn{
			AccountID: aAcct.ID, DatePosted: d, Amount: "5.00", Description: "Rehash One",
		}, a.dek); err != nil {
			t.Fatalf("create x: %v", err)
		}
		y, err := tx.Create(ctx, aid, TransactionIn{
			AccountID: aAcct.ID, DatePosted: d, Amount: "5.00", Description: "Rehash Two",
		}, a.dek)
		if err != nil {
			t.Fatalf("create y: %v", err)
		}
		// Pointing y at x's description recreates x's dedup hash.
		if _, err := tx.Update(ctx, aid, y.ID, TransactionPatch{Description: strPtr("Rehash One")}, a.dek); !errors.Is(err, ErrDedupConflict) {
			t.Fatalf("colliding rehash: got %v, want ErrDedupConflict", err)
		}
		got, err := tx.Get(ctx, aid, y.ID, a.dek)
		if err != nil || got.Description != "Rehash Two" {
			t.Fatalf("y must be unchanged, got %+v (err %v)", got, err)
		}
	})

	t.Run("validation", func(t *testing.T) {
		base := func() TransactionIn {
			return TransactionIn{
				AccountID: aAcct.ID, DatePosted: dateOf("2026-01-10"),
				Amount: "1.00", Description: "validation row",
			}
		}
		cases := []struct {
			name    string
			mut     func(*TransactionIn)
			wantErr error // nil = a plain (non-sentinel) validation error
		}{
			{"empty amount", func(in *TransactionIn) { in.Amount = "" }, nil},
			{"blank amount", func(in *TransactionIn) { in.Amount = "   " }, nil},
			{"3 fractional digits", func(in *TransactionIn) { in.Amount = "10.001" }, nil},
			{"non-decimal", func(in *TransactionIn) { in.Amount = "abc" }, nil},
			{"exponent", func(in *TransactionIn) { in.Amount = "1e2" }, nil},
			{"sign only", func(in *TransactionIn) { in.Amount = "-" }, nil},
			{"two dots", func(in *TransactionIn) { in.Amount = "1.2.3" }, nil},
			{"out of range", func(in *TransactionIn) { in.Amount = "1234567890123.45" }, nil},
			{"executed after posted", func(in *TransactionIn) {
				e := dateOf("2026-01-11")
				in.DateExecuted = &e
			}, nil},
			{"zero account", func(in *TransactionIn) { in.AccountID = zeroUUID }, ErrAccountNotFound},
			{"foreign account", func(in *TransactionIn) { in.AccountID = bAcct.ID }, ErrAccountNotFound},
			{"zero category", func(in *TransactionIn) {
				in.CategoryID = strPtr(zeroUUID)
			}, ErrCategoryNotFound},
			{"foreign category", func(in *TransactionIn) {
				in.CategoryID = &bCat.ID
			}, ErrCategoryNotFound},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				in := base()
				tc.mut(&in)
				_, err := tx.Create(ctx, aid, in, a.dek)
				if tc.wantErr != nil {
					if !errors.Is(err, tc.wantErr) {
						t.Fatalf("got %v, want %v", err, tc.wantErr)
					}
				} else if err == nil {
					t.Fatal("expected a validation error, got nil")
				}
			})
		}
		// The boundary amount that DOES fit numeric(14,2) succeeds.
		in := base()
		in.Amount = "999999999999.99"
		if _, err := tx.Create(ctx, aid, in, a.dek); err != nil {
			t.Fatalf("max in-range amount must be accepted: %v", err)
		}
	})

	// Filter/search rows: five distinct dates in March 2026 (no other
	// subtest uses those dates), two uncategorized, three categorized.
	var fIDs = map[string]string{}
	seedFilterRow := func(desc, amount string, d string, cat *string, notes *string) {
		t.Helper()
		created, err := tx.Create(ctx, aid, TransactionIn{
			AccountID: aAcct.ID, DatePosted: dateOf(d), Amount: amount,
			Description: desc, CategoryID: cat, Notes: notes,
		}, a.dek)
		if err != nil {
			t.Fatalf("seed %q: %v", desc, err)
		}
		fIDs[desc] = created.ID
	}

	t.Run("filters and pagination", func(t *testing.T) {
		seedFilterRow("Filter Alpha Mart", "-10.00", "2026-03-01", &aCat.ID, nil)
		seedFilterRow("Filter Beta Cafe", "25.50", "2026-03-02", nil, nil)
		seedFilterRow("Filter Alpha Studio", "7.25", "2026-03-03", &aCat.ID, strPtr("filter-note"))
		seedFilterRow("Filter Gamma Grocer", "100.00", "2026-03-04", nil, nil)
		seedFilterRow("Filter Delta Deli", "3.00", "2026-03-05", &aCat.ID, nil)
		march := TransactionFilter{DateFrom: ptrDate("2026-03-01")}
		wantAll := []string{fIDs["Filter Delta Deli"], fIDs["Filter Gamma Grocer"],
			fIDs["Filter Alpha Studio"], fIDs["Filter Beta Cafe"], fIDs["Filter Alpha Mart"]}
		cases := []struct {
			name string
			f    TransactionFilter
			want []string
		}{
			{"all march rows, date desc", march, wantAll},
			{"date range inclusive", TransactionFilter{
				DateFrom: ptrDate("2026-03-02"), DateTo: ptrDate("2026-03-03"),
			}, []string{fIDs["Filter Alpha Studio"], fIDs["Filter Beta Cafe"]}},
			{"categorized", TransactionFilter{
				DateFrom: ptrDate("2026-03-01"), CategoryID: strPtr(aCat.ID),
			}, []string{fIDs["Filter Delta Deli"], fIDs["Filter Alpha Studio"], fIDs["Filter Alpha Mart"]}},
			{"uncategorized", TransactionFilter{
				DateFrom: ptrDate("2026-03-01"), CategoryID: strPtr(""),
			}, []string{fIDs["Filter Gamma Grocer"], fIDs["Filter Beta Cafe"]}},
			{"amount bounds inclusive", TransactionFilter{
				DateFrom: ptrDate("2026-03-01"), MinAmount: strPtr("10"), MaxAmount: strPtr("26"),
			}, []string{fIDs["Filter Beta Cafe"]}},
			{"negative amount bound", TransactionFilter{
				DateFrom: ptrDate("2026-03-01"), MinAmount: strPtr("-100"), MaxAmount: strPtr("-5"),
			}, []string{fIDs["Filter Alpha Mart"]}},
			{"account match", TransactionFilter{
				DateFrom: ptrDate("2026-03-01"), AccountIDs: []string{aAcct.ID},
			}, wantAll},
			{"account no match", TransactionFilter{
				DateFrom: ptrDate("2026-03-01"), AccountIDs: []string{bAcct.ID},
			}, []string{}},
			{"limit 2 first page", TransactionFilter{
				DateFrom: ptrDate("2026-03-01"), Limit: 2,
			}, []string{fIDs["Filter Delta Deli"], fIDs["Filter Gamma Grocer"]}},
			{"limit 2 middle page", TransactionFilter{
				DateFrom: ptrDate("2026-03-01"), Limit: 2, Offset: 2,
			}, []string{fIDs["Filter Alpha Studio"], fIDs["Filter Beta Cafe"]}},
			{"offset past end", TransactionFilter{
				DateFrom: ptrDate("2026-03-01"), Offset: 5,
			}, []string{}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got, err := tx.List(ctx, aid, tc.f, a.dek)
				if err != nil {
					t.Fatalf("list: %v", err)
				}
				if !reflect.DeepEqual(txIDs(got), tc.want) {
					t.Fatalf("got %v, want %v", txIDs(got), tc.want)
				}
			})
		}
	})

	t.Run("search", func(t *testing.T) {
		if len(fIDs) != 5 {
			t.Fatal("filter subtest did not seed the search rows")
		}
		cases := []struct {
			name string
			f    TransactionFilter
			want []string
		}{
			{"case-insensitive word", TransactionFilter{Search: "delta"},
				[]string{fIDs["Filter Delta Deli"]}},
			{"case-insensitive uppercase", TransactionFilter{Search: "DELI"},
				[]string{fIDs["Filter Delta Deli"]}},
			{"substring plus filters", TransactionFilter{
				Search: "alpha", CategoryID: strPtr(aCat.ID), DateFrom: ptrDate("2026-03-01"),
			}, []string{fIDs["Filter Alpha Studio"], fIDs["Filter Alpha Mart"]}},
			{"search with limit", TransactionFilter{Search: "filter", Limit: 2},
				[]string{fIDs["Filter Delta Deli"], fIDs["Filter Gamma Grocer"]}},
			{"search with offset", TransactionFilter{Search: "filter", Limit: 10, Offset: 3},
				[]string{fIDs["Filter Beta Cafe"], fIDs["Filter Alpha Mart"]}},
			{"no match", TransactionFilter{Search: "zzz-no-match"},
				[]string{}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got, err := tx.List(ctx, aid, tc.f, a.dek)
				if err != nil {
					t.Fatalf("list: %v", err)
				}
				if !reflect.DeepEqual(txIDs(got), tc.want) {
					t.Fatalf("got %v, want %v", txIDs(got), tc.want)
				}
			})
		}
	})

	t.Run("per-user scoping", func(t *testing.T) {
		if rtID == "" {
			t.Fatal("round-trip subtest did not run")
		}
		if _, err := tx.Get(ctx, bid, rtID, b.dek); !errors.Is(err, ErrTransactionNotFound) {
			t.Fatalf("b get a row: got %v, want ErrTransactionNotFound", err)
		}
		if _, err := tx.Update(ctx, bid, rtID, TransactionPatch{Description: strPtr("hacked")}, b.dek); !errors.Is(err, ErrTransactionNotFound) {
			t.Fatalf("b update a row: got %v, want ErrTransactionNotFound", err)
		}
		if err := tx.Delete(ctx, bid, rtID); !errors.Is(err, ErrTransactionNotFound) {
			t.Fatalf("b delete a row: got %v, want ErrTransactionNotFound", err)
		}
		// a's row survives all of the above.
		if got, err := tx.Get(ctx, aid, rtID, a.dek); err != nil || got.Description == "hacked" {
			t.Fatalf("a row must be intact, got %+v (err %v)", got, err)
		}
		// Upsert into a's account from b is an account check failure.
		if _, _, err := tx.Upsert(ctx, bid, TransactionIn{
			AccountID: aAcct.ID, DatePosted: dateOf("2026-04-01"), Amount: "1.00", Description: "sneak",
		}, b.dek); !errors.Is(err, ErrAccountNotFound) {
			t.Fatalf("b upsert into a account: got %v, want ErrAccountNotFound", err)
		}
		// b's list contains only b's rows.
		bRows, err := tx.List(ctx, bid, TransactionFilter{Limit: maxTxLimit}, b.dek)
		if err != nil {
			t.Fatalf("b list: %v", err)
		}
		for _, got := range bRows {
			if got.ID == rtID {
				t.Fatal("b's list leaked a's row")
			}
		}
	})

	t.Run("delete", func(t *testing.T) {
		created, err := tx.Create(ctx, aid, TransactionIn{
			AccountID: aAcct.ID, DatePosted: dateOf("2026-04-02"), Amount: "2.00", Description: "doomed",
		}, a.dek)
		if err != nil {
			t.Fatalf("create doomed: %v", err)
		}
		if err := tx.Delete(ctx, aid, created.ID); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := tx.Get(ctx, aid, created.ID, a.dek); !errors.Is(err, ErrTransactionNotFound) {
			t.Fatalf("get deleted: got %v, want ErrTransactionNotFound", err)
		}
		if err := tx.Delete(ctx, aid, created.ID); !errors.Is(err, ErrTransactionNotFound) {
			t.Fatalf("double delete: got %v, want ErrTransactionNotFound", err)
		}
	})
}

func TestCanonicalAmount(t *testing.T) {
	ok := []struct{ in, want string }{
		{"10", "10.00"},
		{"-5.5", "-5.50"},
		{"007.5", "7.50"},
		{"-0.00", "0.00"},
		{"0", "0.00"},
		{"+3.20", "3.20"},
		{"  12.34  ", "12.34"},
		{".5", "0.50"},
		{"999999999999.99", "999999999999.99"},
	}
	for _, tc := range ok {
		got, err := canonicalAmount(tc.in)
		if err != nil {
			t.Errorf("canonicalAmount(%q) error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("canonicalAmount(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	bad := []string{"", "   ", "10.001", "abc", "1e2", "-", ".", "--1", "1.2.3", "1234567890123.45"}
	for _, in := range bad {
		if _, err := canonicalAmount(in); err == nil {
			t.Errorf("canonicalAmount(%q) must fail", in)
		}
	}
}

func TestNormalizeDescription(t *testing.T) {
	cases := map[string]string{
		"  Coffee   Shop ": "coffee shop",
		"ABC\tDEF\nGHI":    "abc def ghi",
		"":                 "",
		"already":          "already",
	}
	for in, want := range cases {
		if got := normalizeDescription(in); got != want {
			t.Errorf("normalizeDescription(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDedupHash(t *testing.T) {
	d := dateOf("2026-01-05")
	// Stable: same inputs → same hash.
	if h1, h2 := dedupHash("acct", d, "10.00", "coffee shop"),
		dedupHash("acct", d, "10.00", "coffee shop"); h1 != h2 {
		t.Fatal("dedupHash must be deterministic")
	}
	// Description normalization is applied inside the hash: spacing and
	// case variants collide.
	base := dedupHash("acct", d, "10.00", "  COFFEE   SHOP ")
	if base != dedupHash("acct", d, "10.00", "coffee shop") {
		t.Fatal("normalized-description invariance broken")
	}
	// Each identity component changes the hash. (The amount is the
	// CANONICAL form here — canonicalization happens in the caller.)
	if base == dedupHash("other", d, "10.00", "coffee shop") {
		t.Error("account must affect the hash")
	}
	if base == dedupHash("acct", dateOf("2026-01-06"), "10.00", "coffee shop") {
		t.Error("date must affect the hash")
	}
	if base == dedupHash("acct", d, "10.01", "coffee shop") {
		t.Error("amount must affect the hash")
	}
	if base == dedupHash("acct", d, "10.00", "tea shop") {
		t.Error("description must affect the hash")
	}
	if !strings.HasPrefix(base, "") || len(base) != 64 {
		t.Errorf("hash must be 64 hex chars, got %q", base)
	}
}

// TestTransactionAggregate exercises Aggregate (plan step 3.2): per-user
// October-2026 fixtures across categorized, uncategorized, and a
// net-zero group, plus rows just outside the period and a foreign user's
// row. One fresh testcontainer (the per-Test-fn convention).
func TestTransactionAggregate(t *testing.T) {
	env := newTestEnv(t, "agg-a", "agg-b")
	tx := NewTransactionStore(env.pool)
	acct := NewAccountStore(env.pool)
	cat := NewCategoryStore(env.pool)
	a := env.user(t, "agg-a")
	b := env.user(t, "agg-b")
	ctx := env.ctx
	aid, bid := a.row.ID, b.row.ID

	aAcct, err := acct.Create(ctx, aid, AccountIn{Name: "Agg Bank", Type: "bank", Currency: "USD"}, a.dek)
	if err != nil {
		t.Fatalf("create a account: %v", err)
	}
	bAcct, err := acct.Create(ctx, bid, AccountIn{Name: "Agg Bank B", Type: "bank", Currency: "USD"}, b.dek)
	if err != nil {
		t.Fatalf("create b account: %v", err)
	}
	groc, err := cat.Create(ctx, aid, CategoryIn{Name: "Agg Groceries"}, a.dek)
	if err != nil {
		t.Fatalf("create a groceries: %v", err)
	}
	coffee, err := cat.Create(ctx, aid, CategoryIn{Name: "Agg Coffee"}, a.dek)
	if err != nil {
		t.Fatalf("create a coffee: %v", err)
	}
	zero, err := cat.Create(ctx, aid, CategoryIn{Name: "Agg Zero"}, a.dek)
	if err != nil {
		t.Fatalf("create a zero: %v", err)
	}
	seed := func(acctID string, dek []byte, posted, amount string, category *string, desc string) {
		t.Helper()
		in := TransactionIn{
			AccountID:   acctID,
			DatePosted:  dateOf(posted),
			Amount:      amount,
			CategoryID:  category,
			Description: desc,
		}
		if _, err := tx.Create(ctx, aid, in, dek); err != nil {
			t.Fatalf("seed %s %s: %v", posted, amount, err)
		}
	}
	seedCat := func(id string, amount, posted, desc string) { seed(aAcct.ID, a.dek, posted, amount, &id, desc) }
	seedNone := func(amount, posted, desc string) { seed(aAcct.ID, a.dek, posted, amount, nil, desc) }

	// In-period fixtures (October 2026).
	seedCat(groc.ID, "-50.25", "2026-10-02", "groceries A")
	seedCat(groc.ID, "-0.75", "2026-10-03", "groceries B")
	seedCat(groc.ID, "+10", "2026-10-04", "groceries refund")
	seedCat(coffee.ID, "-3.5", "2026-10-05", "coffee A")
	seedNone("-1.23", "2026-10-06", "unfiled A")
	seedNone("+5", "2026-10-07", "unfiled B")
	seedCat(zero.ID, "+2.50", "2026-10-08", "zero A")
	seedCat(zero.ID, "-2.50", "2026-10-09", "zero B")
	// Just outside the period, in a group that would move if the date
	// bounds leaked: the zero group must stay 2.50 / 2.50 / 0.00.
	seedCat(zero.ID, "-2.50", "2026-09-30", "zero before")
	seedCat(zero.ID, "+2.50", "2026-11-01", "zero after")
	// User b, in-period but out of scope for user a's aggregation.
	if _, err := tx.Create(ctx, bid, TransactionIn{
		AccountID:   bAcct.ID,
		DatePosted:  dateOf("2026-10-15"),
		Amount:      "-100.00",
		Description: "b only",
	}, b.dek); err != nil {
		t.Fatalf("seed b row: %v", err)
	}

	got, err := tx.Aggregate(ctx, aid, dateOf("2026-10-01"), dateOf("2026-10-31"))
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	want := []CategorySpend{
		{CategoryID: "", Spend: "1.23", Income: "5.00", Net: "3.77"},
		{CategoryID: groc.ID, Spend: "51.00", Income: "10.00", Net: "-41.00"},
		{CategoryID: coffee.ID, Spend: "3.50", Income: "0.00", Net: "-3.50"},
		{CategoryID: zero.ID, Spend: "2.50", Income: "2.50", Net: "0.00"},
	}
	if len(got) != len(want) {
		t.Fatalf("want %d groups, got %d: %+v", len(want), len(got), got)
	}
	if got[0].CategoryID != "" {
		t.Errorf("uncategorized group must come first (NULLS FIRST), got %q", got[0].CategoryID)
	}
	byID := make(map[string]CategorySpend, len(got))
	for _, g := range got {
		byID[g.CategoryID] = g
	}
	for _, w := range want {
		if g, ok := byID[w.CategoryID]; !ok {
			t.Errorf("missing group for %q (all: %+v)", w.CategoryID, got)
		} else if g != w {
			t.Errorf("group %s: want %+v, got %+v", w.CategoryID, w, g)
		}
	}

	// An empty period (and a foreign user's period) yield zero groups.
	none, err := tx.Aggregate(ctx, aid, dateOf("2026-08-01"), dateOf("2026-08-31"))
	if err != nil {
		t.Fatalf("aggregate empty period: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("empty period must have no groups, got %+v", none)
	}
	other, err := tx.Aggregate(ctx, bid, dateOf("2026-10-01"), dateOf("2026-10-31"))
	if err != nil {
		t.Fatalf("aggregate user b: %v", err)
	}
	if len(other) != 1 || other[0] != (CategorySpend{CategoryID: "", Spend: "100.00", Income: "0.00", Net: "-100.00"}) {
		t.Errorf("user b aggregation wrong: %+v", other)
	}
}

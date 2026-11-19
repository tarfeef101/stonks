// Transfer, external-origin, and savings-sums tests (round 51b): one
// postgres:18 testcontainer per test (newTestEnv) covering
// CreateTransfer (mirrored legs, shared transfer_group_id, auto and
// verbatim memos, dedup-conflict rollback, validation), the Delete
// partner cleanup, external_origin round trips through Create, the
// SavingsSums buckets, plus a DB-free table test of
// parseNumericToCents.
package repo

import (
	"errors"
	"testing"
)

// transferLegsEqual compares two scanned Transaction legs value-wise
// on the stable fields: *string fields through their values,
// *time.Time through Equal. CreatedAt/UpdatedAt are excluded.
// (Transaction is not comparable — its scan field holds []byte — so
// the existing txEqual is not reusable here.)
func transferLegsEqual(got, want Transaction) bool {
	if got.ID != want.ID || got.AccountID != want.AccountID ||
		got.Amount != want.Amount || got.CategoryID != want.CategoryID ||
		got.Description != want.Description || got.Notes != want.Notes ||
		got.Raw != want.Raw || got.DedupHash != want.DedupHash ||
		got.ReviewPending != want.ReviewPending {
		return false
	}
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	if deref(got.OffsetGroupID) != deref(want.OffsetGroupID) ||
		deref(got.TransferGroupID) != deref(want.TransferGroupID) ||
		deref(got.ExternalOrigin) != deref(want.ExternalOrigin) {
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

func TestTransferCreate(t *testing.T) {
	env := newTestEnv(t, "tra")
	tx := NewTransactionStore(env.pool)
	acct := NewAccountStore(env.pool)
	a := env.user(t, "tra")
	ctx := env.ctx

	accA, err := acct.Create(ctx, a.row.ID, AccountIn{Name: "Chequing", Type: "bank", Currency: "USD"}, a.dek)
	if err != nil {
		t.Fatalf("create account A: %v", err)
	}
	accB, err := acct.Create(ctx, a.row.ID, AccountIn{Name: "RRSP", Type: "rrsp", Currency: "USD"}, a.dek)
	if err != nil {
		t.Fatalf("create account B: %v", err)
	}

	t.Run("auto memos, shared group, mirrored legs", func(t *testing.T) {
		in := TransferIn{
			FromAccountID: accA.ID,
			ToAccountID:   accB.ID,
			DatePosted:    dateOf("2024-01-15"),
			Amount:        "100.50",
		}
		fromLeg, toLeg, err := tx.CreateTransfer(ctx, a.row.ID, in, a.dek)
		if err != nil {
			t.Fatalf("create transfer: %v", err)
		}
		if fromLeg.AccountID != accA.ID || toLeg.AccountID != accB.ID {
			t.Fatalf("leg accounts: from %q, to %q", fromLeg.AccountID, toLeg.AccountID)
		}
		if fromLeg.Amount != "-100.50" || toLeg.Amount != "100.50" {
			t.Fatalf("leg amounts: %q / %q", fromLeg.Amount, toLeg.Amount)
		}
		if fromLeg.ID == toLeg.ID {
			t.Fatal("legs must have distinct ids")
		}
		if fromLeg.TransferGroupID == nil || toLeg.TransferGroupID == nil ||
			*fromLeg.TransferGroupID == "" ||
			*fromLeg.TransferGroupID != *toLeg.TransferGroupID {
			t.Fatalf("shared group: from %v, to %v", fromLeg.TransferGroupID, toLeg.TransferGroupID)
		}
		if fromLeg.Description != "Transfer to RRSP" || toLeg.Description != "Transfer from Chequing" {
			t.Fatalf("auto memos: %q / %q", fromLeg.Description, toLeg.Description)
		}
		if fromLeg.CategoryID != "" || toLeg.CategoryID != "" ||
			fromLeg.ExternalOrigin != nil || toLeg.ExternalOrigin != nil ||
			fromLeg.OffsetGroupID != nil || toLeg.OffsetGroupID != nil {
			t.Fatalf("legs must be uncategorized, non-external, non-offset: %+v / %+v", fromLeg, toLeg)
		}
		if fromLeg.DateExecuted != nil || !fromLeg.DatePosted.Equal(in.DatePosted) || !toLeg.DatePosted.Equal(in.DatePosted) {
			t.Fatalf("leg dates: %+v / %+v", fromLeg.DatePosted, toLeg.DatePosted)
		}
		if fromLeg.DedupHash == "" || fromLeg.DedupHash == toLeg.DedupHash {
			t.Fatalf("per-leg dedup hashes: %q / %q", fromLeg.DedupHash, toLeg.DedupHash)
		}
		if fromLeg.Notes != "" || fromLeg.Raw != "" || fromLeg.ReviewPending {
			t.Fatalf("from leg optionals must be unset: %+v", fromLeg)
		}

		// Round trip through Get (exercises the new txSelect columns).
		gotFrom, err := tx.Get(ctx, a.row.ID, fromLeg.ID, a.dek)
		if err != nil {
			t.Fatalf("get from leg: %v", err)
		}
		if !transferLegsEqual(gotFrom, fromLeg) {
			t.Fatalf("get from leg: want %+v, got %+v", fromLeg, gotFrom)
		}
		gotTo, err := tx.Get(ctx, a.row.ID, toLeg.ID, a.dek)
		if err != nil {
			t.Fatalf("get to leg: %v", err)
		}
		if !transferLegsEqual(gotTo, toLeg) {
			t.Fatalf("get to leg: want %+v, got %+v", toLeg, gotTo)
		}
	})

	t.Run("verbatim memo + mirrored optionals, fresh group", func(t *testing.T) {
		firstGroup := ""
		var firstFrom Transaction
		// Re-create the first transfer to capture its group id (the
		// first subtest's legs are not in scope here).
		firstFrom, _, err = tx.CreateTransfer(ctx, a.row.ID, TransferIn{
			FromAccountID: accA.ID, ToAccountID: accB.ID,
			DatePosted: dateOf("2024-01-16"), Amount: "1.00",
		}, a.dek)
		if err != nil {
			t.Fatalf("create first transfer: %v", err)
		}
		firstGroup = *firstFrom.TransferGroupID

		in := TransferIn{
			FromAccountID: accA.ID,
			ToAccountID:   accB.ID,
			DateExecuted:  ptrDate("2024-01-14"),
			DatePosted:    dateOf("2024-01-15"),
			Amount:        "42.10",
			Description:   "Xmas",
			Notes:         strPtr("holiday transfer"),
			Raw:           strPtr("RAW:xmas"),
			ReviewPending: true,
		}
		fromLeg, toLeg, err := tx.CreateTransfer(ctx, a.row.ID, in, a.dek)
		if err != nil {
			t.Fatalf("create transfer: %v", err)
		}
		if fromLeg.Description != "Xmas" || toLeg.Description != "Xmas" {
			t.Fatalf("verbatim memo: %q / %q", fromLeg.Description, toLeg.Description)
		}
		if fromLeg.Notes != "holiday transfer" || toLeg.Notes != "holiday transfer" ||
			fromLeg.Raw != "RAW:xmas" || toLeg.Raw != "RAW:xmas" ||
			!fromLeg.ReviewPending || !toLeg.ReviewPending {
			t.Fatalf("mirrored optionals: %+v / %+v", fromLeg, toLeg)
		}
		if fromLeg.DateExecuted == nil || !fromLeg.DateExecuted.Equal(*in.DateExecuted) ||
			toLeg.DateExecuted == nil || !toLeg.DateExecuted.Equal(*in.DateExecuted) {
			t.Fatalf("mirrored date_executed: %+v / %+v", fromLeg.DateExecuted, toLeg.DateExecuted)
		}
		if fromLeg.TransferGroupID == nil || *fromLeg.TransferGroupID == "" ||
			*fromLeg.TransferGroupID != *toLeg.TransferGroupID ||
			*fromLeg.TransferGroupID == firstGroup {
			t.Fatalf("fresh shared group: %v (first %q)", fromLeg.TransferGroupID, firstGroup)
		}
		if fromLeg.Amount != "-42.10" || toLeg.Amount != "42.10" {
			t.Fatalf("leg amounts: %q / %q", fromLeg.Amount, toLeg.Amount)
		}
	})
}

func TestTransferDedupConflict(t *testing.T) {
	env := newTestEnv(t, "tdd")
	tx := NewTransactionStore(env.pool)
	acct := NewAccountStore(env.pool)
	a := env.user(t, "tdd")
	ctx := env.ctx

	accA, err := acct.Create(ctx, a.row.ID, AccountIn{Name: "Alpha", Type: "bank", Currency: "USD"}, a.dek)
	if err != nil {
		t.Fatalf("create account A: %v", err)
	}
	accB, err := acct.Create(ctx, a.row.ID, AccountIn{Name: "Beta", Type: "tfsa", Currency: "USD"}, a.dek)
	if err != nil {
		t.Fatalf("create account B: %v", err)
	}

	in := TransferIn{
		FromAccountID: accA.ID,
		ToAccountID:   accB.ID,
		DatePosted:    dateOf("2024-02-01"),
		Amount:        "100.50",
	}
	if _, _, err := tx.CreateTransfer(ctx, a.row.ID, in, a.dek); err != nil {
		t.Fatalf("first transfer: %v", err)
	}
	na := countTx(t, env.pool, ctx, a.row.ID, accA.ID)
	nb := countTx(t, env.pool, ctx, a.row.ID, accB.ID)

	// Same transfer again: the first leg hits the dedup key; the whole
	// transfer rolls back and NEITHER leg persists.
	_, _, err = tx.CreateTransfer(ctx, a.row.ID, in, a.dek)
	if !errors.Is(err, ErrDedupConflict) {
		t.Fatalf("want ErrDedupConflict, got %v", err)
	}
	if got := countTx(t, env.pool, ctx, a.row.ID, accA.ID); got != na {
		t.Fatalf("account A rows: want %d, got %d", na, got)
	}
	if got := countTx(t, env.pool, ctx, a.row.ID, accB.ID); got != nb {
		t.Fatalf("account B rows: want %d, got %d", nb, got)
	}
}

func TestTransferValidation(t *testing.T) {
	env := newTestEnv(t, "tv-a", "tv-b")
	tx := NewTransactionStore(env.pool)
	acct := NewAccountStore(env.pool)
	a := env.user(t, "tv-a")
	b := env.user(t, "tv-b")
	ctx := env.ctx

	accA, err := acct.Create(ctx, a.row.ID, AccountIn{Name: "Alpha", Type: "bank", Currency: "USD"}, a.dek)
	if err != nil {
		t.Fatalf("create account A: %v", err)
	}
	accB, err := acct.Create(ctx, a.row.ID, AccountIn{Name: "Beta", Type: "tfsa", Currency: "USD"}, a.dek)
	if err != nil {
		t.Fatalf("create account B: %v", err)
	}
	accC, err := acct.Create(ctx, b.row.ID, AccountIn{Name: "Gamma", Type: "bank", Currency: "USD"}, a.dek)
	if err != nil {
		t.Fatalf("create account C: %v", err)
	}

	mk := func(from, to, amount string) (Transaction, Transaction, error) {
		return tx.CreateTransfer(ctx, a.row.ID, TransferIn{
			FromAccountID: from, ToAccountID: to,
			DatePosted: dateOf("2024-03-01"), Amount: amount,
		}, a.dek)
	}

	t.Run("same account", func(t *testing.T) {
		if _, _, err := mk(accA.ID, accA.ID, "10"); err == nil {
			t.Fatal("same-account transfer must fail")
		}
	})
	t.Run("foreign account", func(t *testing.T) {
		_, _, err := mk(accA.ID, accC.ID, "10")
		if !errors.Is(err, ErrAccountNotFound) {
			t.Fatalf("want ErrAccountNotFound, got %v", err)
		}
	})
	for _, amount := range []string{"0", "-5", "1.234"} {
		t.Run("amount "+amount, func(t *testing.T) {
			if _, _, err := mk(accA.ID, accB.ID, amount); err == nil {
				t.Fatalf("amount %q must fail", amount)
			}
		})
	}

	if got := countTx(t, env.pool, ctx, a.row.ID, accA.ID); got != 0 {
		t.Fatalf("account A rows: want 0, got %d", got)
	}
	if got := countTx(t, env.pool, ctx, a.row.ID, accB.ID); got != 0 {
		t.Fatalf("account B rows: want 0, got %d", got)
	}
	if got := countTx(t, env.pool, ctx, b.row.ID, accC.ID); got != 0 {
		t.Fatalf("account C rows: want 0, got %d", got)
	}
}

func TestTransferDeletePartnerCleanup(t *testing.T) {
	env := newTestEnv(t, "tdel")
	tx := NewTransactionStore(env.pool)
	acct := NewAccountStore(env.pool)
	a := env.user(t, "tdel")
	ctx := env.ctx

	accA, err := acct.Create(ctx, a.row.ID, AccountIn{Name: "Alpha", Type: "bank", Currency: "USD"}, a.dek)
	if err != nil {
		t.Fatalf("create account A: %v", err)
	}
	accB, err := acct.Create(ctx, a.row.ID, AccountIn{Name: "Beta", Type: "tfsa", Currency: "USD"}, a.dek)
	if err != nil {
		t.Fatalf("create account B: %v", err)
	}

	fromLeg, toLeg, err := tx.CreateTransfer(ctx, a.row.ID, TransferIn{
		FromAccountID: accA.ID, ToAccountID: accB.ID,
		DatePosted: dateOf("2024-03-05"), Amount: "250",
	}, a.dek)
	if err != nil {
		t.Fatalf("create transfer: %v", err)
	}

	// Delete the debit (from) leg; the credit leg survives, ungrouped.
	if err := tx.Delete(ctx, a.row.ID, fromLeg.ID); err != nil {
		t.Fatalf("delete from leg: %v", err)
	}
	if _, err := tx.Get(ctx, a.row.ID, fromLeg.ID, a.dek); !errors.Is(err, ErrTransactionNotFound) {
		t.Fatalf("want ErrTransactionNotFound for deleted leg, got %v", err)
	}
	partner, err := tx.Get(ctx, a.row.ID, toLeg.ID, a.dek)
	if err != nil {
		t.Fatalf("get partner: %v", err)
	}
	if partner.TransferGroupID != nil {
		t.Fatalf("partner must be ungrouped, got group %q", *partner.TransferGroupID)
	}
	if partner.Amount != "250.00" || partner.Description != toLeg.Description ||
		!partner.DatePosted.Equal(toLeg.DatePosted) {
		t.Fatalf("partner must keep amount/description/dates: %+v (was %+v)", partner, toLeg)
	}
}

func TestExternalOriginViaCreate(t *testing.T) {
	env := newTestEnv(t, "teo")
	tx := NewTransactionStore(env.pool)
	acct := NewAccountStore(env.pool)
	a := env.user(t, "teo")
	ctx := env.ctx

	acc, err := acct.Create(ctx, a.row.ID, AccountIn{Name: "RRSP", Type: "rrsp", Currency: "USD"}, a.dek)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	mk := func(day string, origin *string) (Transaction, error) {
		in := TransactionIn{
			AccountID: acc.ID, DatePosted: dateOf(day), Amount: "100",
			Description: "contribution", ExternalOrigin: origin,
		}
		return tx.Create(ctx, a.row.ID, in, a.dek)
	}

	t.Run("nil origin stays NULL", func(t *testing.T) {
		got, err := mk("2024-01-01", nil)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if got.ExternalOrigin != nil {
			t.Fatalf("want nil origin, got %q", *got.ExternalOrigin)
		}
	})
	days := []string{"2024-02-01", "2024-02-02", "2024-02-03", "2024-02-04"}
	for i, v := range []string{"employer", "government_grant", "inherited", "other"} {
		t.Run("round trip "+v, func(t *testing.T) {
			got, err := mk(days[i], strPtr(v))
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if got.ExternalOrigin == nil || *got.ExternalOrigin != v {
				t.Fatalf("want origin %q, got %v", v, got.ExternalOrigin)
			}
		})
	}
	t.Run("garbage origin rejected", func(t *testing.T) {
		if _, err := mk("2024-01-31", strPtr("moonlight")); err == nil {
			t.Fatal("garbage origin must fail")
		}
		if got := countTx(t, env.pool, ctx, a.row.ID, acc.ID); got != 5 {
			t.Fatalf("account rows: want 5 (4 valid + nil), got %d", got)
		}
	})
}

func TestSavingsSums(t *testing.T) {
	env := newTestEnv(t, "ss-a", "ss-b")
	tx := NewTransactionStore(env.pool)
	acct := NewAccountStore(env.pool)
	a := env.user(t, "ss-a")
	b := env.user(t, "ss-b")
	ctx := env.ctx

	rrsp, err := acct.Create(ctx, a.row.ID, AccountIn{Name: "RRSP", Type: "rrsp", Currency: "USD"}, a.dek)
	if err != nil {
		t.Fatalf("create rrsp: %v", err)
	}
	tfsa, err := acct.Create(ctx, b.row.ID, AccountIn{Name: "TFSA", Type: "tfsa", Currency: "USD"}, b.dek)
	if err != nil {
		t.Fatalf("create tfsa: %v", err)
	}

	mk := func(user testUser, accountID, day, amount string, origin *string) {
		t.Helper()
		if _, err := tx.Create(ctx, user.row.ID, TransactionIn{
			AccountID: accountID, DatePosted: dateOf(day), Amount: amount,
			Description: "seed", ExternalOrigin: origin,
		}, user.dek); err != nil {
			t.Fatalf("seed %s: %v", day, err)
		}
	}
	// a: one (rrsp, 2021) bucket with every origin bucket populated.
	mk(a, rrsp.ID, "2021-01-01", "200", nil)
	mk(a, rrsp.ID, "2021-02-01", "100", strPtr("employer"))
	mk(a, rrsp.ID, "2021-03-01", "50", strPtr("government_grant"))
	mk(a, rrsp.ID, "2021-04-01", "75", strPtr("inherited"))
	mk(a, rrsp.ID, "2021-05-01", "-30", strPtr("other"))
	// b: one (tfsa, 2022) bucket, contribution only.
	mk(b, tfsa.ID, "2022-06-01", "40", nil)

	sumsA, err := tx.SavingsSums(ctx, a.row.ID)
	if err != nil {
		t.Fatalf("savings sums a: %v", err)
	}
	if len(sumsA) != 1 {
		t.Fatalf("user a buckets: want exactly 1, got %d: %+v", len(sumsA), sumsA)
	}
	want := SavingsSum{
		AccountID: rrsp.ID, Year: 2021,
		Contribution: 20000, Employer: 10000, GovernmentGrant: 5000,
		Inherited: 7500, Other: -3000,
	}
	if sumsA[0] != want {
		t.Fatalf("bucket a: want %+v, got %+v", want, sumsA[0])
	}

	sumsB, err := tx.SavingsSums(ctx, b.row.ID)
	if err != nil {
		t.Fatalf("savings sums b: %v", err)
	}
	if len(sumsB) != 1 {
		t.Fatalf("user b buckets: want exactly 1, got %d: %+v", len(sumsB), sumsB)
	}
	want = SavingsSum{AccountID: tfsa.ID, Year: 2022, Contribution: 4000}
	if sumsB[0] != want {
		t.Fatalf("bucket b: want %+v, got %+v", want, sumsB[0])
	}
}

func TestParseNumericToCents(t *testing.T) {
	ok := map[string]int64{
		"0":       0,
		"0.00":    0,
		"100":     10000,
		"100.5":   10050,
		"100.50":  10050,
		"-5.50":   -550,
		"-5":      -500,
		"+7.25":   725,
		".5":      50,
		" 12.34 ": 1234,
	}
	for in, want := range ok {
		got, err := parseNumericToCents(in)
		if err != nil {
			t.Fatalf("parseNumericToCents(%q): unexpected error: %v", in, err)
		}
		if got != want {
			t.Fatalf("parseNumericToCents(%q): want %d, got %d", in, want, got)
		}
	}
	for _, in := range []string{
		"", "1.234", "abc", "1,000", "12.345", "-",
		"123456789012345678901234567890", // 30 digits: int64 overflow
	} {
		if _, err := parseNumericToCents(in); err == nil {
			t.Fatalf("parseNumericToCents(%q): expected error, got nil", in)
		}
	}
}

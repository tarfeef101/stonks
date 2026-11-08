// Mortgage store tests (plan step 7.1, migration 0009): one shared
// postgres:18 testcontainer (newTestEnv) with subtests covering the
// CRUD round trip (the whole 8-field spec sealed as ONE pack),
// created_at ordering, the whole-blob re-seal on update, the no-op
// update rule, the not-found sentinels, per-user isolation, wrong-DEK
// decryption failure, and the account FK.
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

// rowUpdatedAt reads the raw updated_at (the wire DTO omits the
// timestamps; the no-op rule is about the ROW's column).
func rowUpdatedAt(t *testing.T, pool *pgxpool.Pool, ctx context.Context, id string) time.Time {
	t.Helper()
	var ts time.Time
	if err := pool.QueryRow(ctx, `SELECT updated_at FROM mortgages WHERE id = $1`, id).Scan(&ts); err != nil {
		t.Fatalf("read updated_at: %v", err)
	}
	return ts
}

func TestMortgageStore(t *testing.T) {
	env := newTestEnv(t, "mga-a", "mga-b")
	ms := NewMortgageStore(env.pool)
	if ms == nil {
		t.Fatal("non-nil pool must give non-nil store")
	}
	if NewMortgageStore(nil) != nil {
		t.Fatal("nil pool must give nil store")
	}
	a := env.user(t, "mga-a")
	b := env.user(t, "mga-b")
	ctx := env.ctx
	aid, bid := a.row.ID, b.row.ID

	// 0009's FK: each user needs a mortgage-type account (type is
	// validated by the server layer, not the store — seed real rows).
	as := NewAccountStore(env.pool)
	acctA, err := as.Create(ctx, aid, AccountIn{Name: "Mga A", Type: "mortgage", Currency: "CAD"}, a.dek)
	if err != nil {
		t.Fatalf("seed account a: %v", err)
	}
	acctB, err := as.Create(ctx, bid, AccountIn{Name: "Mga B", Type: "mortgage", Currency: "CAD"}, b.dek)
	if err != nil {
		t.Fatalf("seed account b: %v", err)
	}

	full := MortgageIn{
		AccountID:             acctA.ID,
		Principal:             "500000.00",
		AnnualRate:            "4.99",
		AmortizationYears:     25,
		StartDate:             "2026-01-15",
		PaymentFrequency:      "monthly",
		ExtraPaymentAmount:    "0.00",
		ExtraPaymentFrequency: "monthly",
		ExtraPrepaymentType:   "term",
	}
	fullSpec := MortgageSpec{
		Principal:             full.Principal,
		AnnualRate:            full.AnnualRate,
		AmortizationYears:     full.AmortizationYears,
		StartDate:             full.StartDate,
		PaymentFrequency:      full.PaymentFrequency,
		ExtraPaymentAmount:    full.ExtraPaymentAmount,
		ExtraPaymentFrequency: full.ExtraPaymentFrequency,
		ExtraPrepaymentType:   full.ExtraPrepaymentType,
	}

	t.Run("create + get round trip", func(t *testing.T) {
		created, err := ms.Create(ctx, aid, full, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if created.ID == "" || created.UserID != aid || created.AccountID != acctA.ID || created.Spec != fullSpec {
			t.Fatalf("created: %+v", created)
		}
		got, err := ms.Get(ctx, aid, created.ID, a.dek)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got != created {
			t.Fatalf("get: want %+v, got %+v", created, got)
		}
		// No plaintext spec in the DB: the blob is a sealed pack and
		// must not contain the principal's bytes.
		var blobText string
		err = env.pool.QueryRow(ctx,
			`SELECT spec_encrypted::text FROM mortgages WHERE id = $1`,
			created.ID).Scan(&blobText)
		if err != nil {
			t.Fatalf("read blob: %v", err)
		}
		if strings.Contains(blobText, full.Principal) {
			t.Fatal("spec_encrypted contains the plaintext principal")
		}
		if err := ms.Delete(ctx, aid, created.ID); err != nil {
			t.Fatalf("cleanup delete: %v", err)
		}
	})

	t.Run("list orders by created_at", func(t *testing.T) {
		first, err := ms.Create(ctx, aid, full, a.dek)
		if err != nil {
			t.Fatalf("create 1: %v", err)
		}
		// created_at is timestamptz (microsecond resolution); the
		// second row must land strictly later for the ordering to be
		// deterministic.
		time.Sleep(10 * time.Millisecond)
		second, err := ms.Create(ctx, aid, full, a.dek)
		if err != nil {
			t.Fatalf("create 2: %v", err)
		}
		got, err := ms.List(ctx, aid, a.dek)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("list: got %d rows, want 2", len(got))
		}
		if got[0].ID != first.ID || got[1].ID != second.ID {
			t.Fatalf("list order: %+v", got)
		}
		if got[1].Spec.AmortizationYears != 25 {
			t.Fatalf("years: %+v, want 25 (JSON integer round trip)", got[1].Spec)
		}
		if err := ms.Delete(ctx, aid, first.ID); err != nil {
			t.Fatalf("cleanup delete: %v", err)
		}
		if err := ms.Delete(ctx, aid, second.ID); err != nil {
			t.Fatalf("cleanup delete: %v", err)
		}
	})

	t.Run("update changes some fields, re-seals the whole blob", func(t *testing.T) {
		created, err := ms.Create(ctx, aid, full, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		tsBefore := rowUpdatedAt(t, env.pool, ctx, created.ID)
		updated, err := ms.Update(ctx, aid, created.ID, MortgagePatch{
			AnnualRate:         strPtr("5.49"),
			AmortizationYears:  intPtr(30),
			ExtraPaymentAmount: strPtr("250.00"),
		}, a.dek)
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		want := fullSpec
		want.AnnualRate = "5.49"
		want.AmortizationYears = 30
		want.ExtraPaymentAmount = "250.00"
		if updated.Spec != want {
			t.Fatalf("updated spec: %+v, want %+v", updated.Spec, want)
		}
		if updated.AccountID != acctA.ID || updated.ID != created.ID {
			t.Fatalf("updated row: %+v", updated)
		}
		tsAfter := rowUpdatedAt(t, env.pool, ctx, updated.ID)
		if !tsAfter.After(tsBefore) {
			t.Fatalf("updated_at must move: %v -> %v", tsBefore, tsAfter)
		}
		// The re-sealed blob decrypts to the merged spec (unchanged
		// fields preserved): Get after the update round-trips it.
		got, err := ms.Get(ctx, aid, created.ID, a.dek)
		if err != nil {
			t.Fatalf("get after update: %v", err)
		}
		if got.Spec != want {
			t.Fatalf("get after update: %+v, want %+v", got.Spec, want)
		}
		if err := ms.Delete(ctx, aid, created.ID); err != nil {
			t.Fatalf("cleanup delete: %v", err)
		}
	})

	t.Run("update no-op leaves updated_at put", func(t *testing.T) {
		created, err := ms.Create(ctx, aid, full, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		tsBefore := rowUpdatedAt(t, env.pool, ctx, created.ID)
		// All-nil patch: nothing changes.
		got, err := ms.Update(ctx, aid, created.ID, MortgagePatch{}, a.dek)
		if err != nil {
			t.Fatalf("no-op (nil) update: %v", err)
		}
		if ts := rowUpdatedAt(t, env.pool, ctx, created.ID); !ts.Equal(tsBefore) {
			t.Fatalf("nil patch must not touch updated_at: %v -> %v", tsBefore, ts)
		}
		// Every field present but equal to the current spec.
		got, err = ms.Update(ctx, aid, created.ID, MortgagePatch{
			Principal:             strPtr(fullSpec.Principal),
			AnnualRate:            strPtr(fullSpec.AnnualRate),
			AmortizationYears:     intPtr(fullSpec.AmortizationYears),
			StartDate:             strPtr(fullSpec.StartDate),
			PaymentFrequency:      strPtr(fullSpec.PaymentFrequency),
			ExtraPaymentAmount:    strPtr(fullSpec.ExtraPaymentAmount),
			ExtraPaymentFrequency: strPtr(fullSpec.ExtraPaymentFrequency),
			ExtraPrepaymentType:   strPtr(fullSpec.ExtraPrepaymentType),
		}, a.dek)
		if err != nil {
			t.Fatalf("no-op (unchanged) update: %v", err)
		}
		if ts := rowUpdatedAt(t, env.pool, ctx, created.ID); !ts.Equal(tsBefore) {
			t.Fatalf("unchanged patch must not touch updated_at: %v -> %v", tsBefore, ts)
		}
		if got.Spec != fullSpec {
			t.Fatalf("row must be unchanged: %+v", got)
		}
		if err := ms.Delete(ctx, aid, created.ID); err != nil {
			t.Fatalf("cleanup delete: %v", err)
		}
	})

	t.Run("not-found sentinels", func(t *testing.T) {
		ca, err := ms.Create(ctx, aid, full, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		// Foreign id (user b's row, seen as a) and bogus id.
		if _, err := ms.Get(ctx, bid, ca.ID, b.dek); !errors.Is(err, ErrMortgageNotFound) {
			t.Fatalf("get foreign: got %v, want ErrMortgageNotFound", err)
		}
		if _, err := ms.Update(ctx, bid, ca.ID, MortgagePatch{AnnualRate: strPtr("9.99")}, b.dek); !errors.Is(err, ErrMortgageNotFound) {
			t.Fatalf("update foreign: got %v, want ErrMortgageNotFound", err)
		}
		if err := ms.Delete(ctx, bid, ca.ID); !errors.Is(err, ErrMortgageNotFound) {
			t.Fatalf("delete foreign: got %v, want ErrMortgageNotFound", err)
		}
		if _, err := ms.Get(ctx, aid, zeroUUID, a.dek); !errors.Is(err, ErrMortgageNotFound) {
			t.Fatalf("get bogus: got %v, want ErrMortgageNotFound", err)
		}
		if _, err := ms.Update(ctx, aid, zeroUUID, MortgagePatch{AnnualRate: strPtr("9.99")}, a.dek); !errors.Is(err, ErrMortgageNotFound) {
			t.Fatalf("update bogus: got %v, want ErrMortgageNotFound", err)
		}
		if err := ms.Delete(ctx, aid, zeroUUID); !errors.Is(err, ErrMortgageNotFound) {
			t.Fatalf("delete bogus: got %v, want ErrMortgageNotFound", err)
		}
		// The row must be intact through all the failed attempts.
		if _, err := ms.Get(ctx, aid, ca.ID, a.dek); err != nil {
			t.Fatalf("row must be intact: %v", err)
		}
		if err := ms.Delete(ctx, aid, ca.ID); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
	})

	t.Run("per-user scoping", func(t *testing.T) {
		created, err := ms.Create(ctx, aid, full, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := ms.Get(ctx, bid, created.ID, b.dek); !errors.Is(err, ErrMortgageNotFound) {
			t.Fatalf("other user's get: got %v, want ErrMortgageNotFound", err)
		}
		if _, err := ms.Update(ctx, bid, created.ID, MortgagePatch{Principal: strPtr("1.00")}, b.dek); !errors.Is(err, ErrMortgageNotFound) {
			t.Fatalf("other user's update: got %v, want ErrMortgageNotFound", err)
		}
		if err := ms.Delete(ctx, bid, created.ID); !errors.Is(err, ErrMortgageNotFound) {
			t.Fatalf("other user's delete: got %v, want ErrMortgageNotFound", err)
		}
		// b's own row (on b's account) must not appear in a's list, and
		// a's row must not appear in b's.
		inB := full
		inB.AccountID = acctB.ID
		mine, err := ms.Create(ctx, bid, inB, b.dek)
		if err != nil {
			t.Fatalf("create b: %v", err)
		}
		la, err := ms.List(ctx, aid, a.dek)
		if err != nil {
			t.Fatalf("list a: %v", err)
		}
		lb, err := ms.List(ctx, bid, b.dek)
		if err != nil {
			t.Fatalf("list b: %v", err)
		}
		for _, x := range la {
			if x.ID == mine.ID {
				t.Fatal("other user's mortgage leaked into a's list")
			}
		}
		for _, x := range lb {
			if x.ID == created.ID {
				t.Fatal("other user's mortgage leaked into b's list")
			}
		}
		if _, err := ms.Get(ctx, aid, created.ID, a.dek); err != nil {
			t.Fatalf("row must be intact: %v", err)
		}
		if err := ms.Delete(ctx, aid, created.ID); err != nil {
			t.Fatalf("cleanup delete: %v", err)
		}
		if err := ms.Delete(ctx, bid, mine.ID); err != nil {
			t.Fatalf("cleanup delete b: %v", err)
		}
	})

	t.Run("wrong DEK: decryption failure is an error, not not-found", func(t *testing.T) {
		created, err := ms.Create(ctx, aid, full, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		// The other user's DEK cannot open this row's pack.
		if _, err := ms.Get(ctx, aid, created.ID, b.dek); err == nil {
			t.Fatal("wrong-DEK get must fail")
		} else if errors.Is(err, ErrMortgageNotFound) {
			t.Fatalf("wrong-DEK get: got the not-found sentinel, want a decryption error: %v", err)
		}
		if _, err := ms.List(ctx, aid, b.dek); err == nil {
			t.Fatal("wrong-DEK list must fail")
		} else if errors.Is(err, ErrMortgageNotFound) {
			t.Fatalf("wrong-DEK list: got the not-found sentinel, want a decryption error: %v", err)
		}
		if _, err := ms.Update(ctx, aid, created.ID, MortgagePatch{AnnualRate: strPtr("5.00")}, b.dek); err == nil {
			t.Fatal("wrong-DEK update must fail")
		} else if errors.Is(err, ErrMortgageNotFound) {
			t.Fatalf("wrong-DEK update: got the not-found sentinel, want a decryption error: %v", err)
		}
		// A fresh DEK never wrapped for this user fails the same way.
		fresh, err := crypto.NewDEK()
		if err != nil {
			t.Fatalf("fresh DEK: %v", err)
		}
		if _, err := ms.Get(ctx, aid, created.ID, fresh); err == nil {
			t.Fatal("fresh-DEK get must fail")
		}
		// The row is still intact under the right DEK.
		if _, err := ms.Get(ctx, aid, created.ID, a.dek); err != nil {
			t.Fatalf("row must be intact: %v", err)
		}
		if err := ms.Delete(ctx, aid, created.ID); err != nil {
			t.Fatalf("cleanup delete: %v", err)
		}
	})

	t.Run("FK: ghost account id fails", func(t *testing.T) {
		in := full
		in.AccountID = zeroUUID // syntactically valid, no such row
		if _, err := ms.Create(ctx, aid, in, a.dek); err == nil {
			t.Fatal("create with a ghost account_id must fail (FK)")
		}
		if _, err := ms.List(ctx, aid, a.dek); err != nil {
			t.Fatalf("list must still work: %v", err)
		}
	})
}

func intPtr(i int) *int { return &i }

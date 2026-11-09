package fisync

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// wantFakeTx is the fake adapter's pinned output (see fake.go): the
// e2e and the docs/ADDING-A-FI.md dedup section rely on these exact
// quadruples.
var wantFakeTx = []IncomingTransaction{
	{
		ExternalID:  "fake-tx-1",
		AccountName: "Fake Bank Account",
		DatePosted:  time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC),
		Amount:      "-42.13",
		Description: "FAKE SUBSCRIPTION",
	},
	{
		ExternalID:  "fake-tx-2",
		AccountName: "Fake Bank Account",
		DatePosted:  time.Date(2026, 1, 12, 0, 0, 0, 0, time.UTC),
		Amount:      "1500.00",
		Description: "FAKE PAYROLL",
	},
}

func TestFakeAdapterPinnedDeterministic(t *testing.T) {
	a := NewFakeAdapter(false)
	if a.Code() != "fake-bank" {
		t.Fatalf("code = %q, want fake-bank", a.Code())
	}
	ctx := context.Background()
	cred := TokenCredentials{AccessToken: "ignored", RefreshToken: "ignored"}
	now := time.Date(2026, 11, 8, 9, 5, 0, 0, time.UTC)

	first, err := a.Sync(ctx, cred, now)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !reflect.DeepEqual(first, wantFakeTx) {
		t.Fatalf("first sync: got %+v, want the pinned transactions", first)
	}
	// Determinism: a second call (different now, same tokens) returns
	// an equal slice — stability is what makes repeated ticks dedup to
	// zero new rows.
	second, err := a.Sync(ctx, cred, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("second sync differs from first: got %+v, want %+v", second, first)
	}
	// Independence: the result is a fresh copy, so mutating it cannot
	// poison the next tick.
	first[0].Amount = "tampered"
	third, err := a.Sync(ctx, cred, now)
	if err != nil {
		t.Fatalf("third sync: %v", err)
	}
	if third[0].Amount != "-42.13" {
		t.Fatalf("third sync first amount = %q, want -42.13 (pinned data leaked)", third[0].Amount)
	}
}

func TestFakeAdapterFailMode(t *testing.T) {
	a := NewFakeAdapter(true)
	if a.Code() != "fake-bank" {
		t.Fatalf("code = %q, want fake-bank", a.Code())
	}
	got, err := a.Sync(context.Background(), TokenCredentials{}, time.Time{})
	if err == nil || err.Error() != "fake adapter: simulated failure" {
		t.Fatalf("fail sync: err = %v, want the pinned 'fake adapter: simulated failure'", err)
	}
	if got != nil {
		t.Fatalf("fail sync: got %+v, want nil", got)
	}
}

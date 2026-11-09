package fisync

import (
	"context"
	"errors"
	"time"
)

// fakeFailErr is the pinned failure the fail-mode fake adapter returns
// (the 9.2 e2e S6 asserts on it through the error notification).
var fakeFailErr = errors.New("fake adapter: simulated failure")

// fakeTx is the fake adapter's pinned, deterministic output: the same
// (account, date_posted, amount, description) quadruples on every call,
// so repeated ticks dedup to zero new rows (stability is what makes the
// hourly re-sync safe).
var fakeTx = []IncomingTransaction{
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

type fakeAdapter struct {
	fail bool
}

// NewFakeAdapter returns the dev-only fake institution: Code() is
// "fake-bank" (the 9.1 e2e institution code) and Sync ignores its
// tokens and now and returns the two pinned transactions. fail=true
// makes Sync return the pinned error instead. It is registered only
// behind the FI_FAKE_ADAPTERS dev gate in cmd/worker (no env gate for
// real adapters — docs/ADDING-A-FI.md).
func NewFakeAdapter(fail bool) InstitutionAdapter {
	return &fakeAdapter{fail: fail}
}

// Code returns "fake-bank".
func (f *fakeAdapter) Code() string { return "fake-bank" }

// Sync returns a fresh copy of the pinned transactions (callers may
// mutate the result; the pinned data must not leak across ticks), or
// the pinned failure when built with fail=true.
func (f *fakeAdapter) Sync(context.Context, TokenCredentials, time.Time) ([]IncomingTransaction, error) {
	if f.fail {
		return nil, fakeFailErr
	}
	return append([]IncomingTransaction(nil), fakeTx...), nil
}

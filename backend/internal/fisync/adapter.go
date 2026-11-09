// Package fisync is the step 9.2 FI-sync framework: the
// InstitutionAdapter contract, the adapter Registry, the dev-only fake
// adapter, and the Job that syncs every user's active credentials on
// each hourly tick. Worker-only (no routes, no server ripple, never
// reads env): cmd/worker wires it under the 8.3 advisory-lock schedule
// (lock key 71002, :05 past the hour). Per-institution guidance lives
// in docs/ADDING-A-FI.md.
package fisync

import (
	"context"
	"time"
)

// IncomingTransaction is one raw transaction an adapter reports.
type IncomingTransaction struct {
	ExternalID  string    // stable per institution; logs/diagnostics ONLY, not a dedup key
	AccountName string    // external account label; the job maps it to a local account by plaintext name
	DatePosted  time.Time // UTC
	Amount      string    // canonical signed decimal string, e.g. "-42.13", "1500.00"
	Description string
}

// TokenCredentials are the decrypted credential secrets for one institution.
type TokenCredentials struct {
	AccessToken  string
	RefreshToken string
}

// InstitutionAdapter talks to one institution. Implementations are registered
// with the Registry; the job calls Sync with the decrypted tokens and the
// (possibly faked) current time.
type InstitutionAdapter interface {
	Code() string
	Sync(ctx context.Context, cred TokenCredentials, now time.Time) ([]IncomingTransaction, error)
}

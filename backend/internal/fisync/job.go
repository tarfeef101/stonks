package fisync

import (
	"context"
	"time"

	"finance-tracker/internal/crypto"
	"finance-tracker/internal/repo"
)

// Job is one FI-sync pass over ALL users' credentials (the 9.2 sync framework's
// worker entry point; the 8.3 hourly :05 hook drives it under advisory lock 71002).
type Job struct {
	credentials  *repo.FICredentialStore
	institutions *repo.InstitutionStore // kept for the ADDING-A-FI lifecycle docs; ListAll does the join
	accounts     *repo.AccountStore
	transactions *repo.TransactionStore
	notifs       *repo.NotificationStore
	registry     *Registry
	DEKForUser   func(ctx context.Context, userID string) ([]byte, error) // wired from cmd/worker's existing KEK unwrap
}

// NewJob returns a Job over the given stores, registry, and DEK resolver.
func NewJob(credentials *repo.FICredentialStore, institutions *repo.InstitutionStore,
	accounts *repo.AccountStore, transactions *repo.TransactionStore,
	notifs *repo.NotificationStore, registry *Registry,
	dekForUser func(ctx context.Context, userID string) ([]byte, error)) *Job {
	return &Job{
		credentials:  credentials,
		institutions: institutions,
		accounts:     accounts,
		transactions: transactions,
		notifs:       notifs,
		registry:     registry,
		DEKForUser:   dekForUser,
	}
}

// RunOnce performs one full FI-sync pass. The only error it returns is a ListAll
// failure (the 8.3 hook/scheduler logs it); per-credential failures NEVER fail
// RunOnce — they become notifications and the batch continues (8.3 pattern).
func (j *Job) RunOnce(ctx context.Context, now time.Time) error {
	rows, err := j.credentials.ListAll(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		j.syncRow(ctx, row, now)
	}
	return nil
}

// syncRow syncs ONE credential. Every per-credential failure writes one
// "error" notification for the credential's user and stops the row (the
// batch continues with the next row); a successful pass stamps
// last_synced_at. The one exception: a MarkSynced failure is dropped by
// design — the sync itself succeeded, and the next tick re-syncs and
// dedups to zero new rows.
func (j *Job) syncRow(ctx context.Context, row repo.CredentialRow, now time.Time) {
	dek, err := j.DEKForUser(ctx, row.UserID)
	if err != nil {
		j.notify(ctx, row.UserID, "FI sync failed for "+row.InstitutionCode, err.Error())
		return
	}
	adapter, ok := j.registry.Get(row.InstitutionCode)
	if !ok {
		j.notify(ctx, row.UserID,
			"no adapter registered for institution "+row.InstitutionCode,
			"institution code has no registered adapter")
		return
	}
	accessToken, err := crypto.Open(dek, row.TokenPack)
	if err != nil {
		j.notify(ctx, row.UserID, "FI sync failed for "+row.InstitutionCode, "decrypt: "+err.Error())
		return
	}
	refreshToken, err := crypto.Open(dek, row.RefreshTokenPack)
	if err != nil {
		j.notify(ctx, row.UserID, "FI sync failed for "+row.InstitutionCode, "decrypt: "+err.Error())
		return
	}
	incoming, err := adapter.Sync(ctx, TokenCredentials{
		AccessToken:  string(accessToken),
		RefreshToken: string(refreshToken),
	}, now)
	if err != nil {
		j.notify(ctx, row.UserID, "FI sync failed for "+row.InstitutionCode, err.Error())
		return
	}
	// Account mapping: each DISTINCT external account label maps to ONE
	// local account by exact plaintext name match (reuse); an unmatched
	// label creates one (type bank, USD, no subtype/schema). An account
	// List/Create failure skips the remaining incoming txs for this
	// credential (no MarkSynced).
	accts, err := j.accounts.List(ctx, row.UserID, dek)
	if err != nil {
		j.notify(ctx, row.UserID, "FI sync failed for "+row.InstitutionCode, "account: "+err.Error())
		return
	}
	local := make(map[string]string, len(incoming))
	for _, tx := range incoming {
		if _, done := local[tx.AccountName]; done {
			continue
		}
		id, found := "", false
		for _, a := range accts {
			if a.Name == tx.AccountName {
				id, found = a.ID, true
				break
			}
		}
		if !found {
			created, cerr := j.accounts.Create(ctx, row.UserID, repo.AccountIn{
				Name:     tx.AccountName,
				Type:     "bank",
				Currency: "USD",
			}, dek)
			if cerr != nil {
				j.notify(ctx, row.UserID, "FI sync failed for "+row.InstitutionCode, "account: "+cerr.Error())
				return
			}
			id = created.ID
		}
		local[tx.AccountName] = id
	}
	// Each incoming transaction goes through the Phase 4 dedup upsert:
	// the inserted bool is irrelevant — repeated ticks dedup to zero new
	// rows by design. A per-tx failure notifies and the rest still go.
	for _, tx := range incoming {
		if _, _, uerr := j.transactions.Upsert(ctx, row.UserID, repo.TransactionIn{
			AccountID:   local[tx.AccountName],
			DatePosted:  tx.DatePosted,
			Amount:      tx.Amount,
			Description: tx.Description,
		}, dek); uerr != nil {
			j.notify(ctx, row.UserID, "FI sync failed for "+row.InstitutionCode, "upsert: "+uerr.Error())
			continue
		}
	}
	// Success path (d-e-f finished without an adapter/account error):
	// stamp last_synced_at; a stamp failure is dropped (see syncRow).
	_ = j.credentials.MarkSynced(ctx, row.UserID, row.ID, now)
}

// notify writes one "error" notification (the job's only user-facing
// surface: the job writes NO logs — the 8.3 scheduler logs RunOnce's
// own error). A notification write failure is dropped: it must not
// abort the batch, and there is no further surface to record it on.
func (j *Job) notify(ctx context.Context, userID, title, body string) {
	if _, err := j.notifs.Create(ctx, userID, repo.NotificationIn{
		Kind:  "error",
		Title: title,
		Body:  body,
	}); err != nil {
		// dropped by design (see above)
	}
}

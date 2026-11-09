# Adding an FI institution (step 9.2)

The FI-sync framework lives in `backend/internal/fisync` (worker-only: no
routes, no server ripple, never reads env). This doc is the runbook for
wiring a new institution adapter end to end.

## 1. The InstitutionAdapter contract

`InstitutionAdapter` (fisync/adapter.go) is two methods:

- `Code() string` — the institution `code` from the global
  `fi_institutions` registry (9.1). One adapter per code; a duplicate code
  panics at registry construction (programmer error, loud at startup).
- `Sync(ctx, TokenCredentials, now) ([]IncomingTransaction, error)` — pull
  the institution's transactions for the decrypted tokens at time `now`.

**Determinism is the contract.** The dedup upsert (Phase 4) keys on the
stable quadruple `(account, date_posted, amount, description)`: an adapter
that returns the same quadruples on repeated ticks produces ZERO new rows
on every tick after the first (the fake adapter's whole e2e relies on
this). Anything unstable — server-assigned IDs, timestamps, "last N days"
windows that slide — must be normalized before being returned.
`IncomingTransaction.ExternalID` is for logs/diagnostics ONLY; it is never
a dedup key (the local identity is the quadruple above).

## 2. Registering an adapter

Adapters are registered once in `cmd/worker/main.go` in the `fiAdapters`
list, which feeds `fisync.NewRegistry(fiAdapters...)`. Real adapters are
added unconditionally — there is NO env gate for them. The dev-only fake
adapter (`fisync.NewFakeAdapter(fail)`, code `fake-bank`) is the exception:
it is appended only when `FI_FAKE_ADAPTERS=1` (and fails its sync when
`FI_FAKE_ADAPTER_FAIL=1`) so it can never run against real credentials.

## 3. Credential + institution model (9.1)

Institutions are a global, unencrypted registry (`POST /v1/fi/institutions`);
credentials are per-user. The user pastes an access token + refresh token
manually; each secret is sealed as ONE DEK-encrypted pack (two packs per
`fi_credentials` row), and secrets never travel on the wire in plaintext.
`last_synced_at` is NULL until the first successful sync.

Account mapping: each DISTINCT external `AccountName` an adapter reports
maps to ONE local account, resolved by exact plaintext name match against
the user's accounts; an unmatched name creates one local account with
`type 'bank'`, `currency USD`, `Subtype`/`DefaultSchemaID` nil. Iteration 2
upgrades this to a per-credential multi-account mapping (the single-map
rule is what the 9.2 job and e2e pin).

## 4. Sync lifecycle

The 8.3 scheduler fires the `fi-sync` entry hourly at `:05` UTC (spec
`5 * * * *`) under leader advisory lock **71002**; the locked run calls
`Job.RunOnce(ctx, now)`, which iterates EVERY user's credentials whose
institution is `status='active'` (disabled institutions are skipped
silently by `FICredentialStore.ListAll`). `last_synced_at` is stamped only
on a fully successful per-credential pass. Every per-credential failure
writes ONE notification `Kind "error"` and the batch continues (never fails
the tick); pinned titles/bodies: `"FI sync failed for "+code` (body = the
underlying error, prefixed `decrypt:`/`account:`/`upsert:` for that stage)
and `"no adapter registered for institution "+code` (body `"institution
code has no registered adapter"`). A `MarkSynced` failure is dropped by
design — the next tick re-syncs and dedups to zero.

## 5. Testing

The fake-adapter e2e (`/tmp/opencode/e2e_92/e2e_92.sh`, S0-S8) is the
template for a real adapter's e2e: create the institution + credential via
the API, force a tick at a pinned `WORKER_FAKE_NOW` instant, assert
`last_synced_at` + row counts, force a second tick and assert the dedup
(still N rows), force a failing tick and assert the pinned error
notification with `last_synced_at` UNADVANCED, then tear the credential
and account down and assert the baseline counts. `go test ./...` covers
the unit level (registry/fake/job in `backend/internal/fisync`, `ListAll`
in `repo`).

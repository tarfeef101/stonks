# Operator runbook (plan §10.5)

Production operations for the Personal Finance Tracker on the operator's
k3s cluster: first install, New Relic opt-in, backups + restore drill,
KEK rotation, LLM model updates, and upgrade/rollback.

Audience: the operator (cluster + AWS + IdP owner). Everything here runs
from the operator's workstation; no toolchain is needed on the host — the
only container invocations are `golang:1.27.3` (KEK rotation, §5) and
`postgres:18.x` (restore drill, §4.3), both via Docker.

Cluster facts this runbook assumes (verified 2026-11-25/26, read-only
inspection):

| Fact | Value |
|---|---|
| k3s | v1.37.1+k3s1, 4 nodes (192.168.0.6–.9) |
| Storage | Longhorn v1.13.0, SC `longhorn` (cluster default) |
| Gateway | Envoy Gateway v1.9.3, ns `envoy`; NodePorts 30080 (http) / 30443 (https) |
| Edge objects | Gateway `global` (section `http`, :80, `allowedRoutes: All`) + ListenerSet `tareef-tech` (sections `https`/`https-root`, :443 Terminate) |
| TLS | Secret `tareef-tech-wildcard-tls` in ns `envoy` — dnsNames `tareef.tech` + `*.tareef.tech`, so `stonks.tareef.tech` is covered |
| App DNS | `stonks.tareef.tech` (A → the public edge IP) |

The chart creates **nothing** in `envoy` and **no** ListenerSet /
Certificate / Issuer / ReferenceGrant (D-034).

## 1. Prerequisites

1. **IdP.** Production Auth0 tenant stood up via `infra/auth0` (see
   `docs/AUTH0-SETUP.md`) — the module creates the SPA `finance-spa`
   (PKCE) and the API `finance-api`, whose identifier is literally
   `finance-api`; that identifier is the value of `OIDC_AUDIENCE` (the
   backend requires it at boot — §1.2). Note the tenant `issuer` URL
   and the SPA `client_id` from the Terraform outputs.
2. **Secrets.** SSM parameters created (`infra/aws-ssm`, `enabled = true`)
   and **values set out-of-band** per `docs/AUTH0-SETUP.md` — 16
   SecureStrings under `/finance-tracker/`:

   ```
   KEK  DATABASE_URL  OIDC_ISSUER  OIDC_CLIENT_ID  OIDC_CLIENT_SECRET
   NR_LICENSE_KEY  NR_APPLICATION_ID
   SMTP_HOST  SMTP_PORT  SMTP_USER  SMTP_PASS  SMTP_FROM  SMTP_TLS_POLICY
   LLM_BASE_URL  LLM_API_KEY  LLM_MODEL
   ```

   Then create the k8s Secret **`stonks-secrets`** in namespace `stonks`
   from those values (the chart takes the *name* only, never values —
   D-033). Four keys are **not** SSM parameters: the
   `POSTGRES_USER` / `POSTGRES_DB` / `POSTGRES_PASSWORD` triple (choose
   strong values) and `OIDC_AUDIENCE` (the `finance-api` identifier from
   §1.1 — the backend fails to start without it):

   ```sh
   kubectl create namespace stonks
   kubectl -n stonks create secret generic stonks-secrets \
     --from-literal=KEK="$(make gen-kek)" \
     --from-literal=DATABASE_URL='postgres://ftrk:PASSWORD@<pg-host>:5432/ftrk' \
     --from-literal=OIDC_ISSUER='https://<tenant>.auth0.com/' \
     --from-literal=OIDC_CLIENT_ID='<spa-client-id>' \
     --from-literal=OIDC_AUDIENCE='finance-api' \
     --from-literal=OIDC_CLIENT_SECRET='' \
     --from-literal=NR_LICENSE_KEY='' --from-literal=NR_APPLICATION_ID='' \
     --from-literal=SMTP_HOST='' --from-literal=SMTP_PORT='' \
     --from-literal=SMTP_USER='' --from-literal=SMTP_PASS='' \
     --from-literal=SMTP_FROM='' --from-literal=SMTP_TLS_POLICY='' \
     --from-literal=LLM_BASE_URL='' --from-literal=LLM_API_KEY='' \
     --from-literal=LLM_MODEL='' \
     --from-literal=POSTGRES_USER=ftrk \
     --from-literal=POSTGRES_DB=ftrk \
     --from-literal=POSTGRES_PASSWORD='PASSWORD'
   ```

   (Empty values = feature off. A Terraform-managed
   SSM→Secret sync is fine too — the chart does not care how the Secret
   is maintained, only that the keys exist.) The full key contract is in
   `deploy/chart/stonks/README.md`.
3. **Edge.** Public IP with **80 and 16443** forwarded to the Envoy
   NodePorts 30080/30443 on the cluster (the 16443→30443 forward is an
   operator item — the cluster has no 16443 Service), and a DNS A record
   `stonks.tareef.tech` → that IP.
4. **Artifacts.** Images + chart pushed to GHCR from the git SHA you are
   deploying (`make images-push && make chart-push`, operator-gated).
   All three packages are public → anonymous pull, no
   `imagePullSecrets` (D-033).

## 2. Install

### 2.1 Versions to record (plan §10.1)

| Component | Verified value | When |
|---|---|---|
| Postgres minor (image `postgres:18` resolves to) | **18.6** | 2026-11-26 (`docker run --rm postgres:18 postgres --version`) |
| Chart version | **0.1.2** (`oci://ghcr.io/tarfeef101/stonks-chart`, digest `sha256:ae6079cfea4e6786e0be0a2d0a8c977289d57f99a38cd7e72d08aefff72cdfb7`) | 2026-11-26 |

Pin `postgres.image.tag: "18.6"` in the release values (re-verify and
bump the pin at each deploy if `postgres:18` moved; keep the backup
CronJob's `image:` in sync — §4.1), and pin the chart `version` in the
Terraform block below.

### 2.2 Terraform

```hcl
resource "helm_release" "stonks" {
  name             = "stonks"
  chart            = "oci://ghcr.io/tarfeef101/stonks-chart"
  version          = "0.1.2"
  create_namespace = true
  namespace        = "stonks"

  # SENTINEL OVERRIDE — both MUST be the git short SHA the images were
  # built + pushed from (the chart's `latest` defaults never exist).
  set { name = "images.backend.tag"  value = "<git-short-sha>" }
  set { name = "images.frontend.tag" value = "<git-short-sha>" }
  set { name = "postgres.image.tag"  value = "18.6" }
}
```

`terraform apply` runs the full install: namespace → 15 chart objects →
the `<release>-migrate` **post-install** hook Job (`goose up`, retries
60×5 s while Postgres initialises; D-035). The backend `/readyz` fails
until the schema is current, so no traffic lands on an unmigrated DB.

### 2.3 First-install verification battery

```sh
# 6/6 pods: backend x2, frontend x2, worker x1, postgres x1
kubectl -n stonks get pods
kubectl -n stonks get jobs          # stonks-migrate: Complete

# The app route attached to the ListenerSet (r69 — kind must be ListenerSet)
kubectl -n stonks get httproute stonks -o \
  'jsonpath={range .status.parents[*]}{.conditions[?(@.type=="Accepted")].status}{" "}{.conditions[?(@.type=="ResolvedRefs")].status}{"\n"}{end}'
# expect: True True

# Edge probes (TLS MUST pin the IP — SNI is the hostname, an IP SNI RSTs):
R='--resolve stonks.tareef.tech:30443:192.168.0.8'
curl -sS -o /dev/null -w '%{http_code}\n' $R https://stonks.tareef.tech/          # 200 (SPA)
curl -sS -o /dev/null -w '%{http_code}\n' $R https://stonks.tareef.tech/api       # 401 (auth guard)
curl -sS $R https://stonks.tareef.tech/api/v1/public-config                        # {oidc:{issuer,client_id,audience}}
curl -sSI $R https://stonks.tareef.tech/ | grep -i content-security-policy \
  | grep -oE '(connect-src|frame-src)[^;]*'   # the IdP origin must appear in BOTH directives (D-036)
curl -sS -o /dev/null -w '%{http_code} %{redirect_url}\n' \
  --resolve stonks.tareef.tech:30080:192.168.0.8 \
  http://stonks.tareef.tech/some/path
# -> 301 https://stonks.tareef.tech:16443/some/path — the redirect is
# absolute: scheme + hostname + port 16443 (values.ingress.redirectPort,
# the public HTTPS port from §1.3), path preserved
```

## 3. New Relic (opt-in telemetry)

Off by default with **zero network cost**: without `NR_LICENSE_KEY` the
backend go-agent is disabled at config time, the browser agent is never
imported, and the rendered CSP carries no `bam.nr-data.net` entry.

| Layer | Gate | Behaviour |
|---|---|---|
| Backend go-agent (`cmd/api`) | `NR_LICENSE_KEY` set | app `finance-tracker-api` reports APM data |
| Frontend browser agent | `NR_LICENSE_KEY` **AND** `NR_APPLICATION_ID` (via `/api/v1/public-config`) | dynamic import of `@newrelic/browser-agent` with `{licenseKey, applicationID}`; CSP `connect-src` gains `' https://bam.nr-data.net'` (rendered by the nginx entrypoint at container start) |
| Worker | none | no agent (cron loops only) |

The browser license key is inherently public in a browser app — NR's
own copy/paste install method puts it in a `<script>` tag on the public
page — so the app simply follows that pattern. It only scopes incoming
data to your account; NR additionally lets you block specific domains
from beaconing with it.

**Turn on:**

1. NR account (free tier: 100 GB ingest/month, 1 full platform user).
   Add a browser app: **one.newrelic.com → All capabilities → Browser →
   + Add browser app**, using the **copy/paste** install method (only
   that method keeps the generated JavaScript on the Browser → Settings
   page). At the end of the setup the generated JS snippet shows the
   `licenseKey` and the `applicationID` — copy both.
2. `NR_LICENSE_KEY` = the snippet's `licenseKey`; `NR_APPLICATION_ID`
   = the snippet's `applicationID`. Per NR docs the browser agent's
   license key is the account's **browser monitoring key** — distinct
   from the ingest license key in the API keys UI. The app uses this
   one value for BOTH layers (go-agent + browser agent); if the NR UI
   later shows RUM but no APM data, the account's ingest key differs
   and that is why.
3. Set both SSM parameters (out-of-band, never via Terraform):

   ```sh
   aws ssm put-parameter --name /finance-tracker/NR_LICENSE_KEY \
     --type SecureString --value '<key>' --overwrite
   aws ssm put-parameter --name /finance-tracker/NR_APPLICATION_ID \
     --type SecureString --value '<id>' --overwrite
   kubectl -n stonks patch secret stonks-secrets --type merge \
     -p "{\"data\":{\"NR_LICENSE_KEY\":\"$(printf '%s' '<key>' | base64 -w0)\",\"NR_APPLICATION_ID\":\"$(printf '%s' '<id>' | base64 -w0)\"}}"
   ```

4. `kubectl -n stonks rollout restart deploy stonks-backend stonks-frontend`
5. Verify: backend log shows the New Relic agent connecting; the browser
   Network tab shows a `bam.nr-data.net` beacon on page load;
   `curl -sSI $R https://stonks.tareef.tech/ | grep -i content-security-policy`
   contains `bam.nr-data.net` in `connect-src`; the NR UI shows RUM + APM
   data within a few minutes.

**Turn off:** blank both values (SSM + Secret), `rollout restart` both
deployments — the agent stops and the CSP entry disappears on the next
container start.

## 4. Backups

### 4.1 The CronJob (operator-owned — NOT in the chart)

`deploy/ops/pg-backup-cronjob.yaml` is applied **outside** the Helm
release on purpose (D-035 precedent: ops topology stays out of the
public chart; D-037):

```sh
kubectl apply -f deploy/ops/pg-backup-cronjob.yaml
```

It creates PVC `stonks-backups` (10 Gi, `longhorn`) and CronJob
`stonks-pg-backup`: daily **03:00 UTC**, one plain-SQL
`pg_dump | gzip` per run, **14 newest kept** (RPO 24 h, ~2-week
retention). Adjust the header-comment assumptions if your release name /
namespace / Secret name differ from the defaults (`stonks` / `stonks` /
`stonks-secrets`).

Two properties to know:

- The dump image (`postgres:18.6`) must be ≥ the server major — keep it
  in sync with `postgres.image.tag` in your release values.
- The PVC lives **on** the cluster (node-replicated Longhorn). It does
  not survive total cluster loss. If you want an off-cluster copy, add
  your own transport after each run — the files are portable plain-SQL
  gzips (e.g. an `aws s3 cp` step).

Check the latest run:

```sh
kubectl -n stonks get jobs -o name | grep stonks-pg-backup- | tail -1 \
  | xargs kubectl -n stonks logs
```

### 4.2 Restore procedure (full)

1. **Drain writes:** patch the HPA to 0/0 first (it otherwise rescales
   the backend, §5 step 2), then
   `kubectl -n stonks scale deploy stonks-backend stonks-worker --replicas=0`.
2. **Find the dump to restore** (newest first):

   ```sh
   kubectl -n stonks run scratch --rm -it --restart=Never \
     --image=busybox --overrides='{
       "spec":{"containers":[
         {"name":"scratch","image":"busybox",
          "command":["sh","-c","ls -t /mnt | head"],
          "volumeMounts":[{"name":"backups","mountPath":"/mnt"}]}],
         "volumes":[
           {"name":"backups","persistentVolumeClaim":{"claimName":"stonks-backups"}}]}}'
   ```

3. **Restore into a clean schema** — one `postgres:18.6` pod that
   mounts the backup PVC and reads the DB creds from the same Secret
   the app uses (`envFrom` → `POSTGRES_USER/PASSWORD/DB`). Delete a
   stale `stonks-restore` pod from an earlier run first — the name is
   fixed and `kubectl create` fails if it exists:

   ```sh
   cat <<'EOF' | kubectl -n stonks create -f -
   apiVersion: v1
   kind: Pod
   metadata:
     name: stonks-restore
     labels:
       app.kubernetes.io/name: stonks
       app.kubernetes.io/component: ops
   spec:
     restartPolicy: Never
     containers:
       - name: restore
         image: postgres:18.6
         command: ["/bin/sh", "-c"]
         args:
           - |
             set -e
             dump=$(ls -t /backups/stonks-*.sql.gz | head -1)
             echo "restoring $dump"
             gunzip -c "$dump" > /tmp/restore.sql
             psql -v ON_ERROR_STOP=1 -h stonks-postgres -U "$POSTGRES_USER" -d "$POSTGRES_DB" \
               -c "DROP SCHEMA public CASCADE; CREATE SCHEMA public;"
             psql -v ON_ERROR_STOP=1 -h stonks-postgres -U "$POSTGRES_USER" -d "$POSTGRES_DB" \
               -f /tmp/restore.sql
             psql -h stonks-postgres -U "$POSTGRES_USER" -d "$POSTGRES_DB" \
               -c "select relname, n_live_tup from pg_stat_user_tables order by 1;"
         envFrom:
           - secretRef:
               name: stonks-secrets
         volumeMounts:
           - name: backups
             mountPath: /backups
     volumes:
       - name: backups
         persistentVolumeClaim:
           claimName: stonks-backups
   EOF
   kubectl -n stonks logs -f stonks-restore
   ```

   To restore a specific file instead of the newest, replace
   `dump=$(ls -t /backups/stonks-*.sql.gz | head -1)` with
   `dump=/backups/stonks-<timestamp>.sql.gz`.
4. **Verify:** the pod's final output lists every table with its row
   count — `users` (plus a spot-check table) must match the
   pre-incident count. Then `kubectl -n stonks delete pod stonks-restore`.
5. **Scale back up** (restore the HPA first if you patched it) and run
   the §2.3 battery.

### 4.3 Restore drill (quarterly)

Do §4.2 against a **scratch** database so the live data is untouched:
create a throwaway DB (`CREATE DATABASE drill;`), point the restore at
it, compare row counts per table with the live DB
(`select relname, n_live_tup from pg_stat_user_tables order by 1;`), then
`DROP DATABASE drill;`. A backup that has never been restored is a
hope, not a backup.

## 5. KEK rotation

The KEK (64 hex, 32 bytes) wraps every user's per-user DEK
(`users.wrapped_dek`). Transaction data is encrypted with the **DEK**, so
rotation never re-encrypts data — it only re-wraps the DEK column, via
`crypto.Rewrap(oldKek, newKek, wrapped)`
(`backend/internal/crypto/crypto.go`).

Runbook (≈15 min of zero API availability; pick a quiet window):

1. **New key:** `NEW_KEK=$(make gen-kek)`. Keep `OLD_KEK` (current SSM
   value) at hand.
2. **Drain** (the backend HPA minReplicas=2 would fight a scale-to-0):

   ```sh
   kubectl -n stonks patch hpa stonks-backend --type merge \
     -p '{"spec":{"minReplicas":0,"maxReplicas":0}}'
   kubectl -n stonks scale deploy stonks-backend stonks-worker --replicas=0
   ```

3. **Reach the DB:** `kubectl -n stonks port-forward svc/stonks-postgres 5432`
   (leave it running).
4. **Rewrap.** The rotation program is deliberately **not** a permanent
   `cmd/` in this repo (D-037) — it is an ad-hoc file you create for the
   rotation and delete afterwards (the `docker run` below removes it, on
   the host as well, in the mounted dir). Save it as
   `backend/tmp_kekrotate.go` (it imports the module's own
   `internal/crypto`, so it must live inside `backend/` while it
   compiles):

   ```go
   //go:build ignore

   // Ad-hoc KEK rotation (docs/OPERATOR.md §5). Create, run, delete.
   // Not a permanent command on purpose (D-037).
   package main

   import (
       "bytes"
       "database/sql"
       "fmt"
       "log"
       "os"

       "finance-tracker/internal/crypto"

       _ "github.com/jackc/pgx/v5/stdlib"
   )

   func main() {
       oldKek, err := crypto.KEKFromHex(os.Getenv("OLD_KEK"))
       if err != nil { log.Fatal(err) }
       newKek, err := crypto.KEKFromHex(os.Getenv("NEW_KEK"))
       if err != nil { log.Fatal(err) }
       db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
       if err != nil { log.Fatal(err) }
       defer db.Close()

       rows, err := db.Query("SELECT id, wrapped_dek FROM users")
       if err != nil { log.Fatal(err) }
       type user struct {
           id     string // uuid as text
           dek    []byte
           packed []byte
       }
       var users []user
       for rows.Next() {
           var u user
           var wrapped []byte
           if err := rows.Scan(&u.id, &wrapped); err != nil {
               rows.Close()
               log.Fatal(err)
           }
           u.dek, err = crypto.UnwrapDEK(oldKek, wrapped) // fails if the row was already re-wrapped
           if err != nil { rows.Close(); log.Fatal(err) }
           u.packed, err = crypto.WrapDEK(newKek, u.dek)  // == crypto.Rewrap(old, new, wrapped)
           if err != nil { rows.Close(); log.Fatal(err) }
           // Verify BEFORE trusting: the new pack must unwrap under the
           // new KEK to exactly the DEK we just read under the old KEK.
           check, err := crypto.UnwrapDEK(newKek, u.packed)
           if err != nil || !bytes.Equal(check, u.dek) {
               rows.Close()
               log.Fatal("verification failed — nothing written, abort")
           }
           users = append(users, u)
       }
       rows.Close()

       tx, err := db.Begin()
       if err != nil { log.Fatal(err) }
       st, err := tx.Prepare("UPDATE users SET wrapped_dek = $1 WHERE id = $2")
       if err != nil { log.Fatal(err) }
       defer st.Close()
       for _, u := range users {
           if _, err := st.Exec(u.packed, u.id); err != nil {
               tx.Rollback()
               log.Fatalf("mid-transaction failure — rolled back: %v", err)
           }
       }
       if err := tx.Commit(); err != nil { log.Fatalf("commit failed (rolled back): %v", err) }
       fmt.Printf("rewrapped %d users\n", len(users))
   }
   ```

   ```sh
   docker run --rm \
     -e DATABASE_URL='postgres://<user>:<password>@localhost:5432/<db>?sslmode=disable' \
     -e OLD_KEK="$OLD_KEK" -e NEW_KEK="$NEW_KEK" \
     -v "$PWD/backend:/src" -v gomodcache:/go/pkg/mod -w /src \
     golang:1.27.3 sh -c 'go run ./tmp_kekrotate.go; rc=$?; rm -f ./tmp_kekrotate.go; exit $rc'
   ```

   (`DATABASE_URL` targets the port-forward from step 3; the user /
   password / db are the `POSTGRES_*` values you chose in §1.2.)
   All-or-nothing by construction: every row is unwrapped **and**
   re-wrap-verified under the old KEK before any write; the writes happen
   in one transaction; any failure exits with everything intact.
5. **Flip the key** only after step 4 prints success:

   ```sh
   aws ssm put-parameter --name /finance-tracker/KEK \
     --type SecureString --value "$NEW_KEK" --overwrite
   kubectl -n stonks patch secret stonks-secrets --type merge \
     -p "{\"data\":{\"KEK\":\"$(printf '%s' "$NEW_KEK" | base64 -w0)\"}}"
   ```

   (Order matters: DB first, key second. If you stop anywhere before
   step 5, the OLD KEK still unwraps everything.)
6. **Undrain:**

   ```sh
   kubectl -n stonks scale deploy stonks-backend stonks-worker --replicas=1
   kubectl -n stonks patch hpa stonks-backend --type merge \
     -p '{"spec":{"minReplicas":2,"maxReplicas":4}}'
   ```

   Then run the §2.3 battery. A real login + page load exercises an
   `UnwrapDEK(NEW_KEK)` end-to-end.

## 6. LLM model update

The app's default model is configured through the `LLM_*` keys of
`stonks-secrets` (`LLM_BASE_URL` = the server's OpenAI-compatible base,
`LLM_API_KEY`, `LLM_MODEL` = the **full model id the server reports** in
`/v1/models`, not a filename). Per-user credentials, when set in the UI,
override these.

- **Dev:** `make dev-llm-clean && make dev-llm` — wipes the
  `ftrk-llm-model` volume, re-downloads + sha256-verifies the model
  (~2.5 GB), restarts `llama.cpp`. To pick a different model, edit the
  `llama-init` URL in `docker-compose.dev.yml` first and re-verify the
  sha256.
- **Prod:** put the new model on the server (operator-owned, off-cluster
  or in-cluster as you run it), then update `LLM_MODEL` (and
  `LLM_BASE_URL` if the endpoint moved): SSM parameter →
  `kubectl -n stonks patch secret stonks-secrets …` →
  `kubectl -n stonks rollout restart deploy stonks-backend stonks-worker`.
  Verify with one categorisation from the UI and the worker logs.

## 7. Upgrades & rollbacks

**Upgrade** = bump the SHA, re-apply:

```hcl
  set { name = "images.backend.tag"  value = "<new-git-short-sha>" }
  set { name = "images.frontend.tag" value = "<new-git-short-sha>" }
```

`terraform apply` triggers a `helm upgrade`: rolling deploy per workload
(PDBs minAvailable 1; HPA keeps 2–4 backends), then the
`<release>-migrate` **post-upgrade** hook runs `goose up` (migrations are
forward-only — the chart never runs `goose down`), and `/readyz` holds
traffic off until the schema is current. Then run the §2.3 battery
against the new images.

**Rollback** = set the `images.*.tag` back to the previous SHA and
re-apply (old tags stay in GHCR — they are never deleted), or
`helm rollback stonks <rev> -n stonks` if you are outside Terraform.
Caveat: rolling back the images does NOT roll back the schema — if the
upgrade already applied a migration that the old code cannot tolerate,
restore from backup (§4) instead. Migrations are written to be
additive/backwards-compatible exactly so this is not the common case.

#!/usr/bin/env bash
# Keycloak dev bootstrap (step 0.4). Runs as the container entrypoint.
#
# Phase 1 — first boot only (detected by an empty Keycloak DB):
#   * `kc.sh import --file <realm json> --override false` — OFFLINE import
#     of the committed deploy/keycloak/finance-realm.json (master realm,
#     'finance' realm, finance-spa + finance-api clients, one test user)
#     directly into the ftrk-kc-data volume, with the server NOT running.
#     `kcadm.sh import` was REMOVED in Keycloak 26.8 (verified in 26.8.1);
#     `kc.sh import` is the offline 26.x replacement. (KC_BOOTSTRAP_KEYCLOAK_IMPORT
#     appears in `kc.sh show-config` but is INERT at runtime — empirically
#     verified 2026-11-02: the realm is never imported.)
#     Realm-JSON gotcha (26.x): multivalued client config attributes such
#     as `post.logout.redirect.uris` are stored with a `##` delimiter
#     (Constants.CFG_DELIMITER changed from "," to "##" in 26.x) — a
#     comma-joined value parses as ONE bogus URI and sign-out breaks with
#     "invalid redirect uri". The old ClientRepresentation field
#     `postLogoutRedirectUris` was removed in 26.x — the import/admin API
#     reject it with an "Unrecognized field" 400 (verified 2026-11-09).
#     The KC_BOOTSTRAP_ADMIN_USERNAME/PASSWORD env vars (set compose-wide)
#     are consumed during the import's realm initialization: the admin user
#     is created WITH the env-var password in that same step (verified:
#     a fresh server started against the imported volume answers admin-cli
#     password grant 200). The server does NOT re-sync that password from
#     env on later boots — changing KC_ADMIN_PASSWORD in .env after the
#     first boot requires deleting the ftrk-kc-data volume.
#     Do NOT follow up with `kc.sh bootstrap-admin user`: it errors on the
#     already-existing admin ("user with username exists", exit 0) and that
#     failed run leaves the admin WITHOUT a usable password (kcadm 401s) —
#     verified empirically 2026-11-02.
# Phase 2 — every boot:
#   * start `kc.sh start-dev` in the background,
#   * wait for the readiness endpoint (no curl/wget in the image — bash /dev/tcp),
#   * sync the test user's password via the RAW admin API (bash /dev/tcp):
#     mint a FRESH master-realm admin token (admin-cli password grant),
#     then PUT /admin/realms/finance/users/<id>/reset-password — 204 expected.
#     kcadm is deliberately NOT used here: kcadm 26.8 caches tokens per
#     (server, realm) in $HOME/.keycloak/kcadm.config, and the admin user
#     exists only in the master realm — `kcadm --realm finance` finds no
#     finance token and 401s (verified 2026-11-02). A fresh master admin
#     token DOES authorize other realms' admin endpoints (the same trick
#     the admin console uses). Note admin-cli tokens live only 60s
#     (default accessTokenLifespan) — mint per boot, use immediately.
#     reset-password is PUT, not POST (POST 404s — verified).
#   * wait on Keycloak and propagate its exit code.
#
# Fresh-DB detection: in Keycloak 26.8.1 dev-file mode the H2 database
# lives at /opt/keycloak/data/h2/keycloakdb.mv.db (NOT data/keycloak.h2.db
# — the 26.x layout adds an h2/ subdir; verified against the volume
# 2026-11-03). Absent on a fresh ftrk-kc-data volume, present after any
# boot. Defense in depth: if the import ever runs against an existing
# DB anyway, `--override false` is non-destructive — it logs
# "Realm 'finance' already exists. Import skipped" and exits 0 (verified).
# Delete the volume to force a re-import.
#
# Committed to the repo (deploy/keycloak/), mounted read-only by
# docker-compose.dev.yml. Env (with dev defaults):
#   KC_BOOTSTRAP_ADMIN_USERNAME / KC_BOOTSTRAP_ADMIN_PASSWORD  (compose sets)
#   KC_HEALTH_ENABLED=true / KC_HTTP_MANAGEMENT_HEALTH_ENABLED (compose sets)
#   FTRK_TEST_USER_PASSWORD                                    (compose, default test1234)
set -euo pipefail

SERVER="http://127.0.0.1:8080"
REALM="finance"
H2_FILE="/opt/keycloak/data/h2/keycloakdb.mv.db"
IMPORT_FILE="/opt/keycloak/data/import/finance-realm.json"
ADMIN_USER="${KC_BOOTSTRAP_ADMIN_USERNAME:-admin}"
ADMIN_PASS="${KC_BOOTSTRAP_ADMIN_PASSWORD:-admin}"
TEST_USER="test"
TEST_PASS="${FTRK_TEST_USER_PASSWORD:-test1234}"
# Fixed id from finance-realm.json — the offline import preserves it
# (verified via GET /admin/realms/finance/users?max=10).
TEST_USER_ID="11111111-2222-3333-4444-555555555555"

# --- Phase 1: first-boot-only offline import --------------------------------
if [ ! -e "$H2_FILE" ]; then
  echo "dev-init: fresh Keycloak DB — importing realm '$REALM' from $IMPORT_FILE"
  /opt/keycloak/bin/kc.sh import --file "$IMPORT_FILE" --override false
else
  echo "dev-init: existing Keycloak DB — skipping offline import (idempotent restart)"
fi

# --- Phase 2: start the server (every boot) ---------------------------------
/opt/keycloak/bin/kc.sh "$@" &
KC_PID=$!
trap 'kill -TERM "$KC_PID" 2>/dev/null || true' TERM INT

# HTTP status check with bash /dev/tcp (the image ships no curl/wget).
# NOTE: the connect's `2>/dev/null` must stay scoped to the brace group.
# A bare `exec ... 2>/dev/null` would redirect the shell's OWN stderr for
# the rest of the script (exec with only redirections persists them),
# silently swallowing every later error message after the first successful
# connection.
kc_ready() {
  local line
  if ! { exec 3<> /dev/tcp/127.0.0.1/8080; } 2>/dev/null; then
    return 1
  fi
  printf 'GET /health/ready HTTP/1.0\r\nHost: localhost\r\n\r\n' >&3
  IFS= read -r line <&3 || line=""
  exec 3>&-
  [[ "$line" == *" 200 "* ]]
}

ready=0
for _ in $(seq 1 90); do
  if kc_ready; then
    ready=1
    break
  fi
  kill -0 "$KC_PID" 2>/dev/null || break # keycloak died — stop waiting
  sleep 2
done
if [ "$ready" -ne 1 ]; then
  echo "dev-init: ERROR: Keycloak /health/ready never returned 200" >&2
  kill -TERM "$KC_PID" 2>/dev/null || true
  exit 1
fi
echo "dev-init: Keycloak is ready"

# Mint a fresh master-realm admin token via the admin-cli password grant.
# Tokens live 60s (Keycloak default accessTokenLifespan) — use immediately.
# Returns the token on stdout; fails on connection error or non-200 (e.g.
# 401 invalid_grant on an admin password mismatch).
mint_admin_token() {
  local body token
  body="grant_type=password&client_id=admin-cli&username=${ADMIN_USER}&password=${ADMIN_PASS}"
  exec 3<> /dev/tcp/127.0.0.1/8080
  printf 'POST /realms/master/protocol/openid-connect/token HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s' "${#body}" "$body" >&3
  body=$(tr -d '\r' <&3 | tail -1)
  exec 3>&-
  token=$(printf '%s' "$body" | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')
  [ -n "$token" ] && printf '%s' "$token"
}

# PUT the test user's password via the raw admin API (see header for why
# not kcadm). Expects "HTTP/1.1 204 No Content".
sync_test_user_password() {
  local token json resp_line
  token=$(mint_admin_token) || return 1
  json="{\"type\":\"password\",\"value\":\"${TEST_PASS}\"}"
  exec 3<> /dev/tcp/127.0.0.1/8080
  printf 'PUT /admin/realms/%s/users/%s/reset-password HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s' "$REALM" "$TEST_USER_ID" "$token" "${#json}" "$json" >&3
  IFS= read -r resp_line <&3 || resp_line=""
  exec 3>&-
  [[ "$resp_line" == *" 204 "* ]]
}

# Idempotent: keeps the test user's password in sync with the docs even if
# the realm JSON's credential ever changes. Runs on every boot (the realm
# import itself happened in phase 1, on the very first boot only).
if ! sync_test_user_password; then
  echo "dev-init: ERROR: could not sync test user '$TEST_USER' password via the admin API. Most likely the admin-cli password grant 401'd: if KC_ADMIN_PASSWORD in .env was changed after the first boot, the DB still holds the OLD password (Keycloak does not re-sync it from env on later boots). Stop the stack and delete the ftrk-kc-data volume to re-import (docs/DEVELOPMENT.md)." >&2
  kill -TERM "$KC_PID" 2>/dev/null || true
  exit 1
fi
echo "dev-init: test user '$TEST_USER' password synchronized (see docs/DEVELOPMENT.md)"

# Stay in the foreground: container lifetime == Keycloak lifetime.
wait "$KC_PID"

#!/usr/bin/env bash
# Keycloak dev bootstrap (step 0.4). Runs as the container entrypoint.
#
# Phase 1 — first boot only (detected by an empty Keycloak DB):
#   * `kc.sh import --file <realm json> --override false` — OFFLINE import
#     of the committed deploy/keycloak/finance-realm.json (master realm,
#     'finance' realm, finance-spa + finance-api clients — no users: the
#     test user is created/synced in phase 2, never imported)
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
#   * ensure the test user EXISTS and sync its password via the RAW admin
#     API (bash /dev/tcp): mint a FRESH master-realm admin token
#     (admin-cli password grant), resolve the user's id with GET
#     /admin/realms/finance/users?username=test (create it with POST
#     /admin/realms/finance/users — 201 — if absent), then PUT
#     /admin/realms/finance/users/<id>/reset-password — 204 expected.
#     kcadm is deliberately NOT used here: kcadm 26.8 caches tokens per
#     (server, realm) in $HOME/.keycloak/kcadm.config, and the admin user
#     exists only in the master realm — `kcadm --realm finance` finds no
#     finance token and 401s (verified 2026-11-02). A fresh master admin
#     token DOES authorize other realms' admin endpoints (the same trick
#     the admin console uses). Note admin-cli tokens live only 60s
#     (default accessTokenLifespan) — mint per boot, use immediately.
#     reset-password is PUT, not POST (POST 404s — verified).
#   * sync the realm's access-token lifespan (round 9) via the same raw
#     admin API: GET /admin/realms/finance, rewrite accessTokenLifespan +
#     ssoSessionMaxLifespan(+RememberMe) to N, PUT the full realm back —
#     204 expected. The realm endpoint has NO PATCH in 26.8.1 (405 —
#     verified), so the GET-modify-PUT round-trip (the same one the admin
#     console does) is the partial-update path. The session max lifespan
#     is raised with the token because Keycloak CAPS the access-token
#     expiry at ssoSessionMaxLifespan (empirically: max 36000 capped
#     expires_in at 36000 even with a 604800 access-token lifespan; with
#     both set to the same N the token expires in N — here 3600).
#     finance-realm.json sets neither field, so the global defaults apply
#     (300s access token) — the 5-minute token made the SPA's session
#     expire constantly. The offline import (phase 1) runs on the FIRST
#     boot only, so this per-boot sync is the only way env changes ever
#     land in the persisted volume.
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
#   FTRK_ACCESS_TOKEN_LIFESPAN                                 (compose, default 3600)
set -euo pipefail

SERVER="http://127.0.0.1:8080"
REALM="finance"
H2_FILE="/opt/keycloak/data/h2/keycloakdb.mv.db"
IMPORT_FILE="/opt/keycloak/data/import/finance-realm.json"
ADMIN_USER="${KC_BOOTSTRAP_ADMIN_USERNAME:-admin}"
ADMIN_PASS="${KC_BOOTSTRAP_ADMIN_PASSWORD:-admin}"
TEST_USER="test"
TEST_PASS="${FTRK_TEST_USER_PASSWORD:-test1234}"
# Access-token lifespan for realm 'finance', in seconds (round 9). Keycloak's
# default is 60s, which made the SPA's session expire constantly. Default is
# now 3600 (2026-11-16: down from 604800) — a week of token lifetime bought
# reboot-survival the SPA doesn't need (the oidc-client silent refresh
# renews active sessions well before an hour) while leaving a stolen token
# valid for a week. 1h keeps dev sessions comfortable without that exposure.
# MUST be a positive integer — validated before the server starts so a typo
# in .env fails the container loudly instead of 400-ing at sync time.
LIFESPAN="${FTRK_ACCESS_TOKEN_LIFESPAN:-3600}"
case "$LIFESPAN" in
  '' | *[!0-9]*)
    echo "dev-init: ERROR: FTRK_ACCESS_TOKEN_LIFESPAN must be a positive integer (seconds), got '$LIFESPAN'" >&2
    exit 1
    ;;
esac
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
# not kcadm). Expects "HTTP/1.1 204 No Content". The user's id is resolved
# per boot by fetch_test_user_id — it is not a fixed constant anymore.
sync_test_user_password() {
  local user_id="$1" token json resp_line
  token=$(mint_admin_token) || return 1
  json="{\"type\":\"password\",\"value\":\"${TEST_PASS}\"}"
  exec 3<> /dev/tcp/127.0.0.1/8080
  printf 'PUT /admin/realms/%s/users/%s/reset-password HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s' "$REALM" "$user_id" "$token" "${#json}" "$json" >&3
  IFS= read -r resp_line <&3 || resp_line=""
  exec 3>&-
  [[ "$resp_line" == *" 204 "* ]]
}

# Resolve the test user's id via GET /admin/realms/finance/users?username=test
# and print it on stdout. The response is a single-line JSON array with at
# most one element (the username filter), so the pure-bash split + sed
# pattern from fetch_realm_json (the image has no jq/awk) is complete; the
# greedy sed match is safe because a user representation carries exactly one
# "id" key. Fails on connection error, non-200, or an empty array (user
# absent). Deliberately NO `| head -n 1`: under pipefail, head closing the
# pipe early would SIGPIPE the producer and fail the function for no reason.
fetch_test_user_id() {
  local token raw id
  token=$(mint_admin_token) || return 1
  exec 3<> /dev/tcp/127.0.0.1/8080
  printf 'GET /admin/realms/%s/users?username=%s HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer %s\r\nConnection: close\r\n\r\n' "$REALM" "$TEST_USER" "$token" >&3
  raw=$(tr -d '\r' <&3)
  exec 3>&-
  [[ "${raw%%$'\n'*}" == *" 200 "* ]] || return 1
  id=$(printf '%s' "${raw#*$'\n\n'}" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  [[ -n "$id" ]] || return 1
  printf '%s' "$id"
}

# Create the test user (first boot — the committed realm JSON is user-free,
# see phase 1). The password is NOT set here: sync_test_user_password
# follows and resets it, so FTRK_TEST_USER_PASSWORD stays the single source
# of truth for the credential. Expects "HTTP/1.1 201 Created".
create_test_user() {
  local token json resp_line
  token=$(mint_admin_token) || return 1
  json="{\"username\":\"${TEST_USER}\",\"enabled\":true,\"email\":\"test@localhost\",\"emailVerified\":true,\"firstName\":\"Test\",\"lastName\":\"User\"}"
  exec 3<> /dev/tcp/127.0.0.1/8080
  printf 'POST /admin/realms/%s/users HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s' "$REALM" "$token" "${#json}" "$json" >&3
  IFS= read -r resp_line <&3 || resp_line=""
  exec 3>&-
  [[ "$resp_line" == *" 201 "* ]]
}

# GET the full realm representation via the raw admin API and print the
# compact JSON body on stdout. GET /admin/realms/<realm> answers with a
# single-line JSON body and a Content-Length header (not chunked —
# verified), so a read-to-EOF with "Connection: close" is complete.
# The image has no awk, so the response is split with pure bash patterns:
# status = first line, body = everything after the first blank line
# (response headers never contain a blank line). Fails on connection
# error or a non-200 status.
fetch_realm_json() {
  local token="$1" raw
  exec 3<> /dev/tcp/127.0.0.1/8080
  printf 'GET /admin/realms/%s HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer %s\r\nConnection: close\r\n\r\n' "$REALM" "$token" >&3
  raw=$(tr -d '\r' <&3)
  exec 3>&-
  [[ "${raw%%$'\n'*}" == *" 200 "* ]] || return 1
  printf '%s' "${raw#*$'\n\n'}"
}

# Set the realm's access-token lifespan to $LIFESPAN via a GET-modify-PUT
# round-trip (the realm endpoint has no PATCH in 26.8.1 — see header).
# Rewrites accessTokenLifespan AND ssoSessionMaxLifespan (+RememberMe):
# Keycloak caps the access-token expiry at the SSO session max lifespan,
# so a token can never outlive the session (empirically verified — see
# header). The sed patterns are quote+colon-anchored so
# "ssoSessionMaxLifespan": can never match inside
# "ssoSessionMaxLifespanRememberMe": (nor "accessTokenLifespan": inside
# "accessTokenLifespanForImplicitFlow":). Expects "HTTP/1.1 204 No Content".
sync_access_token_lifespan() {
  local token json len resp_line
  token=$(mint_admin_token) || return 1
  json=$(fetch_realm_json "$token") || return 1
  json=$(printf '%s' "$json" | sed \
    -e "s/\"accessTokenLifespan\":[0-9]*/\"accessTokenLifespan\":${LIFESPAN}/" \
    -e "s/\"ssoSessionMaxLifespan\":[0-9]*/\"ssoSessionMaxLifespan\":${LIFESPAN}/" \
    -e "s/\"ssoSessionMaxLifespanRememberMe\":[0-9]*/\"ssoSessionMaxLifespanRememberMe\":${LIFESPAN}/") || return 1
  # Guards: the rewrites must have actually landed. A future Keycloak that
  # pretty-prints the realm JSON or renames a field would otherwise PUT an
  # unchanged copy back, get a 204, and leave the realm on the short
  # default — fail loudly instead.
  [[ $json == *'"accessTokenLifespan":'$LIFESPAN* ]] || return 1
  [[ $json == *'"ssoSessionMaxLifespan":'$LIFESPAN* ]] || return 1
  [[ $json == *'"ssoSessionMaxLifespanRememberMe":'$LIFESPAN* ]] || return 1
  token=$(mint_admin_token) || return 1
  len=$(printf '%s' "$json" | wc -c) # bytes, not ${#json} chars (UTF-8 locale)
  exec 3<> /dev/tcp/127.0.0.1/8080
  printf 'PUT /admin/realms/%s HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s' "$REALM" "$token" "$len" "$json" >&3
  IFS= read -r resp_line <&3 || resp_line=""
  exec 3>&-
  [[ "$resp_line" == *" 204 "* ]]
}

# Idempotent: ensures the test user exists (created on first boot — the
# realm import no longer carries it) and keeps its password in sync with
# the docs. Runs on every boot (the realm import itself happened in phase
# 1, on the very first boot only). The user's id is resolved per boot via
# the admin API — it is not fixed — so deleting the user, or re-importing
# a realm that assigns a different id, self-heals on the next boot.
if ! TEST_USER_ID=$(fetch_test_user_id); then
  if ! create_test_user; then
    echo "dev-init: ERROR: could not create test user '$TEST_USER' via the admin API (POST expected 201). Most likely the admin-cli password grant 401'd: if KC_ADMIN_PASSWORD in .env was changed after the first boot, the DB still holds the OLD password (Keycloak does not re-sync it from env on later boots). Stop the stack and delete the ftrk-kc-data volume to re-import (docs/DEVELOPMENT.md)." >&2
    kill -TERM "$KC_PID" 2>/dev/null || true
    exit 1
  fi
  echo "dev-init: test user '$TEST_USER' created"
  if ! TEST_USER_ID=$(fetch_test_user_id); then
    echo "dev-init: ERROR: test user '$TEST_USER' was created (POST 201) but GET users?username=$TEST_USER still resolves no id — giving up." >&2
    kill -TERM "$KC_PID" 2>/dev/null || true
    exit 1
  fi
fi
if ! sync_test_user_password "$TEST_USER_ID"; then
  echo "dev-init: ERROR: could not sync test user '$TEST_USER' password via the admin API (reset-password expected 204). Most likely the admin-cli password grant 401'd: if KC_ADMIN_PASSWORD in .env was changed after the first boot, the DB still holds the OLD password (Keycloak does not re-sync it from env on later boots). Stop the stack and delete the ftrk-kc-data volume to re-import (docs/DEVELOPMENT.md)." >&2
  kill -TERM "$KC_PID" 2>/dev/null || true
  exit 1
fi
echo "dev-init: test user '$TEST_USER' password synchronized (see docs/DEVELOPMENT.md)"

# Round 9: sync the access-token lifespan (see LIFESPAN above). Failing is
# treated like the password sync: the admin API is reachable but the
# GET-modify-PUT round-trip didn't 204 — a mangled realm JSON or a
# permission gap should not silently leave the realm on the 5-minute
# default (which is the bug this sync exists to fix).
if ! sync_access_token_lifespan; then
  echo "dev-init: ERROR: could not set realm '$REALM' access-token lifespan to ${LIFESPAN}s via the admin API (GET-modify-PUT, expected 204). Check FTRK_ACCESS_TOKEN_LIFESPAN and that the admin token mint succeeded." >&2
  kill -TERM "$KC_PID" 2>/dev/null || true
  exit 1
fi
echo "dev-init: realm '$REALM' access-token lifespan set to ${LIFESPAN}s"

# Stay in the foreground: container lifetime == Keycloak lifetime.
wait "$KC_PID"

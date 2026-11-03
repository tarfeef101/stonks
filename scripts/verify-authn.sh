#!/usr/bin/env bash
# Scripted AuthN E2E (step 1.3): the step-1.3 done-when checks against the
# running dev stack, using host tools only (curl + jq + docker — the D-001
# verification allowance) and one stdlib-only Go program for the unwrap
# proof.
#
# Checks (per the plan 1.3 done-when):
#
#   1. /healthz is 200 (stack up)
#   2. /api/v1/me without a token → 401 + WWW-Authenticate
#   3. /api/v1/me with a garbage token → 401
#   4. TWO full password-grant logins against the dev realm → /me 200 each
#      time with the SAME sub (identity from the token `sub` claim)
#   5. exactly ONE users row for that sub; email matches the realm user
#   6. the stored wrapped_dek unwraps with the .env KEK to a 32-byte DEK
#      (stdlib-only AES-256-GCM proof program, run in the golang image;
#      the KEK is passed only as a container env var, never argv)
#
# Run:   make dev            # first — full stack must be healthy
#        scripts/verify-authn.sh
#
# Env (all have dev defaults; see .env.example for the full story):
#   FTRK_API_URL                default http://localhost:18080
#   FTRK_KC_URL                 default http://localhost:18081
#   FTRK_REALM                  default finance
#   FTRK_CLIENT_ID              default finance-spa
#   FTRK_AUDIENCE               default finance-api
#   FTRK_TEST_USER              default test
#   FTRK_TEST_USER_PASSWORD     default test1234
#   FTRK_TEST_USER_EMAIL        default test@localhost

set -euo pipefail

cd "$(dirname "$0")/.."

API="${FTRK_API_URL:-http://localhost:18080}"
KC="${FTRK_KC_URL:-http://localhost:18081}"
REALM="${FTRK_REALM:-finance}"
CLIENT_ID="${FTRK_CLIENT_ID:-finance-spa}"
AUDIENCE="${FTRK_AUDIENCE:-finance-api}"
USERNAME="${FTRK_TEST_USER:-test}"
PASSWORD="${FTRK_TEST_USER_PASSWORD:-test1234}"
EMAIL="${FTRK_TEST_USER_EMAIL:-test@localhost}"
GO_IMG="${GO_IMG:-golang:1.27.3}"
TOKEN_URL="${KC}/realms/${REALM}/protocol/openid-connect/token"

WORK="$(mktemp -d /tmp/opencode/verify-authn.XXXXXX)"
trap 'rm -rf "$WORK"' EXIT

PASS=0
ok() { PASS=$((PASS + 1)); echo "ok: $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

assert_eq() { # label expected actual
	[ "$2" = "$3" ] || fail "$1: expected [$2], got [$3]"
	ok "$1 = $3"
}

decode_jwt_claims() { # <jwt> → JSON payload on stdout
	python3 - "$1" <<'PY'
import base64, json, sys
payload = sys.argv[1].split(".")[1]
payload += "=" * (-len(payload) % 4)
print(json.dumps(json.loads(base64.urlsafe_b64decode(payload))))
PY
}

# --- 1. stack up ---------------------------------------------------------------
code="$(curl -s -o /dev/null -w '%{http_code}' "${API}/healthz")" \
	|| fail "cannot reach ${API}/healthz — is the dev stack up (make dev)?"
assert_eq "GET /healthz" "200" "$code"

# --- 2. no token → 401 ----------------------------------------------------------
NOAUTH_HDRS="$(curl -s -i "${API}/api/v1/me")"
code="$(grep -m1 -oE '^HTTP/[0-9.]+ [0-9]+' <<<"$NOAUTH_HDRS" | awk '{print $2}')"
assert_eq "GET /me (no token)" "401" "$code"
WWW="$(grep -i '^www-authenticate:' <<<"$NOAUTH_HDRS" | head -n1 | tr -d '\r')"
[ -n "$WWW" ] || fail "401 response is missing WWW-Authenticate"
ok "401 carries WWW-Authenticate: ${WWW}"

# --- 3. garbage token → 401 ------------------------------------------------------
code="$(curl -s -o /dev/null -w '%{http_code}' \
	-H "Authorization: Bearer not.a.jwt" "${API}/api/v1/me")"
assert_eq "GET /me (garbage token)" "401" "$code"

# --- 4. two password-grant logins → /me 200, same sub ----------------------------
login() { # <outfile-for-token-response>
	curl -fsS -X POST "$TOKEN_URL" \
		--data-urlencode "grant_type=password" \
		--data-urlencode "username=${USERNAME}" \
		--data-urlencode "password=${PASSWORD}" \
		--data-urlencode "client_id=${CLIENT_ID}" \
		--data-urlencode "audience=${AUDIENCE}" \
		--data-urlencode "scope=openid profile email" \
		-o "$1" || fail "password grant failed (check FTRK_TEST_USER / FTRK_TEST_USER_PASSWORD / realm)"
}

login "$WORK/token1.json"
T1="$(jq -er .access_token "$WORK/token1.json")" \
	|| fail "no access_token in first login response"
SUB1="$(jq -er .sub <<<"$(decode_jwt_claims "$T1")")"
[ -n "$SUB1" ] || fail "access token has no sub claim"
ok "login 1: access token with sub = $SUB1"

code="$(curl -s -o "$WORK/me1.json" -w '%{http_code}' \
	-H "Authorization: Bearer $T1" "${API}/api/v1/me")"
assert_eq "GET /me (login 1)" "200" "$code"
ME_SUB1="$(jq -er .sub "$WORK/me1.json")" || fail "no sub in /me response"
assert_eq "/me sub (login 1)" "$SUB1" "$ME_SUB1"

login "$WORK/token2.json"
T2="$(jq -er .access_token "$WORK/token2.json")"
code="$(curl -s -o "$WORK/me2.json" -w '%{http_code}' \
	-H "Authorization: Bearer $T2" "${API}/api/v1/me")"
assert_eq "GET /me (login 2)" "200" "$code"
ME_SUB2="$(jq -er .sub "$WORK/me2.json")"
assert_eq "/me sub (repeat login, idempotent)" "$SUB1" "$ME_SUB2"

# --- 5. exactly one users row -----------------------------------------------------
# psql() { <sql> } — SQL arrives on stdin so that :sub expands. The pgdg
# psql in the postgres:18 image does NOT expand :vars in -c/--command
# (verified 2026-11-03: stdin and -f expand, -c does not), and the
# combined -tAc -v form is avoided as well: inside a short-flag cluster
# -c consumes the next arg as its command.
psql() { # <sql>
	docker compose -f docker-compose.dev.yml exec -T postgres \
		psql -U postgres -d finance -tA -v "sub=${SUB1}" <<SQL
$1
SQL
}

ROWS="$(psql "SELECT count(*) FROM users WHERE auth0_sub = :'sub'")"
assert_eq "users rows for sub" "1" "$ROWS"
DEK_LEN="$(psql "SELECT length(wrapped_dek) FROM users WHERE auth0_sub = :'sub'")"
[ "$DEK_LEN" -ge 28 ] || fail "wrapped_dek too short: ${DEK_LEN} bytes (< 28)"
ok "wrapped_dek stored (${DEK_LEN} bytes = nonce+ct+tag)"
ROW_EMAIL="$(psql "SELECT email FROM users WHERE auth0_sub = :'sub'")"
assert_eq "users.email" "$EMAIL" "$ROW_EMAIL"

# --- 6. unwrap proof: stored DEK decrypts under the .env KEK ----------------------
KEK="$(grep -E '^KEK=' .env | head -n1 | cut -d= -f2-)"
[ -n "$KEK" ] || fail "KEK missing from .env (make gen-kek)"
WRAPPED_HEX="$(psql "SELECT encode(wrapped_dek, 'hex') FROM users WHERE auth0_sub = :'sub'")"
[ -n "$WRAPPED_HEX" ] || fail "no wrapped_dek hex from the database"

cat > "$WORK/unwrap.go" <<'GO'
// One-off unwrap proof for scripts/verify-authn.sh (step 1.3c): proves the
// stored wrapped_dek (hex of [nonce ‖ ciphertext ‖ tag]) opens under the
// KEK to a 32-byte DEK. Stdlib-only and deliberately independent of
// backend/internal/crypto. KEK and PACK arrive via env, never argv.
package main

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"fmt"
	"os"
)

func main() {
	die := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
		os.Exit(1)
	}
	kek, err := hex.DecodeString(os.Getenv("KEK"))
	if err != nil {
		die("KEK is not hex: %v", err)
	}
	pack, err := hex.DecodeString(os.Getenv("PACK"))
	if err != nil {
		die("PACK is not hex: %v", err)
	}
	block, err := aes.NewCipher(kek)
	if err != nil {
		die("aes.NewCipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		die("gcm: %v", err)
	}
	const nonceSize = 12
	if len(pack) < nonceSize+gcm.Overhead() {
		die("pack too short: %d bytes", len(pack))
	}
	plain, err := gcm.Open(nil, pack[:nonceSize], pack[nonceSize:], nil)
	if err != nil {
		die("GCM open failed (KEK/pack mismatch?): %v", err)
	}
	if len(plain) != 32 {
		die("DEK length: want 32, got %d", len(plain))
	}
	fmt.Println("UNWRAP_OK 32")
}
GO

UNWRAP_OUT="$(docker run --rm -e KEK="$KEK" -e PACK="$WRAPPED_HEX" \
	-v "$WORK:/work" -w /work "$GO_IMG" go run unwrap.go)"
assert_eq "stored DEK unwraps under the KEK" "UNWRAP_OK 32" "$UNWRAP_OUT"

echo
echo "AuthN E2E: all ${PASS} checks passed (token 401s, one idempotent user row, DEK unwraps)"

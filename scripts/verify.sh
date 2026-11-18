#!/usr/bin/env bash
# scripts/verify.sh — scripted E2E verification against the local dev
# stack (make dev first), host tools only (curl + jq + python3 + docker —
# the D-001 verification allowance) plus one stdlib-only Go program for
# the AuthN unwrap proof.
#
# Subcommands:
#
#   oidc   Full PKCE authorization-code flow against the local dev IdP
#          (Keycloak realm 'finance'). This is the step-1.2 DoD evidence
#          that the realm/SPA client config works end-to-end in the exact
#          shape the browser SDK (oidc-client-ts, D-018) emits:
#
#            1. OIDC discovery (.well-known/openid-configuration)
#            2. authorize request (response_type=code, S256 PKCE challenge,
#               state, nonce, audience)
#            3. login form GET → parse → credential POST
#            4. capture the 302 → assert redirect_uri + state round-trip
#            5. token exchange (grant_type=authorization_code +
#               code_verifier, public client — no client_secret)
#            6. id_token / access_token claim assertions
#               (iss, aud, sub, nonce, scope)
#
#   authn  The step-1.3 done-when checks against the running dev stack:
#
#            1. /healthz is 200 (stack up)
#            2. /api/v1/me without a token → 401 + WWW-Authenticate
#            3. /api/v1/me with a garbage token → 401
#            4. TWO full password-grant logins against the dev realm →
#               /me 200 each time with the SAME sub (identity from the
#               token `sub` claim)
#            5. exactly ONE users row for that sub; email matches the
#               realm user
#            6. the stored wrapped_dek unwraps with the .env KEK to a
#               32-byte DEK (stdlib-only AES-256-GCM proof program, run
#               in the golang image; the KEK is passed only as a
#               container env var, never argv)
#
#   all    Both flows, in order (oidc first).
#
# The manual BROWSER side of the OIDC DoD (login/logout in the running
# SPA, session persistence, token feeding the API client) is documented
# in docs/DEVELOPMENT.md — a headless host cannot click.
#
# Run:   make dev            # first — full stack must be healthy
#        scripts/verify.sh all
#
# Env (all have dev defaults; see .env.example for the full story):
#   oidc flow:
#   PUBLIC_OIDC_ISSUER          default http://localhost:18081/realms/finance
#   PUBLIC_OIDC_CLIENT_ID       default finance-spa
#   PUBLIC_OIDC_AUDIENCE        default finance-api
#   FTRK_TEST_USER              default test
#   FTRK_TEST_USER_PASSWORD     default test1234
#   authn flow:
#   FTRK_API_URL                default http://localhost:18080
#   FTRK_KC_URL                 default http://localhost:18081
#   FTRK_REALM                  default finance
#   FTRK_CLIENT_ID              default finance-spa
#   FTRK_AUDIENCE               default finance-api
#   FTRK_TEST_USER              default test
#   FTRK_TEST_USER_PASSWORD     default test1234
#   FTRK_TEST_USER_EMAIL        default test@localhost
#   GO_IMG                      default golang:1.27.3

set -euo pipefail

cd "$(dirname "$0")/.."

# --- subcommand dispatch --------------------------------------------------------

usage() {
	cat <<'EOF'
Usage: scripts/verify.sh <oidc|authn|all>

Scripted E2E verification against the local dev stack (make dev first).

  oidc    PKCE S256 authorization-code flow against the dev Keycloak realm
  authn   token 401s, one idempotent user row, DEK unwrap proof
  all     both flows, in order

All env vars have dev defaults; see the script header / .env.example.
EOF
}

case "${1:-}" in
	help | -h | --help)
		usage
		exit 0
		;;
	oidc)
		RUN_OIDC=1
		RUN_AUTHN=0
		;;
	authn)
		RUN_OIDC=0
		RUN_AUTHN=1
		;;
	all)
		RUN_OIDC=1
		RUN_AUTHN=1
		;;
	*)
		usage >&2
		echo >&2
		echo "error: ${1:+unknown subcommand '${1}' — }missing or unknown subcommand" >&2
		exit 2
		;;
esac

# --- shared plumbing ----------------------------------------------------------------

OIDC_WORK=""
AUTHN_WORK=""
cleanup() {
	local d
	for d in "$OIDC_WORK" "$AUTHN_WORK"; do
		[ -n "$d" ] && rm -rf "$d"
	done
	return 0
}
trap cleanup EXIT

PASS=0
ok() { PASS=$((PASS + 1)); echo "ok: $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

assert_eq() { # label expected actual
	[ "$2" = "$3" ] || fail "$1: expected [$2], got [$3]"
	ok "$1 = $3"
}

assert_contains() { # label needle haystack
	case "$3" in
	*"$2"*) ok "$1 contains [$2]" ;;
	*) fail "$1: [$3] does not contain [$2]" ;;
	esac
}

decode_jwt_claims() { # <jwt> → JSON payload on stdout
	python3 - "$1" <<'PY'
import base64, json, sys
payload = sys.argv[1].split(".")[1]
payload += "=" * (-len(payload) % 4)
print(json.dumps(json.loads(base64.urlsafe_b64decode(payload))))
PY
}

# --- oidc flow ----------------------------------------------------------------------

run_oidc() {
	local KC CLIENT_ID AUDIENCE REDIRECT_URI USERNAME PASSWORD KC_HOST
	KC="${PUBLIC_OIDC_ISSUER:-http://localhost:18081/realms/finance}"
	CLIENT_ID="${PUBLIC_OIDC_CLIENT_ID:-finance-spa}"
	AUDIENCE="${PUBLIC_OIDC_AUDIENCE:-finance-api}"
	REDIRECT_URI="http://localhost:18082/auth/callback"
	USERNAME="${FTRK_TEST_USER:-test}"
	PASSWORD="${FTRK_TEST_USER_PASSWORD:-test1234}"
	KC_HOST="${KC%/*}"

	OIDC_WORK="$(mktemp -d /tmp/opencode/oidc-e2e.XXXXXX)"
	PASS=0

	# --- 1. discovery -------------------------------------------------------------
	DISC="$(curl -fsS "${KC}/.well-known/openid-configuration")" ||
		fail "discovery fetch failed — is Keycloak up (make dev-ps)?"
	assert_eq "discovery.issuer" "$KC" "$(jq -r .issuer <<<"$DISC")"
	AUTHORIZE_URL="$(jq -r .authorization_endpoint <<<"$DISC")"
	TOKEN_URL="$(jq -r .token_endpoint <<<"$DISC")"
	ok "authorization_endpoint = $AUTHORIZE_URL"
	ok "token_endpoint = $TOKEN_URL"

	# --- 2. PKCE material + authorize request --------------------------------------
	read -r VERIFIER CHALLENGE STATE NONCE <<<"$(python3 -c '
import base64, hashlib, os, secrets
v = base64.urlsafe_b64encode(os.urandom(48)).decode().rstrip("=")
c = base64.urlsafe_b64encode(hashlib.sha256(v.encode()).digest()).decode().rstrip("=")
print(v, c, secrets.token_urlsafe(16), secrets.token_urlsafe(16))')"

	ENC_REDIRECT="$(python3 -c 'import sys, urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "$REDIRECT_URI")"
	AUTH_URL="${AUTHORIZE_URL}?response_type=code&client_id=${CLIENT_ID}&redirect_uri=${ENC_REDIRECT}&scope=openid%20profile%20email&state=${STATE}&nonce=${NONCE}&code_challenge=${CHALLENGE}&code_challenge_method=S256&audience=${AUDIENCE}"

	# GET the authorize URL and follow redirects to the login page. The jar
	# keeps the AUTH_SESSION_ID cookie the credential POST needs.
	curl -fsSL --max-redirs 10 -c "$OIDC_WORK/cookies" -b "$OIDC_WORK/cookies" -o "$OIDC_WORK/login.html" "$AUTH_URL"
	grep -q 'name="username"' "$OIDC_WORK/login.html" || fail "login page has no username field (unexpected form)"

	# --- 3. parse the credentials form, POST the dev credentials --------------------
	FORM="$(python3 - "$OIDC_WORK/login.html" <<'PY'
import html, re, sys
doc = open(sys.argv[1]).read()
form = None
for m in re.finditer(r'<form\b[^>]*>.*?</form>', doc, re.DOTALL):
    if 'name="username"' in m.group(0):
        form = m.group(0)
        break
if form is None:
    sys.exit("no form with name=\"username\"")
action = re.search(r'action=\"([^\"]+)\"', form)
print(html.unescape(action.group(1)) if action else "")
fields = re.findall(r'<input\b[^>]*\btype="hidden"[^>]*>', form)
pairs = []
for f in fields:
    n = re.search(r'\bname="([^"]+)"', f).group(1)
    v = re.search(r'\bvalue="([^"]*)"', f)
    pairs.append(f"{n}={html.unescape(v.group(1)) if v else ''}")
print("\n".join(pairs))
PY
)"
	FORM_ACTION="$(head -n1 <<<"$FORM")"
	[ -n "$FORM_ACTION" ] || fail "login form has no action attribute"
	case "$FORM_ACTION" in
	http*) ;;
	*) FORM_ACTION="${KC_HOST}${FORM_ACTION}" ;;
	esac

	DATA_ARGS=()
	while IFS= read -r line; do
		[ -n "$line" ] && DATA_ARGS+=(--data-urlencode "$line")
	done < <(tail -n +2 <<<"$FORM")
	DATA_ARGS+=(--data-urlencode "username=${USERNAME}" --data-urlencode "password=${PASSWORD}")

	curl -fsS -c "$OIDC_WORK/cookies" -b "$OIDC_WORK/cookies" -o /dev/null -D "$OIDC_WORK/login.headers" \
		"$FORM_ACTION" "${DATA_ARGS[@]}"

	LOC="$(tr -d '\r' <"$OIDC_WORK/login.headers" | awk 'tolower($1)=="location:" { $1=""; sub(/^[ \t]+/, ""); print }' | head -n1)"
	[ -n "$LOC" ] ||
		fail "no 302 Location after login POST — credentials rejected (check FTRK_TEST_USER / FTRK_TEST_USER_PASSWORD)"

	# --- 4. callback URL: redirect_uri + state round-trip ---------------------------
	CB_PAIR="$(python3 -c '
import sys
from urllib.parse import parse_qs, urlparse
q = parse_qs(urlparse(sys.argv[1]).query)
print(q["code"][0], q["state"][0])' "$LOC")"
	read -r CODE CB_STATE <<<"$CB_PAIR"
	case "$LOC" in
	"$REDIRECT_URI"*) ok "callback landed on the registered redirect_uri" ;;
	*) fail "callback URL is not the registered redirect_uri: $LOC" ;;
	esac
	assert_eq "state round-trip" "$STATE" "$CB_STATE"
	ok "authorization code received (${#CODE} chars)"

	# --- 5. token exchange (PKCE code_verifier, public client) -----------------------
	TOKEN_RESP="$(curl -fsS -X POST "$TOKEN_URL" \
		--data-urlencode "grant_type=authorization_code" \
		--data-urlencode "code=${CODE}" \
		--data-urlencode "code_verifier=${VERIFIER}" \
		--data-urlencode "redirect_uri=${REDIRECT_URI}" \
		--data-urlencode "client_id=${CLIENT_ID}")"
	assert_eq "token_type" "Bearer" "$(jq -r .token_type <<<"$TOKEN_RESP")"
	jq -e '.expires_in | tonumber > 0' >/dev/null <<<"$TOKEN_RESP" || fail "expires_in not positive"
	ok "expires_in = $(jq -r .expires_in <<<"$TOKEN_RESP")"
	ID_TOKEN="$(jq -r .id_token <<<"$TOKEN_RESP")"
	ACCESS_TOKEN="$(jq -r .access_token <<<"$TOKEN_RESP")"
	[ -n "$ID_TOKEN" ] && [ -n "$ACCESS_TOKEN" ] || fail "missing id_token/access_token"

	# --- 6. claim assertions -----------------------------------------------------------
	ID_CLAIMS="$(decode_jwt_claims "$ID_TOKEN")"
	AT_CLAIMS="$(decode_jwt_claims "$ACCESS_TOKEN")"

	assert_eq "id_token.iss" "$KC" "$(jq -r .iss <<<"$ID_CLAIMS")"
	ID_AUD="$(jq -r '.aud | if type == "string" then . else join(",") end' <<<"$ID_CLAIMS")"
	assert_contains "id_token.aud" "$CLIENT_ID" "$ID_AUD"
	[ -n "$(jq -r .sub <<<"$ID_CLAIMS")" ] || fail "id_token.sub is empty"
	ok "id_token.sub = $(jq -r .sub <<<"$ID_CLAIMS")"
	assert_eq "id_token.nonce" "$NONCE" "$(jq -r .nonce <<<"$ID_CLAIMS")"

	assert_eq "access_token.iss" "$KC" "$(jq -r .iss <<<"$AT_CLAIMS")"
	AT_AUD="$(jq -r '.aud | if type == "string" then . else join(",") end' <<<"$AT_CLAIMS")"
	assert_contains "access_token.aud" "$AUDIENCE" "$AT_AUD"
	AT_SCOPE="$(jq -r '.scope // ""' <<<"$AT_CLAIMS")"
	assert_contains "access_token.scope" "profile" "$AT_SCOPE"
	assert_contains "access_token.scope" "email" "$AT_SCOPE"

	echo
	echo "OIDC E2E: all ${PASS} checks passed (PKCE S256 code flow against ${KC})"
}

# --- authn flow ---------------------------------------------------------------------

run_authn() {
	local API KC REALM CLIENT_ID AUDIENCE USERNAME PASSWORD EMAIL GO_IMG TOKEN_URL
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

	AUTHN_WORK="$(mktemp -d /tmp/opencode/verify-authn.XXXXXX)"
	PASS=0

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

	login "$AUTHN_WORK/token1.json"
	T1="$(jq -er .access_token "$AUTHN_WORK/token1.json")" \
		|| fail "no access_token in first login response"
	SUB1="$(jq -er .sub <<<"$(decode_jwt_claims "$T1")")"
	[ -n "$SUB1" ] || fail "access token has no sub claim"
	ok "login 1: access token with sub = $SUB1"

	code="$(curl -s -o "$AUTHN_WORK/me1.json" -w '%{http_code}' \
		-H "Authorization: Bearer $T1" "${API}/api/v1/me")"
	assert_eq "GET /me (login 1)" "200" "$code"
	ME_SUB1="$(jq -er .sub "$AUTHN_WORK/me1.json")" || fail "no sub in /me response"
	assert_eq "/me sub (login 1)" "$SUB1" "$ME_SUB1"

	login "$AUTHN_WORK/token2.json"
	T2="$(jq -er .access_token "$AUTHN_WORK/token2.json")"
	code="$(curl -s -o "$AUTHN_WORK/me2.json" -w '%{http_code}' \
		-H "Authorization: Bearer $T2" "${API}/api/v1/me")"
	assert_eq "GET /me (login 2)" "200" "$code"
	ME_SUB2="$(jq -er .sub "$AUTHN_WORK/me2.json")"
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

	cat > "$AUTHN_WORK/unwrap.go" <<'GO'
// One-off unwrap proof for scripts/verify.sh authn (step 1.3c): proves the
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
		-v "$AUTHN_WORK:/work" -w /work "$GO_IMG" go run unwrap.go)"
	assert_eq "stored DEK unwraps under the KEK" "UNWRAP_OK 32" "$UNWRAP_OUT"

	echo
	echo "AuthN E2E: all ${PASS} checks passed (token 401s, one idempotent user row, DEK unwraps)"
}

# --- run -----------------------------------------------------------------------------

if [ "$RUN_OIDC" = 1 ]; then
	run_oidc
fi
if [ "$RUN_AUTHN" = 1 ]; then
	run_authn
fi

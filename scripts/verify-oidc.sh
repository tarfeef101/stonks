#!/usr/bin/env bash
# Scripted OIDC E2E (step 1.2b): full PKCE authorization-code flow against
# the local dev IdP (Keycloak realm 'finance') using host tools only
# (curl + jq + python3 — the D-001 verification allowance).
#
# This is the step-1.2 DoD evidence that the realm/SPA client config works
# end-to-end in the exact shape the browser SDK (oidc-client-ts, D-018)
# emits:
#
#   1. OIDC discovery (.well-known/openid-configuration)
#   2. authorize request (response_type=code, S256 PKCE challenge, state,
#      nonce, audience)
#   3. login form GET → parse → credential POST
#   4. capture the 302 → assert redirect_uri + state round-trip
#   5. token exchange (grant_type=authorization_code + code_verifier,
#      public client — no client_secret)
#   6. id_token / access_token claim assertions
#      (iss, aud, sub, nonce, scope)
#
# The manual BROWSER side of the DoD (login/logout in the running SPA,
# session persistence, token feeding the API client) is documented in
# docs/DEVELOPMENT.md — a headless host cannot click.
#
# Run:   make dev        # first — Keycloak must be (healthy)
#        scripts/verify-oidc.sh
#
# Env (all have dev defaults; see .env.example for the full story):
#   PUBLIC_AUTH0_ISSUER         default http://localhost:18081/realms/finance
#   PUBLIC_AUTH0_CLIENT_ID      default finance-spa
#   PUBLIC_AUTH0_AUDIENCE       default finance-api
#   FTRK_TEST_USER              default test
#   FTRK_TEST_USER_PASSWORD     default test1234

set -euo pipefail

KC="${PUBLIC_AUTH0_ISSUER:-http://localhost:18081/realms/finance}"
CLIENT_ID="${PUBLIC_AUTH0_CLIENT_ID:-finance-spa}"
AUDIENCE="${PUBLIC_AUTH0_AUDIENCE:-finance-api}"
REDIRECT_URI="http://localhost:18082/auth/callback"
USERNAME="${FTRK_TEST_USER:-test}"
PASSWORD="${FTRK_TEST_USER_PASSWORD:-test1234}"
KC_HOST="${KC%/*}"

WORK="$(mktemp -d /tmp/opencode/oidc-e2e.XXXXXX)"
trap 'rm -rf "$WORK"' EXIT

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
curl -fsSL --max-redirs 10 -c "$WORK/cookies" -b "$WORK/cookies" -o "$WORK/login.html" "$AUTH_URL"
grep -q 'name="username"' "$WORK/login.html" || fail "login page has no username field (unexpected form)"

# --- 3. parse the credentials form, POST the dev credentials --------------------
FORM="$(python3 - "$WORK/login.html" <<'PY'
import html, re, sys
doc = open(sys.argv[1]).read()
form = None
for m in re.finditer(r'<form\b[^>]*>.*?</form>', doc, re.DOTALL):
    if 'name="username"' in m.group(0):
        form = m.group(0)
        break
if form is None:
    sys.exit("no form with name=\"username\"")
action = re.search(r'action="([^"]+)"', form)
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

curl -fsS -c "$WORK/cookies" -b "$WORK/cookies" -o /dev/null -D "$WORK/login.headers" \
	"$FORM_ACTION" "${DATA_ARGS[@]}"

LOC="$(tr -d '\r' <"$WORK/login.headers" | awk 'tolower($1)=="location:" { $1=""; sub(/^[ \t]+/, ""); print }' | head -n1)"
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

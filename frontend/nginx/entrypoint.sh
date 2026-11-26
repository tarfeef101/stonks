#!/bin/sh
# Render the per-build CSP template (shipped as /etc/nginx/csp-template.conf
# by the Dockerfile) into /tmp/nginx/headers.conf, then start nginx.
#
# Deploy-time config: the SPA itself fetches /api/v1/public-config at
# runtime; the only values nginx needs are the IdP origin (frame-src for
# the silent-renew iframe, connect-src for browser-side OIDC discovery /
# token / userinfo) and, when the NR browser agent is on, its data
# endpoint (connect-src). /tmp/nginx is the one location the unprivileged
# nginx user can write (see Dockerfile).
set -eu

# OIDC_ISSUER is required: without an IdP the app cannot be used, and a
# CSP missing the IdP origin would break login on first paint — fail fast
# instead of serving a misconfigured header.
case "${OIDC_ISSUER:?OIDC_ISSUER is required (e.g. https://idp.example.com/realms/finance)}" in
  http://* | https://*) ;;
  *)
    echo 'entrypoint: OIDC_ISSUER must be an http(s) URL' >&2
    exit 1
    ;;
esac

# Origin = scheme + host[:port] only (the realm path is irrelevant at the
# origin level). Busybox ash has no ${var//} pattern substitution, and the
# URL contains "://", so peel scheme/path by parameter expansion:
scheme=${OIDC_ISSUER%%:*}
rest=${OIDC_ISSUER#*://}
issuer_origin="${scheme}://${rest%%/*}"

# The NR browser agent posts to bam.nr-data.net; allow it only when the
# deployment ships a license key (the SPA loads the agent in the same
# condition — same secret, same gate).
connect_extra=
if [ -n "${NR_LICENSE_KEY:-}" ]; then
  connect_extra=' https://bam.nr-data.net'
fi

# sed '|' delimiter: both substitution values are constrained
# (origin = validated http(s) URL minus path; connect_extra is a
# hardcoded constant), so no delimiter/escaping clashes are possible.
sed -e "s|__ISSUER_ORIGIN__|${issuer_origin}|g" \
    -e "s|__CONNECT_EXTRA__|${connect_extra}|g" \
    /etc/nginx/csp-template.conf > /tmp/nginx/headers.conf

exec nginx -g 'daemon off;'

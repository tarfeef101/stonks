# Personal Finance Tracker — frontend
#
# Stage 1: build the SvelteKit SPA in node:26.13.0 (pinned in
# .tool-versions and the Makefile), then derive the nginx
# security-header include (scripts/generate-csp.mjs) from the REAL
# built index.html — the Content-Security-Policy inline-script hashes
# are per-build and must never be hardcoded.
# Stage 2: serve the static bundle with nginx:alpine (1.31.x as of
# 2026-11-02), non-root (USER nginx, L42) on port 8080.
#
# Build-time env (baked into the bundle at build time; all optional —
# defaults keep the dev image working with zero configuration):
#   PUBLIC_API_BASE           API base path           (default /api)
#   PUBLIC_NR_LICENSE_KEY     NR browser license key  (agent off when unset)
#   PUBLIC_NR_APPLICATION_ID  NR browser app id       (agent off when unset)
#   PUBLIC_OIDC_ISSUER        IdP issuer URL; its origin goes into the
#                             CSP frame-src (default
#                             http://localhost:18081/realms/finance,
#                             i.e. the src/lib/auth/client.ts fallback)

FROM node:26.13.0 AS build
WORKDIR /src
COPY package.json package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci --no-audit --no-fund
COPY . .
ARG PUBLIC_API_BASE=/api
ARG PUBLIC_NR_LICENSE_KEY=
ARG PUBLIC_NR_APPLICATION_ID=
ARG PUBLIC_OIDC_ISSUER=http://localhost:18081/realms/finance
ENV PUBLIC_API_BASE=$PUBLIC_API_BASE \
    PUBLIC_NR_LICENSE_KEY=$PUBLIC_NR_LICENSE_KEY \
    PUBLIC_NR_APPLICATION_ID=$PUBLIC_NR_APPLICATION_ID \
    PUBLIC_OIDC_ISSUER=$PUBLIC_OIDC_ISSUER
# Build, then derive the CSP include from the built index.html. The
# NR connect-src origin is only added when the agent is compiled in
# (a license key was provided at build time).
RUN npm run build \
    && if [ -n "$PUBLIC_NR_LICENSE_KEY" ]; then \
           export CSP_CONNECT_EXTRA='https://bam.nr-data.net'; \
       else \
           export CSP_CONNECT_EXTRA=''; \
       fi \
    && CSP_ISSUER="$PUBLIC_OIDC_ISSUER" node scripts/generate-csp.mjs

FROM nginx:alpine
# The nginx:alpine base ships distro packages with known CVEs (brotli,
# curl, libexpat, libxml2, libxslt — trivy gate, round 58); all are fixed
# in the current alpine 3.24 repo, so upgrade at build time.
RUN apk upgrade --no-cache
# SPA + security config: our own main conf (no user directive — the
# container runs as the unprivileged nginx user), our server block
# (port 8080, per-location header includes), and the per-build CSP
# include derived from the built index.html.
RUN rm /etc/nginx/nginx.conf /etc/nginx/conf.d/default.conf
COPY nginx/nginx.conf /etc/nginx/nginx.conf
COPY nginx/default.conf /etc/nginx/conf.d/default.conf
COPY --from=build /src/csp.conf /etc/nginx/headers.conf
COPY --from=build /src/build /usr/share/nginx/html
# nginx's startup mkdir is single-level: it will create /tmp/nginx/<temp>
# itself, but the /tmp/nginx parent must already exist (and be writable by
# the nginx user — /var/cache/nginx is root-owned, hence /tmp).
RUN mkdir -p /tmp/nginx && chown nginx:nginx /tmp/nginx
USER nginx
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s \
    CMD wget -qO- http://127.0.0.1:8080/ >/dev/null 2>&1 || exit 1

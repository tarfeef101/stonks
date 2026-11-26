# Personal Finance Tracker — frontend
#
# Stage 1: build the SvelteKit SPA in node:26.13.0 (pinned in
# .tool-versions and the Makefile), then derive the nginx
# security-header TEMPLATE (scripts/generate-csp.mjs) from the REAL
# built index.html — the Content-Security-Policy inline-script hashes
# are per-build and must never be hardcoded.
# Stage 2: serve the static bundle with nginx:alpine (1.31.x as of
# 2026-11-02), non-root (USER nginx) on port 8080.
#
# The SPA takes NO build-time config (round 64): it fetches its settings
# from GET /api/v1/public-config at RUNTIME. The build only emits the CSP
# template (with __ISSUER_ORIGIN__ / __CONNECT_EXTRA__ markers) derived
# from the built index.html hashes; the container entrypoint
# (nginx/entrypoint.sh) renders the template from deploy-time env
# (OIDC_ISSUER, NR_LICENSE_KEY) into /tmp/nginx/headers.conf at startup.

FROM node:26.13.0 AS build
WORKDIR /src
COPY package.json package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci --no-audit --no-fund
COPY . .
# Build, then derive the CSP TEMPLATE from the built index.html. The
# template carries the per-build hashes plus two __MARKERS__ that the
# nginx entrypoint renders from deploy-time env at container start.
RUN npm run build && node scripts/generate-csp.mjs

FROM nginx:alpine
LABEL org.opencontainers.image.source=https://github.com/tarfeef101/stonks
# The nginx:alpine base ships distro packages with known CVEs (brotli,
# curl, libexpat, libxml2, libxslt — trivy gate, round 58); all are fixed
# in the current alpine 3.24 repo, so upgrade at build time.
RUN apk upgrade --no-cache
# SPA + security config: our own main conf (no user directive — the
# container runs as the unprivileged nginx user), our server block
# (port 8080, per-location header includes), and the per-build CSP
# template derived from the built index.html — the entrypoint renders
# it into /tmp/nginx/headers.conf from deploy-time env at start.
RUN rm /etc/nginx/nginx.conf /etc/nginx/conf.d/default.conf
COPY nginx/nginx.conf /etc/nginx/nginx.conf
COPY nginx/default.conf /etc/nginx/conf.d/default.conf
COPY --from=build /src/csp.conf /etc/nginx/csp-template.conf
COPY nginx/entrypoint.sh /etc/nginx/entrypoint.sh
RUN chmod +x /etc/nginx/entrypoint.sh
COPY --from=build /src/build /usr/share/nginx/html
# nginx's startup mkdir is single-level: it will create /tmp/nginx/<temp>
# itself, but the /tmp/nginx parent must already exist (and be writable by
# the nginx user — /var/cache/nginx is root-owned, hence /tmp). The
# entrypoint also writes the rendered headers.conf there, before nginx
# starts.
RUN mkdir -p /tmp/nginx && chown nginx:nginx /tmp/nginx
USER nginx
# Replaces the base image's docker-entrypoint.sh (safe: our nginx.conf
# sets no worker_processes, so no auto-tuning is bypassed) — renders the
# CSP template from env, then execs nginx itself.
ENTRYPOINT ["/etc/nginx/entrypoint.sh"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s \
    CMD wget -qO- http://127.0.0.1:8080/ >/dev/null 2>&1 || exit 1

# Personal Finance Tracker — frontend
#
# Stage 1: build the SvelteKit SPA in node:26.13.0 (pinned in .tool-versions
# and the Makefile).
# Stage 2: serve the static bundle with nginx:alpine (1.31.x as of 2026-11-02).
#
# Build-time env (baked into the bundle at build time; all optional —
# defaults keep the dev image working with zero configuration):
#   PUBLIC_API_BASE           API base path           (default /api)
#   PUBLIC_NR_LICENSE_KEY     NR browser license key  (agent off when unset)
#   PUBLIC_NR_APPLICATION_ID  NR browser app id       (agent off when unset)

FROM node:26.13.0 AS build
WORKDIR /src
COPY package.json package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci --no-audit --no-fund
COPY . .
ARG PUBLIC_API_BASE=/api
ARG PUBLIC_NR_LICENSE_KEY=
ARG PUBLIC_NR_APPLICATION_ID=
ENV PUBLIC_API_BASE=$PUBLIC_API_BASE \
    PUBLIC_NR_LICENSE_KEY=$PUBLIC_NR_LICENSE_KEY \
    PUBLIC_NR_APPLICATION_ID=$PUBLIC_NR_APPLICATION_ID
RUN npm run build

FROM nginx:alpine
# SPA config: unknown paths fall back to /index.html (client-side routing).
COPY <<'EOF' /etc/nginx/conf.d/default.conf
server {
    listen 80;
    listen [::]:80;
    server_name _;

    root /usr/share/nginx/html;
    index index.html;

    gzip on;
    gzip_types text/css application/javascript application/json image/svg+xml;

    # Hashed assets from the SvelteKit build — safe to cache forever.
    location /_app/ {
        add_header Cache-Control "public, max-age=31536000, immutable";
        try_files $uri =404;
    }

    # SPA fallback: real files first, then the app shell.
    location / {
        try_files $uri $uri/ /index.html;
    }
}
EOF
COPY --from=build /src/build /usr/share/nginx/html
EXPOSE 80
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s \
    CMD wget -qO- http://127.0.0.1/ >/dev/null 2>&1 || exit 1

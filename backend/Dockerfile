# Personal Finance Tracker — backend image.
#
# One image carries the api + worker binaries and the goose CLI; the
# compose service / chart selects the entrypoint (the compose migrate
# service and the Phase 10 k8s migration Job run `goose`). Multi-stage,
# non-root runtime, container-level healthcheck.

FROM golang:1.27.3 AS build
WORKDIR /src
# Cache module downloads separately from the source copy.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=dev
RUN CGO_ENABLED=0 GOOS=linux \
    go build -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" -o /out/api ./cmd/api \
 && CGO_ENABLED=0 GOOS=linux \
    go build -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" -o /out/worker ./cmd/worker
# The goose CLI is image tooling, not a library dependency of our code:
# install it by module version (go install pkg@version) so go.mod/go.sum
# stay library-only instead of carrying the CLI's full driver graph.
# Every driver goose ships is pure Go, so CGO stays off. v3.30.2 is the
# first release whose module graph pins golang.org/x/net >= v0.62.0
# (CVE-2026-78661, x/net/html DoS — trivy gate, round 58).
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go install github.com/pressly/goose/v3/cmd/goose@v3.30.2 \
 && cp /go/bin/goose /out/goose

FROM alpine:3.23
RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -g 10001 ftrk \
 && adduser -D -H -u 10001 -G ftrk ftrk
COPY --from=build /out/api /out/worker /out/goose /usr/local/bin/
USER ftrk
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["api"]

# Personal Finance Tracker — backend image.
#
# One image carries both binaries (api + worker); the compose service /
# chart selects the entrypoint. Multi-stage, non-root runtime,
# container-level healthcheck.

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

FROM alpine:3.23
RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -g 10001 ftrk \
 && adduser -D -H -u 10001 -G ftrk ftrk
COPY --from=build /out/api /out/worker /usr/local/bin/
USER ftrk
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["api"]

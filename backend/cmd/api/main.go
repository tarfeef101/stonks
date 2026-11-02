// Command api is the HTTP server binary.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"finance-tracker/internal/config"
	"finance-tracker/internal/server"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/newrelic/go-agent/v3/newrelic"
)

// version and commit are injected via -ldflags at build time.
var (
	version = "dev"
	commit  = "dev"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("config load failed", "error", err)
		os.Exit(1)
	}

	// New Relic agent (plan: no-ops without a license in iteration 1).
	// go-agent v3.45.x API: NewApplication returns an error on bad config
	// (e.g. a malformed license), and HTTP instrumentation goes through
	// the nil-safe WrapHandle. With no license the agent is disabled
	// explicitly, which keeps every instrumentation call a no-op.
	nrOpts := []newrelic.ConfigOption{
		newrelic.ConfigAppName("finance-tracker-api"),
		newrelic.ConfigLicense(cfg.NRLicenseKey),
	}
	if cfg.NRLicenseKey == "" {
		nrOpts = append(nrOpts, newrelic.ConfigEnabled(false))
	}
	nrApp, err := newrelic.NewApplication(nrOpts...)
	if err != nil {
		logger.Error("new relic agent init failed", "error", err)
		os.Exit(1)
	}
	defer nrApp.Shutdown(5 * time.Second)

	// Database pool. pgxpool.New does not connect immediately; readiness
	// is surfaced through /readyz. An empty DATABASE_URL leaves the pool
	// unset (the API still serves /healthz and /metrics).
	var ready server.Readiness
	if cfg.DatabaseURL != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
		cancel()
		if err != nil {
			logger.Error("database pool create failed", "error", err)
			os.Exit(1)
		}
		defer pool.Close()
		ready = pool
	}

	router := server.New(ready, server.BuildInfo{Version: version, Commit: commit})
	// WrapHandle is nil-safe and a no-op while the agent is disabled.
	// Route-accurate transaction naming can replace the static pattern in
	// a later step.
	_, handler := newrelic.WrapHandle(nrApp, "web", router)

	srv := &http.Server{
		Addr:              ":" + strconv.Itoa(cfg.Port),
		Handler:           handler,
		ReadTimeout:       10 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("api listening", "port", cfg.Port, "version", version, "commit", commit)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-errCh:
		logger.Error("server error", "error", err)
		os.Exit(1)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
	}
	logger.Info("shutdown complete")
}

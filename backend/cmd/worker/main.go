// Command worker is the background job runner.
//
// In iteration 1 this is a stub: it loads configuration (the same source
// as the API) and idles until signalled. The actual jobs — categorisation,
// imports, reports, FI sync, leader election — land in later steps
// (4.1, 5.4, 8.2, 8.3, 9.2).
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"finance-tracker/internal/config"
)

// version is injected via -ldflags at build time.
var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("config load failed", "error", err)
		os.Exit(1)
	}
	_ = cfg

	logger.Info("worker started (stub)", "version", version)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	logger.Info("worker shutting down")
}

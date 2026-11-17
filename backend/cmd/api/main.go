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

	"finance-tracker/internal/authn"
	"finance-tracker/internal/categorize"
	"finance-tracker/internal/config"
	"finance-tracker/internal/crypto"
	"finance-tracker/internal/llm"
	"finance-tracker/internal/mail"
	"finance-tracker/internal/repo"
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

	// Transactional email (step 8.1): configured from SMTP_* regardless
	// of the database — with no host the sender is a NopSender (messages
	// are logged and dropped), so a mail outage can never block the API.
	mailer := mail.New(cfg.SMTP)
	mailEnabled := cfg.SMTP.Host != ""

	// Database pool. pgxpool.New does not connect immediately; readiness
	// is surfaced through /readyz. An empty DATABASE_URL leaves the pool
	// unset (the API still serves /healthz and /metrics; /api/v1/me then
	// answers 503).
	var (
		ready         server.Readiness
		pool          *pgxpool.Pool
		bootstrap     *server.Bootstrap
		settings      server.SettingsStore
		accounts      server.AccountStore
		categories    server.CategoryStore
		transactions  *repo.TransactionStore
		budgets       server.BudgetStore
		notifications server.NotificationStore
		schemas       server.SchemaStore
		imports       server.ImportStore
		jobs          *repo.CategoriseJobStore
		review        server.ReviewStore
		reprompt      *server.Reprompter
		llmCreds      *server.LLMCredentials
		dashboards    server.DashboardStore
		widgets       server.DashboardWidgetStore
		savedViews    server.SavedViewStore
	)
	if cfg.DatabaseURL != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		pool, err = pgxpool.New(ctx, cfg.DatabaseURL)
		cancel()
		if err != nil {
			logger.Error("database pool create failed", "error", err)
			os.Exit(1)
		}
		defer pool.Close()
		ready = pool
	}

	// AuthN (step 1.3). The verifier requires an issuer and audience; a
	// missing value is a configuration error, so fail fast at boot rather
	// than serving a server that can only ever answer 401.
	if cfg.Auth0Issuer == "" || cfg.Auth0Audience == "" {
		logger.Error("authn misconfigured",
			"hint", "AUTH0_ISSUER and AUTH0_AUDIENCE are required (see .env.example)")
		os.Exit(1)
	}
	verifier, err := authn.New(authn.Config{
		Issuer:       cfg.Auth0Issuer,
		Audience:     cfg.Auth0Audience,
		DiscoveryURL: cfg.OIDCDiscoveryURL,
	})
	if err != nil {
		logger.Error("authn verifier init failed", "error", err)
		os.Exit(1)
	}

	// User bootstrap (step 1.3c): the first authenticated request creates
	// the users row and stores the KEK-wrapped per-user DEK. The KEK is
	// only needed with a database; a missing or malformed value there is
	// a configuration error, so fail fast at boot (make gen-kek).
	if pool != nil {
		if cfg.KEK == "" {
			logger.Error("kek misconfigured",
				"hint", "KEK is required with DATABASE_URL: 64 hex chars (make gen-kek, .env)")
			os.Exit(1)
		}
		kek, err := crypto.KEKFromHex(cfg.KEK)
		if err != nil {
			logger.Error("kek parse failed", "error", err)
			os.Exit(1)
		}
		bootstrap, err = server.NewBootstrap(server.NewPGUserStore(pool), kek, mailer)
		if err != nil {
			logger.Error("user bootstrap init failed", "error", err)
			os.Exit(1)
		}
		// Dashboard cards backfill (round 6 rework): one-time startup
		// migration moving 0008 dashboard_cards rows into the new
		// dashboards/dashboard_widgets tables (one "Default" dashboard
		// per user, cards become explore widgets), dropping the old
		// table once every user's cards have moved. Deliberately
		// non-fatal: there is no restart policy here, so a failure
		// (e.g. the goose migrate service still applying 0013) is
		// logged and retried on the next boot — the backfill is
		// idempotent and a silent no-op once the cards table is gone
		// or empty.
		bctx, bcancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := repo.Backfill(bctx, pool, kek); err != nil {
			logger.Error("dashboard cards backfill failed", "error", err)
		}
		bcancel()
		// User settings (step 1.4a): same pool, no KEK of its own — the
		// settings columns are plaintext.
		settings = server.NewPGSettingsStore(pool)
		// Accounts and categories (step 3.1): same pool; names are
		// DEK-encrypted by the repo layer, the handlers pass the per-user
		// DEK from bootstrap.
		accounts = repo.NewAccountStore(pool)
		categories = repo.NewCategoryStore(pool)
		// Transactions and budgets (step 3.2): same pool; the summary
		// route needs both (plus categories) configured.
		transactions = repo.NewTransactionStore(pool)
		budgets = repo.NewBudgetStore(pool)
		// Notifications and import schemas (step 3.3): same pool; the
		// notification routes and the import-schemas list need them
		// configured.
		notifications = repo.NewNotificationStore(pool)
		schemas = repo.NewSchemaStore(pool)
		// Import jobs (step 4.1): same pool; the statement file and
		// filename are DEK-encrypted by the repo layer (the per-user
		// DEK comes from bootstrap, like every other encrypted column).
		imports = repo.NewImportJobStore(pool)
		// Categorise jobs (step 5.4): same pool; the worker
		// (cmd/worker) claims and drives the queue, the API only
		// enqueues (POST) and reads (GET) the caller's jobs.
		jobs = repo.NewCategoriseJobStore(pool)
		// Review queue and reprompt (step 5.5): the review routes read
		// and mutate the review-pending rows the worker flagged (same
		// *repo.TransactionStore, no extra store of its own); the
		// reprompt route re-runs the LLM on one transaction, resolving
		// the provider the same way the worker does (the user's
		// credential, else the process LLM_* env fallback normalised
		// to the compose defaults).
		review = transactions
		reprompt = &server.Reprompter{
			Creds: repo.NewCredentialStore(pool),
			Cats:  repo.NewCategoryStore(pool),
			Txns:  transactions,
			Env: categorize.NormalizeEnv(categorize.LLMEnv{
				BaseURL: cfg.LLM.BaseURL, APIKey: cfg.LLM.APIKey, Model: cfg.LLM.Model,
			}),
			// Endpoint policy (T-P0-1): strict mode, with the
			// docker-network llama service admitted via
			// FTRK_LLM_ENDPOINT_CIDRS.
			LLM: func(baseURL, apiKey, model string) (llm.Categorizer, error) {
				return llm.New(baseURL, apiKey, model, llm.WithEndpointAllowlist(cfg.LLM.EndpointCIDRs)), nil
			},
		}
		// LLM credentials (step 5.6): the per-user credential routes
		// (same pool; the key is sealed with the caller's DEK) plus the
		// test-connection probe, which reuses the process LLM fallback
		// for its model (credentials carry no model) and a 20 s
		// per-attempt timeout (WithTimeout applies to every attempt,
		// the nudge retry included).
		llmCreds = &server.LLMCredentials{
			Store: repo.NewCredentialStore(pool),
			Env: categorize.NormalizeEnv(categorize.LLMEnv{
				BaseURL: cfg.LLM.BaseURL, APIKey: cfg.LLM.APIKey, Model: cfg.LLM.Model,
			}),
			LLM: func(baseURL, apiKey, model string) (llm.Categorizer, error) {
				return llm.New(baseURL, apiKey, model, llm.WithTimeout(20*time.Second),
					llm.WithEndpointAllowlist(cfg.LLM.EndpointCIDRs)), nil
			},
		}
		// Dashboards + widgets (round 6 rework, migration 0013): same
		// pool; widget names and content are DEK-encrypted by the repo
		// layer, the handlers pass the per-user DEK from bootstrap
		// (like every other store).
		dashboards = repo.NewDashboardStore(pool)
		widgets = repo.NewDashboardWidgetStore(pool)
		// Saved Explore views (round 16, migration 0015): same pool;
		// name and state are DEK-encrypted by the repo layer, the
		// handlers pass the per-user DEK from bootstrap (like the
		// dashboards above).
		savedViews = repo.NewSavedViewStore(pool)
	}

	router := server.New(ready, server.BuildInfo{Version: version, Commit: commit}, verifier, bootstrap, settings, accounts, categories, transactions, budgets, notifications, schemas, imports, jobs, review, reprompt, llmCreds, dashboards, widgets, savedViews, repo.NewMortgageStore(pool), mailer, mailEnabled, repo.NewInstitutionStore(pool), repo.NewFICredentialStore(pool), transactions)
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

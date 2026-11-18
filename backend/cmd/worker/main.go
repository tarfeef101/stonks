// Command worker is the background job runner.
//
// Plan 5.4: it runs the categorisation queue. At startup it resets any
// rows left 'running' by a crash (crash recovery), then polls the queue
// every pollInterval and drives each claimed job to its terminal state
// through categorize.Runner. Later steps add more job kinds here.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"finance-tracker/internal/categorize"
	"finance-tracker/internal/config"
	"finance-tracker/internal/crypto"
	"finance-tracker/internal/fisync"
	"finance-tracker/internal/llm"
	"finance-tracker/internal/mail"
	"finance-tracker/internal/repo"
	"finance-tracker/internal/report"
	"finance-tracker/internal/schedule"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pollInterval is how often the idle worker polls the categorise queue.
const pollInterval = 5 * time.Second

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

	// The worker has nothing to do without a database, so an empty
	// DATABASE_URL is a configuration error (unlike the API, which still
	// serves /healthz without one).
	if cfg.DatabaseURL == "" {
		logger.Error("worker misconfigured", "hint", "DATABASE_URL is required")
		os.Exit(1)
	}
	ctx0, cancel0 := context.WithTimeout(context.Background(), 5*time.Second)
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		logger.Error("database pool config failed")
		os.Exit(1)
	}
	poolCfg.MaxConns = 8 // pinned: the worker's steady-state ceiling — its jobs are serial
	pool, err := pgxpool.NewWithConfig(ctx0, poolCfg)
	cancel0()
	if err != nil {
		logger.Error("database pool create failed", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	// The worker unwraps per-user DEKs, so the KEK is mandatory.
	if cfg.KEK == "" {
		logger.Error("kek misconfigured",
			"hint", "KEK is required: 64 hex chars (make gen-kek, .env)")
		os.Exit(1)
	}
	kek, err := crypto.KEKFromHex(cfg.KEK)
	if err != nil {
		logger.Error("kek parse failed", "error", err)
		os.Exit(1)
	}

	// Crash recovery: a previous run may have died between the claim and
	// the terminal Finish, leaving 'running' rows that no worker owns
	// any more. Reset them to pending so they get re-claimed.
	jobs := repo.NewCategoriseJobStore(pool)
	rc, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	reset, err := jobs.ResetRunning(rc)
	cancel()
	if err != nil {
		logger.Error("reset running jobs failed", "error", err)
		os.Exit(1)
	}

	// DekForUser resolves the per-user DEK the way the API's bootstrap
	// path stores it: the users row carries the KEK-wrapped pack.
	dekForUser := func(ctx context.Context, userID string) ([]byte, error) {
		var wrapped []byte
		qerr := pool.QueryRow(ctx,
			`SELECT wrapped_dek FROM users WHERE id = $1`, userID).Scan(&wrapped)
		if errors.Is(qerr, sql.ErrNoRows) {
			return nil, fmt.Errorf("worker: user %s not found", userID)
		}
		if qerr != nil {
			return nil, fmt.Errorf("worker: fetch user %s: %w", userID, qerr)
		}
		dek, uerr := crypto.UnwrapDEK(kek, wrapped)
		if uerr != nil {
			return nil, fmt.Errorf("worker: unwrap DEK for %s: %w", userID, uerr)
		}
		return dek, nil
	}

	// Runner with the plan defaults (batch 25, threshold 0.75, the
	// llm.New factory, env fallback normalised to the compose defaults
	// when LLM_BASE_URL / LLM_MODEL are unset). Nots records the plan
	// 5.5 review notification when a job finishes with review rows.
	r := categorize.NewRunner(
		jobs,
		repo.NewCredentialStore(pool),
		repo.NewCategoryStore(pool),
		repo.NewTransactionStore(pool),
		repo.NewNotificationStore(pool),
		dekForUser,
		categorize.LLMEnv{BaseURL: cfg.LLM.BaseURL, APIKey: cfg.LLM.APIKey, Model: cfg.LLM.Model},
	)
	// Endpoint policy (T-P0-1): the worker dials user-influenced LLM
	// endpoints, so it runs the same strict policy as the API —
	// WithEndpointAllowlist is always armed, and FTRK_LLM_ENDPOINT_CIDRS
	// admits the docker-network llama service. With the CIDRs unset the
	// default http://llama:8080/v1 (a private address) is blocked until
	// the operator sets them.
	r.LLM = func(baseURL, apiKey, model string) (llm.Categorizer, error) {
		return llm.New(baseURL, apiKey, model, llm.WithEndpointAllowlist(cfg.LLM.EndpointCIDRs)), nil
	}

	// Scheduler leader election (step 8.3): a dedicated *pgx.Conn (NOT
	// the pool) holds the session advisory locks, so the lock survives
	// pool churn. pgx.Connect dials eagerly, so a dead database fails
	// fast at startup.
	lc, lcancel := context.WithTimeout(context.Background(), 10*time.Second)
	leaderConn, lerr := pgx.Connect(lc, cfg.DatabaseURL)
	lcancel()
	if lerr != nil {
		logger.Error("leader connection failed", "error", lerr)
		os.Exit(1)
	}
	defer leaderConn.Close(context.Background())

	// The clock: WORKER_FAKE_NOW (dev-only) pins the instant so a
	// compose run can exercise the startup dispatch deterministically.
	schedClock := schedule.NewClock()
	if _, fixed := schedClock.(schedule.FixedClock); fixed {
		logger.Warn("worker: WORKER_FAKE_NOW override active (dev only)")
	}

	// Report dispatch (step 8.3): the daily 00:15 UTC batch — for every
	// (user, due period): DEK -> Collect -> Generate -> render -> SMTP.
	dispatcher := schedule.NewDispatcher(
		schedClock,
		schedule.NewPoolUserLister(pool),
		&report.Collector{
			Tx:       schedule.NewTransactionAdapter(repo.NewTransactionStore(pool)),
			Budget:   schedule.NewBudgetAdapter(repo.NewBudgetStore(pool)),
			Category: schedule.NewCategoryAdapter(repo.NewCategoryStore(pool)),
		},
		dekForUser,
		mail.New(cfg.SMTP),
		schedule.NewPoolNotifier(repo.NewNotificationStore(pool)),
	)
	dispatcher.Log = logger

	// FI sync (step 9.2): dev-only fake adapter behind an explicit env gate; real
	// adapters are added per docs/ADDING-A-FI.md (no env gate).
	var fiAdapters []fisync.InstitutionAdapter
	if os.Getenv("FI_FAKE_ADAPTERS") == "1" {
		fiAdapters = append(fiAdapters, fisync.NewFakeAdapter(os.Getenv("FI_FAKE_ADAPTER_FAIL") == "1"))
	}
	fiJob := fisync.NewJob(
		repo.NewFICredentialStore(pool),
		repo.NewInstitutionStore(pool),
		repo.NewAccountStore(pool),
		repo.NewTransactionStore(pool),
		repo.NewNotificationStore(pool),
		fisync.NewRegistry(fiAdapters...),
		dekForUser, // the existing KEK-unwrap closure — reuse, don't duplicate
	)

	fiHook := schedule.NewFiSyncHook()
	fiHook.Log = logger
	fiHook.Clock = schedClock
	fiHook.RunFI = fiJob.RunOnce

	// P1 schedule plane: the offset-suggestion scan and the mortgage
	// self-heal. Both runs are stateless per fire, so the fresh repo
	// constructors below just share the pool, matching the fiJob/
	// dispatcher style above.
	offsetSuggester := &schedule.OffsetSuggester{
		Log:        logger,
		Clock:      schedClock,
		Txns:       repo.NewTransactionStore(pool),
		DekForUser: dekForUser,
	}
	mortgageHealer := &schedule.MortgageHealer{
		Log:        logger,
		Users:      schedule.NewPoolUserIDLister(pool),
		Notifs:     repo.NewNotificationStore(pool),
		Mortgages:  repo.NewMortgageStore(pool),
		Txns:       repo.NewTransactionStore(pool),
		DekForUser: dekForUser,
	}

	sched := schedule.NewScheduler(schedClock, schedule.NewAdvisoryLock(leaderConn), []schedule.Entry{
		{Name: "report-dispatch", Spec: schedule.ReportDispatchSpec, Key: schedule.LockKeyReport, Run: dispatcher.Dispatch},
		{Name: "fi-sync", Spec: schedule.FiSyncSpec, Key: schedule.LockKeyFiSync, Run: fiHook.Run},
		{Name: "offset-suggestions", Spec: schedule.OffsetSuggestSpec, Key: schedule.LockKeyOffsetSuggest, Run: offsetSuggester.Run},
		{Name: "mortgage-self-heal", Spec: schedule.MortgageSelfHealSpec, Key: schedule.LockKeyMortgageSelfHeal, Run: mortgageHealer.Run},
	})
	sched.Log = logger

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("worker started", "version", version,
		"reset_running", reset, "poll_interval", pollInterval.String())

	go sched.Start(ctx)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			break
		}
		job, err := jobs.ClaimNext(ctx)
		if err != nil {
			if errors.Is(err, repo.ErrNoPendingCategoriseJob) {
				// Idle: no pending jobs. Debug level so an idle worker
				// stays quiet; the ticker paces the re-poll.
				logger.Debug("categorise queue empty", "retry_in", pollInterval.String())
			} else if ctx.Err() != nil {
				break // shutting down mid-claim
			} else {
				// Transient DB error: back off one tick and retry.
				logger.Error("claim next failed", "error", err)
			}
			select {
			case <-ctx.Done():
			case <-ticker.C:
			}
			continue
		}

		logger.Info("categorise job claimed", "job", job.ID,
			"user", job.UserID, "source", job.Source)
		runErr := r.RunJob(ctx, job.ID, job.UserID)
		if ctx.Err() != nil {
			// Shutdown raced the job: the terminal state was persisted
			// best-effort with a cancelled context; stop claiming.
			break
		}
		if runErr != nil {
			// The terminal Finish persist failed, so the row is still
			// 'running'; the next worker startup re-queues it via
			// ResetRunning.
			logger.Error("categorise job terminal persist failed", "job", job.ID, "error", runErr)
			continue
		}
		finished, gerr := jobs.Get(ctx, job.UserID, job.ID)
		if gerr != nil {
			logger.Error("fetch finished job failed", "job", job.ID, "error", gerr)
			continue
		}
		logger.Info("categorise job finished", "job", job.ID,
			"status", finished.Status,
			"total", finished.Total, "done", finished.Done,
			"auto_applied", finished.AutoApplied, "to_review", finished.ToReview,
			"tokens_in", finished.TokensIn, "tokens_out", finished.TokensOut)
	}
	logger.Info("worker shutting down")
}

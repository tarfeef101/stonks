// Job runner (plan step 5.4): drives one claimed categorise job from
// the queue to its terminal state. The worker (cmd/worker) claims jobs
// and hands them here; RunJob resolves the provider (the user's stored
// credential, else the process LLM_* env fallback), counts the
// uncategorised pool, and walks it in batches: each batch is one
// categorize.Run call, high-confidence suggestions are applied directly
// (category_id) and low-confidence ones flagged for review
// (review_pending). Token usage accumulates per batch via Advance; the
// job finishes 'done' when the pool empties (or a batch produced no
// suggestions — the stuck guard) and 'failed' on any error, with
// already-applied batches left applied.
package categorize

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"finance-tracker/internal/llm"
	"finance-tracker/internal/repo"
)

// LLMEnv is the server-level LLM fallback used when the user has no
// stored credential: the same source as the backend's LLM_* env vars.
type LLMEnv struct {
	BaseURL string // API root, ends in /v1 ("" -> defaultBaseURL)
	APIKey  string // "" = keyless self-hosted (no Authorization header)
	Model   string // model id ("" -> defaultModel)
}

// NormalizeEnv fills an LLMEnv's empty BaseURL/Model with the
// compose-network defaults (APIKey stays as-is: "" is the legitimate
// keyless self-hosted value). Both the worker (NewRunner) and the
// reprompt route (server.Reprompter) normalise through it, so the
// fallback is one value in one place.
func NormalizeEnv(env LLMEnv) LLMEnv {
	if env.BaseURL == "" {
		env.BaseURL = defaultBaseURL
	}
	if env.Model == "" {
		env.Model = defaultModel
	}
	return env
}

// Runner walks one categorise job. All stores are user-scoped by the
// job's user_id; DekForUser resolves the per-user DEK (worker: the
// users row + crypto.UnwrapDEK); LLM builds the provider client from
// the resolved endpoint/key and the env model (credentials carry no
// model of their own). Nots records the plan 5.5 "N transactions need
// review" notification when a job finishes with a non-empty review
// queue; nil skips it (the queue is still reachable via the API).
type Runner struct {
	Jobs       *repo.CategoriseJobStore
	Creds      *repo.CredentialStore
	Cats       *repo.CategoryStore
	Txns       *repo.TransactionStore
	Nots       *repo.NotificationStore
	DekForUser func(ctx context.Context, userID string) (dek []byte, err error)
	Env        LLMEnv
	LLM        func(baseURL, apiKey, model string) (llm.Categorizer, error)
	BatchSize  int     // default 25
	Threshold  float64 // default DefaultThreshold
	Log        *slog.Logger
}

const (
	defaultBatch = 25
	// defaultBaseURL is the in-network llama.cpp service name; k8s sets
	// LLM_BASE_URL explicitly.
	defaultBaseURL = "http://llama:8080/v1"
	// defaultModel is the dev compose's full --model path
	// (docker-compose.dev.yml). llama.cpp echoes the --model argument
	// verbatim: it is the id /v1/models reports and the response's
	// model field, and it is what the job's llm_model column records —
	// so clients use the same string. (The server does NOT validate the
	// request's model field, any value is accepted — verified 2026-11-05
	// — so this is a consistency choice, not an enforcement one.) The
	// k8s deployment (10.2) passes the same full path.
	defaultModel = "/models/qwen3-4b-instruct-2507-q4_k_m.gguf"
)

// defaultLLM adapts llm.New (which returns no error) to the Runner's
// factory signature.
func defaultLLM(baseURL, apiKey, model string) (llm.Categorizer, error) {
	return llm.New(baseURL, apiKey, model), nil
}

// NewRunner wires a Runner with the plan defaults applied: batch 25,
// threshold 0.75, the llm.New factory, and the env fallback normalised
// (empty BaseURL/Model -> the compose-network defaults).
func NewRunner(jobs *repo.CategoriseJobStore, creds *repo.CredentialStore,
	cats *repo.CategoryStore, txns *repo.TransactionStore, nots *repo.NotificationStore,
	dekForUser func(ctx context.Context, userID string) ([]byte, error),
	env LLMEnv) *Runner {
	return &Runner{
		Jobs:       jobs,
		Creds:      creds,
		Cats:       cats,
		Txns:       txns,
		Nots:       nots,
		DekForUser: dekForUser,
		Env:        NormalizeEnv(env),
		LLM:        defaultLLM,
		BatchSize:  defaultBatch,
		Threshold:  DefaultThreshold,
		Log:        slog.Default(),
	}
}

// failJob persists the terminal 'failed' state. It returns nil once the
// state is persisted (the job row is the source of truth) and the
// persist error otherwise (e.g. the DB went away mid-job).
func (r *Runner) failJob(ctx context.Context, jobID, model string, err error) error {
	r.Log.Error("categorise job failed", "job", jobID, "error", err)
	msg := err.Error()
	if ferr := r.Jobs.Finish(ctx, jobID, "failed", model, &msg); ferr != nil {
		return ferr
	}
	return nil
}

// RunJob drives one claimed job to its terminal state. The order is
// pinned by the 5.4 design: DEK -> provider (credential, else env) ->
// categories -> total -> client -> batch loop -> Finish. It returns
// nil once the terminal state is persisted; non-nil only when the
// Finish persist itself failed.
func (r *Runner) RunJob(ctx context.Context, jobID, userID string) error {
	log := r.Log
	model := r.Env.Model

	dek, err := r.DekForUser(ctx, userID)
	if err != nil {
		return r.failJob(ctx, jobID, model, fmt.Errorf("runner: DEK for user: %w", err))
	}

	// Provider resolution: the user's most-recently-updated credential
	// wins; without one the process env fallback is used. The requested
	// model is always the env model (credentials carry no model).
	baseURL, apiKey, source := r.Env.BaseURL, r.Env.APIKey, "env"
	rc, rerr := r.Creds.Resolve(ctx, userID, dek)
	if rerr == nil {
		baseURL, apiKey, source = rc.Endpoint, rc.APIKey, "credential"
	} else if !errors.Is(rerr, repo.ErrCredentialNotFound) {
		return r.failJob(ctx, jobID, model, fmt.Errorf("runner: resolve credential: %w", rerr))
	}
	log.Info("categorise job: provider resolved", "job", jobID, "source", source, "model", model)

	catRows, err := r.Cats.List(ctx, userID, dek)
	if err != nil {
		return r.failJob(ctx, jobID, model, fmt.Errorf("runner: list categories: %w", err))
	}
	if len(catRows) == 0 {
		return r.failJob(ctx, jobID, model, errors.New("user has no categories"))
	}
	categories := make([]Category, 0, len(catRows))
	for _, c := range catRows {
		categories = append(categories, Category{ID: c.ID, Name: c.Name})
	}

	total, err := r.Txns.CountUncategorized(ctx, userID, dek)
	if err != nil {
		return r.failJob(ctx, jobID, model, fmt.Errorf("runner: count uncategorized: %w", err))
	}
	if err := r.Jobs.SetTotal(ctx, jobID, total); err != nil {
		return r.failJob(ctx, jobID, model, fmt.Errorf("runner: set total: %w", err))
	}
	if total == 0 {
		log.Info("categorise job done (nothing to categorise)", "job", jobID)
		if err := r.Jobs.Finish(ctx, jobID, "done", model, nil); err != nil {
			return err
		}
		return nil
	}

	if r.LLM == nil {
		return r.failJob(ctx, jobID, model, errors.New("runner: no LLM factory configured"))
	}
	c, err := r.LLM(baseURL, apiKey, model)
	if err != nil {
		return r.failJob(ctx, jobID, model, fmt.Errorf("runner: build LLM client: %w", err))
	}

	// totalReview accumulates the flagged rows across batches; it gates
	// the plan 5.5 review notification at the terminal 'done' (a failed
	// job returns before the terminal Finish, so it never notifies —
	// even with batches already flagged).
	totalReview := 0
	for {
		rows, err := r.Txns.Uncategorized(ctx, userID, r.BatchSize, dek)
		if err != nil {
			return r.failJob(ctx, jobID, model, fmt.Errorf("runner: list uncategorized: %w", err))
		}
		if len(rows) == 0 {
			break
		}
		items := make([]Item, len(rows))
		for i, row := range rows {
			items[i] = Item{
				Description: row.Description,
				// canonical prompt amount: "5.75" / "-12.30", no symbol
				Amount: strconv.FormatFloat(row.Amount, 'f', 2, 64),
			}
		}
		sugs, usage, err := Run(ctx, c, categories, items)
		if err != nil {
			return r.failJob(ctx, jobID, model, fmt.Errorf("runner: categorise batch: %w", err))
		}
		// One pass over the batch: auto-apply pairs (txn + category)
		// and review txn ids, then two round trips (the per-row
		// ApplySuggestion cost 25–50 round trips for a 25-row batch).
		// The two updates touch disjoint columns (review_pending vs
		// category_id), so their order cannot change the result; the
		// review flags go first so the review queue is already
		// populated when the category probe fails the job.
		autos := make([]repo.CategoryPair, 0, len(sugs))
		reviewIDs := make([]string, 0, len(sugs))
		for i, s := range sugs {
			if s.NeedsReview(r.Threshold) {
				reviewIDs = append(reviewIDs, rows[i-1].ID)
			} else {
				autos = append(autos, repo.CategoryPair{TxnID: rows[i-1].ID, CategoryID: s.CategoryID})
			}
		}
		if _, err := r.Txns.MarkReviewPending(ctx, userID, reviewIDs); err != nil {
			return r.failJob(ctx, jobID, model, fmt.Errorf("runner: mark review pending: %w", err))
		}
		if _, err := r.Txns.ApplyCategories(ctx, userID, autos); err != nil {
			return r.failJob(ctx, jobID, model, fmt.Errorf("runner: apply categories: %w", err))
		}
		// The repo methods are strict about the affected rows, so the
		// lengths are truthful counts of applied rows.
		auto, review := len(autos), len(reviewIDs)
		applied := auto + review
		// Advance ALWAYS records the batch (tokens count even when
		// nothing applied); the stuck guard comes after.
		if err := r.Jobs.Advance(ctx, jobID, applied, auto, review,
			usage.PromptTokens, usage.CompletionTokens); err != nil {
			return r.failJob(ctx, jobID, model, fmt.Errorf("runner: advance job: %w", err))
		}
		log.Info("categorise batch", "job", jobID, "items", len(items),
			"auto_applied", auto, "to_review", review,
			"tokens_in", usage.PromptTokens, "tokens_out", usage.CompletionTokens)
		totalReview += review
		if applied == 0 {
			log.Warn("categorise batch produced no suggestions; remaining items stay uncategorised",
				"job", jobID, "items", len(items))
			break
		}
	}

	if err := r.Jobs.Finish(ctx, jobID, "done", model, nil); err != nil {
		return err
	}
	log.Info("categorise job done", "job", jobID)
	if totalReview > 0 {
		r.notifyReviewNeeded(ctx, jobID, userID, totalReview)
	}
	return nil
}

// notifyReviewNeeded records the plan 5.5 notification for a finished
// job that left rows in the review queue ("N transactions need review",
// kind "review", ref {count, job_id}). Failures are logged only: the
// job is already done, a notification write must not undo that, and
// the queue stays reachable via GET /v1/review.
func (r *Runner) notifyReviewNeeded(ctx context.Context, jobID, userID string, count int) {
	if r.Nots == nil {
		return
	}
	body := ReviewNotificationTitle(count)
	ref := ReviewNotificationRef(count, jobID)
	n, err := r.Nots.Create(ctx, userID, repo.NotificationIn{
		Kind:  KindReview,
		Title: body,
		Body:  body,
		Ref:   ref,
	})
	if err != nil {
		r.Log.Error("review notification not recorded", "job", jobID, "count", count, "error", err)
		return
	}
	r.Log.Info("review notification recorded", "job", jobID, "count", count, "notification", n.ID)
}

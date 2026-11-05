// Per-user categorise job repository (plan step 5.4, migration 0007):
// the queue behind the LLM categorisation worker. One row per
// categorisation request — source 'manual' (the POST endpoint) or
// 'import' (chained from a finished import job that inserted rows).
// status walks pending -> running -> done|failed; total/done track
// progress, auto_applied/to_review how the suggestions were applied
// (category_id set vs review_pending set), and llm_model plus the
// token pair give per-job usage (llm_cost_cents stays 0 in iteration
// 1 — see the 0007 header).
//
// The single worker claims jobs with ClaimNext (FOR UPDATE SKIP
// LOCKED) and re-queues 'running' rows left by a crash via
// ResetRunning at startup.
package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// CategoriseJob is one categorise_jobs row, with the JSON tags the API
// serializes (POST/GET /api/v1/categorise return the raw job).
type CategoriseJob struct {
	ID          string    `json:"id"`
	UserID      string    `json:"user_id"`
	Source      string    `json:"source"`
	ImportJobID *string   `json:"import_job_id"`
	Status      string    `json:"status"`
	Total       int       `json:"total"`
	Done        int       `json:"done"`
	AutoApplied int       `json:"auto_applied"`
	ToReview    int       `json:"to_review"`
	LLMModel    string    `json:"llm_model"`
	TokensIn    int       `json:"llm_tokens_in"`
	TokensOut   int       `json:"llm_tokens_out"`
	CostCents   int       `json:"llm_cost_cents"`
	Error       *string   `json:"error"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// CategoriseJobStore persists categorise jobs. One store serves every
// user; the user-scoped methods take a userID and scope their SQL on
// it. ClaimNext/Advance/Finish/ResetRunning are worker-side and
// unscoped by design (the worker owns the queue).
type CategoriseJobStore struct {
	pool *pgxpool.Pool
}

// NewCategoriseJobStore returns a CategoriseJobStore backed by pool
// (nil for a nil pool, mirroring the other repo constructors).
func NewCategoriseJobStore(pool *pgxpool.Pool) *CategoriseJobStore {
	if pool == nil {
		return nil
	}
	return &CategoriseJobStore{pool: pool}
}

// categoriseJobFrom assembles a row from the raw scan columns (the
// nullable columns are the sql.NullString ones).
func categoriseJobFrom(id, userID, source, status string, importJobID sql.NullString,
	total, done, autoApplied, toReview int, llmModel string,
	tokensIn, tokensOut, costCents int, errCol sql.NullString,
	created, updated time.Time) CategoriseJob {
	j := CategoriseJob{
		ID:          id,
		UserID:      userID,
		Source:      source,
		Status:      status,
		Total:       total,
		Done:        done,
		AutoApplied: autoApplied,
		ToReview:    toReview,
		LLMModel:    llmModel,
		TokensIn:    tokensIn,
		TokensOut:   tokensOut,
		CostCents:   costCents,
		CreatedAt:   created,
		UpdatedAt:   updated,
	}
	if importJobID.Valid {
		v := importJobID.String
		j.ImportJobID = &v
	}
	if errCol.Valid {
		v := errCol.String
		j.Error = &v
	}
	return j
}

// categoriseJobSelect is the canonical column list (llm_model
// COALESCEd so the scan target stays a plain string).
const categoriseJobSelect = `
	SELECT id, user_id, source, import_job_id, status,
	       total, done, auto_applied, to_review,
	       COALESCE(llm_model, ''), llm_tokens_in, llm_tokens_out, llm_cost_cents,
	       error, created_at, updated_at
	FROM categorise_jobs`

// Create enqueues a pending job. source must be 'manual' or 'import';
// 'import' requires importJobID (the 0007 check constraint), validated
// here so a bad value is a plain error, not a 23514 surfacing as a 500
// (same rule as the import_jobs status vocabulary).
func (s *CategoriseJobStore) Create(ctx context.Context, userID, source string, importJobID *string) (CategoriseJob, error) {
	if source != "manual" && source != "import" {
		return CategoriseJob{}, errors.New("repo: categorise source must be 'manual' or 'import'")
	}
	if source == "import" && importJobID == nil {
		return CategoriseJob{}, errors.New("repo: import-sourced categorise job requires an import job id")
	}
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO categorise_jobs (user_id, source, import_job_id, status)
		VALUES ($1, $2, $3, 'pending')
		RETURNING id`, userID, source, importJobID).Scan(&id)
	if err != nil {
		return CategoriseJob{}, fmt.Errorf("repo: create categorise job: %w", err)
	}
	return s.Get(ctx, userID, id)
}

// ClaimNext atomically claims the oldest pending job: the UPDATE runs
// against a FOR UPDATE SKIP LOCKED subselect, so a concurrent worker
// (or loop iteration) skips the claimed row instead of double-running
// it. ErrNoPendingCategoriseJob on an empty queue.
func (s *CategoriseJobStore) ClaimNext(ctx context.Context) (CategoriseJob, error) {
	var (
		id, userID, source, status string
		importJobID                sql.NullString
		total, done, autoApplied   int
		toReview                   int
		llmModel                   string
		tokensIn, tokensOut        int
		costCents                  int
		errCol                     sql.NullString
		created, updated           time.Time
	)
	err := s.pool.QueryRow(ctx, `
		UPDATE categorise_jobs
		SET status = 'running', updated_at = now()
		WHERE id = (
			SELECT id FROM categorise_jobs
			WHERE status = 'pending'
			ORDER BY created_at
			LIMIT 1 FOR UPDATE SKIP LOCKED
		)
		RETURNING id, user_id, source, import_job_id, status,
			total, done, auto_applied, to_review,
			COALESCE(llm_model, ''), llm_tokens_in, llm_tokens_out, llm_cost_cents,
			error, created_at, updated_at`).
		Scan(&id, &userID, &source, &importJobID, &status,
			&total, &done, &autoApplied, &toReview, &llmModel,
			&tokensIn, &tokensOut, &costCents, &errCol, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return CategoriseJob{}, ErrNoPendingCategoriseJob
	}
	if err != nil {
		return CategoriseJob{}, fmt.Errorf("repo: claim categorise job: %w", err)
	}
	return categoriseJobFrom(id, userID, source, status, importJobID,
		total, done, autoApplied, toReview, llmModel,
		tokensIn, tokensOut, costCents, errCol, created, updated), nil
}

// Advance accumulates one batch's deltas on the job (all deltas
// non-negative; the worker only ever adds).
func (s *CategoriseJobStore) Advance(ctx context.Context, id string, dDone, dAuto, dReview, dTokIn, dTokOut int) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE categorise_jobs
		SET done = done + $2,
		    auto_applied = auto_applied + $3,
		    to_review = to_review + $4,
		    llm_tokens_in = llm_tokens_in + $5,
		    llm_tokens_out = llm_tokens_out + $6,
		    updated_at = now()
		WHERE id = $1`, id, dDone, dAuto, dReview, dTokIn, dTokOut)
	if err != nil {
		return fmt.Errorf("repo: advance categorise job: %w", err)
	}
	return nil
}

// Finish moves the job to its terminal state ('done' | 'failed').
// model is the requested model string; "" = unknown (an early failure
// before the provider was resolved — the prior value is kept). errMsg
// is set on 'failed' jobs.
func (s *CategoriseJobStore) Finish(ctx context.Context, id, status, model string, errMsg *string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE categorise_jobs
		SET status = $2,
		    llm_model = CASE WHEN $3 = '' THEN llm_model ELSE $3 END,
		    error = $4,
		    updated_at = now()
		WHERE id = $1`, id, status, model, errMsg)
	if err != nil {
		return fmt.Errorf("repo: finish categorise job: %w", err)
	}
	return nil
}

// SetTotal records the uncategorized count (called once after the
// first COUNT, before the batch loop).
func (s *CategoriseJobStore) SetTotal(ctx context.Context, id string, total int) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE categorise_jobs
		SET total = $2, updated_at = now()
		WHERE id = $1`, id, total)
	if err != nil {
		return fmt.Errorf("repo: set categorise job total: %w", err)
	}
	return nil
}

// Get returns one of the user's jobs, or ErrCategoriseJobNotFound when
// the id is not the user's (existence is not disclosed across users).
func (s *CategoriseJobStore) Get(ctx context.Context, userID, id string) (CategoriseJob, error) {
	var (
		idCol, userIDCol, source, status string
		importJobID                      sql.NullString
		total, done, autoApplied         int
		toReview                         int
		llmModel                         string
		tokensIn, tokensOut              int
		costCents                        int
		errCol                           sql.NullString
		created, updated                 time.Time
	)
	err := s.pool.QueryRow(ctx, categoriseJobSelect+`
		WHERE user_id = $1 AND id = $2`, userID, id).
		Scan(&idCol, &userIDCol, &source, &importJobID, &status,
			&total, &done, &autoApplied, &toReview, &llmModel,
			&tokensIn, &tokensOut, &costCents, &errCol, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return CategoriseJob{}, ErrCategoriseJobNotFound
	}
	if err != nil {
		return CategoriseJob{}, fmt.Errorf("repo: get categorise job: %w", err)
	}
	return categoriseJobFrom(idCol, userIDCol, source, status, importJobID,
		total, done, autoApplied, toReview, llmModel,
		tokensIn, tokensOut, costCents, errCol, created, updated), nil
}

// ActiveExists reports whether the user has a pending or running job —
// the POST endpoint's 409 guard.
func (s *CategoriseJobStore) ActiveExists(ctx context.Context, userID string) (bool, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM categorise_jobs
		WHERE user_id = $1 AND status IN ('pending', 'running')`, userID).
		Scan(&n); err != nil {
		return false, fmt.Errorf("repo: check active categorise job: %w", err)
	}
	return n > 0, nil
}

// ResetRunning re-queues 'running' jobs at startup (single-node worker
// crash recovery; the plan's 9.2 multi-node lease supersedes it).
// Returns the number of rows re-queued.
func (s *CategoriseJobStore) ResetRunning(ctx context.Context) (int, error) {
	cmdTag, err := s.pool.Exec(ctx, `
		UPDATE categorise_jobs
		SET status = 'pending', updated_at = now()
		WHERE status = 'running'`)
	if err != nil {
		return 0, fmt.Errorf("repo: reset running categorise jobs: %w", err)
	}
	return int(cmdTag.RowsAffected()), nil
}

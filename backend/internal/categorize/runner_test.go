// Runner tests (plan 5.4): a deterministic fake llm.Categorizer (or the
// real llm.New against an httptest server for provider resolution) + a
// testcontainers postgres:18 with all migrations, following the repo
// package's test-env pattern (that one is unexported, so this is a
// local copy).
package categorize

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"finance-tracker/internal/crypto"
	"finance-tracker/internal/llm"
	"finance-tracker/internal/repo"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// runnerUser is one seeded users row plus its unwrapped DEK.
type runnerUser struct {
	id  string
	dek []byte
}

type runnerEnv struct {
	pool  *pgxpool.Pool
	ctx   context.Context
	users map[string]runnerUser // keyed by sub
}

// newRunnerEnv starts one migrated postgres:18 container and seeds one
// users row per sub (fresh DEK wrapped under a test KEK — the same row
// the 1.3c bootstrap path writes). Copy of the repo package's
// newTestEnv/migratedPool/pingUntilReady (D-019: no shared testutil).
func newRunnerEnv(t *testing.T, subs ...string) *runnerEnv {
	t.Helper()
	ctx := context.Background()
	ctr, err := postgres.Run(ctx, "postgres:18", postgres.BasicWaitStrategies())
	if err != nil {
		t.Fatalf("testcontainers postgres: %v", err)
	}
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = ctr.Stop(sctx, nil)
	})
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	deadline := time.Now().Add(60 * time.Second)
	for {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		perr := db.PingContext(pctx)
		cancel()
		if perr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("postgres readiness: %v", perr)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("goose dialect: %v", err)
	}
	if err := goose.UpContext(ctx, db, "../../migrations"); err != nil {
		t.Fatalf("goose up: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgx pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("pool ping: %v", err)
	}
	kek := make([]byte, crypto.KEKSize)
	if _, err := rand.Read(kek); err != nil {
		t.Fatalf("KEK random: %v", err)
	}
	env := &runnerEnv{pool: pool, ctx: ctx, users: map[string]runnerUser{}}
	for _, sub := range subs {
		dek, err := crypto.NewDEK()
		if err != nil {
			t.Fatalf("generate DEK: %v", err)
		}
		wrapped, err := crypto.WrapDEK(kek, dek)
		if err != nil {
			t.Fatalf("wrap DEK: %v", err)
		}
		var id string
		err = pool.QueryRow(ctx, `
			INSERT INTO users (auth0_sub, email, display_name, wrapped_dek)
			VALUES ($1, $2, $3, $4)
			RETURNING id`, sub, sub+"@localhost", sub, wrapped).Scan(&id)
		if err != nil {
			t.Fatalf("seed user %s: %v", sub, err)
		}
		env.users[sub] = runnerUser{id: id, dek: dek}
	}
	return env
}

// fakeCategorizer is a deterministic llm.Categorizer for runner tests.
// By default every call answers with a valid suggestions array sized to
// the request's item count: odd items get confidence 0.9 (auto), even
// items 0.5 (review). garbage, when set, is returned verbatim on every
// call; errAfter > 0 makes calls N and later fail with err.
type fakeCategorizer struct {
	mu       sync.Mutex
	garbage  json.RawMessage
	errAfter int
	err      error
	in, out  int
	reqs     []llm.Request
}

func (f *fakeCategorizer) Categorize(_ context.Context, req llm.Request) (*llm.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	call := len(f.reqs)
	if f.garbage != nil {
		return &llm.Response{JSON: f.garbage, PromptTokens: f.in, CompletionTokens: f.out}, nil
	}
	if f.errAfter > 0 && call >= f.errAfter {
		return nil, f.err
	}
	// One item per line after the "Transactions:" header line. The
	// prompt ends in a trailing newline, so drop it before counting.
	n := len(strings.Split(strings.TrimSuffix(req.User, "\n"), "\n")) - 1
	entries := make([]map[string]any, 0, n)
	for i := 1; i <= n; i++ {
		conf := 0.9
		if i%2 == 0 {
			conf = 0.5
		}
		entries = append(entries, map[string]any{
			"i": i, "category": "Groceries", "confidence": conf,
		})
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		return nil, fmt.Errorf("fake: marshal: %w", err)
	}
	return &llm.Response{JSON: raw, PromptTokens: f.in, CompletionTokens: f.out}, nil
}

func (f *fakeCategorizer) batchSizes() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]int, len(f.reqs))
	for i, req := range f.reqs {
		out[i] = len(strings.Split(strings.TrimSuffix(req.User, "\n"), "\n")) - 1
	}
	return out
}

// seedData gives sub one account, the named categories, and n
// uncategorised transactions.
func seedData(t *testing.T, env *runnerEnv, sub string, cats []string, n int) string {
	t.Helper()
	u := env.users[sub]
	acct := repo.NewAccountStore(env.pool)
	catStore := repo.NewCategoryStore(env.pool)
	tx := repo.NewTransactionStore(env.pool)
	acctRow, err := acct.Create(env.ctx, u.id,
		repo.AccountIn{Name: "Runner Bank", Type: "bank", Currency: "USD"}, u.dek)
	if err != nil {
		t.Fatalf("create account for %s: %v", sub, err)
	}
	for _, name := range cats {
		if _, err := catStore.Create(env.ctx, u.id, repo.CategoryIn{Name: name}, u.dek); err != nil {
			t.Fatalf("create category %q for %s: %v", name, sub, err)
		}
	}
	for i := 0; i < n; i++ {
		if _, _, err := tx.Upsert(env.ctx, u.id, repo.TransactionIn{
			AccountID:   acctRow.ID,
			DatePosted:  time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i),
			Amount:      fmt.Sprintf("-%d.50", i),
			Description: fmt.Sprintf("Runner Txn %d", i),
		}, u.dek); err != nil {
			t.Fatalf("seed txn %d for %s: %v", i, sub, err)
		}
	}
	return u.id
}

// buildRunner wires a Runner over env with the given LLM factory.
func buildRunner(t *testing.T, env *runnerEnv, factory func(baseURL, apiKey, model string) (llm.Categorizer, error)) *Runner {
	t.Helper()
	byID := map[string]runnerUser{}
	for _, u := range env.users {
		byID[u.id] = u
	}
	return &Runner{
		Jobs:  repo.NewCategoriseJobStore(env.pool),
		Creds: repo.NewCredentialStore(env.pool),
		Cats:  repo.NewCategoryStore(env.pool),
		Txns:  repo.NewTransactionStore(env.pool),
		DekForUser: func(_ context.Context, userID string) ([]byte, error) {
			u, ok := byID[userID]
			if !ok {
				return nil, fmt.Errorf("no user %s", userID)
			}
			return u.dek, nil
		},
		Env:       LLMEnv{BaseURL: "http://unused:8080/v1", Model: "test-model"},
		LLM:       factory,
		BatchSize: defaultBatch,
		Threshold: DefaultThreshold,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestRunJobBatches(t *testing.T) {
	env := newRunnerEnv(t, "batches")
	uid := seedData(t, env, "batches", []string{"Groceries"}, 60)
	fake := &fakeCategorizer{in: 100, out: 50}
	r := buildRunner(t, env, func(_, _, _ string) (llm.Categorizer, error) {
		return fake, nil
	})
	jobs := r.Jobs
	ctx := env.ctx

	job, err := jobs.Create(ctx, uid, "manual", nil)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := r.RunJob(ctx, job.ID, uid); err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	got, err := jobs.Get(ctx, uid, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.Status != "done" || got.Total != 60 || got.Done != 60 ||
		got.AutoApplied != 31 || got.ToReview != 29 ||
		got.TokensIn != 300 || got.TokensOut != 150 || got.LLMModel != "test-model" {
		t.Fatalf("job: %+v", got)
	}
	if sizes := fake.batchSizes(); len(sizes) != 3 || sizes[0] != 25 ||
		sizes[1] != 25 || sizes[2] != 10 {
		t.Fatalf("batch sizes = %v, want [25 25 10]", sizes)
	}

	var auto, review, uncat int
	if err := env.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM transactions WHERE user_id = $1 AND category_id IS NOT NULL`, uid).Scan(&auto); err != nil {
		t.Fatalf("count auto: %v", err)
	}
	if err := env.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM transactions WHERE user_id = $1 AND review_pending`, uid).Scan(&review); err != nil {
		t.Fatalf("count review: %v", err)
	}
	uncat, err = r.Txns.CountUncategorized(ctx, uid, env.users["batches"].dek)
	if err != nil {
		t.Fatalf("count uncategorized: %v", err)
	}
	if auto != 31 || review != 29 || uncat != 0 {
		t.Fatalf("db state: auto=%d review=%d uncategorized=%d", auto, review, uncat)
	}
}

// TestRunJobProviderResolution pins the two provider sources: a stored
// credential beats the env fallback (keyless self_hosted -> NO
// Authorization header), and the env fallback sends its key + model.
func TestRunJobProviderResolution(t *testing.T) {
	type capture struct {
		mu    sync.Mutex
		path  string
		auth  string
		model string
	}
	chatServer := func(c *capture) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c.mu.Lock()
			c.path = r.URL.Path
			c.auth = r.Header.Get("Authorization")
			var body struct {
				Model string `json:"model"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			c.model = body.Model
			c.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"model":"fake","choices":[{"message":{"content":"[{\"i\": 1, \"category\": \"Groceries\", \"confidence\": 0.9}]"}}],"usage":{"prompt_tokens":7,"completion_tokens":11}}`)
		}))
	}

	t.Run("stored keyless credential wins", func(t *testing.T) {
		env := newRunnerEnv(t, "cred")
		uid := seedData(t, env, "cred", []string{"Groceries"}, 1)
		cap1 := &capture{}
		ts := chatServer(cap1)
		defer ts.Close()
		r := buildRunner(t, env, defaultLLM)
		r.Env.Model = "env-model-ignored"
		if _, err := repo.NewCredentialStore(env.pool).Put(env.ctx, uid, repo.CredentialIn{
			Provider: "self_hosted", Endpoint: ts.URL,
		}, env.users["cred"].dek); err != nil {
			t.Fatalf("put credential: %v", err)
		}
		job, err := r.Jobs.Create(env.ctx, uid, "manual", nil)
		if err != nil {
			t.Fatalf("create job: %v", err)
		}
		if err := r.RunJob(env.ctx, job.ID, uid); err != nil {
			t.Fatalf("RunJob: %v", err)
		}
		got, err := r.Jobs.Get(env.ctx, uid, job.ID)
		if err != nil {
			t.Fatalf("get job: %v", err)
		}
		if got.Status != "done" || got.AutoApplied != 1 || got.LLMModel != "env-model-ignored" {
			t.Fatalf("job: %+v", got)
		}
		if cap1.path != "/chat/completions" {
			t.Errorf("path = %q, want /chat/completions", cap1.path)
		}
		if cap1.auth != "" {
			t.Errorf("keyless credential: Authorization = %q, want none", cap1.auth)
		}
	})

	t.Run("env fallback sends key and model", func(t *testing.T) {
		env := newRunnerEnv(t, "envuser")
		uid := seedData(t, env, "envuser", []string{"Groceries"}, 1)
		cap2 := &capture{}
		ts := chatServer(cap2)
		defer ts.Close()
		r := buildRunner(t, env, defaultLLM)
		r.Env = LLMEnv{BaseURL: ts.URL, APIKey: "secret-key", Model: "env-model"}
		job, err := r.Jobs.Create(env.ctx, uid, "manual", nil)
		if err != nil {
			t.Fatalf("create job: %v", err)
		}
		if err := r.RunJob(env.ctx, job.ID, uid); err != nil {
			t.Fatalf("RunJob: %v", err)
		}
		got, err := r.Jobs.Get(env.ctx, uid, job.ID)
		if err != nil {
			t.Fatalf("get job: %v", err)
		}
		if got.Status != "done" || got.LLMModel != "env-model" {
			t.Fatalf("job: %+v", got)
		}
		if cap2.path != "/chat/completions" {
			t.Errorf("path = %q, want /chat/completions", cap2.path)
		}
		if cap2.auth != "Bearer secret-key" {
			t.Errorf("Authorization = %q, want Bearer secret-key", cap2.auth)
		}
		if cap2.model != "env-model" {
			t.Errorf("request model = %q, want env-model", cap2.model)
		}
	})
}

// TestRunJobMidJobError pins the failure path: the second batch's LLM
// call fails; the job is 'failed' with the error recorded, the applied
// batch stays applied, and the remainder stays uncategorised. RunJob
// itself returns nil (the terminal state is persisted).
func TestRunJobMidJobError(t *testing.T) {
	env := newRunnerEnv(t, "miderr")
	uid := seedData(t, env, "miderr", []string{"Groceries"}, 30)
	fake := &fakeCategorizer{in: 100, out: 50, errAfter: 2, err: errors.New("llm boom")}
	r := buildRunner(t, env, func(_, _, _ string) (llm.Categorizer, error) {
		return fake, nil
	})
	job, err := r.Jobs.Create(env.ctx, uid, "manual", nil)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := r.RunJob(env.ctx, job.ID, uid); err != nil {
		t.Fatalf("RunJob (terminal state persisted, want nil): %v", err)
	}
	got, err := r.Jobs.Get(env.ctx, uid, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.Status != "failed" || got.Total != 30 || got.Done != 25 ||
		got.AutoApplied != 13 || got.ToReview != 12 ||
		got.TokensIn != 100 || got.TokensOut != 50 ||
		got.Error == nil || !strings.Contains(*got.Error, "llm boom") {
		t.Fatalf("job: %+v", got)
	}
	uncat, err := r.Txns.CountUncategorized(env.ctx, uid, env.users["miderr"].dek)
	if err != nil {
		t.Fatalf("count uncategorized: %v", err)
	}
	if uncat != 5 {
		t.Fatalf("uncategorised = %d, want 5", uncat)
	}
}

// TestRunJobStuckGuard pins the stuck guard: a batch whose suggestions
// match no row (the model answered with an out-of-range index and an
// unknown category) applies nothing, the loop stops after the single
// call, the job finishes 'done' with done=0, and the items stay
// uncategorised.
func TestRunJobStuckGuard(t *testing.T) {
	env := newRunnerEnv(t, "stuck")
	uid := seedData(t, env, "stuck", []string{"Groceries"}, 3)
	fake := &fakeCategorizer{
		in: 100, out: 50,
		garbage: json.RawMessage(`[{"i": 99, "category": "Nope", "confidence": 0.9}]`),
	}
	r := buildRunner(t, env, func(_, _, _ string) (llm.Categorizer, error) {
		return fake, nil
	})
	job, err := r.Jobs.Create(env.ctx, uid, "manual", nil)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := r.RunJob(env.ctx, job.ID, uid); err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	got, err := r.Jobs.Get(env.ctx, uid, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.Status != "done" || got.Total != 3 || got.Done != 0 ||
		got.AutoApplied != 0 || got.ToReview != 0 ||
		got.TokensIn != 100 || got.TokensOut != 50 {
		t.Fatalf("job: %+v", got)
	}
	if n := len(fake.batchSizes()); n != 1 {
		t.Fatalf("llm calls = %d, want 1", n)
	}
	uncat, err := r.Txns.CountUncategorized(env.ctx, uid, env.users["stuck"].dek)
	if err != nil {
		t.Fatalf("count uncategorized: %v", err)
	}
	if uncat != 3 {
		t.Fatalf("uncategorised = %d, want 3", uncat)
	}
}

// TestRunJobReviewNotification pins the plan 5.5 notification: a
// finished job with a non-empty review queue records exactly one
// "N transactions need review" notification (kind "review", ref
// {count, job_id}); a finished job with an empty queue records none,
// and a failed job records none even when earlier batches already
// flagged rows.
func TestRunJobReviewNotification(t *testing.T) {
	notifications := func(t *testing.T, env *runnerEnv, uid string) []repo.Notification {
		t.Helper()
		nots := repo.NewNotificationStore(env.pool)
		got, err := nots.List(env.ctx, uid, repo.NotificationFilter{})
		if err != nil {
			t.Fatalf("list notifications: %v", err)
		}
		return got
	}

	t.Run("finished job with review rows notifies", func(t *testing.T) {
		env := newRunnerEnv(t, "notif")
		uid := seedData(t, env, "notif", []string{"Groceries"}, 2)
		fake := &fakeCategorizer{in: 10, out: 5}
		r := buildRunner(t, env, func(_, _, _ string) (llm.Categorizer, error) {
			return fake, nil
		})
		r.Nots = repo.NewNotificationStore(env.pool)
		job, err := r.Jobs.Create(env.ctx, uid, "manual", nil)
		if err != nil {
			t.Fatalf("create job: %v", err)
		}
		if err := r.RunJob(env.ctx, job.ID, uid); err != nil {
			t.Fatalf("RunJob: %v", err)
		}
		got, err := r.Jobs.Get(env.ctx, uid, job.ID)
		if err != nil {
			t.Fatalf("get job: %v", err)
		}
		// n=2: item 1 auto (0.9), item 2 review (0.5).
		if got.Status != "done" || got.ToReview != 1 || got.AutoApplied != 1 {
			t.Fatalf("job: %+v", got)
		}
		nots := notifications(t, env, uid)
		if len(nots) != 1 {
			t.Fatalf("notifications = %d, want 1: %+v", len(nots), nots)
		}
		n := nots[0]
		if n.Kind != "review" {
			t.Errorf("kind = %q, want review", n.Kind)
		}
		if n.Title != "1 transactions need review" || n.Body != "1 transactions need review" {
			t.Errorf("title/body = %q / %q", n.Title, n.Body)
		}
		if n.ReadAt != nil {
			t.Errorf("notification recorded read: %+v", n)
		}
		var ref struct {
			Count int    `json:"count"`
			JobID string `json:"job_id"`
		}
		if err := json.Unmarshal(n.Ref, &ref); err != nil {
			t.Fatalf("ref %q: %v", n.Ref, err)
		}
		if ref.Count != 1 || ref.JobID != job.ID {
			t.Errorf("ref = count %d job %s, want count 1 job %s", ref.Count, ref.JobID, job.ID)
		}
	})

	t.Run("finished job with empty queue does not notify", func(t *testing.T) {
		env := newRunnerEnv(t, "noqueue")
		uid := seedData(t, env, "noqueue", []string{"Groceries"}, 1)
		fake := &fakeCategorizer{in: 10, out: 5}
		r := buildRunner(t, env, func(_, _, _ string) (llm.Categorizer, error) {
			return fake, nil
		})
		r.Nots = repo.NewNotificationStore(env.pool)
		job, err := r.Jobs.Create(env.ctx, uid, "manual", nil)
		if err != nil {
			t.Fatalf("create job: %v", err)
		}
		if err := r.RunJob(env.ctx, job.ID, uid); err != nil {
			t.Fatalf("RunJob: %v", err)
		}
		got, err := r.Jobs.Get(env.ctx, uid, job.ID)
		if err != nil {
			t.Fatalf("get job: %v", err)
		}
		// n=1: the single item is auto (0.9); the queue stays empty.
		if got.Status != "done" || got.ToReview != 0 || got.AutoApplied != 1 {
			t.Fatalf("job: %+v", got)
		}
		if nots := notifications(t, env, uid); len(nots) != 0 {
			t.Fatalf("notifications = %d, want 0: %+v", len(nots), nots)
		}
	})

	t.Run("failed job does not notify", func(t *testing.T) {
		env := newRunnerEnv(t, "failnotify")
		uid := seedData(t, env, "failnotify", []string{"Groceries"}, 30)
		fake := &fakeCategorizer{in: 10, out: 5, errAfter: 2, err: errors.New("llm boom")}
		r := buildRunner(t, env, func(_, _, _ string) (llm.Categorizer, error) {
			return fake, nil
		})
		r.Nots = repo.NewNotificationStore(env.pool)
		job, err := r.Jobs.Create(env.ctx, uid, "manual", nil)
		if err != nil {
			t.Fatalf("create job: %v", err)
		}
		if err := r.RunJob(env.ctx, job.ID, uid); err != nil {
			t.Fatalf("RunJob (terminal state persisted, want nil): %v", err)
		}
		got, err := r.Jobs.Get(env.ctx, uid, job.ID)
		if err != nil {
			t.Fatalf("get job: %v", err)
		}
		// The first batch flagged 12 rows for review before the second
		// batch's LLM call failed: cumulative review is 12, the job is
		// 'failed', and the no-notify-on-failed rule still holds.
		if got.Status != "failed" || got.ToReview != 12 {
			t.Fatalf("job: %+v", got)
		}
		if nots := notifications(t, env, uid); len(nots) != 0 {
			t.Fatalf("notifications = %d, want 0: %+v", len(nots), nots)
		}
	})
}

// TestRunJobNoCategories pins the fail-fast: a user with zero
// categories gets a 'failed' job ("user has no categories") before any
// LLM call and before the total is written.
func TestRunJobNoCategories(t *testing.T) {
	env := newRunnerEnv(t, "nocats")
	fake := &fakeCategorizer{}
	r := buildRunner(t, env, func(_, _, _ string) (llm.Categorizer, error) {
		return fake, nil
	})
	uid := env.users["nocats"].id
	job, err := r.Jobs.Create(env.ctx, uid, "manual", nil)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := r.RunJob(env.ctx, job.ID, uid); err != nil {
		t.Fatalf("RunJob (terminal state persisted, want nil): %v", err)
	}
	got, err := r.Jobs.Get(env.ctx, uid, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.Status != "failed" || got.Total != 0 || got.Done != 0 ||
		got.Error == nil || *got.Error != "user has no categories" {
		t.Fatalf("job: %+v", got)
	}
	if n := len(fake.batchSizes()); n != 0 {
		t.Fatalf("llm calls = %d, want 0", n)
	}
}

// Categorise job store tests (plan step 5.4, migration 0007): one
// shared postgres:18 testcontainer (newTestEnv) with subtests covering
// the create -> claim -> advance -> finish lifecycle, empty-queue
// claims, oldest-first claim ordering (staggered created_at), user
// scoping on Get, the ActiveExists 409 guard, and ResetRunning crash
// recovery. TestCategoriseJobCreateSource covers the source vocabulary
// and the import-source FK (a seeded account + import job row).
package repo

import (
	"errors"
	"testing"

	"finance-tracker/internal/crypto"
)

func TestCategoriseJobStore(t *testing.T) {
	env := newTestEnv(t, "catj-a", "catj-b")
	store := NewCategoriseJobStore(env.pool)
	if store == nil {
		t.Fatal("non-nil pool must give a store")
	}
	if NewCategoriseJobStore(nil) != nil {
		t.Fatal("nil pool must give nil store")
	}
	a := env.user(t, "catj-a")
	b := env.user(t, "catj-b")
	ctx := env.ctx

	t.Run("create + claim + advance + finish", func(t *testing.T) {
		job, err := store.Create(ctx, a.row.ID, "manual", nil)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if job.Status != "pending" || job.Source != "manual" || job.UserID != a.row.ID {
			t.Fatalf("created job: %+v", job)
		}
		if job.Total != 0 || job.Done != 0 || job.AutoApplied != 0 || job.ToReview != 0 ||
			job.TokensIn != 0 || job.TokensOut != 0 || job.CostCents != 0 {
			t.Fatalf("created job must be zeroed: %+v", job)
		}
		if job.ImportJobID != nil || job.LLMModel != "" || job.Error != nil {
			t.Fatalf("manual job: ImportJobID/LLMModel/Error must be unset: %+v", job)
		}

		claimed, err := store.ClaimNext(ctx)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if claimed.ID != job.ID || claimed.Status != "running" || claimed.UserID != a.row.ID {
			t.Fatalf("claimed: %+v", claimed)
		}
		if !claimed.UpdatedAt.After(job.UpdatedAt) {
			t.Fatalf("claim must bump updated_at: %v -> %v", job.UpdatedAt, claimed.UpdatedAt)
		}

		if err := store.Advance(ctx, job.ID, 3, 2, 1, 100, 50); err != nil {
			t.Fatalf("advance: %v", err)
		}
		if err := store.Advance(ctx, job.ID, 2, 0, 2, 30, 10); err != nil {
			t.Fatalf("advance 2: %v", err)
		}
		got, err := store.Get(ctx, a.row.ID, job.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Done != 5 || got.AutoApplied != 2 || got.ToReview != 3 ||
			got.TokensIn != 130 || got.TokensOut != 60 {
			t.Fatalf("advanced counts: %+v", got)
		}

		model := "/models/qwen3-4b-instruct-2507-q4_k_m.gguf"
		if err := store.Finish(ctx, job.ID, "done", model, nil); err != nil {
			t.Fatalf("finish: %v", err)
		}
		got, err = store.Get(ctx, a.row.ID, job.ID)
		if err != nil {
			t.Fatalf("get after finish: %v", err)
		}
		if got.Status != "done" || got.LLMModel != model || got.Error != nil {
			t.Fatalf("finished job: %+v", got)
		}

		// A failure before the model is known: an empty model keeps the
		// prior value, the error message is stored.
		f, err := store.Create(ctx, a.row.ID, "manual", nil)
		if err != nil {
			t.Fatalf("create failed job: %v", err)
		}
		msg := "user has no categories"
		if err := store.Finish(ctx, f.ID, "failed", "", &msg); err != nil {
			t.Fatalf("finish failed: %v", err)
		}
		gf, err := store.Get(ctx, a.row.ID, f.ID)
		if err != nil {
			t.Fatalf("get failed job: %v", err)
		}
		if gf.Status != "failed" || gf.Error == nil || *gf.Error != msg || gf.LLMModel != "" {
			t.Fatalf("failed job: %+v", gf)
		}
	})

	t.Run("claim empty queue", func(t *testing.T) {
		// Every job from the previous subtest is terminal.
		if _, err := store.ClaimNext(ctx); !errors.Is(err, ErrNoPendingCategoriseJob) {
			t.Fatalf("want ErrNoPendingCategoriseJob, got %v", err)
		}
	})

	t.Run("claim next ordering, oldest first", func(t *testing.T) {
		mk := func() string {
			j, err := store.Create(ctx, a.row.ID, "manual", nil)
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			return j.ID
		}
		first, second, third := mk(), mk(), mk()
		// Stagger created_at so the claim order is NOT the insertion
		// order: third is oldest, first middle, second newest.
		for id, secs := range map[string]int{third: 10, first: 5} {
			if _, err := store.pool.Exec(ctx, `
				UPDATE categorise_jobs
				SET created_at = now() - interval '1 second' * $2
				WHERE id = $1`, id, secs); err != nil {
				t.Fatalf("stagger %s: %v", id, err)
			}
		}
		want := []string{third, first, second}
		for i, wantID := range want {
			claimed, err := store.ClaimNext(ctx)
			if err != nil {
				t.Fatalf("claim %d: %v", i, err)
			}
			if claimed.ID != wantID {
				t.Fatalf("claim %d: want oldest-first %s, got %s", i, wantID, claimed.ID)
			}
			if claimed.Status != "running" {
				t.Fatalf("claim %d: want running, got %s", i, claimed.Status)
			}
			if err := store.Finish(ctx, claimed.ID, "done", "m", nil); err != nil {
				t.Fatalf("finish %d: %v", i, err)
			}
		}
	})

	t.Run("get user scoping", func(t *testing.T) {
		ja, err := store.Create(ctx, a.row.ID, "manual", nil)
		if err != nil {
			t.Fatalf("create a: %v", err)
		}
		jb, err := store.Create(ctx, b.row.ID, "manual", nil)
		if err != nil {
			t.Fatalf("create b: %v", err)
		}
		// Cross-user reads look identical to a missing id: existence
		// is not disclosed across users.
		if _, err := store.Get(ctx, b.row.ID, ja.ID); !errors.Is(err, ErrCategoriseJobNotFound) {
			t.Fatalf("b reads a's job: want ErrCategoriseJobNotFound, got %v", err)
		}
		if _, err := store.Get(ctx, a.row.ID, jb.ID); !errors.Is(err, ErrCategoriseJobNotFound) {
			t.Fatalf("a reads b's job: want ErrCategoriseJobNotFound, got %v", err)
		}
		none := "00000000-0000-0000-0000-000000000000"
		if _, err := store.Get(ctx, a.row.ID, none); !errors.Is(err, ErrCategoriseJobNotFound) {
			t.Fatalf("missing id: want ErrCategoriseJobNotFound, got %v", err)
		}
		// Leave nothing pending for the later subtests.
		if err := store.Finish(ctx, ja.ID, "done", "m", nil); err != nil {
			t.Fatalf("cleanup a: %v", err)
		}
		if err := store.Finish(ctx, jb.ID, "done", "m", nil); err != nil {
			t.Fatalf("cleanup b: %v", err)
		}
	})

	t.Run("active exists", func(t *testing.T) {
		p1, err := store.Create(ctx, a.row.ID, "manual", nil)
		if err != nil {
			t.Fatalf("create p1: %v", err)
		}
		if _, err := store.Create(ctx, a.row.ID, "manual", nil); err != nil {
			t.Fatalf("create p2: %v", err)
		}
		if active, err := store.ActiveExists(ctx, a.row.ID); err != nil || !active {
			t.Fatalf("two pending: want true, got %v (err %v)", active, err)
		}
		claimed, err := store.ClaimNext(ctx)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if claimed.ID != p1.ID {
			t.Fatalf("claimed %s, want oldest %s", claimed.ID, p1.ID)
		}
		if active, err := store.ActiveExists(ctx, a.row.ID); err != nil || !active {
			t.Fatalf("one running + one pending: want true, got %v (err %v)", active, err)
		}
		if err := store.Finish(ctx, p1.ID, "done", "m", nil); err != nil {
			t.Fatalf("finish p1: %v", err)
		}
		if active, err := store.ActiveExists(ctx, a.row.ID); err != nil || !active {
			t.Fatalf("p2 still pending: want true, got %v (err %v)", active, err)
		}
		claimed, err = store.ClaimNext(ctx)
		if err != nil {
			t.Fatalf("claim p2: %v", err)
		}
		if err := store.Finish(ctx, claimed.ID, "done", "m", nil); err != nil {
			t.Fatalf("finish p2: %v", err)
		}
		if active, err := store.ActiveExists(ctx, a.row.ID); err != nil || active {
			t.Fatalf("all terminal: want false, got %v (err %v)", active, err)
		}
		if active, err := store.ActiveExists(ctx, b.row.ID); err != nil || active {
			t.Fatalf("b has no jobs: want false, got %v (err %v)", active, err)
		}
	})

	t.Run("reset running re-queues", func(t *testing.T) {
		n, err := store.ResetRunning(ctx)
		if err != nil {
			t.Fatalf("reset: %v", err)
		}
		if n != 0 {
			t.Fatalf("nothing running: want 0 re-queued, got %d", n)
		}
		j, err := store.Create(ctx, a.row.ID, "manual", nil)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		claimed, err := store.ClaimNext(ctx)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if claimed.ID != j.ID {
			t.Fatalf("claimed %s, want %s", claimed.ID, j.ID)
		}
		n, err = store.ResetRunning(ctx)
		if err != nil {
			t.Fatalf("reset running: %v", err)
		}
		if n != 1 {
			t.Fatalf("want 1 re-queued, got %d", n)
		}
		got, err := store.Get(ctx, a.row.ID, j.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Status != "pending" {
			t.Fatalf("re-queued job: want pending, got %s", got.Status)
		}
		// Re-claimable, and a third reset finds nothing.
		claimed, err = store.ClaimNext(ctx)
		if err != nil {
			t.Fatalf("re-claim: %v", err)
		}
		if claimed.ID != j.ID {
			t.Fatalf("re-claimed %s, want %s", claimed.ID, j.ID)
		}
		if err := store.Finish(ctx, j.ID, "done", "m", nil); err != nil {
			t.Fatalf("finish: %v", err)
		}
		n, err = store.ResetRunning(ctx)
		if err != nil {
			t.Fatalf("final reset: %v", err)
		}
		if n != 0 {
			t.Fatalf("all terminal: want 0 re-queued, got %d", n)
		}
	})
}

func TestCategoriseJobCreateSource(t *testing.T) {
	env := newTestEnv(t, "catj-c")
	store := NewCategoriseJobStore(env.pool)
	a := env.user(t, "catj-c")
	ctx := env.ctx

	if _, err := store.Create(ctx, a.row.ID, "bogus", nil); err == nil {
		t.Fatal("bogus source must be rejected before the DB")
	}
	if _, err := store.Create(ctx, a.row.ID, "import", nil); err == nil {
		t.Fatal("import source without an import job id must be rejected")
	}

	// A real import job (seeded account + job row): the FK is honored
	// and the id round-trips.
	namePack, err := crypto.Seal(a.dek, []byte("import account"))
	if err != nil {
		t.Fatalf("seal account name: %v", err)
	}
	var acctID string
	if err := env.pool.QueryRow(ctx, `
		INSERT INTO accounts (user_id, name_encrypted, name_hash, type, currency)
		VALUES ($1, $2, $3, 'bank', 'CAD')
		RETURNING id`, a.row.ID, namePack, hashName("import account")).Scan(&acctID); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	filePack, err := crypto.Seal(a.dek, []byte("statement bytes"))
	if err != nil {
		t.Fatalf("seal file: %v", err)
	}
	var impID string
	if err := env.pool.QueryRow(ctx, `
		INSERT INTO import_jobs (user_id, account_id, status, file_encrypted, file_name_encrypted)
		VALUES ($1, $2, 'done', $3, $4)
		RETURNING id`, a.row.ID, acctID, filePack, namePack).Scan(&impID); err != nil {
		t.Fatalf("seed import job: %v", err)
	}
	job, err := store.Create(ctx, a.row.ID, "import", &impID)
	if err != nil {
		t.Fatalf("create import-sourced: %v", err)
	}
	if job.Source != "import" || job.ImportJobID == nil || *job.ImportJobID != impID {
		t.Fatalf("import-sourced job: %+v", job)
	}
	if err := store.Finish(ctx, job.ID, "done", "m", nil); err != nil {
		t.Fatalf("cleanup finish: %v", err)
	}
}

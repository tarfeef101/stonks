// Import job store tests (plan step 4.1): one shared postgres:18
// testcontainer (newTestEnv) with subtests covering the upload round
// trip (file + filename sealed and opened under the user's DEK),
// plaintext-never-stored, filename validation, list order +
// limit/offset, per-user isolation, the account reference check, and
// the wrong-DEK decrypt error.
package repo

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"finance-tracker/internal/crypto"
)

func TestImportJobStore(t *testing.T) {
	env := newTestEnv(t, "imp-a", "imp-b")
	store := NewImportJobStore(env.pool)
	if store == nil {
		t.Fatal("non-nil pool must give non-nil store")
	}
	if NewImportJobStore(nil) != nil {
		t.Fatal("nil pool must give nil store")
	}
	accounts := NewAccountStore(env.pool)
	a := env.user(t, "imp-a")
	b := env.user(t, "imp-b")
	ctx := env.ctx
	aid, bid := a.row.ID, b.row.ID

	acctA, err := accounts.Create(ctx, aid, AccountIn{Name: "Checking", Type: "bank", Currency: "USD"}, a.dek)
	if err != nil {
		t.Fatalf("create account A: %v", err)
	}
	acctB, err := accounts.Create(ctx, bid, AccountIn{Name: "Savings", Type: "cash", Currency: "USD"}, b.dek)
	if err != nil {
		t.Fatalf("create account B: %v", err)
	}

	fileBytes := []byte("date,description,amount\n2026-09-15,Corner Store,-4.20\n")

	t.Run("create + get round trip", func(t *testing.T) {
		in := ImportJobIn{AccountID: acctA.ID, FileName: "statement.csv", File: fileBytes}
		created, err := store.Create(ctx, aid, in, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if created.Status != "uploaded" {
			t.Fatalf("created status = %q, want uploaded", created.Status)
		}
		if created.AccountID != acctA.ID || created.FileName != "statement.csv" {
			t.Fatalf("created: %+v", created)
		}
		if created.Result != nil || created.Error != nil {
			t.Fatalf("fresh job carries result/error: %+v", created)
		}
		got, err := store.Get(ctx, aid, created.ID, a.dek)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.ID != created.ID || got.AccountID != created.AccountID ||
			got.FileName != created.FileName || got.Status != created.Status {
			t.Fatalf("get: want %+v, got %+v", created, got)
		}
		if got.Result != nil || got.Error != nil {
			t.Fatalf("get job carries result/error: %+v", got)
		}
	})

	t.Run("DB stores packs, not plaintext", func(t *testing.T) {
		in := ImportJobIn{AccountID: acctA.ID, FileName: "db-check.csv", File: []byte("raw-bytes-1234567890")}
		job, err := store.Create(ctx, aid, in, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		var (
			filePack, namePack []byte
			res                []byte
			erro               sql.NullString
		)
		err = env.pool.QueryRow(ctx, `
			SELECT file_encrypted, file_name_encrypted, result, error
			FROM import_jobs WHERE id = $1`, job.ID).
			Scan(&filePack, &namePack, &res, &erro)
		if err != nil {
			t.Fatalf("db scan: %v", err)
		}
		if bytes.Equal(filePack, in.File) || bytes.Equal(namePack, []byte(in.FileName)) {
			t.Fatal("file or file name stored as plaintext")
		}
		if len(filePack) < crypto.PackMinSize || len(namePack) < crypto.PackMinSize {
			t.Fatalf("packs shorter than PackMinSize: file %d, name %d", len(filePack), len(namePack))
		}
		if res != nil || erro.Valid {
			t.Fatalf("result/error on fresh row = (%v, %v), want both NULL", res, erro)
		}
	})

	// Runs before the list subtest so user B still has zero jobs.
	t.Run("cross-user isolation", func(t *testing.T) {
		in := ImportJobIn{AccountID: acctA.ID, FileName: "a-private.csv", File: []byte("a's bytes")}
		job, err := store.Create(ctx, aid, in, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := store.Get(ctx, bid, job.ID, b.dek); !errors.Is(err, ErrImportJobNotFound) {
			t.Fatalf("cross-user get: err = %v, want ErrImportJobNotFound", err)
		}
		jobs, err := store.List(ctx, bid, ImportJobFilter{}, b.dek)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(jobs) != 0 {
			t.Fatalf("user B list = %d jobs, want 0 (user A's job leaked)", len(jobs))
		}
	})

	t.Run("list order + limit/offset", func(t *testing.T) {
		// Raw INSERTs with pinned created_at: the order is fully
		// deterministic, independent of wall-clock collisions between
		// store.Create rows (id DESC only breaks exact timestamp ties).
		seed := func(name string, age time.Duration) string {
			t.Helper()
			filePack, err := crypto.Seal(b.dek, []byte("body"))
			if err != nil {
				t.Fatalf("seal file: %v", err)
			}
			namePack, err := crypto.Seal(b.dek, []byte(name))
			if err != nil {
				t.Fatalf("seal name: %v", err)
			}
			ts := time.Now().UTC().Add(-age)
			var id string
			err = env.pool.QueryRow(ctx, `
				INSERT INTO import_jobs
					(user_id, account_id, status, file_encrypted, file_name_encrypted, created_at, updated_at)
				VALUES ($1, $2, 'uploaded', $3, $4, $5, $5)
				RETURNING id`, bid, acctB.ID, filePack, namePack, ts).Scan(&id)
			if err != nil {
				t.Fatalf("seed %s: %v", name, err)
			}
			return id
		}
		oldest := seed("old.csv", 60*time.Second)
		middle := seed("mid.csv", 30*time.Second)
		newest := seed("new.csv", 10*time.Second)

		jobs, err := store.List(ctx, bid, ImportJobFilter{}, b.dek)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(jobs) != 3 {
			t.Fatalf("list = %d jobs, want 3", len(jobs))
		}
		want := []string{newest, middle, oldest}
		for i, j := range jobs {
			if j.ID != want[i] {
				t.Fatalf("list[%d] = %s, want %s", i, j.ID, want[i])
			}
		}
		if jobs[0].FileName != "new.csv" || jobs[0].Status != "uploaded" {
			t.Fatalf("list[0]: %+v", jobs[0])
		}

		page, err := store.List(ctx, bid, ImportJobFilter{Limit: 2}, b.dek)
		if err != nil {
			t.Fatalf("list limit 2: %v", err)
		}
		if len(page) != 2 || page[0].ID != newest || page[1].ID != middle {
			t.Fatalf("limit 2 = %d jobs (%s, %s), want [%s %s]",
				len(page), page[0].ID, page[1].ID, newest, middle)
		}
		page, err = store.List(ctx, bid, ImportJobFilter{Limit: 1, Offset: 1}, b.dek)
		if err != nil {
			t.Fatalf("list offset 1 limit 1: %v", err)
		}
		if len(page) != 1 || page[0].ID != middle {
			t.Fatalf("offset 1 limit 1 = %d jobs, want [%s]", len(page), middle)
		}
		page, err = store.List(ctx, bid, ImportJobFilter{Offset: 10}, b.dek)
		if err != nil {
			t.Fatalf("list offset 10: %v", err)
		}
		if len(page) != 0 {
			t.Fatalf("offset past the end = %d jobs, want 0", len(page))
		}
	})

	t.Run("account checks", func(t *testing.T) {
		if _, err := store.Create(ctx, aid,
			ImportJobIn{AccountID: zeroUUID, FileName: "x.csv", File: []byte("x")}, a.dek); !errors.Is(err, ErrAccountNotFound) {
			t.Fatalf("ghost account: err = %v, want ErrAccountNotFound", err)
		}
		// Another user's account is indistinguishable from a missing
		// one (existence is not disclosed).
		if _, err := store.Create(ctx, aid,
			ImportJobIn{AccountID: acctB.ID, FileName: "x.csv", File: []byte("x")}, a.dek); !errors.Is(err, ErrAccountNotFound) {
			t.Fatalf("foreign account: err = %v, want ErrAccountNotFound", err)
		}
	})

	t.Run("ValidateFileName", func(t *testing.T) {
		if err := ValidateFileName("statement.csv"); err != nil {
			t.Fatalf("valid name: %v", err)
		}
		if err := ValidateFileName(strings.Repeat("n", 255)); err != nil {
			t.Fatalf("255-rune name: %v", err)
		}
		if err := ValidateFileName(""); err == nil || err.Error() != "file name must not be empty" {
			t.Fatalf("empty name: err = %v, want the exact empty message", err)
		}
		if err := ValidateFileName(strings.Repeat("n", 256)); err == nil || err.Error() != "file name must be at most 255 characters" {
			t.Fatalf("256-rune name: err = %v, want the exact length message", err)
		}
	})

	t.Run("create validation", func(t *testing.T) {
		_, err := store.Create(ctx, aid,
			ImportJobIn{AccountID: acctA.ID, FileName: "", File: []byte("x")}, a.dek)
		if err == nil || !strings.Contains(err.Error(), "file name must not be empty") {
			t.Fatalf("empty-name create: err = %v, want the empty message wrapped", err)
		}
	})

	t.Run("wrong DEK is a decrypt error, not not-found", func(t *testing.T) {
		in := ImportJobIn{AccountID: acctA.ID, FileName: "dek-check.csv", File: []byte("x")}
		job, err := store.Create(ctx, aid, in, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		wrong, err := crypto.NewDEK()
		if err != nil {
			t.Fatalf("new DEK: %v", err)
		}
		_, err = store.Get(ctx, aid, job.ID, wrong)
		if err == nil {
			t.Fatal("get with the wrong DEK succeeded, want a decrypt error")
		}
		if errors.Is(err, ErrImportJobNotFound) {
			t.Fatalf("wrong-DEK error masquerades as not-found: %v", err)
		}
	})

	// sameJSON compares two jsonb payloads semantically: postgres
	// re-serializes (key order, whitespace, number spelling), so the
	// bytes are never equal and the maps are.
	sameJSON := func(a, b []byte) bool {
		var x, y map[string]any
		if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
			return false
		}
		return reflect.DeepEqual(x, y)
	}

	t.Run("mapping round trip", func(t *testing.T) {
		file := []byte("date,description,amount\n2026-10-01,Market,-7.10\n")
		in := ImportJobIn{AccountID: acctA.ID, FileName: "map-check.csv", File: file}
		created, err := store.Create(ctx, aid, in, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if created.Mapping != nil {
			t.Fatalf("fresh job carries a mapping: %s", created.Mapping)
		}

		sent := []byte(`{"kind":"csv","map":{"date_posted":{"headers":["Date"],"index":0},"amount":{"index":2}}}`)
		updated, err := store.SetMapping(ctx, aid, created.ID, sent, a.dek)
		if err != nil {
			t.Fatalf("set mapping: %v", err)
		}
		if updated.Status != "mapping" {
			t.Fatalf("updated status = %q, want mapping", updated.Status)
		}
		if !sameJSON(updated.Mapping, sent) {
			t.Fatalf("updated mapping = %s, want %s (semantically)", updated.Mapping, sent)
		}
		if !updated.UpdatedAt.After(created.UpdatedAt) {
			t.Fatalf("updated_at %s is not after created_at %s", updated.UpdatedAt, created.UpdatedAt)
		}

		// A re-read must match the stored document semantically.
		got, err := store.Get(ctx, aid, created.ID, a.dek)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if !sameJSON(got.Mapping, sent) {
			t.Fatalf("re-read mapping = %s, want %s (semantically)", got.Mapping, sent)
		}

		// The file bytes survive the mapping update untouched.
		back, err := store.ReadFile(ctx, aid, created.ID, a.dek)
		if err != nil {
			t.Fatalf("read file: %v", err)
		}
		if !bytes.Equal(back, file) {
			t.Fatalf("file = %q, want %q", back, file)
		}

		// User B sees neither the mapping nor the file.
		if _, err := store.SetMapping(ctx, bid, created.ID, sent, b.dek); !errors.Is(err, ErrImportJobNotFound) {
			t.Fatalf("cross-user set mapping: err = %v, want ErrImportJobNotFound", err)
		}
		if _, err := store.ReadFile(ctx, bid, created.ID, b.dek); !errors.Is(err, ErrImportJobNotFound) {
			t.Fatalf("cross-user read file: err = %v, want ErrImportJobNotFound", err)
		}
	})
}

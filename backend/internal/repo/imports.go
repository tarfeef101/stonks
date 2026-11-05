// Per-user import job repository (plan step 4.1, migration 0005): the
// upload/parse/import state machine for statement files. file_encrypted
// (the statement bytes) and file_name_encrypted (the original filename)
// are DEK-encrypted packs (plan §5: the statement content is the most
// sensitive data the app holds); the store takes the caller's unwrapped
// DEK and never sees the KEK.
//
// status walks uploaded -> mapping -> importing -> done|failed; the
// vocabulary mirrors the 0005 named CHECK (import_jobs_status_check) so
// a bad value is rejected early, not a 23514 surfacing as a 500.
// Create starts every job at 'uploaded'; SetMapping (step 4.5) stores
// the user's field mapping and moves it to 'mapping'; the
// importing/done/failed transitions are the 4.6 runner. result (jsonb),
// mapping (jsonb) and error (text) are NULL until a later step sets
// them.
package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"finance-tracker/internal/crypto"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ImportJobStatuses is the 0005 import_jobs.status vocabulary (the
// pipeline states, plan 4.1).
var ImportJobStatuses = []string{"uploaded", "mapping", "importing", "done", "failed"}

// maxImportFileNameLen caps the original filename (the column is
// unbounded bytea, but the API should 400 before the DB sees it).
const maxImportFileNameLen = 255

// List bounds (the notification convention: default 50, cap 100).
const (
	defaultImportJobLimit = 50
	maxImportJobLimit     = 100
)

// ImportJob is one import_jobs row. FileName is the DEK-decrypted
// original filename. Result is the raw jsonb outcome (nil while unset),
// Error the failure message (nil when unset), and Mapping the raw jsonb
// field mapping the user confirmed (nil while unset, step 4.5).
type ImportJob struct {
	ID        string
	AccountID string
	FileName  string
	Status    string
	Result    *json.RawMessage
	Error     *string
	Mapping   json.RawMessage
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ImportJobIn is the Create input (POST /api/imports): the account the
// job lands in, the original filename, and the statement bytes.
type ImportJobIn struct {
	AccountID string
	FileName  string
	File      []byte
}

// ImportJobFilter is the List query. Limit defaults to 50, capped at
// 100. Rows come back ordered created_at DESC, id DESC.
type ImportJobFilter struct {
	Limit  int
	Offset int
}

// ImportJobStore persists import jobs. One store serves every user;
// each method takes a userID and scopes its SQL on it.
type ImportJobStore struct {
	pool *pgxpool.Pool
}

// NewImportJobStore returns an ImportJobStore backed by pool (nil for a
// nil pool, mirroring the other repo constructors).
func NewImportJobStore(pool *pgxpool.Pool) *ImportJobStore {
	if pool == nil {
		return nil
	}
	return &ImportJobStore{pool: pool}
}

// ValidateFileName checks the original filename without a database, for
// the same 400-before-the-repo reason as ValidatePeriod (budgets.go):
// the 4.1 upload handler runs it before any store call, so a bad name
// is a 400 carrying this exact message; the repo re-runs it (defense-
// in-depth, as with validateName).
func ValidateFileName(name string) error {
	if name == "" {
		return errors.New("file name must not be empty")
	}
	if utf8.RuneCountInString(name) > maxImportFileNameLen {
		return fmt.Errorf("file name must be at most %d characters", maxImportFileNameLen)
	}
	return nil
}

// importJobFrom assembles a row from the raw scan columns and decrypts
// the filename pack under dek. A pack that does not open is a
// key/data mismatch the API surfaces as 500 (the DEK came from the
// user's own wrapped_dek, so this is corruption, not a client error) —
// same rule as the other stores.
func importJobFrom(id, accountID, status string, namePack, result, mapping []byte, erro sql.NullString, createdAt, updatedAt time.Time, dek []byte) (ImportJob, error) {
	name, err := crypto.Open(dek, namePack)
	if err != nil {
		return ImportJob{}, fmt.Errorf("repo: decrypt import job %s file name: %w", id, err)
	}
	j := ImportJob{
		ID:        id,
		AccountID: accountID,
		FileName:  string(name),
		Status:    status,
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}
	if result != nil {
		r := json.RawMessage(result)
		j.Result = &r
	}
	if mapping != nil {
		j.Mapping = json.RawMessage(mapping)
	}
	if erro.Valid {
		e := erro.String
		j.Error = &e
	}
	return j, nil
}

// Create uploads the job: the account must be the user's own (a
// missing or foreign account is ErrAccountNotFound, existence is not
// disclosed), the file + filename are sealed under the user's DEK, and
// the row starts at status 'uploaded'.
func (s *ImportJobStore) Create(ctx context.Context, userID string, in ImportJobIn, dek []byte) (ImportJob, error) {
	if err := ValidateFileName(in.FileName); err != nil {
		return ImportJob{}, fmt.Errorf("repo: %w", err)
	}
	var n int
	if err := s.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM accounts WHERE user_id = $1 AND id = $2",
		userID, in.AccountID).Scan(&n); err != nil {
		return ImportJob{}, fmt.Errorf("repo: check import account: %w", err)
	}
	if n == 0 {
		return ImportJob{}, ErrAccountNotFound
	}
	filePack, err := crypto.Seal(dek, in.File)
	if err != nil {
		return ImportJob{}, fmt.Errorf("repo: seal import file: %w", err)
	}
	namePack, err := crypto.Seal(dek, []byte(in.FileName))
	if err != nil {
		return ImportJob{}, fmt.Errorf("repo: seal import file name: %w", err)
	}
	var id string
	err = s.pool.QueryRow(ctx, `
		INSERT INTO import_jobs (user_id, account_id, status, file_encrypted, file_name_encrypted)
		VALUES ($1, $2, 'uploaded', $3, $4)
		RETURNING id`, userID, in.AccountID, filePack, namePack).Scan(&id)
	if err != nil {
		return ImportJob{}, fmt.Errorf("repo: create import job: %w", err)
	}
	return s.Get(ctx, userID, id, dek)
}

// Get returns one of the user's import jobs, or ErrImportJobNotFound
// when the id is not the user's (existence is not disclosed across
// users). The filename is decrypted under the caller's DEK.
func (s *ImportJobStore) Get(ctx context.Context, userID, id string, dek []byte) (ImportJob, error) {
	var (
		namePack, mapping []byte
		result            []byte
		erro              sql.NullString
		j                 ImportJob
	)
	err := s.pool.QueryRow(ctx, `
		SELECT account_id, status, file_name_encrypted, result, mapping, error,
		       created_at, updated_at
		FROM import_jobs
		WHERE user_id = $1 AND id = $2`, userID, id).
		Scan(&j.AccountID, &j.Status, &namePack, &result, &mapping, &erro, &j.CreatedAt, &j.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ImportJob{}, ErrImportJobNotFound
	}
	if err != nil {
		return ImportJob{}, fmt.Errorf("repo: get import job: %w", err)
	}
	return importJobFrom(id, j.AccountID, j.Status, namePack, result, mapping, erro, j.CreatedAt, j.UpdatedAt, dek)
}

// List returns a page of the user's import jobs, newest first
// (created_at DESC, id DESC).
func (s *ImportJobStore) List(ctx context.Context, userID string, f ImportJobFilter, dek []byte) ([]ImportJob, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultImportJobLimit
	}
	if limit > maxImportJobLimit {
		limit = maxImportJobLimit
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, account_id, status, file_name_encrypted, result, mapping, error,
		       created_at, updated_at
		FROM import_jobs
		WHERE user_id = $1
		ORDER BY created_at DESC, id DESC
		LIMIT `+fmt.Sprint(limit)+` OFFSET `+fmt.Sprint(f.Offset), userID)
	if err != nil {
		return nil, fmt.Errorf("repo: list import jobs: %w", err)
	}
	defer rows.Close()
	out := make([]ImportJob, 0, limit)
	for rows.Next() {
		var (
			id, accountID, status string
			namePack, mapping     []byte
			result                []byte
			erro                  sql.NullString
			createdAt, updatedAt  time.Time
		)
		if err := rows.Scan(&id, &accountID, &status, &namePack, &result, &mapping, &erro, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("repo: scan import job: %w", err)
		}
		j, err := importJobFrom(id, accountID, status, namePack, result, mapping, erro, createdAt, updatedAt, dek)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo: list import jobs: %w", err)
	}
	return out, nil
}

// SetMapping stores the confirmed field mapping (step 4.5: the user
// chose a column per app field in the UI) and moves the job to status
// 'mapping'. The mapping is validated by the server before it gets
// here; the repo just persists it. A job the caller does not own is
// ErrImportJobNotFound (existence is not disclosed); the returned row
// is the re-read (with the filename decrypted under dek), like the
// other Update methods.
func (s *ImportJobStore) SetMapping(ctx context.Context, userID, id string, mapping json.RawMessage, dek []byte) (ImportJob, error) {
	var idOut string
	err := s.pool.QueryRow(ctx, `
		UPDATE import_jobs
		SET mapping = $3, status = 'mapping', updated_at = now()
		WHERE user_id = $1 AND id = $2
		RETURNING id`, userID, id, string(mapping)).Scan(&idOut)
	if errors.Is(err, sql.ErrNoRows) {
		return ImportJob{}, ErrImportJobNotFound
	}
	if err != nil {
		return ImportJob{}, fmt.Errorf("repo: set import job mapping: %w", err)
	}
	return s.Get(ctx, userID, id, dek)
}

// StartRun moves a job to status 'importing' and returns the re-read
// row, like the other update methods. The status precondition (the job
// must be in 'mapping'; terminal jobs are immutable) is the caller's —
// the API checks it before starting, so a row can never be re-opened
// from here. A job the caller does not own is ErrImportJobNotFound.
func (s *ImportJobStore) StartRun(ctx context.Context, userID, id string, dek []byte) (ImportJob, error) {
	var idOut string
	err := s.pool.QueryRow(ctx, `
		UPDATE import_jobs
		SET status = 'importing', updated_at = now()
		WHERE user_id = $1 AND id = $2
		RETURNING id`, userID, id).Scan(&idOut)
	if errors.Is(err, sql.ErrNoRows) {
		return ImportJob{}, ErrImportJobNotFound
	}
	if err != nil {
		return ImportJob{}, fmt.Errorf("repo: start import job run: %w", err)
	}
	return s.Get(ctx, userID, id, dek)
}

// FinishRun writes the run outcome: status is a terminal vocabulary
// value ('done' or 'failed'), result the raw jsonb outcome (nil for a
// failed run) and runErr the failure message (nil for a done run).
// Both are stored verbatim; the API validates the shapes. The status is
// checked against the 0005 vocabulary before the SQL (same rule as the
// package header); a job the caller does not own is ErrImportJobNotFound.
func (s *ImportJobStore) FinishRun(ctx context.Context, userID, id string, status string, result *json.RawMessage, runErr *string, dek []byte) (ImportJob, error) {
	ok := false
	for _, want := range ImportJobStatuses {
		if want == status {
			ok = true
			break
		}
	}
	if !ok {
		return ImportJob{}, fmt.Errorf("repo: import job status %q is not a pipeline state", status)
	}
	// jsonb binds as string (nil = NULL), mirroring SetMapping; a
	// []byte / *json.RawMessage binding is rejected by pgx.
	var res any
	if result != nil {
		res = string(*result)
	}
	var errArg any
	if runErr != nil {
		errArg = *runErr
	}
	var idOut string
	err := s.pool.QueryRow(ctx, `
		UPDATE import_jobs
		SET status = $3, result = $4, error = $5, updated_at = now()
		WHERE user_id = $1 AND id = $2
		RETURNING id`, userID, id, status, res, errArg).Scan(&idOut)
	if errors.Is(err, sql.ErrNoRows) {
		return ImportJob{}, ErrImportJobNotFound
	}
	if err != nil {
		return ImportJob{}, fmt.Errorf("repo: finish import job run: %w", err)
	}
	return s.Get(ctx, userID, id, dek)
}

// ReadFile returns the DEK-decrypted statement bytes for one of the
// user's jobs, or ErrImportJobNotFound when the id is not the user's.
// The 4.5 preview/analyze endpoints and the 4.6 runner need the raw
// file without the caller having to re-upload it.
func (s *ImportJobStore) ReadFile(ctx context.Context, userID, id string, dek []byte) ([]byte, error) {
	var pack []byte
	err := s.pool.QueryRow(ctx, `
		SELECT file_encrypted
		FROM import_jobs
		WHERE user_id = $1 AND id = $2`, userID, id).Scan(&pack)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrImportJobNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("repo: read import job file: %w", err)
	}
	file, err := crypto.Open(dek, pack)
	if err != nil {
		return nil, fmt.Errorf("repo: decrypt import job %s file: %w", id, err)
	}
	return file, nil
}

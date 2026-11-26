package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"finance-tracker/internal/repo"
)

// --- fakes (52a3) ------------------------------------------------------------

// snapAccts implements the full server AccountStore; the snapshot and
// equity routes exercise only Get.
type snapAccts struct {
	byID map[string]repo.Account
}

func (f *snapAccts) List(ctx context.Context, userID string, dek []byte) ([]repo.Account, error) {
	return nil, nil
}
func (f *snapAccts) Get(ctx context.Context, userID, id string, dek []byte) (repo.Account, error) {
	a, ok := f.byID[id]
	if !ok {
		return repo.Account{}, repo.ErrAccountNotFound
	}
	return a, nil
}
func (f *snapAccts) Create(ctx context.Context, userID string, in repo.AccountIn, dek []byte) (repo.Account, error) {
	return repo.Account{}, nil
}
func (f *snapAccts) Update(ctx context.Context, userID, id string, p repo.AccountPatch, dek []byte) (repo.Account, error) {
	return repo.Account{}, nil
}
func (f *snapAccts) Delete(ctx context.Context, userID, id string) error { return nil }

// snapStoreFake implements the full server SnapshotStore with the repo
// semantics: duplicate (account, as_of) conflicts, user+account scoping,
// DESC list ordering (ties: later insertion first).
type snapStoreFake struct {
	mu   sync.Mutex
	rows []repo.Snapshot
	seq  int
}

func (f *snapStoreFake) CreateSnapshot(ctx context.Context, userID string, in repo.SnapshotIn, dek []byte) (repo.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.AccountID == in.AccountID && r.AsOf.Equal(in.AsOf) {
			return repo.Snapshot{}, repo.ErrSnapshotConflict
		}
	}
	f.seq++
	now := time.Now().UTC()
	s := repo.Snapshot{
		ID:        "sn" + strconv.Itoa(f.seq),
		UserID:    userID,
		AccountID: in.AccountID,
		AsOf:      in.AsOf,
		Balance:   in.Balance,
		Note:      in.Note,
		CreatedAt: now,
		UpdatedAt: now,
	}
	f.rows = append(f.rows, s)
	return s, nil
}

func (f *snapStoreFake) ListSnapshots(ctx context.Context, userID, accountID string, dek []byte) ([]repo.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Walk latest-insertion first so the stable sort below keeps
	// "later insertion first" for equal AsOf values.
	out := make([]repo.Snapshot, 0, len(f.rows))
	for i := len(f.rows) - 1; i >= 0; i-- {
		r := f.rows[i]
		if r.UserID == userID && r.AccountID == accountID {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].AsOf.After(out[j].AsOf)
	})
	return out, nil
}

func (f *snapStoreFake) GetSnapshot(ctx context.Context, userID, id string, dek []byte) (repo.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.UserID == userID && r.ID == id {
			return r, nil
		}
	}
	return repo.Snapshot{}, repo.ErrSnapshotNotFound
}

func (f *snapStoreFake) UpdateSnapshot(ctx context.Context, userID, id string, p repo.SnapshotPatch, dek []byte) (repo.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.rows {
		r := &f.rows[i]
		if r.UserID != userID || r.ID != id {
			continue
		}
		if p.Balance != nil {
			r.Balance = *p.Balance
		}
		if p.Note != nil {
			r.Note = *p.Note
		}
		r.UpdatedAt = time.Now().UTC()
		return *r, nil
	}
	return repo.Snapshot{}, repo.ErrSnapshotNotFound
}

func (f *snapStoreFake) DeleteSnapshot(ctx context.Context, userID, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.rows {
		if f.rows[i].UserID == userID && f.rows[i].ID == id {
			f.rows = append(f.rows[:i], f.rows[i+1:]...)
			return nil
		}
	}
	return repo.ErrSnapshotNotFound
}

func (f *snapStoreFake) EquitySeries(ctx context.Context, userID, accountID string, from, to time.Time) ([]repo.EquityPoint, error) {
	if accountID != "a1" {
		return nil, repo.ErrAccountNotFound
	}
	return []repo.EquityPoint{
		{AsOf: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), Balance: 10000, NetContributions: 10000},
		{AsOf: time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC), Balance: 123456, NetContributions: 150000},
	}, nil
}

// --- harness -------------------------------------------------------------------

// newSnapsTest wires the snapshot and equity handlers onto fakes with a
// real (in-memory) bootstrap so requireUser's EnsureUser/UnwrapDEK path
// runs unmodified; these routes touch only h.accounts and h.snapshots,
// so every other store is nil.
func newSnapsTest(t *testing.T, accts *snapAccts, snaps *snapStoreFake) (http.Handler, string) {
	t.Helper()
	key := mintKey(t)
	b, err := NewBootstrap(newMemStore(), testKEK(t), nil)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	h := New(fakeReady{}, BuildInfo{}, liveAuth(t, key), b, nil, accts, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, false, nil, nil, nil, nil, nil, snaps, nil, PublicConfig{})
	token := mintIdentityToken(t, key, "snaps-sub", "snaps@localhost", "Snaps", "snapsuser")
	return h, token
}

// --- response shapes -----------------------------------------------------------

type snapshotResp struct {
	ID        string `json:"id"`
	AccountID string `json:"account_id"`
	AsOf      string `json:"as_of"`
	Balance   string `json:"balance"`
	Note      string `json:"note"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type equityPointResp struct {
	AsOf             string `json:"as_of"`
	Balance          string `json:"balance"`
	NetContributions string `json:"net_contributions"`
}

func decodeSnaps(t *testing.T, body string) []snapshotResp {
	t.Helper()
	var out []snapshotResp
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decoding snapshot list %s: %v", body, err)
	}
	return out
}

// --- tests ---------------------------------------------------------------------

func TestSavingsSnapshots(t *testing.T) {
	accts := &snapAccts{byID: map[string]repo.Account{
		"a1": {ID: "a1", Name: "A1", Type: "tfsa", Currency: "CAD", OpeningBalance: "100.00"},
	}}
	snaps := &snapStoreFake{}
	h, token := newSnapsTest(t, accts, snaps)
	const listPath = "/api/v1/savings/snapshots?account_id=a1"

	// 1. Empty list: a JSON array, never null.
	code, body := doRaw(t, h, http.MethodGet, listPath, "", token)
	if code != http.StatusOK {
		t.Fatalf("list status = %d, want 200 (body %s)", code, body)
	}
	if strings.TrimSpace(body) != "[]" {
		t.Fatalf("empty list body = %s, want []", body)
	}

	// 2. First create.
	code, body = doRaw(t, h, http.MethodPost, "/api/v1/savings/snapshots",
		`{"account_id":"a1","as_of":"2025-12-31","balance":"50000.00","note":"year end"}`, token)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (body %s)", code, body)
	}
	var one snapshotResp
	if err := json.Unmarshal([]byte(body), &one); err != nil {
		t.Fatalf("decoding create body %s: %v", body, err)
	}
	if one.ID != "sn1" || one.AccountID != "a1" || one.AsOf != "2025-12-31" ||
		one.Balance != "50000.00" || one.Note != "year end" {
		t.Fatalf("create = %+v, want id=sn1 as_of=2025-12-31 balance=50000.00 note=year end", one)
	}

	// 3. Duplicate (account, as_of) conflicts.
	code, body = doRaw(t, h, http.MethodPost, "/api/v1/savings/snapshots",
		`{"account_id":"a1","as_of":"2025-12-31","balance":"60000.00","note":""}`, token)
	if code != http.StatusConflict {
		t.Fatalf("dup create status = %d, want 409 (body %s)", code, body)
	}
	wantErrEnvelope(t, body, codeConflict)

	// 4. Second create.
	code, body = doRaw(t, h, http.MethodPost, "/api/v1/savings/snapshots",
		`{"account_id":"a1","as_of":"2026-06-01","balance":"60000.00","note":""}`, token)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (body %s)", code, body)
	}
	one = snapshotResp{}
	if err := json.Unmarshal([]byte(body), &one); err != nil {
		t.Fatalf("decoding create body %s: %v", body, err)
	}
	if one.ID != "sn2" {
		t.Fatalf("create id = %q, want sn2", one.ID)
	}

	// 5. List is DESC by as_of.
	code, body = doRaw(t, h, http.MethodGet, listPath, "", token)
	if code != http.StatusOK {
		t.Fatalf("list status = %d, want 200 (body %s)", code, body)
	}
	list := decodeSnaps(t, body)
	if len(list) != 2 {
		t.Fatalf("list len = %d, want 2 (body %s)", len(list), body)
	}
	if list[0].AsOf != "2026-06-01" || list[1].AsOf != "2025-12-31" {
		t.Fatalf("list order = %q, %q; want 2026-06-01 first", list[0].AsOf, list[1].AsOf)
	}

	// 6. Future date rejected.
	future := time.Now().UTC().AddDate(1, 0, 0).Format("2006-01-02")
	code, body = doRaw(t, h, http.MethodPost, "/api/v1/savings/snapshots",
		`{"account_id":"a1","as_of":"`+future+`","balance":"1.00","note":""}`, token)
	if code != http.StatusBadRequest {
		t.Fatalf("future as_of status = %d, want 400 (body %s)", code, body)
	}
	wantErrEnvelope(t, body, codeInvalidRequest)

	// 7. Malformed inputs rejected.
	badPosts := []string{
		`{"account_id":"a1","as_of":"1899-05-01","balance":"1.00","note":""}`,
		`{"account_id":"a1","as_of":"2026-13-01","balance":"1.00","note":""}`,
		`{"account_id":"a1","as_of":"2026-6-1","balance":"1.00","note":""}`,
		`{"account_id":"a1","as_of":"2026-01-01","balance":"-1.00","note":""}`,
		`{"account_id":"a1","as_of":"2026-01-01","balance":"abc","note":""}`,
		`{"account_id":"a1","as_of":"2026-01-01","balance":"1.00","note":"` + strings.Repeat("n", 501) + `"}`,
	}
	for _, bp := range badPosts {
		code, body = doRaw(t, h, http.MethodPost, "/api/v1/savings/snapshots", bp, token)
		if code != http.StatusBadRequest {
			t.Fatalf("post %s: status = %d, want 400 (body %s)", bp, code, body)
		}
		wantErrEnvelope(t, body, codeInvalidRequest)
	}

	// 8. Missing/foreign account.
	code, body = doRaw(t, h, http.MethodPost, "/api/v1/savings/snapshots",
		`{"account_id":"missing","as_of":"2026-01-01","balance":"1.00","note":""}`, token)
	if code != http.StatusNotFound {
		t.Fatalf("missing acct create status = %d, want 404 (body %s)", code, body)
	}
	wantErrEnvelope(t, body, codeNotFound)
	code, body = doRaw(t, h, http.MethodGet, "/api/v1/savings/snapshots?account_id=missing", "", token)
	if code != http.StatusNotFound {
		t.Fatalf("missing acct list status = %d, want 404 (body %s)", code, body)
	}
	wantErrEnvelope(t, body, codeNotFound)

	// 9. account_id required.
	code, body = doRaw(t, h, http.MethodGet, "/api/v1/savings/snapshots", "", token)
	if code != http.StatusBadRequest {
		t.Fatalf("no account_id status = %d, want 400 (body %s)", code, body)
	}
	wantErrEnvelope(t, body, codeInvalidRequest)

	// 10. Patch balance only: note untouched.
	code, body = doRaw(t, h, http.MethodPatch, "/api/v1/savings/snapshots/sn1",
		`{"balance":"51000.00"}`, token)
	if code != http.StatusOK {
		t.Fatalf("patch balance status = %d, want 200 (body %s)", code, body)
	}
	one = snapshotResp{}
	if err := json.Unmarshal([]byte(body), &one); err != nil {
		t.Fatalf("decoding patch body %s: %v", body, err)
	}
	if one.Balance != "51000.00" || one.Note != "year end" {
		t.Fatalf("patch balance = balance %q note %q; want 51000.00 / year end", one.Balance, one.Note)
	}

	// 11. Patch note to null clears it.
	code, body = doRaw(t, h, http.MethodPatch, "/api/v1/savings/snapshots/sn1",
		`{"note":null}`, token)
	if code != http.StatusOK {
		t.Fatalf("clear note status = %d, want 200 (body %s)", code, body)
	}
	one = snapshotResp{}
	if err := json.Unmarshal([]byte(body), &one); err != nil {
		t.Fatalf("decoding patch body %s: %v", body, err)
	}
	if one.Note != "" || one.Balance != "51000.00" {
		t.Fatalf("clear note = balance %q note %q; want 51000.00 / empty note", one.Balance, one.Note)
	}

	// 12. balance cannot be null.
	code, body = doRaw(t, h, http.MethodPatch, "/api/v1/savings/snapshots/sn1",
		`{"balance":null}`, token)
	if code != http.StatusBadRequest {
		t.Fatalf("null balance status = %d, want 400 (body %s)", code, body)
	}
	wantErrEnvelope(t, body, codeInvalidRequest)

	// 13. Empty patch is a no-op.
	code, body = doRaw(t, h, http.MethodPatch, "/api/v1/savings/snapshots/sn1", `{}`, token)
	if code != http.StatusOK {
		t.Fatalf("noop patch status = %d, want 200 (body %s)", code, body)
	}
	one = snapshotResp{}
	if err := json.Unmarshal([]byte(body), &one); err != nil {
		t.Fatalf("decoding patch body %s: %v", body, err)
	}
	if one.Balance != "51000.00" || one.Note != "" {
		t.Fatalf("noop patch = balance %q note %q; want 51000.00 / empty note", one.Balance, one.Note)
	}

	// 14. Unknown snapshot id.
	code, body = doRaw(t, h, http.MethodPatch, "/api/v1/savings/snapshots/sn404",
		`{"balance":"1.00"}`, token)
	if code != http.StatusNotFound {
		t.Fatalf("unknown id patch status = %d, want 404 (body %s)", code, body)
	}
	wantErrEnvelope(t, body, codeNotFound)

	// 15. Delete, then the list shrinks, then a second delete 404s.
	code, _ = doRaw(t, h, http.MethodDelete, "/api/v1/savings/snapshots/sn1", "", token)
	if code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", code)
	}
	code, body = doRaw(t, h, http.MethodGet, listPath, "", token)
	if code != http.StatusOK {
		t.Fatalf("list after delete status = %d, want 200 (body %s)", code, body)
	}
	list = decodeSnaps(t, body)
	if len(list) != 1 {
		t.Fatalf("list after delete len = %d, want 1 (body %s)", len(list), body)
	}
	code, body = doRaw(t, h, http.MethodDelete, "/api/v1/savings/snapshots/sn1", "", token)
	if code != http.StatusNotFound {
		t.Fatalf("redelete status = %d, want 404 (body %s)", code, body)
	}
	wantErrEnvelope(t, body, codeNotFound)
}

func TestSavingsEquity(t *testing.T) {
	accts := &snapAccts{byID: map[string]repo.Account{
		"a1": {ID: "a1", Name: "A1", Type: "tfsa", Currency: "CAD", OpeningBalance: "100.00"},
	}}
	snaps := &snapStoreFake{}
	h, token := newSnapsTest(t, accts, snaps)

	want := []equityPointResp{
		{AsOf: "2024-01-01", Balance: "100.00", NetContributions: "100.00"},
		{AsOf: "2026-06-15", Balance: "1234.56", NetContributions: "1500.00"},
	}
	checkPoints := func(t *testing.T, body string) {
		t.Helper()
		var env struct {
			Points []equityPointResp `json:"points"`
		}
		if err := json.Unmarshal([]byte(body), &env); err != nil {
			t.Fatalf("decoding %s: %v", body, err)
		}
		if len(env.Points) != len(want) {
			t.Fatalf("points len = %d, want %d (body %s)", len(env.Points), len(want), body)
		}
		for i := range want {
			if env.Points[i] != want[i] {
				t.Errorf("points[%d] = %+v, want %+v", i, env.Points[i], want[i])
			}
		}
	}

	// 1. Canned series with defaults.
	code, body := doRaw(t, h, http.MethodGet, "/api/v1/savings/equity?account_id=a1", "", token)
	if code != http.StatusOK {
		t.Fatalf("equity status = %d, want 200 (body %s)", code, body)
	}
	checkPoints(t, body)

	// 2. Explicit range: same canned points (the fake ignores the range).
	code, body = doRaw(t, h, http.MethodGet, "/api/v1/savings/equity?account_id=a1&from=2025-01-01&to=2026-12-31", "", token)
	if code != http.StatusOK {
		t.Fatalf("equity range status = %d, want 200 (body %s)", code, body)
	}
	checkPoints(t, body)

	// 3. Missing/foreign account.
	code, body = doRaw(t, h, http.MethodGet, "/api/v1/savings/equity?account_id=missing", "", token)
	if code != http.StatusNotFound {
		t.Fatalf("missing acct equity status = %d, want 404 (body %s)", code, body)
	}
	wantErrEnvelope(t, body, codeNotFound)

	// 4. Bad queries.
	badQueries := []string{
		"/api/v1/savings/equity",
		"/api/v1/savings/equity?account_id=a1&from=2027-01-01&to=2026-01-01",
		"/api/v1/savings/equity?account_id=a1&from=abc",
		"/api/v1/savings/equity?account_id=a1&from=1899-01-01",
		"/api/v1/savings/equity?account_id=a1&to=2101-01-01",
	}
	for _, q := range badQueries {
		code, body = doRaw(t, h, http.MethodGet, q, "", token)
		if code != http.StatusBadRequest {
			t.Fatalf("equity %s: status = %d, want 400 (body %s)", q, code, body)
		}
		wantErrEnvelope(t, body, codeInvalidRequest)
	}
}

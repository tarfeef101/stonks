package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"finance-tracker/internal/repo"
)

// --- fakes -----------------------------------------------------------------

type fakeUsers struct {
	user   repo.User
	getErr error
	setErr error
}

func (f *fakeUsers) Get(ctx context.Context, id string, dek []byte) (repo.User, error) {
	if f.getErr != nil {
		return repo.User{}, f.getErr
	}
	return f.user, nil
}

// SetBirthYear mirrors the repo validation: nil is a no-op, "" clears to
// NULL, otherwise a 4-digit year in 1900..current.
func (f *fakeUsers) SetBirthYear(ctx context.Context, id string, birthYear *string, dek []byte) error {
	if f.setErr != nil {
		return f.setErr
	}
	if birthYear == nil {
		return nil
	}
	if *birthYear == "" {
		f.user.BirthYear = nil
		return nil
	}
	if len(*birthYear) != 4 {
		return errors.New("birth year must be four digits")
	}
	y, err := strconv.Atoi(*birthYear)
	if err != nil || y < 1900 || y > time.Now().UTC().Year() {
		return errors.New("birth year out of range")
	}
	f.user.BirthYear = &y
	return nil
}

type fakeAccounts struct{ list []repo.Account }

func (f *fakeAccounts) List(ctx context.Context, userID string, dek []byte) ([]repo.Account, error) {
	return f.list, nil
}
func (f *fakeAccounts) Get(ctx context.Context, userID, id string, dek []byte) (repo.Account, error) {
	return repo.Account{}, nil
}
func (f *fakeAccounts) Create(ctx context.Context, userID string, in repo.AccountIn, dek []byte) (repo.Account, error) {
	return repo.Account{}, nil
}
func (f *fakeAccounts) Update(ctx context.Context, userID, id string, p repo.AccountPatch, dek []byte) (repo.Account, error) {
	return repo.Account{}, nil
}
func (f *fakeAccounts) Delete(ctx context.Context, userID, id string) error { return nil }

type fakeTxns struct{ sums []repo.SavingsSum }

func (f *fakeTxns) List(ctx context.Context, userID string, fl repo.TransactionFilter, dek []byte) ([]repo.Transaction, error) {
	return nil, nil
}
func (f *fakeTxns) AccountDebits(ctx context.Context, accountID string) ([]repo.DebitTxn, error) {
	return nil, nil
}
func (f *fakeTxns) Create(ctx context.Context, userID string, in repo.TransactionIn, dek []byte) (repo.Transaction, error) {
	return repo.Transaction{}, nil
}
func (f *fakeTxns) Upsert(ctx context.Context, userID string, in repo.TransactionIn, dek []byte) (repo.Transaction, bool, error) {
	return repo.Transaction{}, false, nil
}
func (f *fakeTxns) UpsertBatch(ctx context.Context, userID string, ins []repo.TransactionIn, dek []byte) ([]bool, error) {
	return nil, nil
}
func (f *fakeTxns) Update(ctx context.Context, userID, id string, p repo.TransactionPatch, dek []byte) (repo.Transaction, error) {
	return repo.Transaction{}, nil
}
func (f *fakeTxns) Delete(ctx context.Context, userID, id string) error { return nil }
func (f *fakeTxns) Aggregate(ctx context.Context, userID string, from, to time.Time, accountIDs []string) ([]repo.CategorySpend, error) {
	return nil, nil
}
func (f *fakeTxns) TrendAggregate(ctx context.Context, userID, period string, from, to time.Time, accountIDs []string) ([]repo.TrendBucket, error) {
	return nil, nil
}
func (f *fakeTxns) SavingsSums(ctx context.Context, userID string) ([]repo.SavingsSum, error) {
	return f.sums, nil
}

type fakeSnaps struct {
	snapshots map[string][]repo.Snapshot
	equity    map[string][]repo.EquityPoint
}

func (f *fakeSnaps) CreateSnapshot(ctx context.Context, userID string, in repo.SnapshotIn, dek []byte) (repo.Snapshot, error) {
	return repo.Snapshot{}, nil
}
func (f *fakeSnaps) ListSnapshots(ctx context.Context, userID, accountID string, dek []byte) ([]repo.Snapshot, error) {
	return f.snapshots[accountID], nil
}
func (f *fakeSnaps) GetSnapshot(ctx context.Context, userID, id string, dek []byte) (repo.Snapshot, error) {
	return repo.Snapshot{}, nil
}
func (f *fakeSnaps) UpdateSnapshot(ctx context.Context, userID, id string, p repo.SnapshotPatch, dek []byte) (repo.Snapshot, error) {
	return repo.Snapshot{}, nil
}
func (f *fakeSnaps) DeleteSnapshot(ctx context.Context, userID, id string) error { return nil }
func (f *fakeSnaps) EquitySeries(ctx context.Context, userID, accountID string, from, to time.Time) ([]repo.EquityPoint, error) {
	return f.equity[accountID], nil
}

type fakePlans struct{}

func (fakePlans) ListLimitOverrides(ctx context.Context, userID, planType string, year int) ([]repo.LimitOverride, error) {
	return nil, nil
}
func (fakePlans) UpsertLimitOverride(ctx context.Context, userID, planType string, year int, annualLimit string) (repo.LimitOverride, error) {
	return repo.LimitOverride{}, nil
}
func (fakePlans) DeleteLimitOverride(ctx context.Context, userID, id string) error { return nil }
func (fakePlans) ListRoomAdjustments(ctx context.Context, userID, planType string, dek []byte) ([]repo.RoomAdjustment, error) {
	return nil, nil
}
func (fakePlans) CreateRoomAdjustment(ctx context.Context, userID, planType string, year int, amount, note string, dek []byte) (repo.RoomAdjustment, error) {
	return repo.RoomAdjustment{}, nil
}
func (fakePlans) DeleteRoomAdjustment(ctx context.Context, userID, id string) error { return nil }

// --- harness -----------------------------------------------------------------

// newSavingsTest wires the room and birth-year handlers onto fakes with a
// real (in-memory) bootstrap so requireUser's EnsureUser/UnwrapDEK path
// runs unmodified.
func newSavingsTest(t *testing.T, users *fakeUsers) (http.Handler, string) {
	t.Helper()
	key := mintKey(t)
	b, err := NewBootstrap(newMemStore(), testKEK(t), nil)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	accounts := &fakeAccounts{list: []repo.Account{
		{ID: "a-tfsa", Type: "tfsa", Name: "My TFSA", OpeningBalance: "0.00"},
		{ID: "a-rrif", Type: "rrif", Name: "My RRIF", OpeningBalance: "10000.00"},
		{ID: "a-inv", Type: "investment", Name: "Brokerage", OpeningBalance: "250.00"},
		{ID: "a-chk", Type: "checking", Name: "Cheque", OpeningBalance: "999.00"},
	}}
	txns := &fakeTxns{sums: []repo.SavingsSum{
		{AccountID: "a-tfsa", Year: 2026, Contribution: 350000, Employer: 50000},
		{AccountID: "a-tfsa", Year: 2025, Contribution: -200000, GovernmentGrant: 100000},
	}}
	snaps := &fakeSnaps{
		snapshots: map[string][]repo.Snapshot{
			"a-rrif": {
				{ID: "s2", AsOf: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), Balance: "60000.00"},
				{ID: "s1", AsOf: time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC), Balance: "50000.00"},
			},
		},
		equity: map[string][]repo.EquityPoint{
			"a-tfsa": {{Balance: 300000}},
			"a-rrif": {{Balance: 6000000}},
			"a-inv":  {{Balance: 25000}},
			"a-chk":  {{Balance: 99900}},
		},
	}
	h := New(fakeReady{}, BuildInfo{}, liveAuth(t, key), b, nil, accounts, nil, txns, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, false, nil, nil, nil, nil, users, snaps, fakePlans{})
	token := mintIdentityToken(t, key, "savings-sub", "savings@localhost", "Savings", "savingsuser")
	return h, token
}

func doRaw(t *testing.T, h http.Handler, method, path, body, token string) (int, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	b, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(b)
}

// --- response shapes ---------------------------------------------------------

type roomCard struct {
	PlanType          string   `json:"plan_type"`
	Beneficiary       string   `json:"beneficiary"`
	AnnualLimit       *string  `json:"annual_limit"`
	LimitSource       string   `json:"limit_source"`
	Used              string   `json:"used"`
	Room              *string  `json:"room"`
	ReCreatedNextYear string   `json:"re_created_next_year"`
	LifetimeLimit     *string  `json:"lifetime_limit"`
	LifetimeUsed      *string  `json:"lifetime_used"`
	LifetimeRoom      *string  `json:"lifetime_room"`
	RRIFMinimum       *string  `json:"rrif_minimum"`
	RRIFFactorBps     *int64   `json:"rrif_factor_bps"`
	RRIFAge           *int     `json:"rrif_age"`
	Warnings          []string `json:"warnings"`
	Notes             []string `json:"notes"`
}

type roomBody struct {
	Year            int        `json:"year"`
	BirthYear       *int       `json:"birth_year"`
	RegisteredTotal string     `json:"registered_total"`
	InvestmentTotal string     `json:"investment_total"`
	Cards           []roomCard `json:"cards"`
}

func decodeRoom(t *testing.T, body string) roomBody {
	t.Helper()
	var r roomBody
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("decoding room body %s: %v", body, err)
	}
	return r
}

func roomCardFor(t *testing.T, r roomBody, body, plan string) roomCard {
	t.Helper()
	for _, c := range r.Cards {
		if c.PlanType == plan {
			return c
		}
	}
	t.Fatalf("no %q card in %s", plan, body)
	return roomCard{}
}

func wantStrPtr(t *testing.T, got, want *string, name string) {
	t.Helper()
	if want == nil {
		if got != nil {
			t.Errorf("%s = %q, want null", name, *got)
		}
		return
	}
	if got == nil || *got != *want {
		t.Errorf("%s = %v, want %q", name, got, *want)
	}
}

func strp(s string) *string { return &s }
func int64p(v int64) *int64 { return &v }

func wantErrEnvelope(t *testing.T, body, code string) {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decoding error envelope %s: %v", body, err)
	}
	if env.Error.Code != code {
		t.Errorf("error code = %q, want %q (body %s)", env.Error.Code, code, body)
	}
}

// --- tests -------------------------------------------------------------------

func TestSavingsRoom(t *testing.T) {
	by := 1980
	h, token := newSavingsTest(t, &fakeUsers{user: repo.User{ID: "u1", BirthYear: &by}})

	t.Run("year 2026", func(t *testing.T) {
		code, body := doRaw(t, h, http.MethodGet, "/api/v1/savings/room?year=2026", "", token)
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", code, body)
		}
		r := decodeRoom(t, body)
		if r.Year != 2026 {
			t.Errorf("year = %d, want 2026", r.Year)
		}
		if r.BirthYear == nil || *r.BirthYear != 1980 {
			t.Errorf("birth_year = %v, want 1980", r.BirthYear)
		}
		if r.RegisteredTotal != "63000.00" {
			t.Errorf("registered_total = %q, want %q", r.RegisteredTotal, "63000.00")
		}
		if r.InvestmentTotal != "250.00" {
			t.Errorf("investment_total = %q, want %q", r.InvestmentTotal, "250.00")
		}
		if len(r.Cards) != 2 {
			t.Fatalf("cards = %d, want 2 (body %s)", len(r.Cards), body)
		}

		tfsa := roomCardFor(t, r, body, "tfsa")
		wantStrPtr(t, tfsa.AnnualLimit, strp("7000.00"), "tfsa.annual_limit")
		if tfsa.LimitSource != "statutory" {
			t.Errorf("tfsa.limit_source = %q, want %q", tfsa.LimitSource, "statutory")
		}
		if tfsa.Used != "4000.00" {
			t.Errorf("tfsa.used = %q, want %q", tfsa.Used, "4000.00")
		}
		wantStrPtr(t, tfsa.Room, strp("5000.00"), "tfsa.room")
		if tfsa.ReCreatedNextYear != "0.00" {
			t.Errorf("tfsa.re_created_next_year = %q, want %q", tfsa.ReCreatedNextYear, "0.00")
		}
		wantStrPtr(t, tfsa.LifetimeLimit, nil, "tfsa.lifetime_limit")
		if tfsa.RRIFAge != nil {
			t.Errorf("tfsa.rrif_age = %v, want null", *tfsa.RRIFAge)
		}

		rrif := roomCardFor(t, r, body, "rrif")
		wantStrPtr(t, rrif.AnnualLimit, nil, "rrif.annual_limit")
		if rrif.LimitSource != "none" {
			t.Errorf("rrif.limit_source = %q, want %q", rrif.LimitSource, "none")
		}
		if rrif.Used != "0.00" {
			t.Errorf("rrif.used = %q, want %q", rrif.Used, "0.00")
		}
		wantStrPtr(t, rrif.Room, nil, "rrif.room")
		if rrif.ReCreatedNextYear != "0.00" {
			t.Errorf("rrif.re_created_next_year = %q, want %q", rrif.ReCreatedNextYear, "0.00")
		}
		if rrif.RRIFAge == nil || *rrif.RRIFAge != 46 {
			t.Errorf("rrif.rrif_age = %v, want 46", rrif.RRIFAge)
		}
		if rrif.RRIFFactorBps == nil || *rrif.RRIFFactorBps != 227 {
			t.Errorf("rrif.rrif_factor_bps = %v, want 227", rrif.RRIFFactorBps)
		}
		wantStrPtr(t, rrif.RRIFMinimum, strp("1136.36"), "rrif.rrif_minimum")
	})

	t.Run("year 2025", func(t *testing.T) {
		code, body := doRaw(t, h, http.MethodGet, "/api/v1/savings/room?year=2025", "", token)
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", code, body)
		}
		r := decodeRoom(t, body)
		if r.Year != 2025 {
			t.Errorf("year = %d, want 2025", r.Year)
		}
		tfsa := roomCardFor(t, r, body, "tfsa")
		wantStrPtr(t, tfsa.AnnualLimit, strp("7000.00"), "tfsa.annual_limit")
		if tfsa.Used != "0.00" {
			t.Errorf("tfsa.used = %q, want %q", tfsa.Used, "0.00")
		}
		wantStrPtr(t, tfsa.Room, strp("7000.00"), "tfsa.room")
		if tfsa.ReCreatedNextYear != "2000.00" {
			t.Errorf("tfsa.re_created_next_year = %q, want %q", tfsa.ReCreatedNextYear, "2000.00")
		}
	})

	t.Run("default year", func(t *testing.T) {
		code, body := doRaw(t, h, http.MethodGet, "/api/v1/savings/room", "", token)
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", code, body)
		}
		r := decodeRoom(t, body)
		if r.Year != time.Now().UTC().Year() {
			t.Errorf("year = %d, want %d", r.Year, time.Now().UTC().Year())
		}
	})

	for _, ys := range []string{"abc", "1899", "2101"} {
		t.Run("bad year "+ys, func(t *testing.T) {
			code, body := doRaw(t, h, http.MethodGet, "/api/v1/savings/room?year="+ys, "", token)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", code, body)
			}
			wantErrEnvelope(t, body, codeInvalidRequest)
		})
	}
}

func TestPatchBirthYear(t *testing.T) {
	users := &fakeUsers{user: repo.User{ID: "u1"}}
	h, token := newSavingsTest(t, users)
	const patchPath = "/api/v1/me/birth-year"

	// Start nil: the room shows birth_year null.
	code, body := doRaw(t, h, http.MethodGet, "/api/v1/savings/room?year=2026", "", token)
	if code != http.StatusOK {
		t.Fatalf("room status = %d, want 200 (body %s)", code, body)
	}
	if r := decodeRoom(t, body); r.BirthYear != nil {
		t.Fatalf("initial birth_year = %d, want null", *r.BirthYear)
	}

	// Set 1980.
	code, body = doRaw(t, h, http.MethodPatch, patchPath, `{"birth_year":"1980"}`, token)
	if code != http.StatusOK {
		t.Fatalf("patch status = %d, want 200 (body %s)", code, body)
	}
	if r := decodeRoom(t, body); r.BirthYear == nil || *r.BirthYear != 1980 {
		t.Fatalf("patch response birth_year = %v, want 1980", r.BirthYear)
	}
	code, body = doRaw(t, h, http.MethodGet, "/api/v1/savings/room?year=2026", "", token)
	if code != http.StatusOK {
		t.Fatalf("room status = %d, want 200 (body %s)", code, body)
	}
	if r := decodeRoom(t, body); r.BirthYear == nil || *r.BirthYear != 1980 {
		t.Fatalf("room birth_year after set = %v, want 1980", r.BirthYear)
	}

	// Clear to NULL.
	code, body = doRaw(t, h, http.MethodPatch, patchPath, `{"birth_year":""}`, token)
	if code != http.StatusOK {
		t.Fatalf("clear status = %d, want 200 (body %s)", code, body)
	}
	if r := decodeRoom(t, body); r.BirthYear != nil {
		t.Fatalf("clear response birth_year = %d, want null", *r.BirthYear)
	}

	// Absent field: no-op, still null.
	code, body = doRaw(t, h, http.MethodPatch, patchPath, `{}`, token)
	if code != http.StatusOK {
		t.Fatalf("noop status = %d, want 200 (body %s)", code, body)
	}
	if r := decodeRoom(t, body); r.BirthYear != nil {
		t.Fatalf("noop response birth_year = %d, want null (unchanged)", *r.BirthYear)
	}

	// Invalid values: 400, state unchanged.
	for _, v := range []string{"198", "2500"} {
		code, body = doRaw(t, h, http.MethodPatch, patchPath, `{"birth_year":"`+v+`"}`, token)
		if code != http.StatusBadRequest {
			t.Fatalf("patch %q status = %d, want 400 (body %s)", v, code, body)
		}
		wantErrEnvelope(t, body, codeInvalidRequest)
	}
	code, body = doRaw(t, h, http.MethodGet, "/api/v1/savings/room?year=2026", "", token)
	if code != http.StatusOK {
		t.Fatalf("room status = %d, want 200 (body %s)", code, body)
	}
	if r := decodeRoom(t, body); r.BirthYear != nil {
		t.Fatalf("birth_year after invalid patches = %d, want null (unchanged)", *r.BirthYear)
	}
}

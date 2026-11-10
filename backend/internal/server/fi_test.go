// Route tests for the FI institution + credential endpoints (plan
// step 9.1, migration 0010). All in-memory: the fakes mirror the
// repo-layer semantics the handlers depend on (the GLOBAL institution
// registry with the duplicate-code sentinel, the per-user credentials
// scoped by user id — a foreign id is not found — one row per
// (user, institution), the institution code/name joined in, and no
// secrets surfaced), and inject per-operation errors for the 500
// mapping table. The pack encryption path stays with the repo
// testcontainer tests.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"

	"finance-tracker/internal/repo"

	"github.com/lestrrat-go/jwx/v3/jwk"
)

// memInstitutionStore mirrors the repo.InstitutionStore semantics the
// handlers depend on: a GLOBAL registry (no user scoping), the
// duplicate-code sentinel, Create returning the stored row (status
// defaults to "active" like the DB column), and List in code order.
type memInstitutionStore struct {
	rows        []repo.Institution
	createErr   error
	getErr      error
	listErr     error
	createCalls int
}

func (s *memInstitutionStore) Get(_ context.Context, id string) (repo.Institution, error) {
	if s.getErr != nil {
		return repo.Institution{}, s.getErr
	}
	for _, r := range s.rows {
		if r.ID == id {
			return r, nil
		}
	}
	return repo.Institution{}, repo.ErrInstitutionNotFound
}

func (s *memInstitutionStore) GetByCode(_ context.Context, code string) (repo.Institution, error) {
	for _, r := range s.rows {
		if r.Code == code {
			return r, nil
		}
	}
	return repo.Institution{}, repo.ErrInstitutionNotFound
}

func (s *memInstitutionStore) Create(_ context.Context, in repo.InstitutionIn) (repo.Institution, error) {
	if s.createErr != nil {
		return repo.Institution{}, s.createErr
	}
	s.createCalls++
	for _, r := range s.rows {
		if r.Code == in.Code {
			return repo.Institution{}, repo.ErrInstitutionCodeTaken
		}
	}
	row := repo.Institution{ID: nextTestID(), Code: in.Code, Name: in.Name, Status: "active"}
	s.rows = append(s.rows, row)
	return row, nil
}

func (s *memInstitutionStore) List(_ context.Context) ([]repo.Institution, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	rows := append([]repo.Institution(nil), s.rows...)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Code < rows[j].Code })
	return rows, nil
}

// memFICredentialStore mirrors the repo.FICredentialStore semantics:
// rows scoped by user id (a foreign user never sees or deletes
// another user's row), one row per (user, institution), List/Get
// joining the institution code and name, and no secrets surfaced
// (the fakes only store what the handlers pass — already-trimmed
// plaintext, which the real repo seals in packs).
type memFICredentialStore struct {
	inst        *memInstitutionStore
	rows        []repo.Credential
	createErr   error
	getErr      error
	listErr     error
	deleteErr   error
	createCalls int
	deleteCalls int
}

func (s *memFICredentialStore) List(_ context.Context, userID string, _ []byte) ([]repo.Credential, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	rows := []repo.Credential{}
	for _, r := range s.rows {
		if r.UserID == userID {
			rows = append(rows, r)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].InstitutionCode < rows[j].InstitutionCode })
	return rows, nil
}

func (s *memFICredentialStore) Get(_ context.Context, userID, id string, _ []byte) (repo.Credential, error) {
	if s.getErr != nil {
		return repo.Credential{}, s.getErr
	}
	for _, r := range s.rows {
		if r.UserID == userID && r.ID == id {
			return r, nil
		}
	}
	return repo.Credential{}, repo.ErrFICredentialNotFound
}

func (s *memFICredentialStore) Create(_ context.Context, userID string, in repo.FICredentialIn, _ []byte) (repo.Credential, error) {
	if s.createErr != nil {
		return repo.Credential{}, s.createErr
	}
	s.createCalls++
	for _, r := range s.rows {
		if r.UserID == userID && r.InstitutionID == in.InstitutionID {
			return repo.Credential{}, repo.ErrFICredentialExists
		}
	}
	inst, err := s.inst.Get(context.Background(), in.InstitutionID)
	if err != nil {
		return repo.Credential{}, err
	}
	row := repo.Credential{
		ID:              nextTestID(),
		UserID:          userID,
		InstitutionID:   in.InstitutionID,
		InstitutionCode: inst.Code,
		InstitutionName: inst.Name,
	}
	s.rows = append(s.rows, row)
	return row, nil
}

func (s *memFICredentialStore) Delete(_ context.Context, userID, id string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.deleteCalls++
	for i, r := range s.rows {
		if r.UserID == userID && r.ID == id {
			s.rows = append(s.rows[:i], s.rows[i+1:]...)
			return nil
		}
	}
	return repo.ErrFICredentialNotFound
}

// newFIServer builds a full router with a live token verifier, an
// in-memory bootstrap, and the given FI stores (both non-nil here; the
// 503 nil-store path is shared with the other store tests). The key is
// returned so tests can mint tokens for several users under the same
// verifier.
func newFIServer(t *testing.T, inst InstitutionStore, creds FICredentialStore) (http.Handler, jwk.Key) {
	t.Helper()
	key := mintKey(t)
	b, err := NewBootstrap(newMemStore(), testKEK(t), nil)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	h := New(fakeReady{}, BuildInfo{}, liveAuth(t, key), b, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, false, inst, creds, nil)
	return h, key
}

// wantInstitutionKeys / wantCredentialKeys are the pinned wire field
// orders (the DTO field order is part of the contract; a map decode
// would lose it).
var wantInstitutionKeys = []string{"id", "code", "name", "status"}

var wantCredentialKeys = []string{
	"id", "institution_id", "institution_code", "institution_name", "last_synced_at",
}

// jsonArrayKeyOrders returns the top-level key order of every element
// of a JSON array body (each element is a JSON object, in wire order).
func jsonArrayKeyOrders(t *testing.T, body string) [][]string {
	t.Helper()
	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("body is not a JSON array: %v (%s)", err, body)
	}
	orders := make([][]string, 0, len(raw))
	for _, el := range raw {
		orders = append(orders, jsonKeyOrder(t, string(el)))
	}
	return orders
}

// TestFIInstitutions covers the institution registry routes: fresh
// list (body exactly []), create (201, the name stored trimmed, the
// EXACT wire key order, status "active"), list in code order (the
// pinned key order on every element), 409 duplicate code, the 400
// validation table (every problem "; "-joined, one 400 per request),
// and the pinned 500 mapping.
func TestFIInstitutions(t *testing.T) {
	inst := &memInstitutionStore{}
	creds := &memFICredentialStore{inst: inst}
	h, key := newFIServer(t, inst, creds)
	hdr := authHdr(mintIdentityToken(t, key, "fi-inst", "fi-inst@localhost", "fi-inst", "fi-inst"))

	// Fresh registry: the list body is exactly [] (not null).
	code, out := doReq(t, h, http.MethodGet, "/api/v1/fi/institutions", "", hdr)
	if code != http.StatusOK || strings.TrimSpace(out) != "[]" {
		t.Fatalf("fresh list = %d %s, want 200 []", code, out)
	}

	// Create: the name is stored trimmed; the body has the EXACT key
	// order and the DB-default status.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/fi/institutions",
		`{"code":"zeta-bank","name":"  Zeta Bank  "}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("create = %d (body %s), want 201", code, out)
	}
	keys := jsonKeyOrder(t, out)
	if !reflect.DeepEqual(keys, wantInstitutionKeys) {
		t.Fatalf("create key order = %v, want %v", keys, wantInstitutionKeys)
	}
	var zeta struct {
		ID     string `json:"id"`
		Code   string `json:"code"`
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &zeta); err != nil {
		t.Fatalf("unmarshal create body: %v", err)
	}
	if zeta.Code != "zeta-bank" || zeta.Name != "Zeta Bank" || zeta.Status != "active" {
		t.Fatalf("create row = %+v, want zeta-bank / Zeta Bank (trimmed) / active", zeta)
	}

	// A code that sorts first: the list is in CODE order, with the
	// pinned key order on every element.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/fi/institutions",
		`{"code":"alpha-savings","name":"Alpha Savings"}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("create alpha = %d (body %s), want 201", code, out)
	}
	code, out = doReq(t, h, http.MethodGet, "/api/v1/fi/institutions", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("list = %d (body %s), want 200", code, out)
	}
	orders := jsonArrayKeyOrders(t, out)
	if !reflect.DeepEqual(orders, [][]string{wantInstitutionKeys, wantInstitutionKeys}) {
		t.Fatalf("list key orders = %v, want %v", orders, wantInstitutionKeys)
	}
	var list []struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(list) != 2 || list[0].Code != "alpha-savings" || list[1].Code != "zeta-bank" {
		t.Fatalf("list order = %v, want alpha-savings then zeta-bank", list)
	}

	// Duplicate code: 409, no second row.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/fi/institutions",
		`{"code":"zeta-bank","name":"Zeta Bank Again"}`, hdr)
	expectAPIError(t, code, out, http.StatusConflict, codeConflict, "institution code already exists")
	if len(inst.rows) != 2 {
		t.Fatalf("rows = %d, want 2 (the duplicate must not be stored)", len(inst.rows))
	}

	// Code-shape violations (the name is valid): each is the exact
	// pinned 400 message.
	badCode := "institution code must be 1-64 characters of lowercase letters, digits, and hyphens, starting and ending with a letter or digit"
	for _, tc := range []struct {
		name string
		code string
	}{
		{"uppercase", "Alpha"},
		{"leading hyphen", "-alpha"},
		{"65 chars", strings.Repeat("a", 65)},
	} {
		t.Run("bad code "+tc.name, func(t *testing.T) {
			code, out := doReq(t, h, http.MethodPost, "/api/v1/fi/institutions",
				`{"code":"`+tc.code+`","name":"Valid Name"}`, hdr)
			expectAPIError(t, code, out, http.StatusBadRequest, codeInvalidRequest, badCode)
		})
	}

	// Blank name: the exact pinned 400 message.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/fi/institutions",
		`{"code":"beta","name":"   "}`, hdr)
	expectAPIError(t, code, out, http.StatusBadRequest, codeInvalidRequest, "institution name must be 1-120 characters")

	// Both problems in one request: "; "-joined, still one 400.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/fi/institutions",
		`{"code":"Bad","name":"   "}`, hdr)
	expectAPIError(t, code, out, http.StatusBadRequest, codeInvalidRequest,
		badCode+"; institution name must be 1-120 characters")

	// A non-sentinel store error maps to the pinned 500.
	inst.createErr = errors.New("boom")
	code, out = doReq(t, h, http.MethodPost, "/api/v1/fi/institutions",
		`{"code":"omega","name":"Omega"}`, hdr)
	expectAPIError(t, code, out, http.StatusInternalServerError, codeInternal, "fi institution operation failed")
	inst.createErr = nil
}

// TestFICredentials covers the credential routes: create (201, the
// EXACT wire key order, the institution code/name joined in,
// last_synced_at null, NO secret keys on the wire), list in
// institution-code order, 409 duplicate (user, institution), the 400
// validation table (every problem "; "-joined), 404 unknown
// institution (the credential store is never reached), delete (204
// with an empty body, 404 on repeat, a malformed id never reaches the
// store), foreign-user scoping (their list is exactly [] and deleting
// the other user's row is a 404), and the pinned 500 mapping.
func TestFICredentials(t *testing.T) {
	inst := &memInstitutionStore{}
	creds := &memFICredentialStore{inst: inst}
	h, key := newFIServer(t, inst, creds)
	hdrA := authHdr(mintIdentityToken(t, key, "fi-a", "fi-a@localhost", "fi-a", "fi-a"))
	hdrB := authHdr(mintIdentityToken(t, key, "fi-b", "fi-b@localhost", "fi-b", "fi-b"))

	// Seed two institutions (alphabetically apart) and keep their ids.
	code, out := doReq(t, h, http.MethodPost, "/api/v1/fi/institutions",
		`{"code":"zeta-bank","name":"Zeta Bank"}`, hdrA)
	if code != http.StatusCreated {
		t.Fatalf("seed zeta = %d (body %s), want 201", code, out)
	}
	zetaID := idFrom(t, out)
	code, out = doReq(t, h, http.MethodPost, "/api/v1/fi/institutions",
		`{"code":"alpha-savings","name":"Alpha Savings"}`, hdrA)
	if code != http.StatusCreated {
		t.Fatalf("seed alpha = %d (body %s), want 201", code, out)
	}
	alphaID := idFrom(t, out)

	// Fresh list: exactly [] (not null).
	code, out = doReq(t, h, http.MethodGet, "/api/v1/fi/credentials", "", hdrA)
	if code != http.StatusOK || strings.TrimSpace(out) != "[]" {
		t.Fatalf("fresh list = %d %s, want 200 []", code, out)
	}

	// Create: the body has the EXACT key order, the joined institution
	// code+name, last_synced_at null, and NO secret keys.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/fi/credentials",
		`{"institution_id":"`+zetaID+`","token":"  tok-1  ","refresh_token":"refresh-1"}`, hdrA)
	if code != http.StatusCreated {
		t.Fatalf("create = %d (body %s), want 201", code, out)
	}
	keys := jsonKeyOrder(t, out)
	if !reflect.DeepEqual(keys, wantCredentialKeys) {
		t.Fatalf("create key order = %v, want %v", keys, wantCredentialKeys)
	}
	var zetaCred struct {
		InstitutionCode string `json:"institution_code"`
		InstitutionName string `json:"institution_name"`
	}
	if err := json.Unmarshal([]byte(out), &zetaCred); err != nil {
		t.Fatalf("unmarshal create body: %v", err)
	}
	if zetaCred.InstitutionCode != "zeta-bank" || zetaCred.InstitutionName != "Zeta Bank" {
		t.Fatalf("create join = %q %q, want zeta-bank / Zeta Bank", zetaCred.InstitutionCode, zetaCred.InstitutionName)
	}
	if !strings.Contains(out, `"last_synced_at":null`) {
		t.Fatalf("create body %s lacks last_synced_at null", out)
	}
	zetaCredID := idFrom(t, out)

	// Second credential (a different institution, same user): the list
	// is in institution-CODE order (alpha before zeta) with the join
	// fields and the pinned key order on every element.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/fi/credentials",
		`{"institution_id":"`+alphaID+`","token":"tok-2","refresh_token":"refresh-2"}`, hdrA)
	if code != http.StatusCreated {
		t.Fatalf("create alpha cred = %d (body %s), want 201", code, out)
	}
	alphaCredID := idFrom(t, out)
	code, out = doReq(t, h, http.MethodGet, "/api/v1/fi/credentials", "", hdrA)
	if code != http.StatusOK {
		t.Fatalf("list = %d (body %s), want 200", code, out)
	}
	orders := jsonArrayKeyOrders(t, out)
	if !reflect.DeepEqual(orders, [][]string{wantCredentialKeys, wantCredentialKeys}) {
		t.Fatalf("list key orders = %v, want %v", orders, wantCredentialKeys)
	}
	var list []struct {
		ID              string `json:"id"`
		InstitutionCode string `json:"institution_code"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(list) != 2 || list[0].InstitutionCode != "alpha-savings" || list[0].ID != alphaCredID ||
		list[1].InstitutionCode != "zeta-bank" || list[1].ID != zetaCredID {
		t.Fatalf("list order = %+v, want alpha-savings then zeta-bank", list)
	}

	// Duplicate (user, institution): 409.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/fi/credentials",
		`{"institution_id":"`+zetaID+`","token":"tok-1b","refresh_token":"refresh-1b"}`, hdrA)
	expectAPIError(t, code, out, http.StatusConflict, codeConflict, "a credential for this institution already exists")

	// Empty token: the exact pinned 400 message.
	tokenMsg := "token must be 1-4096 characters"
	code, out = doReq(t, h, http.MethodPost, "/api/v1/fi/credentials",
		`{"institution_id":"`+alphaID+`","token":"   ","refresh_token":"r"}`, hdrA)
	expectAPIError(t, code, out, http.StatusBadRequest, codeInvalidRequest, tokenMsg)

	// Oversized token (4097 chars): the same pinned 400 message.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/fi/credentials",
		`{"institution_id":"`+alphaID+`","token":"`+strings.Repeat("a", 4097)+`","refresh_token":"r"}`, hdrA)
	expectAPIError(t, code, out, http.StatusBadRequest, codeInvalidRequest, tokenMsg)

	// Malformed institution_id: the exact pinned 400 message.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/fi/credentials",
		`{"institution_id":"not-a-uuid","token":"tok","refresh_token":"r"}`, hdrA)
	expectAPIError(t, code, out, http.StatusBadRequest, codeInvalidRequest, "institution_id must be a valid UUID")

	// All three problems in one request: "; "-joined, still one 400.
	code, out = doReq(t, h, http.MethodPost, "/api/v1/fi/credentials",
		`{"institution_id":"not-a-uuid","token":"","refresh_token":""}`, hdrA)
	expectAPIError(t, code, out, http.StatusBadRequest, codeInvalidRequest,
		"institution_id must be a valid UUID; token must be 1-4096 characters; refresh_token must be 1-4096 characters")

	// A well-formed UUID that matches no institution: 404, and the
	// credential store is never reached.
	unknownID := nextTestID()
	before := creds.createCalls
	code, out = doReq(t, h, http.MethodPost, "/api/v1/fi/credentials",
		`{"institution_id":"`+unknownID+`","token":"tok","refresh_token":"r"}`, hdrA)
	expectAPIError(t, code, out, http.StatusNotFound, codeNotFound, "institution not found")
	if creds.createCalls != before {
		t.Fatalf("credential create attempted on an unknown institution (calls %d -> %d)", before, creds.createCalls)
	}

	// A non-sentinel store error maps to the pinned 500.
	creds.createErr = errors.New("boom")
	code, out = doReq(t, h, http.MethodPost, "/api/v1/fi/credentials",
		`{"institution_id":"`+alphaID+`","token":"tok","refresh_token":"r"}`, hdrA)
	expectAPIError(t, code, out, http.StatusInternalServerError, codeInternal, "fi credential operation failed")
	creds.createErr = nil

	// Delete: 204 with an empty body; the repeat is a 404.
	code, out = doReq(t, h, http.MethodDelete, "/api/v1/fi/credentials/"+zetaCredID, "", hdrA)
	if code != http.StatusNoContent || out != "" {
		t.Fatalf("delete = %d %q, want 204 with an empty body", code, out)
	}
	code, out = doReq(t, h, http.MethodDelete, "/api/v1/fi/credentials/"+zetaCredID, "", hdrA)
	expectAPIError(t, code, out, http.StatusNotFound, codeNotFound, "fi credential not found")

	// A malformed id is a 404 without reaching the store.
	before = creds.deleteCalls
	code, out = doReq(t, h, http.MethodDelete, "/api/v1/fi/credentials/not-a-uuid", "", hdrA)
	expectAPIError(t, code, out, http.StatusNotFound, codeNotFound, "fi credential not found")
	if creds.deleteCalls != before {
		t.Fatalf("store delete called for a malformed id (calls %d -> %d)", before, creds.deleteCalls)
	}

	// Foreign user: their list is exactly [] and deleting the other
	// user's remaining credential is a 404 (no existence disclosure).
	code, out = doReq(t, h, http.MethodGet, "/api/v1/fi/credentials", "", hdrB)
	if code != http.StatusOK || strings.TrimSpace(out) != "[]" {
		t.Fatalf("foreign list = %d %s, want 200 []", code, out)
	}
	code, out = doReq(t, h, http.MethodDelete, "/api/v1/fi/credentials/"+alphaCredID, "", hdrB)
	expectAPIError(t, code, out, http.StatusNotFound, codeNotFound, "fi credential not found")
}

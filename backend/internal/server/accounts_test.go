// Route tests for the account endpoints (plan step 3.1). All in-memory:
// memAccountStore mirrors the repo.AccountStore semantics the handlers
// depend on (user-id scoping, per-user case-sensitive uniqueness on the
// trimmed name, subtype trim-to-NULL, no-op patches) and injects
// per-operation errors for the mapping table; the crypto and
// schema-visibility paths stay with the 2.5a/2.6b testcontainer tests.
// This file also defines the shared helpers (newCrudServer, authHdr,
// apiErr, idFrom, nextTestID) that categories_test.go reuses.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"finance-tracker/internal/repo"

	"github.com/lestrrat-go/jwx/v3/jwk"
)

// nextTestID mints a UUID-shaped id (36 chars, dashes at 8/13/18/23) that
// passes uuidShapeOK. It never matches a real row, so tests can use it in
// request paths without touching store state.
var testIDCounter atomic.Int64

func nextTestID() string {
	return fmt.Sprintf("00000000-0000-0000-0000-%012d", testIDCounter.Add(1))
}

// newCrudServer builds a full router with a live token verifier, an
// in-memory bootstrap, and the given stores (nil allowed — that is the
// 503 path). The key is returned so tests can mint tokens for several
// users under the same verifier.
func newCrudServer(t *testing.T, accounts AccountStore, categories CategoryStore) (http.Handler, jwk.Key) {
	t.Helper()
	key := mintKey(t)
	b, err := NewBootstrap(newMemStore(), testKEK(t))
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return New(fakeReady{}, BuildInfo{}, liveAuth(t, key), b, nil, accounts, categories, nil, nil), key
}

// authHdr is the Authorization header for a minted access token.
func authHdr(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

// apiErr decodes an error envelope and returns its code and message.
func apiErr(t *testing.T, body string) (string, string) {
	t.Helper()
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode error envelope: %v (body %s)", err, body)
	}
	return env.Error.Code, env.Error.Message
}

// idFrom pulls the "id" field out of a created/updated row body.
func idFrom(t *testing.T, body string) string {
	t.Helper()
	var row map[string]any
	if err := json.Unmarshal([]byte(body), &row); err != nil {
		t.Fatalf("decode row: %v (body %s)", err, body)
	}
	id, _ := row["id"].(string)
	if id == "" {
		t.Fatalf("row has no id (body %s)", body)
	}
	return id
}

// memAccountRow is one fake-store row. name is stored trimmed (the repo
// trims on write) and is compared case-sensitively (the repo's name hash
// is exact-case); "" subtype/schema means the NULL column.
type memAccountRow struct {
	userID   string
	id       string
	name     string
	typ      string
	subtype  string
	currency string
	schema   string
}

func toRepoAccount(r memAccountRow) repo.Account {
	return repo.Account{
		ID:              r.id,
		Name:            r.name,
		Type:            r.typ,
		Subtype:         r.subtype,
		Currency:        r.currency,
		DefaultSchemaID: r.schema,
	}
}

// memAccountStore is the in-memory AccountStore the route tests run
// against. It mirrors the repo semantics the handlers rely on — scoping
// on user id, per-user case-sensitive uniqueness of the trimmed name,
// subtype trim-to-NULL, no-op patches — and can inject a per-operation
// error to exercise the handler's error mapping (real failures need a DB).
type memAccountStore struct {
	rows      []memAccountRow
	listErr   error
	createErr error
	updateErr error
	deleteErr error
}

func (m *memAccountStore) find(userID, id string) (memAccountRow, bool) {
	for _, r := range m.rows {
		if r.userID == userID && r.id == id {
			return r, true
		}
	}
	return memAccountRow{}, false
}

func (m *memAccountStore) List(_ context.Context, userID string, _ []byte) ([]repo.Account, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	out := make([]repo.Account, 0, len(m.rows))
	for _, r := range m.rows { // insertion order = the repo's ORDER BY created_at, id
		if r.userID == userID {
			out = append(out, toRepoAccount(r))
		}
	}
	return out, nil
}

func (m *memAccountStore) Get(_ context.Context, userID, id string, _ []byte) (repo.Account, error) {
	r, ok := m.find(userID, id)
	if !ok {
		return repo.Account{}, repo.ErrAccountNotFound
	}
	return toRepoAccount(r), nil
}

func (m *memAccountStore) Create(_ context.Context, userID string, in repo.AccountIn, _ []byte) (repo.Account, error) {
	if m.createErr != nil {
		return repo.Account{}, m.createErr
	}
	name := strings.TrimSpace(in.Name)
	for _, r := range m.rows {
		if r.userID == userID && r.name == name {
			return repo.Account{}, repo.ErrNameTaken
		}
	}
	var (
		subtype string
		schema  string
	)
	if in.Subtype != nil {
		subtype = strings.TrimSpace(*in.Subtype) // trim to "" = NULL (repo)
	}
	if in.DefaultSchemaID != nil && *in.DefaultSchemaID != "" {
		schema = *in.DefaultSchemaID
	}
	id := nextTestID()
	m.rows = append(m.rows, memAccountRow{
		userID: userID, id: id, name: name, typ: in.Type,
		subtype: subtype, currency: in.Currency, schema: schema,
	})
	return toRepoAccount(m.rows[len(m.rows)-1]), nil
}

func (m *memAccountStore) Update(_ context.Context, userID, id string, p repo.AccountPatch, _ []byte) (repo.Account, error) {
	if m.updateErr != nil {
		return repo.Account{}, m.updateErr
	}
	idx := -1
	for i, r := range m.rows {
		if r.userID == userID && r.id == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return repo.Account{}, repo.ErrAccountNotFound
	}
	r := m.rows[idx]
	if p.Name != nil {
		name := strings.TrimSpace(*p.Name)
		for _, o := range m.rows {
			if o.userID == userID && o.id != id && o.name == name {
				return repo.Account{}, repo.ErrNameTaken
			}
		}
		r.name = name
	}
	if p.Type != nil {
		r.typ = *p.Type
	}
	if p.Currency != nil {
		r.currency = *p.Currency
	}
	if p.Subtype != nil {
		if inner := *p.Subtype; inner != nil {
			r.subtype = strings.TrimSpace(*inner) // trim to "" = NULL (repo)
		} else {
			r.subtype = ""
		}
	}
	if p.DefaultSchemaID != nil {
		if inner := *p.DefaultSchemaID; inner != nil && *inner != "" {
			r.schema = *inner
		} else {
			r.schema = "" // inner-nil or "" = NULL (repo)
		}
	}
	m.rows[idx] = r
	return toRepoAccount(r), nil
}

func (m *memAccountStore) Delete(_ context.Context, userID, id string) error {
	if m.deleteErr != nil {
		return m.deleteErr
	}
	for i, r := range m.rows {
		if r.userID == userID && r.id == id {
			m.rows = append(m.rows[:i], m.rows[i+1:]...)
			return nil
		}
	}
	return repo.ErrAccountNotFound
}

func TestAccountsCRUD(t *testing.T) {
	store := &memAccountStore{}
	h, key := newCrudServer(t, store, nil)
	token := mintIdentityToken(t, key, "crud-sub", "crud@localhost", "Crud", "crud")

	// An empty list is [] (a JSON array, not null). writeJSON's encoder
	// appends a trailing newline, so compare trimmed.
	code, body := doReq(t, h, http.MethodGet, "/api/v1/accounts", "", authHdr(token))
	if code != http.StatusOK {
		t.Fatalf("GET accounts status = %d, want 200 (body %s)", code, body)
	}
	if strings.TrimSpace(body) != "[]" {
		t.Fatalf("empty list = %s, want []", body)
	}

	// POST trims the name and subtype, answers 201, and JSON-nulls
	// nothing that was set.
	schemaID := nextTestID()
	code, body = doReq(t, h, http.MethodPost, "/api/v1/accounts",
		fmt.Sprintf(`{"name":"  Main Checking  ","type":"bank","currency":"USD","subtype":"  cheques  ","default_schema_id":"%s"}`, schemaID),
		authHdr(token))
	if code != http.StatusCreated {
		t.Fatalf("POST accounts status = %d, want 201 (body %s)", code, body)
	}
	var created map[string]any
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("decode created account: %v (body %s)", err, body)
	}
	if created["name"] != "Main Checking" {
		t.Errorf("created name = %v, want trimmed \"Main Checking\"", created["name"])
	}
	if created["subtype"] != "cheques" {
		t.Errorf("created subtype = %v, want trimmed \"cheques\"", created["subtype"])
	}
	if created["default_schema_id"] != schemaID {
		t.Errorf("created default_schema_id = %v, want %s", created["default_schema_id"], schemaID)
	}
	id := idFrom(t, body)
	if !uuidShapeOK(id) {
		t.Fatalf("created id %q is not UUID-shaped", id)
	}

	// A duplicate (trimmed, exact-case) is 409 conflict.
	code, body = doReq(t, h, http.MethodPost, "/api/v1/accounts",
		`{"name":"  Main Checking","type":"bank","currency":"USD"}`, authHdr(token))
	if code != http.StatusConflict {
		t.Fatalf("duplicate POST status = %d, want 409 (body %s)", code, body)
	}
	c, msg := apiErr(t, body)
	if c != "conflict" || msg != "an account with this name already exists" {
		t.Errorf("envelope = %s/%s, want conflict/an account with this name already exists", c, msg)
	}

	// The list reflects the created row.
	code, body = doReq(t, h, http.MethodGet, "/api/v1/accounts", "", authHdr(token))
	if code != http.StatusOK {
		t.Fatalf("GET accounts status = %d, want 200 (body %s)", code, body)
	}
	var list []map[string]any
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode list: %v (body %s)", err, body)
	}
	if len(list) != 1 || list[0]["id"] != id || list[0]["name"] != "Main Checking" {
		t.Fatalf("list = %s, want the one created row", body)
	}

	// PATCH renames and leaves the other fields untouched.
	code, body = doReq(t, h, http.MethodPatch, "/api/v1/accounts/"+id, `{"name":"Main Chequing"}`, authHdr(token))
	if code != http.StatusOK {
		t.Fatalf("PATCH accounts status = %d, want 200 (body %s)", code, body)
	}
	if got := idFrom(t, body); got != id {
		t.Errorf("patched id = %s, want %s", got, id)
	}
	var patched map[string]any
	if err := json.Unmarshal([]byte(body), &patched); err != nil {
		t.Fatalf("decode patched: %v (body %s)", err, body)
	}
	if patched["name"] != "Main Chequing" || patched["subtype"] != "cheques" || patched["currency"] != "USD" {
		t.Errorf("patched = %s (subtype/currency must be untouched)", body)
	}

	// DELETE answers 204 with an empty body.
	code, body = doReq(t, h, http.MethodDelete, "/api/v1/accounts/"+id, "", authHdr(token))
	if code != http.StatusNoContent {
		t.Fatalf("DELETE accounts status = %d, want 204", code)
	}
	if body != "" {
		t.Errorf("DELETE body = %q, want empty", body)
	}

	// The list is empty again.
	code, body = doReq(t, h, http.MethodGet, "/api/v1/accounts", "", authHdr(token))
	if code != http.StatusOK || strings.TrimSpace(body) != "[]" {
		t.Fatalf("GET after delete = %d %s, want 200 []", code, body)
	}
}

func TestAccountTriStatePatch(t *testing.T) {
	store := &memAccountStore{}
	h, key := newCrudServer(t, store, nil)
	token := mintIdentityToken(t, key, "tri-sub", "tri@localhost", "Tri", "tri")

	schemaID := nextTestID()
	code, body := doReq(t, h, http.MethodPost, "/api/v1/accounts",
		fmt.Sprintf(`{"name":"T","type":"bank","currency":"USD","subtype":"cheques","default_schema_id":"%s"}`, schemaID),
		authHdr(token))
	if code != http.StatusCreated {
		t.Fatalf("POST accounts status = %d, want 201 (body %s)", code, body)
	}
	id := idFrom(t, body)

	patch := func(t *testing.T, payload string) map[string]any {
		code, body := doReq(t, h, http.MethodPatch, "/api/v1/accounts/"+id, payload, authHdr(token))
		if code != http.StatusOK {
			t.Fatalf("PATCH %s status = %d (body %s)", payload, code, body)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode patched: %v (body %s)", err, body)
		}
		return out
	}

	t.Run("absent leaves the column untouched", func(t *testing.T) {
		out := patch(t, `{"name":"T2"}`)
		if out["subtype"] != "cheques" {
			t.Errorf("subtype = %v, want untouched \"cheques\"", out["subtype"])
		}
		if out["default_schema_id"] != schemaID {
			t.Errorf("default_schema_id = %v, want untouched", out["default_schema_id"])
		}
	})

	t.Run("string sets the column", func(t *testing.T) {
		out := patch(t, `{"subtype":"direct"}`)
		if out["subtype"] != "direct" {
			t.Errorf("subtype = %v, want \"direct\"", out["subtype"])
		}
		if out["default_schema_id"] != schemaID {
			t.Errorf("default_schema_id = %v, want untouched", out["default_schema_id"])
		}
	})

	t.Run("null clears the column", func(t *testing.T) {
		out := patch(t, `{"subtype":null}`)
		if out["subtype"] != nil {
			t.Errorf("subtype = %v, want null", out["subtype"])
		}
		out = patch(t, `{"default_schema_id":null}`)
		if out["default_schema_id"] != nil {
			t.Errorf("default_schema_id = %v, want null", out["default_schema_id"])
		}
	})

	t.Run("nulls in one patch clear both", func(t *testing.T) {
		out := patch(t, fmt.Sprintf(`{"subtype":"x","default_schema_id":"%s"}`, schemaID))
		if out["subtype"] != "x" || out["default_schema_id"] != schemaID {
			t.Fatalf("re-set failed: %v", out)
		}
		out = patch(t, `{"subtype":null,"default_schema_id":null}`)
		if out["subtype"] != nil || out["default_schema_id"] != nil {
			t.Errorf("want both null, got %v", out)
		}
	})
}

func TestAccountValidation(t *testing.T) {
	store := &memAccountStore{}
	h, key := newCrudServer(t, store, nil)
	token := mintIdentityToken(t, key, "val-sub", "val@localhost", "Val", "val")

	longName := strings.Repeat("n", 256)
	longSubtype := strings.Repeat("s", 65)
	patchID := nextTestID()

	cases := []struct {
		name   string
		method string
		path   string
		body   string
		want   string // substring of the message
	}{
		{"empty name", http.MethodPost, "/api/v1/accounts",
			`{"name":"   ","type":"bank","currency":"USD"}`, "name must not be empty"},
		{"long name", http.MethodPost, "/api/v1/accounts",
			`{"name":"` + longName + `","type":"bank","currency":"USD"}`, "name must be at most 255 characters"},
		{"bad type", http.MethodPost, "/api/v1/accounts",
			`{"name":"A","type":"vault","currency":"USD"}`, "type must be one of"},
		{"lowercase currency", http.MethodPost, "/api/v1/accounts",
			`{"name":"A","type":"bank","currency":"usd"}`, "currency must be a 3-letter uppercase ISO-4217 code"},
		{"long subtype", http.MethodPost, "/api/v1/accounts",
			`{"name":"A","type":"bank","currency":"USD","subtype":"` + longSubtype + `"}`, "subtype must be at most 64 characters"},
		{"bad schema id", http.MethodPost, "/api/v1/accounts",
			`{"name":"A","type":"bank","currency":"USD","default_schema_id":"nope"}`, "default_schema_id must be a valid UUID"},
		{"non-JSON body", http.MethodPost, "/api/v1/accounts",
			`not json`, "body must be a JSON object"},
		{"array body", http.MethodPost, "/api/v1/accounts",
			`[]`, "body must be a JSON object"},
		{"subtype wrong type", http.MethodPost, "/api/v1/accounts",
			`{"name":"A","type":"bank","currency":"USD","subtype":42}`, "body must be a JSON object"},
		{"patch empty name", http.MethodPatch, "/api/v1/accounts/" + patchID,
			`{"name":""}`, "name must not be empty"},
		{"patch bad type", http.MethodPatch, "/api/v1/accounts/" + patchID,
			`{"type":"vault"}`, "type must be one of"},
		{"patch bad currency", http.MethodPatch, "/api/v1/accounts/" + patchID,
			`{"currency":"US"}`, "currency must be a 3-letter uppercase ISO-4217 code"},
		{"patch long subtype", http.MethodPatch, "/api/v1/accounts/" + patchID,
			`{"subtype":"` + longSubtype + `"}`, "subtype must be at most 64 characters"},
		{"patch bad schema id", http.MethodPatch, "/api/v1/accounts/" + patchID,
			`{"default_schema_id":"nope"}`, "default_schema_id must be a valid UUID"},
		{"patch subtype wrong type", http.MethodPatch, "/api/v1/accounts/" + patchID,
			`{"subtype":42}`, "body must be a JSON object"},
		{"empty patch", http.MethodPatch, "/api/v1/accounts/" + patchID,
			`{}`, "empty patch: provide at least one field"},
		{"unknown-only patch", http.MethodPatch, "/api/v1/accounts/" + patchID,
			`{"bogus":1}`, "empty patch: provide at least one field"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := doReq(t, h, tc.method, tc.path, tc.body, authHdr(token))
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", code, body)
			}
			c, msg := apiErr(t, body)
			if c != "invalid_request" {
				t.Errorf("code = %s, want invalid_request", c)
			}
			if !strings.Contains(msg, tc.want) {
				t.Errorf("message %q does not contain %q", msg, tc.want)
			}
		})
	}

	// A POST with several problems reports them all, joined with "; ".
	code, body := doReq(t, h, http.MethodPost, "/api/v1/accounts", `{}`, authHdr(token))
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", code, body)
	}
	_, msg := apiErr(t, body)
	for _, want := range []string{"name must not be empty", "type must be one of", "currency must be", "; "} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not contain %q (all problems must be joined)", msg, want)
		}
	}
}

func TestAccountsMalformedID(t *testing.T) {
	// If a malformed id ever reached the store, updateErr/deleteErr would
	// make the handler answer 500; a 404 proves the shape check runs first.
	store := &memAccountStore{updateErr: errors.New("boom"), deleteErr: errors.New("boom")}
	h, key := newCrudServer(t, store, nil)
	token := mintIdentityToken(t, key, "mal-sub", "mal@localhost", "Mal", "mal")

	for _, id := range []string{"not-a-uuid", "123", strings.Repeat("0", 35)} {
		for _, method := range []string{http.MethodPatch, http.MethodDelete} {
			body := ""
			if method == http.MethodPatch {
				body = `{"name":"X"}`
			}
			code, out := doReq(t, h, method, "/api/v1/accounts/"+id, body, authHdr(token))
			if code != http.StatusNotFound {
				t.Fatalf("%s /%s status = %d, want 404 (body %s)", method, id, code, out)
			}
			c, msg := apiErr(t, out)
			if c != "not_found" || msg != "account not found" {
				t.Errorf("%s /%s envelope = %s/%s, want not_found/account not found", method, id, c, msg)
			}
		}
	}
}

func TestAccountErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		method string
		err    error
		status int
		code   string
		msg    string
	}{
		{"create name taken", http.MethodPost, repo.ErrNameTaken,
			http.StatusConflict, "conflict", "an account with this name already exists"},
		{"create schema not found", http.MethodPost, repo.ErrSchemaNotFound,
			http.StatusBadRequest, "invalid_request", "default schema not found"},
		{"create other", http.MethodPost, errors.New("boom"),
			http.StatusInternalServerError, "internal", "account operation failed"},
		{"update not found", http.MethodPatch, repo.ErrAccountNotFound,
			http.StatusNotFound, "not_found", "account not found"},
		{"update other", http.MethodPatch, errors.New("boom"),
			http.StatusInternalServerError, "internal", "account operation failed"},
		{"delete not found", http.MethodDelete, repo.ErrAccountNotFound,
			http.StatusNotFound, "not_found", "account not found"},
		{"delete other", http.MethodDelete, errors.New("boom"),
			http.StatusInternalServerError, "internal", "account operation failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &memAccountStore{}
			switch tc.method {
			case http.MethodPost:
				store.createErr = tc.err
			case http.MethodPatch:
				store.updateErr = tc.err
			case http.MethodDelete:
				store.deleteErr = tc.err
			}
			h, key := newCrudServer(t, store, nil)
			token := mintIdentityToken(t, key, "map-sub", "map@localhost", "Map", "map")
			var (
				path string
				body string
			)
			switch tc.method {
			case http.MethodPost:
				path = "/api/v1/accounts"
				body = `{"name":"X","type":"bank","currency":"USD"}`
			case http.MethodPatch:
				path = "/api/v1/accounts/" + nextTestID()
				body = `{"name":"X"}`
			case http.MethodDelete:
				path = "/api/v1/accounts/" + nextTestID()
			}
			code, out := doReq(t, h, tc.method, path, body, authHdr(token))
			if code != tc.status {
				t.Fatalf("status = %d, want %d (body %s)", code, tc.status, out)
			}
			c, msg := apiErr(t, out)
			if c != tc.code || msg != tc.msg {
				t.Errorf("envelope = %s/%s, want %s/%s", c, msg, tc.code, tc.msg)
			}
		})
	}
}

func TestAccountsAuthAndConfig(t *testing.T) {
	t.Run("no token", func(t *testing.T) {
		// prefixAuth answers 401 before any route or handler runs; the
		// body is the verifier's own, not the envelope, so status only.
		h, _ := newCrudServer(t, &memAccountStore{}, nil)
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete} {
			path := "/api/v1/accounts"
			if method == http.MethodPatch || method == http.MethodDelete {
				path = "/api/v1/accounts/" + nextTestID()
			}
			code, _ := doReq(t, h, method, path, "", nil)
			if code != http.StatusUnauthorized {
				t.Errorf("%s without token: status = %d, want 401", method, code)
			}
		}
	})
	t.Run("nil store", func(t *testing.T) {
		h, key := newCrudServer(t, nil, nil)
		token := mintIdentityToken(t, key, "nc-sub", "nc@localhost", "N", "nc")
		code, body := doReq(t, h, http.MethodGet, "/api/v1/accounts", "", authHdr(token))
		if code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 (body %s)", code, body)
		}
		c, msg := apiErr(t, body)
		if c != "internal" || msg != "database not configured" {
			t.Errorf("envelope = %s/%s, want internal/database not configured", c, msg)
		}
	})
	t.Run("nil bootstrap", func(t *testing.T) {
		key := mintKey(t)
		h := New(fakeReady{}, BuildInfo{}, liveAuth(t, key), nil, nil, &memAccountStore{}, nil, nil, nil)
		token := mintIdentityToken(t, key, "nb-sub", "nb@localhost", "N", "nb")
		code, body := doReq(t, h, http.MethodGet, "/api/v1/accounts", "", authHdr(token))
		if code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 (body %s)", code, body)
		}
		c, msg := apiErr(t, body)
		if c != "internal" || msg != "database not configured" {
			t.Errorf("envelope = %s/%s, want internal/database not configured", c, msg)
		}
	})
}

func TestAccountsPerUserScoping(t *testing.T) {
	store := &memAccountStore{}
	h, key := newCrudServer(t, store, nil)
	tokenA := mintIdentityToken(t, key, "scope-a", "a@localhost", "A", "a")
	tokenB := mintIdentityToken(t, key, "scope-b", "b@localhost", "B", "b")

	// A creates an account.
	code, body := doReq(t, h, http.MethodPost, "/api/v1/accounts",
		`{"name":"Shared","type":"bank","currency":"USD"}`, authHdr(tokenA))
	if code != http.StatusCreated {
		t.Fatalf("POST (a) status = %d, want 201 (body %s)", code, body)
	}
	id := idFrom(t, body)

	// B's list is empty: A's row is invisible.
	code, body = doReq(t, h, http.MethodGet, "/api/v1/accounts", "", authHdr(tokenB))
	if code != http.StatusOK || strings.TrimSpace(body) != "[]" {
		t.Fatalf("GET (b) = %d %s, want 200 []", code, body)
	}

	// B cannot patch or delete A's row; existence is not disclosed.
	code, body = doReq(t, h, http.MethodPatch, "/api/v1/accounts/"+id, `{"name":"Hacked"}`, authHdr(tokenB))
	if code != http.StatusNotFound {
		t.Fatalf("PATCH (b, a's id) status = %d, want 404 (body %s)", code, body)
	}
	if c, _ := apiErr(t, body); c != "not_found" {
		t.Errorf("PATCH (b) code = %s, want not_found", c)
	}
	code, _ = doReq(t, h, http.MethodDelete, "/api/v1/accounts/"+id, "", authHdr(tokenB))
	if code != http.StatusNotFound {
		t.Fatalf("DELETE (b, a's id) status = %d, want 404", code)
	}

	// A's row is untouched.
	code, body = doReq(t, h, http.MethodGet, "/api/v1/accounts", "", authHdr(tokenA))
	if code != http.StatusOK || !strings.Contains(body, `"Shared"`) {
		t.Fatalf("GET (a) = %d %s, want the untouched row", code, body)
	}

	// The same name is reusable by B (uniqueness is per user).
	code, body = doReq(t, h, http.MethodPost, "/api/v1/accounts",
		`{"name":"Shared","type":"cash","currency":"EUR"}`, authHdr(tokenB))
	if code != http.StatusCreated {
		t.Fatalf("POST (b, same name) status = %d, want 201 (body %s)", code, body)
	}
}

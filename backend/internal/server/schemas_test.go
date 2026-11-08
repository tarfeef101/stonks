// Route tests for the import-schema endpoints (plan steps 3.3 + 4.5):
// GET /api/v1/import-schemas and POST /api/v1/import-schemas ("save
// mapping as preset") over the in-memory memSchemaStore, which mirrors
// the repo.SchemaStore contract the handlers depend on:
// EnsureBuiltins runs first (and is recorded), the list is the three
// global builtins (insertion order, standing in for created_at, id)
// followed by the caller's customs (insertion order), other users'
// customs never leak in, Get sees builtins and the caller's own rows,
// and Create names the id, trims the name and stores the spec
// verbatim. The shared helpers (authHdr, apiErr, nextTestID, doReq)
// live in accounts_test.go and settings_test.go.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"finance-tracker/internal/repo"

	"github.com/lestrrat-go/jwx/v3/jwk"
)

// memSchemaRow is one fake-store row; builtins carry an empty userID.
type memSchemaRow struct {
	userID string
	sc     repo.Schema
}

// memSchemaStore mirrors the repo.SchemaStore semantics the handlers
// depend on: EnsureBuiltins is recorded (and its error passed through),
// List answers the builtins first, then the caller's customs, both in
// insertion order (the repo orders by builtin DESC, created_at, id),
// excluding every other user's customs, Get sees builtins and the
// caller's own rows only, and Create trims the name, mints the id and
// stores the spec verbatim (Builtin: false).
type memSchemaStore struct {
	rows []memSchemaRow

	ensureErr error
	listErr   error
	ensured   bool
	getErr    error
	createErr error
}

// memBuiltinSchemas mirrors repo.builtinSchemas (unexported — the route
// tests cannot reference it): the three global presets with empty specs.
func memBuiltinSchemas() []repo.Schema {
	return []repo.Schema{
		{ID: nextTestID(), Name: "Generic CSV", Kind: "csv", Builtin: true, Spec: json.RawMessage(`{}`)},
		{ID: nextTestID(), Name: "OFX Statement", Kind: "ofx", Builtin: true, Spec: json.RawMessage(`{}`)},
		{ID: nextTestID(), Name: "Generic JSON", Kind: "json", Builtin: true, Spec: json.RawMessage(`{}`)},
	}
}

// addCustomSchema seeds one custom row and returns its minted id.
func addCustomSchema(m *memSchemaStore, userID, name, kind string, spec json.RawMessage) string {
	id := nextTestID()
	m.rows = append(m.rows, memSchemaRow{
		userID: userID,
		sc:     repo.Schema{ID: id, Name: name, Kind: kind, Builtin: false, Spec: spec},
	})
	return id
}

func (m *memSchemaStore) EnsureBuiltins(_ context.Context) (int, error) {
	if m.ensureErr != nil {
		return 0, m.ensureErr
	}
	m.ensured = true
	n := 0
	for _, r := range m.rows {
		if r.sc.Builtin {
			n++
		}
	}
	return n, nil
}

func (m *memSchemaStore) List(_ context.Context, userID string, _ []byte) ([]repo.Schema, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	out := []repo.Schema{}
	for _, r := range m.rows {
		if r.sc.Builtin {
			out = append(out, r.sc)
		}
	}
	for _, r := range m.rows {
		if !r.sc.Builtin && r.userID == userID {
			out = append(out, r.sc)
		}
	}
	return out, nil
}

func (m *memSchemaStore) Get(_ context.Context, userID, id string, _ []byte) (repo.Schema, error) {
	if m.getErr != nil {
		return repo.Schema{}, m.getErr
	}
	for _, r := range m.rows {
		if r.sc.ID == id && (r.sc.Builtin || r.userID == userID) {
			return r.sc, nil
		}
	}
	return repo.Schema{}, repo.ErrSchemaNotFound
}

func (m *memSchemaStore) Create(_ context.Context, userID string, in repo.SchemaIn, _ []byte) (repo.Schema, error) {
	if m.createErr != nil {
		return repo.Schema{}, m.createErr
	}
	sc := repo.Schema{
		ID:      nextTestID(),
		Name:    strings.TrimSpace(in.Name),
		Kind:    in.Kind,
		Builtin: false,
		Spec:    in.Spec,
	}
	m.rows = append(m.rows, memSchemaRow{userID: userID, sc: sc})
	return sc, nil
}

// newSchemasServer builds a full router with a live token verifier, an
// in-memory bootstrap, and the given schema store (nil allowed — that
// is the 503 path). The key is returned so tests can mint tokens for
// several users under the same verifier.
func newSchemasServer(t *testing.T, store SchemaStore) (http.Handler, jwk.Key) {
	t.Helper()
	key := mintKey(t)
	b, err := NewBootstrap(newMemStore(), testKEK(t), nil)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return New(fakeReady{}, BuildInfo{}, liveAuth(t, key), b, nil, nil, nil, nil, nil, nil, store, nil, nil, nil, nil, nil, nil, nil, nil, false, nil, nil), key
}

// TestImportSchemasList covers the list contract: the three builtins
// first (builtin: true, empty spec, no bookkeeping timestamps) in seed
// order, then the caller's customs (builtin: false, spec passed through
// verbatim) in seed order — and other users' customs excluded. It also
// proves the handler runs EnsureBuiltins before the list.
func TestImportSchemasList(t *testing.T) {
	store := &memSchemaStore{}
	for _, b := range memBuiltinSchemas() {
		store.rows = append(store.rows, memSchemaRow{sc: b})
	}
	specA1 := `{"columns":["date","amount"],"kind":"csv"}`
	specA2 := `{"nested":true}`
	a1 := addCustomSchema(store, "id-sch-a", "A One", "csv", json.RawMessage(specA1))
	a2 := addCustomSchema(store, "id-sch-a", "A Two", "json", json.RawMessage(specA2))
	b1 := addCustomSchema(store, "id-sch-b", "B One", "ofx", json.RawMessage(`{"columns":["x"]}`))
	h, key := newSchemasServer(t, store)
	hdrA := authHdr(mintIdentityToken(t, key, "sch-a", "a@localhost", "A", "a"))
	hdrB := authHdr(mintIdentityToken(t, key, "sch-b", "b@localhost", "B", "b"))

	code, out := doReq(t, h, http.MethodGet, "/api/v1/import-schemas", "", hdrA)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", code, out)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if len(rows) != 5 {
		t.Fatalf("A's list = %d rows, want 5 (3 builtins + A's 2 customs; B's excluded)", len(rows))
	}
	if !store.ensured {
		t.Fatal("list answered without running EnsureBuiltins first")
	}

	// Builtins first, in seed order, with an empty spec and no
	// bookkeeping timestamps.
	wantBuiltins := []struct {
		name string
		kind string
	}{
		{"Generic CSV", "csv"},
		{"OFX Statement", "ofx"},
		{"Generic JSON", "json"},
	}
	for i, want := range wantBuiltins {
		row := rows[i]
		if row["name"] != want.name || row["kind"] != want.kind {
			t.Errorf("row %d = %v/%v, want %s/%s", i, row["name"], row["kind"], want.name, want.kind)
		}
		if row["builtin"] != true {
			t.Errorf("row %d builtin = %v, want true", i, row["builtin"])
		}
		spec, ok := row["spec"].(map[string]any)
		if !ok || len(spec) != 0 {
			t.Errorf("row %d spec = %v, want an empty object", i, row["spec"])
		}
		if _, ok := row["created_at"]; ok {
			t.Errorf("row %d carries created_at (bookkeeping stays server-side)", i)
		}
		if _, ok := row["updated_at"]; ok {
			t.Errorf("row %d carries updated_at (bookkeeping stays server-side)", i)
		}
	}

	// A's customs after the builtins, in seed order, spec verbatim.
	if rows[3]["id"] != a1 || rows[4]["id"] != a2 {
		t.Fatalf("customs = [%v, %v], want [%s, %s] after the builtins", rows[3]["id"], rows[4]["id"], a1, a2)
	}
	for i, wantSpec := range []string{specA1, specA2} {
		row := rows[3+i]
		if row["builtin"] != false {
			t.Errorf("row %d builtin = %v, want false", 3+i, row["builtin"])
		}
		got, ok := row["spec"].(map[string]any)
		if !ok {
			t.Fatalf("row %d spec = %v, want an object", 3+i, row["spec"])
		}
		var want map[string]any
		if err := json.Unmarshal([]byte(wantSpec), &want); err != nil {
			t.Fatalf("test bug: bad want spec: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("row %d spec = %v, want the verbatim document %s", 3+i, got, wantSpec)
		}
	}

	// B sees the builtins plus only its own custom.
	code, out = doReq(t, h, http.MethodGet, "/api/v1/import-schemas", "", hdrB)
	if code != http.StatusOK {
		t.Fatalf("B: status = %d, want 200 (body %s)", code, out)
	}
	var bRows []map[string]any
	if err := json.Unmarshal([]byte(out), &bRows); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, out)
	}
	if len(bRows) != 4 {
		t.Fatalf("B's list = %d rows, want 4 (3 builtins + B's custom; A's excluded)", len(bRows))
	}
	if bRows[3]["id"] != b1 {
		t.Errorf("B's custom = %v, want %s last", bRows[3]["id"], b1)
	}
}

// TestImportSchemasErrors covers the two 500s: an EnsureBuiltins
// failure (before the list) and a List failure (after the ensure, which
// still ran).
func TestImportSchemasErrors(t *testing.T) {
	t.Run("ensure failure is 500", func(t *testing.T) {
		store := &memSchemaStore{ensureErr: errors.New("boom")}
		h, key := newSchemasServer(t, store)
		hdr := authHdr(mintIdentityToken(t, key, "sch-e", "e@localhost", "E", "e"))
		code, out := doReq(t, h, http.MethodGet, "/api/v1/import-schemas", "", hdr)
		if code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeInternal || m != "builtin schemas could not be ensured" {
			t.Errorf("error = %s/%q, want %s/builtin schemas could not be ensured", c, m, codeInternal)
		}
		if store.ensured {
			t.Error("store marked ensured despite the failure")
		}
	})

	t.Run("list failure is 500 after the ensure ran", func(t *testing.T) {
		store := &memSchemaStore{listErr: errors.New("boom")}
		h, key := newSchemasServer(t, store)
		hdr := authHdr(mintIdentityToken(t, key, "sch-l", "l@localhost", "L", "l"))
		code, out := doReq(t, h, http.MethodGet, "/api/v1/import-schemas", "", hdr)
		if code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeInternal || m != "schema operation failed" {
			t.Errorf("error = %s/%q, want %s/schema operation failed", c, m, codeInternal)
		}
		if !store.ensured {
			t.Error("list failure, but EnsureBuiltins did not run first")
		}
	})
}

// TestImportSchemasAuth covers the auth and 503 edges: missing/garbage
// tokens are 401 (status only — the body is the authn package's
// business), and a valid token against a nil store is a 503.
func TestImportSchemasAuth(t *testing.T) {
	t.Run("missing and garbage tokens are 401", func(t *testing.T) {
		// testAuth verifies against a nil key set: every token fails.
		h := New(fakeReady{}, BuildInfo{}, testAuth(), nil, nil, nil, nil, nil, nil, nil, &memSchemaStore{}, nil, nil, nil, nil, nil, nil, nil, nil, false, nil, nil)
		if code, _ := doReq(t, h, http.MethodGet, "/api/v1/import-schemas", "", nil); code != http.StatusUnauthorized {
			t.Errorf("no token: status = %d, want 401", code)
		}
		if code, _ := doReq(t, h, http.MethodGet, "/api/v1/import-schemas", "",
			map[string]string{"Authorization": "Bearer not.a.jwt"}); code != http.StatusUnauthorized {
			t.Errorf("garbage token: status = %d, want 401", code)
		}
	})

	t.Run("nil store is 503 with a valid token", func(t *testing.T) {
		h, key := newSchemasServer(t, nil)
		tok := mintIdentityToken(t, key, "sch-nc", "nc@localhost", "N", "nc")
		code, out := doReq(t, h, http.MethodGet, "/api/v1/import-schemas", "", authHdr(tok))
		if code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeInternal || m != "database not configured" {
			t.Errorf("envelope = %s/%q, want internal/database not configured", c, m)
		}
	})
}

// TestImportSchemasCreate covers POST /api/v1/import-schemas: a valid
// document is stored (name trimmed, spec verbatim) and shows up in the
// caller's list; each check in the ladder (body decode, name, kind
// vocabulary, spec shape, kind/spec agreement) fails with its own 400
// and writes no row; a store error is a 500.
func TestImportSchemasCreate(t *testing.T) {
	validSpec := `{"kind":"csv","map":{"amount":{"index":2}}}`

	t.Run("valid document is stored and listed", func(t *testing.T) {
		store := &memSchemaStore{}
		h, key := newSchemasServer(t, store)
		hdr := authHdr(mintIdentityToken(t, key, "sch-cr", "cr@localhost", "CR", "cr"))
		body := `{"name":"  Padded Name  ","kind":"csv","spec":` + validSpec + `}`
		code, out := doReq(t, h, http.MethodPost, "/api/v1/import-schemas", body, hdr)
		if code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (body %s)", code, out)
		}
		var row schemaOut
		if err := json.Unmarshal([]byte(out), &row); err != nil {
			t.Fatalf("decode: %v (body %s)", err, out)
		}
		if row.Name != "Padded Name" || row.Kind != "csv" || row.Builtin {
			t.Errorf("created row = %+v, want trimmed name, kind csv, builtin false", row)
		}
		if string(row.Spec) != validSpec {
			t.Errorf("spec = %s, want the sent bytes verbatim", row.Spec)
		}
		if len(store.rows) != 1 || store.rows[0].sc.Name != "Padded Name" {
			t.Errorf("store = %+v, want exactly the one trimmed row", store.rows)
		}

		// The new preset is visible in the caller's list.
		code, out = doReq(t, h, http.MethodGet, "/api/v1/import-schemas", "", hdr)
		if code != http.StatusOK {
			t.Fatalf("list status = %d, want 200 (body %s)", code, out)
		}
		var rows []schemaOut
		if err := json.Unmarshal([]byte(out), &rows); err != nil {
			t.Fatalf("decode list: %v (body %s)", err, out)
		}
		found := false
		for _, r := range rows {
			if r.ID == row.ID && r.Name == "Padded Name" && !r.Builtin {
				found = true
			}
		}
		if !found {
			t.Errorf("list of %d rows does not contain the new preset", len(rows))
		}
	})

	t.Run("invalid documents are 400 without writing", func(t *testing.T) {
		store := &memSchemaStore{}
		h, key := newSchemasServer(t, store)
		hdr := authHdr(mintIdentityToken(t, key, "sch-cb", "cb@localhost", "CB", "cb"))
		cases := []struct {
			name string
			body string
			msg  string
		}{
			{"malformed body", `{not json`, "invalid request body"},
			{"empty name", `{"name":"   ","kind":"csv","spec":` + validSpec + `}`, "name must not be empty"},
			{"name too long", `{"name":"` + strings.Repeat("a", 256) + `","kind":"csv","spec":` + validSpec + `}`, "name must be at most 255 characters"},
			{"bad kind", `{"name":"Bad Kind","kind":"yaml","spec":` + validSpec + `}`, "kind must be one of csv, ofx, json"},
			{"spec not a document", `{"name":"Bad Spec","kind":"csv","spec":"nope"}`, "spec must be a valid mapping document"},
			{"spec null", `{"name":"Null Spec","kind":"csv","spec":null}`, "kind must be one of csv, ofx, json"},
			{"empty csv spec", `{"name":"Empty CSV","kind":"csv","spec":{"kind":"csv","map":{}}}`, "csv mapping needs at least one field"},
			{"kind/spec mismatch", `{"name":"Mismatch","kind":"ofx","spec":{"kind":"csv","map":{"amount":{"index":2}}}}`, "kind must match the spec"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				code, out := doReq(t, h, http.MethodPost, "/api/v1/import-schemas", tc.body, hdr)
				wantAPIError(t, code, out, http.StatusBadRequest, "invalid_request", tc.msg)
				if len(store.rows) != 0 {
					t.Fatalf("store has %d rows, want none", len(store.rows))
				}
			})
		}
	})

	t.Run("create store error is 500", func(t *testing.T) {
		store := &memSchemaStore{createErr: errors.New("boom")}
		h, key := newSchemasServer(t, store)
		hdr := authHdr(mintIdentityToken(t, key, "sch-ce", "ce@localhost", "CE", "ce"))
		body := `{"name":"Boom","kind":"csv","spec":` + validSpec + `}`
		code, out := doReq(t, h, http.MethodPost, "/api/v1/import-schemas", body, hdr)
		wantAPIError(t, code, out, http.StatusInternalServerError, "internal", "schema operation failed")
	})
}

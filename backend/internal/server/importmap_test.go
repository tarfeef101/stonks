// Route tests for the import mapping endpoints (plan step 4.5) on the
// in-memory stores from imports_test.go / schemas_test.go:
//
//	GET  /api/v1/imports/presets        builtin specs + the caller's customs
//	GET  /api/v1/imports/{id}/analyze   format facts + suggested mapping
//	POST /api/v1/imports/{id}/preview   stateless mapped-row preview
//	POST /api/v1/imports/{id}/mapping   store the confirmed mapping
//
// Success bodies are raw JSON values (only errors are enveloped), so
// the tests decode them into the package's own wire structs. The
// fixtures and expected values follow the verified parser behavior
// (handoff 4.5, section 2).
package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"finance-tracker/internal/csvparse"
	"finance-tracker/internal/repo"

	"github.com/lestrrat-go/jwx/v3/jwk"
)

// newImportMapServer builds a full router with a live token verifier,
// an in-memory bootstrap, and the given import + schema stores (nil
// allowed — that is the 503 path). The key is returned so tests can
// mint tokens for several users under the same verifier.
func newImportMapServer(t *testing.T, imp ImportStore, schemas SchemaStore) (http.Handler, jwk.Key) {
	t.Helper()
	key := mintKey(t)
	b, err := NewBootstrap(newMemStore(), testKEK(t))
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return New(fakeReady{}, BuildInfo{}, liveAuth(t, key), b, nil, nil, nil, nil, nil, nil, schemas, imp, nil), key
}

// ip is a pointer to one csv index (fieldRef.Index is *int; a zero
// value would mean column 0, so "absent" is nil and -1 is explicit).
func ip(i int) *int { return &i }

// mustSpec decodes a presetOut spec into the package's own Mapping
// type (same-package tests may name it directly).
func mustSpec(t *testing.T, raw json.RawMessage) Mapping {
	t.Helper()
	var m Mapping
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode spec %s: %v", raw, err)
	}
	return m
}

// wantAPIError pins a failed response: the exact status plus the
// envelope code and message (errors are the only enveloped bodies).
func wantAPIError(t *testing.T, status int, body string, wantStatus int, code, message string) {
	t.Helper()
	if status != wantStatus {
		t.Fatalf("status = %d, want %d (body %s)", status, wantStatus, body)
	}
	if c, m := apiErr(t, body); c != code || m != message {
		t.Fatalf("envelope = %s/%q, want %s/%q (body %s)", c, m, code, message, body)
	}
}

// doAnalyze GETs one job's analyze endpoint, optionally with ?preset=.
func doAnalyze(t *testing.T, h http.Handler, hdr map[string]string, jobID, preset string) (int, string) {
	t.Helper()
	path := "/api/v1/imports/" + jobID + "/analyze"
	if preset != "" {
		path += "?preset=" + preset
	}
	return doReq(t, h, http.MethodGet, path, "", hdr)
}

// doPreview posts {"mapping":<doc>,"limit":N} to one job's preview
// endpoint; a negative limit omits the key (the handler's default of 10
// applies).
func doPreview(t *testing.T, h http.Handler, hdr map[string]string, jobID, doc string, limit int) (int, string) {
	t.Helper()
	body := `{"mapping":` + doc
	if limit >= 0 {
		body += `,"limit":` + strconv.Itoa(limit)
	}
	body += `}`
	return doReq(t, h, http.MethodPost, "/api/v1/imports/"+jobID+"/preview", body, hdr)
}

// analyzeSetup builds the standard fixture: one user (sub), one
// account, one seeded job (file + filename), and the given schema
// store (nil -> no saved presets). Returns the handler, the auth
// header, the job id, and the store (for error injection / state
// assertions).
func analyzeSetup(t *testing.T, sub, fileName string, file []byte, schemas SchemaStore) (http.Handler, map[string]string, string, *memImportStore) {
	t.Helper()
	// The seeded job belongs to the store's only account; both must
	// use the same id or the job is invisible to Get/ReadFile.
	accountID := nextTestID()
	store := &memImportStore{accounts: map[string]string{accountID: "id-" + sub}}
	jobID := seedJobFile(store, accountID, fileName, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), file)
	h, key := newImportMapServer(t, store, schemas)
	hdr := authHdr(mintIdentityToken(t, key, sub, sub+"@localhost", sub, sub))
	return h, hdr, jobID, store
}

// Statement fixtures (expected values computed from the verified
// parsers).
var (
	// tdCSV: the filename token "td" claims the TD preset; the
	// balance column holds only two recurrence pairs, so it is a
	// plain amount (date / text / amount / amount).
	tdCSV = []byte("Date,Transaction Details,Amount,Balance\n" +
		"2026-01-05,COFFEE ROASTERY,-55.55,4944.45\n" +
		"2026-01-12,GROCERY STORE,-120.30,4824.15\n" +
		"2026-01-20,PAYROLL DEP,2500.00,7324.15\n")

	// plainCSV: no filename hint, no header signature -> generic-csv
	// and the Suggest fallback.
	plainCSV = []byte("Date,Details,Amount\n" +
		"2026-01-05,COFFEE ROASTERY,-55.55\n" +
		"2026-01-12,GROCERY STORE,-120.30\n" +
		"2026-01-20,PAYROLL DEP,2500.00\n")

	// scotiaCSV: no filename hint; the seven-column header row is the
	// Scotiabank signature (Filter left empty).
	scotiaCSV = []byte("Filter,Date,Description,Sub-description,Type of Transaction,Amount,Balance\n" +
		",2026-01-05,COFFEE ROASTERY,,,-55.55,4944.45\n" +
		",2026-01-12,GROCERY STORE,,,-120.30,4824.15\n" +
		",2026-01-20,PAYROLL DEP,,,2500.00,7324.15\n")
)

var (
	// tinyOFX: two transactions in the ofxparse record form (tags
	// carry no closing ">").
	tinyOFX = []byte("OFX100\n%\n!\n<BANKMSGSRSV1\n<BANKTRANLIST\n" +
		"<DTSTART:20251001\n<DTEND:20251231\n" +
		"<STMTTRN\n<TRNAMT:-55.55\n<DTPOSTED:20260105123000[-5]\n<NAME:COFFEE ROASTERY\n</STMTTRN\n" +
		"<STMTTRN\n<TRNAMT:100.00\n<DTPOSTED:20260120\n<NAME:LANDLORD PAYMENT\n</STMTTRN\n" +
		"</BANKTRANLIST\n</BANKMSGSRSV1\n")

	// emptyOFX: a valid skeleton with no STMTTRN records.
	emptyOFX = []byte("OFX100\n%\n!\n<BANKMSGSRSV1\n<BANKTRANLIST\n" +
		"<DTSTART:20251001\n<DTEND:20251231\n</BANKTRANLIST\n</BANKMSGSRSV1\n")

	// jsonNested: the generic doc's /transactions pointer finds two
	// items; the amounts keep their file spelling in the preview.
	jsonNested = []byte(`{"transactions":[{"datePosted":"2026-01-05","amount":-45.23,"description":"COFFEE ROASTERY"},` +
		`{"datePosted":"2026-01-12","amount":2500.00,"description":"PAYROLL DEP"}]}`)

	// jsonFlat: a root-level array (the generic doc's "" pointer).
	jsonFlat = []byte(`[{"date":"2026-01-05","amount":-10.00,"name":"COFFEE ROASTERY"},` +
		`{"date":"2026-01-12","amount":5.50,"name":"PAYROLL DEP"}]`)

	jsonEmpty = []byte(`{"transactions":[]}`)
	// jsonMalformed: truncated -> a parse error, not ErrEmpty.
	jsonMalformed = []byte(`{"transactions": [`)
)

// genericJSONMapping is the generic-json preset document hand-copied
// from presets.go — deliberately NOT presetMapping(presets.Get(...)),
// which would turn the preset tests into a tautology.
func genericJSONMapping() Mapping {
	return Mapping{
		Kind:          "json",
		ArrayPointers: []string{"", "/transactions", "/data", "/items", "/records", "/txns"},
		Map: map[csvparse.Field]fieldRef{
			csvparse.FieldDateExecuted:   {Pointers: []string{"/dateExecuted", "/executedDate", "/executionDate", "/executeDate"}},
			csvparse.FieldDatePosted:     {Pointers: []string{"/datePosted", "/postedDate", "/posted", "/transactionDate", "/txDate", "/date"}},
			csvparse.FieldAmount:         {Pointers: []string{"/amount", "/transactionAmount", "/txAmount", "/value"}},
			csvparse.FieldDescription:    {Pointers: []string{"/description", "/name", "/memo", "/narrative", "/details", "/payee"}},
			csvparse.FieldCategory:       {Pointers: []string{"/category", "/categoryName", "/category_name"}},
			csvparse.FieldRunningBalance: {Pointers: []string{"/runningBalance", "/running_balance", "/balance", "/endBalance"}},
		},
	}
}

// sixtyRowCSV is a 60-row "Date,Details,Amount" statement: the preview
// limit-clamp fixture (limit 100 -> 50 rows, total 60).
func sixtyRowCSV() []byte {
	var b strings.Builder
	b.WriteString("Date,Details,Amount\n")
	for i := 1; i <= 60; i++ {
		fmt.Fprintf(&b, "2026-02-%02d,ROW %02d,-1.50\n", (i-1)%28+1, i)
	}
	return []byte(b.String())
}

// TestImportPresets covers GET /imports/presets: the eight builtins in
// canonical order (specs rebuilt from the preset definitions), then the
// caller's saved presets — legacy DB builtin rows stay out of the
// list (the !Builtin filter).
func TestImportPresets(t *testing.T) {
	schemas := &memSchemaStore{}
	// A legacy DB builtin row: excluded by the handler.
	schemas.rows = append(schemas.rows, memSchemaRow{sc: repo.Schema{
		ID: nextTestID(), Name: "Legacy Builtin", Kind: "csv", Builtin: true, Spec: json.RawMessage(`{}`),
	}})
	customSpec := `{"kind":"csv","map":{"amount":{"index":2}}}`
	customID := addCustomSchema(schemas, "id-ipm-a", "My Bank", "csv", json.RawMessage(customSpec))

	store := &memImportStore{accounts: map[string]string{nextTestID(): "id-ipm-a"}}
	h, key := newImportMapServer(t, store, schemas)
	hdr := authHdr(mintIdentityToken(t, key, "ipm-a", "a@localhost", "A", "a"))
	code, body := doReq(t, h, http.MethodGet, "/api/v1/imports/presets", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", code, body)
	}
	var out []presetOut
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode presets: %v (body %s)", err, body)
	}
	wantIDs := []string{"generic-csv", "generic-json", "ofx-generic", "td", "scotiabank", "bmo", "rbc", "desjardins", customID}
	if len(out) != len(wantIDs) {
		t.Fatalf("presets = %d rows, want %d", len(out), len(wantIDs))
	}
	for i, p := range out {
		if p.ID != wantIDs[i] {
			t.Errorf("preset[%d].id = %q, want %q", i, p.ID, wantIDs[i])
		}
		if i < 8 && !p.Builtin {
			t.Errorf("preset %q: builtin = false, want true", p.ID)
		}
	}
	custom := out[len(out)-1]
	if custom.Builtin || custom.Name != "My Bank" || custom.Bank != "" || string(custom.Spec) != customSpec {
		t.Errorf("custom preset = %+v, want name My Bank, bank \"\", spec verbatim", custom)
	}
	byID := map[string]presetOut{}
	for _, p := range out {
		byID[p.ID] = p
	}
	check := func(id string, want Mapping) {
		t.Helper()
		if m := mustSpec(t, byID[id].Spec); !reflect.DeepEqual(m, want) {
			t.Errorf("%s spec = %+v, want %+v", id, m, want)
		}
	}
	check("generic-csv", Mapping{Kind: "csv", Map: map[csvparse.Field]fieldRef{}})
	check("ofx-generic", Mapping{Kind: "ofx", Map: map[csvparse.Field]fieldRef{}})
	check("bmo", Mapping{Kind: "ofx", Map: map[csvparse.Field]fieldRef{}})
	check("rbc", Mapping{Kind: "ofx", Map: map[csvparse.Field]fieldRef{}})
	check("desjardins", Mapping{Kind: "ofx", Map: map[csvparse.Field]fieldRef{}})
	check("generic-json", genericJSONMapping())
	check("td", Mapping{Kind: "csv", Map: map[csvparse.Field]fieldRef{
		csvparse.FieldDateExecuted:   {Headers: []string{"execution date", "execute date", "executed date"}, Index: ip(-1)},
		csvparse.FieldDatePosted:     {Headers: []string{"date", "date posted", "posted date", "posting date"}, Index: ip(-1)},
		csvparse.FieldAmount:         {Headers: []string{"amount", "transaction amount", "tx amount", "amount (cad)"}, Index: ip(-1)},
		csvparse.FieldDescription:    {Headers: []string{"description", "memo", "details", "transaction details"}, Index: ip(-1)},
		csvparse.FieldRunningBalance: {Headers: []string{"balance", "running balance", "end balance", "ending balance"}, Index: ip(-1)},
	}})
	check("scotiabank", Mapping{Kind: "csv", Map: map[csvparse.Field]fieldRef{
		csvparse.FieldDatePosted:     {Headers: []string{"date"}, Index: ip(-1)},
		csvparse.FieldDescription:    {Headers: []string{"description"}, Index: ip(-1)},
		csvparse.FieldAmount:         {Headers: []string{"amount"}, Index: ip(-1)},
		csvparse.FieldRunningBalance: {Headers: []string{"balance"}, Index: ip(-1)},
	}})
}

// TestImportAnalyzeCSV covers the csv branch of GET /imports/{id}/analyze:
// the preset picked by filename token or header signature (and its
// resolved suggestion), the generic-csv + Suggest fallback, a saved
// custom preset via ?preset=, and the 400/404/500 edges.
func TestImportAnalyzeCSV(t *testing.T) {
	schemas := &memSchemaStore{}
	customID := addCustomSchema(schemas, "id-an-csv", "Custom CSV", "csv",
		json.RawMessage(`{"kind":"csv","map":{"date_posted":{"index":0},"amount":{"index":2}}}`))
	badID := addCustomSchema(schemas, "id-an-csv", "Broken", "csv", json.RawMessage(`{"kind":"csv"}`))

	t.Run("filename token picks the TD preset", func(t *testing.T) {
		h, hdr, jobID, _ := analyzeSetup(t, "an-csv", "TD_Transactions_2026-01.csv", tdCSV, schemas)
		code, body := doAnalyze(t, h, hdr, jobID, "")
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", code, body)
		}
		var out analyzeOut
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode: %v (body %s)", err, body)
		}
		if out.Kind != "csv" || out.Preset == nil || out.Preset.ID != "td" || out.Preset.Name != "TD Bank (CSV)" {
			t.Errorf("kind/preset = %q/%+v, want csv/td", out.Kind, out.Preset)
		}
		if !reflect.DeepEqual(out.Headers, []string{"Date", "Transaction Details", "Amount", "Balance"}) {
			t.Errorf("headers = %v", out.Headers)
		}
		wantCols := []analyzeColumn{
			{Index: 0, Heading: "Date", Kind: "date"},
			{Index: 1, Heading: "Transaction Details", Kind: "text"},
			{Index: 2, Heading: "Amount", Kind: "amount"},
			{Index: 3, Heading: "Balance", Kind: "amount"},
		}
		if !reflect.DeepEqual(out.Columns, wantCols) {
			t.Errorf("columns = %+v, want %+v", out.Columns, wantCols)
		}
		// resolveCSV against the TD doc: date_executed and category
		// have no matching header (and a -1 index) and are omitted.
		want := Mapping{Kind: "csv", Map: map[csvparse.Field]fieldRef{
			csvparse.FieldDatePosted:     {Index: ip(0)},
			csvparse.FieldDescription:    {Index: ip(1)},
			csvparse.FieldAmount:         {Index: ip(2)},
			csvparse.FieldRunningBalance: {Index: ip(3)},
		}}
		if !reflect.DeepEqual(out.Suggested, want) {
			t.Errorf("suggested = %+v, want %+v", out.Suggested, want)
		}
	})

	t.Run("header signature picks scotiabank", func(t *testing.T) {
		h, hdr, jobID, _ := analyzeSetup(t, "an-csv", "statement.csv", scotiaCSV, schemas)
		code, body := doAnalyze(t, h, hdr, jobID, "")
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", code, body)
		}
		var out analyzeOut
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode: %v (body %s)", err, body)
		}
		if out.Preset == nil || out.Preset.ID != "scotiabank" {
			t.Errorf("preset = %+v, want scotiabank (no filename hint)", out.Preset)
		}
		want := Mapping{Kind: "csv", Map: map[csvparse.Field]fieldRef{
			csvparse.FieldDatePosted:     {Index: ip(1)},
			csvparse.FieldDescription:    {Index: ip(2)},
			csvparse.FieldAmount:         {Index: ip(5)},
			csvparse.FieldRunningBalance: {Index: ip(6)},
		}}
		if !reflect.DeepEqual(out.Suggested, want) {
			t.Errorf("suggested = %+v, want %+v", out.Suggested, want)
		}
	})

	t.Run("no match falls back to generic-csv and Suggest", func(t *testing.T) {
		h, hdr, jobID, _ := analyzeSetup(t, "an-csv", "statement.csv", plainCSV, schemas)
		code, body := doAnalyze(t, h, hdr, jobID, "")
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", code, body)
		}
		var out analyzeOut
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode: %v (body %s)", err, body)
		}
		if out.Preset == nil || out.Preset.ID != "generic-csv" {
			t.Errorf("preset = %+v, want generic-csv", out.Preset)
		}
		want := Mapping{Kind: "csv", Map: map[csvparse.Field]fieldRef{
			csvparse.FieldDateExecuted:   {Index: ip(0)},
			csvparse.FieldDatePosted:     {Index: ip(0)},
			csvparse.FieldAmount:         {Index: ip(2)},
			csvparse.FieldDescription:    {Index: ip(1)},
			csvparse.FieldCategory:       {Index: ip(1)},
			csvparse.FieldRunningBalance: {Index: ip(2)},
		}}
		if !reflect.DeepEqual(out.Suggested, want) {
			t.Errorf("suggested = %+v, want %+v", out.Suggested, want)
		}
	})

	t.Run("?preset= with a saved custom csv preset", func(t *testing.T) {
		h, hdr, jobID, _ := analyzeSetup(t, "an-csv", "statement.csv", tdCSV, schemas)
		code, body := doAnalyze(t, h, hdr, jobID, customID)
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", code, body)
		}
		var out analyzeOut
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode: %v (body %s)", err, body)
		}
		if out.Preset == nil || out.Preset.ID != customID || out.Preset.Name != "Custom CSV" {
			t.Errorf("preset = %+v, want the saved custom", out.Preset)
		}
		// The saved doc is re-resolved against the file's headers; the
		// index-only refs pass through.
		want := Mapping{Kind: "csv", Map: map[csvparse.Field]fieldRef{
			csvparse.FieldDatePosted: {Index: ip(0)},
			csvparse.FieldAmount:     {Index: ip(2)},
		}}
		if !reflect.DeepEqual(out.Suggested, want) {
			t.Errorf("suggested = %+v, want %+v", out.Suggested, want)
		}
	})

	t.Run("preset lookup errors", func(t *testing.T) {
		// Every case needs a readable csv job; the preset check fires
		// before the parse.
		h, hdr, jobID, _ := analyzeSetup(t, "an-csv", "statement.csv", tdCSV, schemas)
		cases := []struct {
			preset string
			code   string
			msg    string
		}{
			{"nope", "invalid_request", "unknown preset"},
			{"generic-json", "invalid_request", "preset kind must match the file type"},
			{badID, "invalid_request", fmt.Sprintf("preset %q is not a usable mapping", badID)},
		}
		for _, tc := range cases {
			t.Run(tc.preset, func(t *testing.T) {
				code, body := doAnalyze(t, h, hdr, jobID, tc.preset)
				wantAPIError(t, code, body, http.StatusBadRequest, tc.code, tc.msg)
			})
		}
	})

	t.Run("schema store error is 500", func(t *testing.T) {
		// A real store error (not a sentinel) is a 500. The preset id
		// is a fresh, valid uuid (builtins resolve before the store is
		// touched) and the job is the one analyzeSetup seeded (Get runs
		// before the lookup).
		h, hdr, jobID, _ := analyzeSetup(t, "an-csv", "statement.csv", tdCSV,
			&memSchemaStore{getErr: errors.New("boom")})
		code, body := doAnalyze(t, h, hdr, jobID, nextTestID())
		wantAPIError(t, code, body, http.StatusInternalServerError, "internal", "preset is not usable")
	})

	t.Run("parse and lookup edges", func(t *testing.T) {
		t.Run("unsupported file type is 400", func(t *testing.T) {
			h, hdr, jobID, _ := analyzeSetup(t, "an-csv", "notes.txt", []byte("hello"), &memSchemaStore{})
			code, body := doAnalyze(t, h, hdr, jobID, "")
			wantAPIError(t, code, body, http.StatusBadRequest, "invalid_request", "unsupported file type")
		})

		t.Run("header-only csv is 400", func(t *testing.T) {
			// csvparse reports ErrEmpty for a header row with no data;
			// the csv branch maps every parse error to the same 400.
			h, hdr, jobID, _ := analyzeSetup(t, "an-csv", "empty.csv", []byte("Date,Amount\n"), &memSchemaStore{})
			code, body := doAnalyze(t, h, hdr, jobID, "")
			wantAPIError(t, code, body, http.StatusBadRequest, "invalid_request", "file could not be parsed as CSV")
		})

		t.Run("bad ids are 404", func(t *testing.T) {
			h, hdr, _, _ := analyzeSetup(t, "an-csv", "statement.csv", tdCSV, &memSchemaStore{})
			for _, id := range []string{nextTestID(), "not-a-uuid"} {
				t.Run(id, func(t *testing.T) {
					code, body := doAnalyze(t, h, hdr, id, "")
					wantAPIError(t, code, body, http.StatusNotFound, "not_found", "import job not found")
				})
			}
		})

		t.Run("import store error is 500", func(t *testing.T) {
			h, hdr, jobID, store := analyzeSetup(t, "an-csv", "statement.csv", tdCSV, &memSchemaStore{})
			store.getErr = errors.New("boom")
			code, body := doAnalyze(t, h, hdr, jobID, "")
			wantAPIError(t, code, body, http.StatusInternalServerError, "internal", "import operation failed")
		})
	})
}

// mustDoc marshals a Mapping into the raw document string the preview
// and set-mapping request bodies carry.
func mustDoc(t *testing.T, m Mapping) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal mapping: %v", err)
	}
	return string(b)
}

// TestImportAnalyzeJSON covers the json branch of analyze: the first
// item's keys (sorted, nil when none), the generic-json suggestion,
// and the 400 edges.
func TestImportAnalyzeJSON(t *testing.T) {
	t.Run("nested array", func(t *testing.T) {
		h, hdr, jobID, _ := analyzeSetup(t, "an-json", "data.json", jsonNested, &memSchemaStore{})
		code, body := doAnalyze(t, h, hdr, jobID, "")
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", code, body)
		}
		var out analyzeOut
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode: %v (body %s)", err, body)
		}
		if out.Kind != "json" {
			t.Errorf("kind = %q, want json", out.Kind)
		}
		if !reflect.DeepEqual(out.Keys, []string{"amount", "datePosted", "description"}) {
			t.Errorf("keys = %v", out.Keys)
		}
		if !reflect.DeepEqual(out.Suggested, genericJSONMapping()) {
			t.Errorf("suggested = %+v, want the generic-json doc", out.Suggested)
		}
		// Without ?preset= the key is still present, as null (Preset
		// has no omitempty).
		var raw map[string]any
		if err := json.Unmarshal([]byte(body), &raw); err != nil {
			t.Fatalf("decode raw: %v (body %s)", err, body)
		}
		if v, ok := raw["preset"]; !ok || v != nil {
			t.Errorf("preset = %v (present %v), want null", v, ok)
		}
	})

	t.Run("root-level array", func(t *testing.T) {
		h, hdr, jobID, _ := analyzeSetup(t, "an-json", "data.json", jsonFlat, &memSchemaStore{})
		code, body := doAnalyze(t, h, hdr, jobID, "")
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", code, body)
		}
		var out analyzeOut
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode: %v (body %s)", err, body)
		}
		if !reflect.DeepEqual(out.Keys, []string{"amount", "date", "name"}) {
			t.Errorf("keys = %v", out.Keys)
		}
		if !reflect.DeepEqual(out.Suggested, genericJSONMapping()) {
			t.Errorf("suggested = %+v, want the generic-json doc", out.Suggested)
		}
	})

	t.Run("empty array yields no keys", func(t *testing.T) {
		h, hdr, jobID, _ := analyzeSetup(t, "an-json", "data.json", jsonEmpty, &memSchemaStore{})
		code, body := doAnalyze(t, h, hdr, jobID, "")
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", code, body)
		}
		var out analyzeOut
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode: %v (body %s)", err, body)
		}
		if out.Keys != nil {
			t.Errorf("keys = %v, want nil", out.Keys)
		}
		if !reflect.DeepEqual(out.Suggested, genericJSONMapping()) {
			t.Errorf("suggested = %+v, want the generic-json doc", out.Suggested)
		}
	})

	t.Run("parse and lookup edges", func(t *testing.T) {
		// One malformed file serves all three: the preset lookup fires
		// before the parse.
		h, hdr, jobID, _ := analyzeSetup(t, "an-json", "data.json", jsonMalformed, &memSchemaStore{})
		code, body := doAnalyze(t, h, hdr, jobID, "")
		wantAPIError(t, code, body, http.StatusBadRequest, "invalid_request", "file could not be parsed as JSON")
		code, body = doAnalyze(t, h, hdr, jobID, "td")
		wantAPIError(t, code, body, http.StatusBadRequest, "invalid_request", "preset kind must match the file type")
		code, body = doAnalyze(t, h, hdr, jobID, "nope")
		wantAPIError(t, code, body, http.StatusBadRequest, "invalid_request", "unknown preset")
	})
}

// TestImportAnalyzeOFX covers the ofx branch of analyze: the bank
// preset picked by filename token (else ofx-generic) and the identity
// suggestion (the OFX column layout is fixed, so the map stays empty),
// plus the 400 for a file without transactions.
func TestImportAnalyzeOFX(t *testing.T) {
	identity := Mapping{Kind: "ofx", Map: map[csvparse.Field]fieldRef{}}
	cases := []struct {
		file   string
		preset string
	}{
		{"statement.ofx", "ofx-generic"},
		{"BMO_2026-01.qfx", "bmo"},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			h, hdr, jobID, _ := analyzeSetup(t, "an-ofx", tc.file, tinyOFX, &memSchemaStore{})
			code, body := doAnalyze(t, h, hdr, jobID, "")
			if code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", code, body)
			}
			var out analyzeOut
			if err := json.Unmarshal([]byte(body), &out); err != nil {
				t.Fatalf("decode: %v (body %s)", err, body)
			}
			if out.Kind != "ofx" || out.Preset == nil || out.Preset.ID != tc.preset {
				t.Errorf("kind/preset = %q/%+v, want ofx/%s", out.Kind, out.Preset, tc.preset)
			}
			if !reflect.DeepEqual(out.Suggested, identity) {
				t.Errorf("suggested = %+v, want the identity mapping", out.Suggested)
			}
		})
	}

	t.Run("no transactions is 400", func(t *testing.T) {
		// A valid skeleton without STMTTRN records: ErrEmpty.
		h, hdr, jobID, _ := analyzeSetup(t, "an-ofx", "statement.ofx", emptyOFX, &memSchemaStore{})
		code, body := doAnalyze(t, h, hdr, jobID, "")
		wantAPIError(t, code, body, http.StatusBadRequest, "invalid_request", "no transactions found in file")
	})

	t.Run("garbage is 400", func(t *testing.T) {
		// The ofx parser reports no-records for input with no OFX
		// structure at all.
		h, hdr, jobID, _ := analyzeSetup(t, "an-ofx", "statement.ofx", []byte("hello world"), &memSchemaStore{})
		code, body := doAnalyze(t, h, hdr, jobID, "")
		wantAPIError(t, code, body, http.StatusBadRequest, "invalid_request", "no transactions found in file")
	})
}

// TestImportPreview covers POST /imports/{id}/preview: the mapped rows
// (header match and index fallback for csv, pointer resolution for
// json, the fixed columns for ofx), the limit clamp (default 10, cap
// 50, total always the whole file), and the 400/404/500 edges.
func TestImportPreview(t *testing.T) {
	csvCols := []string{"date_executed", "date_posted", "amount", "description", "category", "running_balance"}

	t.Run("csv header match, index fallback, unmapped fields", func(t *testing.T) {
		// date_posted: its header matches column 0; amount: no header
		// match, the positional index 2 wins; the other fields are
		// unmapped and come back empty.
		doc := `{"kind":"csv","map":{"date_posted":{"headers":["Date"],"index":-1},"amount":{"headers":["nope"],"index":2}}}`
		h, hdr, jobID, _ := analyzeSetup(t, "pv-csv", "statement.csv", plainCSV, &memSchemaStore{})
		code, body := doPreview(t, h, hdr, jobID, doc, -1)
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", code, body)
		}
		var out previewOut
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode: %v (body %s)", err, body)
		}
		if out.Total != 3 {
			t.Errorf("total = %d, want 3", out.Total)
		}
		want := [][]string{
			{"", "2026-01-05", "-55.55", "", "", ""},
			{"", "2026-01-12", "-120.30", "", "", ""},
			{"", "2026-01-20", "2500.00", "", "", ""},
		}
		if !reflect.DeepEqual(out.Rows, want) {
			t.Errorf("rows = %v, want %v", out.Rows, want)
		}
		if !reflect.DeepEqual(out.Columns, csvCols) {
			t.Errorf("columns = %v, want %v", out.Columns, csvCols)
		}
	})

	t.Run("csv short row leaves the out-of-range cell empty", func(t *testing.T) {
		file := []byte("Date,Details,Amount\n2026-01-05,COFFEE ROASTERY,-55.55\n2026-01-12\n")
		doc := `{"kind":"csv","map":{"date_posted":{"index":0},"amount":{"index":2}}}`
		h, hdr, jobID, _ := analyzeSetup(t, "pv-csv", "statement.csv", file, &memSchemaStore{})
		code, body := doPreview(t, h, hdr, jobID, doc, -1)
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", code, body)
		}
		var out previewOut
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode: %v (body %s)", err, body)
		}
		if out.Total != 2 {
			t.Errorf("total = %d, want 2", out.Total)
		}
		want := [][]string{
			{"", "2026-01-05", "-55.55", "", "", ""},
			{"", "2026-01-12", "", "", "", ""},
		}
		if !reflect.DeepEqual(out.Rows, want) {
			t.Errorf("rows = %v, want %v", out.Rows, want)
		}
	})

	t.Run("limit clamps to 50 and defaults to 10", func(t *testing.T) {
		h, hdr, jobID, _ := analyzeSetup(t, "pv-csv", "statement.csv", sixtyRowCSV(), &memSchemaStore{})
		doc := `{"kind":"csv","map":{"date_posted":{"index":0},"amount":{"index":2}}}`
		check := func(limit, wantRows int) {
			t.Helper()
			code, body := doPreview(t, h, hdr, jobID, doc, limit)
			if code != http.StatusOK {
				t.Fatalf("limit %d: status = %d, want 200 (body %s)", limit, code, body)
			}
			var out previewOut
			if err := json.Unmarshal([]byte(body), &out); err != nil {
				t.Fatalf("limit %d: decode: %v (body %s)", limit, err, body)
			}
			if out.Total != 60 {
				t.Errorf("limit %d: total = %d, want 60", limit, out.Total)
			}
			if len(out.Rows) != wantRows {
				t.Errorf("limit %d: rows = %d, want %d", limit, len(out.Rows), wantRows)
			}
			if limit == 100 {
				if want := []string{"", "2026-02-01", "-1.50", "", "", ""}; !reflect.DeepEqual(out.Rows[0], want) {
					t.Errorf("limit 100: rows[0] = %v, want %v", out.Rows[0], want)
				}
				if want := []string{"", "2026-02-22", "-1.50", "", "", ""}; !reflect.DeepEqual(out.Rows[49], want) {
					t.Errorf("limit 100: rows[49] = %v, want %v", out.Rows[49], want)
				}
			}
		}
		check(100, 50)
		check(0, 10)
		check(-1, 10)
	})

	t.Run("json keeps the file's number spelling", func(t *testing.T) {
		doc := mustDoc(t, genericJSONMapping())
		h, hdr, jobID, _ := analyzeSetup(t, "pv-json", "data.json", jsonNested, &memSchemaStore{})
		code, body := doPreview(t, h, hdr, jobID, doc, -1)
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", code, body)
		}
		var out previewOut
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode: %v (body %s)", err, body)
		}
		if out.Total != 2 {
			t.Errorf("total = %d, want 2", out.Total)
		}
		want := [][]string{
			{"", "2026-01-05", "-45.23", "COFFEE ROASTERY", "", ""},
			{"", "2026-01-12", "2500.00", "PAYROLL DEP", "", ""},
		}
		if !reflect.DeepEqual(out.Rows, want) {
			t.Errorf("rows = %v, want %v", out.Rows, want)
		}
		if !reflect.DeepEqual(out.Columns, csvCols) {
			t.Errorf("columns = %v, want %v", out.Columns, csvCols)
		}
	})

	t.Run("json root-level array", func(t *testing.T) {
		doc := mustDoc(t, genericJSONMapping())
		h, hdr, jobID, _ := analyzeSetup(t, "pv-json", "data.json", jsonFlat, &memSchemaStore{})
		code, body := doPreview(t, h, hdr, jobID, doc, -1)
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", code, body)
		}
		var out previewOut
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode: %v (body %s)", err, body)
		}
		if out.Total != 2 {
			t.Errorf("total = %d, want 2", out.Total)
		}
		want := [][]string{
			{"", "2026-01-05", "-10.00", "COFFEE ROASTERY", "", ""},
			{"", "2026-01-12", "5.50", "PAYROLL DEP", "", ""},
		}
		if !reflect.DeepEqual(out.Rows, want) {
			t.Errorf("rows = %v, want %v", out.Rows, want)
		}
	})

	t.Run("ofx fixed columns", func(t *testing.T) {
		// The OFX layout is fixed: date_posted, amount, name, then the
		// three unmapped canonical columns, empty.
		doc := `{"kind":"ofx","map":{}}`
		h, hdr, jobID, _ := analyzeSetup(t, "pv-ofx", "statement.ofx", tinyOFX, &memSchemaStore{})
		code, body := doPreview(t, h, hdr, jobID, doc, -1)
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", code, body)
		}
		var out previewOut
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode: %v (body %s)", err, body)
		}
		if out.Total != 2 {
			t.Errorf("total = %d, want 2", out.Total)
		}
		want := [][]string{
			{"20260105123000[-5]", "-55.55", "COFFEE ROASTERY", "", "", ""},
			{"20260120", "100.00", "LANDLORD PAYMENT", "", "", ""},
		}
		if !reflect.DeepEqual(out.Rows, want) {
			t.Errorf("rows = %v, want %v", out.Rows, want)
		}
		if !reflect.DeepEqual(out.Columns, csvCols) {
			t.Errorf("columns = %v, want %v", out.Columns, csvCols)
		}
	})

	t.Run("unknown job is 404", func(t *testing.T) {
		// A valid document against a job that does not exist: the
		// body decodes and validates, Get reports it missing.
		h, hdr, _, _ := analyzeSetup(t, "pv-err", "statement.csv", plainCSV, &memSchemaStore{})
		code, body := doPreview(t, h, hdr, nextTestID(), `{"kind":"csv","map":{"amount":{"index":2}}}`, -1)
		wantAPIError(t, code, body, http.StatusNotFound, "not_found", "import job not found")
	})

	t.Run("malformed body is 400 before any lookup", func(t *testing.T) {
		// The body is decoded before the job is fetched, so the bad
		// job id never surfaces.
		h, hdr, _, _ := analyzeSetup(t, "pv-err", "statement.csv", plainCSV, &memSchemaStore{})
		code, body := doPreview(t, h, hdr, nextTestID(), `{"kind":"csv","map":`, -1)
		wantAPIError(t, code, body, http.StatusBadRequest, "invalid_request", "invalid mapping document")
	})

	t.Run("oversized body is 400, not 413", func(t *testing.T) {
		// decodeBody caps the read at 1 MiB; the truncated JSON is a
		// decode error (only set-mapping has the 413 path).
		h, hdr, _, _ := analyzeSetup(t, "pv-err", "statement.csv", plainCSV, &memSchemaStore{})
		code, body := doPreview(t, h, hdr, nextTestID(), strings.Repeat("a", 1<<20+1), -1)
		wantAPIError(t, code, body, http.StatusBadRequest, "invalid_request", "invalid mapping document")
	})

	t.Run("kind mismatch is 400", func(t *testing.T) {
		doc := mustDoc(t, genericJSONMapping())
		h, hdr, jobID, _ := analyzeSetup(t, "pv-err", "statement.csv", plainCSV, &memSchemaStore{})
		code, body := doPreview(t, h, hdr, jobID, doc, -1)
		wantAPIError(t, code, body, http.StatusBadRequest, "invalid_request", "mapping kind must match the file type")
	})

	t.Run("csv doc without fields is 400", func(t *testing.T) {
		h, hdr, jobID, _ := analyzeSetup(t, "pv-err", "statement.csv", plainCSV, &memSchemaStore{})
		code, body := doPreview(t, h, hdr, jobID, `{"kind":"csv","map":{}}`, -1)
		wantAPIError(t, code, body, http.StatusBadRequest, "invalid_request", "csv mapping needs at least one field")
	})

	t.Run("json file without items is 400", func(t *testing.T) {
		h, hdr, jobID, _ := analyzeSetup(t, "pv-err", "data.json", []byte(`{"foo":[1,2]}`), &memSchemaStore{})
		code, body := doPreview(t, h, hdr, jobID, mustDoc(t, genericJSONMapping()), -1)
		wantAPIError(t, code, body, http.StatusBadRequest, "invalid_request", "no transactions found at the mapped location")
	})

	t.Run("store errors are 500", func(t *testing.T) {
		doc := `{"kind":"csv","map":{"amount":{"index":2}}}`
		h, hdr, jobID, store := analyzeSetup(t, "pv-err", "statement.csv", plainCSV, &memSchemaStore{})
		store.getErr = errors.New("boom")
		code, body := doPreview(t, h, hdr, jobID, doc, -1)
		wantAPIError(t, code, body, http.StatusInternalServerError, "internal", "import operation failed")
		h2, hdr2, jobID2, store2 := analyzeSetup(t, "pv-err", "statement.csv", plainCSV, &memSchemaStore{})
		store2.readFileErr = errors.New("boom")
		code, body = doPreview(t, h2, hdr2, jobID2, doc, -1)
		wantAPIError(t, code, body, http.StatusInternalServerError, "internal", "import operation failed")
	})
}

// TestImportSetMapping covers POST /imports/{id}/mapping: the stored
// document is the raw request body verbatim (status "mapping"), the
// validate table (exactly one bad field per case), the kind match, the
// 413 cap, the 404s and the 500.
func TestImportSetMapping(t *testing.T) {
	t.Run("csv mapping is stored verbatim", func(t *testing.T) {
		h, hdr, jobID, store := analyzeSetup(t, "sm-csv", "statement.csv", plainCSV, &memSchemaStore{})
		sent := `{"kind":"csv", "map":{"amount":{"index":2},"date_posted":{"headers":["Date"],"index":0}}}`
		code, body := doReq(t, h, http.MethodPost, "/api/v1/imports/"+jobID+"/mapping", sent, hdr)
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", code, body)
		}
		var out importOut
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode: %v (body %s)", err, body)
		}
		if out.ID != jobID || out.Status != "mapping" || out.FileName != "statement.csv" ||
			out.AccountID != store.rows[0].AccountID {
			t.Errorf("job = %+v, want id %s, status mapping", out, jobID)
		}
		if out.Result != nil || out.Error != nil {
			t.Errorf("result/error = %v/%v, want nil", out.Result, out.Error)
		}
		// The response re-encodes the row (json.RawMessage compacts
		// whitespace on marshal), so the wire bytes are the compact
		// form; the store itself keeps the sent bytes verbatim.
		var sentDoc, outDoc map[string]any
		if err := json.Unmarshal([]byte(sent), &sentDoc); err != nil {
			t.Fatalf("decode sent doc: %v", err)
		}
		if err := json.Unmarshal(out.Mapping, &outDoc); err != nil {
			t.Fatalf("decode response mapping %s: %v", out.Mapping, err)
		}
		if !reflect.DeepEqual(sentDoc, outDoc) {
			t.Errorf("response mapping = %s, want %s (semantically)", out.Mapping, sent)
		}
		if !bytes.Equal(store.rows[0].Mapping, []byte(sent)) {
			t.Errorf("stored mapping = %s, want the sent bytes verbatim", store.rows[0].Mapping)
		}
	})

	t.Run("ofx identity mapping is stored", func(t *testing.T) {
		h, hdr, jobID, store := analyzeSetup(t, "sm-ofx", "BMO_2026-01.qfx", tinyOFX, &memSchemaStore{})
		sent := `{"kind":"ofx","map":{}}`
		code, body := doReq(t, h, http.MethodPost, "/api/v1/imports/"+jobID+"/mapping", sent, hdr)
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", code, body)
		}
		var out importOut
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode: %v (body %s)", err, body)
		}
		if out.Status != "mapping" || !bytes.Equal(out.Mapping, []byte(sent)) {
			t.Errorf("job = %+v, want status mapping and the sent bytes", out)
		}
		if !bytes.Equal(store.rows[0].Mapping, []byte(sent)) {
			t.Errorf("stored mapping = %s, want the sent bytes verbatim", store.rows[0].Mapping)
		}
	})

	t.Run("mapping document validation errors", func(t *testing.T) {
		h, hdr, jobID, _ := analyzeSetup(t, "sm-bad", "sm-bad.csv", plainCSV, &memSchemaStore{})
		cases := []struct {
			doc string
			msg string
		}{
			{`{}`, "kind must be one of csv, ofx, json"},
			{`{"kind":"nope","map":{}}`, "kind must be one of csv, ofx, json"},
			{`{"kind":"csv","map":{}}`, "csv mapping needs at least one field"},
			{`{"kind":"csv","map":{"amount":{}}}`, "field amount: index is required for csv"},
			{`{"kind":"csv","map":{"amount":{"index":-2}}}`, "field amount: index must be >= -1"},
			{`{"kind":"csv","map":{"bogus":{"index":0}}}`, `unknown field "bogus"`},
			{`{"kind":"csv","map":{"amount":{"index":0}},"array_pointers":["x"]}`, "array_pointers is only for json mappings"},
			{`{"kind":"json","map":{"amount":{}}}`, `field "amount" has no pointers`},
			{`{"kind":"ofx","map":{"amount":{"index":0}}}`, "ofx mapping must be empty (the OFX fields are fixed)"},
		}
		for _, tc := range cases {
			t.Run(tc.doc, func(t *testing.T) {
				code, body := doReq(t, h, http.MethodPost, "/api/v1/imports/"+jobID+"/mapping", tc.doc, hdr)
				wantAPIError(t, code, body, http.StatusBadRequest, "invalid_request", tc.msg)
			})
		}
	})

	t.Run("mapping kind must match the file type", func(t *testing.T) {
		h, hdr, jobID, _ := analyzeSetup(t, "sm-kind", "sm-kind.csv", plainCSV, &memSchemaStore{})
		doc := `{"kind":"json","map":{"amount":{"pointers":["/amount"]}}}`
		code, body := doReq(t, h, http.MethodPost, "/api/v1/imports/"+jobID+"/mapping", doc, hdr)
		wantAPIError(t, code, body, http.StatusBadRequest, "invalid_request", "mapping kind must match the file type")
	})

	t.Run("oversized mapping is rejected before any lookup", func(t *testing.T) {
		h, hdr, _, _ := analyzeSetup(t, "sm-big", "sm-big.csv", plainCSV, &memSchemaStore{})
		code, body := doReq(t, h, http.MethodPost, "/api/v1/imports/"+nextTestID()+"/mapping", strings.Repeat("a", 1<<20+1), hdr)
		wantAPIError(t, code, body, http.StatusRequestEntityTooLarge, "payload_too_large", "mapping is too large")
	})

	t.Run("malformed mapping document is 400", func(t *testing.T) {
		h, hdr, jobID, _ := analyzeSetup(t, "sm-mal", "sm-mal.csv", plainCSV, &memSchemaStore{})
		code, body := doReq(t, h, http.MethodPost, "/api/v1/imports/"+jobID+"/mapping", `{"kind":"csv","map":`, hdr)
		wantAPIError(t, code, body, http.StatusBadRequest, "invalid_request", "invalid mapping document")
	})

	t.Run("missing job is 404", func(t *testing.T) {
		h, hdr, _, _ := analyzeSetup(t, "sm-miss", "sm-miss.csv", plainCSV, &memSchemaStore{})
		doc := `{"kind":"csv","map":{"amount":{"index":2}}}`
		code, body := doReq(t, h, http.MethodPost, "/api/v1/imports/"+nextTestID()+"/mapping", doc, hdr)
		wantAPIError(t, code, body, http.StatusNotFound, "not_found", "import job not found")
	})

	t.Run("non-uuid job id is 404", func(t *testing.T) {
		h, hdr, _, _ := analyzeSetup(t, "sm-noid", "sm-noid.csv", plainCSV, &memSchemaStore{})
		code, body := doReq(t, h, http.MethodPost, "/api/v1/imports/not-a-uuid/mapping", `{}`, hdr)
		wantAPIError(t, code, body, http.StatusNotFound, "not_found", "import job not found")
	})

	t.Run("another user's job is not found", func(t *testing.T) {
		accountX := nextTestID()
		accountY := nextTestID()
		store := &memImportStore{accounts: map[string]string{accountX: "id-sm-x", accountY: "id-sm-y"}}
		jobID := seedJobFile(store, accountX, "sm-x.csv", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), plainCSV)
		h, key := newImportMapServer(t, store, &memSchemaStore{})
		hdr := authHdr(mintIdentityToken(t, key, "sm-y", "sm-y@localhost", "sm-y", "sm-y"))
		doc := `{"kind":"csv","map":{"amount":{"index":2}}}`
		code, body := doReq(t, h, http.MethodPost, "/api/v1/imports/"+jobID+"/mapping", doc, hdr)
		wantAPIError(t, code, body, http.StatusNotFound, "not_found", "import job not found")
	})

	t.Run("set-mapping store error is 500", func(t *testing.T) {
		h, hdr, jobID, store := analyzeSetup(t, "sm-err", "sm-err.csv", plainCSV, &memSchemaStore{})
		store.setMappingErr = errors.New("boom")
		doc := `{"kind":"csv","map":{"amount":{"index":2}}}`
		code, body := doReq(t, h, http.MethodPost, "/api/v1/imports/"+jobID+"/mapping", doc, hdr)
		wantAPIError(t, code, body, http.StatusInternalServerError, "internal", "import operation failed")
	})
}

// TestImportMapAuth pins the auth and configuration failure modes for
// all five import-mapping routes: a missing or invalid token is 401
// (status only — the token middleware owns the body), and a valid
// token against a nil store is 503 because the not-configured check
// runs before the user lookup.
func TestImportMapAuth(t *testing.T) {
	jobID := nextTestID()
	routes := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/imports/presets"},
		{http.MethodGet, "/api/v1/imports/" + jobID + "/analyze"},
		{http.MethodPost, "/api/v1/imports/" + jobID + "/preview"},
		{http.MethodPost, "/api/v1/imports/" + jobID + "/mapping"},
		{http.MethodPost, "/api/v1/import-schemas"},
	}

	t.Run("missing or invalid token is 401", func(t *testing.T) {
		h := New(fakeReady{}, BuildInfo{}, testAuth(), nil, nil, nil, nil, nil, nil, nil, &memSchemaStore{}, &memImportStore{}, nil)
		for _, tc := range routes {
			for name, hdr := range map[string]map[string]string{
				"no-token": {},
				"garbage":  {"Authorization": "Bearer not-a-token"},
			} {
				t.Run(tc.method+" "+tc.path+" "+name, func(t *testing.T) {
					code, _ := doReq(t, h, tc.method, tc.path, "", hdr)
					if code != http.StatusUnauthorized {
						t.Fatalf("status = %d, want 401", code)
					}
				})
			}
		}
	})

	t.Run("valid token with nil stores is 503", func(t *testing.T) {
		h, key := newImportMapServer(t, nil, nil)
		hdr := authHdr(mintIdentityToken(t, key, "sm-auth", "sm-auth@localhost", "sm-auth", "sm-auth"))
		for _, tc := range routes {
			t.Run(tc.method+" "+tc.path, func(t *testing.T) {
				code, body := doReq(t, h, tc.method, tc.path, "", hdr)
				wantAPIError(t, code, body, http.StatusServiceUnavailable, "internal", "database not configured")
			})
		}
	})
}

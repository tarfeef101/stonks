// Route tests for the 0019 plan fields on the account endpoints
// (beneficiary name/birth year, opening balance, opened_at). They build
// on the in-memory store and shared helpers from accounts_test.go.
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// planFieldsServer builds a route-test server with a fresh in-memory
// account store and a minted identity token under one verifier.
func planFieldsServer(t *testing.T) (http.Handler, string) {
	t.Helper()
	h, key := newCrudServer(t, &memAccountStore{}, nil)
	return h, mintIdentityToken(t, key, "planf-sub", "planf@localhost", "Plan", "Fields")
}

// planFieldsRow decodes an account row body into a map for field
// asserts (JSON null → nil, numbers → float64).
func planFieldsRow(t *testing.T, body string) map[string]any {
	t.Helper()
	var row map[string]any
	if err := json.Unmarshal([]byte(body), &row); err != nil {
		t.Fatalf("decode row: %v (body %s)", err, body)
	}
	return row
}

func TestAccountPlanFieldsCreate(t *testing.T) {
	h, token := planFieldsServer(t)
	hdr := authHdr(token)

	// 1. full plan-metadata create
	status, body := doReq(t, h, http.MethodPost, "/api/v1/accounts",
		`{"name":"Kids RESP","type":"resp","currency":"CAD","beneficiary_name":"Baby Smith","beneficiary_birth_year":"2024","opening_balance":"1000","opened_at":"2024-01-15"}`, hdr)
	if status != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201 (body %s)", status, body)
	}
	row := planFieldsRow(t, body)
	if row["beneficiary_name"] != "Baby Smith" {
		t.Errorf("beneficiary_name = %v, want %q", row["beneficiary_name"], "Baby Smith")
	}
	if row["beneficiary_birth_year"] != float64(2024) {
		t.Errorf("beneficiary_birth_year = %v, want 2024", row["beneficiary_birth_year"])
	}
	if row["opening_balance"] != "1000.00" {
		t.Errorf("opening_balance = %v, want %q", row["opening_balance"], "1000.00")
	}
	if row["opened_at"] != "2024-01-15" {
		t.Errorf("opened_at = %v, want %q", row["opened_at"], "2024-01-15")
	}

	// 2. plain create: every plan field unset
	status, body = doReq(t, h, http.MethodPost, "/api/v1/accounts",
		`{"name":"Plain TFSA","type":"tfsa","currency":"CAD"}`, hdr)
	if status != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201 (body %s)", status, body)
	}
	row = planFieldsRow(t, body)
	if row["beneficiary_name"] != nil {
		t.Errorf("beneficiary_name = %v, want null", row["beneficiary_name"])
	}
	if row["beneficiary_birth_year"] != nil {
		t.Errorf("beneficiary_birth_year = %v, want null", row["beneficiary_birth_year"])
	}
	if row["opening_balance"] != "0.00" {
		t.Errorf("opening_balance = %v, want %q", row["opening_balance"], "0.00")
	}
	if row["opened_at"] != nil {
		t.Errorf("opened_at = %v, want null", row["opened_at"])
	}

	// 3. plan metadata is allowed on any account type
	status, body = doReq(t, h, http.MethodPost, "/api/v1/accounts",
		`{"name":"RRSP X","type":"rrsp","currency":"CAD","beneficiary_name":"Anyone","beneficiary_birth_year":"1990"}`, hdr)
	if status != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201 (body %s)", status, body)
	}

	// 4. the list returns all three with fields intact
	status, body = doReq(t, h, http.MethodGet, "/api/v1/accounts", "", hdr)
	if status != http.StatusOK {
		t.Fatalf("list: status = %d, want 200 (body %s)", status, body)
	}
	var list []map[string]any
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode list: %v (body %s)", err, body)
	}
	if len(list) != 3 {
		t.Fatalf("list = %d entries, want 3", len(list))
	}
	var kids map[string]any
	for _, e := range list {
		if e["name"] == "Kids RESP" {
			kids = e
			break
		}
	}
	if kids == nil {
		t.Fatalf("Kids RESP missing from list (body %s)", body)
	}
	if kids["beneficiary_name"] != "Baby Smith" || kids["beneficiary_birth_year"] != float64(2024) ||
		kids["opening_balance"] != "1000.00" || kids["opened_at"] != "2024-01-15" {
		t.Errorf("Kids RESP list entry missing plan fields: %v", kids)
	}

	// 5. lower boundaries are valid
	status, body = doReq(t, h, http.MethodPost, "/api/v1/accounts",
		`{"name":"Edge","type":"investment","currency":"CAD","opening_balance":"0","opened_at":"1900-01-01","beneficiary_birth_year":"1900"}`, hdr)
	if status != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201 (body %s)", status, body)
	}
	row = planFieldsRow(t, body)
	if row["opening_balance"] != "0.00" {
		t.Errorf("opening_balance = %v, want %q", row["opening_balance"], "0.00")
	}
	if row["opened_at"] != "1900-01-01" {
		t.Errorf("opened_at = %v, want %q", row["opened_at"], "1900-01-01")
	}
	if row["beneficiary_birth_year"] != float64(1900) {
		t.Errorf("beneficiary_birth_year = %v, want 1900", row["beneficiary_birth_year"])
	}
}

func TestAccountPlanFieldsValidation(t *testing.T) {
	h, token := planFieldsServer(t)
	hdr := authHdr(token)

	const benYearFrag = "beneficiary_birth_year must be a 4-digit year between 1900 and the current year"
	const decimalFrag = "opening_balance must be a decimal amount with at most 2 decimal places"
	const negativeFrag = "opening_balance must not be negative"
	const dateFrag = "opened_at must be a YYYY-MM-DD date between 1900-01-01 and 2100-12-31"

	cases := []struct {
		field, value, frag string
	}{
		{"beneficiary_birth_year", `"20241"`, benYearFrag},
		{"beneficiary_birth_year", `"abcd"`, benYearFrag},
		{"beneficiary_birth_year", `"1899"`, benYearFrag},
		{"beneficiary_birth_year", strconv.Quote(strconv.Itoa(time.Now().UTC().Year() + 1)), benYearFrag},
		{"opening_balance", `"abc"`, decimalFrag},
		{"opening_balance", `"1.999"`, decimalFrag},
		{"opening_balance", `"-5.00"`, negativeFrag},
		{"opened_at", `"2024-1-15"`, dateFrag},
		{"opened_at", `"2024-01-32"`, dateFrag},
		{"opened_at", `"1899-06-01"`, dateFrag},
		{"opened_at", `"2101-01-01"`, dateFrag},
	}
	for i, c := range cases {
		name := "V" + strconv.Itoa(i+1)
		reqBody := fmt.Sprintf(`{"name":"%s","type":"tfsa","currency":"CAD","%s":%s}`, name, c.field, c.value)
		status, resp := doReq(t, h, http.MethodPost, "/api/v1/accounts", reqBody, hdr)
		if status != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400 (body %s)", name, status, resp)
		}
		code, msg := apiErr(t, resp)
		if code != "invalid_request" {
			t.Errorf("%s: code = %q, want invalid_request", name, code)
		}
		if !strings.Contains(msg, c.frag) {
			t.Errorf("%s: message %q does not contain %q", name, msg, c.frag)
		}
	}

	// combined: both problems joined with "; "
	status, resp := doReq(t, h, http.MethodPost, "/api/v1/accounts",
		`{"name":"V12","type":"tfsa","currency":"CAD","beneficiary_birth_year":"18","opening_balance":"-1.00"}`, hdr)
	if status != http.StatusBadRequest {
		t.Fatalf("V12: status = %d, want 400 (body %s)", status, resp)
	}
	code, msg := apiErr(t, resp)
	if code != "invalid_request" {
		t.Errorf("V12: code = %q, want invalid_request", code)
	}
	if !strings.Contains(msg, benYearFrag) || !strings.Contains(msg, negativeFrag) {
		t.Errorf("V12: message %q missing one or both fragments", msg)
	}

	// PATCH validation on a real account
	status, body := doReq(t, h, http.MethodPost, "/api/v1/accounts",
		`{"name":"Patch Valid","type":"tfsa","currency":"CAD"}`, hdr)
	if status != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201 (body %s)", status, body)
	}
	id := idFrom(t, body)
	for _, pc := range []string{
		`{"opening_balance":"1.999"}`,
		`{"beneficiary_birth_year":"1899"}`,
		`{"opened_at":"nope"}`,
	} {
		status, resp := doReq(t, h, http.MethodPatch, "/api/v1/accounts/"+id, pc, hdr)
		if status != http.StatusBadRequest {
			t.Fatalf("PATCH %s: status = %d, want 400 (body %s)", pc, status, resp)
		}
		code, _ = apiErr(t, resp)
		if code != "invalid_request" {
			t.Errorf("PATCH %s: code = %q, want invalid_request", pc, code)
		}
	}

	// empty patch keeps its own message
	status, resp = doReq(t, h, http.MethodPatch, "/api/v1/accounts/"+id, `{}`, hdr)
	if status != http.StatusBadRequest {
		t.Fatalf("PATCH {}: status = %d, want 400 (body %s)", status, resp)
	}
	code, msg = apiErr(t, resp)
	if code != "invalid_request" || msg != "empty patch: provide at least one field" {
		t.Errorf("PATCH {}: code=%q message=%q, want invalid_request / empty patch message", code, msg)
	}
}

func TestAccountPlanFieldsPatch(t *testing.T) {
	h, token := planFieldsServer(t)
	hdr := authHdr(token)

	status, body := doReq(t, h, http.MethodPost, "/api/v1/accounts",
		`{"name":"Kids RESP","type":"resp","currency":"CAD","beneficiary_name":"Baby Smith","beneficiary_birth_year":"2024","opening_balance":"1000","opened_at":"2024-01-15"}`, hdr)
	if status != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201 (body %s)", status, body)
	}
	id := idFrom(t, body)
	patchURL := "/api/v1/accounts/" + id

	// 1. update beneficiary name + birth year; opening balance untouched
	status, body = doReq(t, h, http.MethodPatch, patchURL,
		`{"beneficiary_name":"Baby S.","beneficiary_birth_year":"2025"}`, hdr)
	if status != http.StatusOK {
		t.Fatalf("patch 1: status = %d, want 200 (body %s)", status, body)
	}
	row := planFieldsRow(t, body)
	if row["beneficiary_name"] != "Baby S." {
		t.Errorf("beneficiary_name = %v, want %q", row["beneficiary_name"], "Baby S.")
	}
	if row["beneficiary_birth_year"] != float64(2025) {
		t.Errorf("beneficiary_birth_year = %v, want 2025", row["beneficiary_birth_year"])
	}
	if row["opening_balance"] != "1000.00" {
		t.Errorf("opening_balance = %v, want %q (untouched)", row["opening_balance"], "1000.00")
	}

	// 2. null clears beneficiary_name
	status, body = doReq(t, h, http.MethodPatch, patchURL, `{"beneficiary_name":null}`, hdr)
	if status != http.StatusOK {
		t.Fatalf("patch 2: status = %d, want 200 (body %s)", status, body)
	}
	if row = planFieldsRow(t, body); row["beneficiary_name"] != nil {
		t.Errorf("beneficiary_name = %v, want null", row["beneficiary_name"])
	}

	// 3. null clears beneficiary_birth_year
	status, body = doReq(t, h, http.MethodPatch, patchURL, `{"beneficiary_birth_year":null}`, hdr)
	if status != http.StatusOK {
		t.Fatalf("patch 3: status = %d, want 200 (body %s)", status, body)
	}
	if row = planFieldsRow(t, body); row["beneficiary_birth_year"] != nil {
		t.Errorf("beneficiary_birth_year = %v, want null", row["beneficiary_birth_year"])
	}

	// 4. set opening balance
	status, body = doReq(t, h, http.MethodPatch, patchURL, `{"opening_balance":"250.50"}`, hdr)
	if status != http.StatusOK {
		t.Fatalf("patch 4: status = %d, want 200 (body %s)", status, body)
	}
	if row = planFieldsRow(t, body); row["opening_balance"] != "250.50" {
		t.Errorf("opening_balance = %v, want %q", row["opening_balance"], "250.50")
	}

	// 5. null resets opening balance to 0
	status, body = doReq(t, h, http.MethodPatch, patchURL, `{"opening_balance":null}`, hdr)
	if status != http.StatusOK {
		t.Fatalf("patch 5: status = %d, want 200 (body %s)", status, body)
	}
	if row = planFieldsRow(t, body); row["opening_balance"] != "0.00" {
		t.Errorf("opening_balance = %v, want %q", row["opening_balance"], "0.00")
	}

	// 6. set opened_at
	status, body = doReq(t, h, http.MethodPatch, patchURL, `{"opened_at":"2025-03-01"}`, hdr)
	if status != http.StatusOK {
		t.Fatalf("patch 6: status = %d, want 200 (body %s)", status, body)
	}
	if row = planFieldsRow(t, body); row["opened_at"] != "2025-03-01" {
		t.Errorf("opened_at = %v, want %q", row["opened_at"], "2025-03-01")
	}

	// 7. null clears opened_at
	status, body = doReq(t, h, http.MethodPatch, patchURL, `{"opened_at":null}`, hdr)
	if status != http.StatusOK {
		t.Fatalf("patch 7: status = %d, want 200 (body %s)", status, body)
	}
	if row = planFieldsRow(t, body); row["opened_at"] != nil {
		t.Errorf("opened_at = %v, want null", row["opened_at"])
	}

	// 8. plain rename still works; the cleared fields stay cleared
	status, body = doReq(t, h, http.MethodPatch, patchURL, `{"name":"Renamed RESP"}`, hdr)
	if status != http.StatusOK {
		t.Fatalf("patch 8: status = %d, want 200 (body %s)", status, body)
	}
	status, body = doReq(t, h, http.MethodGet, "/api/v1/accounts", "", hdr)
	if status != http.StatusOK {
		t.Fatalf("list: status = %d, want 200 (body %s)", status, body)
	}
	var list []map[string]any
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode list: %v (body %s)", err, body)
	}
	if len(list) != 1 {
		t.Fatalf("list = %d entries, want 1", len(list))
	}
	entry := list[0]
	if entry["name"] != "Renamed RESP" {
		t.Errorf("name = %v, want %q", entry["name"], "Renamed RESP")
	}
	if entry["beneficiary_name"] != nil {
		t.Errorf("beneficiary_name = %v, want null", entry["beneficiary_name"])
	}
	if entry["opening_balance"] != "0.00" {
		t.Errorf("opening_balance = %v, want %q", entry["opening_balance"], "0.00")
	}
	if entry["opened_at"] != nil {
		t.Errorf("opened_at = %v, want null", entry["opened_at"])
	}
}

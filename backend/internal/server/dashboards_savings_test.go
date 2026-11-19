// Round 52b3: the "savings" dashboard widget kind. Migration 0019
// extended the 0013 widget kind CHECK with "savings"; its content is a
// small canonical JSON object the server validates: "" (the default
// all-accounts room view) or {"view":"room"} or
// {"view":"appreciation","account_id":"<uuid>"}. The appreciation
// account must belong to the caller: a foreign or missing account is
// a plain 404 (existence is not disclosed). The server does not
// re-serialize the content — it round-trips exactly as sent. All
// in-memory, mirroring dashboards_test.go (shared helpers authHdr,
// apiErr, idFrom, doReq, nextTestID, dashJSON, mintIdentityToken live
// in the other server test files).
package server

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/lestrrat-go/jwx/v3/jwk"
)

// newSavingsDashServer is newDashServer with the accounts store wired
// at its position (the 6th New() param) so the savings appreciation
// check can resolve accounts.
func newSavingsDashServer(t *testing.T, accts AccountStore, dash DashboardStore, wid DashboardWidgetStore) (http.Handler, jwk.Key) {
	t.Helper()
	key := mintKey(t)
	b, err := NewBootstrap(newMemStore(), testKEK(t), nil)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return New(
		fakeReady{}, BuildInfo{}, liveAuth(t, key), b,
		nil, accts, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		dash, wid, nil, nil, nil, false, nil, nil, nil, nil, nil, nil, nil,
	), key
}

// TestSavingsWidgetValidation pins the create-time savings content
// rules: the kind is accepted, the content round-trips as sent, the
// defaults are span 6 / row_span 4; every other content shape is a
// 400 with the exact message; the 256-rune cap fires before the parse.
func TestSavingsWidgetValidation(t *testing.T) {
	accID := nextTestID()
	accts := &memAccountStore{}
	accts.rows = append(accts.rows, memAccountRow{userID: "id-sv-a", id: accID, name: "TFSA", typ: "tfsa", currency: "CAD"})
	dash, wid := newMemDashStores()
	h, key := newSavingsDashServer(t, accts, dash, wid)
	tok := mintIdentityToken(t, key, "sv-a", "sv-a@example.com", "Savings A", "savingsuser")
	hdr := authHdr(tok)

	code, out := doReq(t, h, http.MethodPost, "/api/v1/dashboards", `{"name":"D"}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("setup: %d (%s)", code, out)
	}
	wpath := "/api/v1/dashboards/" + idFrom(t, out) + "/widgets"

	appreciation := `{"view":"appreciation","account_id":"` + accID + `"}`
	okCases := []struct {
		name        string
		body        string
		wantContent string
	}{
		{"absent content", `{"name":"W","kind":"savings"}`, ""},
		{"room object", `{"name":"W","kind":"savings","content":` + strconv.Quote(`{"view":"room"}`) + `}`, `{"view":"room"}`},
		{"appreciation object", `{"name":"W","kind":"savings","content":` + strconv.Quote(appreciation) + `}`, appreciation},
	}
	for _, tc := range okCases {
		t.Run(tc.name, func(t *testing.T) {
			code, out := doReq(t, h, http.MethodPost, wpath, tc.body, hdr)
			if code != http.StatusCreated {
				t.Fatalf("status = %d, want 201 (body %s)", code, out)
			}
			m := dashJSON(t, out)
			if m["kind"] != "savings" {
				t.Fatalf("kind = %v, want savings", m["kind"])
			}
			if m["content"] != tc.wantContent {
				t.Fatalf("content = %v, want %v (round-trips as sent)", m["content"], tc.wantContent)
			}
			if m["span"] != float64(6) {
				t.Fatalf("span = %v, want 6", m["span"])
			}
			if m["row_span"] != float64(4) {
				t.Fatalf("row_span = %v, want 4", m["row_span"])
			}
		})
	}

	badCases := []struct {
		name string
		body string
		want string
	}{
		{"not json", `{"name":"W","kind":"savings","content":` + strconv.Quote("not json") + `}`, `savings content must be "" or a JSON object`},
		{"array", `{"name":"W","kind":"savings","content":"[]"}`, `savings content must be "" or a JSON object`},
		{"empty object", `{"name":"W","kind":"savings","content":` + strconv.Quote(`{}`) + `}`, `savings content: view must be "room" or "appreciation"`},
		{"bogus view", `{"name":"W","kind":"savings","content":` + strconv.Quote(`{"view":"bogus"}`) + `}`, `savings content: view must be "room" or "appreciation"`},
		{"number view", `{"name":"W","kind":"savings","content":` + strconv.Quote(`{"view":123}`) + `}`, `savings content: view must be "room" or "appreciation"`},
		{"appreciation missing account", `{"name":"W","kind":"savings","content":` + strconv.Quote(`{"view":"appreciation"}`) + `}`, `savings content: account_id must be a valid UUID`},
		{"bad uuid", `{"name":"W","kind":"savings","content":` + strconv.Quote(`{"view":"appreciation","account_id":"nope"}`) + `}`, `savings content: account_id must be a valid UUID`},
		{"number account_id", `{"name":"W","kind":"savings","content":` + strconv.Quote(`{"view":"appreciation","account_id":123}`) + `}`, `savings content: account_id must be a valid UUID`},
		{"room with account", `{"name":"W","kind":"savings","content":` + strconv.Quote(`{"view":"room","account_id":"`+accID+`"}`) + `}`, `savings content: account_id is not allowed with the room view`},
		{"unknown key", `{"name":"W","kind":"savings","content":` + strconv.Quote(`{"view":"room","bogus":1}`) + `}`, `savings content: unknown key "bogus"`},
		{"over cap", `{"name":"W","kind":"savings","content":` + strconv.Quote(`{"view":"room","pad":"`+strings.Repeat("a", 240)+`"}`) + `}`, `content must be at most 256 characters`},
	}
	for _, tc := range badCases {
		t.Run(tc.name, func(t *testing.T) {
			code, out := doReq(t, h, http.MethodPost, wpath, tc.body, hdr)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", code, out)
			}
			if _, m := apiErr(t, out); m != tc.want {
				t.Fatalf("message = %q, want %q", m, tc.want)
			}
		})
	}
}

// TestSavingsWidgetOwnership pins the appreciation account rules on
// BOTH create and update: the account must belong to the caller — a
// foreign or missing account is a plain 404 (existence is not
// disclosed); a kind swap to savings re-checks the content in effect
// (a markdown widget patched to savings re-parses its EXISTING
// content).
func TestSavingsWidgetOwnership(t *testing.T) {
	ownAcc := nextTestID()
	foreignAcc := nextTestID()
	accts := &memAccountStore{}
	accts.rows = append(accts.rows,
		memAccountRow{userID: "id-sv-b", id: ownAcc, name: "TFSA", typ: "tfsa", currency: "CAD"},
		memAccountRow{userID: "id-sv-c", id: foreignAcc, name: "RRSP", typ: "rrsp", currency: "CAD"},
	)
	dash, wid := newMemDashStores()
	h, key := newSavingsDashServer(t, accts, dash, wid)
	tok := mintIdentityToken(t, key, "sv-b", "sv-b@example.com", "Savings B", "savingsuser")
	hdr := authHdr(tok)

	code, out := doReq(t, h, http.MethodPost, "/api/v1/dashboards", `{"name":"D"}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("setup: %d (%s)", code, out)
	}
	wpath := "/api/v1/dashboards/" + idFrom(t, out) + "/widgets"

	mk := func(body string) string {
		t.Helper()
		code, out := doReq(t, h, http.MethodPost, wpath, body, hdr)
		if code != http.StatusCreated {
			t.Fatalf("setup widget: %d (%s)", code, out)
		}
		return idFrom(t, out)
	}

	apprec := func(accID string) string {
		return `{"view":"appreciation","account_id":"` + accID + `"}`
	}

	t.Run("appreciation on own account", func(t *testing.T) {
		body := `{"name":"W","kind":"savings","content":` + strconv.Quote(apprec(ownAcc)) + `}`
		code, out := doReq(t, h, http.MethodPost, wpath, body, hdr)
		if code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (body %s)", code, out)
		}
	})

	t.Run("appreciation on missing account", func(t *testing.T) {
		body := `{"name":"W","kind":"savings","content":` + strconv.Quote(apprec(nextTestID())) + `}`
		code, out := doReq(t, h, http.MethodPost, wpath, body, hdr)
		if code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body %s)", code, out)
		}
		c, m := apiErr(t, out)
		if c != codeNotFound || m != "account not found" {
			t.Fatalf("got %q %q, want code %q message account not found", c, m, codeNotFound)
		}
	})

	t.Run("appreciation on foreign account", func(t *testing.T) {
		body := `{"name":"W","kind":"savings","content":` + strconv.Quote(apprec(foreignAcc)) + `}`
		code, out := doReq(t, h, http.MethodPost, wpath, body, hdr)
		if code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body %s)", code, out)
		}
		c, m := apiErr(t, out)
		if c != codeNotFound || m != "account not found" {
			t.Fatalf("got %q %q, want code %q message account not found", c, m, codeNotFound)
		}
	})

	t.Run("patch kind swap re-checks content", func(t *testing.T) {
		wid := mk(`{"name":"M","kind":"markdown","content":"hello"}`)
		body := `{"kind":"savings","content":` + strconv.Quote(apprec(nextTestID())) + `}`
		code, out := doReq(t, h, http.MethodPatch, wpath+"/"+wid, body, hdr)
		if code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body %s)", code, out)
		}
		c, m := apiErr(t, out)
		if c != codeNotFound || m != "account not found" {
			t.Fatalf("got %q %q, want code %q message account not found", c, m, codeNotFound)
		}
	})

	t.Run("patch to savings keeps existing content", func(t *testing.T) {
		wid := mk(`{"name":"M","kind":"markdown","content":"hello"}`)
		code, out := doReq(t, h, http.MethodPatch, wpath+"/"+wid, `{"kind":"savings"}`, hdr)
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", code, out)
		}
		if _, m := apiErr(t, out); m != `savings content must be "" or a JSON object` {
			t.Fatalf("message = %q", m)
		}
	})

	t.Run("patch savings content swaps", func(t *testing.T) {
		wid := mk(`{"name":"W","kind":"savings"}`)
		code, out := doReq(t, h, http.MethodPatch, wpath+"/"+wid, `{"content":`+strconv.Quote(apprec(ownAcc))+`}`, hdr)
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", code, out)
		}
		m := dashJSON(t, out)
		if m["content"] != apprec(ownAcc) {
			t.Fatalf("content = %v, want %v", m["content"], apprec(ownAcc))
		}
		code, out = doReq(t, h, http.MethodPatch, wpath+"/"+wid, `{"content":`+strconv.Quote(`{"view":"room"}`)+`}`, hdr)
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", code, out)
		}
		if m := dashJSON(t, out); m["content"] != `{"view":"room"}` {
			t.Fatalf("content = %v", m["content"])
		}
	})
}

// Transfer and external-origin route tests (round 52b2a): a POST
// /api/v1/transactions with transfer_from_account_id answers 201 with
// the two legs (debit on the source, credit on the destination) under
// one shared transfer_group_id; the transfer-specific 400s carry the
// repo's exact messages; the 404/409 reference and dedup paths behave
// as in the repo (a dedup conflict rolls the whole transfer back); and
// external_origin accepts the 0019 vocabulary on positive rows only.
package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

// transferOut is the transfer response: the two legs keyed "from"
// (the source debit, negative amount) and "to" (the destination
// credit, positive amount).
type transferOut struct {
	From transactionOut `json:"from"`
	To   transactionOut `json:"to"`
}

func TestTransferCreate(t *testing.T) {
	store := &memTxStore{}
	h, key := newTxServer(t, store, nil, nil)
	tok := mintIdentityToken(t, key, "tf-a", "tf-a@localhost", "T", "A")
	user := "id-tf-a"
	fromAcc := nextTestID()
	toAcc := nextTestID()
	stranger := nextTestID() // well-formed UUID the user does not own
	cat := nextTestID()
	store.markAccountNamed(user, fromAcc, "Chequings")
	store.markAccountNamed(user, toAcc, "TFSA")
	store.markCategory(user, cat)
	hdr := authHdr(tok)
	post := func(body string) (int, string) {
		return doReq(t, h, http.MethodPost, "/api/v1/transactions", body, hdr)
	}
	postedRows := func(t *testing.T, day string) []transactionOut {
		t.Helper()
		code, out := doReq(t, h, http.MethodGet, "/api/v1/transactions", "", hdr)
		if code != http.StatusOK {
			t.Fatalf("list: status = %d (body %s)", code, out)
		}
		var got []transactionOut
		for _, r := range txList(t, out) {
			if r.DatePosted == day {
				got = append(got, r)
			}
		}
		return got
	}
	decodeLegs := func(t *testing.T, out string) transferOut {
		t.Helper()
		var legs transferOut
		if err := json.Unmarshal([]byte(out), &legs); err != nil {
			t.Fatalf("decode: %v (body %s)", err, out)
		}
		return legs
	}

	t.Run("201 both legs one group", func(t *testing.T) {
		body := `{"account_id":"` + toAcc + `","date_executed":"2026-01-03","date_posted":"2026-01-05","amount":"10.5","description":"rent split","notes":"n","raw":"r","review_pending":true,"transfer_from_account_id":"` + fromAcc + `"}`
		code, out := post(body)
		if code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (body %s)", code, out)
		}
		legs := decodeLegs(t, out)
		if legs.From.AccountID != fromAcc || legs.From.Amount != "-10.50" ||
			legs.To.AccountID != toAcc || legs.To.Amount != "10.50" {
			t.Fatalf("legs mismatch: %+v", legs)
		}
		if legs.From.TransferGroupID == nil || legs.To.TransferGroupID == nil ||
			*legs.From.TransferGroupID == "" || *legs.From.TransferGroupID != *legs.To.TransferGroupID {
			t.Errorf("group mismatch: from %v to %v", legs.From.TransferGroupID, legs.To.TransferGroupID)
		}
		for name, leg := range map[string]transactionOut{"from": legs.From, "to": legs.To} {
			if leg.DatePosted != "2026-01-05" || leg.DateExecuted == nil || *leg.DateExecuted != "2026-01-03" ||
				leg.Description != "rent split" || leg.CategoryID != nil || leg.ExternalOrigin != nil ||
				leg.Notes == nil || *leg.Notes != "n" || leg.Raw == nil || *leg.Raw != "r" || !leg.ReviewPending {
				t.Errorf("%s leg mismatch: %+v", name, leg)
			}
		}
	})

	t.Run("auto memos name the counterparty", func(t *testing.T) {
		body := `{"account_id":"` + toAcc + `","date_posted":"2026-01-06","amount":"25","transfer_from_account_id":"` + fromAcc + `"}`
		code, out := post(body)
		if code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (body %s)", code, out)
		}
		legs := decodeLegs(t, out)
		if legs.From.Description != "Transfer to TFSA" || legs.To.Description != "Transfer from Chequings" {
			t.Errorf("memos = %q / %q", legs.From.Description, legs.To.Description)
		}
	})

	t.Run("zero amount is 400", func(t *testing.T) {
		body := `{"account_id":"` + toAcc + `","date_posted":"2026-01-07","amount":"0.00","transfer_from_account_id":"` + fromAcc + `"}`
		code, out := post(body)
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeInvalidRequest || m != "transfer amount must be positive" {
			t.Errorf("error = %s/%q", c, m)
		}
	})

	t.Run("negative amount is 400", func(t *testing.T) {
		body := `{"account_id":"` + toAcc + `","date_posted":"2026-01-07","amount":"-3","description":"neg","transfer_from_account_id":"` + fromAcc + `"}`
		code, out := post(body)
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeInvalidRequest || m != "transfer amount must be positive" {
			t.Errorf("error = %s/%q", c, m)
		}
	})

	t.Run("same account is 400", func(t *testing.T) {
		body := `{"account_id":"` + toAcc + `","date_posted":"2026-01-07","amount":"1","transfer_from_account_id":"` + toAcc + `"}`
		code, out := post(body)
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeInvalidRequest || m != "transfer source and destination must be different accounts" {
			t.Errorf("error = %s/%q", c, m)
		}
	})

	t.Run("bad source uuid is 400", func(t *testing.T) {
		body := `{"account_id":"` + toAcc + `","date_posted":"2026-01-07","amount":"1","description":"bad","transfer_from_account_id":"not-a-uuid"}`
		code, out := post(body)
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeInvalidRequest || m != "transfer_from_account_id must be a valid UUID" {
			t.Errorf("error = %s/%q", c, m)
		}
	})

	t.Run("empty source is 400", func(t *testing.T) {
		body := `{"account_id":"` + toAcc + `","date_posted":"2026-01-07","amount":"1","description":"empty","transfer_from_account_id":""}`
		code, out := post(body)
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeInvalidRequest || m != "transfer_from_account_id must be a valid UUID" {
			t.Errorf("error = %s/%q", c, m)
		}
	})

	t.Run("category is 400", func(t *testing.T) {
		body := `{"account_id":"` + toAcc + `","date_posted":"2026-01-07","amount":"1","description":"cat","category_id":"` + cat + `","transfer_from_account_id":"` + fromAcc + `"}`
		code, out := post(body)
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeInvalidRequest || m != "category_id is not allowed on a transfer" {
			t.Errorf("error = %s/%q", c, m)
		}
	})

	t.Run("origin is 400", func(t *testing.T) {
		body := `{"account_id":"` + toAcc + `","date_posted":"2026-01-07","amount":"1","description":"org","external_origin":"employer","transfer_from_account_id":"` + fromAcc + `"}`
		code, out := post(body)
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeInvalidRequest || m != "external_origin is not allowed on a transfer" {
			t.Errorf("error = %s/%q", c, m)
		}
	})

	t.Run("unowned source is 404", func(t *testing.T) {
		body := `{"account_id":"` + toAcc + `","date_posted":"2026-01-07","amount":"1","description":"str1","transfer_from_account_id":"` + stranger + `"}`
		code, out := post(body)
		if code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeNotFound || m != "account not found" {
			t.Errorf("error = %s/%q", c, m)
		}
	})

	t.Run("unowned destination is 404", func(t *testing.T) {
		body := `{"account_id":"` + stranger + `","date_posted":"2026-01-07","amount":"1","description":"str2","transfer_from_account_id":"` + fromAcc + `"}`
		code, out := post(body)
		if code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeNotFound || m != "account not found" {
			t.Errorf("error = %s/%q", c, m)
		}
	})

	t.Run("date rule is 400", func(t *testing.T) {
		body := `{"account_id":"` + toAcc + `","date_executed":"2026-01-06","date_posted":"2026-01-05","amount":"1","description":"late","transfer_from_account_id":"` + fromAcc + `"}`
		code, out := post(body)
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeInvalidRequest || m != "date_executed must not be after date_posted" {
			t.Errorf("error = %s/%q", c, m)
		}
	})

	t.Run("dedup conflict is 409 and rolls back", func(t *testing.T) {
		body := `{"account_id":"` + toAcc + `","date_posted":"2026-01-08","amount":"5","description":"dup","transfer_from_account_id":"` + fromAcc + `"}`
		code, out := post(body)
		if code != http.StatusCreated {
			t.Fatalf("first: status = %d, want 201 (body %s)", code, out)
		}
		code, out = post(body)
		if code != http.StatusConflict {
			t.Fatalf("second: status = %d, want 409 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeConflict || m != "a transaction with this dedup key already exists" {
			t.Errorf("error = %s/%q", c, m)
		}
		rows := postedRows(t, "2026-01-08")
		if len(rows) != 2 {
			t.Errorf("2026-01-08 rows = %d, want 2 (the first transfer's legs only): %+v", len(rows), rows)
		}
	})

	t.Run("sibling row blocks the whole transfer", func(t *testing.T) {
		// A normal row on the destination with the identity the credit
		// leg would take.
		seedTx(t, store, user, toAcc, "", "2026-01-09", "5.00", "dup2", "")
		body := `{"account_id":"` + toAcc + `","date_posted":"2026-01-09","amount":"5","description":"dup2","transfer_from_account_id":"` + fromAcc + `"}`
		code, out := post(body)
		if code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 (body %s)", code, out)
		}
		for _, r := range postedRows(t, "2026-01-09") {
			if r.AccountID == fromAcc {
				t.Errorf("debit leg persisted despite the conflict: %+v", r)
			}
		}
	})
}

func TestOriginCreate(t *testing.T) {
	store := &memTxStore{}
	h, key := newTxServer(t, store, nil, nil)
	tok := mintIdentityToken(t, key, "or-a", "or-a@localhost", "O", "R")
	user := "id-or-a"
	acc := nextTestID()
	store.markAccount(user, acc)
	hdr := authHdr(tok)
	post := func(body string) (int, string) {
		return doReq(t, h, http.MethodPost, "/api/v1/transactions", body, hdr)
	}

	t.Run("vocabulary accepted and round-trips", func(t *testing.T) {
		for _, tc := range []struct{ date, origin string }{
			{"2026-02-01", "employer"},
			{"2026-02-02", "government_grant"},
			{"2026-02-03", "inherited"},
			{"2026-02-04", "other"},
		} {
			body := `{"account_id":"` + acc + `","date_posted":"` + tc.date + `","amount":"100","external_origin":"` + tc.origin + `","description":"origin ` + tc.origin + `"}`
			code, out := post(body)
			if code != http.StatusCreated {
				t.Fatalf("%s: status = %d, want 201 (body %s)", tc.origin, code, out)
			}
			var row transactionOut
			if err := json.Unmarshal([]byte(out), &row); err != nil {
				t.Fatalf("%s: decode: %v", tc.origin, err)
			}
			if row.ExternalOrigin == nil || *row.ExternalOrigin != tc.origin {
				t.Errorf("%s: origin = %v, want %q", tc.origin, row.ExternalOrigin, tc.origin)
			}
		}
		// List round trip: the employer row still carries its origin.
		code, out := doReq(t, h, http.MethodGet, "/api/v1/transactions", "", hdr)
		if code != http.StatusOK {
			t.Fatalf("list: status = %d (body %s)", code, out)
		}
		found := false
		for _, r := range txList(t, out) {
			if r.ExternalOrigin != nil && *r.ExternalOrigin == "employer" {
				found = true
			}
		}
		if !found {
			t.Errorf("employer origin missing from list")
		}
	})

	t.Run("blank and null are unset", func(t *testing.T) {
		code, out := post(`{"account_id":"` + acc + `","date_posted":"2026-02-05","amount":"10","external_origin":"","description":"blank origin"}`)
		if code != http.StatusCreated {
			t.Fatalf("blank: status = %d, want 201 (body %s)", code, out)
		}
		var row transactionOut
		if err := json.Unmarshal([]byte(out), &row); err != nil {
			t.Fatalf("blank: decode: %v", err)
		}
		if row.ExternalOrigin != nil {
			t.Errorf("blank: origin = %v, want null", row.ExternalOrigin)
		}
		code, out = post(`{"account_id":"` + acc + `","date_posted":"2026-02-05","amount":"10","external_origin":null,"description":"null origin"}`)
		if code != http.StatusCreated {
			t.Fatalf("null: status = %d, want 201 (body %s)", code, out)
		}
		if err := json.Unmarshal([]byte(out), &row); err != nil {
			t.Fatalf("null: decode: %v", err)
		}
		if row.ExternalOrigin != nil {
			t.Errorf("null: origin = %v, want null", row.ExternalOrigin)
		}
	})

	t.Run("normal row stays null", func(t *testing.T) {
		body := `{"account_id":"` + acc + `","date_posted":"2026-02-06","amount":"10","description":"no origin"}`
		code, out := post(body)
		if code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (body %s)", code, out)
		}
		var row transactionOut
		if err := json.Unmarshal([]byte(out), &row); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if row.ExternalOrigin != nil {
			t.Errorf("origin = %v, want null", row.ExternalOrigin)
		}
	})

	t.Run("bad value is 400", func(t *testing.T) {
		body := `{"account_id":"` + acc + `","date_posted":"2026-02-07","amount":"10","external_origin":"employerx","description":"bad origin"}`
		code, out := post(body)
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeInvalidRequest || m != "external_origin must be one of: employer, government_grant, inherited, other" {
			t.Errorf("error = %s/%q", c, m)
		}
	})

	t.Run("non-positive amount is 400", func(t *testing.T) {
		for _, amount := range []string{"0", "-5"} {
			body := `{"account_id":"` + acc + `","date_posted":"2026-02-08","amount":"` + amount + `","external_origin":"employer","description":"zero origin"}`
			code, out := post(body)
			if code != http.StatusBadRequest {
				t.Fatalf("%s: status = %d, want 400 (body %s)", amount, code, out)
			}
			if c, m := apiErr(t, out); c != codeInvalidRequest || m != "external_origin requires a positive amount" {
				t.Errorf("%s: error = %s/%q", amount, c, m)
			}
		}
	})
}

// TestTransferPatchImmutability pins the 52b2b guard: a transfer leg
// answers 409 for any patch to amount/date_posted/date_executed (both
// legs, exact message), while description-only patches and patches on
// ordinary rows still succeed, and a missing row still 404s through
// the guard's Get.
func TestTransferPatchImmutability(t *testing.T) {
	store := &memTxStore{}
	h, key := newTxServer(t, store, nil, nil)
	tok := mintIdentityToken(t, key, "tf-b", "tf-b@localhost", "T", "B")
	user := "id-tf-b"
	fromAcc := nextTestID()
	toAcc := nextTestID()
	store.markAccount(user, fromAcc)
	store.markAccount(user, toAcc)
	hdr := authHdr(tok)

	body := `{"account_id":"` + toAcc + `","date_posted":"2026-03-05","amount":"40","description":"m","transfer_from_account_id":"` + fromAcc + `"}`
	code, out := doReq(t, h, http.MethodPost, "/api/v1/transactions", body, hdr)
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", code, out)
	}
	var legs transferOut
	if err := json.Unmarshal([]byte(out), &legs); err != nil {
		t.Fatalf("decode: %v (body %s)", err, out)
	}
	patch := func(id, body string) (int, string) {
		return doReq(t, h, http.MethodPatch, "/api/v1/transactions/"+id, body, hdr)
	}

	for _, tc := range []struct{ field string }{
		{"amount"},
		{"date_posted"},
		{"date_executed"},
	} {
		t.Run(tc.field+" is 409", func(t *testing.T) {
			var body string
			switch tc.field {
			case "amount":
				body = `{"amount":"99.99"}`
			case "date_posted":
				body = `{"date_posted":"2026-03-06"}`
			case "date_executed":
				body = `{"date_executed":"2026-03-04"}`
			}
			code, out := patch(legs.From.ID, body)
			if code != http.StatusConflict {
				t.Fatalf("status = %d, want 409 (body %s)", code, out)
			}
			if c, m := apiErr(t, out); c != codeConflict || m != "transfer legs cannot be modified: amount, date_posted and date_executed are immutable" {
				t.Errorf("error = %s/%q", c, m)
			}
		})
	}

	t.Run("the to leg is guarded too", func(t *testing.T) {
		code, out := patch(legs.To.ID, `{"amount":"1"}`)
		if code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 (body %s)", code, out)
		}
	})

	t.Run("description-only patch still allowed", func(t *testing.T) {
		code, out := patch(legs.From.ID, `{"description":"renamed leg"}`)
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", code, out)
		}
		var row transactionOut
		if err := json.Unmarshal([]byte(out), &row); err != nil {
			t.Fatalf("decode: %v (body %s)", err, out)
		}
		if row.Description != "renamed leg" || row.TransferGroupID == nil {
			t.Errorf("row = %+v", row)
		}
	})

	t.Run("a normal row amount patch is untouched by the guard", func(t *testing.T) {
		normal := seedTx(t, store, user, fromAcc, "", "2026-03-06", "-5.00", "plain", "")
		code, out := patch(normal.ID, `{"amount":"-6.00"}`)
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", code, out)
		}
	})

	t.Run("404 on a missing row is not shadowed", func(t *testing.T) {
		code, out := patch(nextTestID(), `{"amount":"1"}`)
		if code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeNotFound || m != "transaction not found" {
			t.Errorf("error = %s/%q", c, m)
		}
	})
}

// TestTransferDeletePartner pins the delete path: removing one leg
// answers 204, the partner leg survives with its transfer_group_id
// cleared (the round-52b2a fake mirrors the repo's ungrouping), and
// the survivor's amount, dates, and memo are untouched.
func TestTransferDeletePartner(t *testing.T) {
	store := &memTxStore{}
	h, key := newTxServer(t, store, nil, nil)
	tok := mintIdentityToken(t, key, "tf-c", "tf-c@localhost", "T", "C")
	user := "id-tf-c"
	fromAcc := nextTestID()
	toAcc := nextTestID()
	store.markAccountNamed(user, fromAcc, "Chequings")
	store.markAccountNamed(user, toAcc, "TFSA")
	hdr := authHdr(tok)

	body := `{"account_id":"` + toAcc + `","date_posted":"2026-03-07","amount":"30","transfer_from_account_id":"` + fromAcc + `"}`
	code, out := doReq(t, h, http.MethodPost, "/api/v1/transactions", body, hdr)
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", code, out)
	}
	var legs transferOut
	if err := json.Unmarshal([]byte(out), &legs); err != nil {
		t.Fatalf("decode: %v (body %s)", err, out)
	}

	code, out = doReq(t, h, http.MethodDelete, "/api/v1/transactions/"+legs.From.ID, "", hdr)
	if code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body %s)", code, out)
	}

	code, out = doReq(t, h, http.MethodGet, "/api/v1/transactions", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("list: status = %d (body %s)", code, out)
	}
	rows := txList(t, out)
	var survivor *transactionOut
	for _, r := range rows {
		if r.ID == legs.To.ID {
			c := r
			survivor = &c
		}
		if r.ID == legs.From.ID {
			t.Errorf("deleted leg still listed: %+v", r)
		}
	}
	if survivor == nil {
		t.Fatalf("survivor leg missing from list: %s", out)
	}
	if survivor.TransferGroupID != nil {
		t.Errorf("group = %v, want null (the partner was deleted)", *survivor.TransferGroupID)
	}
	if survivor.Amount != "30.00" || survivor.DatePosted != "2026-03-07" || survivor.Description != "Transfer from Chequings" {
		t.Errorf("survivor mutated: %+v", survivor)
	}
}

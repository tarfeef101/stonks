// API error envelope and shared request helpers (plan step 3.1). Every
// non-2xx JSON response from /api/v1 uses the shape
// {"error": {"code", "message"}}; the codes are the consts below (the
// frontend, step 1.4b, parses them on errors). The settings routes
// (1.4a) adopted the envelope early; 3.1 standardizes it for the
// accounts and categories routes and moves the shared helpers here.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// Error codes carried in the envelope.
const (
	codeInvalidRequest  = "invalid_request"
	codeUnauthorized    = "unauthorized"
	codeNotFound        = "not_found"
	codeConflict        = "conflict"
	codePayloadTooLarge = "payload_too_large"
	codeInternal        = "internal"
)

type errorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// writeAPIError writes the JSON error envelope with the given status.
func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	var env errorEnvelope
	env.Error.Code = code
	env.Error.Message = message
	writeJSON(w, status, env)
}

// notConfigured answers 503 when a store is unavailable because no
// database pool was configured (main.go leaves the store nil).
func notConfigured(w http.ResponseWriter) {
	writeAPIError(w, http.StatusServiceUnavailable, codeInternal, "database not configured")
}

// maxNameLen is the name limit for accounts.name and
// user_categories.name (VARCHAR(255) in 0001/0002).
const maxNameLen = 255

// maxSubtypeLen is the accounts.subtype limit (0002); the repo package
// enforces the same rule at the DB boundary, the handlers 400 first.
const maxSubtypeLen = 64

// checkName mirrors repo.validateName (message strings included) so a
// client sees the same 400 the DB layer would have produced as a 500.
func checkName(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("name must not be empty")
	}
	if utf8.RuneCountInString(name) > maxNameLen {
		return fmt.Errorf("name must be at most %d characters", maxNameLen)
	}
	return nil
}

// uuidShapeOK reports whether s looks like a uuid column value (36 chars,
// dashes at 8/13/18/23, hex digits in either case). The handlers use it
// to reject malformed ids before the DB turns them into 22P02
// (invalid_text_representation) errors that could only surface as 500.
func uuidShapeOK(s string) bool {
	if len(s) != 36 {
		return false
	}
	for _, d := range []int{8, 13, 18, 23} {
		if s[d] != '-' {
			return false
		}
	}
	isHex := func(i, n int) bool {
		for k := i; k < i+n; k++ {
			c := s[k]
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
				return false
			}
		}
		return true
	}
	return isHex(0, 8) && isHex(9, 4) && isHex(14, 4) && isHex(19, 4) && isHex(24, 12)
}

// decodeBody decodes the request body into v. The body is capped at 1 MiB
// (no payload we accept comes close) so a slow client cannot stall a
// handler.
func decodeBody(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

// triStateString is a JSON field that can be absent (no change), null
// (clear the column / demote to root), or a string (new value). A plain
// *T field cannot express the three states — JSON null nils the pointer
// without calling UnmarshalJSON — so the tri-state rides on a
// non-pointer struct with an explicit set flag.
type triStateString struct {
	set   bool
	value *string
}

// UnmarshalJSON implements json.Unmarshaler. Go delivers "null" here
// (unlike pointer fields), which is what lets us tell null from absent.
func (t *triStateString) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		t.set = true
		t.value = nil
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("must be a string or null: %w", err)
	}
	t.set = true
	t.value = &s
	return nil
}

// toRepo converts the tri-state to the repo's **string idiom: absent →
// nil (untouched), present → pointer (nil inner = clear).
func (t triStateString) toRepo() **string {
	if !t.set {
		return nil
	}
	return &t.value
}

// triStateDate is the PATCH /api/v1/transactions/{id} (step 3.2)
// tri-state for date_executed: absent or null = clear to NULL, a
// "YYYY-MM-DD" string = set. It rides a non-pointer struct with an
// explicit set flag for the same reason as triStateString — JSON null
// nils a plain pointer field without calling UnmarshalJSON.
type triStateDate struct {
	set   bool
	value *time.Time
}

// UnmarshalJSON implements json.Unmarshaler. "null" clears the date;
// anything else must be a strictly-shaped YYYY-MM-DD string.
func (t *triStateDate) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		t.set = true
		t.value = nil
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("must be a date string or null: %w", err)
	}
	d, err := parseDateParam("date_executed", s)
	if err != nil {
		return err
	}
	t.set = true
	t.value = &d
	return nil
}

// toRepo converts the tri-state to the repo's **time.Time idiom:
// absent → nil (untouched), present → pointer (nil inner = clear).
func (t triStateDate) toRepo() **time.Time {
	if !t.set {
		return nil
	}
	return &t.value
}

// strOrNil renders an unset ("" ) repo column as JSON null.
func strOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

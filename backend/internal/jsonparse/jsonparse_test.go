package jsonparse

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// flatSpec addresses a root array of flat items.
var flatSpec = Spec{Fields: []FieldSpec{
	{Name: "date", Pointers: []string{"/date"}},
	{Name: "amount", Pointers: []string{"/amount"}},
	{Name: "description", Pointers: []string{"/description"}},
}}

func mustParse(t *testing.T, in string, spec Spec) *File {
	t.Helper()
	f, err := Parse(strings.NewReader(in), spec)
	if err != nil {
		t.Fatalf("Parse(%q) error: %v", in, err)
	}
	return f
}

func wantRow(t *testing.T, i int, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("row[%d] has %d fields (%v), want %d", i, len(got), got, len(want))
	}
	for j := range want {
		if got[j] != want[j] {
			t.Fatalf("row[%d][%d] = %q, want %q", i, j, got[j], want[j])
		}
	}
}

// TestParseTooLarge: a document over maxParseBytes is refused
// before the decoder sees it, and a document within the cap still
// takes the usual path (T-P0-3).
func TestParseTooLarge(t *testing.T) {
	old := maxParseBytes
	maxParseBytes = 10
	defer func() { maxParseBytes = old }()

	_, err := Parse(strings.NewReader(`[{"date":"1"}]`), flatSpec)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Parse error = %v, want ErrTooLarge", err)
	}
	_, err = Parse(strings.NewReader("[]"), flatSpec)
	if !errors.Is(err, ErrEmpty) {
		t.Fatalf("Parse error = %v, want ErrEmpty (in-cap path)", err)
	}
}

func TestParseRootArray(t *testing.T) {
	f := mustParse(t, `[
		{"date": "2026-01-05", "amount": -45.23, "description": "GROCERY"},
		{"date": "2026-01-06", "amount": 2500.00, "description": "PAYROLL"}
	]`, flatSpec)
	if len(f.Rows) != 2 {
		t.Fatalf("len(Rows) = %d, want 2: %v", len(f.Rows), f.Rows)
	}
	wantRow(t, 0, f.Rows[0], "2026-01-05", "-45.23", "GROCERY")
	wantRow(t, 1, f.Rows[1], "2026-01-06", "2500.00", "PAYROLL")
}

func TestParseNestedArrayAndFallbacks(t *testing.T) {
	// Root is an object: the array lives at /transactions, found via
	// the second pointer (root array is tried and misses first).
	spec := Spec{
		ArrayPointers: []string{"/records", "/transactions"},
		Fields: []FieldSpec{
			{Name: "amount", Pointers: []string{"/amount"}},
			{Name: "date", Pointers: []string{"/postedAt", "/date"}},
		},
	}
	in := `{
		"account": "CA 123",
		"transactions": [
			{"postedAt": "2026-01-05T12:30:00Z", "amount": -12.99},
			{"date": "2026-01-06", "amount": 88.00}
		]
	}`
	f := mustParse(t, in, spec)
	if len(f.Rows) != 2 {
		t.Fatalf("len(Rows) = %d, want 2: %v", len(f.Rows), f.Rows)
	}
	// Row 0: /postedAt wins over /date; row 1: /postedAt missing -> /date.
	wantRow(t, 0, f.Rows[0], "-12.99", "2026-01-05T12:30:00Z")
	wantRow(t, 1, f.Rows[1], "88.00", "2026-01-06")
}

func TestParseMissingAndNonScalar(t *testing.T) {
	// Missing key -> ""; null -> try the next pointer; object value ->
	// "" (a field pointing at a nested object is a spec mismatch, not
	// a parse error).
	spec := Spec{Fields: []FieldSpec{
		{Name: "amount", Pointers: []string{"/amount", "/value"}},
		{Name: "meta", Pointers: []string{"/meta"}},
	}}
	in := `[
		{"value": 42.5, "meta": {"k": "v"}},
		{"amount": null}
	]`
	f := mustParse(t, in, spec)
	if len(f.Rows) != 2 {
		t.Fatalf("len(Rows) = %d, want 2: %v", len(f.Rows), f.Rows)
	}
	wantRow(t, 0, f.Rows[0], "42.5", "") // /amount missing -> /value; object -> ""
	wantRow(t, 1, f.Rows[1], "", "")     // /amount null -> /value missing
}

func TestParseNumbersStayRaw(t *testing.T) {
	// UseNumber keeps the file spelling: no float64 rounding of long
	// numerics, no 1e+21 scientific notation, no 45.2 trailing zero.
	spec := Spec{Fields: []FieldSpec{
		{Name: "amount", Pointers: []string{"/amount"}},
		{Name: "acct", Pointers: []string{"/acct"}},
		{Name: "big", Pointers: []string{"/big"}},
	}}
	in := `[{"amount": 45.20, "acct": 12345678901234567890, "big": 123456789012345678901}]`
	f := mustParse(t, in, spec)
	wantRow(t, 0, f.Rows[0], "45.20", "12345678901234567890", "123456789012345678901")
}

func TestParseNestedFieldPointer(t *testing.T) {
	spec := Spec{Fields: []FieldSpec{
		{Name: "date", Pointers: []string{"/posted/at"}},
		{Name: "amt", Pointers: []string{"/money/amt"}},
	}}
	in := `[{"posted": {"at": "2026-01-05"}, "money": {"amt": -3.5}}]`
	f := mustParse(t, in, spec)
	wantRow(t, 0, f.Rows[0], "2026-01-05", "-3.5")
}

func TestParseRFCEscapedKeys(t *testing.T) {
	// Keys containing "/" and "~" are legal via RFC 6901 escapes:
	// "a/b" -> /a~1b ; "a~b" -> /a~0b ; literal "~1" key -> /~01.
	spec := Spec{Fields: []FieldSpec{
		{Name: "slash", Pointers: []string{"/a~1b"}},
		{Name: "tilde", Pointers: []string{"/a~0b"}},
		{Name: "one", Pointers: []string{"/~01"}},
	}}
	in := `[{"a/b": "s", "a~b": "t", "~1": "o"}]`
	f := mustParse(t, in, spec)
	wantRow(t, 0, f.Rows[0], "s", "t", "o")
}

func TestParseSkipsNonObjectItems(t *testing.T) {
	// A mixed array: non-object items are skipped, objects survive in
	// order.
	f := mustParse(t, `[42, "x", {"date": "2026-01-05", "amount": 1, "description": "A"}, null, {"date": "2026-01-06", "amount": 2, "description": "B"}]`, flatSpec)
	if len(f.Rows) != 2 {
		t.Fatalf("len(Rows) = %d, want 2: %v", len(f.Rows), f.Rows)
	}
	wantRow(t, 0, f.Rows[0], "2026-01-05", "1", "A")
	wantRow(t, 1, f.Rows[1], "2026-01-06", "2", "B")
}

func TestParseIgnoresTrailingData(t *testing.T) {
	// One JSON document is enough; a stray second value after it is
	// ignored (some exporters append whitespace junk).
	f := mustParse(t, `[{"date": "2026-01-05", "amount": 1, "description": "A"}] trailing-junk`, flatSpec)
	if len(f.Rows) != 1 {
		t.Fatalf("len(Rows) = %d, want 1: %v", len(f.Rows), f.Rows)
	}
}

func TestParseEmpty(t *testing.T) {
	cases := []struct {
		name string
		in   string
		spec Spec
	}{
		{"no array found", `{"transactions": "not-an-array"}`, Spec{ArrayPointers: []string{"/transactions"}, Fields: flatSpec.Fields}},
		{"root object, no pointers", `{"no": "array here"}`, flatSpec},
		{"empty array", `[]`, flatSpec},
		{"empty nested array", `{"transactions": []}`, Spec{ArrayPointers: []string{"/transactions"}, Fields: flatSpec.Fields}},
		{"only non-object items", `[1, 2, 3]`, flatSpec},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(c.in), c.spec)
			if !errors.Is(err, ErrEmpty) {
				t.Errorf("Parse err = %v, want ErrEmpty", err)
			}
		})
	}
}

func TestParseMalformedJSON(t *testing.T) {
	_, err := Parse(strings.NewReader(`{"date": `), flatSpec)
	if err == nil || errors.Is(err, ErrEmpty) {
		t.Fatalf("err = %v, want a hard JSON error, not nil or ErrEmpty", err)
	}
}

func TestResolve(t *testing.T) {
	root := map[string]any{
		"a": []any{map[string]any{"b": float64(7)}, "s"},
		"t": map[string]any{"deep": []any{"x"}},
	}
	cases := []struct {
		pointer string
		want    any
		found   bool
	}{
		{"", root, true},
		{"/a", root["a"], true},
		{"/a/0/b", float64(7), true},
		{"/a/1", "s", true},
		{"/a/2", nil, false},     // out of range
		{"/a/x", nil, false},     // non-numeric index
		{"/t/deep/0", "x", true}, // object then array
		{"/a/0/b/c", nil, false}, // scalar has no children
		{"/missing", nil, false},
		{"/a/", nil, false}, // trailing "/" -> token ""
		{"a/0", nil, false}, // must start with "/"
	}
	for _, c := range cases {
		got, found := resolve(root, c.pointer)
		if found != c.found || !reflect.DeepEqual(got, c.want) {
			t.Errorf("resolve(%q) = (%v, %v), want (%v, %v)", c.pointer, got, found, c.want, c.found)
		}
	}
}

func TestUnescapeRef(t *testing.T) {
	cases := map[string]string{
		"plain":  "plain",
		"a~1b":   "a/b",
		"a~0b":   "a~b",
		"~01":    "~1", // a literal "~1" key, encoded per RFC 6901
		"~1~0":   "/~",
		"a~b~1c": "a~b/c",
	}
	for in, want := range cases {
		if got := unescapeRef(in); got != want {
			t.Errorf("unescapeRef(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSpecValid(t *testing.T) {
	ok := Spec{
		ArrayPointers: []string{"", "/transactions"},
		Fields: []FieldSpec{
			{Name: "amount", Pointers: []string{"/amount", "/value"}},
			{Name: "date", Pointers: []string{"/date"}},
		},
	}
	if err := ok.Valid(); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	bad := []struct {
		name string
		spec Spec
	}{
		{"no fields", Spec{}},
		{"empty field name", Spec{Fields: []FieldSpec{{Name: "", Pointers: []string{"/x"}}}}},
		{"duplicate field name", Spec{Fields: []FieldSpec{
			{Name: "a", Pointers: []string{"/x"}},
			{Name: "a", Pointers: []string{"/y"}},
		}}},
		{"field without pointers", Spec{Fields: []FieldSpec{{Name: "a"}}}},
		{"field pointer not rooted", Spec{Fields: []FieldSpec{{Name: "a", Pointers: []string{"amount"}}}}},
		{"array pointer not rooted", Spec{ArrayPointers: []string{"transactions"}, Fields: ok.Fields}},
	}
	for _, c := range bad {
		if err := c.spec.Valid(); err == nil {
			t.Errorf("%s: Valid() = nil, want error", c.name)
		}
	}
}

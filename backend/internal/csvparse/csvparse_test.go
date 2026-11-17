package csvparse

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"unicode/utf16"
)

func mustParse(t *testing.T, in string) *File {
	t.Helper()
	f, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Parse(%q) error: %v", in, err)
	}
	return f
}

// TestParseMaxRows: more than MaxRows raw records is an error. The
// counter includes the header and all-empty records, so with
// MaxRows=2 the header + one data row parse, and a third record
// trips the cap (T-P0-3).
func TestParseMaxRows(t *testing.T) {
	old := MaxRows
	MaxRows = 2
	defer func() { MaxRows = old }()

	f := mustParse(t, "a,b\n1,2\n")
	if len(f.Rows) != 1 {
		t.Fatalf("Rows = %d, want 1", len(f.Rows))
	}
	_, err := Parse(strings.NewReader("a,b\n1,2\n3,4\n"))
	if !errors.Is(err, ErrTooManyRows) {
		t.Fatalf("Parse error = %v, want ErrTooManyRows", err)
	}
}

func TestParseCommaHeader(t *testing.T) {
	f := mustParse(t, "Date,Description,Amount\n01/05/2026,Coffee,-4.50\n01/06/2026,Salary,1800.00\n")
	if f.Delimiter != ',' || f.Encoding != "utf-8" {
		t.Fatalf("transport = (%q, %q), want (, utf-8)", f.Delimiter, f.Encoding)
	}
	if !f.HasHeader || len(f.Header) != 3 || f.Header[0] != "Date" {
		t.Fatalf("Header = %v, want [Date Description Amount]", f.Header)
	}
	if len(f.Rows) != 2 || f.Rows[0][1] != "Coffee" {
		t.Fatalf("Rows = %v", f.Rows)
	}
	kinds := []ColumnKind{KindDate, KindText, KindAmount}
	if len(f.Columns) != 3 {
		t.Fatalf("Columns = %+v", f.Columns)
	}
	for i, want := range kinds {
		if f.Columns[i].Kind != want {
			t.Errorf("column %d kind = %v, want %v", i, f.Columns[i].Kind, want)
		}
	}
	if f.Columns[0].DateFormat != "mm-dd-yyyy" {
		t.Errorf("DateFormat = %q, want mm-dd-yyyy", f.Columns[0].DateFormat)
	}
}

func TestParseDelimiterSniffing(t *testing.T) {
	f := mustParse(t, "Date;Description;Amount\n05/01/2026;Café;-12.50\n")
	if f.Delimiter != ';' {
		t.Errorf("Delimiter = %q, want ;", f.Delimiter)
	}
	if f.Header[1] != "Description" {
		t.Errorf("Header = %v (semicolon not respected?)", f.Header)
	}

	tab := "Date\tDescription\tAmount\n01/05/2026\tCoffee\t-4.50\n"
	f = mustParse(t, tab)
	if f.Delimiter != '\t' {
		t.Errorf("Delimiter = %q, want tab", f.Delimiter)
	}
	if len(f.Columns) != 3 || f.Header[2] != "Amount" {
		t.Errorf("tab file mis-parsed: Header=%v Columns=%d", f.Header, len(f.Columns))
	}
}

func TestParseQuoting(t *testing.T) {
	f := mustParse(t, "Description,Amount\n\"Grocery Store, Ltd.\",-85.40\n\"He said \"\"hi\"\"\",-1.00\n")
	if len(f.Rows) != 2 {
		t.Fatalf("Rows = %v", f.Rows)
	}
	if f.Rows[0][0] != "Grocery Store, Ltd." {
		t.Errorf("quoted comma field = %q", f.Rows[0][0])
	}
	if f.Rows[1][0] != `He said "hi"` {
		t.Errorf("escaped quotes = %q", f.Rows[1][0])
	}
	// A stray quote mid-field (LazyQuotes) must not lose the row.
	f = mustParse(t, "Description,Amount\nCoffee's spot,-4.50\n")
	if len(f.Rows) != 1 || f.Rows[0][0] != "Coffee's spot" {
		t.Errorf("lazy-quote row = %v", f.Rows)
	}
}

func TestParseEncodings(t *testing.T) {
	// UTF-8 BOM: stripped, label utf-8.
	bom := "\ufeffDate,Description,Amount\n01/05/2026,Café,-4.50\n"
	f := mustParse(t, bom)
	if f.Encoding != "utf-8" || f.Header[0] != "Date" {
		t.Fatalf("BOM: Encoding=%q Header[0]=%q", f.Encoding, f.Header[0])
	}
	if f.Rows[0][1] != "Café" {
		t.Errorf("BOM row = %v", f.Rows[0])
	}

	body := "Date,Description,Amount\n01/05/2026,Café,-4.50\n"
	units := utf16.Encode([]rune(body))
	le := make([]byte, 2+2*len(units))
	le[0], le[1] = 0xFF, 0xFE
	be := make([]byte, 2+2*len(units))
	be[0], be[1] = 0xFE, 0xFF
	for i, u := range units {
		le[2+2*i] = byte(u)
		le[3+2*i] = byte(u >> 8)
		be[2+2*i] = byte(u >> 8)
		be[3+2*i] = byte(u)
	}
	got, err := Parse(bytes.NewReader(le))
	if err != nil {
		t.Fatalf("Parse(UTF-16LE) error: %v", err)
	}
	if got.Encoding != "utf-16le" || got.Header[1] != "Description" || got.Rows[0][1] != "Café" {
		t.Errorf("UTF-16LE: Encoding=%q Header=%v Row0=%v", got.Encoding, got.Header, got.Rows[0])
	}
	got, err = Parse(bytes.NewReader(be))
	if err != nil {
		t.Fatalf("Parse(UTF-16BE) error: %v", err)
	}
	if got.Encoding != "utf-16be" || got.Header[1] != "Description" || got.Rows[0][1] != "Café" {
		t.Errorf("UTF-16BE: Encoding=%q Header=%v Row0=%v", got.Encoding, got.Header, got.Rows[0])
	}

	// latin-1: 0xE9 is é; the bytes are not valid UTF-8.
	lat := "Date,Description,Amount\n01/05/2026,Caf\xE9,-4.50\n"
	f = mustParse(t, lat)
	if f.Encoding != "latin-1" {
		t.Fatalf("Encoding = %q, want latin-1", f.Encoding)
	}
	if f.Rows[0][1] != "Café" {
		t.Errorf("latin-1 row = %q, want Café", f.Rows[0][1])
	}
}

func TestParseHeaderless(t *testing.T) {
	f := mustParse(t, "2026-01-05,Coffee shop,-4.50\n2026-01-06,Salary,1800.00\n2026-01-07,Rent,-1200.00\n")
	if f.HasHeader || f.Header != nil {
		t.Fatalf("HasHeader=%v Header=%v, want headerless", f.HasHeader, f.Header)
	}
	if len(f.Rows) != 3 {
		t.Fatalf("Rows = %v", f.Rows)
	}
	if f.Columns[0].Kind != KindDate || f.Columns[2].Kind != KindAmount || f.Columns[1].Kind != KindText {
		t.Fatalf("headerless kinds = %v", f.Columns)
	}
	for _, c := range f.Columns {
		if c.Heading != "" {
			t.Errorf("headerless heading = %q", c.Heading)
		}
	}
}

func TestParseHeaderWithDateAmountFirstRow(t *testing.T) {
	// First cell is text but the row mixes a date and an amount:
	// headerless too (data-first layout).
	in := "Coffee,2026-01-05,-4.50\nSalary,2026-01-06,1800.00\nRent,2026-01-07,-1200.00\n"
	f := mustParse(t, in)
	if f.HasHeader {
		t.Fatalf("data-first row mistaken for a header: %v", f.Header)
	}
	if f.Columns[1].Kind != KindDate || f.Columns[2].Kind != KindAmount {
		t.Fatalf("kinds = %v", f.Columns)
	}
}

func TestParseEmptyAndDegenerate(t *testing.T) {
	for _, in := range []string{"", "   \n\t\n", "Date,Amount\n"} {
		_, err := Parse(strings.NewReader(in))
		if !errors.Is(err, ErrEmpty) {
			t.Errorf("Parse(%q) err = %v, want ErrEmpty", in, err)
		}
	}
}

func TestParseRaggedAndTrailingEmpty(t *testing.T) {
	in := "Date,Amount,\n01/05/2026,-4.50\n01/06/2026,9.99,extra\n"
	f := mustParse(t, in)
	if len(f.Header) != 2 || f.Header[1] != "Amount" {
		t.Fatalf("trailing header cell not trimmed: %v", f.Header)
	}
	if len(f.Rows) != 2 || len(f.Rows[0]) != 2 {
		t.Fatalf("Rows = %v", f.Rows)
	}
	if len(f.Columns) != 3 {
		t.Fatalf("ragged width: Columns = %v", f.Columns)
	}
	if f.Columns[2].Kind != KindText {
		t.Errorf("extra column kind = %v, want text", f.Columns[2].Kind)
	}
}

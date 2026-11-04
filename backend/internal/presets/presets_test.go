package presets

import (
	"testing"

	"finance-tracker/internal/csvparse"
)

// scotiaHeaders is the verified Scotiabank CSV header row
// (docs/IMPORT-PRESETS.md).
var scotiaHeaders = []string{
	"Filter", "Date", "Description", "Sub-description",
	"Type of Transaction", "Amount", "Balance",
}

func wantID(p *Preset) string {
	if p == nil {
		return ""
	}
	return p.ID
}

func TestBuiltinShape(t *testing.T) {
	wantIDs := []string{
		GenericCSV, GenericJSON, OFXGeneric,
		TDBank, ScotiabankID, BMO, RBC, Desjardins,
	}
	all := All()
	if len(all) != len(wantIDs) {
		t.Fatalf("All() has %d presets, want %d", len(all), len(wantIDs))
	}
	seen := map[string]bool{}
	for i, p := range all {
		if p.ID != wantIDs[i] {
			t.Fatalf("All()[%d].ID = %q, want %q", i, p.ID, wantIDs[i])
		}
		if seen[p.ID] {
			t.Fatalf("duplicate preset ID %q", p.ID)
		}
		seen[p.ID] = true
		if g := Get(p.ID); g == nil || g.ID != p.ID {
			t.Fatalf("Get(%q) = %v, want the preset", p.ID, g)
		}
		switch p.Format {
		case FormatCSV:
			for field, ref := range p.Map {
				if len(ref.Headers) == 0 && ref.Index < 0 {
					t.Errorf("%s: field %s has no headers and no positional index", p.ID, field)
				}
			}
		case FormatOFX:
			if p.Map != nil {
				t.Errorf("%s: OFX preset has a Map (the mapping is identity)", p.ID)
			}
		case FormatJSON:
			if p.Map == nil {
				t.Errorf("%s: JSON preset has no Map", p.ID)
				continue
			}
			spec, err := p.JSONSpec()
			if err != nil {
				t.Errorf("%s: JSONSpec() error: %v", p.ID, err)
			} else if err := spec.Valid(); err != nil {
				t.Errorf("%s: JSONSpec is invalid: %v", p.ID, err)
			}
		default:
			t.Errorf("%s: format %q does not match import_schemas.kind (0001)", p.ID, p.Format)
		}
	}
	if Get("cibc") != nil {
		t.Errorf("Get(cibc) != nil: CIBC deliberately has no builtin")
	}
	if Get("nope") != nil {
		t.Errorf("Get(nope) != nil, want nil")
	}
}

func TestMatch(t *testing.T) {
	cases := []struct {
		name    string
		file    string
		headers []string
		want    string // "" = nil
	}{
		{"td token claims", "TD_Transactions_2026-01.csv", nil, TDBank},
		{"std never claims td", "std_transactions.csv", nil, GenericCSV},
		{"STD is not td", "STD_Transactions.csv", nil, GenericCSV},
		{"cibc has no preset", "CIBC_2026.csv", nil, GenericCSV},
		{"scotia token", "scotia_export.csv", nil, ScotiabankID},
		{"scotiabank token", "SCOTIABANK_1234.csv", nil, ScotiabankID},
		{"scotia header signature", "statement.csv", scotiaHeaders, ScotiabankID},
		{"sig needs ALL scotia headers", "statement.csv",
			[]string{"Filter", "Date", "Description", "Type of Transaction", "Amount", "Balance"},
			GenericCSV},
		{"sig tolerates extra columns", "statement.csv",
			append(scotiaHeaders, "Extra"), ScotiabankID},
		{"unknown csv falls to generic", "anything.csv", []string{"Weird", "Cols"}, GenericCSV},
		{"plain csv falls to generic", "x.csv", nil, GenericCSV},
		{"bmo token ofx", "bmo-chequing.OFX", nil, BMO},
		{"rbc token qfx", "rbc_history.qfx", nil, RBC},
		{"royal token claims rbc", "royal_bank_2026.ofx", nil, RBC},
		{"desjardins token", "desjardins_998877.qfx", nil, Desjardins},
		{"unknown ofx falls to generic", "weird_bank.ofx", nil, OFXGeneric},
		{"unknown qfx falls to generic", "whatever.qfx", nil, OFXGeneric},
		{"json is always generic", "accounts.json", nil, GenericJSON},
		{"txt not claimed", "notes.txt", nil, ""},
		{"no extension not claimed", "README", nil, ""},
		{"pdf not claimed", "statement.pdf", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := wantID(Match(c.file, c.headers)); got != c.want {
				t.Fatalf("Match(%q, %v) = %q, want %q", c.file, c.headers, got, c.want)
			}
		})
	}
}

func TestCSVColumns(t *testing.T) {
	// Scotia: the verified row with real-world noise (case, padding).
	got := Get(ScotiabankID).CSVColumns([]string{
		" Filter ", "DATE", "Description", "Sub-Description",
		"Type of transaction", " AMOUNT ", "Balance",
	})
	if len(got) != 4 {
		t.Fatalf("scotia resolved %d fields, want 4: %v", len(got), got)
	}
	want := map[csvparse.Field]int{
		csvparse.FieldDatePosted:     1,
		csvparse.FieldDescription:    2,
		csvparse.FieldAmount:         5,
		csvparse.FieldRunningBalance: 6,
	}
	for field, w := range want {
		if got[field] != w {
			t.Errorf("scotia %s = %d, want %d", field, got[field], w)
		}
	}
	if _, ok := got[csvparse.FieldCategory]; ok {
		t.Errorf("scotia maps category (the layout has no category column)")
	}

	// TD: alias matching, case-insensitive + trimmed.
	got = Get(TDBank).CSVColumns([]string{"Date Posted", "MEMO", "Amount (CAD)", "Running Balance"})
	want = map[csvparse.Field]int{
		csvparse.FieldDateExecuted:   -1, // no alias column
		csvparse.FieldDatePosted:     0,
		csvparse.FieldAmount:         2,
		csvparse.FieldDescription:    1,
		csvparse.FieldRunningBalance: 3,
	}
	if len(got) != len(want) {
		t.Fatalf("td resolved %d fields, want %d: %v", len(got), len(want), got)
	}
	for field, w := range want {
		if got[field] != w {
			t.Errorf("td %s = %d, want %d", field, got[field], w)
		}
	}

	// No alias matches: every mapped field is -1 (the builtins have no
	// positional fallback).
	got = Get(TDBank).CSVColumns([]string{"Foo", "Bar"})
	if len(got) != 5 {
		t.Fatalf("td resolved %d fields, want 5: %v", len(got), got)
	}
	for field, idx := range got {
		if idx != -1 {
			t.Errorf("td %s = %d, want -1", field, idx)
		}
	}

	// Positional fallback (user-saved presets, headerless exports like
	// Desjardins business CSV): empty row uses Index; unmatched headers
	// also fall back; a matching header beats the positional index.
	pos := Preset{
		ID:     "synthetic",
		Format: FormatCSV,
		Map: map[csvparse.Field]FieldRef{
			csvparse.FieldDatePosted:     {Headers: []string{"date"}, Index: 0},
			csvparse.FieldDescription:    {Index: 1},
			csvparse.FieldAmount:         {Index: 2},
			csvparse.FieldRunningBalance: {Index: -1},
		},
	}
	got = pos.CSVColumns(nil)
	if got[csvparse.FieldDatePosted] != 0 || got[csvparse.FieldDescription] != 1 ||
		got[csvparse.FieldAmount] != 2 || got[csvparse.FieldRunningBalance] != -1 {
		t.Errorf("headerless row: got %v", got)
	}
	got = pos.CSVColumns([]string{"unrelated", "headers"})
	if got[csvparse.FieldDatePosted] != 0 || got[csvparse.FieldRunningBalance] != -1 {
		t.Errorf("unmatched headers: got %v", got)
	}
	got = pos.CSVColumns([]string{"nope", "Date", "z"})
	if got[csvparse.FieldDatePosted] != 1 {
		t.Errorf("matching header = %d, want 1 (beats Index 0)", got[csvparse.FieldDatePosted])
	}

	// Non-CSV and map-less presets report nil.
	if Get(OFXGeneric).CSVColumns(scotiaHeaders) != nil {
		t.Errorf("ofx-generic CSVColumns != nil")
	}
	if Get(GenericJSON).CSVColumns(scotiaHeaders) != nil {
		t.Errorf("generic-json CSVColumns != nil")
	}
	if Get(GenericCSV).CSVColumns(scotiaHeaders) != nil {
		t.Errorf("generic-csv CSVColumns != nil (Suggest pre-fills instead)")
	}
}

func TestJSONSpec(t *testing.T) {
	spec, err := Get(GenericJSON).JSONSpec()
	if err != nil {
		t.Fatalf("JSONSpec() error: %v", err)
	}
	if len(spec.ArrayPointers) != 6 || spec.ArrayPointers[0] != "" || spec.ArrayPointers[1] != "/transactions" {
		t.Errorf("ArrayPointers = %v", spec.ArrayPointers)
	}
	if len(spec.Fields) != len(csvparse.Fields) {
		t.Fatalf("spec has %d fields, want %d", len(spec.Fields), len(csvparse.Fields))
	}
	for i, f := range spec.Fields {
		if f.Name != string(csvparse.Fields[i]) {
			t.Errorf("spec.Fields[%d].Name = %q, want %q (canonical order)", i, f.Name, csvparse.Fields[i])
		}
		if len(f.Pointers) == 0 {
			t.Errorf("spec.Fields[%d] has no pointers", i)
		}
	}
	if err := spec.Valid(); err != nil {
		t.Errorf("spec is invalid: %v", err)
	}
	if _, err := Get(TDBank).JSONSpec(); err == nil {
		t.Errorf("td JSONSpec() = nil error, want a non-JSON error")
	}
}

func TestTokens(t *testing.T) {
	cases := map[string][]string{
		"TD_Transactions_2026-01.csv": {"td", "transactions", "2026", "01"},
		"bmo-chequing.OFX":            {"bmo", "chequing"},
		"weird__name.csv":             {"weird", "name"},
		"noext":                       {"noext"},
		"":                            {},
	}
	for in, want := range cases {
		got := tokens(in)
		if len(got) != len(want) {
			t.Errorf("tokens(%q) = %v, want %v", in, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("tokens(%q) = %v, want %v", in, got, want)
				break
			}
		}
	}
}

func TestHitExactOnly(t *testing.T) {
	if hit([]string{"std"}, []string{"td"}) {
		t.Errorf("std hit td: tokens must match exactly")
	}
	if !hit([]string{"td", "transactions"}, []string{"td"}) {
		t.Errorf("td did not hit td")
	}
	if hit(nil, []string{"bmo"}) {
		t.Errorf("nil tokens hit bmo")
	}
}

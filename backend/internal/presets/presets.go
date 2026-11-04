// Package presets holds the builtin import presets (plan 4.4): the
// seed mapping for the import_schemas table (0001) -- one entry per
// generic format and per supported Canadian FI. Presets are code,
// not DB rows: step 4.5 serves them to the import page, and "save
// mapping as preset" writes per-user copies into import_schemas.
//
// Builtins (verified formats and their sources: docs/IMPORT-PRESETS.md):
//
//	generic-csv    any CSV (mapping comes from 4.2's Suggest)
//	generic-json   any JSON (alias-pointer fallbacks)
//	ofx-generic    OFX / QFX, any issuer (plan 4.3 parser)
//	td             TD Bank CSV
//	scotiabank     Scotiabank CSV (verified header row)
//	bmo            BMO OFX / QFX
//	rbc            RBC OFX / QFX
//	desjardins     Desjardins OFX
//
// CIBC deliberately has NO builtin: its authoritative record is the
// PDF eStatement (plan 4.4 documents the third-party converter path).
//
// Pure-stdlib: no DB, no routes.
package presets

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode"

	"finance-tracker/internal/csvparse"
	"finance-tracker/internal/jsonparse"
)

// Format is the import kind; the values match import_schemas.kind
// (0001). OFX and QFX share one parser (plan 4.3), so one kind.
type Format string

const (
	FormatCSV  Format = "csv"
	FormatOFX  Format = "ofx"
	FormatJSON Format = "json"
)

// Builtin preset IDs.
const (
	GenericCSV   = "generic-csv"
	GenericJSON  = "generic-json"
	OFXGeneric   = "ofx-generic"
	TDBank       = "td"
	ScotiabankID = "scotiabank"
	BMO          = "bmo"
	RBC          = "rbc"
	Desjardins   = "desjardins"
)

// FieldRef says where one app field lives in a parsed file.
//
//	CSV:  Headers -- candidate header names, matched case-insensitively
//	     after trimming, first match wins. Index -- positional column
//	     (0-based) used when no candidate header matches, headerless
//	     exports included; -1 = no fallback (the field stays unmapped).
//	     Write Index: -1 explicitly: a zero value would mean column 0.
//	JSON: Pointers -- JSON pointers (RFC 6901) within each item, tried
//	     in order.
//
// OFX presets carry no Map: ofxparse (plan 4.3) already emits the
// app fields (date_posted, amount, name, refnum), so the mapping is
// identity.
type FieldRef struct {
	Headers  []string
	Index    int
	Pointers []string
}

// Preset is one builtin import preset. Its JSON form is the spec an
// import job stores (import_schemas.spec, 0001): kind = Format,
// spec = {array_pointers, map: {field: {headers, index, pointers}}}.
type Preset struct {
	ID            string
	Name          string
	Bank          string // "" for the generic presets
	Format        Format
	ArrayPointers []string                    // JSON presets only
	Map           map[csvparse.Field]FieldRef // CSV + JSON; nil for OFX and generic-csv
	FilenameHints []string                    // lower-case filename tokens that claim the file
	headerSig     []string                    // CSV only: ALL must be present to claim by headers
}

// All returns the builtins in canonical order. Treat as read-only.
func All() []Preset { return builtins }

// Get looks up a builtin by ID; nil when unknown.
func Get(id string) *Preset {
	for i := range builtins {
		if builtins[i].ID == id {
			return &builtins[i]
		}
	}
	return nil
}

// Match picks the preset for an uploaded file. The extension drives
// the format: .csv matches a bank preset by filename token, then by
// header signature, else generic-csv; .ofx / .qfx match a bank
// preset by filename token, else ofx-generic; .json is generic-json.
// Any other extension returns nil -- step 4.5 then lets the user
// choose a format and preset.
func Match(filename string, headers []string) *Preset {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".csv":
		if p, ok := matchCSV(filename, headers); ok {
			return p
		}
		return Get(GenericCSV)
	case ".ofx", ".qfx":
		return matchOFX(filename)
	case ".json":
		return Get(GenericJSON)
	}
	return nil
}

// CSVColumns resolves p.Map against a CSV header row: for each mapped
// field, the index of the first candidate header that matches
// (case-insensitive, trimmed), else the positional Index fallback
// (headerless exports pass an empty row), else -1 (unmapped). Returns
// nil for non-CSV presets and for CSV presets without a Map
// (generic-csv: 4.2's Suggest produces the pre-fill instead). Step
// 4.5 uses the result to pre-fill the mapping table.
func (p *Preset) CSVColumns(headers []string) map[csvparse.Field]int {
	if p.Format != FormatCSV || p.Map == nil {
		return nil
	}
	norm := make([]string, len(headers))
	for i, h := range headers {
		norm[i] = normHeader(h)
	}
	out := map[csvparse.Field]int{}
	for _, field := range csvparse.Fields {
		ref, ok := p.Map[field]
		if !ok {
			continue
		}
		idx := -1
		if len(headers) > 0 {
		cand:
			for i, h := range norm {
				for _, want := range ref.Headers {
					if h == normHeader(want) {
						idx = i
						break cand
					}
				}
			}
		}
		if idx < 0 && ref.Index >= 0 {
			idx = ref.Index
		}
		out[field] = idx
	}
	return out
}

// JSONSpec returns the jsonparse.Spec for a JSON preset: the array
// pointer fallbacks plus one FieldSpec per mapped field, in the
// canonical csvparse.Fields order (the row-column order). Errors for
// non-JSON presets.
func (p *Preset) JSONSpec() (jsonparse.Spec, error) {
	if p.Format != FormatJSON {
		return jsonparse.Spec{}, fmt.Errorf("presets: %s is not a JSON preset", p.ID)
	}
	spec := jsonparse.Spec{ArrayPointers: p.ArrayPointers}
	for _, field := range csvparse.Fields {
		ref, ok := p.Map[field]
		if !ok {
			continue
		}
		spec.Fields = append(spec.Fields, jsonparse.FieldSpec{
			Name:     string(field),
			Pointers: ref.Pointers,
		})
	}
	return spec, nil
}

func matchCSV(filename string, headers []string) (*Preset, bool) {
	// 1. filename token (most specific claim a user can make).
	toks := tokens(filename)
	for i := range builtins {
		p := &builtins[i]
		if p.Format != FormatCSV || p.Bank == "" {
			continue
		}
		if hit(toks, p.FilenameHints) {
			return p, true
		}
	}
	// 2. header signature (the verified layout must match exactly).
	if len(headers) > 0 {
		have := make(map[string]bool, len(headers))
		for _, h := range headers {
			have[normHeader(h)] = true
		}
		for i := range builtins {
			p := &builtins[i]
			if p.Format != FormatCSV || p.Bank == "" || len(p.headerSig) == 0 {
				continue
			}
			ok := true
			for _, h := range p.headerSig {
				if !have[normHeader(h)] {
					ok = false
					break
				}
			}
			if ok {
				return p, true
			}
		}
	}
	return nil, false
}

func matchOFX(filename string) *Preset {
	toks := tokens(filename)
	for i := range builtins {
		p := &builtins[i]
		if p.Format != FormatOFX || p.Bank == "" {
			continue
		}
		if hit(toks, p.FilenameHints) {
			return p
		}
	}
	return Get(OFXGeneric)
}

// tokens splits the file name (without extension) into lower-case
// alphanumeric tokens; "TD_Transactions_2026-01.csv" ->
// [td transactions 2026 01]. Exact token match only, so "std" never
// claims the TD preset.
func tokens(filename string) []string {
	base := strings.TrimSuffix(filename, filepath.Ext(filename))
	var toks []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			toks = append(toks, strings.ToLower(cur.String()))
			cur.Reset()
		}
	}
	for _, r := range base {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return toks
}

func hit(tokens, hints []string) bool {
	for _, t := range tokens {
		for _, h := range hints {
			if t == h {
				return true
			}
		}
	}
	return false
}

func normHeader(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

var builtins = []Preset{
	{
		ID:     GenericCSV,
		Name:   "Generic CSV",
		Format: FormatCSV,
		// No Map: 4.2's Suggest engine produces the pre-fill.
	},
	{
		ID:            GenericJSON,
		Name:          "Generic JSON",
		Format:        FormatJSON,
		ArrayPointers: []string{"", "/transactions", "/data", "/items", "/records", "/txns"},
		Map: map[csvparse.Field]FieldRef{
			csvparse.FieldDateExecuted:   {Pointers: []string{"/dateExecuted", "/executedDate", "/executionDate", "/executeDate"}},
			csvparse.FieldDatePosted:     {Pointers: []string{"/datePosted", "/postedDate", "/posted", "/transactionDate", "/txDate", "/date"}},
			csvparse.FieldAmount:         {Pointers: []string{"/amount", "/transactionAmount", "/txAmount", "/value"}},
			csvparse.FieldDescription:    {Pointers: []string{"/description", "/name", "/memo", "/narrative", "/details", "/payee"}},
			csvparse.FieldCategory:       {Pointers: []string{"/category", "/categoryName", "/category_name"}},
			csvparse.FieldRunningBalance: {Pointers: []string{"/runningBalance", "/running_balance", "/balance", "/endBalance"}},
		},
	},
	{
		ID:     OFXGeneric,
		Name:   "OFX / QFX (any issuer)",
		Format: FormatOFX,
	},
	{
		ID:     TDBank,
		Name:   "TD Bank (CSV)",
		Bank:   "TD Bank",
		Format: FormatCSV,
		// The header ROW is not verified (docs/IMPORT-PRESETS.md) --
		// no headerSig: the preset claims by filename token only, and
		// 4.2's Suggest fills anything the aliases miss.
		FilenameHints: []string{"td"},
		Map: map[csvparse.Field]FieldRef{
			csvparse.FieldDateExecuted:   {Headers: []string{"execution date", "execute date", "executed date"}, Index: -1},
			csvparse.FieldDatePosted:     {Headers: []string{"date", "date posted", "posted date", "posting date"}, Index: -1},
			csvparse.FieldAmount:         {Headers: []string{"amount", "transaction amount", "tx amount", "amount (cad)"}, Index: -1},
			csvparse.FieldDescription:    {Headers: []string{"description", "memo", "details", "transaction details"}, Index: -1},
			csvparse.FieldRunningBalance: {Headers: []string{"balance", "running balance", "end balance", "ending balance"}, Index: -1},
		},
	},
	{
		ID:            ScotiabankID,
		Name:          "Scotiabank (CSV)",
		Bank:          "Scotiabank",
		Format:        FormatCSV,
		FilenameHints: []string{"scotia", "scotiabank"},
		// Verified layout (docs/IMPORT-PRESETS.md): Filter, Date,
		// Description, Sub-description, Type of Transaction, Amount,
		// Balance. Filter / Sub-description / Type of Transaction are
		// deliberately unmapped (statement metadata, not a
		// transaction field).
		headerSig: []string{"filter", "date", "description", "sub-description", "type of transaction", "amount", "balance"},
		Map: map[csvparse.Field]FieldRef{
			csvparse.FieldDatePosted:     {Headers: []string{"date"}, Index: -1},
			csvparse.FieldDescription:    {Headers: []string{"description"}, Index: -1},
			csvparse.FieldAmount:         {Headers: []string{"amount"}, Index: -1},
			csvparse.FieldRunningBalance: {Headers: []string{"balance"}, Index: -1},
		},
	},
	{
		ID:            BMO,
		Name:          "BMO (OFX / QFX)",
		Bank:          "BMO",
		Format:        FormatOFX,
		FilenameHints: []string{"bmo"},
	},
	{
		ID:            RBC,
		Name:          "RBC (OFX / QFX)",
		Bank:          "RBC",
		Format:        FormatOFX,
		FilenameHints: []string{"rbc", "royal"},
	},
	{
		ID:            Desjardins,
		Name:          "Desjardins (OFX)",
		Bank:          "Desjardins",
		Format:        FormatOFX,
		FilenameHints: []string{"desjardins"},
	},
}

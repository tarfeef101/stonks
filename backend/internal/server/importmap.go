// Import mapping endpoints (plan step 4.5): the field mapping the user
// confirms for an uploaded statement.
//
//	POST /api/v1/imports/{id}/mapping   store the confirmed mapping
//	                                   (raw body, verbatim) and move
//	                                   the job to status "mapping"
//	POST /api/v1/imports/{id}/preview   apply a candidate mapping
//	                                   statelessly; first mapped rows
//	GET  /api/v1/imports/{id}/analyze   inspect the file: headers /
//	                                   columns / keys + a suggested
//	                                   mapping (auto match or ?preset=)
//	GET  /api/v1/imports/presets        builtin presets + the caller's
//	                                   saved ones
//
// The mapping document (import_jobs.mapping, and import_schemas.spec
// when the user saves the mapping as a preset):
//
//	{"kind":"csv|ofx|json",
//	 "array_pointers":["/transactions"],   // json files only
//	 "map":{"date_posted":{"headers":["date"],"index":1},
//	         "amount":{"index":2}}}
//
// csv refs carry headers (candidate names, first case-insensitive
// trimmed match wins) and index (0-based positional fallback, -1 =
// unmapped — a pointer so "absent" differs from column 0); json refs
// carry pointers (RFC 6901, tried in order); ofx carries no map
// (ofxparse already emits the app fields, so the mapping is the
// identity). The 4.6 runner reads the same document back.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"finance-tracker/internal/csvparse"
	"finance-tracker/internal/jsonparse"
	"finance-tracker/internal/ofxparse"
	"finance-tracker/internal/presets"
	"finance-tracker/internal/repo"

	"github.com/go-chi/chi/v5"
)

// maxMappingBytes caps the mapping document bodies (a mapping is small
// by construction; the cap guards a fat-fingered upload before the
// parse).
const maxMappingBytes = 1 << 20

// Mapping is one confirmed field mapping (see the package comment for
// the shape). Kind is one of "csv", "ofx", "json" and must match the
// uploaded file's extension; the map keys are the canonical
// csvparse.Fields.
type Mapping struct {
	Kind          string                      `json:"kind"`
	ArrayPointers []string                    `json:"array_pointers,omitempty"`
	Map           map[csvparse.Field]fieldRef `json:"map"`
}

// fieldRef locates one app field inside one parsed file. CSV: Headers
// (candidate column names) and Index (0-based fallback column, -1 =
// unmapped). JSON: Pointers (RFC 6901, tried in order).
type fieldRef struct {
	Headers  []string `json:"headers,omitempty"`
	Index    *int     `json:"index,omitempty"`
	Pointers []string `json:"pointers,omitempty"`
}

// validate checks the invariants the 4.6 runner relies on. The kind-vs-
// file match is the handler's job (it sees the extension); this sees
// the document alone.
func (m Mapping) validate() error {
	switch m.Kind {
	case "csv", "ofx", "json":
	default:
		return errors.New("kind must be one of csv, ofx, json")
	}
	if m.Kind != "json" && len(m.ArrayPointers) > 0 {
		return errors.New("array_pointers is only for json mappings")
	}
	for field := range m.Map {
		if !isCanonicalField(field) {
			return fmt.Errorf("unknown field %q", string(field))
		}
	}
	switch m.Kind {
	case "csv":
		if len(m.Map) == 0 {
			return errors.New("csv mapping needs at least one field")
		}
		for field, ref := range m.Map {
			if ref.Index == nil {
				return fmt.Errorf("field %s: index is required for csv", field)
			}
			if *ref.Index < -1 {
				return fmt.Errorf("field %s: index must be >= -1", field)
			}
		}
	case "json":
		if len(m.Map) == 0 {
			return errors.New("json mapping needs at least one field")
		}
		if err := m.jsonSpec().Valid(); err != nil {
			// Valid's messages already carry the jsonparse: prefix;
			// strip it, this is the mapping's own error surface.
			return errors.New(strings.TrimPrefix(err.Error(), "jsonparse: "))
		}
	case "ofx":
		if len(m.Map) != 0 {
			return errors.New("ofx mapping must be empty (the OFX fields are fixed)")
		}
	}
	return nil
}

func isCanonicalField(f csvparse.Field) bool {
	for _, c := range csvparse.Fields {
		if c == f {
			return true
		}
	}
	return false
}

// fieldNames is the canonical field order (the preview column order for
// csv and ofx; json follows its spec's fields).
func fieldNames() []string {
	names := make([]string, len(csvparse.Fields))
	for i, f := range csvparse.Fields {
		names[i] = string(f)
	}
	return names
}

// jsonSpec is the jsonparse.Spec for a json mapping: the array pointer
// fallbacks plus one FieldSpec per mapped field, in the canonical
// order (the row-column order the preview shows).
func (m Mapping) jsonSpec() jsonparse.Spec {
	spec := jsonparse.Spec{ArrayPointers: m.ArrayPointers}
	for _, field := range csvparse.Fields {
		ref, ok := m.Map[field]
		if !ok {
			continue
		}
		spec.Fields = append(spec.Fields, jsonparse.FieldSpec{
			Name:     string(field),
			Pointers: ref.Pointers,
		})
	}
	return spec
}

// kindFromName maps the uploaded file's extension to the mapping kind
// (OFX and QFX share one parser, plan 4.3); any other extension is "".
func kindFromName(filename string) string {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".csv":
		return "csv"
	case ".json":
		return "json"
	case ".ofx", ".qfx":
		return "ofx"
	}
	return ""
}

// resolveCSV resolves a csv mapping document against the file's actual
// header row, in the canonical field order: for each mapped field the
// first candidate header that matches (case-insensitive, trimmed)
// wins, else the positional Index (when >= 0), else the field is
// omitted (unmapped). The returned refs carry only the resolved index —
// that is what the UI pre-fills and the preview applies.
func resolveCSV(m Mapping, headers []string) map[csvparse.Field]fieldRef {
	norm := make([]string, len(headers))
	for i, h := range headers {
		norm[i] = strings.ToLower(strings.TrimSpace(h))
	}
	out := map[csvparse.Field]fieldRef{}
	for _, field := range csvparse.Fields {
		ref, ok := m.Map[field]
		if !ok {
			continue
		}
		idx := -1
		for i, h := range norm {
			for _, want := range ref.Headers {
				if h == strings.ToLower(strings.TrimSpace(want)) {
					idx = i
					break
				}
			}
			if idx >= 0 {
				break
			}
		}
		if idx < 0 && ref.Index != nil && *ref.Index >= 0 {
			idx = *ref.Index
		}
		if idx < 0 {
			continue
		}
		out[field] = fieldRef{Index: &idx}
	}
	return out
}

// suggestCSV builds a suggested csv mapping from 4.2's Suggest: for
// each field whose best candidate scores above zero, the top column. A
// file Suggest cannot read at all gets the empty map (the UI shows
// every field unmapped).
func suggestCSV(f *csvparse.File) Mapping {
	out := map[csvparse.Field]fieldRef{}
	for _, s := range csvparse.Suggest(f) {
		if len(s.Columns) == 0 || s.Columns[0].Score <= 0 {
			continue
		}
		idx := s.Columns[0].Index
		out[s.Field] = fieldRef{Index: &idx}
	}
	return Mapping{Kind: "csv", Map: out}
}

// csvMappedRow applies a resolved csv mapping to one data row: one
// value per canonical field (the preview column order), "" for an
// unmapped field or a short row.
func csvMappedRow(resolved map[csvparse.Field]fieldRef, row []string) []string {
	out := make([]string, len(csvparse.Fields))
	for i, field := range csvparse.Fields {
		ref, ok := resolved[field]
		if !ok || ref.Index == nil || *ref.Index < 0 || *ref.Index >= len(row) {
			out[i] = ""
			continue
		}
		out[i] = row[*ref.Index]
	}
	return out
}

// mappedFile parses the file and applies the mapping to ALL of its
// data rows: the preview column names, one mapped row per record in
// source order, and the total mapped row count. For csv it also
// returns the parsed file (the 4.6 runner needs the mapped column's
// inferred DateFormat); json and ofx return nil there. The last return
// is the kind-specific parse error message ("" when the file parsed) —
// the same wording the preview has always answered with. Preview and
// the runner share this one path, so a mapping renders the same rows
// in both.
func mappedFile(m Mapping, file []byte) (names []string, rows [][]string, total int, csvFile *csvparse.File, msg string) {
	names = fieldNames()
	switch m.Kind {
	case "csv":
		f, err := csvparse.Parse(bytes.NewReader(file))
		if err != nil {
			msg := "file could not be parsed as CSV"
			if errors.Is(err, csvparse.ErrTooManyRows) {
				msg = "file has too many rows to import"
			}
			return nil, nil, 0, nil, msg
		}
		resolved := resolveCSV(m, f.Header)
		total = len(f.Rows)
		rows = make([][]string, 0, total)
		for _, row := range f.Rows {
			rows = append(rows, csvMappedRow(resolved, row))
		}
		return names, rows, total, f, ""
	case "json":
		spec := m.jsonSpec()
		names = make([]string, 0, len(spec.Fields))
		for _, fs := range spec.Fields {
			names = append(names, fs.Name)
		}
		f, err := jsonparse.Parse(bytes.NewReader(file), spec)
		if err != nil {
			msg := "file could not be parsed as JSON"
			if errors.Is(err, jsonparse.ErrEmpty) {
				msg = "no transactions found at the mapped location"
			} else if errors.Is(err, jsonparse.ErrTooLarge) {
				msg = "file is too large to import"
			}
			return nil, nil, 0, nil, msg
		}
		return names, f.Rows, len(f.Rows), nil, ""
	case "ofx":
		txns, err := ofxparse.Parse(bytes.NewReader(file))
		if err != nil {
			msg := "file could not be parsed as OFX"
			if errors.Is(err, ofxparse.ErrEmpty) {
				msg = "no transactions found in file"
			} else if errors.Is(err, ofxparse.ErrTooLarge) {
				msg = "file is too large to import"
			}
			return nil, nil, 0, nil, msg
		}
		// The OFX layout is fixed: the parser already emits the app
		// fields, so the row is date, amount, name, then the unmapped
		// canonical columns empty.
		rows = make([][]string, 0, len(txns))
		for _, t := range txns {
			rows = append(rows, []string{t.DatePosted, t.Amount, t.Name, "", "", ""})
		}
		return names, rows, len(txns), nil, ""
	}
	return nil, nil, 0, nil, ""
}

// presetDoc is the builtin preset's mapping document, or nil when the
// preset carries no map (ofx + generic-csv: the pre-fill comes from
// Suggest, the ofx mapping is the identity).
func presetDoc(p *presets.Preset) *Mapping {
	if p.Map == nil {
		return nil
	}
	m := presetMapping(p)
	return &m
}

// presetMapping is a builtin preset's full mapping document (the same
// shape 4.4 seeded into import_schemas.spec). CSV refs carry headers +
// Index (always set; -1 = no fallback, the zero value would mean
// column 0); JSON refs carry pointers. OFX and generic-csv come back
// with an empty map.
func presetMapping(p *presets.Preset) Mapping {
	m := Mapping{Kind: string(p.Format), Map: map[csvparse.Field]fieldRef{}}
	if p.Format == presets.FormatJSON {
		m.ArrayPointers = p.ArrayPointers
	}
	for _, field := range csvparse.Fields {
		ref, ok := p.Map[field]
		if !ok {
			continue
		}
		switch p.Format {
		case presets.FormatCSV:
			idx := ref.Index
			m.Map[field] = fieldRef{Headers: ref.Headers, Index: &idx}
		case presets.FormatJSON:
			m.Map[field] = fieldRef{Pointers: ref.Pointers}
		}
	}
	return m
}

// firstItemKeys locates the transaction array with the given pointer
// fallbacks (the generic-json set) and returns the sorted keys of the
// array's first item when that item is an object; nil when no pointer
// reaches one. The keys feed the UI's pointer picker (the user picks
// item keys, not raw JSON).
func firstItemKeys(root any, pointers []string) []string {
	for _, pointer := range pointers {
		v, ok := resolvePointer(root, pointer)
		if !ok {
			continue
		}
		arr, ok := v.([]any)
		if !ok || len(arr) == 0 {
			continue
		}
		item, ok := arr[0].(map[string]any)
		if !ok {
			continue
		}
		keys := make([]string, 0, len(item))
		for k := range item {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return keys
	}
	return nil
}

// resolvePointer walks an RFC 6901 pointer against a decoded JSON
// value; ok is false when any token is missing or not traversable.
func resolvePointer(v any, pointer string) (any, bool) {
	if pointer == "" {
		return v, true
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, false
	}
	cur := v
	for _, token := range strings.Split(pointer[1:], "/") {
		// ~1 -> / first, then ~0 -> ~ (the RFC 6901 unescape order).
		token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
		switch next := cur.(type) {
		case map[string]any:
			val, ok := next[token]
			if !ok {
				return nil, false
			}
			cur = val
		case []any:
			i, err := strconv.Atoi(token)
			if err != nil || i < 0 || i >= len(next) {
				return nil, false
			}
			cur = next[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

// presetRef names the preset a response is referring to.
type presetRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// chosenPreset is the preset an analyze request resolved to: a builtin
// (by ID/name) or one of the caller's saved presets; doc is the
// mapping document to suggest (nil for presets without a map —
// generic-csv and ofx).
type chosenPreset struct {
	id   string
	name string
	doc  *Mapping
}

// Preset lookup sentinels (400s); other store errors are 500s.
var (
	errPresetUnknown = errors.New("unknown preset")
	errPresetKind    = errors.New("preset kind must match the file type")
	errPresetUsable  = errors.New("preset is not a usable mapping")
)

// lookupPreset resolves a ?preset= value: a builtin preset ID first,
// else one of the caller's saved presets (custom import_schemas rows).
// The kind must match the file, and a custom spec must unmarshal and
// validate as a mapping document.
func (h *handler) lookupPreset(ctx context.Context, c caller, presetID, kind string) (*chosenPreset, error) {
	if p := presets.Get(presetID); p != nil {
		if string(p.Format) != kind {
			return nil, errPresetKind
		}
		return &chosenPreset{id: p.ID, name: p.Name, doc: presetDoc(p)}, nil
	}
	if h.schemas == nil {
		return nil, errPresetUnknown
	}
	sc, err := h.schemas.Get(ctx, c.row.ID, presetID, c.dek)
	if errors.Is(err, repo.ErrSchemaNotFound) {
		return nil, errPresetUnknown
	}
	if err != nil {
		return nil, err
	}
	if sc.Kind != kind {
		return nil, errPresetKind
	}
	var saved Mapping
	if err := json.Unmarshal(sc.Spec, &saved); err != nil {
		return nil, errPresetUsable
	}
	if err := saved.validate(); err != nil {
		return nil, errPresetUsable
	}
	return &chosenPreset{id: sc.ID, name: sc.Name, doc: &saved}, nil
}

// analyzeOut is the wire response of GET /imports/{id}/analyze: the
// format facts (headers + inferred columns for csv, the first item's
// keys for json) and the suggested mapping the UI pre-fills.
type analyzeOut struct {
	Kind      string          `json:"kind"`
	Preset    *presetRef      `json:"preset"`
	Headers   []string        `json:"headers,omitempty"`
	Columns   []analyzeColumn `json:"columns,omitempty"`
	Suggested Mapping         `json:"suggested"`
	Keys      []string        `json:"keys,omitempty"`
}

// analyzeColumn is one csvparse.Column in the analyze response (the
// inferred kind is the UI's type badge).
type analyzeColumn struct {
	Index   int    `json:"index"`
	Heading string `json:"heading"`
	Kind    string `json:"kind"`
}

// previewOut is the wire response of POST /imports/{id}/preview:
// Columns and Rows are aligned (each row has one cell per column), and
// Total is the mapped row count of the whole file, not of the page.
type previewOut struct {
	Columns []string   `json:"columns"`
	Rows    [][]string `json:"rows"`
	Total   int        `json:"total"`
}

// presetOut is the wire row of the presets list. Spec is the mapping
// document the UI would pre-fill (jsonb, as stored).
type presetOut struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Bank    string          `json:"bank"`
	Builtin bool            `json:"builtin"`
	Kind    string          `json:"kind"`
	Spec    json.RawMessage `json:"spec"`
}

// importPresets serves GET /api/v1/imports/presets: the code builtins
// (specs rebuilt from the preset definitions) followed by the
// caller's saved presets (import_schemas, customs only — the legacy DB
// builtin rows are not the code builtins and stay out of the list).
func (h *handler) importPresets(w http.ResponseWriter, r *http.Request) {
	if h.imports == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	all := presets.All()
	out := make([]presetOut, 0, len(all)+8)
	for i := range all {
		p := &all[i]
		spec, err := json.Marshal(presetMapping(p))
		if err != nil {
			// presetMapping's output is a fixed-shape struct; a
			// marshal failure would be a programming error.
			slog.Error("marshal builtin preset failed", "preset", p.ID, "error", err)
			writeAPIError(w, http.StatusInternalServerError, codeInternal, "preset is not usable")
			return
		}
		out = append(out, presetOut{
			ID:      p.ID,
			Name:    p.Name,
			Bank:    p.Bank,
			Builtin: true,
			Kind:    string(p.Format),
			Spec:    spec,
		})
	}
	if h.schemas != nil {
		schemas, err := h.schemas.List(r.Context(), c.row.ID, c.dek)
		if err != nil {
			slog.Error("list import schemas failed", "error", err)
			writeAPIError(w, http.StatusInternalServerError, codeInternal, "schema operation failed")
			return
		}
		for _, sc := range schemas {
			if sc.Builtin {
				continue
			}
			out = append(out, presetOut{ID: sc.ID, Name: sc.Name, Kind: sc.Kind, Spec: sc.Spec})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// importAnalyze serves GET /api/v1/imports/{id}/analyze: it inspects
// the uploaded file and answers with the format facts plus a suggested
// mapping. The suggestion comes from the ?preset= preset when given (a
// builtin ID or one of the caller's saved presets), else from the
// filename/header match (presets.Match). The suggestion is a starting
// point; the user confirms or edits it before it is stored.
func (h *handler) importAnalyze(w http.ResponseWriter, r *http.Request) {
	if h.imports == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if !uuidShapeOK(id) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "import job not found")
		return
	}
	job, err := h.imports.Get(r.Context(), c.row.ID, id, c.dek)
	if err != nil {
		h.importError(w, "analyze", err)
		return
	}
	kind := kindFromName(job.FileName)
	if kind == "" {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "unsupported file type")
		return
	}
	file, err := h.imports.ReadFile(r.Context(), c.row.ID, id, c.dek)
	if err != nil {
		h.importError(w, "analyze", err)
		return
	}

	// The starting preset: ?preset= (a builtin ID or one of the
	// caller's saved presets) wins; the csv/ofx filename match is
	// decided below (csv needs the header row first).
	var chosen *chosenPreset
	if presetID := r.URL.Query().Get("preset"); presetID != "" {
		ch, err := h.lookupPreset(r.Context(), c, presetID, kind)
		switch {
		case errors.Is(err, errPresetUnknown), errors.Is(err, errPresetKind):
			writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
			return
		case errors.Is(err, errPresetUsable):
			writeAPIError(w, http.StatusBadRequest, codeInvalidRequest,
				fmt.Sprintf("preset %q is not a usable mapping", presetID))
			return
		case err != nil:
			slog.Error("get preset failed", "error", err)
			writeAPIError(w, http.StatusInternalServerError, codeInternal, "preset is not usable")
			return
		}
		chosen = ch
	}

	out := analyzeOut{Kind: kind}
	if chosen != nil {
		out.Preset = &presetRef{ID: chosen.id, Name: chosen.name}
	}

	switch kind {
	case "csv":
		f, err := csvparse.Parse(bytes.NewReader(file))
		if err != nil {
			msg := "file could not be parsed as CSV"
			if errors.Is(err, csvparse.ErrTooManyRows) {
				msg = "file has too many rows to import"
			}
			writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, msg)
			return
		}
		if chosen == nil {
			// Match never returns nil for .csv (falls back to
			// generic-csv).
			p := presets.Match(job.FileName, f.Header)
			chosen = &chosenPreset{id: p.ID, name: p.Name, doc: presetDoc(p)}
			out.Preset = &presetRef{ID: p.ID, Name: p.Name}
		}
		headers := f.Header
		if headers == nil {
			headers = []string{}
		}
		out.Headers = headers
		for _, col := range f.Columns {
			out.Columns = append(out.Columns, analyzeColumn{
				Index:   col.Index,
				Heading: col.Heading,
				Kind:    col.Kind.String(),
			})
		}
		if chosen.doc != nil {
			out.Suggested = Mapping{Kind: kind, Map: resolveCSV(*chosen.doc, headers)}
		} else {
			out.Suggested = suggestCSV(f)
		}
	case "json":
		var root any
		if err := json.Unmarshal(file, &root); err != nil {
			writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "file could not be parsed as JSON")
			return
		}
		generic := presets.Get(presets.GenericJSON)
		out.Keys = firstItemKeys(root, generic.ArrayPointers)
		if chosen != nil && chosen.doc != nil {
			out.Suggested = *chosen.doc
		} else {
			out.Suggested = presetMapping(generic)
		}
	case "ofx":
		if _, err := ofxparse.Parse(bytes.NewReader(file)); err != nil {
			msg := "file could not be parsed as OFX"
			if errors.Is(err, ofxparse.ErrEmpty) {
				msg = "no transactions found in file"
			} else if errors.Is(err, ofxparse.ErrTooLarge) {
				msg = "file is too large to import"
			}
			writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, msg)
			return
		}
		if chosen == nil {
			p := presets.Match(job.FileName, nil)
			chosen = &chosenPreset{id: p.ID, name: p.Name, doc: presetDoc(p)}
			out.Preset = &presetRef{ID: p.ID, Name: p.Name}
		}
		out.Suggested = Mapping{Kind: kind, Map: map[csvparse.Field]fieldRef{}}
	}
	writeJSON(w, http.StatusOK, out)
}

// importPreview serves POST /api/v1/imports/{id}/preview: a stateless
// "what would the mapped rows look like" — the body carries the
// candidate mapping (not yet stored) and a row limit (default 10, cap
// 50). The response is the first mapped rows plus the total mapped row
// count (the UI shows "10 of 1,234").
func (h *handler) importPreview(w http.ResponseWriter, r *http.Request) {
	if h.imports == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if !uuidShapeOK(id) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "import job not found")
		return
	}
	var in struct {
		Mapping Mapping `json:"mapping"`
		Limit   int     `json:"limit"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "invalid mapping document")
		return
	}
	if err := in.Mapping.validate(); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	job, err := h.imports.Get(r.Context(), c.row.ID, id, c.dek)
	if err != nil {
		h.importError(w, "preview", err)
		return
	}
	if kindFromName(job.FileName) != in.Mapping.Kind {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "mapping kind must match the file type")
		return
	}
	file, err := h.imports.ReadFile(r.Context(), c.row.ID, id, c.dek)
	if err != nil {
		h.importError(w, "preview", err)
		return
	}

	limit := in.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}

	names, rows, total, _, msg := mappedFile(in.Mapping, file)
	if msg != "" {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, msg)
		return
	}
	if total > limit {
		rows = rows[:limit]
	}
	writeJSON(w, http.StatusOK, previewOut{Columns: names, Rows: rows, Total: total})
}

// importSetMapping serves POST /api/v1/imports/{id}/mapping: stores
// the user-confirmed mapping document (the raw body, verbatim — the
// jsonb column normalizes it) and moves the job to status "mapping".
// The body is capped at maxMappingBytes and its kind must match the
// file's extension.
func (h *handler) importSetMapping(w http.ResponseWriter, r *http.Request) {
	if h.imports == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if !uuidShapeOK(id) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "import job not found")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxMappingBytes+1))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "invalid mapping document")
		return
	}
	if len(body) > maxMappingBytes {
		writeAPIError(w, http.StatusRequestEntityTooLarge, codePayloadTooLarge, "mapping is too large")
		return
	}
	var m Mapping
	if err := json.Unmarshal(body, &m); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "invalid mapping document")
		return
	}
	if err := m.validate(); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	job, err := h.imports.Get(r.Context(), c.row.ID, id, c.dek)
	if err != nil {
		h.importError(w, "set mapping", err)
		return
	}
	if kindFromName(job.FileName) != m.Kind {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "mapping kind must match the file type")
		return
	}
	job, err = h.imports.SetMapping(r.Context(), c.row.ID, id, json.RawMessage(body), c.dek)
	if err != nil {
		h.importError(w, "set mapping", err)
		return
	}
	writeJSON(w, http.StatusOK, toImportOut(job))
}

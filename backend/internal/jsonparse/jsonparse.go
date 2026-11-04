// Package jsonparse extracts transaction rows from JSON statement
// exports using a spec of JSON-pointer field paths (plan 4.4). It is
// a pure-stdlib library: no DB, no routes. Step 4.6's importer
// consumes Parse exactly the way it consumes csvparse and ofxparse:
// values come back RAW strings and 4.6 validates and normalizes
// dates and amounts.
package jsonparse

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ErrEmpty is returned by Parse when the document holds no
// transaction items (no array found at the spec's pointers, an empty
// array, or only non-object items).
var ErrEmpty = errors.New("jsonparse: no transaction items")

// FieldSpec names one app field and the JSON pointers (RFC 6901) to
// look for it at, relative to each item. Pointers are tried in order;
// the first that resolves to a scalar (string / number / bool) wins.
// A missing pointer, JSON null, or an object/array value never wins
// and the next pointer is tried (a null exporter wrote means "empty",
// not "stop looking").
type FieldSpec struct {
	Name     string
	Pointers []string
}

// Spec locates the transaction array and names the fields to extract.
// ArrayPointers is tried in order; an empty list means "the document
// root IS the array". Each pointer must be "" or start with "/".
type Spec struct {
	ArrayPointers []string
	Fields        []FieldSpec
}

// File is the extraction result: one row per item, each row holding
// the RAW scalar string for every field (missing -> ""). Row order is
// item order in the source array.
type File struct {
	Rows [][]string
}

// Valid reports whether spec is well-formed: at least one field,
// unique non-empty field names, and pointers that are "" or start
// with "/". User-saved presets (import_schemas.spec, 0001) run this
// before Parse.
func (s Spec) Valid() error {
	seen := make(map[string]bool, len(s.Fields))
	if len(s.Fields) == 0 {
		return errors.New("jsonparse: spec has no fields")
	}
	for i, f := range s.Fields {
		if f.Name == "" {
			return fmt.Errorf("jsonparse: field %d has an empty name", i)
		}
		if seen[f.Name] {
			return fmt.Errorf("jsonparse: duplicate field name %q", f.Name)
		}
		seen[f.Name] = true
		if len(f.Pointers) == 0 {
			return fmt.Errorf("jsonparse: field %q has no pointers", f.Name)
		}
		for _, p := range f.Pointers {
			if p != "" && !strings.HasPrefix(p, "/") {
				return fmt.Errorf("jsonparse: field %q pointer %q must start with \"/\"", f.Name, p)
			}
		}
	}
	for _, p := range s.ArrayPointers {
		if p != "" && !strings.HasPrefix(p, "/") {
			return fmt.Errorf("jsonparse: array pointer %q must start with \"/\"", p)
		}
	}
	return nil
}

// Parse decodes the JSON document, locates the item array per spec,
// and extracts one raw-string row per item. Malformed JSON is a hard
// error; a document without a usable item array is ErrEmpty.
func Parse(r io.Reader, spec Spec) (*File, error) {
	if err := spec.Valid(); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber() // keep numbers in their file spelling (no float64)
	var root any
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("jsonparse: %w", err)
	}
	items, ok := findItems(root, spec.ArrayPointers)
	if !ok || len(items) == 0 {
		return nil, ErrEmpty
	}
	f := &File{}
	for _, it := range items {
		obj, ok := it.(map[string]any)
		if !ok {
			continue // a spec points INTO objects; skip the rest
		}
		row := make([]string, len(spec.Fields))
		for i, fs := range spec.Fields {
			for _, p := range fs.Pointers {
				v, found := resolve(obj, p)
				if !found {
					continue
				}
				if s, ok := rawString(v); ok {
					row[i] = s
				}
				break // resolved: scalar kept, non-scalar stays ""
			}
		}
		f.Rows = append(f.Rows, row)
	}
	if len(f.Rows) == 0 {
		return nil, ErrEmpty
	}
	return f, nil
}

// findItems walks the spec's array pointers (root when empty) and
// returns the first array it finds, object or not -- empty arrays and
// all-non-object items both end up as ErrEmpty in Parse.
func findItems(root any, pointers []string) ([]any, bool) {
	if len(pointers) == 0 {
		arr, ok := root.([]any)
		return arr, ok
	}
	for _, p := range pointers {
		if v, ok := resolve(root, p); ok {
			if arr, ok := v.([]any); ok {
				return arr, true
			}
		}
	}
	return nil, false
}

// resolve walks a decoded JSON document along one RFC 6901 pointer.
// The empty pointer addresses the document root. Reference tokens
// unescape "~1" -> "/" and "~0" -> "~" (in that order, so a literal
// "~1" key -- encoded "~01" -- comes back intact).
func resolve(v any, pointer string) (any, bool) {
	if pointer == "" {
		return v, true
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, false
	}
	for _, tok := range strings.Split(pointer[1:], "/") {
		tok = unescapeRef(tok)
		switch cur := v.(type) {
		case map[string]any:
			next, ok := cur[tok]
			if !ok {
				return nil, false
			}
			v = next
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(cur) {
				return nil, false
			}
			v = cur[i]
		default:
			return nil, false // scalars have no children
		}
	}
	return v, true
}

func unescapeRef(s string) string {
	if !strings.Contains(s, "~") {
		return s
	}
	s = strings.ReplaceAll(s, "~1", "/")
	return strings.ReplaceAll(s, "~0", "~")
}

// rawString renders one JSON scalar as its raw file spelling.
// Numbers keep the literal they had in the file (json.Number, via
// UseNumber) -- 45.23 stays "45.23" and a long account number is
// never round-tripped through float64. Null, objects and arrays are
// not scalars: found-but-not-scalar means the field stays "".
func rawString(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case json.Number:
		return x.String(), true
	case bool:
		return strconv.FormatBool(x), true
	default:
		return "", false
	}
}

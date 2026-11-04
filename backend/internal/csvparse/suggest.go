package csvparse

import (
	"sort"
	"strings"
)

// Suggest ranks the file's columns for each importable field, from
// the column's inferred kind, its heading text and its position.
// Results follow the Fields order; within a field, candidates are
// sorted by score (desc) then column index (asc), and zero-score
// columns are omitted.
func Suggest(f *File) []Suggestion {
	firstDate, firstText := -1, -1
	for _, c := range f.Columns {
		if c.Kind == KindDate && firstDate == -1 {
			firstDate = c.Index
		}
		if c.Kind == KindText && firstText == -1 {
			firstText = c.Index
		}
	}
	out := make([]Suggestion, 0, len(Fields))
	for _, field := range Fields {
		var cols []ScoredColumn
		for _, c := range f.Columns {
			if s := scoreField(field, c, firstDate, firstText); s > 0 {
				cols = append(cols, ScoredColumn{Index: c.Index, Heading: c.Heading, Score: s})
			}
		}
		sort.Slice(cols, func(i, j int) bool {
			if cols[i].Score != cols[j].Score {
				return cols[i].Score > cols[j].Score
			}
			return cols[i].Index < cols[j].Index
		})
		out = append(out, Suggestion{Field: field, Columns: cols})
	}
	return out
}

// scoreField scores one column for one field. Headings are matched
// case-insensitively as substrings (bank headers vary freely).
func scoreField(field Field, c Column, firstDate, firstText int) float64 {
	h := strings.ToLower(c.Heading)
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(h, s) {
				return true
			}
		}
		return false
	}
	switch field {
	case FieldDatePosted:
		if c.Kind != KindDate {
			return 0
		}
		s := 3.0
		if has("post") {
			s += 2.5
		} else if has("date") {
			s += 1.0
		}
		if c.Index == firstDate {
			s += 0.5
		}
		return s
	case FieldDateExecuted:
		if c.Kind != KindDate {
			return 0
		}
		s := 2.0
		switch {
		case has("execut"):
			s += 2.5
		case has("transaction"):
			s += 1.5
		case has("value"):
			s += 1.0
		}
		return s
	case FieldAmount:
		if c.Kind != KindAmount {
			return 0
		}
		s := 3.0
		if c.RunningBalance {
			s = 1.0
		}
		if has("amount") {
			s += 2.0
		} else if has("debit", "credit") {
			s += 1.0
		}
		return s
	case FieldRunningBalance:
		if c.Kind != KindAmount {
			return 0
		}
		s := 0.5
		if c.RunningBalance {
			s = 4.0
		}
		if has("balance", "running", "solde") {
			s += 2.0
		}
		return s
	case FieldDescription:
		if c.Kind != KindText {
			return 0
		}
		s := 3.0
		if has("description", "desc", "payee", "memo", "details", "narrative", "particulars") {
			s += 2.0
		}
		if c.Index == firstText {
			s += 0.5
		}
		return s
	case FieldCategory:
		if c.Kind != KindText {
			return 0
		}
		s := 1.0
		switch {
		case has("categor"):
			s += 3.0
		case has("merchant", "code", "mcc"):
			s += 2.0
		}
		return s
	}
	return 0
}

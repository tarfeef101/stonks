// Package csvparse turns uploaded bank statement CSV files into a
// normalized table plus per-column type inference and ranked field
// suggestions (plan 4.2). It is a pure-stdlib library: no DB, no
// routes. Step 4.5's mapping endpoint consumes Parse + Suggest.
package csvparse

import "errors"

// Field is one of the transaction fields the app can map an imported
// column onto. The vocabulary mirrors plan 4.2 and the 0002
// transactions columns; running_balance has no transactions column --
// it is suggested so the mapping UI can show (and the import step can
// sanity-check) the statement running balance.
type Field string

// The importable fields, in the order Suggest reports them.
const (
	FieldDateExecuted   Field = "date_executed"
	FieldDatePosted     Field = "date_posted"
	FieldAmount         Field = "amount"
	FieldDescription    Field = "description"
	FieldCategory       Field = "category"
	FieldRunningBalance Field = "running_balance"
)

// Fields lists Field in its canonical order.
var Fields = []Field{
	FieldDateExecuted,
	FieldDatePosted,
	FieldAmount,
	FieldDescription,
	FieldCategory,
	FieldRunningBalance,
}

// ColumnKind is the inferred type of one column.
type ColumnKind int

const (
	KindUnknown ColumnKind = iota // no non-empty sample cells
	KindText
	KindDate
	KindAmount
)

// String renders the kind for logs and the mapping UI.
func (k ColumnKind) String() string {
	switch k {
	case KindDate:
		return "date"
	case KindAmount:
		return "amount"
	case KindText:
		return "text"
	default:
		return "unknown"
	}
}

// Column is the inference result for one column.
type Column struct {
	Index   int
	Heading string // trimmed header cell, or "" for headerless files
	Kind    ColumnKind
	// DateFormat is "mm-dd-yyyy" | "dd-mm-yyyy" | "iso8601"; only
	// meaningful when Kind == KindDate.
	DateFormat string
	// RunningBalance marks amount columns whose values track a
	// cumulative statement balance (each row ~= previous row +/- the
	// row's amount).
	RunningBalance bool
}

// File is a parsed statement: the detected transport properties
// (delimiter, encoding), the header row when one was detected, the
// data rows, and per-column inference over the sampled rows.
type File struct {
	Delimiter rune   // ',' ';' or '\t', as sniffed
	Encoding  string // "utf-8" | "utf-16le" | "utf-16be" | "latin-1"
	HasHeader bool   // false for headerless exports (first row is data)
	Header    []string
	Rows      [][]string // data rows only; the header row, when present, is excluded
	Columns   []Column
}

// ScoredColumn is one ranked candidate in a field suggestion.
type ScoredColumn struct {
	Index   int
	Heading string
	Score   float64
}

// Suggestion is the ranked candidate list for one field (best first).
type Suggestion struct {
	Field   Field
	Columns []ScoredColumn
}

// ErrEmpty is returned by Parse when the input has no data rows.
var ErrEmpty = errors.New("csvparse: no data rows")

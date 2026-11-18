// Package reporttypes holds the three row types at the report
// boundary — Row, Budget and Name — shared by the report core
// (internal/report) and the schedule package's report adapters
// (internal/schedule/adapters.go). The adapters used to field-for-field
// copy these shapes out of the repo types, a parallel data plane that
// could drift from the collector's contract; the declarations now live
// here exactly once, and both sides reference them.
//
// The package is stdlib-only (the same bar as internal/report): it
// defines data shapes and nothing else, so it stays importable without
// a database. internal/report re-exports the types as aliases
// (report.Row, report.Budget, report.Name) so all existing code —
// collector.go, report.go, report_test.go, the mailer path — compiles
// unchanged; the adapters reference reporttypes directly.
package reporttypes

import "time"

// Row is one transaction row. TopMerchants reads only Description (the
// decrypted memo) and Amount (the canonical 2-decimal signed amount,
// negative = spend); the collector's categorySpend also reads
// CategoryID (the row's category, "" = uncategorized — a NULL or
// deleted category), Date (the row's date_posted at UTC date precision;
// zero = no FX lookup) and Currency (the account's ISO-4217 code; empty
// = no FX conversion).
type Row struct {
	Description string
	Amount      string
	CategoryID  string
	Date        time.Time
	Currency    string
}

// Budget is one budget row as the collector consumes it; the adapter
// decrypts Targets (a JSON object keyed by category id -> canonical
// 2-decimal string, exactly repo.Budget.Targets after decryption).
//
// Order contract: BudgetSource.List returns rows in a stable,
// deterministic order (repo.BudgetStore.List orders by created_at, id),
// and Collect takes the FIRST active row whose Period matches. (The
// handoff's "lexicographically first ID" is not expressible here: the
// pinned struct has no ID field — see REPORT.md, PINCORR.)
type Budget struct {
	Period  string
	Active  bool
	Targets map[string]string
}

// Name is one category row as the collector consumes it.
type Name struct {
	ID   string
	Name string
}

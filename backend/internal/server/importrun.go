// importRun is the final step (4.6) of the import pipeline: it applies
// the job's confirmed mapping to the uploaded statement, normalizes
// dates and amounts, and writes each row through the transaction
// store's dedup-aware Upsert, so re-importing a file inserts nothing
// new (colliding rows come back "skipped"). A job must be in the
// "mapping" state; the run walks mapping -> importing -> done | failed.
// Row-level decode errors never fail the run: they are collected in
// result.Errors and the remaining rows are imported. Structural
// failures (unreadable file, invalid mapping, store errors) fail the
// run.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"finance-tracker/internal/csvparse"
	"finance-tracker/internal/repo"

	"github.com/go-chi/chi/v5"
)

// importRunResult is the raw jsonb a successful (or partially
// successful) run stores in import_jobs.result: the rows inserted, the
// rows skipped as dedup collisions, and the per-row decode errors.
// Errors is a slice, never nil, so a clean run renders [] not null.
type importRunResult struct {
	Inserted int        `json:"inserted"`
	Skipped  int        `json:"skipped"`
	Errors   []rowError `json:"errors"`
}

// rowError is one failed row: its 0-based index within the parsed data
// rows, the canonical field that failed to decode, and a message the
// frontend can show beside the row.
type rowError struct {
	Row     int    `json:"row"`
	Field   string `json:"field"`
	Message string `json:"message"`
}

// Date layouts the runner tries, per file kind. CSV uses the mapped
// column's inferred format (translated by csvDateFormatToLayout) and
// falls back to the json list; json and ofx use the lists directly.
var (
	jsonDateLayouts = []string{
		"2006-01-02T15:04:05Z07:00", // RFC3339
		"2006-01-02",
		"01-02-2006", // mm-dd-yyyy
		"02-01-2006", // dd-mm-yyyy
	}
	ofxDateLayouts = []string{
		"20060102150405[-0700]", // pinned OFX layout (zone fix applied first)
		"20060102150405",
		"20060102", // date-only DTPOSTED (no time, no zone)
	}
)

// csvDateFormatToLayout translates a csvparse inferred format name into
// a time layout; unrecognized names yield "" (the caller falls back to
// jsonDateLayouts).
func csvDateFormatToLayout(format string) string {
	switch format {
	case "mm-dd-yyyy":
		return "01-02-2006"
	case "dd-mm-yyyy":
		return "02-01-2006"
	case "iso8601":
		return "2006-01-02"
	}
	return ""
}

// normalizeOFXZone rewrites an OFX timestamp's timezone to the full
// [-0700]-style offset the pinned layout expects. OFX allows a
// one-digit shorthand zone ("<DTPOSTED:20260105123000[-5]"), which
// time.Parse rejects: the layout's [-0700] verb wants four digits.
// Full offsets ([-0500], [+0530]) and zone-less values pass through.
func normalizeOFXZone(raw string) string {
	open := strings.LastIndex(raw, "[")
	if open < 0 {
		return raw
	}
	closeIdx := strings.Index(raw[open:], "]")
	if closeIdx < 0 {
		return raw
	}
	closeIdx += open
	zone := raw[open+1 : closeIdx]
	if len(zone) != 2 { // sign + one digit; full offsets are sign + four digits
		return raw
	}
	return raw[:open+1] + zone[:1] + "0" + zone[1:] + "00" + raw[closeIdx:]
}

// parseImportDate tries each layout in order and returns the first
// match.
func parseImportDate(s string, layouts ...string) (time.Time, error) {
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("no date layout matched %q", s)
}

// cell returns the row's value at idx, or "" for a missing index (short
// rows never index out of range).
func cell(row []string, idx int) string {
	if idx < 0 || idx >= len(row) {
		return ""
	}
	return row[idx]
}

// csvFieldIndex is a canonical field's position in a mapped csv row:
// csvMappedRow fills the row in csvparse.Fields order, so date_posted /
// amount / description sit at fixed canonical positions.
func csvFieldIndex(f csvparse.Field) int {
	for i, c := range csvparse.Fields {
		if c == f {
			return i
		}
	}
	return -1
}

// importRowColumns resolves, for one mapped file, the row positions of
// the essential fields (date_posted, amount, description — -1 when
// unmapped) and the date layouts to try. csv rows are canonical-order
// (the essential check re-resolves the mapping against the header);
// json rows follow the spec's field order (names carries it); ofx rows
// are fixed-position (the parser emits date, amount, name).
func importRowColumns(m Mapping, csvFile *csvparse.File, names []string) (dateIdx, amountIdx, descIdx int, layouts []string, err error) {
	switch m.Kind {
	case "csv":
		dateIdx = csvFieldIndex(csvparse.FieldDatePosted)
		amountIdx = csvFieldIndex(csvparse.FieldAmount)
		descIdx = csvFieldIndex(csvparse.FieldDescription)
		resolved := resolveCSV(m, csvFile.Header)
		for _, f := range []csvparse.Field{csvparse.FieldDatePosted, csvparse.FieldAmount} {
			ref, ok := resolved[f]
			if !ok || ref.Index == nil || *ref.Index < 0 {
				return 0, 0, 0, nil, fmt.Errorf("mapping does not cover %s", f)
			}
		}
		// The date layout is the inferred format of the mapped date
		// column (its index is a FILE column position); unrecognized
		// or missing formats fall back to the json list.
		layouts = jsonDateLayouts
		dateCol := *resolved[csvparse.FieldDatePosted].Index
		if dateCol < len(csvFile.Columns) {
			if l := csvDateFormatToLayout(csvFile.Columns[dateCol].DateFormat); l != "" {
				layouts = []string{l}
			}
		}
	case "json":
		dateIdx, amountIdx, descIdx = -1, -1, -1
		for i, n := range names {
			switch n {
			case string(csvparse.FieldDatePosted):
				dateIdx = i
			case string(csvparse.FieldAmount):
				amountIdx = i
			case string(csvparse.FieldDescription):
				descIdx = i
			}
		}
		if dateIdx < 0 {
			return 0, 0, 0, nil, errors.New("mapping does not cover date_posted")
		}
		if amountIdx < 0 {
			return 0, 0, 0, nil, errors.New("mapping does not cover amount")
		}
		layouts = jsonDateLayouts
	case "ofx":
		// Fixed layout (see mappedFile): date, amount, name.
		dateIdx, amountIdx, descIdx = 0, 1, 2
		layouts = ofxDateLayouts
	}
	return dateIdx, amountIdx, descIdx, layouts, nil
}

// importRun serves POST /api/v1/imports/{id}/run (step 4.6). The job
// must be in the "mapping" state (terminal and earlier states answer
// 409); after StartRun every exit is terminal — a failure is persisted
// as status "failed" and answered with the raw row, the same 200 shape
// a successful run answers with.
func (h *handler) importRun(w http.ResponseWriter, r *http.Request) {
	if h.imports == nil || h.transactions == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	id := chi.URLParam(r, "id")
	if !uuidShapeOK(id) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "import job not found")
		return
	}
	job, err := h.imports.Get(ctx, c.row.ID, id, c.dek)
	if err != nil {
		h.importError(w, "run", err)
		return
	}
	if job.Status != "mapping" {
		writeAPIError(w, http.StatusConflict, codeConflict, "import job must be in the mapping state to run")
		return
	}
	job, err = h.imports.StartRun(ctx, c.row.ID, id, c.dek)
	if err != nil {
		h.importError(w, "run", err)
		return
	}

	file, err := h.imports.ReadFile(ctx, c.row.ID, id, c.dek)
	if err != nil {
		h.runFatal(w, r, c, job, err)
		return
	}
	var m Mapping
	if err := json.Unmarshal(job.Mapping, &m); err != nil {
		h.runFatal(w, r, c, job, fmt.Errorf("import job mapping is not a mapping document: %w", err))
		return
	}
	if err := m.validate(); err != nil {
		h.runFatal(w, r, c, job, fmt.Errorf("import job mapping is invalid: %w", err))
		return
	}
	names, rows, _, csvFile, msg := mappedFile(m, file)
	if msg != "" {
		h.runFatal(w, r, c, job, errors.New(msg))
		return
	}
	dateIdx, amountIdx, descIdx, layouts, err := importRowColumns(m, csvFile, names)
	if err != nil {
		h.runFatal(w, r, c, job, err)
		return
	}

	res := importRunResult{Errors: []rowError{}}
	var offMin, offMax *time.Time // posted-date range of the inserted rows
	for i, row := range rows {
		dateCell := cell(row, dateIdx)
		date, err := parseImportDate(normalizeOFXZone(dateCell), layouts...)
		if err != nil {
			res.Errors = append(res.Errors, rowError{
				Row:     i,
				Field:   "date_posted",
				Message: fmt.Sprintf("not a valid date: %q", dateCell),
			})
			continue
		}
		amount, err := repo.CanonicalAmount(cell(row, amountIdx))
		if err != nil {
			res.Errors = append(res.Errors, rowError{
				Row:     i,
				Field:   "amount",
				Message: err.Error(),
			})
			continue
		}
		in := repo.TransactionIn{
			AccountID:    job.AccountID,
			DateExecuted: &date, // date_executed = date_posted always
			DatePosted:   date,
			Amount:       amount,
			Description:  strings.TrimSpace(cell(row, descIdx)),
		}
		_, inserted, err := h.transactions.Upsert(ctx, c.row.ID, in, c.dek)
		if err != nil {
			h.runFatal(w, r, c, job, err)
			return
		}
		if inserted {
			res.Inserted++
			if offMin == nil || date.Before(*offMin) {
				offMin = &date
			}
			if offMax == nil || date.After(*offMax) {
				offMax = &date
			}
		} else {
			res.Skipped++
		}
	}

	raw, err := json.Marshal(res)
	if err != nil {
		h.runFatal(w, r, c, job, err)
		return
	}
	result := json.RawMessage(raw)
	finished, err := h.imports.FinishRun(ctx, c.row.ID, id, "done", &result, nil, c.dek)
	if err != nil {
		h.runFatal(w, r, c, job, err)
		return
	}
	// Plan 5.4: chain a categorise job for the newly inserted rows —
	// best effort: on error the import job stays "done" and the user
	// can trigger a manual categorise later. Skipped when nothing new
	// landed (a re-import with every row deduped has nothing to
	// categorise) or when no jobs store is wired (fake-store tests).
	if h.jobs != nil && res.Inserted > 0 {
		if _, cerr := h.jobs.Create(ctx, c.row.ID, "import", &finished.ID); cerr != nil {
			slog.Warn("create chained categorise job failed",
				"import_job", finished.ID, "error", cerr)
		}
	}
	// Offset scan (migration 0012): the inserted rows may net out
	// against each other or against older rows (a credit-card payment
	// imported from two institutions) — the window spans the inserted
	// date range so the older leg can fall outside it. Best effort
	// like the categorise chain: on error the import stays "done" and
	// a manual scan (POST /api/v1/offsets/scan) can retry.
	if h.offsets != nil && res.Inserted > 0 && offMin != nil {
		cands, oerr := h.offsets.FindOffsetCandidates(ctx, c.row.ID, c.dek, offMin, offMax, offsetScanWindowDays)
		if oerr != nil {
			slog.Warn("chained offset scan failed",
				"import_job", finished.ID, "error", oerr)
		} else if n, ierr := h.offsets.InsertOffsetSuggestions(ctx, c.row.ID, cands); ierr != nil {
			slog.Warn("chained offset suggestions failed",
				"import_job", finished.ID, "error", ierr, "suggestions", n)
		}
	}
	writeJSON(w, http.StatusOK, toImportOut(finished))
}

// runFatal persists the run failure (status "failed", the message, no
// result) and answers with the failed row — a dead endpoint would
// otherwise strand the job in "importing" forever.
func (h *handler) runFatal(w http.ResponseWriter, r *http.Request, c caller, job repo.ImportJob, err error) {
	slog.Error("import run failed", "job", job.ID, "error", err)
	msg := err.Error()
	finished, ferr := h.imports.FinishRun(r.Context(), c.row.ID, job.ID, "failed", nil, &msg, c.dek)
	if ferr != nil {
		h.importError(w, "run", ferr)
		return
	}
	writeJSON(w, http.StatusOK, toImportOut(finished))
}

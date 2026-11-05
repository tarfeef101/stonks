// Transaction endpoints (plan step 3.2): GET/POST /api/v1/transactions
// and PATCH/DELETE /api/v1/transactions/{id} over repo.TransactionStore.
// POST is the manual-entry path (the 4.7 importer reuses the same repo
// Create); a dedup-key collision is a 409, not an error. Encrypted
// fields (description, notes, raw) are sealed by the repo layer — the
// handlers only ever see plaintext in and out — and every store call is
// scoped to the caller's users.id.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"finance-tracker/internal/repo"

	"github.com/go-chi/chi/v5"
)

// TransactionStore is the persistence surface the transaction handlers
// need. *repo.TransactionStore satisfies it (its extra Get is unused
// here); route tests use an in-memory implementation. Upsert is the
// dedup-aware insert the 4.6 import runner calls per row.
type TransactionStore interface {
	List(ctx context.Context, userID string, f repo.TransactionFilter, dek []byte) ([]repo.Transaction, error)
	Create(ctx context.Context, userID string, in repo.TransactionIn, dek []byte) (repo.Transaction, error)
	Upsert(ctx context.Context, userID string, in repo.TransactionIn, dek []byte) (repo.Transaction, bool, error)
	Update(ctx context.Context, userID, id string, p repo.TransactionPatch, dek []byte) (repo.Transaction, error)
	Delete(ctx context.Context, userID, id string) error
	Aggregate(ctx context.Context, userID string, from, to time.Time) ([]repo.CategorySpend, error)
}

// Wire DTOs. transactionOut excludes DedupHash (re-import identity) and
// the audit timestamps (bookkeeping); date columns are "YYYY-MM-DD"
// strings and unset DateExecuted/CategoryID/Notes/Raw are JSON null.
// Amount is the canonical 2-decimal form.
type transactionOut struct {
	ID            string  `json:"id"`
	AccountID     string  `json:"account_id"`
	DateExecuted  *string `json:"date_executed"`
	DatePosted    string  `json:"date_posted"`
	Amount        string  `json:"amount"`
	CategoryID    *string `json:"category_id"`
	Description   string  `json:"description"`
	Notes         *string `json:"notes"`
	Raw           *string `json:"raw"`
	ReviewPending bool    `json:"review_pending"`
}

func toTransactionOut(tx repo.Transaction) transactionOut {
	var executed *string
	if tx.DateExecuted != nil {
		s := dateOnly(*tx.DateExecuted)
		executed = &s
	}
	return transactionOut{
		ID:            tx.ID,
		AccountID:     tx.AccountID,
		DateExecuted:  executed,
		DatePosted:    dateOnly(tx.DatePosted),
		Amount:        tx.Amount,
		CategoryID:    strOrNil(tx.CategoryID),
		Description:   tx.Description,
		Notes:         strOrNil(tx.Notes),
		Raw:           strOrNil(tx.Raw),
		ReviewPending: tx.ReviewPending,
	}
}

// transactionIn is the POST body. DateExecuted is null (or "") for
// unset, otherwise a "YYYY-MM-DD" string. CategoryID/Notes/Raw are null
// (or "") for unset/uncategorized — the same convention as the repo's
// TransactionIn.
type transactionIn struct {
	AccountID     string  `json:"account_id"`
	DateExecuted  *string `json:"date_executed"`
	DatePosted    string  `json:"date_posted"`
	Amount        string  `json:"amount"`
	CategoryID    *string `json:"category_id"`
	Description   string  `json:"description"`
	Notes         *string `json:"notes"`
	Raw           *string `json:"raw"`
	ReviewPending bool    `json:"review_pending"`
}

// transactionPatch is the PATCH body. Plain pointers are null = absent
// = untouched (there is no NULL-able plain field to clear); the
// NULL-able columns use the tri-states (absent = untouched, null =
// clear, value = set).
type transactionPatch struct {
	Description   *string        `json:"description"`
	Amount        *string        `json:"amount"`
	DatePosted    *string        `json:"date_posted"`
	ReviewPending *bool          `json:"review_pending"`
	CategoryID    triStateString `json:"category_id"`
	Notes         triStateString `json:"notes"`
	Raw           triStateString `json:"raw"`
	DateExecuted  triStateDate   `json:"date_executed"`
}

// dateOnly renders a time as its UTC calendar day, the "YYYY-MM-DD"
// string the repo binds to date columns.
func dateOnly(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

// parseDateParam parses a YYYY-MM-DD value (query param or body field),
// naming the offender in the error. The shape check is deliberate:
// time.Parse would also accept unpadded months/days ("2026-1-2"),
// which is not a date the date columns accept.
func parseDateParam(name, s string) (time.Time, error) {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return time.Time{}, fmt.Errorf("%s must be a YYYY-MM-DD date", name)
	}
	for _, i := range []int{0, 1, 2, 3, 5, 6, 8, 9} {
		if s[i] < '0' || s[i] > '9' {
			return time.Time{}, fmt.Errorf("%s must be a YYYY-MM-DD date", name)
		}
	}
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be a YYYY-MM-DD date", name)
	}
	return d, nil
}

// afterDateOnly is the repo's date-precision rule (repo.afterDate):
// strictly-after, comparing calendar-day strings.
func afterDateOnly(a, b time.Time) bool {
	return dateOnly(a) > dateOnly(b)
}

// buildTxIn converts the POST body to repo input, running every check
// the repo would otherwise surface as a 500 (malformed ids, dates,
// amounts) with the repo's exact messages; all problems are collected
// and joined with "; ". Reference existence stays in the repo —
// ErrAccountNotFound/ErrCategoryNotFound come back as 404s.
func buildTxIn(b transactionIn) (repo.TransactionIn, error) {
	var problems []string
	in := repo.TransactionIn{
		AccountID:     b.AccountID,
		Amount:        b.Amount,
		Description:   b.Description,
		ReviewPending: b.ReviewPending,
	}
	if !uuidShapeOK(b.AccountID) {
		problems = append(problems, "account_id must be a valid UUID")
	}
	postedOK := true
	if s, err := parseDateParam("date_posted", b.DatePosted); err != nil {
		problems = append(problems, err.Error())
		postedOK = false
	} else {
		in.DatePosted = s
	}
	if b.DateExecuted != nil && *b.DateExecuted != "" {
		if s, err := parseDateParam("date_executed", *b.DateExecuted); err != nil {
			problems = append(problems, err.Error())
		} else {
			in.DateExecuted = &s
		}
	}
	if b.CategoryID != nil {
		if *b.CategoryID == "" {
			b.CategoryID = nil
		} else if !uuidShapeOK(*b.CategoryID) {
			problems = append(problems, "category_id must be a valid UUID")
		}
	}
	if b.CategoryID != nil {
		in.CategoryID = b.CategoryID
	}
	if b.Notes != nil && *b.Notes != "" {
		in.Notes = b.Notes
	}
	if b.Raw != nil && *b.Raw != "" {
		in.Raw = b.Raw
	}
	if _, err := repo.CanonicalAmount(b.Amount); err != nil {
		problems = append(problems, err.Error())
	}
	if postedOK && in.DateExecuted != nil && afterDateOnly(*in.DateExecuted, in.DatePosted) {
		problems = append(problems, "date_executed must not be after date_posted")
	}
	if len(problems) > 0 {
		return repo.TransactionIn{}, errors.New(strings.Join(problems, "; "))
	}
	return in, nil
}

// buildTxPatch converts the PATCH body to repo input. Wrong tri-state
// shapes already 400 from UnmarshalJSON; this checks the set values
// (amount canonical, date_posted shape, category_id shape, and the
// in-request date rule — the repo re-checks it against the stored row).
func buildTxPatch(b transactionPatch) (repo.TransactionPatch, error) {
	p := repo.TransactionPatch{
		Description:   b.Description,
		Amount:        b.Amount,
		CategoryID:    b.CategoryID.toRepo(),
		Notes:         b.Notes.toRepo(),
		Raw:           b.Raw.toRepo(),
		DateExecuted:  b.DateExecuted.toRepo(),
		ReviewPending: b.ReviewPending,
	}
	var problems []string
	if b.Amount != nil {
		if _, err := repo.CanonicalAmount(*b.Amount); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if b.DatePosted != nil {
		if s, err := parseDateParam("date_posted", *b.DatePosted); err != nil {
			problems = append(problems, err.Error())
		} else {
			p.DatePosted = &s
		}
	}
	if b.CategoryID.set && b.CategoryID.value != nil && *b.CategoryID.value != "" && !uuidShapeOK(*b.CategoryID.value) {
		problems = append(problems, "category_id must be a valid UUID")
	}
	if p.DatePosted != nil && b.DateExecuted.set && b.DateExecuted.value != nil {
		if afterDateOnly(*b.DateExecuted.value, *p.DatePosted) {
			problems = append(problems, "date_executed must not be after date_posted")
		}
	}
	if len(problems) > 0 {
		return repo.TransactionPatch{}, errors.New(strings.Join(problems, "; "))
	}
	return p, nil
}

// txFilterFromQuery builds the List query from the query string.
// account_id is repeatable; category_id's first value applies ("" =
// uncategorized only); date_from/date_to bound date_posted inclusively;
// min/max_amount are canonicalized; limit/offset are integers >= 0.
// Empty-string params are treated as absent.
func txFilterFromQuery(q url.Values) (repo.TransactionFilter, error) {
	var f repo.TransactionFilter
	for _, a := range q["account_id"] {
		if a == "" {
			continue
		}
		if !uuidShapeOK(a) {
			return f, errors.New("account_id must be a valid UUID")
		}
		f.AccountIDs = append(f.AccountIDs, a)
	}
	if v, ok := q["category_id"]; ok && len(v) > 0 {
		c := v[0]
		if c == "" {
			uncat := ""
			f.CategoryID = &uncat
		} else if !uuidShapeOK(c) {
			return f, errors.New("category_id must be a valid UUID")
		} else {
			f.CategoryID = &c
		}
	}
	if s := q.Get("date_from"); s != "" {
		d, err := parseDateParam("date_from", s)
		if err != nil {
			return f, err
		}
		f.DateFrom = &d
	}
	if s := q.Get("date_to"); s != "" {
		d, err := parseDateParam("date_to", s)
		if err != nil {
			return f, err
		}
		f.DateTo = &d
	}
	if s := q.Get("min_amount"); s != "" {
		v, err := repo.CanonicalAmount(s)
		if err != nil {
			return f, err
		}
		f.MinAmount = &v
	}
	if s := q.Get("max_amount"); s != "" {
		v, err := repo.CanonicalAmount(s)
		if err != nil {
			return f, err
		}
		f.MaxAmount = &v
	}
	f.Search = q.Get("search")
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return f, errors.New("limit must be an integer >= 0")
		}
		f.Limit = n
	}
	if s := q.Get("offset"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return f, errors.New("offset must be an integer >= 0")
		}
		f.Offset = n
	}
	return f, nil
}

// transactionError maps the repo's transaction sentinels to responses;
// everything else is a 500 with the details only in the server log.
func (h *handler) transactionError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, repo.ErrDateRule):
		// The create path catches this in validation; the patch path
		// only sees it from the repo's check against the stored row.
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "date_executed must not be after date_posted")
	case errors.Is(err, repo.ErrTransactionNotFound):
		writeAPIError(w, http.StatusNotFound, codeNotFound, "transaction not found")
	case errors.Is(err, repo.ErrAccountNotFound):
		writeAPIError(w, http.StatusNotFound, codeNotFound, "account not found")
	case errors.Is(err, repo.ErrCategoryNotFound):
		writeAPIError(w, http.StatusNotFound, codeNotFound, "category not found")
	case errors.Is(err, repo.ErrDedupConflict):
		writeAPIError(w, http.StatusConflict, codeConflict, "a transaction with this dedup key already exists")
	default:
		slog.Error("transaction operation failed", "op", op, "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "transaction operation failed")
	}
}

// transactionsList serves GET /api/v1/transactions with the filters
// documented on txFilterFromQuery.
func (h *handler) transactionsList(w http.ResponseWriter, r *http.Request) {
	if h.transactions == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	f, err := txFilterFromQuery(r.URL.Query())
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	txs, err := h.transactions.List(r.Context(), c.row.ID, f, c.dek)
	if err != nil {
		h.transactionError(w, "list", err)
		return
	}
	out := make([]transactionOut, 0, len(txs))
	for _, tx := range txs {
		out = append(out, toTransactionOut(tx))
	}
	writeJSON(w, http.StatusOK, out)
}

// transactionsCreate serves POST /api/v1/transactions (manual entry)
// and answers 201 with the created row. A dedup-key collision answers
// 409 (the re-import path, step 4.7, is the one that wants the existing
// row back — via the repo's Upsert, not this endpoint).
func (h *handler) transactionsCreate(w http.ResponseWriter, r *http.Request) {
	if h.transactions == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	var in transactionIn
	if err := decodeBody(r, &in); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "body must be a JSON object: "+err.Error())
		return
	}
	repoIn, err := buildTxIn(in)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	tx, err := h.transactions.Create(r.Context(), c.row.ID, repoIn, c.dek)
	if err != nil {
		h.transactionError(w, "create", err)
		return
	}
	writeJSON(w, http.StatusCreated, toTransactionOut(tx))
}

// transactionsUpdate serves PATCH /api/v1/transactions/{id}. A
// malformed id answers 404 (not_found) instead of reaching the DB; a
// well-formed id the caller does not own is also 404 (existence is not
// disclosed).
func (h *handler) transactionsUpdate(w http.ResponseWriter, r *http.Request) {
	if h.transactions == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if !uuidShapeOK(id) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "transaction not found")
		return
	}
	var p transactionPatch
	if err := decodeBody(r, &p); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "body must be a JSON object: "+err.Error())
		return
	}
	if p.Description == nil && p.Amount == nil && p.DatePosted == nil && p.ReviewPending == nil &&
		!p.CategoryID.set && !p.Notes.set && !p.Raw.set && !p.DateExecuted.set {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "empty patch: provide at least one field")
		return
	}
	repoP, err := buildTxPatch(p)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	tx, err := h.transactions.Update(r.Context(), c.row.ID, id, repoP, c.dek)
	if err != nil {
		h.transactionError(w, "update", err)
		return
	}
	writeJSON(w, http.StatusOK, toTransactionOut(tx))
}

// transactionsDelete serves DELETE /api/v1/transactions/{id} and
// answers 204 (no content) on success.
func (h *handler) transactionsDelete(w http.ResponseWriter, r *http.Request) {
	if h.transactions == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if !uuidShapeOK(id) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "transaction not found")
		return
	}
	if err := h.transactions.Delete(r.Context(), c.row.ID, id); err != nil {
		h.transactionError(w, "delete", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

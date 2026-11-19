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
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"finance-tracker/internal/repo"

	"github.com/go-chi/chi/v5"
)

// TransactionStore is the persistence surface the transaction handlers
// need. *repo.TransactionStore satisfies it in full; route tests use
// an in-memory implementation. Get backs the PATCH-time transfer-leg
// lookup and CreateTransfer persists both legs of a transfer under one
// shared group. Upsert is the
// dedup-aware single-row insert; UpsertBatch is the dedup-aware batch
// insert the 4.6 import runner calls once per run (perRow[i] is true
// iff ins[i] landed, false when its dedup identity already existed).
type TransactionStore interface {
	List(ctx context.Context, userID string, f repo.TransactionFilter, dek []byte) ([]repo.Transaction, error)
	AccountDebits(ctx context.Context, accountID string) ([]repo.DebitTxn, error)
	Create(ctx context.Context, userID string, in repo.TransactionIn, dek []byte) (repo.Transaction, error)
	Upsert(ctx context.Context, userID string, in repo.TransactionIn, dek []byte) (repo.Transaction, bool, error)
	UpsertBatch(ctx context.Context, userID string, ins []repo.TransactionIn, dek []byte) ([]bool, error)
	Update(ctx context.Context, userID, id string, p repo.TransactionPatch, dek []byte) (repo.Transaction, error)
	Delete(ctx context.Context, userID, id string) error
	Aggregate(ctx context.Context, userID string, from, to time.Time, accountIDs []string) ([]repo.CategorySpend, error)
	TrendAggregate(ctx context.Context, userID, period string, from, to time.Time, accountIDs []string) ([]repo.TrendBucket, error)
	SavingsSums(ctx context.Context, userID string) ([]repo.SavingsSum, error)
	Get(ctx context.Context, userID, id string, dek []byte) (repo.Transaction, error)
	CreateTransfer(ctx context.Context, userID string, in repo.TransferIn, dek []byte) (fromLeg, toLeg repo.Transaction, err error)
}

// Wire DTOs. transactionOut excludes DedupHash (re-import identity) and
// the audit timestamps (bookkeeping); date columns are "YYYY-MM-DD"
// strings and unset DateExecuted/CategoryID/Notes/Raw are JSON null.
// Amount is the canonical 2-decimal form.
type transactionOut struct {
	ID                string  `json:"id"`
	AccountID         string  `json:"account_id"`
	DateExecuted      *string `json:"date_executed"`
	DatePosted        string  `json:"date_posted"`
	Amount            string  `json:"amount"`
	Currency          string  `json:"currency"`
	AmountConverted   string  `json:"amount_converted"`
	ConvertedCurrency string  `json:"converted_currency"`
	CategoryID        *string `json:"category_id"`
	Description       string  `json:"description"`
	Notes             *string `json:"notes"`
	Raw               *string `json:"raw"`
	ReviewPending     bool    `json:"review_pending"`
	TransferGroupID   *string `json:"transfer_group_id"`
	ExternalOrigin    *string `json:"external_origin"`
}

func toTransactionOut(tx repo.Transaction) transactionOut {
	var executed *string
	if tx.DateExecuted != nil {
		s := dateOnly(*tx.DateExecuted)
		executed = &s
	}
	return transactionOut{
		ID:              tx.ID,
		AccountID:       tx.AccountID,
		DateExecuted:    executed,
		DatePosted:      dateOnly(tx.DatePosted),
		Amount:          tx.Amount,
		CategoryID:      strOrNil(tx.CategoryID),
		Description:     tx.Description,
		Notes:           strOrNil(tx.Notes),
		Raw:             strOrNil(tx.Raw),
		ReviewPending:   tx.ReviewPending,
		TransferGroupID: tx.TransferGroupID,
		ExternalOrigin:  tx.ExternalOrigin,
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
	// TransferFromAccountID present (non-null) makes the POST a
	// transfer between the two accounts; the answer is the two legs
	// keyed "from" (source debit) and "to" (destination credit)
	// instead of one row. Null or "" is a normal single-row create.
	TransferFromAccountID *string `json:"transfer_from_account_id"`
	// ExternalOrigin names where an external contribution came from
	// (the 0019 vocabulary); null or "" = a normal user contribution.
	ExternalOrigin *string `json:"external_origin"`
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

// externalOriginSet is the 0019 origin vocabulary (the handler's own
// copy of the repo's set: buildTxIn pre-validates so a bad value is a
// 400, not a 500 through transactionError).
var externalOriginSet = map[string]bool{
	"employer":         true,
	"government_grant": true,
	"inherited":        true,
	"other":            true,
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
	canonical, canErr := repo.CanonicalAmount(b.Amount)
	if canErr != nil {
		problems = append(problems, canErr.Error())
	}
	if postedOK && in.DateExecuted != nil && afterDateOnly(*in.DateExecuted, in.DatePosted) {
		problems = append(problems, "date_executed must not be after date_posted")
	}
	if b.ExternalOrigin != nil && *b.ExternalOrigin != "" {
		if !externalOriginSet[*b.ExternalOrigin] {
			problems = append(problems, "external_origin must be one of: employer, government_grant, inherited, other")
		} else if canErr != nil || canonical == "0.00" || strings.HasPrefix(canonical, "-") {
			problems = append(problems, "external_origin requires a positive amount")
		} else {
			o := *b.ExternalOrigin
			in.ExternalOrigin = &o
		}
	}
	if len(problems) > 0 {
		return repo.TransactionIn{}, errors.New(strings.Join(problems, "; "))
	}
	return in, nil
}

// buildTransferIn converts a transfer POST body (transfer_from_account_id
// present) to repo.TransferIn. The shared checks already ran in
// buildTxIn (account shape, dates, canonical amount, the date rule);
// this adds the transfer-specific rules with the repo's exact messages:
// the source must be a valid UUID different from the destination, the
// amount must be positive, and category/origin are not transfer fields.
// All problems are collected and joined with "; " (house style).
func buildTransferIn(b transactionIn, repoIn repo.TransactionIn) (repo.TransferIn, error) {
	var problems []string
	from := *b.TransferFromAccountID
	if !uuidShapeOK(from) {
		problems = append(problems, "transfer_from_account_id must be a valid UUID")
	}
	if from == repoIn.AccountID {
		problems = append(problems, "transfer source and destination must be different accounts")
	}
	canonical, canErr := repo.CanonicalAmount(repoIn.Amount)
	if canErr == nil && (canonical == "0.00" || strings.HasPrefix(canonical, "-")) {
		problems = append(problems, "transfer amount must be positive")
	}
	if b.CategoryID != nil {
		problems = append(problems, "category_id is not allowed on a transfer")
	}
	if b.ExternalOrigin != nil && *b.ExternalOrigin != "" {
		problems = append(problems, "external_origin is not allowed on a transfer")
	}
	if len(problems) > 0 {
		return repo.TransferIn{}, errors.New(strings.Join(problems, "; "))
	}
	return repo.TransferIn{
		FromAccountID: from,
		ToAccountID:   repoIn.AccountID,
		DateExecuted:  repoIn.DateExecuted,
		DatePosted:    repoIn.DatePosted,
		Amount:        repoIn.Amount,
		Description:   repoIn.Description,
		Notes:         repoIn.Notes,
		Raw:           repoIn.Raw,
		ReviewPending: repoIn.ReviewPending,
	}, nil
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
// account_id is repeatable; category_id is repeatable too — each value
// is a UUID or "" (uncategorized only) — and combines with OR;
// date_from/date_to bound date_posted inclusively;
// min/max_amount are canonicalized; sign is "spending" (amount < 0) or
// "income" (amount > 0) — with sign set, min/max_amount are absolute
// magnitudes and a negative value is a 400; limit/offset are integers
// >= 0. Empty-string params are treated as absent.
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
	for _, c := range q["category_id"] {
		if c != "" && !uuidShapeOK(c) {
			return f, errors.New("category_id must be a valid UUID")
		}
		f.CategoryIDs = append(f.CategoryIDs, c)
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
	if s := q.Get("sign"); s != "" {
		if s != "spending" && s != "income" {
			return f, errors.New(`sign must be "spending" or "income"`)
		}
		f.Sign = s
	}
	// With a sign, the amount bounds are absolute magnitudes (spending
	// matches -max..-min, income matches min..max); negatives are a 400.
	if f.Sign != "" {
		if f.MinAmount != nil && strings.HasPrefix(*f.MinAmount, "-") {
			return f, errors.New("min_amount must be non-negative when sign is set")
		}
		if f.MaxAmount != nil && strings.HasPrefix(*f.MaxAmount, "-") {
			return f, errors.New("max_amount must be non-negative when sign is set")
		}
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

	// Display-only FX (REVIEW-PLAN §4.4): each row is converted into
	// the user's default currency at its posting date. The account
	// currency map is one List per request (a repo-side join would
	// ripple into the shared txSelect); any lookup failure degrades
	// to showing the row in its own currency.
	var fxCache *fxCache
	base := ""
	curByAcc := make(map[string]string)
	if h.fx != nil && h.settings != nil {
		if b, err := h.settings.DefaultCurrency(r.Context(), c.row.Sub); err != nil {
			slog.Warn("default currency lookup failed; showing amounts unconverted", "error", err)
		} else {
			base = b
			fxCache = newFXCache(h.fx, base)
			if h.accounts != nil {
				if accts, err := h.accounts.List(r.Context(), c.row.ID, c.dek); err != nil {
					slog.Warn("account list failed; showing amounts unconverted", "error", err)
				} else {
					for _, a := range accts {
						curByAcc[a.ID] = a.Currency
					}
				}
			}
		}
	}
	out := make([]transactionOut, 0, len(txs))
	for _, tx := range txs {
		o := toTransactionOut(tx)
		cur := curByAcc[tx.AccountID]
		if fxCache != nil {
			cents, err := parseCents(tx.Amount)
			if err != nil {
				slog.Warn("transaction amount not convertible", "id", tx.ID, "error", err)
			} else if factor, conv := fxCache.convert(r.Context(), tx.DatePosted, cur); conv {
				o.Currency = cur
				o.AmountConverted = formatCents(int64(math.Round(float64(cents) * factor)))
				o.ConvertedCurrency = base
			}
		}
		out = append(out, o)
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
	if in.TransferFromAccountID != nil {
		tIn, err := buildTransferIn(in, repoIn)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
			return
		}
		fromLeg, toLeg, err := h.transactions.CreateTransfer(r.Context(), c.row.ID, tIn, c.dek)
		if err != nil {
			h.transactionError(w, "create-transfer", err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]transactionOut{
			"from": toTransactionOut(fromLeg),
			"to":   toTransactionOut(toLeg),
		})
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
// disclosed). A transfer leg answers 409 for any patch to its amount
// or dates: the legs share one group, and editing either would desync
// its partner (the repo has no such rule, so the handler enforces it).
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
	if repoP.Amount != nil || repoP.DatePosted != nil || repoP.DateExecuted != nil {
		cur, err := h.transactions.Get(r.Context(), c.row.ID, id, c.dek)
		if err != nil {
			h.transactionError(w, "get", err)
			return
		}
		if cur.TransferGroupID != nil {
			writeAPIError(w, http.StatusConflict, codeConflict, "transfer legs cannot be modified: amount, date_posted and date_executed are immutable")
			return
		}
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

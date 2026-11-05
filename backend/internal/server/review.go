// Review queue endpoints (plan step 5.5): GET /api/v1/review (the
// queue the categorise worker flagged), POST
// /api/v1/review/{id}/accept (resolve one row with one of the
// caller's categories), POST /api/v1/review/accept-all (resolve every
// row with one category) and POST /api/v1/review/{id}/reprompt
// (re-run the LLM on one row). A row leaves the queue when it gets a
// category: the queue predicate is review_pending = true AND
// category_id IS NULL, so a manual edit that sets a category resolves
// the row too. Every store call is scoped to the caller's users.id.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"finance-tracker/internal/categorize"
	"finance-tracker/internal/llm"
	"finance-tracker/internal/repo"

	"github.com/go-chi/chi/v5"
)

// ReviewStore is the persistence surface the review handlers need.
// *repo.TransactionStore satisfies it (its extra transaction-CRUD
// methods are unused here); route tests use an in-memory
// implementation.
type ReviewStore interface {
	ReviewPending(ctx context.Context, userID string, dek []byte) ([]repo.Transaction, error)
	GetReviewPending(ctx context.Context, userID, id string, dek []byte) (repo.Transaction, error)
	AcceptReview(ctx context.Context, userID, txnID, categoryID string) error
	AcceptAll(ctx context.Context, userID, categoryID string) (int, error)
}

// CredentialResolver resolves the user's stored LLM credential, the
// same resolution the worker applies (the most-recently-updated
// credential wins; ErrCredentialNotFound means "fall back to the
// process env").
type CredentialResolver interface {
	Resolve(ctx context.Context, userID string, dek []byte) (repo.ResolvedCredential, error)
}

// CategoryLister lists the user's categories (reprompt builds its
// single-item prompt from them, like the worker does per batch).
type CategoryLister interface {
	List(ctx context.Context, userID string, dek []byte) ([]repo.Category, error)
}

// Reprompter re-runs the LLM on one review-pending transaction (the
// reprompt route). It is deliberately interface-based — unlike the
// worker's concrete-store Runner — so the reprompt route tests run
// fully in-memory like the other route tests.
type Reprompter struct {
	Creds CredentialResolver
	Cats  CategoryLister
	Txns  ReviewStore
	Env   categorize.LLMEnv
	LLM   func(baseURL, apiKey, model string) (llm.Categorizer, error)
}

// Reprompt re-categorises one review-pending transaction: the provider
// is resolved like the worker's (stored credential, else the env
// fallback — NormalizeEnv fills the fallback's empty BaseURL/Model),
// the user's categories are listed, and a single-item categorize.Run
// executes. A suggestion at or above the threshold is applied via
// ReviewStore.AcceptReview (category set, review_pending cleared) and
// applied reports true. A row that was resolved in the meantime
// (concurrent accept) is not an error: the suggestion comes back with
// applied false.
func (p *Reprompter) Reprompt(ctx context.Context, userID string, dek []byte, tx repo.Transaction) (categorize.Suggestion, bool, error) {
	if p.Creds == nil || p.Cats == nil || p.Txns == nil || p.LLM == nil {
		return categorize.Suggestion{}, false, errors.New("reprompt not configured")
	}
	env := categorize.NormalizeEnv(p.Env)

	baseURL, apiKey := env.BaseURL, env.APIKey
	rc, rerr := p.Creds.Resolve(ctx, userID, dek)
	if rerr == nil {
		baseURL, apiKey = rc.Endpoint, rc.APIKey
	} else if !errors.Is(rerr, repo.ErrCredentialNotFound) {
		return categorize.Suggestion{}, false, rerr
	}

	catRows, err := p.Cats.List(ctx, userID, dek)
	if err != nil {
		return categorize.Suggestion{}, false, err
	}
	if len(catRows) == 0 {
		return categorize.Suggestion{}, false, errors.New("user has no categories")
	}
	categories := make([]categorize.Category, 0, len(catRows))
	for _, c := range catRows {
		categories = append(categories, categorize.Category{ID: c.ID, Name: c.Name})
	}

	c, err := p.LLM(baseURL, apiKey, env.Model)
	if err != nil {
		return categorize.Suggestion{}, false, err
	}
	sugs, _, err := categorize.Run(ctx, c, categories, []categorize.Item{
		{Description: tx.Description, Amount: tx.Amount},
	})
	if err != nil {
		return categorize.Suggestion{}, false, err
	}
	sug, ok := sugs[1] // single item: the 1-based key
	if !ok {
		return categorize.Suggestion{}, false, errors.New("no suggestion returned for the transaction")
	}
	if sug.NeedsReview(categorize.DefaultThreshold) {
		return sug, false, nil
	}
	if aerr := p.Txns.AcceptReview(ctx, userID, tx.ID, sug.CategoryID); aerr != nil {
		if errors.Is(aerr, repo.ErrTransactionNotFound) {
			// The row was resolved concurrently (a manual accept
			// raced this reprompt): report the suggestion, applied
			// false — the queue is empty, nothing to fix.
			return sug, false, nil
		}
		return sug, false, aerr
	}
	return sug, true, nil
}

// Wire DTOs. reviewOut keeps the review row minimal — the category
// picker is filled from the existing GET /api/v1/categories — and
// matches the transactions-list house style (canonical amount string,
// "YYYY-MM-DD" date_posted).
type reviewOut struct {
	ID          string `json:"id"`
	AccountID   string `json:"account_id"`
	DatePosted  string `json:"date_posted"`
	Amount      string `json:"amount"`
	Description string `json:"description"`
}

func toReviewOut(tx repo.Transaction) reviewOut {
	return reviewOut{
		ID:          tx.ID,
		AccountID:   tx.AccountID,
		DatePosted:  dateOnly(tx.DatePosted),
		Amount:      tx.Amount,
		Description: tx.Description,
	}
}

// repromptOut is the reprompt response: the model's answer for the one
// row and whether it was applied (confidence at or above the
// threshold).
type repromptOut struct {
	Category   string  `json:"category"`
	Confidence float64 `json:"confidence"`
	Rationale  string  `json:"rationale"`
	Applied    bool    `json:"applied"`
}

// acceptAllBody is the POST /api/v1/review/accept-all body.
type acceptAllBody struct {
	CategoryID string `json:"category_id"`
}

// acceptAllOut is the accept-all response: how many rows were
// resolved by the call.
type acceptAllOut struct {
	Applied int `json:"applied"`
}

// reviewError maps the review repo sentinels to responses. NOTE:
// ErrCategoryNotFound is a 400 here, NOT the transactions routes' 404
// (transactionError): a category that is not the caller's is a
// request problem, and the row the request points at still exists.
// Everything else is a 500 with the details only in the server log.
func (h *handler) reviewError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, repo.ErrTransactionNotFound):
		writeAPIError(w, http.StatusNotFound, codeNotFound, "transaction not found")
	case errors.Is(err, repo.ErrCategoryNotFound):
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "category_id is not one of your categories")
	default:
		slog.Error("review operation failed", "op", op, "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "review operation failed")
	}
}

// reviewList serves GET /api/v1/review: the review queue — every row
// the categorise worker flagged that still has no category, oldest
// id first.
func (h *handler) reviewList(w http.ResponseWriter, r *http.Request) {
	if h.review == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	txs, err := h.review.ReviewPending(r.Context(), c.row.ID, c.dek)
	if err != nil {
		h.reviewError(w, "list", err)
		return
	}
	out := make([]reviewOut, 0, len(txs))
	for _, tx := range txs {
		out = append(out, toReviewOut(tx))
	}
	writeJSON(w, http.StatusOK, out)
}

// reviewAccept serves POST /api/v1/review/{id}/accept: resolves one
// review-pending row with one of the caller's own categories and
// answers 200 with the full updated row (the PATCH house pattern). A
// malformed id, a row the caller does not own, and a row that is no
// longer pending all answer 404 (existence is not disclosed).
func (h *handler) reviewAccept(w http.ResponseWriter, r *http.Request) {
	if h.review == nil {
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
	var b struct {
		CategoryID string `json:"category_id"`
	}
	if err := decodeBody(r, &b); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "body must be a JSON object: "+err.Error())
		return
	}
	if !uuidShapeOK(b.CategoryID) {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "category_id must be a valid UUID")
		return
	}
	tx, err := h.review.GetReviewPending(r.Context(), c.row.ID, id, c.dek)
	if err != nil {
		h.reviewError(w, "get", err)
		return
	}
	if err := h.review.AcceptReview(r.Context(), c.row.ID, id, b.CategoryID); err != nil {
		h.reviewError(w, "accept", err)
		return
	}
	// Apply the change locally and echo the full row: GetReviewPending
	// already loaded (and decrypted) it, and AcceptReview is the only
	// mutation between load and response.
	tx.CategoryID = b.CategoryID
	tx.ReviewPending = false
	writeJSON(w, http.StatusOK, toTransactionOut(tx))
}

// reviewAcceptAll serves POST /api/v1/review/accept-all: resolves
// every review-pending row of the caller with one category and
// returns how many it applied (0 when the queue was already empty).
func (h *handler) reviewAcceptAll(w http.ResponseWriter, r *http.Request) {
	if h.review == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	var b acceptAllBody
	if err := decodeBody(r, &b); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "body must be a JSON object: "+err.Error())
		return
	}
	if !uuidShapeOK(b.CategoryID) {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "category_id must be a valid UUID")
		return
	}
	n, err := h.review.AcceptAll(r.Context(), c.row.ID, b.CategoryID)
	if err != nil {
		h.reviewError(w, "accept all", err)
		return
	}
	writeJSON(w, http.StatusOK, acceptAllOut{Applied: n})
}

// reviewReprompt serves POST /api/v1/review/{id}/reprompt (no body):
// re-runs the LLM on one review-pending row and answers with the
// suggestion; applied is true when the confidence met the threshold
// and the row was resolved by this call. Provider, category-listing
// and persistence failures answer 500 ("reprompt failed") — the row
// stays in the queue either way.
func (h *handler) reviewReprompt(w http.ResponseWriter, r *http.Request) {
	if h.review == nil || h.reprompt == nil {
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
	tx, err := h.review.GetReviewPending(r.Context(), c.row.ID, id, c.dek)
	if err != nil {
		h.reviewError(w, "get", err)
		return
	}
	sug, applied, err := h.reprompt.Reprompt(r.Context(), c.row.ID, c.dek, tx)
	if err != nil {
		slog.Error("reprompt failed", "op", "reprompt", "transaction", id, "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "reprompt failed")
		return
	}
	writeJSON(w, http.StatusOK, repromptOut{
		Category:   sug.Category,
		Confidence: sug.Confidence,
		Rationale:  sug.Rationale,
		Applied:    applied,
	})
}

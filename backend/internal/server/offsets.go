// Offset endpoints (migration 0012): transactions that net each other
// out. Some rows are internal movements between the caller's own
// accounts — paying a credit card from chequing — and must not count
// as spend in one account and income in the other. POST
// /api/v1/offsets/scan runs the matcher over the caller's ungrouped
// rows and stores its pairs as pending suggestions; GET
// /api/v1/offsets/suggestions lists them; the accept and dismiss
// writes resolve one suggestion (accept groups the two legs, dismiss
// drops the pair out of future scans). POST /api/v1/offsets/groups
// hand-groups a balanced set of the caller's rows; GET and DELETE
// list and dissolve groups. Every store call is scoped to the
// caller's users.id.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"finance-tracker/internal/repo"

	"github.com/go-chi/chi/v5"
)

// offsetScanWindowDays is the default posting-date window one scan
// matches within (both legs within 7 days of each other).
const offsetScanWindowDays = 7

// OffsetStore is the persistence surface the offset handlers need.
// *repo.TransactionStore satisfies it (its extra transaction-CRUD
// methods are unused here); route tests use an in-memory
// implementation.
type OffsetStore interface {
	FindOffsetCandidates(ctx context.Context, userID string, dek []byte, from, to *time.Time, windowDays int) ([]repo.OffsetCandidate, error)
	InsertOffsetSuggestions(ctx context.Context, userID string, cands []repo.OffsetCandidate) (int, error)
	ListOffsetSuggestions(ctx context.Context, userID string, dek []byte) ([]repo.OffsetSuggestion, error)
	AcceptOffsetSuggestion(ctx context.Context, userID, id string, dek []byte) (string, error)
	DismissOffsetSuggestion(ctx context.Context, userID, id string) error
	SetOffsetGroup(ctx context.Context, userID string, txIDs []string, dek []byte) (string, error)
	ClearOffsetGroup(ctx context.Context, userID, id string) error
	ListOffsetGroups(ctx context.Context, userID string, dek []byte) ([]repo.OffsetGroup, error)
}

// Wire DTOs. The legs reuse the transactions-list wire shape
// (transactionOut) so the UI can render them without a second
// contract; created_at is RFC3339 UTC (the llm-credentials style).
type offsetSuggestionOut struct {
	ID        string         `json:"id"`
	A         transactionOut `json:"a"`
	B         transactionOut `json:"b"`
	CreatedAt string         `json:"created_at"`
}

type offsetGroupOut struct {
	ID      string           `json:"id"`
	Members []transactionOut `json:"members"`
}

// offsetGroupBody is the POST /api/v1/offsets/groups body: a hand-
// picked set of the caller's transactions (at least two, unique) whose
// amounts must net to exactly zero.
type offsetGroupBody struct {
	TransactionIDs []string `json:"transaction_ids"`
}

// offsetGroupIDOut is the accept/create response: the id of the group
// the rows now share.
type offsetGroupIDOut struct {
	GroupID string `json:"group_id"`
}

// offsetScanOut is the scan response: how many new suggestions the
// scan proposed (0 when the matcher found nothing new).
type offsetScanOut struct {
	Suggested int `json:"suggested"`
}

func toOffsetSuggestionOut(s repo.OffsetSuggestion) offsetSuggestionOut {
	return offsetSuggestionOut{
		ID:        s.ID,
		A:         toTransactionOut(s.A),
		B:         toTransactionOut(s.B),
		CreatedAt: s.CreatedAt.UTC().Format(time.RFC3339),
	}
}

func toOffsetGroupOut(g repo.OffsetGroup) offsetGroupOut {
	members := make([]transactionOut, 0, len(g.Members))
	for _, m := range g.Members {
		members = append(members, toTransactionOut(m))
	}
	return offsetGroupOut{ID: g.ID, Members: members}
}

// offsetError maps the offset repo sentinels to responses:
// ErrOffsetSuggestionNotFound / ErrOffsetGroupNotFound /
// ErrTransactionNotFound are 404s (existence is not disclosed),
// ErrOffsetAlreadyGrouped is a 409 (a leg was grouped in the meantime),
// ErrOffsetNotBalanced is a 400 (the request's rows don't net to
// zero). Everything else is a 500 with the details only in the
// server log.
func (h *handler) offsetError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, repo.ErrOffsetSuggestionNotFound),
		errors.Is(err, repo.ErrOffsetGroupNotFound),
		errors.Is(err, repo.ErrTransactionNotFound):
		writeAPIError(w, http.StatusNotFound, codeNotFound, "not found")
	case errors.Is(err, repo.ErrOffsetAlreadyGrouped):
		writeAPIError(w, http.StatusConflict, codeConflict, "a transaction is already in an offset group")
	case errors.Is(err, repo.ErrOffsetNotBalanced):
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "the selected transactions do not net to zero")
	default:
		slog.Error("offset operation failed", "op", op, "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "offset operation failed")
	}
}

// parseOffsetScanParams reads the optional scan window from the query
// string: from/to are YYYY-MM-DD (at least one leg of a pair must fall
// in [from, to]) and window_days is the posting-date proximity (1..60,
// default offsetScanWindowDays).
func parseOffsetScanParams(r *http.Request) (from, to *time.Time, windowDays int, msg string) {
	windowDays = offsetScanWindowDays
	q := r.URL.Query()
	if s := q.Get("from"); s != "" {
		d, err := parseDateParam("from", s)
		if err != nil {
			return nil, nil, 0, err.Error()
		}
		from = &d
	}
	if s := q.Get("to"); s != "" {
		d, err := parseDateParam("to", s)
		if err != nil {
			return nil, nil, 0, err.Error()
		}
		to = &d
	}
	if s := q.Get("window_days"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 60 {
			return nil, nil, 0, "window_days must be an integer between 1 and 60"
		}
		windowDays = n
	}
	return from, to, windowDays, ""
}

// offsetsScan serves POST /api/v1/offsets/scan (no body): runs the
// matcher over the caller's ungrouped transactions and stores the new
// pairs as pending suggestions. The matcher is idempotent — pairs that
// already have a suggestion in any status are skipped — so the
// response is simply how many NEW suggestions were proposed.
func (h *handler) offsetsScan(w http.ResponseWriter, r *http.Request) {
	if h.offsets == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	from, to, windowDays, msg := parseOffsetScanParams(r)
	if msg != "" {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, msg)
		return
	}
	cands, err := h.offsets.FindOffsetCandidates(r.Context(), c.row.ID, c.dek, from, to, windowDays)
	if err != nil {
		h.offsetError(w, "scan", err)
		return
	}
	n, err := h.offsets.InsertOffsetSuggestions(r.Context(), c.row.ID, cands)
	if err != nil {
		h.offsetError(w, "insert suggestions", err)
		return
	}
	writeJSON(w, http.StatusOK, offsetScanOut{Suggested: n})
}

// offsetsSuggestionList serves GET /api/v1/offsets/suggestions: the
// caller's pending suggestions, oldest first, both legs opened.
func (h *handler) offsetsSuggestionList(w http.ResponseWriter, r *http.Request) {
	if h.offsets == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	sugs, err := h.offsets.ListOffsetSuggestions(r.Context(), c.row.ID, c.dek)
	if err != nil {
		h.offsetError(w, "list suggestions", err)
		return
	}
	out := make([]offsetSuggestionOut, 0, len(sugs))
	for _, s := range sugs {
		out = append(out, toOffsetSuggestionOut(s))
	}
	writeJSON(w, http.StatusOK, out)
}

// offsetsSuggestionAccept serves POST /api/v1/offsets/suggestions/{id}/
// accept (no body): turns one pending suggestion into a group (both
// legs share a fresh offset_group_id) and answers with the group id.
// A suggestion that was already resolved (or is not the caller's)
// answers 404.
func (h *handler) offsetsSuggestionAccept(w http.ResponseWriter, r *http.Request) {
	if h.offsets == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if !uuidShapeOK(id) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "not found")
		return
	}
	groupID, err := h.offsets.AcceptOffsetSuggestion(r.Context(), c.row.ID, id, c.dek)
	if err != nil {
		h.offsetError(w, "accept suggestion", err)
		return
	}
	writeJSON(w, http.StatusOK, offsetGroupIDOut{GroupID: groupID})
}

// offsetsSuggestionDismiss serves POST
// /api/v1/offsets/suggestions/{id}/dismiss (no body): drops the
// suggestion out of future scans. Dismissals stick until the pair is
// re-opened by clearing a group. Answers 204.
func (h *handler) offsetsSuggestionDismiss(w http.ResponseWriter, r *http.Request) {
	if h.offsets == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if !uuidShapeOK(id) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "not found")
		return
	}
	if err := h.offsets.DismissOffsetSuggestion(r.Context(), c.row.ID, id); err != nil {
		h.offsetError(w, "dismiss suggestion", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// offsetsGroupCreate serves POST /api/v1/offsets/groups: hand-groups a
// set of the caller's rows whose amounts must net to exactly zero.
// The body needs at least two distinct transaction ids (400 when not);
// an id that is not the caller's, or a row already grouped, answers
// 404/409 via offsetError. Answers with the new group id.
func (h *handler) offsetsGroupCreate(w http.ResponseWriter, r *http.Request) {
	if h.offsets == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	var b offsetGroupBody
	if err := decodeBody(r, &b); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "body must be a JSON object: "+err.Error())
		return
	}
	seen := make(map[string]bool, len(b.TransactionIDs))
	for _, id := range b.TransactionIDs {
		if !uuidShapeOK(id) {
			writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "transaction_ids must be UUIDs")
			return
		}
		if seen[id] {
			writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "transaction_ids must be distinct")
			return
		}
		seen[id] = true
	}
	if len(b.TransactionIDs) < 2 {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "at least two transactions are required")
		return
	}
	groupID, err := h.offsets.SetOffsetGroup(r.Context(), c.row.ID, b.TransactionIDs, c.dek)
	if err != nil {
		h.offsetError(w, "create group", err)
		return
	}
	writeJSON(w, http.StatusOK, offsetGroupIDOut{GroupID: groupID})
}

// offsetsGroupList serves GET /api/v1/offsets/groups: the caller's
// active groups, members in (date_posted, id) order within a group.
func (h *handler) offsetsGroupList(w http.ResponseWriter, r *http.Request) {
	if h.offsets == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	groups, err := h.offsets.ListOffsetGroups(r.Context(), c.row.ID, c.dek)
	if err != nil {
		h.offsetError(w, "list groups", err)
		return
	}
	out := make([]offsetGroupOut, 0, len(groups))
	for _, g := range groups {
		out = append(out, toOffsetGroupOut(g))
	}
	writeJSON(w, http.StatusOK, out)
}

// offsetsGroupClear serves DELETE /api/v1/offsets/groups/{id}:
// dissolves one of the caller's groups — the members go back to
// visible in the lists and aggregates, and the suggestions touching
// them are deleted so the matcher can re-propose the pairs. Answers
// 204.
func (h *handler) offsetsGroupClear(w http.ResponseWriter, r *http.Request) {
	if h.offsets == nil {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if !uuidShapeOK(id) {
		writeAPIError(w, http.StatusNotFound, codeNotFound, "not found")
		return
	}
	if err := h.offsets.ClearOffsetGroup(r.Context(), c.row.ID, id); err != nil {
		h.offsetError(w, "clear group", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

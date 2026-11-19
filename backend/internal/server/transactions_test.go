// Transaction route tests (plan step 3.2): CRUD round trips over an
// in-memory TransactionStore that mirrors the repo semantics the
// handlers rely on (per-user scoping, reference ownership, amount
// canonicalization, the 0002 dedup hash, the List filter set, the
// period aggregation), plus the validation, error-mapping, auth, and
// 503 paths. Real DB failures are covered by the repo tests.
package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"finance-tracker/internal/repo"

	"github.com/lestrrat-go/jwx/v3/jwk"
)

// dateOf parses a "2006-01-02" fixture string to UTC (the server
// package's own copy; repo.dateOf is unexported).
func dateOf(s string) time.Time {
	tm, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return tm
}

// memTxRow is one in-memory transaction row scoped to its owner.
type memTxRow struct {
	userID string
	tx     repo.Transaction
}

// memTxStore is the in-memory TransactionStore the route tests run
// against. It mirrors the repo semantics the handlers rely on:
// per-user scoping, ownership pre-checks on the account and category
// references, amount canonicalization, the executed-not-after-posted
// date rule, the 0002 dedup hash (a Create colliding with an existing
// identity is ErrDedupConflict; an Update recomputes it), the List
// filter set (repeatable account ids, repeatable category ids — each a
// UUID or ""=uncategorized only, OR-combined — inclusive date bounds,
// min/max amounts, the 1000-row search candidate window, the
// date_posted DESC / id DESC ordering), and the
// per-user period aggregation (same account filter as List; every call
// is recorded in aggregateCalls / trendCalls so tests can assert the
// handler's window math). The per-op error fields exercise the
// handler's error mapping (real failures need a DB).
type memTxStore struct {
	rows           []memTxRow
	accounts       map[string]map[string]bool // userID -> owned account ids
	categories     map[string]map[string]bool // userID -> owned category ids
	listErr        error
	createErr      error
	updateErr      error
	deleteErr      error
	aggregateErr   error
	aggregateCalls []aggregateCall
	trendCalls     []trendCall
}

// aggregateCall is one recorded Aggregate invocation (bounds UTC-normalized
// as passed; accountIDs copied so later mutation cannot rewrite history).
type aggregateCall struct {
	from       time.Time
	to         time.Time
	accountIDs []string
}

// trendCall is one recorded TrendAggregate invocation (bounds
// UTC-normalized as passed; accountIDs copied so later mutation cannot
// rewrite history).
type trendCall struct {
	from       time.Time
	to         time.Time
	accountIDs []string
}

func (m *memTxStore) ownAccount(userID, id string) bool {
	set, ok := m.accounts[userID]
	return ok && set[id]
}

func (m *memTxStore) ownCategory(userID, id string) bool {
	set, ok := m.categories[userID]
	return ok && set[id]
}

// markAccount / markCategory record ownership (the write side of
// ownAccount/ownCategory), initializing the outer maps lazily so the
// zero-value store is safe to seed.
func (m *memTxStore) markAccount(userID, id string) {
	if m.accounts == nil {
		m.accounts = map[string]map[string]bool{}
	}
	m.accounts[userID] = setOne(m.accounts[userID], id)
}

func (m *memTxStore) markCategory(userID, id string) {
	if m.categories == nil {
		m.categories = map[string]map[string]bool{}
	}
	m.categories[userID] = setOne(m.categories[userID], id)
}

func (m *memTxStore) find(userID, id string) int {
	for i, r := range m.rows {
		if r.userID == userID && r.tx.ID == id {
			return i
		}
	}
	return -1
}

// memDedupHash mirrors the repo's 0002 formula: sha256 over
// account_id ‖ NUL ‖ date_posted ‖ NUL ‖ canonical amount ‖ NUL ‖
// normalized description (trimmed, case-folded, whitespace collapsed).
// normalizeDescription is unexported in the repo package.
func memDedupHash(accountID string, datePosted time.Time, amount, description string) string {
	h := sha256.New()
	h.Write([]byte(accountID))
	h.Write([]byte{0})
	h.Write([]byte(datePosted.UTC().Format("2006-01-02")))
	h.Write([]byte{0})
	h.Write([]byte(amount))
	h.Write([]byte{0})
	h.Write([]byte(strings.ToLower(strings.Join(strings.Fields(description), " "))))
	return hex.EncodeToString(h.Sum(nil))
}

func (m *memTxStore) Create(_ context.Context, userID string, in repo.TransactionIn, _ []byte) (repo.Transaction, error) {
	if m.createErr != nil {
		return repo.Transaction{}, m.createErr
	}
	if !m.ownAccount(userID, in.AccountID) {
		return repo.Transaction{}, repo.ErrAccountNotFound
	}
	if in.CategoryID != nil && *in.CategoryID != "" && !m.ownCategory(userID, *in.CategoryID) {
		return repo.Transaction{}, repo.ErrCategoryNotFound
	}
	amount, err := repo.CanonicalAmount(in.Amount)
	if err != nil {
		return repo.Transaction{}, err
	}
	if in.DateExecuted != nil && dateOnly(*in.DateExecuted) > dateOnly(in.DatePosted) {
		return repo.Transaction{}, repo.ErrDateRule
	}
	hash := memDedupHash(in.AccountID, in.DatePosted, amount, in.Description)
	for _, r := range m.rows {
		if r.userID == userID && r.tx.DedupHash == hash {
			return repo.Transaction{}, repo.ErrDedupConflict
		}
	}
	cat, notes, raw := "", "", ""
	if in.CategoryID != nil {
		cat = *in.CategoryID
	}
	if in.Notes != nil {
		notes = *in.Notes
	}
	if in.Raw != nil {
		raw = *in.Raw
	}
	now := time.Now().UTC()
	tx := repo.Transaction{
		ID:            nextTestID(),
		AccountID:     in.AccountID,
		DateExecuted:  in.DateExecuted,
		DatePosted:    in.DatePosted,
		Amount:        amount,
		CategoryID:    cat,
		Description:   in.Description,
		Notes:         notes,
		Raw:           raw,
		DedupHash:     hash,
		ReviewPending: in.ReviewPending,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	m.rows = append(m.rows, memTxRow{userID: userID, tx: tx})
	return tx, nil
}

func (m *memTxStore) Update(_ context.Context, userID, id string, p repo.TransactionPatch, _ []byte) (repo.Transaction, error) {
	if m.updateErr != nil {
		return repo.Transaction{}, m.updateErr
	}
	i := m.find(userID, id)
	if i < 0 {
		return repo.Transaction{}, repo.ErrTransactionNotFound
	}
	tx := m.rows[i].tx
	if p.Description != nil {
		tx.Description = *p.Description
	}
	if p.Amount != nil {
		amount, err := repo.CanonicalAmount(*p.Amount)
		if err != nil {
			return repo.Transaction{}, err
		}
		tx.Amount = amount
	}
	if p.DatePosted != nil {
		tx.DatePosted = *p.DatePosted
	}
	if p.CategoryID != nil {
		if *p.CategoryID == nil {
			tx.CategoryID = ""
		} else {
			if **p.CategoryID != "" && !m.ownCategory(userID, **p.CategoryID) {
				return repo.Transaction{}, repo.ErrCategoryNotFound
			}
			tx.CategoryID = **p.CategoryID
		}
	}
	if p.Notes != nil {
		if *p.Notes == nil {
			tx.Notes = ""
		} else {
			tx.Notes = **p.Notes
		}
	}
	if p.Raw != nil {
		if *p.Raw == nil {
			tx.Raw = ""
		} else {
			tx.Raw = **p.Raw
		}
	}
	if p.DateExecuted != nil {
		tx.DateExecuted = *p.DateExecuted
	}
	if p.ReviewPending != nil {
		tx.ReviewPending = *p.ReviewPending
	}
	// The repo re-checks the date rule against the stored row.
	if tx.DateExecuted != nil && dateOnly(*tx.DateExecuted) > dateOnly(tx.DatePosted) {
		return repo.Transaction{}, repo.ErrDateRule
	}
	// The dedup hash is recomputed from the new identity; a collision
	// with a sibling row is ErrDedupConflict.
	hash := memDedupHash(tx.AccountID, tx.DatePosted, tx.Amount, tx.Description)
	for j, other := range m.rows {
		if j != i && other.userID == userID && other.tx.DedupHash == hash {
			return repo.Transaction{}, repo.ErrDedupConflict
		}
	}
	tx.DedupHash = hash
	tx.UpdatedAt = time.Now().UTC()
	m.rows[i] = memTxRow{userID: userID, tx: tx}
	return tx, nil
}

func (m *memTxStore) Delete(_ context.Context, userID, id string) error {
	if m.deleteErr != nil {
		return m.deleteErr
	}
	i := m.find(userID, id)
	if i < 0 {
		return repo.ErrTransactionNotFound
	}
	m.rows = append(m.rows[:i], m.rows[i+1:]...)
	return nil
}

// AccountDebits mirrors the repo's narrow debit query: the account's
// ungrouped debit rows, in (date_posted, id) order. Like the repo it
// does NOT filter by user — the caller owns the account (the handlers
// only ever pass the acting user's linked account). listErr is honored
// so the handler's error mapping is testable.
func (m *memTxStore) AccountDebits(_ context.Context, accountID string) ([]repo.DebitTxn, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	// DebitTxn carries no id, but the (date_posted, id) tie-break needs
	// it, so the sort runs over (id, DebitTxn) pairs.
	type debitHit struct {
		id string
		d  repo.DebitTxn
	}
	var hits []debitHit
	for _, r := range m.rows {
		if r.tx.AccountID != accountID || r.tx.OffsetGroupID != nil {
			continue
		}
		c, err := parseCents(r.tx.Amount)
		if err != nil {
			return nil, err
		}
		if c >= 0 {
			continue
		}
		hits = append(hits, debitHit{id: r.tx.ID, d: repo.DebitTxn{Date: r.tx.DatePosted, Amount: r.tx.Amount}})
	}
	sort.Slice(hits, func(i, j int) bool {
		di, dj := dateOnly(hits[i].d.Date), dateOnly(hits[j].d.Date)
		if di != dj {
			return di < dj
		}
		return hits[i].id < hits[j].id
	})
	out := make([]repo.DebitTxn, 0, len(hits))
	for _, hit := range hits {
		out = append(out, hit.d)
	}
	return out, nil
}

func (m *memTxStore) List(_ context.Context, userID string, f repo.TransactionFilter, _ []byte) ([]repo.Transaction, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}
	var minCents, maxCents int64
	hasMin, hasMax := false, false
	if f.MinAmount != nil {
		v, err := parseCents(*f.MinAmount)
		if err != nil {
			return nil, err
		}
		minCents, hasMin = v, true
	}
	if f.MaxAmount != nil {
		v, err := parseCents(*f.MaxAmount)
		if err != nil {
			return nil, err
		}
		maxCents, hasMax = v, true
	}

	var matched []repo.Transaction
	for _, r := range m.rows {
		if r.userID != userID {
			continue
		}
		if len(f.AccountIDs) > 0 {
			in := false
			for _, a := range f.AccountIDs {
				if r.tx.AccountID == a {
					in = true
					break
				}
			}
			if !in {
				continue
			}
		}
		if len(f.CategoryIDs) > 0 {
			in := false
			for _, c := range f.CategoryIDs {
				if (c == "" && r.tx.CategoryID == "") || (c != "" && r.tx.CategoryID == c) {
					in = true
					break
				}
			}
			if !in {
				continue
			}
		}
		d := dateOnly(r.tx.DatePosted)
		if f.DateFrom != nil && d < dateOnly(*f.DateFrom) {
			continue
		}
		if f.DateTo != nil && d > dateOnly(*f.DateTo) {
			continue
		}
		if hasMin || hasMax || f.Sign != "" {
			c, err := parseCents(r.tx.Amount)
			if err != nil {
				return nil, err
			}
			switch f.Sign {
			case "spending":
				if c >= 0 {
					continue
				}
				c = -c // absolute magnitude
			case "income":
				if c <= 0 {
					continue
				}
			}
			if hasMin && c < minCents {
				continue
			}
			if hasMax && c > maxCents {
				continue
			}
		}
		matched = append(matched, r.tx)
	}
	sort.Slice(matched, func(i, j int) bool {
		di, dj := dateOnly(matched[i].DatePosted), dateOnly(matched[j].DatePosted)
		if di != dj {
			return di > dj
		}
		return matched[i].ID > matched[j].ID
	})

	if strings.TrimSpace(f.Search) == "" {
		return pageTx(matched, offset, limit), nil
	}
	// Search: candidate window (most recent 1000 matching the OTHER
	// filters) + case-insensitive substring over the description, then
	// offset/limit over the matches (repo.searchCandidateLimit).
	window := matched
	if len(window) > 1000 {
		window = window[:1000]
	}
	needle := strings.ToLower(strings.TrimSpace(f.Search))
	var hits []repo.Transaction
	for _, tx := range window {
		if strings.Contains(strings.ToLower(tx.Description), needle) {
			hits = append(hits, tx)
		}
	}
	return pageTx(hits, offset, limit), nil
}

// pageTx applies offset/limit to a pre-sorted slice (the repo's
// LIMIT/OFFSET semantics; an offset past the end is an empty page).
func pageTx(rows []repo.Transaction, offset, limit int) []repo.Transaction {
	if offset >= len(rows) {
		return []repo.Transaction{}
	}
	end := offset + limit
	if end > len(rows) {
		end = len(rows)
	}
	return rows[offset:end]
}

func (m *memTxStore) Aggregate(_ context.Context, userID string, from, to time.Time, accountIDs []string) ([]repo.CategorySpend, error) {
	m.aggregateCalls = append(m.aggregateCalls, aggregateCall{
		from:       from,
		to:         to,
		accountIDs: append([]string(nil), accountIDs...),
	})
	if m.aggregateErr != nil {
		return nil, m.aggregateErr
	}
	type acc struct {
		spend, income, net int64
	}
	byCat := make(map[string]*acc)
	for _, r := range m.rows {
		if r.userID != userID {
			continue
		}
		if len(accountIDs) > 0 {
			in := false
			for _, a := range accountIDs {
				if r.tx.AccountID == a {
					in = true
					break
				}
			}
			if !in {
				continue
			}
		}
		d := dateOnly(r.tx.DatePosted)
		if d < dateOnly(from) || d > dateOnly(to) {
			continue
		}
		c, err := parseCents(r.tx.Amount)
		if err != nil {
			return nil, err
		}
		a := byCat[r.tx.CategoryID]
		if a == nil {
			a = &acc{}
			byCat[r.tx.CategoryID] = a
		}
		if c < 0 {
			a.spend -= c
		} else if c > 0 {
			a.income += c
		}
		a.net += c
	}
	out := make([]repo.CategorySpend, 0, len(byCat))
	for id, a := range byCat {
		out = append(out, repo.CategorySpend{
			CategoryID: id,
			Spend:      formatCents(a.spend),
			Income:     formatCents(a.income),
			Net:        formatCents(a.net),
		})
	}
	// Same order as the SQL: category id asc, "" (uncategorized) first.
	sort.Slice(out, func(i, j int) bool { return out[i].CategoryID < out[j].CategoryID })
	return out, nil
}

// TrendAggregate is the fake's single-query trend scan: the same
// in-memory aggregation as Aggregate, bucketed by each row's own
// period window (periodBounds of its posting date). Bucket starts are
// the UTC-midnight window starts, asc; within a bucket the groups are
// category id asc, "" (uncategorized) first — the same order as the
// SQL.
func (m *memTxStore) TrendAggregate(_ context.Context, userID, period string, from, to time.Time, accountIDs []string) ([]repo.TrendBucket, error) {
	m.trendCalls = append(m.trendCalls, trendCall{
		from:       from,
		to:         to,
		accountIDs: append([]string(nil), accountIDs...),
	})
	if m.aggregateErr != nil {
		return nil, m.aggregateErr
	}
	type acc struct {
		spend, income, net int64
	}
	byWindow := make(map[string]map[string]*acc)
	var windows []string
	for _, r := range m.rows {
		if r.userID != userID {
			continue
		}
		if len(accountIDs) > 0 {
			in := false
			for _, a := range accountIDs {
				if r.tx.AccountID == a {
					in = true
					break
				}
			}
			if !in {
				continue
			}
		}
		d := dateOnly(r.tx.DatePosted)
		if d < dateOnly(from) || d > dateOnly(to) {
			continue
		}
		c, err := parseCents(r.tx.Amount)
		if err != nil {
			return nil, err
		}
		wf, _ := periodBounds(period, r.tx.DatePosted)
		key := wf.Format("2006-01-02")
		if byWindow[key] == nil {
			byWindow[key] = make(map[string]*acc)
			windows = append(windows, key)
		}
		a := byWindow[key][r.tx.CategoryID]
		if a == nil {
			a = &acc{}
			byWindow[key][r.tx.CategoryID] = a
		}
		if c < 0 {
			a.spend -= c
		} else if c > 0 {
			a.income += c
		}
		a.net += c
	}
	sort.Strings(windows)
	out := make([]repo.TrendBucket, 0, len(windows))
	for _, key := range windows {
		byCat := byWindow[key]
		ids := make([]string, 0, len(byCat))
		for id := range byCat {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		groups := make([]repo.CategorySpend, 0, len(ids))
		for _, id := range ids {
			a := byCat[id]
			groups = append(groups, repo.CategorySpend{
				CategoryID: id,
				Spend:      formatCents(a.spend),
				Income:     formatCents(a.income),
				Net:        formatCents(a.net),
			})
		}
		bucket, err := time.Parse("2006-01-02", key)
		if err != nil {
			panic(err)
		}
		out = append(out, repo.TrendBucket{Bucket: bucket, Groups: groups})
	}
	return out, nil
}

// SavingsSums is the savings-plan rollup; the transaction and summary
// routes never call it (the savings routes do, in a later step).
func (m *memTxStore) SavingsSums(_ context.Context, _ string) ([]repo.SavingsSum, error) {
	return nil, nil
}

// newTxServer builds a full router with a live token verifier, an
// in-memory bootstrap, and the given stores (nil allowed — that is the
// 503 path). The key is returned so tests can mint tokens for several
// users under the same verifier.
func newTxServer(t *testing.T, tx TransactionStore, cats CategoryStore, budgets BudgetStore) (http.Handler, jwk.Key) {
	t.Helper()
	key := mintKey(t)
	b, err := NewBootstrap(newMemStore(), testKEK(t), nil)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return New(fakeReady{}, BuildInfo{}, liveAuth(t, key), b, nil, nil, cats, tx, budgets, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, false, nil, nil, nil, nil, nil, nil, nil), key
}

// setOne adds id to a user's ownership set, creating the set if needed.
func setOne(m map[string]bool, id string) map[string]bool {
	if m == nil {
		m = map[string]bool{}
	}
	m[id] = true
	return m
}

// seedTx creates one row directly in the store (bypassing the HTTP
// create path so tests can pin dates and attributes exactly).
// categoryID and executed of "" mean uncategorized / unset.
func seedTx(t *testing.T, m *memTxStore, userID, accountID, categoryID, posted, amount, description, executed string) repo.Transaction {
	t.Helper()
	m.markAccount(userID, accountID)
	if categoryID != "" {
		m.markCategory(userID, categoryID)
	}
	in := repo.TransactionIn{
		AccountID:   accountID,
		DatePosted:  dateOf(posted),
		Amount:      amount,
		Description: description,
	}
	if categoryID != "" {
		in.CategoryID = &categoryID
	}
	if executed != "" {
		d := dateOf(executed)
		in.DateExecuted = &d
	}
	tx, err := m.Create(context.Background(), userID, in, nil)
	if err != nil {
		t.Fatalf("seed %s %s: %v", posted, amount, err)
	}
	return tx
}

// txList decodes a GET /api/v1/transactions body.
func txList(t *testing.T, body string) []transactionOut {
	t.Helper()
	var out []transactionOut
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode list: %v (body %s)", err, body)
	}
	return out
}

// TestTransactionsCreate covers POST /api/v1/transactions: the 201
// round trip (canonical amounts, date strings, nulls for unset fields),
// the dedup key (409, incl. normalized descriptions), the date rule,
// the 404 reference checks, and the 400 validation table.
func TestTransactionsCreate(t *testing.T) {
	store := &memTxStore{}
	h, key := newTxServer(t, store, nil, nil)
	tok := mintIdentityToken(t, key, "tx-a", "a@localhost", "A", "a")
	user := "id-tx-a"
	acc := nextTestID()
	cat := nextTestID()
	store.markAccount(user, acc)
	store.markCategory(user, cat)
	hdr := authHdr(tok)
	post := func(body string) (int, string) {
		return doReq(t, h, http.MethodPost, "/api/v1/transactions", body, hdr)
	}

	t.Run("full row", func(t *testing.T) {
		body := `{"account_id":"` + acc + `","date_executed":"2026-10-02","date_posted":"2026-10-03","amount":"-50.25","category_id":"` + cat + `","description":"groceries","notes":"receipt kept","raw":"RAW LINE","review_pending":true}`
		code, out := post(body)
		if code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (body %s)", code, out)
		}
		var row transactionOut
		if err := json.Unmarshal([]byte(out), &row); err != nil {
			t.Fatalf("decode: %v (body %s)", err, out)
		}
		if row.ID == "" || len(row.ID) != 36 {
			t.Errorf("id = %q, want a uuid", row.ID)
		}
		if row.AccountID != acc || row.DateExecuted == nil || *row.DateExecuted != "2026-10-02" ||
			row.DatePosted != "2026-10-03" || row.Amount != "-50.25" ||
			row.CategoryID == nil || *row.CategoryID != cat || row.Description != "groceries" ||
			row.Notes == nil || *row.Notes != "receipt kept" || row.Raw == nil || *row.Raw != "RAW LINE" ||
			!row.ReviewPending {
			t.Errorf("row mismatch: %+v", row)
		}
	})

	t.Run("amounts are canonical", func(t *testing.T) {
		for in, want := range map[string]string{"10": "10.00", "0": "0.00", "-5.5": "-5.50", "007.5": "7.50"} {
			body := `{"account_id":"` + acc + `","date_posted":"2026-10-01","amount":"` + in + `","description":"amt ` + in + `"}`
			code, out := post(body)
			if code != http.StatusCreated {
				t.Fatalf("%s: status = %d (body %s)", in, code, out)
			}
			var row transactionOut
			if err := json.Unmarshal([]byte(out), &row); err != nil {
				t.Fatalf("%s: decode: %v", in, err)
			}
			if row.Amount != want {
				t.Errorf("%s: amount = %q, want %q", in, row.Amount, want)
			}
		}
	})

	t.Run("nulls stay null", func(t *testing.T) {
		body := `{"account_id":"` + acc + `","date_executed":null,"date_posted":"2026-10-05","amount":"1","category_id":null,"description":"bare","notes":null,"raw":null}`
		code, out := post(body)
		if code != http.StatusCreated {
			t.Fatalf("status = %d (body %s)", code, out)
		}
		var row transactionOut
		if err := json.Unmarshal([]byte(out), &row); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if row.DateExecuted != nil || row.CategoryID != nil || row.Notes != nil || row.Raw != nil {
			t.Errorf("unset fields must be null: %+v", row)
		}
	})

	t.Run("dedup conflict", func(t *testing.T) {
		base := `{"account_id":"` + acc + `","date_posted":"2026-10-20","amount":"-9.99","description":"dup"}`
		if code, out := post(base); code != http.StatusCreated {
			t.Fatalf("first: status = %d (body %s)", code, out)
		}
		// Same identity, different notes: the dedup key still collides.
		again := `{"account_id":"` + acc + `","date_posted":"2026-10-20","amount":"-9.99","description":"dup","notes":"other"}`
		code, out := post(again)
		if code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 (body %s)", code, out)
		}
		c, m := apiErr(t, out)
		if c != codeConflict || m != "a transaction with this dedup key already exists" {
			t.Errorf("error = %s/%q", c, m)
		}
		// Case and spacing variants of the description collide too.
		variant := `{"account_id":"` + acc + `","date_posted":"2026-10-20","amount":"-9.99","description":"  DUP  "}`
		if code, _ := post(variant); code != http.StatusConflict {
			t.Errorf("normalized description: status = %d, want 409", code)
		}
	})

	t.Run("date rule", func(t *testing.T) {
		body := `{"account_id":"` + acc + `","date_executed":"2026-10-04","date_posted":"2026-10-03","amount":"1","description":"late"}`
		code, out := post(body)
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeInvalidRequest || m != "date_executed must not be after date_posted" {
			t.Errorf("error = %s/%q", c, m)
		}
	})

	t.Run("foreign references are 404", func(t *testing.T) {
		other := nextTestID()
		body := `{"account_id":"` + other + `","date_posted":"2026-10-03","amount":"1","description":"x"}`
		code, out := post(body)
		if code != http.StatusNotFound {
			t.Fatalf("account: status = %d, want 404 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeNotFound || m != "account not found" {
			t.Errorf("account error = %s/%q", c, m)
		}
		body = `{"account_id":"` + acc + `","date_posted":"2026-10-03","amount":"1","category_id":"` + other + `","description":"x"}`
		code, out = post(body)
		if code != http.StatusNotFound {
			t.Fatalf("category: status = %d, want 404 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeNotFound || m != "category not found" {
			t.Errorf("category error = %s/%q", c, m)
		}
	})

	t.Run("validation 400s", func(t *testing.T) {
		cases := []struct {
			name string
			body string
			want string
		}{
			{"bad account id", `{"account_id":"nope","date_posted":"2026-10-03","amount":"1","description":"x"}`, "account_id must be a valid UUID"},
			{"bad posted", `{"account_id":"` + acc + `","date_posted":"2026-1-2","amount":"1","description":"x"}`, "date_posted must be a YYYY-MM-DD date"},
			{"bad executed", `{"account_id":"` + acc + `","date_executed":"20261002","date_posted":"2026-10-03","amount":"1","description":"x"}`, "date_executed must be a YYYY-MM-DD date"},
			{"impossible date", `{"account_id":"` + acc + `","date_posted":"2026-02-30","amount":"1","description":"x"}`, "date_posted must be a YYYY-MM-DD date"},
			{"bad category id", `{"account_id":"` + acc + `","date_posted":"2026-10-03","amount":"1","category_id":"nope","description":"x"}`, "category_id must be a valid UUID"},
			{"empty amount", `{"account_id":"` + acc + `","date_posted":"2026-10-03","amount":"","description":"x"}`, "amount must not be empty"},
			{"bad amount", `{"account_id":"` + acc + `","date_posted":"2026-10-03","amount":"abc","description":"x"}`, `amount "abc" is not a valid decimal`},
			{"long amount", `{"account_id":"` + acc + `","date_posted":"2026-10-03","amount":"1.234","description":"x"}`, `amount "1.234" has more than 2 decimal places`},
			{"multi problem", `{"account_id":"nope","date_posted":"bad","amount":"1.234","description":"x"}`, "account_id must be a valid UUID; date_posted must be a YYYY-MM-DD date; amount \"1.234\" has more than 2 decimal places"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				code, out := post(tc.body)
				if code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400 (body %s)", code, out)
				}
				if c, m := apiErr(t, out); c != codeInvalidRequest || m != tc.want {
					t.Errorf("error = %s/%q, want %s/%q", c, m, codeInvalidRequest, tc.want)
				}
			})
		}
	})

	t.Run("non-object body", func(t *testing.T) {
		code, out := post(`[1,2]`)
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeInvalidRequest || !strings.HasPrefix(m, "body must be a JSON object: ") {
			t.Errorf("error = %s/%q", c, m)
		}
	})
}

// TestTransactionsPatch covers PATCH /api/v1/transactions/{id}: the
// tri-state semantics (absent = untouched, null = clear, value = set)
// for every clearable field, the re-canonicalized amount, the date
// rule against the stored row, the dedup recompute, and the 404/400
// paths.
func TestTransactionsPatch(t *testing.T) {
	store := &memTxStore{}
	h, key := newTxServer(t, store, nil, nil)
	tok := mintIdentityToken(t, key, "tx-a", "a@localhost", "A", "a")
	user := "id-tx-a"
	acc := nextTestID()
	cat := nextTestID()
	otherCat := nextTestID()
	store.markAccount(user, acc)
	store.markCategory(user, cat)
	store.markCategory(user, otherCat)
	hdr := authHdr(tok)

	seed := seedTx(t, store, user, acc, cat, "2026-10-03", "-12.34", "orig", "2026-10-02")
	patch := func(id, body string) (int, string) {
		return doReq(t, h, http.MethodPatch, "/api/v1/transactions/"+id, body, hdr)
	}

	t.Run("clear and set the tri-states", func(t *testing.T) {
		// notes/raw are unset on the seed row: set, then clear.
		code, out := patch(seed.ID, `{"notes":"n1","raw":"r1"}`)
		if code != http.StatusOK {
			t.Fatalf("status = %d (body %s)", code, out)
		}
		var row transactionOut
		if err := json.Unmarshal([]byte(out), &row); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if row.Notes == nil || *row.Notes != "n1" || row.Raw == nil || *row.Raw != "r1" {
			t.Errorf("set: %+v", row)
		}
		// category_id: null clears, a value sets; date_executed null
		// clears the seeded date.
		code, out = patch(seed.ID, `{"category_id":null,"date_executed":null,"notes":null,"raw":null}`)
		if code != http.StatusOK {
			t.Fatalf("status = %d (body %s)", code, out)
		}
		if err := json.Unmarshal([]byte(out), &row); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if row.CategoryID != nil || row.DateExecuted != nil || row.Notes != nil || row.Raw != nil {
			t.Errorf("cleared fields must be null: %+v", row)
		}
		code, out = patch(seed.ID, `{"category_id":"`+otherCat+`","date_executed":"2026-10-01"}`)
		if code != http.StatusOK {
			t.Fatalf("status = %d (body %s)", code, out)
		}
		if err := json.Unmarshal([]byte(out), &row); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if row.CategoryID == nil || *row.CategoryID != otherCat || row.DateExecuted == nil || *row.DateExecuted != "2026-10-01" {
			t.Errorf("re-set: %+v", row)
		}
	})

	t.Run("plain fields", func(t *testing.T) {
		code, out := patch(seed.ID, `{"description":"new","amount":"-5","review_pending":false,"date_posted":"2026-10-09"}`)
		if code != http.StatusOK {
			t.Fatalf("status = %d (body %s)", code, out)
		}
		var row transactionOut
		if err := json.Unmarshal([]byte(out), &row); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if row.Description != "new" || row.Amount != "-5.00" || row.ReviewPending || row.DatePosted != "2026-10-09" {
			t.Errorf("row: %+v", row)
		}
	})

	t.Run("date rule against stored row", func(t *testing.T) {
		// date_posted is now 2026-10-09; an executed date past it 400s
		// even though both are well-shaped.
		code, out := patch(seed.ID, `{"date_executed":"2026-10-10"}`)
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); m != "date_executed must not be after date_posted" {
			t.Errorf("error = %s/%q", c, m)
		}
	})

	t.Run("bad tri-state shapes", func(t *testing.T) {
		code, out := patch(seed.ID, `{"date_executed":5}`)
		if code != http.StatusBadRequest {
			t.Fatalf("number: status = %d, want 400 (body %s)", code, out)
		}
		// The tri-state message carries no field-name prefix (same
		// convention as triStateString's "must be a string or null").
		if c, m := apiErr(t, out); c != codeInvalidRequest || !strings.Contains(m, "must be a date string or null") {
			t.Errorf("number error = %s/%q", c, m)
		}
		code, out = patch(seed.ID, `{"date_executed":"2026-2-30"}`)
		if code != http.StatusBadRequest {
			t.Fatalf("bad date: status = %d, want 400 (body %s)", code, out)
		}
	})

	t.Run("dedup recompute", func(t *testing.T) {
		// A sibling with the identity the seed row would take.
		sib := seedTx(t, store, user, acc, "", "2026-10-09", "-5.00", "moved", "")
		_ = sib
		code, out := patch(seed.ID, `{"description":"moved"}`)
		if code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeConflict || m != "a transaction with this dedup key already exists" {
			t.Errorf("error = %s/%q", c, m)
		}
	})

	t.Run("empty patch", func(t *testing.T) {
		code, out := patch(seed.ID, `{}`)
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeInvalidRequest || m != "empty patch: provide at least one field" {
			t.Errorf("error = %s/%q", c, m)
		}
	})

	t.Run("unknown id is 404", func(t *testing.T) {
		code, out := patch(nextTestID(), `{"description":"x"}`)
		if code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeNotFound || m != "transaction not found" {
			t.Errorf("error = %s/%q", c, m)
		}
	})

	t.Run("non-object body", func(t *testing.T) {
		code, out := patch(seed.ID, `"str"`)
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); !strings.HasPrefix(m, "body must be a JSON object: ") {
			t.Errorf("error = %s/%q", c, m)
		}
	})
}

// TestTransactionsDelete covers DELETE /api/v1/transactions/{id}: 204
// bare on success, 404 on a repeat.
func TestTransactionsDelete(t *testing.T) {
	store := &memTxStore{}
	h, key := newTxServer(t, store, nil, nil)
	tok := mintIdentityToken(t, key, "tx-a", "a@localhost", "A", "a")
	user := "id-tx-a"
	acc := nextTestID()
	seed := seedTx(t, store, user, acc, "", "2026-10-03", "1", "gone", "")
	hdr := authHdr(tok)

	code, out := doReq(t, h, http.MethodDelete, "/api/v1/transactions/"+seed.ID, "", hdr)
	if code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body %q)", code, out)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("204 must be bare, got %q", out)
	}
	if len(store.rows) != 0 {
		t.Errorf("row not deleted: %+v", store.rows)
	}
	code, out = doReq(t, h, http.MethodDelete, "/api/v1/transactions/"+seed.ID, "", hdr)
	if code != http.StatusNotFound {
		t.Fatalf("repeat: status = %d, want 404 (body %s)", code, out)
	}
}

// TestTransactionsList covers GET /api/v1/transactions: the default
// ordering (date_posted DESC, id DESC), every filter (repeatable
// account_id, repeatable category_id — each a UUID or ""=uncategorized
// only, OR-combined — inclusive date bounds, min/max amounts,
// case-insensitive description search), limit/offset paging, the 400
// filter table, and per-user scoping.
func TestTransactionsList(t *testing.T) {
	store := &memTxStore{}
	h, key := newTxServer(t, store, nil, nil)
	tok := mintIdentityToken(t, key, "tx-a", "a@localhost", "A", "a")
	user := "id-tx-a"
	a1, a2 := nextTestID(), nextTestID()
	c1, c2 := nextTestID(), nextTestID()
	hdr := authHdr(tok)
	get := func(qs string) (int, string) {
		return doReq(t, h, http.MethodGet, "/api/v1/transactions"+qs, "", hdr)
	}
	ids := func(body string) []string {
		out := txList(t, body)
		ids := make([]string, len(out))
		for i, r := range out {
			ids[i] = r.ID
		}
		return ids
	}

	// Seed order defines the id order (nextTestID is a counter): t4 <
	// t1 < t2 < t3 < tb. On 2026-10-05, t3 (newer id) sorts first.
	t4 := seedTx(t, store, user, a2, c1, "2026-10-01", "-2.5", "groceries top-up", "")
	t1 := seedTx(t, store, user, a1, c1, "2026-10-03", "-50.25", "Groceries run", "2026-10-02")
	t2 := seedTx(t, store, user, a1, c2, "2026-10-05", "10", "Paycheck", "")
	t3 := seedTx(t, store, user, a2, "", "2026-10-05", "-3.14", "coffee", "")
	tb := seedTx(t, store, "id-tx-b", nextTestID(), "", "2026-10-06", "1", "b's row", "")

	t.Run("no filter: date_posted desc, id desc", func(t *testing.T) {
		code, out := get("")
		if code != http.StatusOK {
			t.Fatalf("status = %d (body %s)", code, out)
		}
		got := ids(out)
		want := []string{t3.ID, t2.ID, t1.ID, t4.ID}
		if len(got) != len(want) {
			t.Fatalf("len = %d, want %d (body %s)", len(got), len(want), out)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("order = %v, want %v", got, want)
			}
		}
		rows := txList(t, out)
		first := rows[0]
		if first.Description != "coffee" || first.Amount != "-3.14" || first.CategoryID != nil || first.Notes != nil {
			t.Errorf("first row = %+v", first)
		}
	})

	t.Run("repeatable account_id", func(t *testing.T) {
		if code, out := get("?account_id=" + a1); code != http.StatusOK ||
			ids(out) == nil || !sameIDs(ids(out), t2.ID, t1.ID) {
			t.Errorf("a1: status=%d ids=%v, want [%s %s]", code, ids(out), t2.ID, t1.ID)
		}
		if code, out := get("?account_id=" + a2); code != http.StatusOK ||
			!sameIDs(ids(out), t3.ID, t4.ID) {
			t.Errorf("a2: status=%d ids=%v, want [%s %s]", code, ids(out), t3.ID, t4.ID)
		}
		if code, out := get("?account_id=" + a1 + "&account_id=" + a2); code != http.StatusOK ||
			!sameIDs(ids(out), t3.ID, t2.ID, t1.ID, t4.ID) {
			t.Errorf("a1+a2: status=%d ids=%v", code, ids(out))
		}
	})

	t.Run("category_id: repeatable, OR-combined; empty=uncategorized", func(t *testing.T) {
		if code, out := get("?category_id=" + c1); code != http.StatusOK ||
			!sameIDs(ids(out), t1.ID, t4.ID) {
			t.Errorf("c1: status=%d ids=%v, want [%s %s]", code, ids(out), t1.ID, t4.ID)
		}
		// category_id present-but-empty means "uncategorized only";
		// absent means "no filter".
		if code, out := get("?category_id="); code != http.StatusOK || !sameIDs(ids(out), t3.ID) {
			t.Errorf("empty: status=%d ids=%v, want [%s]", code, ids(out), t3.ID)
		}
		// Repeated values combine with OR: c1 OR c2.
		if code, out := get("?category_id=" + c1 + "&category_id=" + c2); code != http.StatusOK ||
			!sameIDs(ids(out), t2.ID, t1.ID, t4.ID) {
			t.Errorf("c1+c2: status=%d ids=%v, want [%s %s %s]", code, ids(out), t2.ID, t1.ID, t4.ID)
		}
		// The empty value participates in the OR: uncategorized OR c1.
		if code, out := get("?category_id=&category_id=" + c1); code != http.StatusOK ||
			!sameIDs(ids(out), t3.ID, t1.ID, t4.ID) {
			t.Errorf("empty+c1: status=%d ids=%v, want [%s %s %s]", code, ids(out), t3.ID, t1.ID, t4.ID)
		}
	})

	t.Run("date bounds are inclusive", func(t *testing.T) {
		if code, out := get("?date_from=2026-10-03&date_to=2026-10-05"); code != http.StatusOK ||
			!sameIDs(ids(out), t3.ID, t2.ID, t1.ID) {
			t.Errorf("status=%d ids=%v, want [%s %s %s]", code, ids(out), t3.ID, t2.ID, t1.ID)
		}
	})

	t.Run("min and max amounts", func(t *testing.T) {
		if code, out := get("?min_amount=-10"); code != http.StatusOK ||
			!sameIDs(ids(out), t3.ID, t2.ID, t4.ID) {
			t.Errorf("min: status=%d ids=%v", code, ids(out))
		}
		if code, out := get("?max_amount=0"); code != http.StatusOK ||
			!sameIDs(ids(out), t3.ID, t1.ID, t4.ID) {
			t.Errorf("max: status=%d ids=%v", code, ids(out))
		}
		if code, out := get("?min_amount=-5&max_amount=-2"); code != http.StatusOK ||
			!sameIDs(ids(out), t3.ID, t4.ID) {
			t.Errorf("min+max: status=%d ids=%v", code, ids(out))
		}
	})

	t.Run("sign: spending and income", func(t *testing.T) {
		if code, out := get("?sign=spending"); code != http.StatusOK ||
			!sameIDs(ids(out), t3.ID, t1.ID, t4.ID) {
			t.Errorf("spending: status=%d ids=%v", code, ids(out))
		}
		if code, out := get("?sign=income"); code != http.StatusOK ||
			!sameIDs(ids(out), t2.ID) {
			t.Errorf("income: status=%d ids=%v", code, ids(out))
		}
	})

	t.Run("sign composes with absolute min/max bounds", func(t *testing.T) {
		// With sign set the bounds are ABSOLUTE magnitudes: the -50.25
		// spending row has magnitude 50.25.
		if code, out := get("?sign=spending&min_amount=3"); code != http.StatusOK ||
			!sameIDs(ids(out), t3.ID, t1.ID) {
			t.Errorf("spending+min: status=%d ids=%v", code, ids(out))
		}
		if code, out := get("?sign=spending&max_amount=10"); code != http.StatusOK ||
			!sameIDs(ids(out), t3.ID, t4.ID) {
			t.Errorf("spending+max: status=%d ids=%v", code, ids(out))
		}
		if code, out := get("?sign=spending&min_amount=3&max_amount=10"); code != http.StatusOK ||
			!sameIDs(ids(out), t3.ID) {
			t.Errorf("spending+min+max: status=%d ids=%v", code, ids(out))
		}
		if code, out := get("?sign=income&min_amount=5&max_amount=20"); code != http.StatusOK ||
			!sameIDs(ids(out), t2.ID) {
			t.Errorf("income+bounds: status=%d ids=%v", code, ids(out))
		}
	})

	t.Run("search is case-insensitive over description", func(t *testing.T) {
		if code, out := get("?search=GROCERIES"); code != http.StatusOK ||
			!sameIDs(ids(out), t1.ID, t4.ID) {
			t.Errorf("status=%d ids=%v, want [%s %s]", code, ids(out), t1.ID, t4.ID)
		}
		code, out := get("?search=zzz-not-there")
		if code != http.StatusOK {
			t.Fatalf("status = %d (body %s)", code, out)
		}
		if strings.TrimSpace(out) != "[]" {
			t.Errorf("no-match body = %q, want []", out)
		}
	})

	t.Run("limit and offset", func(t *testing.T) {
		if code, out := get("?limit=2"); code != http.StatusOK || !sameIDs(ids(out), t3.ID, t2.ID) {
			t.Errorf("limit: status=%d ids=%v", code, ids(out))
		}
		if code, out := get("?limit=2&offset=2"); code != http.StatusOK || !sameIDs(ids(out), t1.ID, t4.ID) {
			t.Errorf("offset: status=%d ids=%v", code, ids(out))
		}
		code, out := get("?limit=100&offset=99")
		if code != http.StatusOK {
			t.Fatalf("status = %d (body %s)", code, out)
		}
		if strings.TrimSpace(out) != "[]" {
			t.Errorf("offset past end = %q, want []", out)
		}
	})

	t.Run("bad filters are 400", func(t *testing.T) {
		cases := []struct {
			qs   string
			want string
		}{
			{"?account_id=notauuid", "account_id must be a valid UUID"},
			{"?category_id=notauuid", "category_id must be a valid UUID"},
			{"?date_from=2026-1-2", "date_from must be a YYYY-MM-DD date"},
			{"?date_to=2026-13-01", "date_to must be a YYYY-MM-DD date"},
			{"?min_amount=abc", `amount "abc" is not a valid decimal`},
			{"?max_amount=1.234", `amount "1.234" has more than 2 decimal places`},
			{"?sign=both", `sign must be "spending" or "income"`},
			{"?min_amount=-10&sign=spending", "min_amount must be non-negative when sign is set"},
			{"?max_amount=-1&sign=income", "max_amount must be non-negative when sign is set"},
			{"?limit=-1", "limit must be an integer >= 0"},
			{"?offset=xyz", "offset must be an integer >= 0"},
		}
		for _, tc := range cases {
			t.Run(tc.qs, func(t *testing.T) {
				code, out := get(tc.qs)
				if code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400 (body %s)", code, out)
				}
				if c, m := apiErr(t, out); c != codeInvalidRequest || m != tc.want {
					t.Errorf("error = %s/%q, want %s/%q", c, m, codeInvalidRequest, tc.want)
				}
			})
		}
	})

	t.Run("per-user scoping", func(t *testing.T) {
		other := mintIdentityToken(t, key, "tx-b", "b@localhost", "B", "b")
		code, out := doReq(t, h, http.MethodGet, "/api/v1/transactions", "", authHdr(other))
		if code != http.StatusOK {
			t.Fatalf("status = %d (body %s)", code, out)
		}
		if !sameIDs(ids(out), tb.ID) {
			t.Errorf("b's list = %v, want [%s]", ids(out), tb.ID)
		}
	})
}

// sameIDs reports whether got equals want element-wise.
func sameIDs(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestTransactionErrors covers the handler's error mapping: a generic
// store failure is a 500 whose details stay in the server log, the
// not-found sentinel is a 404, and a malformed path id answers 404
// before the store is ever called.
func TestTransactionErrors(t *testing.T) {
	tok := func(store *memTxStore) (http.Handler, map[string]string) {
		h, key := newTxServer(t, store, nil, nil)
		tok := mintIdentityToken(t, key, "tx-a", "a@localhost", "A", "a")
		return h, authHdr(tok)
	}

	t.Run("generic store failures are 500", func(t *testing.T) {
		boom := errors.New("boom")
		ops := []struct {
			name string
			run  func(store *memTxStore) (int, string)
		}{
			{"list", func(store *memTxStore) (int, string) {
				h, hdr := tok(store)
				return doReq(t, h, http.MethodGet, "/api/v1/transactions", "", hdr)
			}},
			{"create", func(store *memTxStore) (int, string) {
				h, hdr := tok(store)
				return doReq(t, h, http.MethodPost, "/api/v1/transactions",
					`{"account_id":"`+nextTestID()+`","date_posted":"2026-10-03","amount":"1","description":"x"}`, hdr)
			}},
			{"update", func(store *memTxStore) (int, string) {
				h, hdr := tok(store)
				return doReq(t, h, http.MethodPatch, "/api/v1/transactions/"+nextTestID(), `{"description":"x"}`, hdr)
			}},
			{"delete", func(store *memTxStore) (int, string) {
				h, hdr := tok(store)
				return doReq(t, h, http.MethodDelete, "/api/v1/transactions/"+nextTestID(), "", hdr)
			}},
		}
		for _, op := range ops {
			t.Run(op.name, func(t *testing.T) {
				store := &memTxStore{}
				switch op.name {
				case "list":
					store.listErr = boom
				case "create":
					store.createErr = boom
				case "update":
					store.updateErr = boom
				case "delete":
					store.deleteErr = boom
				}
				code, out := op.run(store)
				if code != http.StatusInternalServerError {
					t.Fatalf("status = %d, want 500 (body %s)", code, out)
				}
				if c, m := apiErr(t, out); c != codeInternal || m != "transaction operation failed" {
					t.Errorf("error = %s/%q", c, m)
				}
			})
		}
	})

	t.Run("not-found sentinel is 404", func(t *testing.T) {
		store := &memTxStore{}
		h, hdr := tok(store)
		id := nextTestID()
		code, out := doReq(t, h, http.MethodPatch, "/api/v1/transactions/"+id, `{"description":"x"}`, hdr)
		if code != http.StatusNotFound {
			t.Fatalf("update: status = %d, want 404 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeNotFound || m != "transaction not found" {
			t.Errorf("update error = %s/%q", c, m)
		}
		code, out = doReq(t, h, http.MethodDelete, "/api/v1/transactions/"+id, "", hdr)
		if code != http.StatusNotFound {
			t.Fatalf("delete: status = %d, want 404 (body %s)", code, out)
		}
	})

	t.Run("malformed path id never reaches the store", func(t *testing.T) {
		// Injected errors would surface as 500 if the store were
		// called; the shape check must answer 404 first.
		store := &memTxStore{updateErr: errors.New("boom"), deleteErr: errors.New("boom")}
		h, hdr := tok(store)
		for _, method := range []string{http.MethodPatch, http.MethodDelete} {
			code, out := doReq(t, h, method, "/api/v1/transactions/nope", `{"description":"x"}`, hdr)
			if code != http.StatusNotFound {
				t.Fatalf("%s: status = %d, want 404 (body %s)", method, code, out)
			}
			if c, m := apiErr(t, out); c != codeNotFound || m != "transaction not found" {
				t.Errorf("%s error = %s/%q", method, c, m)
			}
		}
	})
}

// TestTransactionAuth covers the auth and 503 edges of the transaction
// routes: missing/garbage tokens are 401 (status only — the body is
// the authn package's business), and a valid token against a nil store
// is a 503.
func TestTransactionAuth(t *testing.T) {
	t.Run("missing and garbage tokens are 401", func(t *testing.T) {
		// testAuth verifies against a nil key set: every token fails.
		h := New(fakeReady{}, BuildInfo{}, testAuth(), nil, nil, nil, nil, &memTxStore{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, false, nil, nil, nil, nil, nil, nil, nil)
		if code, _ := doReq(t, h, http.MethodGet, "/api/v1/transactions", "", nil); code != http.StatusUnauthorized {
			t.Errorf("no token: status = %d, want 401", code)
		}
		if code, _ := doReq(t, h, http.MethodGet, "/api/v1/transactions", "",
			map[string]string{"Authorization": "Bearer not.a.jwt"}); code != http.StatusUnauthorized {
			t.Errorf("garbage token: status = %d, want 401", code)
		}
	})

	t.Run("nil store is 503 with a valid token", func(t *testing.T) {
		h, key := newTxServer(t, nil, nil, nil)
		tok := mintIdentityToken(t, key, "tx-a", "a@localhost", "A", "a")
		code, out := doReq(t, h, http.MethodGet, "/api/v1/transactions", "", authHdr(tok))
		if code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 (body %s)", code, out)
		}
		if c, m := apiErr(t, out); c != codeInternal || m != "database not configured" {
			t.Errorf("error = %s/%q", c, m)
		}
	})
}

package schedule

// T2 self-heal tests: in-memory map-backed fakes in the style of
// schedule_test.go; no clock needed — the heal is timeless. The single
// mortgage fixture is the matchSpecA shape (485k @ 4.39%, 25y
// semi-monthly, no recurring extra) with 20 grid debits, the 4th short
// and the 6th an isolated extra, so the real engine decides what the
// live set must be.

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"testing"

	"finance-tracker/internal/mortgage"
	"finance-tracker/internal/repo"
)

// --- fakes ------------------------------------------------------------

// fakeUserIDLister is a UserIDLister with a fixed id list.
type fakeUserIDLister struct {
	ids []string
	err error
}

func (f *fakeUserIDLister) ListUserIDs(ctx context.Context) ([]string, error) {
	return f.ids, f.err
}

// fakeHealNotifs is a map-backed NotifStore: rows keyed by user then
// id. ListByKind returns ids in sorted order (map order must not leak
// into the assertions) and only rows of the requested kind, like the
// real store.
type fakeHealNotifs struct {
	rows      map[string]map[string]repo.Notification
	errFor    func(uid string) error
	delErr    map[string]error
	listCalls []string
	delCalls  []string
}

func newFakeHealNotifs() *fakeHealNotifs {
	return &fakeHealNotifs{rows: map[string]map[string]repo.Notification{}, delErr: map[string]error{}}
}

func (f *fakeHealNotifs) add(uid string, n repo.Notification) {
	if f.rows[uid] == nil {
		f.rows[uid] = map[string]repo.Notification{}
	}
	f.rows[uid][n.ID] = n
}

func (f *fakeHealNotifs) ListByKind(ctx context.Context, uid, kind string) ([]repo.Notification, error) {
	f.listCalls = append(f.listCalls, uid)
	if f.errFor != nil {
		if err := f.errFor(uid); err != nil {
			return nil, err
		}
	}
	var out []repo.Notification
	for _, n := range f.rows[uid] {
		if n.Kind == kind {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f *fakeHealNotifs) Delete(ctx context.Context, uid, id string) error {
	f.delCalls = append(f.delCalls, id)
	if err := f.delErr[id]; err != nil {
		return err
	}
	if _, ok := f.rows[uid][id]; !ok {
		return repo.ErrNotificationNotFound
	}
	delete(f.rows[uid], id)
	return nil
}

func (f *fakeHealNotifs) remaining(uid string) []string {
	var ids []string
	for id := range f.rows[uid] {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// fakeMortgageGetter is a map-backed MortgageGetter. getErrFor takes
// precedence over the map; an unknown id without errFor is
// ErrMortgageNotFound, mirroring the real store.
type fakeMortgageGetter struct {
	byID      map[string]repo.Mortgage
	getErrFor func(mid string) error
	getCalls  []string
}

func (f *fakeMortgageGetter) Get(ctx context.Context, uid, mid string, dek []byte) (repo.Mortgage, error) {
	f.getCalls = append(f.getCalls, mid)
	if f.getErrFor != nil {
		if err := f.getErrFor(mid); err != nil {
			return repo.Mortgage{}, err
		}
	}
	m, ok := f.byID[mid]
	if !ok {
		return repo.Mortgage{}, repo.ErrMortgageNotFound
	}
	return m, nil
}

// fakeDebitLister is a map-backed DebitLister keyed by account id.
type fakeDebitLister struct {
	debits map[string][]repo.DebitTxn
	errFor func(acctID string) error
}

func (f *fakeDebitLister) AccountDebits(ctx context.Context, acctID string) ([]repo.DebitTxn, error) {
	if f.errFor != nil {
		if err := f.errFor(acctID); err != nil {
			return nil, err
		}
	}
	return f.debits[acctID], nil
}

// newHealer wires the healer against the fakes; dekErrFor (when set)
// selects the per-user DEK failure.
func newHealer(t *testing.T, users *fakeUserIDLister, notifs *fakeHealNotifs, morts *fakeMortgageGetter, txns *fakeDebitLister, dekErrFor func(uid string) error) *MortgageHealer {
	t.Helper()
	return &MortgageHealer{
		Log:       testLogger(t),
		Users:     users,
		Notifs:    notifs,
		Mortgages: morts,
		Txns:      txns,
		DekForUser: func(ctx context.Context, uid string) ([]byte, error) {
			if dekErrFor != nil {
				if err := dekErrFor(uid); err != nil {
					return nil, err
				}
			}
			return []byte("dek"), nil
		},
	}
}

// aberrantRefBytes mirrors the server's aberrantRef: encoding/json
// sorts map keys, so the bytes are deterministic — and byte-identical
// to the refs the server writes (round 32).
func aberrantRefBytes(mortgageID, kind, date string) json.RawMessage {
	ref, _ := json.Marshal(map[string]any{
		"mortgage_id": mortgageID,
		"kind":        kind,
		"date":        date,
	})
	return ref
}

func healNotif(id, mortgageID, kind, date string) repo.Notification {
	return repo.Notification{ID: id, Kind: AberrantKind, Ref: aberrantRefBytes(mortgageID, kind, date)}
}

// --- fixture ----------------------------------------------------------

// healDates is the fixture's extra/short debit dates, as the "01-02"
// strings the refs and classifications key on.
type healDates struct {
	extraDate string
	shortDate string
}

// healFixture builds the deterministic mortgage + debits used by the
// self-heal tests and runs the real engine over them, asserting the
// expected shape (one extra, one short, no suggestions) so an engine
// change that moves either date fails loudly here instead of silently
// flipping the heal expectations. The amounts are the rounded base the
// real account pays (1326.18), like the in-tree server fixtures.
func healFixture(t *testing.T) (repo.Mortgage, []repo.DebitTxn, *healDates) {
	t.Helper()
	m := repo.Mortgage{
		ID:        "mort-1",
		UserID:    "u1",
		AccountID: "acct-1",
		Spec: repo.MortgageSpec{
			Principal:             "485000",
			AnnualRate:            "4.39",
			AmortizationYears:     25,
			StartDate:             "2026-01-15",
			PaymentFrequency:      "semi_monthly",
			ExtraPaymentAmount:    "0",
			ExtraPaymentFrequency: "monthly",
			ExtraPrepaymentType:   "principal",
		},
	}
	spec, err := SpecToEngine(m)
	if err != nil {
		t.Fatalf("SpecToEngine: %v", err)
	}
	s, err := mortgage.Generate(spec)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	debits := make([]repo.DebitTxn, 0, 20)
	for i := 0; i < 20; i++ {
		amt := "-1326.18"
		switch i {
		case 3:
			amt = "-1300.00" // 26.18 short
		case 5:
			amt = "-6326.18" // base + isolated 5000.00 extra
		}
		debits = append(debits, repo.DebitTxn{Date: s.Payments[i].Date, Amount: amt})
	}
	txns := make([]mortgage.Txn, 0, len(debits))
	for _, d := range debits {
		amt, err := strconv.ParseFloat(d.Amount, 64)
		if err != nil {
			t.Fatalf("fixture amount %q: %v", d.Amount, err)
		}
		txns = append(txns, mortgage.Txn{Date: d.Date, Amount: amt})
	}
	res, err := mortgage.Match(spec, txns)
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if len(res.Extras) != 1 || len(res.Shorts) != 1 || len(res.Suggestions) != 0 {
		t.Fatalf("fixture shape: extras = %v shorts = %v suggestions = %v, want one extra and one short",
			res.Extras, res.Shorts, res.Suggestions)
	}
	return m, debits, &healDates{
		extraDate: res.Extras[0].Date.Format("2006-01-02"),
		shortDate: res.Shorts[0].Date.Format("2006-01-02"),
	}
}

// --- scenarios --------------------------------------------------------

// 1. Live items are kept: notifications for the extra and the short
// with no stored classifications -> nothing deleted.
func TestHealKeepsLive(t *testing.T) {
	m, debits, d := healFixture(t)
	notifs := newFakeHealNotifs()
	notifs.add("u1", healNotif("n-extra", m.ID, "extra", d.extraDate))
	notifs.add("u1", healNotif("n-short", m.ID, "short", d.shortDate))
	morts := &fakeMortgageGetter{byID: map[string]repo.Mortgage{m.ID: m}}
	txns := &fakeDebitLister{debits: map[string][]repo.DebitTxn{m.AccountID: debits}}
	h := newHealer(t, &fakeUserIDLister{ids: []string{"u1"}}, notifs, morts, txns, nil)

	if err := h.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(notifs.delCalls) != 0 {
		t.Fatalf("delCalls = %v, want nothing", notifs.delCalls)
	}
	if got := notifs.remaining("u1"); len(got) != 2 || got[0] != "n-extra" || got[1] != "n-short" {
		t.Fatalf("remaining = %v, want both rows", got)
	}
	if len(morts.getCalls) != 1 || morts.getCalls[0] != m.ID {
		t.Errorf("getCalls = %v, want one get of %s", morts.getCalls, m.ID)
	}
}

// 2. Stale items are deleted: a stored classification for the extra
// removes it from the live set -> the extra's row is deleted, the
// short's stays. The healer does not validate the classification
// value, only its presence.
func TestHealDeletesStale(t *testing.T) {
	m, debits, d := healFixture(t)
	m.Spec.PaymentClassifications = []repo.PaymentClassification{{
		Date: d.extraDate, Kind: "extra", Classification: "gift",
	}}
	notifs := newFakeHealNotifs()
	notifs.add("u1", healNotif("n-extra", m.ID, "extra", d.extraDate))
	notifs.add("u1", healNotif("n-short", m.ID, "short", d.shortDate))
	morts := &fakeMortgageGetter{byID: map[string]repo.Mortgage{m.ID: m}}
	txns := &fakeDebitLister{debits: map[string][]repo.DebitTxn{m.AccountID: debits}}
	h := newHealer(t, &fakeUserIDLister{ids: []string{"u1"}}, notifs, morts, txns, nil)

	if err := h.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := notifs.delCalls; len(got) != 1 || got[0] != "n-extra" {
		t.Fatalf("delCalls = %v, want exactly [n-extra]", got)
	}
	if got := notifs.remaining("u1"); len(got) != 1 || got[0] != "n-short" {
		t.Fatalf("remaining = %v, want only n-short", got)
	}
}

// 3. An unparseable ref is deleted outright: no report can ever own
// the row, so the mortgage is never fetched.
func TestHealDeletesUnparseableRef(t *testing.T) {
	notifs := newFakeHealNotifs()
	notifs.rows["u1"] = map[string]repo.Notification{
		"n-bad": {ID: "n-bad", Kind: AberrantKind, Ref: json.RawMessage("not-json")},
	}
	morts := &fakeMortgageGetter{byID: map[string]repo.Mortgage{}}
	txns := &fakeDebitLister{debits: map[string][]repo.DebitTxn{}}
	h := newHealer(t, &fakeUserIDLister{ids: []string{"u1"}}, notifs, morts, txns, nil)

	if err := h.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := notifs.delCalls; len(got) != 1 || got[0] != "n-bad" {
		t.Fatalf("delCalls = %v, want [n-bad]", got)
	}
	if got := notifs.remaining("u1"); len(got) != 0 {
		t.Fatalf("remaining = %v, want none", got)
	}
	if len(morts.getCalls) != 0 {
		t.Errorf("getCalls = %v, want none (an unparseable ref cannot name a mortgage)", morts.getCalls)
	}
}

// 4. A ref with an empty kind is incomplete and is deleted outright.
func TestHealDeletesEmptyKindRef(t *testing.T) {
	notifs := newFakeHealNotifs()
	notifs.rows["u1"] = map[string]repo.Notification{
		"n-kindless": {ID: "n-kindless", Kind: AberrantKind, Ref: aberrantRefBytes("mort-1", "", "2026-02-15")},
	}
	morts := &fakeMortgageGetter{byID: map[string]repo.Mortgage{}}
	txns := &fakeDebitLister{debits: map[string][]repo.DebitTxn{}}
	h := newHealer(t, &fakeUserIDLister{ids: []string{"u1"}}, notifs, morts, txns, nil)

	if err := h.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := notifs.delCalls; len(got) != 1 || got[0] != "n-kindless" {
		t.Fatalf("delCalls = %v, want [n-kindless]", got)
	}
	if got := notifs.remaining("u1"); len(got) != 0 {
		t.Fatalf("remaining = %v, want none", got)
	}
}

// 5. Gone mortgage: Get returns ErrMortgageNotFound -> the group's
// rows are orphans and are all deleted.
func TestHealDeletesGoneMortgage(t *testing.T) {
	m, _, d := healFixture(t)
	notifs := newFakeHealNotifs()
	notifs.add("u1", healNotif("n-extra", m.ID, "extra", d.extraDate))
	notifs.add("u1", healNotif("n-short", m.ID, "short", d.shortDate))
	morts := &fakeMortgageGetter{byID: map[string]repo.Mortgage{}} // unknown id -> ErrMortgageNotFound
	txns := &fakeDebitLister{debits: map[string][]repo.DebitTxn{}}
	h := newHealer(t, &fakeUserIDLister{ids: []string{"u1"}}, notifs, morts, txns, nil)

	if err := h.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := notifs.delCalls; len(got) != 2 || got[0] != "n-extra" || got[1] != "n-short" {
		t.Fatalf("delCalls = %v, want both rows", got)
	}
	if got := notifs.remaining("u1"); len(got) != 0 {
		t.Fatalf("remaining = %v, want none", got)
	}
}

// 6. Get error (other than not-found): unknown, so the mortgage's rows
// are left untouched and the run is not fatal.
func TestHealGetErrorLeavesRows(t *testing.T) {
	m, debits, d := healFixture(t)
	notifs := newFakeHealNotifs()
	notifs.add("u1", healNotif("n-extra", m.ID, "extra", d.extraDate))
	notifs.add("u1", healNotif("n-short", m.ID, "short", d.shortDate))
	morts := &fakeMortgageGetter{
		byID: map[string]repo.Mortgage{m.ID: m},
		getErrFor: func(mid string) error {
			if mid == m.ID {
				return errors.New("boom-get")
			}
			return nil
		},
	}
	txns := &fakeDebitLister{debits: map[string][]repo.DebitTxn{m.AccountID: debits}}
	h := newHealer(t, &fakeUserIDLister{ids: []string{"u1"}}, notifs, morts, txns, nil)

	if err := h.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(notifs.delCalls) != 0 {
		t.Fatalf("delCalls = %v, want nothing", notifs.delCalls)
	}
	if got := notifs.remaining("u1"); len(got) != 2 {
		t.Fatalf("remaining = %v, want both rows untouched", got)
	}
	if len(morts.getCalls) != 1 || morts.getCalls[0] != m.ID {
		t.Errorf("getCalls = %v, want one get of %s", morts.getCalls, m.ID)
	}
}

// 7. DEK failure: the user is skipped before any listing — no
// ListByKind call for them, their rows untouched, the other user
// still healed, the run not fatal.
func TestHealDekFailureSkipsUser(t *testing.T) {
	m, debits, d := healFixture(t)
	notifs := newFakeHealNotifs()
	notifs.add("u1", healNotif("n-extra", m.ID, "extra", d.extraDate))
	notifs.add("u2", healNotif("n-2", m.ID, "short", d.shortDate))
	morts := &fakeMortgageGetter{byID: map[string]repo.Mortgage{m.ID: m}}
	txns := &fakeDebitLister{debits: map[string][]repo.DebitTxn{m.AccountID: debits}}
	h := newHealer(t,
		&fakeUserIDLister{ids: []string{"u1", "u2"}},
		notifs, morts, txns,
		func(uid string) error {
			if uid == "u2" {
				return errors.New("boom-dek")
			}
			return nil
		},
	)

	if err := h.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, c := range notifs.listCalls {
		if c == "u2" {
			t.Fatalf("listCalls = %v: u2 must be skipped before the listing", notifs.listCalls)
		}
	}
	if got := notifs.remaining("u2"); len(got) != 1 || got[0] != "n-2" {
		t.Fatalf("remaining(u2) = %v, want the untouched row", got)
	}
	if got := notifs.remaining("u1"); len(got) != 1 || got[0] != "n-extra" {
		t.Fatalf("remaining(u1) = %v, want the live row (the good user is still healed)", got)
	}
	if len(notifs.delCalls) != 0 {
		t.Fatalf("delCalls = %v, want nothing (both items are live)", notifs.delCalls)
	}
}

// 8. ListUserIDs error: the run aborts with the wrapped error
// (mirrors the dispatcher's ListUsers handling).
func TestHealListUserIDsError(t *testing.T) {
	errUsers := errors.New("boom-list-users")
	h := newHealer(t,
		&fakeUserIDLister{err: errUsers},
		newFakeHealNotifs(),
		&fakeMortgageGetter{byID: map[string]repo.Mortgage{}},
		&fakeDebitLister{debits: map[string][]repo.DebitTxn{}},
		nil,
	)

	err := h.Run(context.Background())
	if !errors.Is(err, errUsers) {
		t.Fatalf("Run = %v, want it to wrap %v", err, errUsers)
	}
}

// 9. ListByKind error: the user is skipped (rows untouched, no
// mortgage fetch) and the run is not fatal.
func TestHealListByKindError(t *testing.T) {
	errList := errors.New("boom-list")
	notifs := newFakeHealNotifs()
	notifs.errFor = func(uid string) error { return errList }
	notifs.rows["u1"] = map[string]repo.Notification{
		"n-1": {ID: "n-1", Kind: AberrantKind, Ref: aberrantRefBytes("mort-1", "extra", "2026-02-15")},
	}
	morts := &fakeMortgageGetter{byID: map[string]repo.Mortgage{}}
	txns := &fakeDebitLister{debits: map[string][]repo.DebitTxn{}}
	h := newHealer(t, &fakeUserIDLister{ids: []string{"u1"}}, notifs, morts, txns, nil)

	if err := h.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v (per-user failures are logged and skipped)", err)
	}
	if len(notifs.delCalls) != 0 {
		t.Fatalf("delCalls = %v, want nothing", notifs.delCalls)
	}
	if got := notifs.remaining("u1"); len(got) != 1 {
		t.Fatalf("remaining = %v, want the untouched row", got)
	}
	if len(morts.getCalls) != 0 {
		t.Errorf("getCalls = %v, want none", morts.getCalls)
	}
}

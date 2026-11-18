// Offset store tests (migration 0012): one shared postgres:18
// testcontainer (newTestEnv) with sequential subtests covering the
// matcher (mirror amounts, date window, same currency, from/to range,
// created_at watermark, idempotence), the suggestion lifecycle (list,
// accept, dismiss), hand groups (balanced three-row set, unbalanced,
// already grouped, missing id), clear/re-scan, per-user scoping, and
// the worker-tick helpers (UsersWithTxSince).
package repo

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestOffsetStore(t *testing.T) {
	env := newTestEnv(t, "off-a", "off-b")
	tx := NewTransactionStore(env.pool)
	acct := NewAccountStore(env.pool)
	if tx == nil || acct == nil {
		t.Fatal("non-nil pool must give non-nil stores")
	}
	a := env.user(t, "off-a")
	b := env.user(t, "off-b")
	aid, bid := a.row.ID, b.row.ID
	ctx := env.ctx

	// A: three accounts (two USD, one EUR) with a mirror pair inside
	// the default 7-day window, one pair 19 days apart, one EUR/USD
	// pair, one unmirrored row, and a three-row net-to-zero set that
	// never pairs up (the scan only proposes exact mirrors). B: its
	// own mirror pair on the same dates — the isolation fixture.
	cheq, err := acct.Create(ctx, aid, AccountIn{Name: "Off Chequing", Type: "bank", Currency: "USD"}, a.dek)
	if err != nil {
		t.Fatalf("create cheq: %v", err)
	}
	cc, err := acct.Create(ctx, aid, AccountIn{Name: "Off Credit", Type: "credit_card", Currency: "USD"}, a.dek)
	if err != nil {
		t.Fatalf("create cc: %v", err)
	}
	eur, err := acct.Create(ctx, aid, AccountIn{Name: "Off EUR", Type: "bank", Currency: "EUR"}, a.dek)
	if err != nil {
		t.Fatalf("create eur: %v", err)
	}
	bcheq, err := acct.Create(ctx, bid, AccountIn{Name: "Off Chequing B", Type: "bank", Currency: "USD"}, b.dek)
	if err != nil {
		t.Fatalf("create bcheq: %v", err)
	}
	bcc, err := acct.Create(ctx, bid, AccountIn{Name: "Off Credit B", Type: "credit_card", Currency: "USD"}, b.dek)
	if err != nil {
		t.Fatalf("create bcc: %v", err)
	}

	seed := func(uid string, dek []byte, acctID, date, amount, desc string) Transaction {
		t.Helper()
		tm, err := tx.Create(ctx, uid, TransactionIn{
			AccountID:   acctID,
			DatePosted:  dateOf(date),
			Amount:      amount,
			Description: desc,
		}, dek)
		if err != nil {
			t.Fatalf("create %s (%s): %v", desc, amount, err)
		}
		return tm
	}

	t1 := seed(aid, a.dek, cheq.ID, "2026-11-01", "-100.00", "cc pay")
	t2 := seed(aid, a.dek, cc.ID, "2026-11-03", "100.00", "cc pay posted")
	t3 := seed(aid, a.dek, cheq.ID, "2026-10-01", "-50.00", "far out")
	t4 := seed(aid, a.dek, cc.ID, "2026-10-20", "50.00", "far in")
	t5 := seed(aid, a.dek, cheq.ID, "2026-11-02", "-75.00", "eur leg out")
	t6 := seed(aid, a.dek, eur.ID, "2026-11-02", "75.00", "eur leg in")
	t7 := seed(aid, a.dek, cheq.ID, "2026-11-05", "-25.00", "stray")
	t8 := seed(aid, a.dek, cheq.ID, "2026-11-04", "-30.00", "three one")
	t9 := seed(aid, a.dek, cc.ID, "2026-11-04", "12.50", "three two")
	t10 := seed(aid, a.dek, cc.ID, "2026-11-04", "17.50", "three three")
	bt1 := seed(bid, b.dek, bcheq.ID, "2026-11-01", "-100.00", "b cc pay")
	bt2 := seed(bid, b.dek, bcc.ID, "2026-11-01", "100.00", "b cc pay posted")

	// pairKey canonicalises a pair to "lo|hi" (the uuid order the pair
	// key uses) so order does not matter in the assertions.
	pairKey := func(x, y Transaction) string {
		lo, hi := x.ID, y.ID
		if lo > hi {
			lo, hi = hi, lo
		}
		return lo + "|" + hi
	}
	pairSet := func(cands []OffsetCandidate) map[string]bool {
		m := make(map[string]bool, len(cands))
		for _, c := range cands {
			m[pairKey(c.A, c.B)] = true
		}
		return m
	}
	listIDs := func(t *testing.T) map[string]bool {
		t.Helper()
		txs, err := tx.List(ctx, aid, TransactionFilter{Limit: 100}, a.dek)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		m := make(map[string]bool, len(txs))
		for _, tx := range txs {
			m[tx.ID] = true
		}
		return m
	}

	t.Run("matcher finds the windowed mirror pair", func(t *testing.T) {
		cands, err := tx.FindOffsetCandidates(ctx, aid, a.dek, nil, nil, 7)
		if err != nil {
			t.Fatalf("find: %v", err)
		}
		want := map[string]bool{pairKey(t1, t2): true}
		if got := pairSet(cands); len(got) != len(want) || !samePairSet(got, want) {
			t.Fatalf("window 7 found %v, want %v", got, want)
		}
		// The pair's legs must be fully opened (description decrypted).
		if cands[0].A.Description != t1.Description && cands[0].B.Description != t1.Description {
			t.Errorf("legs not opened: %+v", cands[0])
		}
	})

	t.Run("wider window admits the far pair, not the EUR pair", func(t *testing.T) {
		cands, err := tx.FindOffsetCandidates(ctx, aid, a.dek, nil, nil, 30)
		if err != nil {
			t.Fatalf("find: %v", err)
		}
		want := map[string]bool{pairKey(t1, t2): true, pairKey(t3, t4): true}
		if got := pairSet(cands); len(got) != len(want) || !samePairSet(got, want) {
			t.Fatalf("window 30 found %v, want %v", got, want)
		}
	})

	t.Run("from/to keeps only pairs with a leg in range", func(t *testing.T) {
		from, to := dateOf("2026-10-15"), dateOf("2026-10-25")
		cands, err := tx.FindOffsetCandidates(ctx, aid, a.dek, &from, &to, 30)
		if err != nil {
			t.Fatalf("find: %v", err)
		}
		want := map[string]bool{pairKey(t3, t4): true} // t4 is in range; t1/t2 are not
		if got := pairSet(cands); len(got) != len(want) || !samePairSet(got, want) {
			t.Fatalf("range scan found %v, want %v", got, want)
		}
	})

	t.Run("insert, list, and idempotent re-scan", func(t *testing.T) {
		cands, err := tx.FindOffsetCandidates(ctx, aid, a.dek, nil, nil, 7)
		if err != nil {
			t.Fatalf("find: %v", err)
		}
		n, err := tx.InsertOffsetSuggestions(ctx, aid, cands)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		if n != 1 {
			t.Fatalf("inserted %d suggestions, want 1", n)
		}
		sugs, err := tx.ListOffsetSuggestions(ctx, aid, a.dek, 0, 0)
		if err != nil {
			t.Fatalf("list suggestions: %v", err)
		}
		if len(sugs) != 1 || !samePairSet(map[string]bool{pairKey(sugs[0].A, sugs[0].B): true}, map[string]bool{pairKey(t1, t2): true}) {
			t.Fatalf("got %d suggestions %+v, want the t1/t2 pair", len(sugs), sugs)
		}
		// The pair now has a row in ANY status — re-scans skip it.
		cands2, err := tx.FindOffsetCandidates(ctx, aid, a.dek, nil, nil, 7)
		if err != nil {
			t.Fatalf("re-find: %v", err)
		}
		if len(cands2) != 0 {
			t.Fatalf("re-scan found %d candidates, want 0", len(cands2))
		}
		n2, err := tx.InsertOffsetSuggestions(ctx, aid, cands)
		if err != nil {
			t.Fatalf("re-insert: %v", err)
		}
		if n2 != 0 {
			t.Fatalf("re-insert added %d suggestions, want 0", n2)
		}
	})

	var groupID string
	t.Run("accept groups the legs and hides them", func(t *testing.T) {
		sugs, err := tx.ListOffsetSuggestions(ctx, aid, a.dek, 0, 0)
		if err != nil {
			t.Fatalf("list suggestions: %v", err)
		}
		if len(sugs) != 1 {
			t.Fatalf("got %d suggestions, want 1", len(sugs))
		}
		groupID, err = tx.AcceptOffsetSuggestion(ctx, aid, sugs[0].ID, a.dek)
		if err != nil {
			t.Fatalf("accept: %v", err)
		}
		if groupID == "" {
			t.Fatal("accept returned an empty group id")
		}
		// t1/t2 leave the list; the other eight rows stay.
		if got := listIDs(t); !(!got[t1.ID] && !got[t2.ID] && got[t3.ID] && got[t4.ID] &&
			got[t5.ID] && got[t6.ID] && got[t7.ID] && got[t8.ID] && got[t9.ID] && got[t10.ID]) {
			t.Fatalf("list after accept = %v (want t1/t2 hidden)", got)
		}
		groups, err := tx.ListOffsetGroups(ctx, aid, a.dek, 0, 0)
		if err != nil {
			t.Fatalf("list groups: %v", err)
		}
		if len(groups) != 1 || groups[0].ID != groupID || len(groups[0].Members) != 2 {
			t.Fatalf("groups = %+v, want one group %s with 2 members", groups, groupID)
		}
		sugs2, err := tx.ListOffsetSuggestions(ctx, aid, a.dek, 0, 0)
		if err != nil {
			t.Fatalf("list suggestions: %v", err)
		}
		if len(sugs2) != 0 {
			t.Fatalf("accepted suggestion still pending: %+v", sugs2)
		}
		if _, err := tx.AcceptOffsetSuggestion(ctx, aid, sugs[0].ID, a.dek); !errors.Is(err, ErrOffsetSuggestionNotFound) {
			t.Fatalf("re-accept err = %v, want ErrOffsetSuggestionNotFound", err)
		}
	})

	t.Run("clear re-opens the pair to the matcher", func(t *testing.T) {
		if err := tx.ClearOffsetGroup(ctx, aid, groupID); err != nil {
			t.Fatalf("clear: %v", err)
		}
		if got := listIDs(t); !got[t1.ID] || !got[t2.ID] || len(got) != 10 {
			t.Fatalf("list after clear = %d rows %v (want all 10)", len(got), got)
		}
		cands, err := tx.FindOffsetCandidates(ctx, aid, a.dek, nil, nil, 7)
		if err != nil {
			t.Fatalf("find: %v", err)
		}
		if len(cands) != 1 || !samePairSet(pairSet(cands), map[string]bool{pairKey(t1, t2): true}) {
			t.Fatalf("post-clear scan found %v, want the t1/t2 pair re-proposable", pairSet(cands))
		}
		if err := tx.ClearOffsetGroup(ctx, aid, uuid.NewString()); !errors.Is(err, ErrOffsetGroupNotFound) {
			t.Fatalf("clear unknown err = %v, want ErrOffsetGroupNotFound", err)
		}
	})

	t.Run("dismiss sticks until the pair is re-opened", func(t *testing.T) {
		cands, err := tx.FindOffsetCandidates(ctx, aid, a.dek, nil, nil, 7)
		if err != nil {
			t.Fatalf("find: %v", err)
		}
		if n, err := tx.InsertOffsetSuggestions(ctx, aid, cands); err != nil || n != 1 {
			t.Fatalf("insert = %d, %v (want 1, nil)", n, err)
		}
		sugs, err := tx.ListOffsetSuggestions(ctx, aid, a.dek, 0, 0)
		if err != nil {
			t.Fatalf("list suggestions: %v", err)
		}
		if len(sugs) != 1 {
			t.Fatalf("got %d suggestions, want 1", len(sugs))
		}
		if err := tx.DismissOffsetSuggestion(ctx, aid, sugs[0].ID); err != nil {
			t.Fatalf("dismiss: %v", err)
		}
		sugs2, err := tx.ListOffsetSuggestions(ctx, aid, a.dek, 0, 0)
		if err != nil {
			t.Fatalf("list suggestions: %v", err)
		}
		if len(sugs2) != 0 {
			t.Fatalf("dismissed suggestion still pending: %+v", sugs2)
		}
		cands2, err := tx.FindOffsetCandidates(ctx, aid, a.dek, nil, nil, 7)
		if err != nil {
			t.Fatalf("find: %v", err)
		}
		if len(cands2) != 0 {
			t.Fatalf("dismissed pair re-proposed: %v", cands2)
		}
		if err := tx.DismissOffsetSuggestion(ctx, aid, sugs[0].ID); !errors.Is(err, ErrOffsetSuggestionNotFound) {
			t.Fatalf("re-dismiss err = %v, want ErrOffsetSuggestionNotFound", err)
		}
	})

	t.Run("hand group: balanced, unbalanced, grouped, missing", func(t *testing.T) {
		gid, err := tx.SetOffsetGroup(ctx, aid, []string{t8.ID, t9.ID, t10.ID}, a.dek)
		if err != nil {
			t.Fatalf("set group: %v", err)
		}
		groups, err := tx.ListOffsetGroups(ctx, aid, a.dek, 0, 0)
		if err != nil {
			t.Fatalf("list groups: %v", err)
		}
		if len(groups) != 1 || groups[0].ID != gid || len(groups[0].Members) != 3 {
			t.Fatalf("groups = %+v, want one group with 3 members", groups)
		}
		if _, err := tx.SetOffsetGroup(ctx, aid, []string{t5.ID, t7.ID}, a.dek); !errors.Is(err, ErrOffsetNotBalanced) {
			t.Fatalf("unbalanced err = %v, want ErrOffsetNotBalanced", err)
		}
		if _, err := tx.SetOffsetGroup(ctx, aid, []string{t8.ID, t7.ID}, a.dek); !errors.Is(err, ErrOffsetAlreadyGrouped) {
			t.Fatalf("grouped err = %v, want ErrOffsetAlreadyGrouped", err)
		}
		if _, err := tx.SetOffsetGroup(ctx, aid, []string{uuid.NewString()}, a.dek); !errors.Is(err, ErrTransactionNotFound) {
			t.Fatalf("missing err = %v, want ErrTransactionNotFound", err)
		}
	})

	t.Run("per-user scoping", func(t *testing.T) {
		// B's scan sees only B's pair (A's identical pair on the same
		// dates was never visible to B — and vice versa, subtest 1).
		cands, err := tx.FindOffsetCandidates(ctx, bid, b.dek, nil, nil, 7)
		if err != nil {
			t.Fatalf("find: %v", err)
		}
		want := map[string]bool{pairKey(bt1, bt2): true}
		if got := pairSet(cands); len(got) != len(want) || !samePairSet(got, want) {
			t.Fatalf("B's scan found %v, want %v", got, want)
		}
		if n, err := tx.InsertOffsetSuggestions(ctx, bid, cands); err != nil || n != 1 {
			t.Fatalf("insert = %d, %v (want 1, nil)", n, err)
		}
		sugsA, err := tx.ListOffsetSuggestions(ctx, aid, a.dek, 0, 0)
		if err != nil {
			t.Fatalf("list A suggestions: %v", err)
		}
		if len(sugsA) != 0 {
			t.Fatalf("A sees B's suggestion: %+v", sugsA)
		}
		sugsB, err := tx.ListOffsetSuggestions(ctx, bid, b.dek, 0, 0)
		if err != nil {
			t.Fatalf("list B suggestions: %v", err)
		}
		if len(sugsB) != 1 {
			t.Fatalf("B got %d suggestions, want 1", len(sugsB))
		}
		if _, err := tx.AcceptOffsetSuggestion(ctx, aid, sugsB[0].ID, a.dek); !errors.Is(err, ErrOffsetSuggestionNotFound) {
			t.Fatalf("A accepts B's suggestion: err = %v, want ErrOffsetSuggestionNotFound", err)
		}
		groupsA, err := tx.ListOffsetGroups(ctx, aid, a.dek, 0, 0)
		if err != nil {
			t.Fatalf("list A groups: %v", err)
		}
		if len(groupsA) != 1 || len(groupsA[0].Members) != 3 {
			t.Fatalf("A's groups = %+v, want only the three-row group", groupsA)
		}
	})

	// Late-import fixture: two legs whose posting dates are ~40 days
	// old but whose rows were created just now — the case the
	// created_at watermark scan exists for (a backfill of old dates).
	lateA := seed(aid, a.dek, cheq.ID, "2026-10-08", "-200.00", "late out")
	lateB := seed(aid, a.dek, cc.ID, "2026-10-10", "200.00", "late in")

	t.Run("late-import pair: found by the created_at scan, not the date range", func(t *testing.T) {
		since := time.Now().Add(-time.Hour)
		cands, err := tx.FindOffsetCandidatesSince(ctx, aid, a.dek, 7, since)
		if err != nil {
			t.Fatalf("find since: %v", err)
		}
		// Exactly the late pair: t1/t2 carries a dismissed
		// suggestion (earlier subtest), t3/t4 is 19 days apart
		// (outside window 7), t5/t6 are different currencies, and
		// t8/t9/t10 are grouped.
		want := map[string]bool{pairKey(lateA, lateB): true}
		if got := pairSet(cands); len(got) != len(want) || !samePairSet(got, want) {
			t.Fatalf("created_at scan found %v, want %v", got, want)
		}
		from := time.Now().Add(-24 * time.Hour)
		to := time.Now()
		ranged, err := tx.FindOffsetCandidates(ctx, aid, a.dek, &from, &to, 7)
		if err != nil {
			t.Fatalf("find ranged: %v", err)
		}
		if got := pairSet(ranged); got[pairKey(lateA, lateB)] {
			t.Fatalf("date-range scan found the late pair it must miss: %v", got)
		}
	})

	t.Run("pair created before the watermark is not found", func(t *testing.T) {
		// t3/t4 is 19 days apart (window 30 admits it) and has no
		// suggestion row, but it was created before now — the strict
		// created_at > since watermark excludes it.
		cands, err := tx.FindOffsetCandidatesSince(ctx, aid, a.dek, 30, time.Now())
		if err != nil {
			t.Fatalf("find since: %v", err)
		}
		if len(cands) != 0 {
			t.Fatalf("watermark scan found candidates created before the watermark: %v", cands)
		}
	})

	t.Run("late pair is idempotent once suggested", func(t *testing.T) {
		cands, err := tx.FindOffsetCandidatesSince(ctx, aid, a.dek, 7, time.Now().Add(-time.Hour))
		if err != nil {
			t.Fatalf("find since: %v", err)
		}
		n, err := tx.InsertOffsetSuggestions(ctx, aid, cands)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		if n != 1 {
			t.Fatalf("inserted %d suggestions, want 1", n)
		}
		cands2, err := tx.FindOffsetCandidatesSince(ctx, aid, a.dek, 7, time.Now().Add(-time.Hour))
		if err != nil {
			t.Fatalf("re-find since: %v", err)
		}
		if len(cands2) != 0 {
			t.Fatalf("re-scan found %d candidates, want 0 (the late pair is already suggested)", len(cands2))
		}
	})

	t.Run("UsersWithTxSince lists exactly the users with new rows", func(t *testing.T) {
		users, err := tx.UsersWithTxSince(ctx, time.Now().Add(-time.Hour))
		if err != nil {
			t.Fatalf("users with new tx: %v", err)
		}
		// Both seeded users have rows created in this test run; no
		// other user exists in the container.
		if len(users) != 2 {
			t.Fatalf("users = %v, want exactly two", users)
		}
		got := map[string]bool{}
		for _, u := range users {
			got[u] = true
		}
		if !got[aid] || !got[bid] {
			t.Fatalf("users = %v, want %v and %v", users, aid, bid)
		}
		if users[0] > users[1] {
			t.Fatalf("users not ascending: %v", users)
		}
	})
}

// samePairSet reports whether two "lo|hi" pair sets are equal.
func samePairSet(got, want map[string]bool) bool {
	if len(got) != len(want) {
		return false
	}
	for k := range want {
		if !got[k] {
			return false
		}
	}
	return true
}

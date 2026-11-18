package schedule

// T3 offset-suggester tests: fixed clock + a recording fake scanner
// (no DB). The since passthrough, per-user forwarding, zero-candidate
// skip, DEK skip, and the never-fatal per-user error handling are all
// asserted through the fake's recorded calls.

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"finance-tracker/internal/repo"
)

// --- fakes ------------------------------------------------------------

// offsetCandCall records one FindOffsetCandidatesSince invocation.
type offsetCandCall struct {
	userID     string
	windowDays int
	since      time.Time
}

func (c offsetCandCall) uid() string { return c.userID }

// offsetInsCall records one InsertOffsetSuggestions invocation.
type offsetInsCall struct {
	userID string
	cands  []repo.OffsetCandidate
}

func (c offsetInsCall) uid() string { return c.userID }

// fakeOffsetScanner is a recording OffsetScanner: fixed user list,
// per-user candidate table, and per-user error selectors.
type fakeOffsetScanner struct {
	users      []string
	usersErr   error
	sinceCalls []time.Time
	candCalls  []offsetCandCall
	cands      map[string][]repo.OffsetCandidate
	candErrFor func(uid string) error
	insCalls   []offsetInsCall
	insErrFor  func(uid string) error
	insCounts  map[string]int // return value of InsertOffsetSuggestions per user
}

func (f *fakeOffsetScanner) UsersWithTxSince(ctx context.Context, since time.Time) ([]string, error) {
	f.sinceCalls = append(f.sinceCalls, since)
	return f.users, f.usersErr
}

func (f *fakeOffsetScanner) FindOffsetCandidatesSince(ctx context.Context, userID string, dek []byte, windowDays int, since time.Time) ([]repo.OffsetCandidate, error) {
	f.candCalls = append(f.candCalls, offsetCandCall{userID: userID, windowDays: windowDays, since: since})
	if f.candErrFor != nil {
		if err := f.candErrFor(userID); err != nil {
			return nil, err
		}
	}
	return f.cands[userID], nil
}

func (f *fakeOffsetScanner) InsertOffsetSuggestions(ctx context.Context, userID string, cands []repo.OffsetCandidate) (int, error) {
	f.insCalls = append(f.insCalls, offsetInsCall{userID: userID, cands: cands})
	if f.insErrFor != nil {
		if err := f.insErrFor(userID); err != nil {
			return 0, err
		}
	}
	if n, ok := f.insCounts[userID]; ok {
		return n, nil
	}
	return len(cands), nil
}

func offsetCand(a, b string) repo.OffsetCandidate {
	return repo.OffsetCandidate{A: repo.Transaction{ID: a}, B: repo.Transaction{ID: b}}
}

func newSuggester(t *testing.T, now time.Time, sc *fakeOffsetScanner, dekErrFor func(uid string) error) (*OffsetSuggester, *strings.Builder) {
	t.Helper()
	logBuf := &strings.Builder{}
	return &OffsetSuggester{
		Log:   slog.New(slog.NewJSONHandler(logBuf, nil)),
		Clock: fakeClock{now: now},
		Txns:  sc,
		DekForUser: func(ctx context.Context, uid string) ([]byte, error) {
			if dekErrFor != nil {
				if err := dekErrFor(uid); err != nil {
					return nil, err
				}
			}
			return []byte("dek"), nil
		},
	}, logBuf
}

// userOf reports whether any recorded call targeted uid.
func userOf[T interface{ uid() string }](calls []T, uid string) bool {
	for _, c := range calls {
		if c.uid() == uid {
			return true
		}
	}
	return false
}

// --- scenarios --------------------------------------------------------

// 1. since passthrough: fixed clock T -> UsersWithTxSince(T-2h) and
// FindOffsetCandidatesSince receives (7, T-2h).
func TestOffsetSuggesterSincePassthrough(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	sc := &fakeOffsetScanner{users: []string{"u1"}}
	s, _ := newSuggester(t, now, sc, nil)

	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := now.Add(-2 * time.Hour)
	if len(sc.sinceCalls) != 1 || !sc.sinceCalls[0].Equal(want) {
		t.Fatalf("sinceCalls = %v, want [%s]", sc.sinceCalls, want)
	}
	if len(sc.candCalls) != 1 || sc.candCalls[0].userID != "u1" ||
		sc.candCalls[0].windowDays != 7 || !sc.candCalls[0].since.Equal(want) {
		t.Fatalf("candCalls = %+v, want u1 with window 7 and since %s", sc.candCalls, want)
	}
}

// 2. Candidates are forwarded per user and the returned insert counts
// are summed into the finish log.
func TestOffsetSuggesterForwardsAndSums(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	u1 := []repo.OffsetCandidate{offsetCand("a1", "b1"), offsetCand("a2", "b2")}
	u2 := []repo.OffsetCandidate{offsetCand("a3", "b3")}
	sc := &fakeOffsetScanner{
		users:     []string{"u1", "u2"},
		cands:     map[string][]repo.OffsetCandidate{"u1": u1, "u2": u2},
		insCounts: map[string]int{"u1": 2, "u2": 1},
	}
	s, logBuf := newSuggester(t, now, sc, nil)

	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sc.insCalls) != 2 {
		t.Fatalf("insCalls = %+v, want one per user", sc.insCalls)
	}
	if sc.insCalls[0].userID != "u1" || len(sc.insCalls[0].cands) != 2 {
		t.Errorf("insCalls[0] = %+v, want u1's two candidates", sc.insCalls[0])
	}
	if sc.insCalls[1].userID != "u2" || len(sc.insCalls[1].cands) != 1 {
		t.Errorf("insCalls[1] = %+v, want u2's one candidate", sc.insCalls[1])
	}
	if !strings.Contains(logBuf.String(), `"inserted":3`) {
		t.Errorf("finish log must sum the returned counts (want \"inserted\":3): %s", logBuf.String())
	}
}

// 3. A user with zero candidates -> no InsertOffsetSuggestions call
// for that user.
func TestOffsetSuggesterZeroCandidatesNoInsert(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	sc := &fakeOffsetScanner{
		users: []string{"u1"},
		cands: map[string][]repo.OffsetCandidate{"u1": nil},
	}
	s, _ := newSuggester(t, now, sc, nil)

	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !userOf(sc.candCalls, "u1") {
		t.Fatalf("candCalls = %+v, want u1 scanned", sc.candCalls)
	}
	if len(sc.insCalls) != 0 {
		t.Fatalf("insCalls = %+v, want none (zero candidates)", sc.insCalls)
	}
}

// 4. DEK failure: that user is skipped (no candidate scan, no
// insert), the other users still processed, Run nil.
func TestOffsetSuggesterDekFailureSkipsUser(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	sc := &fakeOffsetScanner{
		users: []string{"u1", "u2"},
		cands: map[string][]repo.OffsetCandidate{
			"u1": []repo.OffsetCandidate{offsetCand("a1", "b1")},
			"u2": []repo.OffsetCandidate{offsetCand("a2", "b2")},
		},
	}
	s, _ := newSuggester(t, now, sc, func(uid string) error {
		if uid == "u2" {
			return errors.New("boom-dek")
		}
		return nil
	})

	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if userOf(sc.candCalls, "u2") {
		t.Fatalf("candCalls = %+v: u2 must be skipped before the candidate scan", sc.candCalls)
	}
	if len(sc.insCalls) != 1 || sc.insCalls[0].userID != "u1" {
		t.Fatalf("insCalls = %+v, want exactly u1", sc.insCalls)
	}
}

// 5. UsersWithTxSince error -> Run returns the wrapped error.
func TestOffsetSuggesterUsersSinceError(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	errUsers := errors.New("boom-users")
	sc := &fakeOffsetScanner{usersErr: errUsers}
	s, _ := newSuggester(t, now, sc, nil)

	err := s.Run(context.Background())
	if !errors.Is(err, errUsers) {
		t.Fatalf("Run = %v, want it to wrap %v", err, errUsers)
	}
}

// 6. Per-user candidate or insert error -> Run returns nil (logged,
// never fatal) and the other user is still processed.
func TestOffsetSuggesterPerUserErrors(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		candErr error
		insErr  error
	}{
		"candidate error": {candErr: errors.New("boom-cands")},
		"insert error":    {insErr: errors.New("boom-insert")},
	} {
		t.Run(name, func(t *testing.T) {
			sc := &fakeOffsetScanner{
				users: []string{"u1", "u2"},
				cands: map[string][]repo.OffsetCandidate{
					"u1": []repo.OffsetCandidate{offsetCand("a1", "b1")},
					"u2": []repo.OffsetCandidate{offsetCand("a2", "b2")},
				},
				candErrFor: func(uid string) error {
					if uid == "u2" {
						return tc.candErr
					}
					return nil
				},
				insErrFor: func(uid string) error {
					if uid == "u2" {
						return tc.insErr
					}
					return nil
				},
			}
			s, _ := newSuggester(t, now, sc, nil)

			if err := s.Run(context.Background()); err != nil {
				t.Fatalf("Run = %v, want nil (per-user errors are logged and skipped)", err)
			}
			if !userOf(sc.insCalls, "u1") {
				t.Fatalf("insCalls = %+v, want u1 still processed", sc.insCalls)
			}
			if tc.candErr != nil && userOf(sc.insCalls, "u2") {
				t.Fatalf("insCalls = %+v, u2 must not be inserted after a candidate error", sc.insCalls)
			}
		})
	}
}

package categorize

// Golden tests (plan 5.2): every test drives Run through a mock
// llm.Categorizer — no HTTP, no containers — with fixed categories and
// items, so the expected suggestions are pinned.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"finance-tracker/internal/llm"
)

// mockCategorizer returns canned JSON (or an error) and records the
// Request it received, for prompt assertions.
type mockCategorizer struct {
	json  json.RawMessage
	err   error
	req   llm.Request
	calls int
}

func (m *mockCategorizer) Categorize(_ context.Context, req llm.Request) (*llm.Response, error) {
	m.calls++
	m.req = req
	if m.err != nil {
		return nil, m.err
	}
	return &llm.Response{JSON: m.json}, nil
}

var testCategories = []Category{
	{ID: "cat-groceries", Name: "Groceries"},
	{ID: "cat-transport", Name: "Transport"},
}

var testItems = []Item{
	{Description: "Wholefoods Market", Amount: "-84.20"},
	{Description: "", Amount: "-3.50"},
}

func TestRunHappy(t *testing.T) {
	mock := &mockCategorizer{json: json.RawMessage(`
		[
			{"i": 1, "category": "Groceries", "confidence": 0.9, "rationale": "grocery store"},
			{"i": 2, "category": "Transport", "confidence": 0.55, "rationale": "likely fare"}
		]`)}
	got, err := Run(context.Background(), mock, testCategories, testItems)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	want1 := Suggestion{I: 1, CategoryID: "cat-groceries", Category: "Groceries", Confidence: 0.9, Rationale: "grocery store"}
	if got[1] != want1 {
		t.Errorf("got[1] = %+v, want %+v", got[1], want1)
	}
	want2 := Suggestion{I: 2, CategoryID: "cat-transport", Category: "Transport", Confidence: 0.55, Rationale: "likely fare"}
	if got[2] != want2 {
		t.Errorf("got[2] = %+v, want %+v", got[2], want2)
	}
	// The provider must have received the pinned prompts.
	if mock.req.System != SystemPrompt(testCategories) {
		t.Errorf("System prompt not passed through:\n%s", mock.req.System)
	}
	if mock.req.User != UserPrompt(testItems) {
		t.Errorf("User prompt not passed through:\n%s", mock.req.User)
	}
}

func TestRunConfidenceBoundary(t *testing.T) {
	cases := []struct {
		conf float64
		want bool
	}{
		{0.6, true},
		{0.75, false}, // exactly at the threshold: NOT review (pinned)
		{0.8, false},
	}
	for _, tc := range cases {
		s := Suggestion{Confidence: tc.conf}
		if got := s.NeedsReview(DefaultThreshold); got != tc.want {
			t.Errorf("NeedsReview(%v) for confidence %v = %v, want %v",
				DefaultThreshold, tc.conf, got, tc.want)
		}
	}
}

func TestRunUnknownCategory(t *testing.T) {
	mock := &mockCategorizer{json: json.RawMessage(`
		[
			{"i": 1, "category": "Betting", "confidence": 0.9, "rationale": "invented category"},
			{"i": 2, "category": "Transport", "confidence": 0.6}
		]`)}
	got, err := Run(context.Background(), mock, testCategories, testItems)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, ok := got[1]; ok {
		t.Errorf("item 1 should be skipped for unknown category, got %+v", got[1])
	}
	if _, ok := got[2]; !ok {
		t.Fatalf("item 2 missing: %+v", got)
	}
	if got[2].CategoryID != "cat-transport" || got[2].Category != "Transport" {
		t.Errorf("got[2] = %+v, want Transport suggestion", got[2])
	}
}

func TestRunMissingEntry(t *testing.T) {
	mock := &mockCategorizer{json: json.RawMessage(
		`[{"i": 1, "category": "Groceries", "confidence": 0.9}]`)}
	got, err := Run(context.Background(), mock, testCategories, testItems)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if _, ok := got[2]; ok {
		t.Errorf("item 2 should be absent when the model omits it")
	}
	if got[1].Category != "Groceries" {
		t.Errorf("got[1] = %+v, want Groceries", got[1])
	}
}

func TestRunClampsConfidence(t *testing.T) {
	mock := &mockCategorizer{json: json.RawMessage(`
		[
			{"i": 1, "category": "Groceries", "confidence": 1.5},
			{"i": 2, "category": "Transport", "confidence": -0.2}
		]`)}
	got, err := Run(context.Background(), mock, testCategories, testItems)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got[1].Confidence != 1.0 {
		t.Errorf("confidence 1.5 clamped to %v, want 1.0", got[1].Confidence)
	}
	if got[2].Confidence != 0.0 {
		t.Errorf("confidence -0.2 clamped to %v, want 0.0", got[2].Confidence)
	}
}

func TestRunDuplicateI(t *testing.T) {
	mock := &mockCategorizer{json: json.RawMessage(`
		[
			{"i": 1, "category": "Groceries", "confidence": 0.2, "rationale": "first entry"},
			{"i": 1, "category": "Transport", "confidence": 0.9, "rationale": "second entry"}
		]`)}
	got, err := Run(context.Background(), mock, testCategories, testItems)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	want := Suggestion{I: 1, CategoryID: "cat-groceries", Category: "Groceries", Confidence: 0.2, Rationale: "first entry"}
	if got[1] != want {
		t.Errorf("got[1] = %+v, want first entry %+v", got[1], want)
	}
}

func TestRunOutOfRangeI(t *testing.T) {
	mock := &mockCategorizer{json: json.RawMessage(`
		[
			{"i": 0, "category": "Groceries", "confidence": 0.9},
			{"i": 3, "category": "Groceries", "confidence": 0.9},
			{"i": 2, "category": "Transport", "confidence": 0.7}
		]`)}
	got, err := Run(context.Background(), mock, testCategories, testItems)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1 (only i=2 in range)", len(got))
	}
	if got[2].CategoryID != "cat-transport" {
		t.Errorf("got[2] = %+v, want Transport", got[2])
	}
}

func TestRunCaseInsensitive(t *testing.T) {
	mock := &mockCategorizer{json: json.RawMessage(`
		[{"i": 1, "category": "groceries", "confidence": 0.9}]`)}
	got, err := Run(context.Background(), mock, testCategories, testItems)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := Suggestion{I: 1, CategoryID: "cat-groceries", Category: "Groceries", Confidence: 0.9}
	if got[1] != want {
		t.Errorf("got[1] = %+v, want canonical %+v", got[1], want)
	}
}

func TestRunProviderError(t *testing.T) {
	boom := errors.New("provider exploded")
	mock := &mockCategorizer{err: boom}
	_, err := Run(context.Background(), mock, testCategories, testItems)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want provider error to propagate unchanged", err)
	}
}

func TestRunEmptyInputs(t *testing.T) {
	t.Run("no categories", func(t *testing.T) {
		mock := &mockCategorizer{json: json.RawMessage(`[]`)}
		_, err := Run(context.Background(), mock, nil, testItems)
		if !errors.Is(err, errNoCategories) {
			t.Fatalf("err = %v, want %v", err, errNoCategories)
		}
		if mock.calls != 0 {
			t.Errorf("provider called %d times, want 0 (fail fast)", mock.calls)
		}
	})
	t.Run("no items", func(t *testing.T) {
		mock := &mockCategorizer{json: json.RawMessage(`[]`)}
		_, err := Run(context.Background(), mock, testCategories, nil)
		if !errors.Is(err, errNoItems) {
			t.Fatalf("err = %v, want %v", err, errNoItems)
		}
		if mock.calls != 0 {
			t.Errorf("provider called %d times, want 0 (fail fast)", mock.calls)
		}
	})
}

func TestPrompts(t *testing.T) {
	wantSystem := "You are a transaction categoriser for a personal finance app.\n" +
		"Assign each numbered transaction to exactly one of the categories below.\n" +
		"\nCategories:\n" +
		"1. Groceries\n" +
		"2. Transport\n" +
		"\nRules:\n" +
		"- Choose only from the listed categories; never invent a new one.\n" +
		"- \"confidence\" is your certainty in [0, 1]; be calibrated -- 1.0 only when unambiguous.\n" +
		"- If no listed category fits well, choose the closest one and give it low confidence.\n" +
		"\nRespond with ONLY a JSON array -- no prose, no markdown -- one object per transaction, in the given order:\n" +
		"[{\"i\": 1, \"category\": \"<the category name exactly as listed>\", \"confidence\": 0.9, \"rationale\": \"<one short sentence>\"}]\n"
	gotSystem := SystemPrompt(testCategories)
	if gotSystem != wantSystem {
		t.Errorf("SystemPrompt mismatch:\n--- got ---\n%s\n--- want ---\n%s", gotSystem, wantSystem)
	}

	items := []Item{
		{Description: "Wholefoods Market", Amount: "-84.20"},
		{Description: "", Amount: "-3.50"},
		{Description: "Coffee", Amount: ""},
	}
	wantUser := "Transactions:\n" +
		"1. Wholefoods Market (amount: -84.20)\n" +
		"2. (no description) (amount: -3.50)\n" +
		"3. Coffee\n"
	gotUser := UserPrompt(items)
	if gotUser != wantUser {
		t.Errorf("UserPrompt mismatch:\n--- got ---\n%s\n--- want ---\n%s", gotUser, wantUser)
	}
}

func TestRunMalformedJSON(t *testing.T) {
	mock := &mockCategorizer{json: json.RawMessage(`not json`)}
	_, err := Run(context.Background(), mock, testCategories, testItems)
	if err == nil {
		t.Fatal("Run should error on a non-array JSON value")
	}
	// Defense in depth: an object response gets the pinned error.
	mockObj := &mockCategorizer{json: json.RawMessage(`{"i": 1}`)}
	_, err = Run(context.Background(), mockObj, testCategories, testItems)
	if !errors.Is(err, errNotArray) {
		t.Fatalf("err = %v, want %v", err, errNotArray)
	}
}

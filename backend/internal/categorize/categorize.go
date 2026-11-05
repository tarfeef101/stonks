package categorize

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"finance-tracker/internal/llm"
)

// Category is one of the user's categories as passed in by the
// caller. Name is already decrypted (repo.CategoryStore decrypts it);
// this package only consumes plain names and resolves the ID from a
// model answer.
type Category struct {
	ID   string
	Name string
}

// Item is one transaction to categorise. Description may be ""
// (rendered as "(no description)"); Amount is the canonical string and
// may be "" (then omitted from the prompt line).
type Item struct {
	Description string
	Amount      string
}

// Suggestion is one model answer, validated and normalised: I is the
// 1-based item index; Category is the CANONICAL listed name (not the
// model's casing); CategoryID is resolved from the match; Confidence
// is clamped into [0,1]; Rationale is "" when absent.
type Suggestion struct {
	I          int
	CategoryID string
	Category   string
	Confidence float64
	Rationale  string
}

// DefaultThreshold is the low-confidence cut-off: a suggestion whose
// confidence is strictly below it needs review. Exactly 0.75 is NOT
// review (pinned by TestRunConfidenceBoundary).
const DefaultThreshold = 0.75

// NeedsReview reports whether the suggestion's confidence is strictly
// below threshold.
func (s Suggestion) NeedsReview(threshold float64) bool {
	return s.Confidence < threshold
}

var (
	// errNoCategories and errNoItems are fail-fast: Run refuses to
	// call the provider without them.
	errNoCategories = errors.New("categorize: no categories provided")
	errNoItems      = errors.New("categorize: no items to categorise")
	// errNotArray is returned when the provider's JSON value is not
	// an array at all (e.g. an object).
	errNotArray = errors.New("categorize: response is not a JSON array")
)

// entry is one object of the model's JSON array, decoded with pointers
// so absent fields are distinguishable from zero values.
type entry struct {
	I          *int     `json:"i"`
	Category   *string  `json:"category"`
	Confidence *float64 `json:"confidence"`
	Rationale  *string  `json:"rationale"`
}

// Run categorises items with one provider call.
//
// Contract (pinned by the golden tests):
//   - Fails fast before any provider call: errNoCategories / errNoItems.
//   - Provider errors propagate unchanged.
//   - The answer must be a JSON array (anything else -> errNotArray;
//     invalid JSON -> error).
//   - Per-entry validation: i present, int, within [1..N]; category
//     present, non-empty after trim, matching a listed name
//     case-insensitively; confidence present and finite. Out-of-range i,
//     unknown category, non-finite confidence, wrong types -> the entry
//     is SKIPPED; the batch never fails for one bad entry (the caller
//     marks such items for review, plan 5.4).
//   - Confidence is clamped to [0,1]; duplicate i -> FIRST entry wins.
//
// Returns a map keyed by 1-based item index; an absent key means no
// suggestion for that item.
func Run(ctx context.Context, c llm.Categorizer, categories []Category, items []Item) (map[int]Suggestion, error) {
	if len(categories) == 0 {
		return nil, errNoCategories
	}
	if len(items) == 0 {
		return nil, errNoItems
	}
	resp, err := c.Categorize(ctx, llm.Request{
		System: SystemPrompt(categories),
		User:   UserPrompt(items),
	})
	if err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(resp.JSON)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, errNotArray
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(resp.JSON, &raws); err != nil {
		return nil, fmt.Errorf("categorize: invalid JSON: %w", err)
	}
	byName := make(map[string]Category, len(categories))
	for _, cat := range categories {
		byName[strings.ToLower(strings.TrimSpace(cat.Name))] = cat
	}
	out := make(map[int]Suggestion, len(items))
	for _, raw := range raws {
		var e entry
		if err := json.Unmarshal(raw, &e); err != nil {
			continue // wrong shape for this entry: skip it
		}
		if e.I == nil || e.Category == nil || e.Confidence == nil {
			continue
		}
		i := *e.I
		if i < 1 || i > len(items) {
			continue
		}
		name := strings.TrimSpace(*e.Category)
		if name == "" {
			continue
		}
		cat, ok := byName[strings.ToLower(name)]
		if !ok {
			continue
		}
		conf := *e.Confidence
		if math.IsNaN(conf) || math.IsInf(conf, 0) {
			continue
		}
		if conf < 0 {
			conf = 0
		} else if conf > 1 {
			conf = 1
		}
		if _, dup := out[i]; dup {
			continue // first entry wins
		}
		rationale := ""
		if e.Rationale != nil {
			rationale = *e.Rationale
		}
		out[i] = Suggestion{
			I:          i,
			CategoryID: cat.ID,
			Category:   cat.Name,
			Confidence: conf,
			Rationale:  rationale,
		}
	}
	return out, nil
}

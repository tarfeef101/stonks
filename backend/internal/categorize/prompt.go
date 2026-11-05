// Package categorize turns a model's answer into validated per-item
// category suggestions (plan 5.2). It owns the pinned prompt formats
// (SystemPrompt / UserPrompt), the shape of the model's JSON-array
// answer, and the low-confidence policy (DefaultThreshold /
// NeedsReview). It does not own the LLM transport (that is
// llm.Categorizer, plan 5.1) and does not fetch categories: the caller
// (plan 5.4) passes decrypted category names and plain item fields.
package categorize

import (
	"fmt"
	"strings"
)

// SystemPrompt renders the pinned system message: numbered category
// names (categories have no description column) plus the JSON-array
// answer contract. The exact text is pinned by TestPrompts.
func SystemPrompt(categories []Category) string {
	var b strings.Builder
	b.WriteString("You are a transaction categoriser for a personal finance app.\n")
	b.WriteString("Assign each numbered transaction to exactly one of the categories below.\n")
	b.WriteString("\nCategories:\n")
	for i, c := range categories {
		fmt.Fprintf(&b, "%d. %s\n", i+1, c.Name)
	}
	b.WriteString("\nRules:\n")
	b.WriteString("- Choose only from the listed categories; never invent a new one.\n")
	b.WriteString("- \"confidence\" is your certainty in [0, 1]; be calibrated -- 1.0 only when unambiguous.\n")
	b.WriteString("- If no listed category fits well, choose the closest one and give it low confidence.\n")
	b.WriteString("\nRespond with ONLY a JSON array -- no prose, no markdown -- one object per transaction, in the given order:\n")
	b.WriteString("[{\"i\": 1, \"category\": \"<the category name exactly as listed>\", \"confidence\": 0.9, \"rationale\": \"<one short sentence>\"}]\n")
	return b.String()
}

// UserPrompt renders the pinned user message: one line per item,
// 1-based. An empty description becomes "(no description)"; a
// non-empty canonical amount is appended as " (amount: <amount>)".
func UserPrompt(items []Item) string {
	var b strings.Builder
	b.WriteString("Transactions:\n")
	for i, it := range items {
		desc := it.Description
		if desc == "" {
			desc = "(no description)"
		}
		line := fmt.Sprintf("%d. %s", i+1, desc)
		if it.Amount != "" {
			line += fmt.Sprintf(" (amount: %s)", it.Amount)
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

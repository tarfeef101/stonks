package llm

import (
	"encoding/json"
	"fmt"
	"strings"
)

// extractJSON pulls a single JSON value out of raw model output and
// returns it COMPACTED (re-marshaled), so stored/forwarded JSON is
// canonical. Strategy: (1) strip a markdown fence, (2) parse the
// whole text, (3) fallback to the first { or [ (whichever comes
// first) through the last } or ] of the same family, (4) error.
func extractJSON(s string) (json.RawMessage, error) {
	t := stripFence(s)

	// Fast path: the whole (fence-stripped) text is one JSON value.
	if b, err := parseCompact(t); err == nil {
		return b, nil
	}

	// Fallback: slice from the first { or [ to the last matching
	// family closer, and validate that slice.
	openIdx := strings.IndexAny(t, "{[")
	if openIdx < 0 {
		return nil, fmt.Errorf("llm: no JSON value found in model output")
	}
	closer := byte('}')
	if t[openIdx] == '[' {
		closer = ']'
	}
	closeIdx := strings.LastIndexByte(t, closer)
	if closeIdx <= openIdx {
		return nil, fmt.Errorf("llm: no JSON value found in model output")
	}
	return parseCompact(t[openIdx : closeIdx+1])
}

// parseCompact validates s as a single JSON value and returns it
// re-marshaled (compact, canonical). Surrounded prose, trailing
// junk, or invalid JSON is rejected.
func parseCompact(s string) (json.RawMessage, error) {
	var v any
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &v); err != nil {
		return nil, err
	}
	compact, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return compact, nil
}

// stripFence removes a surrounding markdown code fence ("```json" or
// bare "```") when the whole text is one fenced block. Non-fenced
// text is returned trimmed, unchanged.
func stripFence(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") {
		return t
	}
	rest := t[3:]
	nl := strings.IndexByte(rest, '\n')
	if nl < 0 {
		return t // no newline after the opening fence: not a block
	}
	rest = rest[nl+1:]
	trimmed := strings.TrimRight(rest, " \t\r\n")
	if !strings.HasSuffix(trimmed, "```") {
		return t // no closing fence: not a block
	}
	return strings.TrimSpace(trimmed[:len(trimmed)-3])
}

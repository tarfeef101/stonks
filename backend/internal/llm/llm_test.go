package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// recorded captures one request the test server saw.
type recorded struct {
	path   string
	header http.Header
	raw    []byte
}

// newServer returns an httptest server that records every request
// into recs, then answers via fn.
func newServer(t *testing.T, recs *[]recorded, fn func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		*recs = append(*recs, recorded{path: r.URL.Path, header: r.Header.Clone(), raw: raw})
		fn(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// respondOK writes a 200 chat-completions response whose single
// choice carries content.
func respondOK(w http.ResponseWriter, model, content string, promptTokens, completionTokens int) {
	w.Header().Set("Content-Type", "application/json")
	body := fmt.Sprintf(`{"model":%q,"choices":[{"message":{"role":"assistant","content":%q}}],"usage":{"prompt_tokens":%d,"completion_tokens":%d}}`,
		model, content, promptTokens, completionTokens)
	_, _ = io.WriteString(w, body)
}

// reqBody unmarshals a recorded request body into a generic map.
func reqBody(t *testing.T, rec recorded) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.raw, &m); err != nil {
		t.Fatalf("request body is not JSON: %v\n%s", err, rec.raw)
	}
	return m
}

// userContent returns the content of the LAST (user) message.
func userContent(t *testing.T, m map[string]any) string {
	t.Helper()
	msgs, ok := m["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatalf("no messages in request body: %v", m)
	}
	last, ok := msgs[len(msgs)-1].(map[string]any)
	if !ok {
		t.Fatalf("last message is not an object: %v", m)
	}
	s, _ := last["content"].(string)
	return s
}

// 1. Happy path: valid JSON content comes back compacted, model and
// usage are parsed, and the request carries the pinned
// temperature/max_tokens, the right messages, and a Bearer key.
func TestCategorize_HappyPath(t *testing.T) {
	var recs []recorded
	srv := newServer(t, &recs, func(w http.ResponseWriter, r *http.Request) {
		respondOK(w, "test-model", `  {"i": 1,  "category": "Groceries", "confidence": 0.9}  `, 12, 34)
	})
	p := New(srv.URL+"/v1", "sk-test", "test-model")
	res, err := p.Categorize(context.Background(), Request{System: "You are a categorizer.", User: "Categorize: milk $4"})
	if err != nil {
		t.Fatalf("Categorize: %v", err)
	}
	if want := `{"category":"Groceries","confidence":0.9,"i":1}`; string(res.JSON) != want {
		t.Errorf("JSON = %s, want %s", res.JSON, want)
	}
	if res.Model != "test-model" {
		t.Errorf("Model = %q, want test-model", res.Model)
	}
	if res.PromptTokens != 12 || res.CompletionTokens != 34 {
		t.Errorf("tokens = %d/%d, want 12/34", res.PromptTokens, res.CompletionTokens)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d requests, want 1", len(recs))
	}
	rec := recs[0]
	if got := rec.header.Get("Authorization"); got != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want Bearer sk-test", got)
	}
	m := reqBody(t, rec)
	if temp, _ := m["temperature"].(float64); temp != 0 {
		t.Errorf("temperature = %v, want 0", m["temperature"])
	}
	if mt, _ := m["max_tokens"].(float64); mt != 1024 {
		t.Errorf("max_tokens = %v, want 1024 (default)", m["max_tokens"])
	}
	if model, _ := m["model"].(string); model != "test-model" {
		t.Errorf("model = %q, want test-model", model)
	}
	msgs, _ := m["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "You are a categorizer." {
		t.Errorf("first message = %v, want system prompt", first)
	}
	last, _ := msgs[1].(map[string]any)
	if last["role"] != "user" || last["content"] != "Categorize: milk $4" {
		t.Errorf("last message = %v, want user prompt", last)
	}
}

// 2. Keyless provider (self-hosted llama.cpp) sends NO
// Authorization header.
func TestCategorize_NoAPIKeySendsNoAuthorization(t *testing.T) {
	var recs []recorded
	srv := newServer(t, &recs, func(w http.ResponseWriter, r *http.Request) {
		respondOK(w, "llama3", `{"i":1,"category":"Dining"}`, 3, 5)
	})
	p := New(srv.URL+"/v1", "", "llama3")
	if _, err := p.Categorize(context.Background(), Request{User: "coffee $3"}); err != nil {
		t.Fatalf("Categorize: %v", err)
	}
	if got := recs[0].header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want absent", got)
	}
	if got := recs[0].header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

// 3. Markdown-fenced JSON (```json or bare ```) is stripped and
// parsed.
func TestCategorize_FencedJSON(t *testing.T) {
	for _, content := range []string{
		"```json\n{\"i\": 2, \"category\": \"Transport\"}\n```",
		"```\n{\"i\": 2, \"category\": \"Transport\"}\n```",
	} {
		var recs []recorded
		srv := newServer(t, &recs, func(w http.ResponseWriter, r *http.Request) {
			respondOK(w, "m", content, 1, 1)
		})
		p := New(srv.URL+"/v1", "", "m")
		res, err := p.Categorize(context.Background(), Request{User: "bus ticket $2"})
		if err != nil {
			t.Fatalf("Categorize(%q): %v", content, err)
		}
		if want := `{"category":"Transport","i":2}`; string(res.JSON) != want {
			t.Errorf("JSON = %s, want %s", res.JSON, want)
		}
		if len(recs) != 1 {
			t.Errorf("got %d requests, want 1", len(recs))
		}
	}
}

// 4. JSON wrapped in prose is recovered by bracket extraction.
func TestCategorize_ProseWrappedJSON(t *testing.T) {
	var recs []recorded
	srv := newServer(t, &recs, func(w http.ResponseWriter, r *http.Request) {
		respondOK(w, "m", `Here you go: {"i": 3, "category": "Utilities"} — hope that helps!`, 1, 1)
	})
	p := New(srv.URL+"/v1", "", "m")
	res, err := p.Categorize(context.Background(), Request{User: "electric bill $80"})
	if err != nil {
		t.Fatalf("Categorize: %v", err)
	}
	if want := `{"category":"Utilities","i":3}`; string(res.JSON) != want {
		t.Errorf("JSON = %s, want %s", res.JSON, want)
	}
	if len(recs) != 1 {
		t.Errorf("got %d requests, want 1", len(recs))
	}
}

// 5. Malformed first attempt -> exactly ONE nudge retry succeeds.
func TestCategorize_MalformedThenGood(t *testing.T) {
	var recs []recorded
	srv := newServer(t, &recs, func(w http.ResponseWriter, r *http.Request) {
		if len(recs) == 1 { // first call
			respondOK(w, "m", "I think the category is groceries", 1, 1)
			return
		}
		respondOK(w, "m", `{"i": 1, "category": "Groceries"}`, 1, 1)
	})
	p := New(srv.URL+"/v1", "", "m")
	res, err := p.Categorize(context.Background(), Request{User: "milk $4"})
	if err != nil {
		t.Fatalf("Categorize: %v", err)
	}
	if want := `{"category":"Groceries","i":1}`; string(res.JSON) != want {
		t.Errorf("JSON = %s, want %s", res.JSON, want)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d requests, want exactly 2", len(recs))
	}
	second := userContent(t, reqBody(t, recs[1]))
	if !strings.Contains(second, "not valid JSON") {
		t.Errorf("2nd user message %q does not contain the format nudge", second)
	}
	if !strings.HasPrefix(second, "milk $4\n\nYour previous response was not valid JSON.") {
		t.Errorf("2nd user message = %q, want original prompt + exact nudge suffix", second)
	}
}

// 6. Malformed twice -> error naming 2 attempts; exactly 2 requests.
func TestCategorize_MalformedTwice(t *testing.T) {
	var recs []recorded
	srv := newServer(t, &recs, func(w http.ResponseWriter, r *http.Request) {
		respondOK(w, "m", "sorry, I cannot produce JSON", 1, 1)
	})
	p := New(srv.URL+"/v1", "", "m")
	_, err := p.Categorize(context.Background(), Request{User: "milk $4"})
	if err == nil {
		t.Fatal("want error after two malformed attempts")
	}
	if !strings.Contains(err.Error(), "after 2 attempts") {
		t.Errorf("error = %q, want it to contain %q", err, "after 2 attempts")
	}
	if !strings.Contains(err.Error(), "cannot produce JSON") {
		t.Errorf("error = %q, want it to quote the last raw content", err)
	}
	if len(recs) != 2 {
		t.Errorf("got %d requests, want exactly 2", len(recs))
	}
}

// 7. HTTP 500 -> error carries the status; NO retry.
func TestCategorize_HTTPErrorNoRetry(t *testing.T) {
	var recs []recorded
	srv := newServer(t, &recs, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal explosion", http.StatusInternalServerError)
	})
	p := New(srv.URL+"/v1", "", "m")
	_, err := p.Categorize(context.Background(), Request{User: "milk $4"})
	if err == nil {
		t.Fatal("want error on HTTP 500")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error = %q, want it to contain status 500", err)
	}
	if len(recs) != 1 {
		t.Errorf("got %d requests, want 1 (no retry on HTTP errors)", len(recs))
	}
}

// 8. 200 with a non-JSON body -> error; NO retry.
func TestCategorize_NonJSONBody(t *testing.T) {
	var recs []recorded
	srv := newServer(t, &recs, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html>bad gateway</html>")
	})
	p := New(srv.URL+"/v1", "", "m")
	_, err := p.Categorize(context.Background(), Request{User: "milk $4"})
	if err == nil {
		t.Fatal("want error on non-JSON response body")
	}
	if len(recs) != 1 {
		t.Errorf("got %d requests, want 1", len(recs))
	}
}

// 9. 200 with valid JSON body but empty choices -> "no content".
func TestCategorize_EmptyChoices(t *testing.T) {
	var recs []recorded
	srv := newServer(t, &recs, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"m","choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0}}`)
	})
	p := New(srv.URL+"/v1", "", "m")
	_, err := p.Categorize(context.Background(), Request{User: "milk $4"})
	if err == nil {
		t.Fatal("want error on empty choices")
	}
	if !strings.Contains(err.Error(), "no content") {
		t.Errorf("error = %q, want it to contain %q", err, "no content")
	}
	if len(recs) != 1 {
		t.Errorf("got %d requests, want 1", len(recs))
	}
}

// 10. Per-call timeout: the attempt is cut off at 50ms.
func TestCategorize_PerAttemptTimeout(t *testing.T) {
	var recs []recorded
	srv := newServer(t, &recs, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		respondOK(w, "m", `{"i":1}`, 1, 1)
	})
	p := New(srv.URL+"/v1", "", "m", WithTimeout(50*time.Millisecond))
	start := time.Now()
	_, err := p.Categorize(context.Background(), Request{User: "milk $4"})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("want timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed >= 2*time.Second {
		t.Errorf("Categorize took %s, want < 2s", elapsed)
	}
	if len(recs) != 1 {
		t.Errorf("got %d requests, want 1 (no retry on timeout)", len(recs))
	}
}

// 11. URL construction: baseURL with or without a trailing slash
// both hit exactly /v1/chat/completions.
func TestCategorize_URLConstruction(t *testing.T) {
	for _, base := range []string{"/v1", "/v1/"} {
		var recs []recorded
		srv := newServer(t, &recs, func(w http.ResponseWriter, r *http.Request) {
			respondOK(w, "m", `{"i":1}`, 1, 1)
		})
		p := New(srv.URL+base, "", "m")
		if _, err := p.Categorize(context.Background(), Request{User: "milk $4"}); err != nil {
			t.Fatalf("Categorize(base %q): %v", base, err)
		}
		if len(recs) != 1 {
			t.Fatalf("base %q: got %d requests, want 1", base, len(recs))
		}
		if recs[0].path != "/v1/chat/completions" {
			t.Errorf("base %q: server saw path %q, want /v1/chat/completions", base, recs[0].path)
		}
	}
}

// 12. Parent ctx cancelled preflight -> immediate error, no request
// (at most one).
func TestCategorize_ContextCanceled(t *testing.T) {
	var recs []recorded
	srv := newServer(t, &recs, func(w http.ResponseWriter, r *http.Request) {
		respondOK(w, "m", `{"i":1}`, 1, 1)
	})
	p := New(srv.URL+"/v1", "", "m")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := p.Categorize(ctx, Request{User: "milk $4"})
	if err == nil {
		t.Fatal("want error on canceled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed >= 2*time.Second {
		t.Errorf("Categorize took %s, want immediate", elapsed)
	}
	if len(recs) > 1 {
		t.Errorf("got %d requests, want 0 or 1", len(recs))
	}
}

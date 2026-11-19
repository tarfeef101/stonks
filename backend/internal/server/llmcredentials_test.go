// llmcredentials_test.go is the route-level tests for the LLM
// credentials (plan step 5.6): the list, the per-provider upsert and
// delete, and the test-connection probe. All in-memory:
// memCredStore mirrors the repo.CredentialStore semantics the handlers
// depend on (caller scoping, the upsert timestamp rules, the repo's
// validation and masking, ErrCredentialNotFound on delete) and injects
// per-operation errors for the mapping table; credTestStub is a full
// llm.Response provider (model echo + usage) for the test route.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"finance-tracker/internal/categorize"
	"finance-tracker/internal/llm"
	"finance-tracker/internal/repo"

	"github.com/lestrrat-go/jwx/v3/jwk"
)

// maskKeyTest mirrors repo.maskKey (the repo's copy is unexported):
// at most 8 runes fully masked, longer keys keep first4 + "…" + last4.
func maskKeyTest(key string) string {
	runes := []rune(key)
	if len(runes) <= 8 {
		return "****"
	}
	return string(runes[:4]) + "…" + string(runes[len(runes)-4:])
}

// memCredStore is an in-memory LLMCredentialStore fake. The rows are
// user-scoped maps (provider -> row); Put validates with
// checkCredentialIn (the handler's mirror of repo.validateCredentialIn)
// and trims the endpoint, the repo's rules. now is injectable so the
// upsert timestamp assertions are deterministic.
type memCredStore struct {
	rows      map[string]map[string]repo.LLMCredential // user id -> provider -> row
	now       func() time.Time
	listErr   error
	putErr    error
	deleteErr error
}

func newMemCredStore() *memCredStore {
	return &memCredStore{
		rows: map[string]map[string]repo.LLMCredential{},
		now:  func() time.Time { return time.Now().UTC() },
	}
}

func (s *memCredStore) Put(_ context.Context, userID string, in repo.CredentialIn, _ []byte) (repo.LLMCredential, error) {
	if s.putErr != nil {
		return repo.LLMCredential{}, s.putErr
	}
	if err := checkCredentialIn(in.Provider, in.Endpoint, in.APIKey); err != nil {
		return repo.LLMCredential{}, err
	}
	if s.rows[userID] == nil {
		s.rows[userID] = map[string]repo.LLMCredential{}
	}
	now := s.now()
	created := now
	if prev, ok := s.rows[userID][in.Provider]; ok {
		created = prev.CreatedAt
	}
	masked := ""
	if in.APIKey != nil && *in.APIKey != "" {
		masked = maskKeyTest(*in.APIKey)
	}
	row := repo.LLMCredential{
		Provider:     in.Provider,
		Endpoint:     strings.TrimSpace(in.Endpoint),
		APIKeyMasked: masked,
		CreatedAt:    created,
		UpdatedAt:    now,
	}
	s.rows[userID][in.Provider] = row
	return row, nil
}

func (s *memCredStore) List(_ context.Context, userID string, _ []byte) ([]repo.LLMCredential, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	// The repo orders by provider (SQL text order); iterate the
	// vocabulary in the same order.
	out := make([]repo.LLMCredential, 0, 3)
	for _, p := range []string{"custom", "openai", "self_hosted"} {
		if row, ok := s.rows[userID][p]; ok {
			out = append(out, row)
		}
	}
	return out, nil
}

func (s *memCredStore) Delete(_ context.Context, userID, provider string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	if _, ok := s.rows[userID][provider]; !ok {
		return repo.ErrCredentialNotFound
	}
	delete(s.rows[userID], provider)
	return nil
}

// credTestStub is an llm.Categorizer that answers with a canned FULL
// response (the model echo and usage the test route reports, unlike
// review_test.go's stubCategorizer which carries JSON only) and records
// every request it is sent. Pointer receiver: the factory hands out the
// same instance the test asserts on.
type credTestStub struct {
	resp *llm.Response
	err  error
	reqs []llm.Request
}

func (c *credTestStub) Categorize(_ context.Context, req llm.Request) (*llm.Response, error) {
	// The in-memory stub would otherwise answer in sub-millisecond
	// time and the route's latency_ms would round to 0; a small
	// sleep keeps the latency assertion deterministic. The real
	// probe latency is covered by the e2e run.
	time.Sleep(5 * time.Millisecond)
	c.reqs = append(c.reqs, req)
	if c.err != nil {
		return nil, c.err
	}
	return c.resp, nil
}

// newCredServer wires New with the live token verifier, the in-memory
// bootstrap, and only the credential wrapper (the other stores stay
// nil — this file never touches their routes). The key is returned so
// tests can mint tokens for several users under the same verifier;
// calls records every (baseURL, apiKey, model) the LLM factory was
// handed (nil factoryErr builds the stub; non-nil makes the factory
// fail).
func newCredServer(t *testing.T, store LLMCredentialStore, stub *credTestStub, factoryErr error) (http.Handler, jwk.Key, *[]llmArgs) {
	t.Helper()
	key := mintKey(t)
	b, err := NewBootstrap(newMemStore(), testKEK(t), nil)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	calls := &[]llmArgs{}
	wrap := &LLMCredentials{
		Store: store,
		Env:   categorize.LLMEnv{Model: "test-env-model"},
		LLM: func(baseURL, apiKey, model string) (llm.Categorizer, error) {
			*calls = append(*calls, llmArgs{baseURL: baseURL, apiKey: apiKey, model: model})
			if factoryErr != nil {
				return nil, factoryErr
			}
			return stub, nil
		},
	}
	return New(fakeReady{}, BuildInfo{}, liveAuth(t, key), b, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, wrap, nil, nil, nil, nil, nil, false, nil, nil, nil, nil, nil, nil, nil), key, calls
}

// credHdr is the Authorization header for a fresh identity token on
// sub (profile claims filled from the sub, the way the other route
// tests do).
func credHdr(t *testing.T, key jwk.Key, sub string) map[string]string {
	t.Helper()
	return authHdr(mintIdentityToken(t, key, sub, sub+"@localhost", sub, sub))
}

// credOut is the wire shape the list/put routes answer.
type credOut struct {
	Provider     string `json:"provider"`
	Endpoint     string `json:"endpoint"`
	APIKeyMasked string `json:"api_key_masked"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

func TestLLMCredList(t *testing.T) {
	key1 := "sk-test-abcde1234567890"
	key2 := "short"
	store := newMemCredStore()
	h, key, _ := newCredServer(t, store, &credTestStub{}, nil)

	// alice: two providers (openai keyed, self_hosted keyless);
	// bob: one (custom keyed).
	if _, err := store.Put(context.Background(), "id-alice", repo.CredentialIn{
		Provider: "openai", Endpoint: "https://api.openai.com/v1", APIKey: &key1,
	}, nil); err != nil {
		t.Fatalf("seed alice openai: %v", err)
	}
	if _, err := store.Put(context.Background(), "id-alice", repo.CredentialIn{
		Provider: "self_hosted", Endpoint: "http://llama:8080/v1",
	}, nil); err != nil {
		t.Fatalf("seed alice self_hosted: %v", err)
	}
	if _, err := store.Put(context.Background(), "id-bob", repo.CredentialIn{
		Provider: "custom", Endpoint: "https://llm.example.com/v1", APIKey: &key2,
	}, nil); err != nil {
		t.Fatalf("seed bob custom: %v", err)
	}

	t.Run("shape and mask pass-through", func(t *testing.T) {
		code, body := doReq(t, h, http.MethodGet, "/api/v1/llm-credentials", "", credHdr(t, key, "alice"))
		if code != http.StatusOK {
			t.Fatalf("GET status = %d, want 200 (body %s)", code, body)
		}
		var rows []credOut
		if err := json.Unmarshal([]byte(body), &rows); err != nil {
			t.Fatalf("body is not a JSON list: %v (%s)", err, body)
		}
		if len(rows) != 2 {
			t.Fatalf("rows = %d, want 2 (body %s)", len(rows), body)
		}
		// Repo order: by provider (custom < openai < self_hosted).
		if rows[0].Provider != "openai" || rows[1].Provider != "self_hosted" {
			t.Fatalf("order = [%s %s], want [openai self_hosted]", rows[0].Provider, rows[1].Provider)
		}
		if rows[0].Endpoint != "https://api.openai.com/v1" {
			t.Errorf("openai endpoint = %q", rows[0].Endpoint)
		}
		if rows[0].APIKeyMasked != maskKeyTest(key1) {
			t.Errorf("openai mask = %q, want %q", rows[0].APIKeyMasked, maskKeyTest(key1))
		}
		if strings.Contains(body, key1) {
			t.Errorf("body contains the full key")
		}
		if rows[1].APIKeyMasked != "" {
			t.Errorf("self_hosted mask = %q, want \"\"", rows[1].APIKeyMasked)
		}
		for _, r := range rows {
			if _, err := time.Parse(time.RFC3339, r.CreatedAt); err != nil {
				t.Errorf("created_at %q is not RFC3339: %v", r.CreatedAt, err)
			}
			if _, err := time.Parse(time.RFC3339, r.UpdatedAt); err != nil {
				t.Errorf("updated_at %q is not RFC3339: %v", r.UpdatedAt, err)
			}
		}
	})

	t.Run("user scoping", func(t *testing.T) {
		code, body := doReq(t, h, http.MethodGet, "/api/v1/llm-credentials", "", credHdr(t, key, "bob"))
		if code != http.StatusOK {
			t.Fatalf("GET status = %d, want 200 (body %s)", code, body)
		}
		var rows []credOut
		if err := json.Unmarshal([]byte(body), &rows); err != nil {
			t.Fatalf("body is not a JSON list: %v (%s)", err, body)
		}
		if len(rows) != 1 || rows[0].Provider != "custom" {
			t.Fatalf("bob rows = %s, want exactly [custom]", body)
		}
		if rows[0].APIKeyMasked != "****" {
			t.Errorf("short-key mask = %q, want \"****\"", rows[0].APIKeyMasked)
		}
	})

	t.Run("empty answers []", func(t *testing.T) {
		code, body := doReq(t, h, http.MethodGet, "/api/v1/llm-credentials", "", credHdr(t, key, "carol"))
		if code != http.StatusOK {
			t.Fatalf("GET status = %d, want 200 (body %s)", code, body)
		}
		if strings.TrimSpace(body) != "[]" {
			t.Fatalf("empty list body = %s, want []", body)
		}
	})

	t.Run("no token", func(t *testing.T) {
		code, _ := doReq(t, h, http.MethodGet, "/api/v1/llm-credentials", "", nil)
		if code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", code)
		}
	})

	t.Run("list error", func(t *testing.T) {
		store.listErr = errors.New("boom")
		defer func() { store.listErr = nil }()
		code, body := doReq(t, h, http.MethodGet, "/api/v1/llm-credentials", "", credHdr(t, key, "alice"))
		if code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 (body %s)", code, body)
		}
		c, _ := apiErr(t, body)
		if c != codeInternal {
			t.Errorf("code = %q, want %q", c, codeInternal)
		}
	})
}

func TestLLMCredPut(t *testing.T) {
	const key1 = "sk-test-put-key-0123456789"
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	t1 := t0.Add(time.Hour)
	store := newMemCredStore()
	store.now = func() time.Time { return t0 }
	h, key, _ := newCredServer(t, store, &credTestStub{}, nil)
	hdr := credHdr(t, key, "alice")

	t.Run("create self_hosted keyless", func(t *testing.T) {
		code, body := doReq(t, h, http.MethodPut, "/api/v1/llm-credentials/self_hosted",
			`{"endpoint":" http://llama:8080/v1 "}`, hdr)
		if code != http.StatusOK {
			t.Fatalf("PUT status = %d, want 200 (body %s)", code, body)
		}
		var row credOut
		if err := json.Unmarshal([]byte(body), &row); err != nil {
			t.Fatalf("body is not a row: %v (%s)", err, body)
		}
		if row.Provider != "self_hosted" || row.Endpoint != "http://llama:8080/v1" {
			t.Fatalf("row = %+v (endpoint must be trimmed)", row)
		}
		if row.APIKeyMasked != "" {
			t.Errorf("mask = %q, want \"\"", row.APIKeyMasked)
		}
		if row.CreatedAt != t0.Format(time.RFC3339) || row.UpdatedAt != t0.Format(time.RFC3339) {
			t.Errorf("timestamps = %s / %s, want %s / %s", row.CreatedAt, row.UpdatedAt,
				t0.Format(time.RFC3339), t0.Format(time.RFC3339))
		}
	})

	t.Run("upsert keeps created_at, moves updated_at", func(t *testing.T) {
		store.now = func() time.Time { return t1 }
		code, body := doReq(t, h, http.MethodPut, "/api/v1/llm-credentials/self_hosted",
			`{"endpoint":"http://llama:8080/v1"}`, hdr)
		if code != http.StatusOK {
			t.Fatalf("PUT status = %d, want 200 (body %s)", code, body)
		}
		var row credOut
		if err := json.Unmarshal([]byte(body), &row); err != nil {
			t.Fatalf("body is not a row: %v (%s)", err, body)
		}
		if row.CreatedAt != t0.Format(time.RFC3339) {
			t.Errorf("created_at = %s, want %s (upsert must keep it)", row.CreatedAt, t0.Format(time.RFC3339))
		}
		if row.UpdatedAt != t1.Format(time.RFC3339) {
			t.Errorf("updated_at = %s, want %s", row.UpdatedAt, t1.Format(time.RFC3339))
		}
	})

	t.Run("create openai with key, masked in the answer", func(t *testing.T) {
		code, body := doReq(t, h, http.MethodPut, "/api/v1/llm-credentials/openai",
			fmt.Sprintf(`{"endpoint":"https://api.openai.com/v1","api_key":%q}`, key1), hdr)
		if code != http.StatusOK {
			t.Fatalf("PUT status = %d, want 200 (body %s)", code, body)
		}
		var row credOut
		if err := json.Unmarshal([]byte(body), &row); err != nil {
			t.Fatalf("body is not a row: %v (%s)", err, body)
		}
		if row.APIKeyMasked != maskKeyTest(key1) {
			t.Errorf("mask = %q, want %q", row.APIKeyMasked, maskKeyTest(key1))
		}
		if strings.Contains(body, key1) {
			t.Errorf("body contains the full key")
		}
	})

	t.Run("openai without key", func(t *testing.T) {
		code, body := doReq(t, h, http.MethodPut, "/api/v1/llm-credentials/openai",
			`{"endpoint":"https://api.openai.com/v1"}`, hdr)
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", code, body)
		}
		c, m := apiErr(t, body)
		if c != codeInvalidRequest || m != "provider openai requires an API key" {
			t.Errorf("envelope = %q / %q", c, m)
		}
	})

	t.Run("unknown path provider", func(t *testing.T) {
		code, body := doReq(t, h, http.MethodPut, "/api/v1/llm-credentials/bogus",
			`{"endpoint":"https://x.example.com/v1","api_key":"k"}`, hdr)
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", code, body)
		}
		c, m := apiErr(t, body)
		if c != codeInvalidRequest || m != "provider must be one of self_hosted, openai, custom" {
			t.Errorf("envelope = %q / %q", c, m)
		}
	})

	t.Run("empty endpoint", func(t *testing.T) {
		code, body := doReq(t, h, http.MethodPut, "/api/v1/llm-credentials/self_hosted",
			`{"endpoint":"   "}`, hdr)
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", code, body)
		}
		c, m := apiErr(t, body)
		if c != codeInvalidRequest || m != "endpoint must not be empty" {
			t.Errorf("envelope = %q / %q", c, m)
		}
	})

	t.Run("endpoint too long", func(t *testing.T) {
		long := strings.Repeat("e", 2049)
		code, body := doReq(t, h, http.MethodPut, "/api/v1/llm-credentials/self_hosted",
			fmt.Sprintf(`{"endpoint":%q}`, long), hdr)
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", code, body)
		}
		c, m := apiErr(t, body)
		if c != codeInvalidRequest || m != "endpoint must be at most 2048 characters" {
			t.Errorf("envelope = %q / %q", c, m)
		}
	})

	t.Run("malformed body", func(t *testing.T) {
		code, body := doReq(t, h, http.MethodPut, "/api/v1/llm-credentials/self_hosted",
			`{nope`, hdr)
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", code, body)
		}
		if c, _ := apiErr(t, body); c != codeInvalidRequest {
			t.Errorf("code = %q, want %q", c, codeInvalidRequest)
		}
	})

	t.Run("no token", func(t *testing.T) {
		code, _ := doReq(t, h, http.MethodPut, "/api/v1/llm-credentials/self_hosted",
			`{"endpoint":"http://x"}`, nil)
		if code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", code)
		}
	})

	t.Run("put error", func(t *testing.T) {
		store.putErr = errors.New("boom")
		defer func() { store.putErr = nil }()
		code, body := doReq(t, h, http.MethodPut, "/api/v1/llm-credentials/self_hosted",
			`{"endpoint":"http://x"}`, hdr)
		if code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 (body %s)", code, body)
		}
		if c, _ := apiErr(t, body); c != codeInternal {
			t.Errorf("code = %q, want %q", c, codeInternal)
		}
	})
}

func TestLLMCredDelete(t *testing.T) {
	store := newMemCredStore()
	h, key, _ := newCredServer(t, store, &credTestStub{}, nil)
	alice, bob := credHdr(t, key, "alice"), credHdr(t, key, "bob")

	seed := func(sub, provider, endpoint, apiKey string) {
		t.Helper()
		var k *string
		if apiKey != "" {
			k = &apiKey
		}
		if _, err := store.Put(context.Background(), "id-"+sub, repo.CredentialIn{
			Provider: provider, Endpoint: endpoint, APIKey: k,
		}, nil); err != nil {
			t.Fatalf("seed %s %s: %v", sub, provider, err)
		}
	}
	seed("alice", "openai", "https://api.openai.com/v1", "sk-test-del-key")

	t.Run("delete stored", func(t *testing.T) {
		code, _ := doReq(t, h, http.MethodDelete, "/api/v1/llm-credentials/openai", "", alice)
		if code != http.StatusNoContent {
			t.Fatalf("DELETE status = %d, want 204", code)
		}
		code, body := doReq(t, h, http.MethodGet, "/api/v1/llm-credentials", "", alice)
		if code != http.StatusOK || strings.TrimSpace(body) != "[]" {
			t.Fatalf("after delete: status = %d body = %s, want 200 []", code, body)
		}
	})

	t.Run("delete again is 404", func(t *testing.T) {
		code, body := doReq(t, h, http.MethodDelete, "/api/v1/llm-credentials/openai", "", alice)
		if code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body %s)", code, body)
		}
		c, m := apiErr(t, body)
		if c != codeNotFound || m != "credential not found" {
			t.Errorf("envelope = %q / %q", c, m)
		}
	})

	t.Run("unknown provider is 404, no validation on delete", func(t *testing.T) {
		code, body := doReq(t, h, http.MethodDelete, "/api/v1/llm-credentials/bogus", "", alice)
		if code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body %s)", code, body)
		}
		if c, _ := apiErr(t, body); c != codeNotFound {
			t.Errorf("code = %q, want %q", c, codeNotFound)
		}
	})

	t.Run("another user's provider is 404 (no disclosure)", func(t *testing.T) {
		seed("alice", "custom", "https://llm.example.com/v1", "sk-other")
		code, _ := doReq(t, h, http.MethodDelete, "/api/v1/llm-credentials/custom", "", bob)
		if code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", code)
		}
		if err := store.Delete(context.Background(), "id-alice", "custom"); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
	})

	t.Run("no token", func(t *testing.T) {
		code, _ := doReq(t, h, http.MethodDelete, "/api/v1/llm-credentials/openai", "", nil)
		if code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", code)
		}
	})

	t.Run("delete error", func(t *testing.T) {
		store.deleteErr = errors.New("boom")
		defer func() { store.deleteErr = nil }()
		seed("alice", "openai", "https://api.openai.com/v1", "sk-test-del-key")
		code, body := doReq(t, h, http.MethodDelete, "/api/v1/llm-credentials/openai", "", alice)
		if code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 (body %s)", code, body)
		}
		if c, _ := apiErr(t, body); c != codeInternal {
			t.Errorf("code = %q, want %q", c, codeInternal)
		}
	})
}

func TestLLMCredTest(t *testing.T) {
	const formKey = "sk-test-probe-key-0987654321"
	stub := &credTestStub{resp: &llm.Response{
		JSON:             []byte(`[{"index":0,"category":"Groceries","confidence":0.9}]`),
		Model:            "qwen3-test-model",
		PromptTokens:     12,
		CompletionTokens: 7,
	}}
	h, key, calls := newCredServer(t, newMemCredStore(), stub, nil)

	t.Run("success self_hosted: latency, tokens, model, trimmed endpoint", func(t *testing.T) {
		code, body := doReq(t, h, http.MethodPost, "/api/v1/llm-credentials/test",
			`{"provider":"self_hosted","endpoint":" http://llama:8080/v1 "}`, credHdr(t, key, "alice"))
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", code, body)
		}
		var out struct {
			LatencyMS int    `json:"latency_ms"`
			TokensIn  int    `json:"tokens_in"`
			TokensOut int    `json:"tokens_out"`
			Model     string `json:"model"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("body is not the test result: %v (%s)", err, body)
		}
		if out.LatencyMS <= 0 {
			t.Errorf("latency_ms = %d, want > 0", out.LatencyMS)
		}
		if out.TokensIn != 12 || out.TokensOut != 7 {
			t.Errorf("tokens = %d in / %d out, want 12 / 7", out.TokensIn, out.TokensOut)
		}
		if out.Model != "qwen3-test-model" {
			t.Errorf("model = %q, want %q", out.Model, "qwen3-test-model")
		}
		if strings.Contains(body, formKey) {
			t.Errorf("body contains the key")
		}
		if len(*calls) != 1 {
			t.Fatalf("factory calls = %d, want 1", len(*calls))
		}
		got := (*calls)[0]
		// The endpoint is trimmed before the factory; the key is ""
		// (the form had none); the model is the NORMALISED env model
		// (credentials carry no model of their own).
		if got.baseURL != "http://llama:8080/v1" || got.apiKey != "" || got.model != "test-env-model" {
			t.Errorf("factory args = %+v, want {http://llama:8080/v1 \"\" test-env-model}", got)
		}
		if len(stub.reqs) != 1 {
			t.Fatalf("categorize calls = %d, want 1", len(stub.reqs))
		}
		if stub.reqs[0].System == "" || stub.reqs[0].User == "" {
			t.Errorf("probe prompt is empty: %+v", stub.reqs[0])
		}
	})

	t.Run("success openai: the form key reaches the provider", func(t *testing.T) {
		*calls = (*calls)[:0]
		code, body := doReq(t, h, http.MethodPost, "/api/v1/llm-credentials/test",
			fmt.Sprintf(`{"provider":"openai","endpoint":"https://api.openai.com/v1","api_key":%q}`, formKey),
			credHdr(t, key, "alice"))
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", code, body)
		}
		if strings.Contains(body, formKey) {
			t.Errorf("body contains the full key")
		}
		if len(*calls) != 1 || (*calls)[0].apiKey != formKey {
			t.Fatalf("factory key = %+v, want the form key", *calls)
		}
	})

	t.Run("provider failure is 400 without the key", func(t *testing.T) {
		stub.err = errors.New("llm probe: connection refused")
		defer func() { stub.err = nil }()
		code, body := doReq(t, h, http.MethodPost, "/api/v1/llm-credentials/test",
			fmt.Sprintf(`{"provider":"openai","endpoint":"https://api.openai.com/v1","api_key":%q}`, formKey),
			credHdr(t, key, "alice"))
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", code, body)
		}
		c, m := apiErr(t, body)
		if c != codeInvalidRequest {
			t.Errorf("code = %q, want %q", c, codeInvalidRequest)
		}
		if !strings.Contains(m, "connection test failed") {
			t.Errorf("message = %q, want the generic connection-test message", m)
		}
		if strings.Contains(body, formKey) {
			t.Errorf("error body contains the full key")
		}
	})

	t.Run("openai without key: 400, no call", func(t *testing.T) {
		*calls = (*calls)[:0]
		code, body := doReq(t, h, http.MethodPost, "/api/v1/llm-credentials/test",
			`{"provider":"openai","endpoint":"https://api.openai.com/v1"}`, credHdr(t, key, "alice"))
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", code, body)
		}
		c, m := apiErr(t, body)
		if c != codeInvalidRequest || m != "provider openai requires an API key" {
			t.Errorf("envelope = %q / %q", c, m)
		}
		if len(*calls) != 0 {
			t.Errorf("factory calls = %d, want 0 (validation is before any call)", len(*calls))
		}
	})

	t.Run("unknown provider: 400, no call", func(t *testing.T) {
		*calls = (*calls)[:0]
		code, body := doReq(t, h, http.MethodPost, "/api/v1/llm-credentials/test",
			`{"provider":"bogus","endpoint":"https://x.example.com/v1"}`, credHdr(t, key, "alice"))
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", code, body)
		}
		c, _ := apiErr(t, body)
		if c != codeInvalidRequest {
			t.Errorf("code = %q, want %q", c, codeInvalidRequest)
		}
		if len(*calls) != 0 {
			t.Errorf("factory calls = %d, want 0", len(*calls))
		}
	})

	t.Run("factory error is 500", func(t *testing.T) {
		h2, key2, _ := newCredServer(t, newMemCredStore(), &credTestStub{}, errors.New("factory down"))
		code, body := doReq(t, h2, http.MethodPost, "/api/v1/llm-credentials/test",
			`{"provider":"self_hosted","endpoint":"http://llama:8080/v1"}`, credHdr(t, key2, "alice"))
		if code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 (body %s)", code, body)
		}
		if c, _ := apiErr(t, body); c != codeInternal {
			t.Errorf("code = %q, want %q", c, codeInternal)
		}
	})

	t.Run("no token", func(t *testing.T) {
		code, _ := doReq(t, h, http.MethodPost, "/api/v1/llm-credentials/test",
			`{"provider":"self_hosted","endpoint":"http://x"}`, nil)
		if code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", code)
		}
	})
}

func TestLLMCredNotConfigured(t *testing.T) {
	routeSet := [][2]string{
		{http.MethodGet, "/api/v1/llm-credentials"},
		{http.MethodPut, "/api/v1/llm-credentials/self_hosted"},
		{http.MethodDelete, "/api/v1/llm-credentials/self_hosted"},
		{http.MethodPost, "/api/v1/llm-credentials/test"},
	}

	// nil wrapper: the root-router auth middleware runs first (401
	// for a missing or unverified token, house order like the other
	// routes), and only an AUTHENTICATED caller sees the 503
	// notConfigured readiness answer.
	h, key, _ := newCredServer(t, nil, &credTestStub{}, nil)
	for _, r := range routeSet {
		code, _ := doReq(t, h, r[0], r[1], "", nil)
		if code != http.StatusUnauthorized {
			t.Fatalf("%s %s (no token): status = %d, want 401", r[0], r[1], code)
		}
		code, _ = doReq(t, h, r[0], r[1], "",
			map[string]string{"Authorization": "Bearer not-a-token"})
		if code != http.StatusUnauthorized {
			t.Fatalf("%s %s (bad token): status = %d, want 401", r[0], r[1], code)
		}
		code, body := doReq(t, h, r[0], r[1], "", credHdr(t, key, "alice"))
		if code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s (authed): status = %d, want 503 (body %s)", r[0], r[1], code, body)
		}
		if c, _ := apiErr(t, body); c != codeInternal {
			t.Errorf("%s %s: code = %q, want %q", r[0], r[1], c, codeInternal)
		}
	}

	// store present, LLM factory absent: the test route is 503, the
	// credential routes still work.
	h3, key3 := newCredServerLLMNil(t, newMemCredStore())
	code, body := doReq(t, h3, http.MethodPost, "/api/v1/llm-credentials/test",
		`{"provider":"self_hosted","endpoint":"http://x"}`, credHdr(t, key3, "alice"))
	if code != http.StatusServiceUnavailable {
		t.Fatalf("no LLM: status = %d, want 503 (body %s)", code, body)
	}
	code, b := doReq(t, h3, http.MethodGet, "/api/v1/llm-credentials", "", credHdr(t, key3, "alice"))
	if code != http.StatusOK || strings.TrimSpace(b) != "[]" {
		t.Fatalf("list with nil LLM: status = %d body = %s, want 200 []", code, b)
	}
}

// newCredServerLLMNil wires the credential wrapper with a store but a
// nil LLM factory (the test route's extra dependency) — the wiring
// main.go could not produce, kept here for the 503 branch.
func newCredServerLLMNil(t *testing.T, store LLMCredentialStore) (http.Handler, jwk.Key) {
	t.Helper()
	key := mintKey(t)
	b, err := NewBootstrap(newMemStore(), testKEK(t), nil)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	wrap := &LLMCredentials{Store: store, Env: categorize.LLMEnv{Model: "test-env-model"}}
	return New(fakeReady{}, BuildInfo{}, liveAuth(t, key), b, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, wrap, nil, nil, nil, nil, nil, false, nil, nil, nil, nil, nil, nil, nil), key
}

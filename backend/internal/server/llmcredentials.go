// LLM credential endpoints (plan step 5.6): GET /api/v1/llm-credentials
// (the caller's credentials, key masked), PUT and DELETE
// /api/v1/llm-credentials/{provider} (at most one row per provider; the
// API key is sealed with the caller's DEK and never read back — reads
// return only its mask), and POST /api/v1/llm-credentials/test (fire one
// trivial chat-completions call at the endpoint the form shows and
// report the latency and token usage). Every route is caller-scoped;
// the handler order is the house one (notConfigured, then requireUser).
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"finance-tracker/internal/categorize"
	"finance-tracker/internal/llm"
	"finance-tracker/internal/repo"

	"github.com/go-chi/chi/v5"
)

// LLMCredentialStore is the persistence surface the credential routes
// need. *repo.CredentialStore satisfies it (its extra Get/Resolve
// methods are unused here); the route tests use an in-memory
// implementation.
type LLMCredentialStore interface {
	Put(ctx context.Context, userID string, in repo.CredentialIn, dek []byte) (repo.LLMCredential, error)
	List(ctx context.Context, userID string, dek []byte) ([]repo.LLMCredential, error)
	Delete(ctx context.Context, userID, provider string) error
}

// LLMCredentials wires the plan 5.6 routes: the per-user credential
// store plus the process LLM fallback (Env — the same normalised values
// the worker and the reprompt route use; credentials carry no model of
// their own, so the test probe asks the env model) and an injectable
// provider factory (the same shape as Reprompter's LLM field, so the
// route tests run fully in-memory).
type LLMCredentials struct {
	Store LLMCredentialStore
	Env   categorize.LLMEnv
	LLM   func(baseURL, apiKey, model string) (llm.Categorizer, error)
}

// llmMaxEndpointLen mirrors repo.maxEndpointLen (the 0003 endpoint
// column limit); the message strings mirror repo.validateCredentialIn
// exactly, the same house pattern as checkName.
const llmMaxEndpointLen = 2048

// checkCredentialIn validates a PUT/test body against the repo's rules:
// the provider vocabulary, the endpoint shape, and the key requirement
// for the cloud providers.
func checkCredentialIn(provider, endpoint string, apiKey *string) error {
	if !llmProviderIn(provider) {
		return fmt.Errorf("provider must be one of %s", strings.Join(repo.CredentialProviders, ", "))
	}
	if strings.TrimSpace(endpoint) == "" {
		return errors.New("endpoint must not be empty")
	}
	if utf8.RuneCountInString(endpoint) > llmMaxEndpointLen {
		return fmt.Errorf("endpoint must be at most %d characters", llmMaxEndpointLen)
	}
	if (provider == "openai" || provider == "custom") && (apiKey == nil || *apiKey == "") {
		return fmt.Errorf("provider %s requires an API key", provider)
	}
	return nil
}

// llmProviderIn reports whether provider is in the 0003 vocabulary
// (repo.CredentialProviders: self_hosted, openai, custom).
func llmProviderIn(provider string) bool {
	for _, p := range repo.CredentialProviders {
		if p == provider {
			return true
		}
	}
	return false
}

// llmCredReady reports whether the credential routes are wired (the
// wrapper and its store are present); the test route needs the LLM
// factory too. main.go builds the whole wrapper with a database, so a
// nil wrapper is "no database configured" for every route.
func (h *handler) llmCredReady() bool {
	return h.llmCreds != nil && h.llmCreds.Store != nil
}

// Wire DTOs. llmCredOut keeps the key masked ("" = the row has none)
// and renders the timestamps as RFC3339 UTC, the only place the row
// reaches the wire.
type llmCredOut struct {
	Provider     string `json:"provider"`
	Endpoint     string `json:"endpoint"`
	APIKeyMasked string `json:"api_key_masked"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

func toLLMCredOut(c repo.LLMCredential) llmCredOut {
	return llmCredOut{
		Provider:     c.Provider,
		Endpoint:     c.Endpoint,
		APIKeyMasked: c.APIKeyMasked,
		CreatedAt:    c.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:    c.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// llmCredIn is the PUT body. api_key is absent/null/"" = no key (valid
// only for self_hosted — the requirement is enforced by the repo, the
// handler 400s first with the same message).
type llmCredIn struct {
	Endpoint string  `json:"endpoint"`
	APIKey   *string `json:"api_key"`
}

// llmCredTestIn is the POST /test body: the FORM values (test what is
// on the screen, not the stored row) with the PUT validation.
type llmCredTestIn struct {
	Provider string  `json:"provider"`
	Endpoint string  `json:"endpoint"`
	APIKey   *string `json:"api_key"`
}

// llmCredTestOut is the test-connection success: the round-trip
// latency and the usage the provider reported (0 when it omits usage),
// the model echo (absent when the provider does not send one).
type llmCredTestOut struct {
	LatencyMS int    `json:"latency_ms"`
	TokensIn  int    `json:"tokens_in"`
	TokensOut int    `json:"tokens_out"`
	Model     string `json:"model,omitempty"`
}

// llmCredsList serves GET /api/v1/llm-credentials: the caller's
// credentials (at most one per provider), in provider order. An empty
// set answers [] (not null).
func (h *handler) llmCredsList(w http.ResponseWriter, r *http.Request) {
	if !h.llmCredReady() {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	creds, err := h.llmCreds.Store.List(r.Context(), c.row.ID, c.dek)
	if err != nil {
		slog.Error("llm credential list failed", "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "failed to list credentials")
		return
	}
	out := make([]llmCredOut, 0, len(creds))
	for _, cr := range creds {
		out = append(out, toLLMCredOut(cr))
	}
	writeJSON(w, http.StatusOK, out)
}

// llmCredsPut serves PUT /api/v1/llm-credentials/{provider}: creates or
// replaces the caller's credential for the path provider (the UNIQUE
// (user_id, provider) row) and answers 200 with the row — masked key
// and timestamps. The provider vocabulary, endpoint shape and key
// requirement are checked before the store (400 invalid_request, the
// repo's exact messages); the key is sealed with the caller's DEK and
// never returned.
func (h *handler) llmCredsPut(w http.ResponseWriter, r *http.Request) {
	if !h.llmCredReady() {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	provider := chi.URLParam(r, "provider")
	var b llmCredIn
	if err := decodeBody(r, &b); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "body must be a JSON object: "+err.Error())
		return
	}
	if err := checkCredentialIn(provider, b.Endpoint, b.APIKey); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	cred, err := h.llmCreds.Store.Put(r.Context(), c.row.ID, repo.CredentialIn{
		Provider: provider,
		Endpoint: b.Endpoint,
		APIKey:   b.APIKey,
	}, c.dek)
	if err != nil {
		slog.Error("llm credential save failed", "provider", provider, "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "failed to save credential")
		return
	}
	writeJSON(w, http.StatusOK, toLLMCredOut(cred))
}

// llmCredsDelete serves DELETE /api/v1/llm-credentials/{provider}:
// removes the caller's row for the provider and answers 204. A row the
// caller does not have (including an unknown provider) answers 404
// not_found — existence is not disclosed.
func (h *handler) llmCredsDelete(w http.ResponseWriter, r *http.Request) {
	if !h.llmCredReady() {
		notConfigured(w)
		return
	}
	c, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	provider := chi.URLParam(r, "provider")
	err := h.llmCreds.Store.Delete(r.Context(), c.row.ID, provider)
	if err != nil {
		if errors.Is(err, repo.ErrCredentialNotFound) {
			writeAPIError(w, http.StatusNotFound, codeNotFound, "credential not found")
			return
		}
		slog.Error("llm credential delete failed", "provider", provider, "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "failed to delete credential")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// llmCredsTest serves POST /api/v1/llm-credentials/test: fires ONE
// trivial chat-completions call at the endpoint the form shows and
// answers with the round-trip latency and the usage the provider
// reported. The body is validated like the PUT (400 invalid_request
// before any call — including openai/custom without a key); a failed
// round trip (connection refused, non-2xx, timeout, no content,
// malformed JSON) is 400 invalid_request. The probe uses the pinned 5.2
// prompt formats with a fixed two-category list and one short
// transaction; the provider's one-retry nudge is what guarantees JSON
// back — the route never interprets the answer. The route is
// authenticated (any signed-in user may probe their own form values)
// but the user row is not needed.
func (h *handler) llmCredsTest(w http.ResponseWriter, r *http.Request) {
	if !h.llmCredReady() || h.llmCreds.LLM == nil {
		notConfigured(w)
		return
	}
	if _, ok := h.requireUser(w, r); !ok {
		return
	}
	var b llmCredTestIn
	if err := decodeBody(r, &b); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "body must be a JSON object: "+err.Error())
		return
	}
	if err := checkCredentialIn(b.Provider, b.Endpoint, b.APIKey); err != nil {
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	env := categorize.NormalizeEnv(h.llmCreds.Env)
	key := ""
	if b.APIKey != nil {
		key = *b.APIKey
	}
	provider, err := h.llmCreds.LLM(strings.TrimSpace(b.Endpoint), key, env.Model)
	if err != nil {
		slog.Error("llm credential test: build provider failed", "endpoint", b.Endpoint, "error", err)
		writeAPIError(w, http.StatusInternalServerError, codeInternal, "failed to start connection test")
		return
	}
	req := llm.Request{
		System: categorize.SystemPrompt([]categorize.Category{{Name: "Groceries"}, {Name: "Utilities"}}),
		User:   categorize.UserPrompt([]categorize.Item{{Description: "connection check", Amount: "1.00"}}),
	}
	start := time.Now()
	resp, err := provider.Categorize(r.Context(), req)
	if err != nil {
		// Full details in the server log only; the client message
		// carries the llm package's error (status, endpoint and
		// response snippets — never request material, so never the
		// key).
		slog.Error("llm credential test failed", "endpoint", b.Endpoint, "model", env.Model, "error", err)
		writeAPIError(w, http.StatusBadRequest, codeInvalidRequest, "connection test failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, llmCredTestOut{
		LatencyMS: int(time.Since(start).Round(time.Millisecond).Milliseconds()),
		TokensIn:  resp.PromptTokens,
		TokensOut: resp.CompletionTokens,
		Model:     resp.Model,
	})
}

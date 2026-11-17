// Package llm is the LLM provider abstraction for transaction
// categorization (plan 5.1). A Categorizer takes a raw prompt pair
// (system + user) and returns the model's answer as compact,
// validated JSON. The answer's SHAPE (category name, confidence,
// rationale) is validated by the caller (plan 5.2), not here: this
// package only guarantees "valid JSON back from an LLM", and 5.2's
// golden tests mock Categorizer, not the HTTP transport.
//
// The only implementation is Provider, an OpenAI-compatible
// chat-completions client. One implementation covers self-hosted
// llama.cpp, OpenAI, Azure, and any compatible endpoint:
//
//	New("http://llama:8080/v1", "", "qwen2.5:7b")         // keyless
//	New("https://api.openai.com/v1", key, "gpt-4o-mini")
//
// baseURL is the API root and ENDS in /v1 (a trailing slash is
// normalized away); the client appends /chat/completions. apiKey may
// be "" for self-hosted servers that take no key — then NO
// Authorization header is sent.
//
// JSON guarantee: the model's content is extracted as a single JSON
// value (markdown fences stripped, first {/[ to last }/] slice as
// fallback) and compacted. A malformed first attempt is retried
// exactly once with a format nudge appended to the user message; any
// other error (HTTP status, timeout, no content) is returned
// immediately, without retry.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Request is one categorization prompt: a (possibly empty) system
// prompt and a required user prompt.
type Request struct {
	System string // system prompt (may be "")
	User   string // user prompt (required)
}

// Response is the result of one Categorize call.
type Response struct {
	JSON             json.RawMessage // compact, valid JSON — the model's answer
	Model            string          // model field from the API response ("" if absent)
	PromptTokens     int             // from usage; 0 if the server omits usage
	CompletionTokens int             // same
}

// Categorizer turns a prompt pair into the model's JSON answer.
type Categorizer interface {
	Categorize(ctx context.Context, req Request) (*Response, error)
}

// nudgeSuffix is appended to the user message when the first attempt
// returns malformed JSON.
const nudgeSuffix = "\n\nYour previous response was not valid JSON. Respond with ONLY a single JSON value -- no prose, no markdown fences."

// defaultTimeout bounds each attempt; the first call and the
// format-nudge retry each get their own deadline.
const defaultTimeout = 60 * time.Second

// defaultMaxTokens is the max_tokens sent unless WithMaxTokens
// overrides it (values <= 0 omit the field entirely).
const defaultMaxTokens = 1024

var errNoContent = errors.New("llm: model returned no content")

// maxResponseBytes caps a chat-completions response body (T-P0-1): a
// real answer is orders of magnitude smaller, and the cap bounds
// memory against a hostile endpoint streaming an unbounded body. The
// first read past the cap fails with *http.MaxBytesError.
var maxResponseBytes int64 = 4 << 20

// Provider is an OpenAI-compatible chat-completions Categorizer.
type Provider struct {
	url       string
	apiKey    string
	model     string
	timeout   time.Duration
	maxTokens int
	client    *http.Client

	// scheme + host identify the endpoint so the pre-flight and
	// dial-time policy checks know what they are vetting (T-P0-1).
	// Empty = non-http(s) or unparseable URL: the checks are skipped
	// and the request fails in Do with the standard url error.
	scheme string
	host   string

	// policy vets the endpoint's addresses (see endpoint.go).
	policy endpointPolicy
}

// Option customizes a Provider created by New.
type Option func(*Provider)

// WithTimeout sets the per-call timeout. It applies to EVERY attempt
// (the first call and the retry), not to the whole Categorize call.
// Default 60s.
func WithTimeout(d time.Duration) Option {
	return func(p *Provider) { p.timeout = d }
}

// WithMaxTokens sets the max_tokens field of the request body.
// Values <= 0 omit the field entirely. Default 1024.
func WithMaxTokens(n int) Option {
	return func(p *Provider) { p.maxTokens = n }
}

// New builds a Provider for an OpenAI-compatible API root. baseURL
// must end in /v1 (a trailing slash is normalized away); the client
// POSTs to baseURL + "/chat/completions". apiKey may be "" (then no
// Authorization header is sent).
//
// Endpoint policy (T-P0-1): the always-rejected set (cloud metadata
// / link-local 169.254.0.0/16 and fe80::/10, IPv6 loopback ::1,
// unspecified 0.0.0.0 / ::) is refused in every mode and can never
// be allowlisted, and response bodies are capped at maxResponseBytes.
// A bare New is non-strict beyond that. WithEndpointAllowlist arms
// strict mode: plain http is refused unless the address is
// allowlisted, private and loopback addresses need an explicit CIDR,
// and public https is the only other thing dialable. A hostname is
// resolved and EVERY returned address is vetted — before the request
// and again at dial time, when the connection is made to a vetted IP
// (TOCTOU guard) while SNI and the Host header keep the original
// host. See endpoint.go.
func New(baseURL, apiKey, model string, opts ...Option) *Provider {
	p := &Provider{
		apiKey:    apiKey,
		model:     model,
		timeout:   defaultTimeout,
		maxTokens: defaultMaxTokens,
	}
	for _, opt := range opts {
		opt(p)
	}
	p.url = strings.TrimRight(baseURL, "/") + "/chat/completions"
	// Identify the endpoint host so the transport can vet the
	// address at dial time. Non-http(s) schemes (and unparseable
	// URLs) skip the checks; Do fails on them with the standard
	// url error, as before.
	if u, err := url.Parse(p.url); err == nil {
		if u.Scheme == "http" || u.Scheme == "https" {
			if h := u.Hostname(); h != "" {
				p.scheme = u.Scheme
				p.host = h
			}
		}
	}
	p.client = &http.Client{Transport: p.transport()}
	return p
}

// transport returns the transport the provider's client dials with.
// For an identified endpoint host, every dial re-resolves the host,
// vets EVERY returned address against the policy, and connects to a
// vetted IP — closing the resolve-then-dial race. The transport
// still presents the ORIGINAL host for SNI and the Host header, so
// TLS and virtual hosting are unaffected. A redirect that leaves the
// endpoint's host is refused: a provider must not silently call a
// second server (T-P0-1).
func (p *Provider) transport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	if p.host == "" {
		return t
	}
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if host != p.host {
			return nil, fmt.Errorf("llm: %w: redirect host %s", errEndpointBlocked, host)
		}
		ips, err := p.policy.checkHost(ctx, p.scheme, host)
		if err != nil {
			return nil, err
		}
		d := net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		return d.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
	}
	return t
}

var _ Categorizer = (*Provider)(nil)

// message is one chat message in the request body.
type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatRequest is the /chat/completions request body. temperature is
// pinned to 0 for determinism; max_tokens is omitted when <= 0;
// response_format is deliberately NOT sent — maximum compatibility
// across llama.cpp / OpenAI / Azure, the nudge retry is the JSON
// guarantee.
type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []message `json:"messages"`
	Temperature float64   `json:"temperature"`
	MaxTokens   *int      `json:"max_tokens,omitempty"`
}

// Categorize sends req and returns the model's answer as compact,
// validated JSON. A malformed first attempt is retried exactly once
// with nudgeSuffix appended to the user message; both attempts
// malformed -> error. Other errors (HTTP, timeout, no content) are
// returned immediately, without retry.
func (p *Provider) Categorize(ctx context.Context, req Request) (*Response, error) {
	var lastRaw string
	for attempt := 0; attempt < 2; attempt++ {
		content, model, pt, ct, err := p.do(ctx, req)
		if err != nil {
			return nil, err
		}
		lastRaw = content
		raw, err := extractJSON(content)
		if err == nil {
			return &Response{JSON: raw, Model: model, PromptTokens: pt, CompletionTokens: ct}, nil
		}
		req.User += nudgeSuffix
	}
	return nil, fmt.Errorf("llm: malformed JSON response after 2 attempts: %s", snippet(lastRaw, 200))
}

// do performs ONE HTTP call with its own per-call timeout and
// returns the raw content string. It never retries.
func (p *Provider) do(ctx context.Context, req Request) (content, model string, promptTokens, completionTokens int, err error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	// Pre-flight policy check: a blocked endpoint fails before any
	// byte goes out. The transport checks again at dial time; the
	// pair closes the resolve-then-dial window (T-P0-1).
	if p.host != "" {
		if _, err := p.policy.checkHost(ctx, p.scheme, p.host); err != nil {
			return "", "", 0, 0, fmt.Errorf("llm: %w", err)
		}
	}

	msgs := make([]message, 0, 2)
	if req.System != "" {
		msgs = append(msgs, message{Role: "system", Content: req.System})
	}
	msgs = append(msgs, message{Role: "user", Content: req.User})

	body := chatRequest{Model: p.model, Messages: msgs, Temperature: 0}
	if p.maxTokens > 0 {
		body.MaxTokens = &p.maxTokens
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return "", "", 0, 0, fmt.Errorf("llm: encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(buf))
	if err != nil {
		return "", "", 0, 0, fmt.Errorf("llm: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return "", "", 0, 0, fmt.Errorf("llm: %w", err)
	}
	defer resp.Body.Close()
	// Cap the body before reading it (T-P0-1); a read past the cap
	// fails with *http.MaxBytesError, and the %w wrap below keeps it
	// matchable with errors.As.
	resp.Body = http.MaxBytesReader(nil, resp.Body, maxResponseBytes)
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", 0, 0, fmt.Errorf("llm: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", 0, 0, fmt.Errorf("llm: API error %d: %s", resp.StatusCode, snippet(string(raw), 200))
	}

	var parsed struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", "", 0, 0, fmt.Errorf("llm: unparseable response body: %w", err)
	}
	if len(parsed.Choices) == 0 || parsed.Choices[0].Message.Content == "" {
		return "", "", 0, 0, errNoContent
	}
	return parsed.Choices[0].Message.Content, parsed.Model, parsed.Usage.PromptTokens, parsed.Usage.CompletionTokens, nil
}

// snippet truncates s to at most n bytes; error messages quote raw
// model/API output, so keep them short.
func snippet(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

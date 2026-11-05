// Per-user LLM credential repository (plan step 2.6, migration 0003).
//
// endpoint_encrypted + api_key_encrypted hold DEK packs; the provider
// enum stays plaintext (routing only, plan §5). UNIQUE (user_id,
// provider) — at most one row per (user, provider) — so Put is an
// upsert. Reads NEVER return the API key: only its mask (first4…last4,
// fully masked when short), so the full secret exists nowhere but the
// encrypted column (plan 5.6 "endpoint + masked API key").
package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"finance-tracker/internal/crypto"

	"github.com/jackc/pgx/v5/pgxpool"
)

// CredentialProviders is the 0003 llm_credentials.provider vocabulary
// (the 5.6 picker set: self-hosted llama.cpp, OpenAI, any other
// OpenAI-compatible endpoint).
var CredentialProviders = []string{"self_hosted", "openai", "custom"}

var credentialProviderOK = func() map[string]bool {
	m := make(map[string]bool, len(CredentialProviders))
	for _, p := range CredentialProviders {
		m[p] = true
	}
	return m
}()

const maxEndpointLen = 2048

// LLMCredential is one llm_credentials row. Endpoint is decrypted;
// APIKeyMasked is the masked key ("" when the row has no key —
// self-hosted providers take none). The full key is never part of the
// type.
type LLMCredential struct {
	Provider     string
	Endpoint     string
	APIKeyMasked string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// CredentialIn is the Put input. APIKey is nil or "" = no key (valid
// only for self_hosted); openai and custom require it.
type CredentialIn struct {
	Provider string
	Endpoint string
	APIKey   *string
}

// providerPriority breaks Resolve's updated_at ties: lower wins
// (openai > custom > self_hosted).
var providerPriority = map[string]int{"openai": 0, "custom": 1, "self_hosted": 2}

// ResolvedCredential is the worker's in-memory credential view: the
// API key in plaintext, for the llm client only. It is never logged
// and never serialized.
type ResolvedCredential struct {
	Provider string
	Endpoint string
	APIKey   string // plaintext — never log, never serialize
}

// CredentialStore persists llm_credentials. One store serves every
// user; each method takes a userID and scopes its SQL on it.
type CredentialStore struct {
	pool *pgxpool.Pool
}

// NewCredentialStore returns a CredentialStore backed by pool (nil for
// a nil pool, mirroring the 1.3c UserStore constructor).
func NewCredentialStore(pool *pgxpool.Pool) *CredentialStore {
	if pool == nil {
		return nil
	}
	return &CredentialStore{pool: pool}
}

// maskKey renders the masked API key: keys of at most 8 runes are fully
// masked, longer keys keep first4 + "…" + last4 (rune-safe).
func maskKey(key string) string {
	runes := []rune(key)
	if len(runes) <= 8 {
		return "****"
	}
	return string(runes[:4]) + "…" + string(runes[len(runes)-4:])
}

// maskOf renders the mask for a Put input key ("" = the row has none).
func maskOf(key *string) string {
	if key == nil || *key == "" {
		return ""
	}
	return maskKey(*key)
}

// validateCredentialIn checks the plaintext input fields. The key
// requirement for cloud providers is enforced HERE, not by the schema
// (0003 comment: api_key_encrypted is nullable).
func validateCredentialIn(in CredentialIn) error {
	if !credentialProviderOK[in.Provider] {
		return fmt.Errorf("provider must be one of %s", strings.Join(CredentialProviders, ", "))
	}
	if strings.TrimSpace(in.Endpoint) == "" {
		return errors.New("endpoint must not be empty")
	}
	if utf8.RuneCountInString(in.Endpoint) > maxEndpointLen {
		return fmt.Errorf("endpoint must be at most %d characters", maxEndpointLen)
	}
	if (in.Provider == "openai" || in.Provider == "custom") && (in.APIKey == nil || *in.APIKey == "") {
		return fmt.Errorf("provider %s requires an API key", in.Provider)
	}
	return nil
}

// credentialFrom assembles an LLMCredential from a row's columns. The
// decrypted key is masked in place — the plaintext never escapes this
// function. A pack that does not open is corruption (500, same rule as
// the other stores).
func credentialFrom(provider string, epPack, keyPack []byte, created, updated time.Time, dek []byte) (LLMCredential, error) {
	ep, err := crypto.Open(dek, epPack)
	if err != nil {
		return LLMCredential{}, fmt.Errorf("repo: decrypt %s credential endpoint: %w", provider, err)
	}
	masked := ""
	if len(keyPack) > 0 {
		key, err := crypto.Open(dek, keyPack)
		if err != nil {
			return LLMCredential{}, fmt.Errorf("repo: decrypt %s credential API key: %w", provider, err)
		}
		masked = maskKey(string(key))
	}
	return LLMCredential{
		Provider:     provider,
		Endpoint:     string(ep),
		APIKeyMasked: masked,
		CreatedAt:    created,
		UpdatedAt:    updated,
	}, nil
}

// Put inserts or updates (UNIQUE (user_id, provider)) the user's
// credential for in.Provider and returns it with the masked key. A
// re-Put updates the endpoint/key in place: created_at stays,
// updated_at moves.
func (s *CredentialStore) Put(ctx context.Context, userID string, in CredentialIn, dek []byte) (LLMCredential, error) {
	if err := validateCredentialIn(in); err != nil {
		return LLMCredential{}, fmt.Errorf("repo: %w", err)
	}
	endpoint := strings.TrimSpace(in.Endpoint)
	epPack, err := crypto.Seal(dek, []byte(endpoint))
	if err != nil {
		return LLMCredential{}, fmt.Errorf("repo: seal credential endpoint: %w", err)
	}
	var keyPack []byte // nil = NULL column (self-hosted takes no key)
	if in.APIKey != nil && *in.APIKey != "" {
		keyPack, err = crypto.Seal(dek, []byte(*in.APIKey))
		if err != nil {
			return LLMCredential{}, fmt.Errorf("repo: seal credential API key: %w", err)
		}
	}
	var created, updated time.Time
	err = s.pool.QueryRow(ctx, `
		INSERT INTO llm_credentials (user_id, provider, endpoint_encrypted, api_key_encrypted)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, provider) DO UPDATE
		SET endpoint_encrypted = EXCLUDED.endpoint_encrypted,
		    api_key_encrypted = EXCLUDED.api_key_encrypted,
		    updated_at = now()
		RETURNING created_at, updated_at`,
		userID, in.Provider, epPack, keyPack).Scan(&created, &updated)
	if err != nil {
		return LLMCredential{}, fmt.Errorf("repo: put credential: %w", err)
	}
	return LLMCredential{
		Provider:     in.Provider,
		Endpoint:     endpoint,
		APIKeyMasked: maskOf(in.APIKey),
		CreatedAt:    created,
		UpdatedAt:    updated,
	}, nil
}

// Get returns the user's credential for provider, or
// ErrCredentialNotFound when the user has none (another user's
// credential for the same provider looks identical — existence is not
// disclosed across users).
func (s *CredentialStore) Get(ctx context.Context, userID, provider string, dek []byte) (LLMCredential, error) {
	var (
		epPack  []byte
		keyPack []byte // nullable bytea: NULL for keyless rows
		created time.Time
		updated time.Time
	)
	err := s.pool.QueryRow(ctx, `
		SELECT endpoint_encrypted, api_key_encrypted, created_at, updated_at
		FROM llm_credentials
		WHERE user_id = $1 AND provider = $2`, userID, provider).
		Scan(&epPack, &keyPack, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return LLMCredential{}, ErrCredentialNotFound
	}
	if err != nil {
		return LLMCredential{}, fmt.Errorf("repo: get credential: %w", err)
	}
	return credentialFrom(provider, epPack, keyPack, created, updated, dek)
}

// List returns the user's credentials (at most one per provider)
// ordered by provider.
func (s *CredentialStore) List(ctx context.Context, userID string, dek []byte) ([]LLMCredential, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT provider, endpoint_encrypted, api_key_encrypted, created_at, updated_at
		FROM llm_credentials
		WHERE user_id = $1
		ORDER BY provider`, userID)
	if err != nil {
		return nil, fmt.Errorf("repo: list credentials: %w", err)
	}
	defer rows.Close()
	out := make([]LLMCredential, 0, 3)
	for rows.Next() {
		var (
			provider string
			epPack   []byte
			keyPack  []byte
			created  time.Time
			updated  time.Time
		)
		if err := rows.Scan(&provider, &epPack, &keyPack, &created, &updated); err != nil {
			return nil, fmt.Errorf("repo: scan credential: %w", err)
		}
		c, err := credentialFrom(provider, epPack, keyPack, created, updated, dek)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo: list credentials: %w", err)
	}
	return out, nil
}

// Delete removes the user's credential for provider. An unknown
// provider for the user is ErrCredentialNotFound; nothing else is
// touched.
func (s *CredentialStore) Delete(ctx context.Context, userID, provider string) error {
	cmdTag, err := s.pool.Exec(ctx, `
		DELETE FROM llm_credentials
		WHERE user_id = $1 AND provider = $2`, userID, provider)
	if err != nil {
		return fmt.Errorf("repo: delete credential: %w", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return ErrCredentialNotFound
	}
	return nil
}

// Resolve returns the user's ACTIVE credential with the API key
// decrypted in memory — the ONLY place the plaintext key ever exists
// (the llm client request; never logged, never serialized).
// Selection: the most recently updated row wins; ties break by provider
// priority openai > custom > self_hosted. ErrCredentialNotFound when
// the user has no rows (the caller falls back to the server env).
func (s *CredentialStore) Resolve(ctx context.Context, userID string, dek []byte) (ResolvedCredential, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT provider, endpoint_encrypted, api_key_encrypted, updated_at
		FROM llm_credentials
		WHERE user_id = $1`, userID)
	if err != nil {
		return ResolvedCredential{}, fmt.Errorf("repo: resolve credential: %w", err)
	}
	defer rows.Close()

	type candidate struct {
		provider string
		epPack   []byte
		keyPack  []byte
		updated  time.Time
	}
	var best *candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.provider, &c.epPack, &c.keyPack, &c.updated); err != nil {
			return ResolvedCredential{}, fmt.Errorf("repo: scan credential: %w", err)
		}
		if best == nil || c.updated.After(best.updated) ||
			(c.updated.Equal(best.updated) && providerPriority[c.provider] < providerPriority[best.provider]) {
			best = &c
		}
	}
	if err := rows.Err(); err != nil {
		return ResolvedCredential{}, fmt.Errorf("repo: resolve credential: %w", err)
	}
	if best == nil {
		return ResolvedCredential{}, ErrCredentialNotFound
	}

	ep, err := crypto.Open(dek, best.epPack)
	if err != nil {
		return ResolvedCredential{}, fmt.Errorf("repo: decrypt %s credential endpoint: %w", best.provider, err)
	}
	resolved := ResolvedCredential{Provider: best.provider, Endpoint: string(ep)}
	if len(best.keyPack) > 0 {
		key, err := crypto.Open(dek, best.keyPack)
		if err != nil {
			return ResolvedCredential{}, fmt.Errorf("repo: decrypt %s credential API key: %w", best.provider, err)
		}
		resolved.APIKey = string(key)
	}
	return resolved, nil
}

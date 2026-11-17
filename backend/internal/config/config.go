// Package config loads application configuration from environment
// variables. Canonical secret names follow plan §6; non-secret settings
// (PORT) use local defaults suitable for the dev stack.
package config

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"finance-tracker/internal/llm"
)

// SMTP holds generic SMTP configuration (iteration 1: Mailpit sink).
type SMTP struct {
	Host      string
	Port      int
	User      string
	Pass      string
	From      string
	TLSPolicy string // starttls | implicit | none
}

// LLM holds OpenAI-compatible LLM server configuration.
type LLM struct {
	BaseURL string
	APIKey  string
	Model   string

	// EndpointCIDRs (FTRK_LLM_ENDPOINT_CIDRS, comma-separated CIDRs or
	// bare IPs) additionally admits private/reserved endpoint
	// addresses — e.g. the docker network's llama service. The
	// always-reject set (metadata/link-local, ::1, 0.0.0.0) is never
	// admitted (see internal/llm).
	EndpointCIDRs []*net.IPNet
}

// Config is the full application configuration shared by cmd/api and
// cmd/worker.
type Config struct {
	// Port is the HTTP listen port for cmd/api.
	Port int

	// DatabaseURL is the Postgres DSN (pgx format). Empty = no database
	// configured: the API still serves /healthz and /metrics, and /readyz
	// reports not-ready.
	DatabaseURL string

	// Auth0Issuer / Auth0Audience / Auth0ClientID / Auth0ClientSecret
	// identify the OIDC issuer (Keycloak dev realm in iteration 1, an
	// Auth0 tenant later — same variable names, plan §6). Auth0Issuer is
	// the token's `iss` value (browser horizon); Auth0Audience is the
	// required access-token audience — in dev that is the realm's
	// audience-mapper value, NOT the client id (D-013, D-021).
	Auth0Issuer       string
	Auth0Audience     string
	Auth0ClientID     string
	Auth0ClientSecret string

	// OIDCDiscoveryURL is the horizon the verifier runs OIDC discovery
	// (JWKS) against. Empty = Auth0Issuer (one-horizon IdPs, e.g. Auth0);
	// set it when the token's `iss` is not reachable from the backend
	// (dev split horizon, D-021).
	OIDCDiscoveryURL string

	// NRLicenseKey enables the New Relic agent; empty = agent no-ops.
	NRLicenseKey string

	// KEK wraps per-user DEKs: 32 bytes, hex-encoded (64 chars).
	KEK string

	SMTP SMTP
	LLM  LLM
}

// Load reads configuration from the process environment.
func Load() (*Config, error) {
	return load(os.Getenv)
}

func load(getenv func(string) string) (*Config, error) {
	cfg := &Config{
		Port:              8080,
		DatabaseURL:       getenv("DATABASE_URL"),
		Auth0Issuer:       getenv("AUTH0_ISSUER"),
		Auth0Audience:     getenv("AUTH0_AUDIENCE"),
		Auth0ClientID:     getenv("AUTH0_CLIENT_ID"),
		Auth0ClientSecret: getenv("AUTH0_CLIENT_SECRET"),
		OIDCDiscoveryURL:  getenv("OIDC_DISCOVERY_URL"),
		NRLicenseKey:      getenv("NR_LICENSE_KEY"),
		KEK:               getenv("KEK"),
		SMTP: SMTP{
			Host:      getenv("SMTP_HOST"),
			User:      getenv("SMTP_USER"),
			Pass:      getenv("SMTP_PASS"),
			From:      getenv("SMTP_FROM"),
			TLSPolicy: getenv("SMTP_TLS_POLICY"),
		},
		LLM: LLM{
			BaseURL: getenv("LLM_BASE_URL"),
			APIKey:  getenv("LLM_API_KEY"),
			Model:   getenv("LLM_MODEL"),
		},
	}
	if v := getenv("PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("PORT: %w", err)
		}
		cfg.Port = p
	}
	if v := getenv("SMTP_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("SMTP_PORT: %w", err)
		}
		cfg.SMTP.Port = p
	}
	if cfg.SMTP.TLSPolicy == "" {
		cfg.SMTP.TLSPolicy = "starttls"
	}
	switch cfg.SMTP.TLSPolicy {
	case "starttls", "implicit", "none":
	default:
		return nil, fmt.Errorf("SMTP_TLS_POLICY: unsupported %q (want starttls, implicit or none)", cfg.SMTP.TLSPolicy)
	}
	if v := getenv("FTRK_LLM_ENDPOINT_CIDRS"); v != "" {
		nets, err := llm.ParseEndpointCIDRs(v)
		if err != nil {
			return nil, fmt.Errorf("FTRK_LLM_ENDPOINT_CIDRS: %w", err)
		}
		cfg.LLM.EndpointCIDRs = nets
	}
	// The KEK wraps every user's DEK, so it is ALWAYS required: fail
	// fast on a missing or degenerate value instead of letting the
	// crypto layer fail per request (or wrap data with a garbage key).
	if cfg.KEK == "" {
		return nil, errors.New("KEK: required (64 hex chars, 32 bytes)")
	}
	if len(cfg.KEK) != 64 {
		return nil, fmt.Errorf("KEK: want 64 hex chars (32 bytes), got %d chars", len(cfg.KEK))
	}
	if _, err := hex.DecodeString(cfg.KEK); err != nil {
		return nil, fmt.Errorf("KEK: not valid hex: %w", err)
	}
	if cfg.KEK == strings.Repeat("0", 64) {
		return nil, errors.New("KEK: all-zeros value is not acceptable")
	}
	return cfg, nil
}

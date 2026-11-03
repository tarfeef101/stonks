// Package authn validates OIDC access tokens (plan step 1.3). The
// verifier is issuer-agnostic: it discovers the issuer's JWKS via plain
// OIDC discovery, so dev runs against the local Keycloak realm and later
// environments run against Auth0 with environment changes only (D-021).
package authn

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	jwtmiddleware "github.com/auth0/go-jwt-middleware/v3"
	"github.com/auth0/go-jwt-middleware/v3/core"
	"github.com/auth0/go-jwt-middleware/v3/jwks"
	"github.com/auth0/go-jwt-middleware/v3/validator"
)

// Config carries the issuer-agnostic OIDC parameters (D-021):
//
//   - Issuer is the required token `iss` (browser horizon in dev).
//   - Audience is the required access-token `aud` (in dev the realm's
//     audience-mapper value — NOT the client id, D-013).
//   - DiscoveryURL is the horizon OIDC discovery (JWKS) runs against.
//     Empty = Issuer (one-horizon IdPs like Auth0 need no extra env).
type Config struct {
	Issuer       string
	Audience     string
	DiscoveryURL string
}

// KeyFunc resolves the signing key(s) for a token. *jwks.Provider's
// KeyFunc satisfies it with lazy OIDC discovery; unit tests inject a
// static key set.
type KeyFunc func(ctx context.Context) (any, error)

// Verifier validates OIDC access tokens and exposes the validated
// subject claim.
type Verifier struct {
	middleware *jwtmiddleware.JWTMiddleware
}

// New builds a Verifier from cfg. It performs no network I/O: JWKS
// discovery is lazy and happens on the first token validation (D-021).
func New(cfg Config) (*Verifier, error) {
	discovery := cfg.DiscoveryURL
	if discovery == "" {
		discovery = cfg.Issuer
	}
	u, err := url.Parse(discovery)
	if err != nil {
		return nil, fmt.Errorf("authn: parse discovery URL %q: %w", discovery, err)
	}
	provider, err := jwks.NewProvider(jwks.WithIssuerURL(u))
	if err != nil {
		return nil, fmt.Errorf("authn: build JWKS provider: %w", err)
	}
	return NewWithKeyFunc(cfg, provider.KeyFunc)
}

// NewWithKeyFunc is New with an explicit key resolver instead of OIDC
// discovery; unit tests use it to avoid any network access.
func NewWithKeyFunc(cfg Config, keyFunc KeyFunc) (*Verifier, error) {
	if cfg.Issuer == "" {
		return nil, errors.New("authn: issuer is required (AUTH0_ISSUER)")
	}
	if cfg.Audience == "" {
		return nil, errors.New("authn: audience is required (AUTH0_AUDIENCE)")
	}
	if keyFunc == nil {
		return nil, errors.New("authn: key func is required")
	}
	v, err := validator.New(
		validator.WithKeyFunc(keyFunc),
		validator.WithAlgorithm(validator.RS256),
		validator.WithIssuer(cfg.Issuer),
		validator.WithAudience(cfg.Audience),
	)
	if err != nil {
		return nil, fmt.Errorf("authn: build validator: %w", err)
	}
	mw, err := jwtmiddleware.New(jwtmiddleware.WithValidator(v))
	if err != nil {
		return nil, fmt.Errorf("authn: build middleware: %w", err)
	}
	return &Verifier{middleware: mw}, nil
}

// Middleware returns HTTP middleware that rejects any request that does
// not carry a valid access token with 401 (JSON body plus an RFC 6750
// WWW-Authenticate header — the library's default error handler).
func (v *Verifier) Middleware(next http.Handler) http.Handler {
	return v.middleware.CheckJWT(next)
}

// Subject extracts the validated `sub` claim from a request context that
// passed through Middleware. It errors when the context carries no
// validated claims or the token has no subject (the dev realm must carry
// the `basic` client scope for tokens to have `sub`, D-022).
func (v *Verifier) Subject(ctx context.Context) (string, error) {
	claims, err := core.GetClaims[*validator.ValidatedClaims](ctx)
	if err != nil {
		return "", fmt.Errorf("authn: no validated claims in context: %w", err)
	}
	if claims.RegisteredClaims.Subject == "" {
		return "", errors.New("authn: access token has no subject (sub) claim")
	}
	return claims.RegisteredClaims.Subject, nil
}

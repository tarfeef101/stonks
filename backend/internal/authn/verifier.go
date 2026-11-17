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
	"time"

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

// Identity is the validated identity extracted from an access token: the
// `sub` claim plus the OIDC profile claims (email, name,
// preferred_username). Profile claims are empty when the token does not
// carry them (D-024); callers must treat them as optional.
type Identity struct {
	Sub      string
	Email    string
	Name     string
	Username string
}

// identityClaims is the WithCustomClaims target (D-024): the auth0 v3
// validator decodes only RFC 7519 registered claims into
// ValidatedClaims.RegisteredClaims, so the OIDC profile claims are opt-in.
// The library re-decodes the payload part of the SAME token string that
// was already signature/iss/aud validated, into a fresh struct per
// request. Validate is a no-op — the middleware is the only trust
// boundary.
type identityClaims struct {
	Email             string `json:"email"`
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
}

func (identityClaims) Validate(context.Context) error { return nil }

// Verifier validates OIDC access tokens and exposes the validated
// identity (subject + profile claims).
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
		// IdP and backend clocks are not perfectly in sync (self-hosted
		// Keycloak drifts in particular), so a 30 s exp/nbf leeway keeps
		// freshly-issued tokens from failing right at their boundaries
		// without meaningfully weakening expiry.
		validator.WithAllowedClockSkew(30*time.Second),
		// D-024: the library only decodes RFC 7519 registered claims, so
		// the OIDC profile claims are opt-in via WithCustomClaims. The
		// factory runs per request and the library re-decodes the payload
		// part of the same already-verified token string.
		validator.WithCustomClaims(func() *identityClaims { return &identityClaims{} }),
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

// Identity extracts the validated identity from a request context that
// passed through Middleware: the `sub` claim plus the OIDC profile claims
// (email, name, preferred_username). It errors when the context carries
// no validated claims or the token has no subject (the dev realm must
// carry the `basic` client scope for tokens to have `sub`, D-022). Profile
// claims are empty when the token does not carry them.
func (v *Verifier) Identity(ctx context.Context) (Identity, error) {
	claims, err := core.GetClaims[*validator.ValidatedClaims](ctx)
	if err != nil {
		return Identity{}, fmt.Errorf("authn: no validated claims in context: %w", err)
	}
	if claims.RegisteredClaims.Subject == "" {
		return Identity{}, errors.New("authn: access token has no subject (sub) claim")
	}
	id := Identity{Sub: claims.RegisteredClaims.Subject}
	// CustomClaims is nil unless the WithCustomClaims option was passed at
	// construction (always the case via New/NewWithKeyFunc).
	if ic, ok := claims.CustomClaims.(*identityClaims); ok && ic != nil {
		id.Email = ic.Email
		id.Name = ic.Name
		id.Username = ic.PreferredUsername
	}
	return id, nil
}

// Subject extracts the validated `sub` claim from a request context that
// passed through Middleware. It errors when the context carries no
// validated claims or the token has no subject (the dev realm must carry
// the `basic` client scope for tokens to have `sub`, D-022).
func (v *Verifier) Subject(ctx context.Context) (string, error) {
	id, err := v.Identity(ctx)
	if err != nil {
		return "", err
	}
	return id.Sub, nil
}

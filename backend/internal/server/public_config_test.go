package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPublicConfigUnauthenticated: GET /api/v1/public-config is the one
// unauthenticated route under /api (D-036); it returns the deploy-time
// public config with the headers the SPA relies on (json, no-store).
func TestPublicConfigUnauthenticated(t *testing.T) {
	cfg := PublicConfig{
		OIDCIssuer:      "https://issuer.test",
		OIDCClientID:    "spa",
		OIDCAudience:    "aud",
		NRLicenseKey:    "lk",
		NRApplicationID: "aid",
	}
	h := New(nil, BuildInfo{}, testAuth(), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, false, nil, nil, nil, nil, nil, nil, nil, cfg)

	code, body, ct := do(t, h, "/api/v1/public-config", nil)
	if code != http.StatusOK {
		t.Fatalf("public-config status = %d, want 200 (body %s)", code, body)
	}
	if ct != "application/json" {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	// do() does not expose all headers; check Cache-Control directly.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/public-config", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("cache-control = %q, want no-store", got)
	}
	for _, want := range []string{
		`"issuer":"https://issuer.test"`,
		`"client_id":"spa"`,
		`"audience":"aud"`,
		`"license_key":"lk"`,
		`"application_id":"aid"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("public-config body %s missing %s", body, want)
		}
	}
}

// TestPublicConfigOmitsEmpty: empty sections and keys are omitted from
// the response (pointer sections + omitempty), so the SPA can treat
// absence as "feature off".
func TestPublicConfigOmitsEmpty(t *testing.T) {
	cfg := PublicConfig{OIDCIssuer: "https://issuer.test", OIDCClientID: "spa"}
	h := New(nil, BuildInfo{}, testAuth(), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, false, nil, nil, nil, nil, nil, nil, nil, cfg)

	code, body, _ := do(t, h, "/api/v1/public-config", nil)
	if code != http.StatusOK {
		t.Fatalf("public-config status = %d, want 200 (body %s)", code, body)
	}
	for _, absent := range []string{`"audience"`, `"newrelic"`, `"license_key"`} {
		if strings.Contains(body, absent) {
			t.Errorf("public-config body %s must not contain %s", body, absent)
		}
	}
}

// TestPublicConfigDoesNotOpenOtherRoutes: the prefixAuth allow-list is
// exact-path — with a fully populated config, /api/v1/me still answers
// 401 without a token.
func TestPublicConfigDoesNotOpenOtherRoutes(t *testing.T) {
	cfg := PublicConfig{
		OIDCIssuer:      "https://issuer.test",
		OIDCClientID:    "spa",
		OIDCAudience:    "aud",
		NRLicenseKey:    "lk",
		NRApplicationID: "aid",
	}
	h := New(nil, BuildInfo{}, testAuth(), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, false, nil, nil, nil, nil, nil, nil, nil, cfg)

	code, body, _ := do(t, h, "/api/v1/me", nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /api/v1/me status = %d, want 401 (body %s)", code, body)
	}
}

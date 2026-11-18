package config

import (
	"strings"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	// The KEK is mandatory even with no database (T-P0-2), so the
	// default load supplies one.
	kek := strings.Repeat("0123456789abcdef", 4)
	cfg, err := load(func(k string) string {
		if k == "KEK" {
			return kek
		}
		return ""
	})
	if err != nil {
		t.Fatalf("load with no env: %v", err)
	}
	if cfg.Port != 8080 {
		t.Errorf("Port = %d, want 8080", cfg.Port)
	}
	if cfg.KEK != kek {
		t.Errorf("KEK = %q, want the supplied value", cfg.KEK)
	}
	if cfg.SMTP.TLSPolicy != "starttls" {
		t.Errorf("SMTP.TLSPolicy = %q, want %q", cfg.SMTP.TLSPolicy, "starttls")
	}
	if cfg.DatabaseURL != "" || cfg.NRLicenseKey != "" ||
		cfg.OIDCAudience != "" || cfg.OIDCDiscoveryURL != "" {
		t.Errorf("expected zero-value secrets, got %+v", cfg)
	}
}

func TestLoadFull(t *testing.T) {
	env := map[string]string{
		"PORT":               "9090",
		"DATABASE_URL":       "postgres://u:p@localhost:5432/fin?sslmode=disable",
		"OIDC_ISSUER":        "https://issuer.example/",
		"OIDC_AUDIENCE":      "aud.example",
		"OIDC_CLIENT_ID":     "cid",
		"OIDC_CLIENT_SECRET": "secret",
		"OIDC_DISCOVERY_URL": "http://keycloak:8080/realms/finance",
		"NR_LICENSE_KEY":     "lic",
		"KEK":                strings.Repeat("ab", 32),
		"SMTP_HOST":          "mailpit",
		"SMTP_PORT":          "1025",
		"SMTP_USER":          "user",
		"SMTP_PASS":          "pass",
		"SMTP_FROM":          "finance@example.com",
		"SMTP_TLS_POLICY":    "none",
		"LLM_BASE_URL":       "http://llama:8080/v1",
		"LLM_API_KEY":        "key",
		"LLM_MODEL":          "model",
	}
	cfg, err := load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("load with full env: %v", err)
	}
	if cfg.Port != 9090 {
		t.Errorf("Port = %d, want 9090", cfg.Port)
	}
	if cfg.DatabaseURL != env["DATABASE_URL"] {
		t.Errorf("DatabaseURL = %q", cfg.DatabaseURL)
	}
	if cfg.OIDCIssuer != env["OIDC_ISSUER"] || cfg.OIDCAudience != env["OIDC_AUDIENCE"] ||
		cfg.OIDCClientID != env["OIDC_CLIENT_ID"] || cfg.OIDCClientSecret != env["OIDC_CLIENT_SECRET"] {
		t.Errorf("OIDC vars not loaded: %+v", cfg)
	}
	if cfg.OIDCDiscoveryURL != env["OIDC_DISCOVERY_URL"] {
		t.Errorf("OIDCDiscoveryURL = %q, want %q", cfg.OIDCDiscoveryURL, env["OIDC_DISCOVERY_URL"])
	}
	if cfg.SMTP.Host != "mailpit" || cfg.SMTP.Port != 1025 || cfg.SMTP.From != "finance@example.com" || cfg.SMTP.TLSPolicy != "none" {
		t.Errorf("SMTP not loaded: %+v", cfg.SMTP)
	}
	if cfg.LLM.BaseURL != env["LLM_BASE_URL"] || cfg.LLM.Model != "model" {
		t.Errorf("LLM not loaded: %+v", cfg.LLM)
	}
}

func TestLoadInvalid(t *testing.T) {
	cases := []struct {
		name string
		key  string
		val  string
	}{
		{"bad port", "PORT", "notanumber"},
		{"bad smtp port", "SMTP_PORT", "abc"},
		{"bad tls policy", "SMTP_TLS_POLICY", "off"},
		{"empty kek", "KEK", ""},
		{"all-zero kek", "KEK", strings.Repeat("0", 64)},
		{"short kek", "KEK", "abc123"},
		{"non-hex kek", "KEK", strings.Repeat("zz", 32)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := load(func(k string) string {
				if k == tc.key {
					return tc.val
				}
				return ""
			})
			if err == nil {
				t.Fatalf("expected error for %s=%s", tc.key, tc.val)
			}
		})
	}
}

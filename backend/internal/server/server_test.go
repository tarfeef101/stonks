package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"finance-tracker/internal/authn"
)

type fakeReady struct{ err error }

func (f fakeReady) Ping(context.Context) error { return f.err }

func do(t *testing.T, h http.Handler, path string, hdr map[string]string) (int, string, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(body), rec.Header().Get("Content-Type")
}

// testAuth builds a verifier whose keyfunc is inert. These tests only
// exercise the middleware's rejection path, which fails before key
// resolution, so no RSA key or network access is needed.
func testAuth() *authn.Verifier {
	v, err := authn.NewWithKeyFunc(
		authn.Config{Issuer: "http://issuer.test", Audience: "test-aud"},
		func(context.Context) (any, error) { return nil, nil },
	)
	if err != nil {
		panic(err)
	}
	return v
}

func TestAPINeedsToken(t *testing.T) {
	h := New(fakeReady{}, BuildInfo{}, testAuth())
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /api request status = %d, want 401", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Error("unauthenticated /api response is missing WWW-Authenticate")
	}
}

func TestHealthz(t *testing.T) {
	h := New(nil, BuildInfo{Version: "test", Commit: "abc"}, testAuth())
	code, body, _ := do(t, h, "/healthz", nil)
	if code != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200 (body %s)", code, body)
	}
	if !strings.Contains(body, `"status":"ok"`) {
		t.Errorf("healthz body = %s", body)
	}
}

func TestReadyzNoDatabase(t *testing.T) {
	h := New(nil, BuildInfo{Version: "test", Commit: "abc"}, testAuth())
	code, body, _ := do(t, h, "/readyz", nil)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d, want 503 (body %s)", code, body)
	}
	if !strings.Contains(body, "no database configured") {
		t.Errorf("readyz body = %s", body)
	}
}

func TestReadyzDatabaseUnreachable(t *testing.T) {
	h := New(fakeReady{err: errors.New("connection refused")}, BuildInfo{}, testAuth())
	code, body, _ := do(t, h, "/readyz", nil)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d, want 503 (body %s)", code, body)
	}
	if !strings.Contains(body, "database unreachable") {
		t.Errorf("readyz body = %s", body)
	}
}

func TestReadyzOK(t *testing.T) {
	h := New(fakeReady{}, BuildInfo{}, testAuth())
	code, body, _ := do(t, h, "/readyz", nil)
	if code != http.StatusOK {
		t.Fatalf("readyz status = %d, want 200 (body %s)", code, body)
	}
	if !strings.Contains(body, `"status":"ready"`) {
		t.Errorf("readyz body = %s", body)
	}
}

func TestMetrics(t *testing.T) {
	h := New(fakeReady{}, BuildInfo{Version: "v1", Commit: "c1"}, testAuth())

	// Exercise a request so the request counters exist.
	if code, body, _ := do(t, h, "/healthz", nil); code != http.StatusOK {
		t.Fatalf("healthz setup: %d %s", code, body)
	}

	for _, tc := range []struct {
		name   string
		accept string
		ct     string
	}{
		{"prometheus text", "", "text/plain"},
		{"openmetrics", "application/openmetrics-text; version=1.0.0", "application/openmetrics-text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body, ct := do(t, h, "/metrics", map[string]string{"Accept": tc.accept})
			if code != http.StatusOK {
				t.Fatalf("metrics status = %d, want 200 (body: %.400s)", code, body)
			}
			if !strings.Contains(ct, tc.ct) {
				t.Errorf("content-type = %q, want to contain %q", ct, tc.ct)
			}
			for _, want := range []string{"ftrk_build_info", `version="v1"`, "ftrk_http_requests_total"} {
				if !strings.Contains(body, want) {
					t.Errorf("metrics body missing %q", want)
				}
			}
		})
	}
}

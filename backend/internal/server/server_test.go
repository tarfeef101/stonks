package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"finance-tracker/internal/authn"
	"finance-tracker/internal/crypto"
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
	h := New(fakeReady{}, BuildInfo{}, testAuth(), nil, nil, nil, nil, nil, nil, nil, nil, nil)
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

// TestMe covers the step 1.3c done-when end to end through the router:
// a valid token idempotently creates exactly one users row with a
// KEK-wrapped DEK, and repeat logins return the stored row.
func TestMe(t *testing.T) {
	key := mintKey(t)
	kek := testKEK(t)
	store := newMemStore()
	b, err := NewBootstrap(store, kek)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	h := New(fakeReady{}, BuildInfo{}, liveAuth(t, key), b, nil, nil, nil, nil, nil, nil, nil, nil)
	token := mintIdentityToken(t, key, "me-sub", "me@localhost", "Me Name", "meuser")

	code, body, _ := do(t, h, "/api/v1/me", map[string]string{"Authorization": "Bearer " + token})
	if code != http.StatusOK {
		t.Fatalf("me status = %d, want 200 (body %s)", code, body)
	}
	for _, want := range []string{`"sub":"me-sub"`, `"email":"me@localhost"`, `"display_name":"Me Name"`} {
		if !strings.Contains(body, want) {
			t.Errorf("me body %s missing %s", body, want)
		}
	}
	if store.count() != 1 {
		t.Fatalf("rows after first login: want 1, got %d", store.count())
	}
	row, ok := store.get("me-sub")
	if !ok {
		t.Fatal("row for me-sub missing")
	}
	if len(row.WrappedDEK) < crypto.PackMinSize {
		t.Fatalf("stored wrapped DEK too short: %d bytes", len(row.WrappedDEK))
	}
	if dek, err := crypto.UnwrapDEK(kek, row.WrappedDEK); err != nil || len(dek) != crypto.DEKSize {
		t.Fatalf("stored DEK does not unwrap to %d bytes: err=%v len=%d", crypto.DEKSize, err, len(dek))
	}

	// Repeat login: still one row, same ID, identical stored DEK.
	code, _, _ = do(t, h, "/api/v1/me", map[string]string{"Authorization": "Bearer " + token})
	if code != http.StatusOK {
		t.Fatalf("me repeat status = %d, want 200", code)
	}
	if store.count() != 1 {
		t.Fatalf("rows after repeat login: want 1, got %d", store.count())
	}
	again, _ := store.get("me-sub")
	if again.ID != row.ID || !bytes.Equal(again.WrappedDEK, row.WrappedDEK) {
		t.Fatal("repeat login changed the stored row")
	}
}

// TestMeNoBootstrap: with a valid token but no database configured, /me
// answers 503 (the API is intentionally operable without a DB for the
// probes/metrics in steps before 1.3c wiring).
func TestMeNoBootstrap(t *testing.T) {
	key := mintKey(t)
	h := New(nil, BuildInfo{}, liveAuth(t, key), nil, nil, nil, nil, nil, nil, nil, nil, nil)
	token := mintIdentityToken(t, key, "no-db-sub", "n@localhost", "N", "n")

	code, body, _ := do(t, h, "/api/v1/me", map[string]string{"Authorization": "Bearer " + token})
	if code != http.StatusServiceUnavailable {
		t.Fatalf("me (no bootstrap) status = %d, want 503 (body %s)", code, body)
	}
	if !strings.Contains(body, "database not configured") {
		t.Errorf("me (no bootstrap) body = %s", body)
	}
}

func TestHealthz(t *testing.T) {
	h := New(nil, BuildInfo{Version: "test", Commit: "abc"}, testAuth(), nil, nil, nil, nil, nil, nil, nil, nil, nil)
	code, body, _ := do(t, h, "/healthz", nil)
	if code != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200 (body %s)", code, body)
	}
	if !strings.Contains(body, `"status":"ok"`) {
		t.Errorf("healthz body = %s", body)
	}
}

func TestReadyzNoDatabase(t *testing.T) {
	h := New(nil, BuildInfo{Version: "test", Commit: "abc"}, testAuth(), nil, nil, nil, nil, nil, nil, nil, nil, nil)
	code, body, _ := do(t, h, "/readyz", nil)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d, want 503 (body %s)", code, body)
	}
	if !strings.Contains(body, "no database configured") {
		t.Errorf("readyz body = %s", body)
	}
}

func TestReadyzDatabaseUnreachable(t *testing.T) {
	h := New(fakeReady{err: errors.New("connection refused")}, BuildInfo{}, testAuth(), nil, nil, nil, nil, nil, nil, nil, nil, nil)
	code, body, _ := do(t, h, "/readyz", nil)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d, want 503 (body %s)", code, body)
	}
	if !strings.Contains(body, "database unreachable") {
		t.Errorf("readyz body = %s", body)
	}
}

func TestReadyzOK(t *testing.T) {
	h := New(fakeReady{}, BuildInfo{}, testAuth(), nil, nil, nil, nil, nil, nil, nil, nil, nil)
	code, body, _ := do(t, h, "/readyz", nil)
	if code != http.StatusOK {
		t.Fatalf("readyz status = %d, want 200 (body %s)", code, body)
	}
	if !strings.Contains(body, `"status":"ready"`) {
		t.Errorf("readyz body = %s", body)
	}
}

func TestMetrics(t *testing.T) {
	h := New(fakeReady{}, BuildInfo{Version: "v1", Commit: "c1"}, testAuth(), nil, nil, nil, nil, nil, nil, nil, nil, nil)

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

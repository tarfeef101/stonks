// Settings endpoint tests (plan step 1.4a). The in-memory store tests
// cover the handler contract (auth, 503s, validation, the one-way
// onboarded flag, the 3.1 error envelope) without infrastructure;
// TestPGSettingsStore proves the real SQL path (defaults, persistence,
// COALESCE semantics, per-user isolation) on a postgres:18 testcontainer
// via `make backend-test` (D-019).
package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"finance-tracker/internal/authn"
)

// memSettingsStore is an in-memory SettingsStore. Get auto-creates a
// row at the DB defaults (0001) for unknown subs, mirroring production
// where the handler bootstraps the users row before any store call.
type memSettingsStore struct {
	mu        sync.Mutex
	rows      map[string]Settings
	getErr    error
	updateErr error
}

func newMemSettingsStore() *memSettingsStore {
	return &memSettingsStore{rows: map[string]Settings{}}
}

func defaultSettings() Settings {
	return Settings{
		Language:        "en",
		DefaultCurrency: "USD",
		DefaultPeriod:   "month",
	}
}

func (m *memSettingsStore) row(sub string) (Settings, bool) {
	st, ok := m.rows[sub]
	return st, ok
}

func (m *memSettingsStore) Get(_ context.Context, sub string) (Settings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getErr != nil {
		return Settings{}, m.getErr
	}
	if st, ok := m.rows[sub]; ok {
		return st, nil
	}
	return defaultSettings(), nil
}

func (m *memSettingsStore) Update(_ context.Context, sub string, p SettingsPatch) (Settings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.updateErr != nil {
		return Settings{}, m.updateErr
	}
	// Auto-create at the DB defaults, as Get does: production bootstraps
	// the users row before any store call, so the store never sees a
	// missing row mid-update.
	st, ok := m.row(sub)
	if !ok {
		st = defaultSettings()
	}
	if p.Email != nil {
		st.Email = *p.Email
	}
	if p.DisplayName != nil {
		st.DisplayName = *p.DisplayName
	}
	if p.Language != nil {
		st.Language = *p.Language
	}
	if p.DefaultCurrency != nil {
		st.DefaultCurrency = *p.DefaultCurrency
	}
	if p.DefaultPeriod != nil {
		st.DefaultPeriod = *p.DefaultPeriod
	}
	if p.NotifyMonthly != nil {
		st.NotifyMonthly = *p.NotifyMonthly
	}
	if p.NotifyQuarterly != nil {
		st.NotifyQuarterly = *p.NotifyQuarterly
	}
	if p.NotifyAnnual != nil {
		st.NotifyAnnual = *p.NotifyAnnual
	}
	// Mirror the SQL COALESCE: only-if-unset, never clear, never overwrite.
	if p.Onboarded != nil && st.OnboardedAt == nil {
		now := time.Now().UTC()
		st.OnboardedAt = &now
	}
	m.rows[sub] = st
	return st, nil
}

// newSettingsServer builds a router with a live token verifier, an
// in-memory bootstrap (user store), and the given settings store (nil
// allowed — the 503 path). Returns the handler and a minted access
// token for sub.
func newSettingsServer(t *testing.T, sub string, store SettingsStore) (http.Handler, string) {
	t.Helper()
	key := mintKey(t)
	b, err := NewBootstrap(newMemStore(), testKEK(t))
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	h := New(fakeReady{}, BuildInfo{}, liveAuth(t, key), b, store, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	return h, mintIdentityToken(t, key, sub, sub+"@localhost", sub, sub)
}

// doReq runs a request with an arbitrary method and an optional raw body
// (do() in server_test.go is GET-only).
func doReq(t *testing.T, h http.Handler, method, path, rawBody string, hdr map[string]string) (int, string) {
	t.Helper()
	// Untyped io.Reader (not *strings.Reader): a typed nil would be a
	// non-nil interface and panic inside httptest.NewRequest.
	var body io.Reader
	if rawBody != "" {
		body = strings.NewReader(rawBody)
	}
	req := httptest.NewRequest(method, path, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	out, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(out)
}

func TestGetSettings(t *testing.T) {
	store := newMemSettingsStore()
	h, token := newSettingsServer(t, "get-sub", store)

	code, body := doReq(t, h, http.MethodGet, "/api/v1/settings", "", map[string]string{"Authorization": "Bearer " + token})
	if code != http.StatusOK {
		t.Fatalf("GET settings status = %d, want 200 (body %s)", code, body)
	}
	var st Settings
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatalf("body is not Settings JSON: %v (%s)", err, body)
	}
	want := defaultSettings()
	if st != want {
		t.Fatalf("fresh settings = %+v, want DB defaults %+v", st, want)
	}
	if st.OnboardedAt != nil {
		t.Errorf("fresh onboarded_at = %v, want nil", *st.OnboardedAt)
	}
	if !strings.Contains(body, `"onboarded_at":null`) {
		t.Errorf("onboarded_at must serialize as JSON null: %s", body)
	}
}

func TestPatchSettings(t *testing.T) {
	store := newMemSettingsStore()
	h, token := newSettingsServer(t, "patch-sub", store)
	hdr := map[string]string{"Authorization": "Bearer " + token}

	patch, _ := json.Marshal(SettingsPatch{
		DefaultCurrency: strPtr("CAD"),
		Language:        strPtr("fr"),
		NotifyMonthly:   boolPtr(true),
	})
	code, body := doReq(t, h, http.MethodPatch, "/api/v1/settings", string(patch), hdr)
	if code != http.StatusOK {
		t.Fatalf("PATCH settings status = %d, want 200 (body %s)", code, body)
	}
	var st Settings
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatalf("body is not Settings JSON: %v (%s)", err, body)
	}
	if st.DefaultCurrency != "CAD" || st.Language != "fr" || !st.NotifyMonthly {
		t.Errorf("patched fields not applied: %+v", st)
	}
	// Untouched fields keep their stored values (DB defaults here).
	if st.DefaultPeriod != "month" || st.NotifyQuarterly || st.NotifyAnnual || st.OnboardedAt != nil {
		t.Errorf("untouched fields changed: %+v", st)
	}

	// A follow-up GET returns the persisted state (one round trip saved
	// the frontend the extra call, but the store must agree).
	code, body = doReq(t, h, http.MethodGet, "/api/v1/settings", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("GET after PATCH status = %d, want 200", code)
	}
	if !strings.Contains(body, `"default_currency":"CAD"`) || !strings.Contains(body, `"language":"fr"`) {
		t.Errorf("GET after PATCH did not persist: %s", body)
	}
}

func TestPatchSettingsValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string // substring of the error message
	}{
		{"empty patch", `{}`, "empty patch"},
		{"empty email", `{"email": ""}`, "email must not be empty"},
		{"lowercase currency", `{"default_currency": "cad"}`, "default_currency"},
		{"four-letter currency", `{"default_currency": "CA$"}`, "default_currency"},
		{"week period", `{"default_period": "week"}`, "default_period"},
		{"onboarded false", `{"onboarded": false}`, "onboarded must be true"},
		{"non-JSON body", `{`, "JSON object"},
		{"JSON array body", `[1]`, "JSON object"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemSettingsStore()
			h, token := newSettingsServer(t, "val-sub", store)
			code, body := doReq(t, h, http.MethodPatch, "/api/v1/settings", tc.body,
				map[string]string{"Authorization": "Bearer " + token})
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", code, body)
			}
			// The 3.1 envelope: {error: {code, message}}.
			var env struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(body), &env); err != nil {
				t.Fatalf("body is not the error envelope: %v (%s)", err, body)
			}
			if env.Error.Code != "invalid_request" {
				t.Errorf("code = %q, want invalid_request", env.Error.Code)
			}
			if !strings.Contains(env.Error.Message, tc.want) {
				t.Errorf("message %q does not contain %q", env.Error.Message, tc.want)
			}
		})
	}
}

func TestPatchOnboardedOneWay(t *testing.T) {
	store := newMemSettingsStore()
	h, token := newSettingsServer(t, "ob-sub", store)
	hdr := map[string]string{"Authorization": "Bearer " + token}

	code, body := doReq(t, h, http.MethodPatch, "/api/v1/settings", `{"onboarded": true}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("onboard status = %d, want 200 (body %s)", code, body)
	}
	var first Settings
	if err := json.Unmarshal([]byte(body), &first); err != nil || first.OnboardedAt == nil {
		t.Fatalf("first onboarded_at missing: err=%v body=%s", err, body)
	}

	// A later patch must NOT move the timestamp (COALESCE semantics).
	code, body = doReq(t, h, http.MethodPatch, "/api/v1/settings", `{"language": "de"}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("patch after onboard status = %d, want 200 (body %s)", code, body)
	}
	var second Settings
	if err := json.Unmarshal([]byte(body), &second); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if second.OnboardedAt == nil || !second.OnboardedAt.Equal(*first.OnboardedAt) {
		t.Fatalf("onboarded_at changed: %v -> %v", *first.OnboardedAt, second.OnboardedAt)
	}
	if second.Language != "de" {
		t.Errorf("language patch lost alongside onboarded: %+v", second)
	}
}

func TestSettingsUnauthorized(t *testing.T) {
	h, _ := newSettingsServer(t, "auth-sub", newMemSettingsStore())
	for _, method := range []string{http.MethodGet, http.MethodPatch} {
		code, _ := doReq(t, h, method, "/api/v1/settings", "", nil)
		if code != http.StatusUnauthorized {
			t.Errorf("%s without token: status = %d, want 401", method, code)
		}
	}
}

func TestSettingsNotConfigured(t *testing.T) {
	// Either dependency missing (no pool in main.go) answers 503 internal.
	bootstrap, err := NewBootstrap(newMemStore(), testKEK(t))
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	for _, tc := range []struct {
		name      string
		bootstrap *Bootstrap
		settings  SettingsStore
	}{
		{"no bootstrap", nil, newMemSettingsStore()},
		{"no settings store", bootstrap, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := mintKey(t)
			h := New(fakeReady{}, BuildInfo{}, liveAuth(t, key), tc.bootstrap, tc.settings, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			token := mintIdentityToken(t, key, "nc-sub", "n@localhost", "N", "n")
			code, body := doReq(t, h, http.MethodGet, "/api/v1/settings", "",
				map[string]string{"Authorization": "Bearer " + token})
			if code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503 (body %s)", code, body)
			}
			if !strings.Contains(body, `"code":"internal"`) {
				t.Errorf("body missing internal code: %s", body)
			}
		})
	}
}

func TestSettingsNotFound(t *testing.T) {
	store := newMemSettingsStore()
	store.getErr = ErrSettingsNotFound
	h, token := newSettingsServer(t, "nf-sub", store)
	code, body := doReq(t, h, http.MethodGet, "/api/v1/settings", "",
		map[string]string{"Authorization": "Bearer " + token})
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", code, body)
	}
	if !strings.Contains(body, `"code":"not_found"`) {
		t.Errorf("body missing not_found code: %s", body)
	}
}

// TestPGSettingsStore exercises the real SQL store against a migrated
// postgres:18 container: the 0001 defaults survive a fresh bootstrap,
// updates persist, onboarded_at is one-way, and users are isolated.
func TestPGSettingsStore(t *testing.T) {
	pool, ctx := migratedPool(t)
	defer pool.Close()

	kek := testKEK(t)
	b, err := NewBootstrap(NewPGUserStore(pool), kek)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	// EnsureUser creates the users row (as /v1/me and the settings
	// handlers do); the settings store then operates on it.
	for _, sub := range []string{"pg-sub-a", "pg-sub-b"} {
		if _, err := b.EnsureUser(ctx, authn.Identity{Sub: sub, Email: sub + "@localhost"}); err != nil {
			t.Fatalf("ensure %s: %v", sub, err)
		}
	}
	store := NewPGSettingsStore(pool)

	st, err := store.Get(ctx, "pg-sub-a")
	if err != nil {
		t.Fatalf("get fresh: %v", err)
	}
	if st.Language != "en" || st.DefaultCurrency != "USD" || st.DefaultPeriod != "month" ||
		st.NotifyMonthly || st.OnboardedAt != nil {
		t.Fatalf("fresh row is not at DB defaults: %+v", st)
	}
	if st.Email != "pg-sub-a@localhost" {
		t.Errorf("email = %q, want the bootstrapped claim", st.Email)
	}

	updated, err := store.Update(ctx, "pg-sub-a", SettingsPatch{
		DefaultCurrency: strPtr("CAD"),
		NotifyAnnual:    boolPtr(true),
		Onboarded:       boolPtr(true),
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.DefaultCurrency != "CAD" || !updated.NotifyAnnual || updated.OnboardedAt == nil {
		t.Fatalf("update not reflected in response: %+v", updated)
	}
	firstAt := *updated.OnboardedAt

	// Persisted across a fresh Get; the one-way flag did not leak to the
	// other user.
	again, err := store.Get(ctx, "pg-sub-a")
	if err != nil {
		t.Fatalf("re-get: %v", err)
	}
	if again.DefaultCurrency != "CAD" || !again.OnboardedAt.Equal(firstAt) {
		t.Fatalf("update not persisted: %+v", again)
	}
	other, err := store.Get(ctx, "pg-sub-b")
	if err != nil {
		t.Fatalf("get other: %v", err)
	}
	if other.DefaultCurrency != "USD" || other.OnboardedAt != nil {
		t.Fatalf("settings leaked across users: %+v", other)
	}

	// COALESCE: a later patch without onboarded keeps the original stamp;
	// an explicit second onboarded:true does the same.
	late, err := store.Update(ctx, "pg-sub-a", SettingsPatch{Language: strPtr("fr")})
	if err != nil {
		t.Fatalf("late update: %v", err)
	}
	if !late.OnboardedAt.Equal(firstAt) {
		t.Fatalf("onboarded_at moved without a flag: %v -> %v", firstAt, *late.OnboardedAt)
	}
	late, err = store.Update(ctx, "pg-sub-a", SettingsPatch{Onboarded: boolPtr(true)})
	if err != nil {
		t.Fatalf("second onboard: %v", err)
	}
	if !late.OnboardedAt.Equal(firstAt) {
		t.Fatalf("onboarded_at overwritten: %v -> %v", firstAt, *late.OnboardedAt)
	}
	if late.Language != "fr" {
		t.Errorf("language lost on second onboard: %+v", late)
	}

	// A missing sub is ErrSettingsNotFound (defensive — the handler
	// bootstraps first).
	if _, err := store.Get(ctx, "pg-no-such-sub"); err != ErrSettingsNotFound {
		t.Fatalf("missing sub: want ErrSettingsNotFound, got %v", err)
	}
}

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }

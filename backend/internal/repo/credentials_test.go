// Credential store tests (plan step 2.6, second half of the 2.6 split):
// one shared postgres:18 testcontainer (newTestEnv) with subtests
// covering the Put/Get round trip, the masked-key behavior (plan 2.6
// done-when: the full key is never returned), the in-place upsert
// (UNIQUE (user_id, provider)), validation, list order, and per-user
// isolation. TestMaskKey is pure (no DB).
package repo

import (
	"errors"
	"strings"
	"testing"
)

func TestMaskKey(t *testing.T) {
	cases := []struct {
		name string
		key  string
		want string
	}{
		{"empty", "", "****"},
		{"8 runes, fully masked", "12345678", "****"},
		{"9 runes, first4…last4", "123456789", "1234…6789"},
		{"long key", "sk-abc123def456ghi789", "sk-a…i789"},
		// Rune-safety: multibyte runes must not split mid-character.
		{"8 runes multibyte", "€€€€€€€€", "****"},
		{"9 runes multibyte", "€€€€€€€€€", "€€€€…€€€€"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := maskKey(tc.key); got != tc.want {
				t.Fatalf("maskKey(%q): want %q, got %q", tc.key, tc.want, got)
			}
		})
	}
	if maskOf(nil) != "" {
		t.Fatalf("maskOf(nil) must be empty")
	}
	empty := ""
	if maskOf(&empty) != "" {
		t.Fatalf("maskOf(&\"\") must be empty")
	}
	k := "sk-abc123"
	if got, want := maskOf(&k), "sk-a…c123"; got != want {
		t.Fatalf("maskOf(key): want %q, got %q", want, got)
	}
}

func TestCredentialStore(t *testing.T) {
	env := newTestEnv(t, "cred-a", "cred-b")
	store := NewCredentialStore(env.pool)
	if store == nil {
		t.Fatal("non-nil pool must give a store")
	}
	if NewCredentialStore(nil) != nil {
		t.Fatal("nil pool must give nil store")
	}
	a := env.user(t, "cred-a")
	b := env.user(t, "cred-b")
	ctx := env.ctx
	aid, bid := a.row.ID, b.row.ID

	t.Run("put + get round trip", func(t *testing.T) {
		// self_hosted takes no key: the mask stays "".
		self, err := store.Put(ctx, aid, CredentialIn{
			Provider: "self_hosted", Endpoint: "  http://localhost:8080/v1  ",
		}, a.dek)
		if err != nil {
			t.Fatalf("put self_hosted: %v", err)
		}
		if self.Provider != "self_hosted" || self.Endpoint != "http://localhost:8080/v1" {
			t.Fatalf("self_hosted: %+v", self)
		}
		if self.APIKeyMasked != "" {
			t.Fatalf("keyless row must have an empty mask, got %q", self.APIKeyMasked)
		}
		got, err := store.Get(ctx, aid, "self_hosted", a.dek)
		if err != nil {
			t.Fatalf("get self_hosted: %v", err)
		}
		if got.Provider != self.Provider || got.Endpoint != self.Endpoint || got.APIKeyMasked != "" ||
			!got.CreatedAt.Equal(self.CreatedAt) || !got.UpdatedAt.Equal(self.UpdatedAt) {
			t.Fatalf("get: want %+v, got %+v", self, got)
		}

		// openai with a long key: the mask is first4…last4, never the key.
		key := "sk-test-1234567890abcdef"
		oa, err := store.Put(ctx, aid, CredentialIn{
			Provider: "openai", Endpoint: "https://api.openai.com/v1", APIKey: strPtr(key),
		}, a.dek)
		if err != nil {
			t.Fatalf("put openai: %v", err)
		}
		if oa.APIKeyMasked != "sk-t…cdef" {
			t.Fatalf("openai mask: want %q, got %q", "sk-t…cdef", oa.APIKeyMasked)
		}
		if strings.Contains(oa.Endpoint, key) || strings.Contains(oa.APIKeyMasked, key) {
			t.Fatalf("the full key must never be returned: %+v", oa)
		}
		got, err = store.Get(ctx, aid, "openai", a.dek)
		if err != nil {
			t.Fatalf("get openai: %v", err)
		}
		if got.APIKeyMasked != oa.APIKeyMasked {
			t.Fatalf("get openai mask: want %q, got %q", oa.APIKeyMasked, got.APIKeyMasked)
		}

		// A short key (<= 8 runes) is fully masked.
		short := "ab12cd"
		cust, err := store.Put(ctx, aid, CredentialIn{
			Provider: "custom", Endpoint: "https://llm.example.com/v1", APIKey: strPtr(short),
		}, a.dek)
		if err != nil {
			t.Fatalf("put custom: %v", err)
		}
		if cust.APIKeyMasked != "****" {
			t.Fatalf("short key mask: want \"****\", got %q", cust.APIKeyMasked)
		}

		if err := store.Delete(ctx, aid, "self_hosted"); err != nil {
			t.Fatalf("cleanup delete self_hosted: %v", err)
		}
		if err := store.Delete(ctx, aid, "openai"); err != nil {
			t.Fatalf("cleanup delete openai: %v", err)
		}
		if err := store.Delete(ctx, aid, "custom"); err != nil {
			t.Fatalf("cleanup delete custom: %v", err)
		}
	})

	t.Run("upsert in place", func(t *testing.T) {
		key1, key2 := "firstkey-1111111", "secondkey-2222222"
		first, err := store.Put(ctx, aid, CredentialIn{
			Provider: "openai", Endpoint: "https://one.example/v1", APIKey: strPtr(key1),
		}, a.dek)
		if err != nil {
			t.Fatalf("first put: %v", err)
		}
		// Re-Put with the same provider: endpoint + key update in place,
		// created_at stays, updated_at moves, no second row appears.
		second, err := store.Put(ctx, aid, CredentialIn{
			Provider: "openai", Endpoint: "https://two.example/v1", APIKey: strPtr(key2),
		}, a.dek)
		if err != nil {
			t.Fatalf("second put: %v", err)
		}
		if second.Endpoint != "https://two.example/v1" {
			t.Fatalf("endpoint must update: %+v", second)
		}
		if second.APIKeyMasked != "seco…2222" {
			t.Fatalf("key must update, mask want %q, got %q", "seco…2222", second.APIKeyMasked)
		}
		if !second.CreatedAt.Equal(first.CreatedAt) {
			t.Fatalf("created_at must stay: %v -> %v", first.CreatedAt, second.CreatedAt)
		}
		if !second.UpdatedAt.After(first.UpdatedAt) {
			t.Fatalf("updated_at must move: %v -> %v", first.UpdatedAt, second.UpdatedAt)
		}
		list, err := store.List(ctx, aid, a.dek)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		openai := 0
		for _, c := range list {
			if c.Provider == "openai" {
				openai++
			}
		}
		if openai != 1 {
			t.Fatalf("upsert must not duplicate: %d openai rows in %+v", openai, list)
		}
		if err := store.Delete(ctx, aid, "openai"); err != nil {
			t.Fatalf("cleanup delete: %v", err)
		}
	})

	t.Run("validation, rejected before the DB", func(t *testing.T) {
		bad := []struct {
			name string
			in   CredentialIn
		}{
			{"bad provider", CredentialIn{Provider: "anthropic", Endpoint: "https://x.example"}},
			{"empty endpoint", CredentialIn{Provider: "openai", Endpoint: "   ", APIKey: strPtr("k1234567890")}},
			{"endpoint too long", CredentialIn{Provider: "self_hosted", Endpoint: strings.Repeat("e", 2049)}},
			{"openai without key", CredentialIn{Provider: "openai", Endpoint: "https://api.openai.com/v1"}},
			{"openai with empty key", CredentialIn{Provider: "openai", Endpoint: "https://api.openai.com/v1", APIKey: strPtr("")}},
			{"custom without key", CredentialIn{Provider: "custom", Endpoint: "https://x.example/v1"}},
		}
		for _, tc := range bad {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := store.Put(ctx, aid, tc.in, a.dek); err == nil {
					t.Fatal("want rejection, got nil")
				}
			})
		}
		// The boundary that DOES fit is accepted (then cleaned up).
		ok, err := store.Put(ctx, aid, CredentialIn{Provider: "self_hosted", Endpoint: strings.Repeat("e", 2048)}, a.dek)
		if err != nil {
			t.Fatalf("2048-char endpoint must be accepted: %v", err)
		}
		if len(ok.Endpoint) != 2048 {
			t.Fatalf("endpoint length: want 2048, got %d", len(ok.Endpoint))
		}
		if err := store.Delete(ctx, aid, "self_hosted"); err != nil {
			t.Fatalf("cleanup delete: %v", err)
		}
	})

	t.Run("list order", func(t *testing.T) {
		for _, p := range []string{"openai", "self_hosted", "custom"} {
			in := CredentialIn{Provider: p, Endpoint: "https://" + p + ".example/v1"}
			if p != "self_hosted" {
				in.APIKey = strPtr("key-" + p + "-123456789")
			}
			if _, err := store.Put(ctx, aid, in, a.dek); err != nil {
				t.Fatalf("put %s: %v", p, err)
			}
		}
		list, err := store.List(ctx, aid, a.dek)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		want := []string{"custom", "openai", "self_hosted"} // alphabetical
		if len(list) != len(want) {
			t.Fatalf("list: want %d rows, got %d", len(want), len(list))
		}
		for i, p := range want {
			if list[i].Provider != p {
				t.Fatalf("row %d: want provider %s, got %s", i, p, list[i].Provider)
			}
		}
		for _, p := range want {
			if err := store.Delete(ctx, aid, p); err != nil {
				t.Fatalf("cleanup delete %s: %v", p, err)
			}
		}
	})

	t.Run("per-user scoping", func(t *testing.T) {
		mine, err := store.Put(ctx, aid, CredentialIn{
			Provider: "openai", Endpoint: "https://mine.example/v1", APIKey: strPtr("sk-mine-987654321"),
		}, a.dek)
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		// User b must not read or delete a's credential — and the row
		// must survive. b's own Put for the same provider is a separate
		// row (UNIQUE is (user_id, provider)).
		if _, err := store.Get(ctx, bid, "openai", b.dek); !errors.Is(err, ErrCredentialNotFound) {
			t.Fatalf("b reads a's credential: want ErrCredentialNotFound, got %v", err)
		}
		if err := store.Delete(ctx, bid, "openai"); !errors.Is(err, ErrCredentialNotFound) {
			t.Fatalf("b deletes a's credential: want ErrCredentialNotFound, got %v", err)
		}
		brown, err := store.Put(ctx, bid, CredentialIn{
			Provider: "openai", Endpoint: "https://b.example/v1", APIKey: strPtr("sk-b-1122334455"),
		}, b.dek)
		if err != nil {
			t.Fatalf("b's own put: %v", err)
		}
		if brown.CreatedAt.Equal(mine.CreatedAt) && brown.UpdatedAt.Equal(mine.UpdatedAt) && brown.Endpoint == mine.Endpoint {
			t.Fatal("b's row must be distinct from a's row")
		}
		if _, err := store.Get(ctx, aid, "openai", a.dek); err != nil {
			t.Fatalf("a's row must survive b's attempts: %v", err)
		}
		lb, err := store.List(ctx, bid, b.dek)
		if err != nil {
			t.Fatalf("list b: %v", err)
		}
		for _, c := range lb {
			if c.Endpoint == mine.Endpoint {
				t.Fatalf("b's list leaked a's credential: %+v", c)
			}
		}
		if err := store.Delete(ctx, aid, "openai"); err != nil {
			t.Fatalf("cleanup delete a: %v", err)
		}
		if err := store.Delete(ctx, bid, "openai"); err != nil {
			t.Fatalf("cleanup delete b: %v", err)
		}
	})

	t.Run("delete unknown provider", func(t *testing.T) {
		if err := store.Delete(ctx, aid, "self_hosted"); !errors.Is(err, ErrCredentialNotFound) {
			t.Fatalf("want ErrCredentialNotFound, got %v", err)
		}
	})
}

func TestCredentialProvidersVocabulary(t *testing.T) {
	want := []string{"self_hosted", "openai", "custom"}
	if len(CredentialProviders) != len(want) {
		t.Fatalf("CredentialProviders: want %v, got %v", want, CredentialProviders)
	}
	for i := range want {
		if CredentialProviders[i] != want[i] {
			t.Fatalf("CredentialProviders: want %v, got %v", want, CredentialProviders)
		}
	}
}

func TestCredentialResolve(t *testing.T) {
	env := newTestEnv(t, "res-a", "res-b")
	store := NewCredentialStore(env.pool)
	a := env.user(t, "res-a")
	b := env.user(t, "res-b")
	ctx := env.ctx

	t.Run("no rows", func(t *testing.T) {
		if _, err := store.Resolve(ctx, a.row.ID, a.dek); !errors.Is(err, ErrCredentialNotFound) {
			t.Fatalf("want ErrCredentialNotFound, got %v", err)
		}
	})

	t.Run("most recently updated wins", func(t *testing.T) {
		if _, err := store.Put(ctx, a.row.ID, CredentialIn{
			Provider: "openai", Endpoint: "https://api.openai.com/v1", APIKey: strPtr("sk-oa-000000000"),
		}, a.dek); err != nil {
			t.Fatalf("put openai: %v", err)
		}
		if _, err := store.Put(ctx, a.row.ID, CredentialIn{
			Provider: "custom", Endpoint: "https://custom.example/v1", APIKey: strPtr("sk-cu-111111111"),
		}, a.dek); err != nil {
			t.Fatalf("put custom: %v", err)
		}
		// custom was updated last, so it beats the older openai row even
		// though openai has higher priority.
		got, err := store.Resolve(ctx, a.row.ID, a.dek)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got.Provider != "custom" || got.Endpoint != "https://custom.example/v1" ||
			got.APIKey != "sk-cu-111111111" {
			t.Fatalf("resolve: %+v", got)
		}
		// Re-Put openai: its updated_at moves to now and it wins again.
		if _, err := store.Put(ctx, a.row.ID, CredentialIn{
			Provider: "openai", Endpoint: "https://api.openai.com/v1", APIKey: strPtr("sk-oa-000000000"),
		}, a.dek); err != nil {
			t.Fatalf("re-put openai: %v", err)
		}
		got, err = store.Resolve(ctx, a.row.ID, a.dek)
		if err != nil {
			t.Fatalf("resolve after re-put: %v", err)
		}
		if got.Provider != "openai" || got.APIKey != "sk-oa-000000000" {
			t.Fatalf("resolve: %+v", got)
		}
	})

	t.Run("tie breaks by provider priority", func(t *testing.T) {
		if _, err := store.Put(ctx, a.row.ID, CredentialIn{
			Provider: "self_hosted", Endpoint: "http://llama:8080/v1",
		}, a.dek); err != nil {
			t.Fatalf("put self_hosted: %v", err)
		}
		// Force an exact updated_at tie across all three rows.
		if _, err := env.pool.Exec(ctx, `
			UPDATE llm_credentials
			SET updated_at = '2026-01-01 00:00:00+00'
			WHERE user_id = $1`, a.row.ID); err != nil {
			t.Fatalf("force tie: %v", err)
		}
		got, err := store.Resolve(ctx, a.row.ID, a.dek)
		if err != nil {
			t.Fatalf("resolve tie: %v", err)
		}
		if got.Provider != "openai" {
			t.Fatalf("tie of all three: want openai, got %+v", got)
		}
		if err := store.Delete(ctx, a.row.ID, "openai"); err != nil {
			t.Fatalf("delete openai: %v", err)
		}
		got, err = store.Resolve(ctx, a.row.ID, a.dek)
		if err != nil {
			t.Fatalf("resolve tie of two: %v", err)
		}
		if got.Provider != "custom" || got.APIKey != "sk-cu-111111111" {
			t.Fatalf("tie of two: want custom, got %+v", got)
		}
		if err := store.Delete(ctx, a.row.ID, "custom"); err != nil {
			t.Fatalf("delete custom: %v", err)
		}
		got, err = store.Resolve(ctx, a.row.ID, a.dek)
		if err != nil {
			t.Fatalf("resolve keyless: %v", err)
		}
		if got.Provider != "self_hosted" || got.Endpoint != "http://llama:8080/v1" || got.APIKey != "" {
			t.Fatalf("keyless self_hosted: want empty APIKey, got %+v", got)
		}
	})

	t.Run("per-user scoping", func(t *testing.T) {
		if _, err := store.Put(ctx, b.row.ID, CredentialIn{
			Provider: "openai", Endpoint: "https://b.example/v1", APIKey: strPtr("sk-bb-222222222"),
		}, b.dek); err != nil {
			t.Fatalf("put b: %v", err)
		}
		got, err := store.Resolve(ctx, b.row.ID, b.dek)
		if err != nil {
			t.Fatalf("resolve b: %v", err)
		}
		if got.Provider != "openai" || got.APIKey != "sk-bb-222222222" || got.Endpoint != "https://b.example/v1" {
			t.Fatalf("resolve b: %+v", got)
		}
		// a's resolution is untouched by b's rows (no cross-user leak).
		got, err = store.Resolve(ctx, a.row.ID, a.dek)
		if err != nil {
			t.Fatalf("resolve a: %v", err)
		}
		if got.Provider != "self_hosted" || got.Endpoint != "http://llama:8080/v1" {
			t.Fatalf("a's resolve leaked b's row: %+v", got)
		}
		if err := store.Delete(ctx, b.row.ID, "openai"); err != nil {
			t.Fatalf("cleanup b: %v", err)
		}
		if err := store.Delete(ctx, a.row.ID, "self_hosted"); err != nil {
			t.Fatalf("cleanup a: %v", err)
		}
	})
}

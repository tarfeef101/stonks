package authn

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jws"
	"github.com/lestrrat-go/jwx/v3/jwt"
)

const (
	testIssuer   = "http://issuer.test"
	testAudience = "test-aud"
)

// mintKey generates an RSA private key as a jwk.Key.
func mintKey(t *testing.T) jwk.Key {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	jk, err := jwk.Import(k)
	if err != nil {
		t.Fatalf("import RSA key: %v", err)
	}
	return jk
}

// testVerifier builds a verifier whose keyfunc always returns the given
// key set, mirroring the JWKS provider contract without any network.
func testVerifier(t *testing.T, keys ...jwk.Key) (*Verifier, jwk.Key) {
	t.Helper()
	set := jwk.NewSet()
	for _, k := range keys {
		if err := set.AddKey(k); err != nil {
			t.Fatalf("add key to set: %v", err)
		}
	}
	v, err := NewWithKeyFunc(
		Config{Issuer: testIssuer, Audience: testAudience},
		func(context.Context) (any, error) { return set, nil },
	)
	if err != nil {
		t.Fatalf("build verifier: %v", err)
	}
	return v, keys[0]
}

// mintToken builds an RS256-signed JWT with the given claims.
func mintToken(t *testing.T, key jwk.Key, sub, iss, aud string) string {
	t.Helper()
	now := time.Now()
	tok, err := jwt.NewBuilder().
		Issuer(iss).
		Subject(sub).
		Audience([]string{aud}).
		IssuedAt(now).
		Expiration(now.Add(15 * time.Minute)).
		Build()
	if err != nil {
		t.Fatalf("build token: %v", err)
	}
	payload, err := jwt.NewSerializer().Serialize(tok)
	if err != nil {
		t.Fatalf("serialize token: %v", err)
	}
	signed, err := jws.Sign(payload, jws.WithKey(jwa.RS256(), key))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return string(signed)
}

// mintSkewed builds a token like mintToken whose exp is
// now.Add(offset) (negative = expired that long ago), to exercise the
// verifier's clock-skew leeway.
func mintSkewed(t *testing.T, key jwk.Key, sub, iss, aud string, offset time.Duration) string {
	t.Helper()
	now := time.Now()
	tok, err := jwt.NewBuilder().
		Issuer(iss).
		Subject(sub).
		Audience([]string{aud}).
		IssuedAt(now).
		Expiration(now.Add(offset)).
		Build()
	if err != nil {
		t.Fatalf("build token: %v", err)
	}
	payload, err := jwt.NewSerializer().Serialize(tok)
	if err != nil {
		t.Fatalf("serialize token: %v", err)
	}
	signed, err := jws.Sign(payload, jws.WithKey(jwa.RS256(), key))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return string(signed)
}

// doToken dispatches a GET through the middleware with the given bearer
// token ("" = no Authorization header) and returns status, body, and the
// WWW-Authenticate header.
func doToken(t *testing.T, v *Verifier, h http.Handler, token string) (int, string, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	v.Middleware(h).ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(body), rec.Header().Get("WWW-Authenticate")
}

func TestVerifierRejections(t *testing.T) {
	v, key := testVerifier(t, mintKey(t))

	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate second key: %v", err)
	}
	otherJK, err := jwk.Import(other)
	if err != nil {
		t.Fatalf("import second key: %v", err)
	}

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("inner handler must not be reached on rejection")
	})

	cases := []struct {
		name  string
		token string
	}{
		{"no token", ""},
		{"malformed token", "not-a-jwt"},
		{"bad signature", mintToken(t, otherJK, "sub-1", testIssuer, testAudience)},
		{"wrong issuer", mintToken(t, key, "sub-1", "http://other-issuer", testAudience)},
		{"wrong audience", mintToken(t, key, "sub-1", testIssuer, "some-other-aud")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body, wwt := doToken(t, v, inner, tc.token)
			if code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (body %s)", code, body)
			}
			if wwt == "" {
				t.Error("401 response is missing the WWW-Authenticate header")
			}
		})
	}
}

func TestVerifierAcceptsAndExposesSubject(t *testing.T) {
	v, key := testVerifier(t, mintKey(t))

	const want = "11111111-2222-3333-4444-555555555555"
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sub, err := v.Subject(r.Context())
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(err.Error()))
			return
		}
		_, _ = w.Write([]byte("sub=" + sub))
	})

	code, body, _ := doToken(t, v, h, mintToken(t, key, want, testIssuer, testAudience))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", code, body)
	}
	if body != "sub="+want {
		t.Fatalf("body = %q, want %q", body, "sub="+want)
	}
}

// TestVerifierClockSkew pins the exp/nbf leeway: a token that expired
// 10 s ago is still accepted (inside the 30 s skew), one expired 60 s
// ago is rejected.
func TestVerifierClockSkew(t *testing.T) {
	v, key := testVerifier(t, mintKey(t))
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})

	t.Run("expired 10s ago accepted", func(t *testing.T) {
		code, body, _ := doToken(t, v, inner,
			mintSkewed(t, key, "sub-1", testIssuer, testAudience, -10*time.Second))
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", code, body)
		}
	})
	t.Run("expired 60s ago rejected", func(t *testing.T) {
		code, body, _ := doToken(t, v, inner,
			mintSkewed(t, key, "sub-1", testIssuer, testAudience, -60*time.Second))
		if code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401 (body %s)", code, body)
		}
	})
}

// TestSubjectWithoutClaims guards the Subject() contract for contexts that
// never passed through the middleware.
func TestSubjectWithoutClaims(t *testing.T) {
	v, _ := testVerifier(t, mintKey(t))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	if _, err := v.Subject(req.Context()); err == nil {
		t.Fatal("Subject returned nil error without validated claims")
	}
}

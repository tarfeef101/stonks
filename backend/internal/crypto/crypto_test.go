package crypto

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func randBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return b
}

func testKeys(t *testing.T) (kek, dek []byte) {
	t.Helper()
	var err error
	kek = randBytes(t, KEKSize)
	dek, err = NewDEK()
	if err != nil {
		t.Fatalf("NewDEK: %v", err)
	}
	return kek, dek
}

func TestNewDEK(t *testing.T) {
	dek, err := NewDEK()
	if err != nil {
		t.Fatalf("NewDEK: %v", err)
	}
	if len(dek) != DEKSize {
		t.Fatalf("DEK size = %d, want %d", len(dek), DEKSize)
	}
	other, err := NewDEK()
	if err != nil {
		t.Fatalf("NewDEK: %v", err)
	}
	if subtle.ConstantTimeCompare(dek, other) == 1 {
		t.Fatal("two fresh DEKs are identical")
	}
}

// Round trip (wrap side): wrap → unwrap returns the same DEK, and the pack
// layout [nonce ‖ ciphertext ‖ tag] is pinned to 12+32+16 bytes.
func TestDEKRoundTrip(t *testing.T) {
	kek, dek := testKeys(t)

	wrapped, err := WrapDEK(kek, dek)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	if len(wrapped) != PackMinSize+DEKSize {
		t.Fatalf("wrapped pack = %d bytes, want %d (nonce+dek+tag)", len(wrapped), PackMinSize+DEKSize)
	}

	got, err := UnwrapDEK(kek, wrapped)
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}
	if subtle.ConstantTimeCompare(got, dek) != 1 {
		t.Fatal("unwrapped DEK != original")
	}

	// A fresh wrap of the same DEK differs (random nonce per wrap).
	again, err := WrapDEK(kek, dek)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	if bytes.Equal(wrapped, again) {
		t.Fatal("two wraps of the same DEK are byte-identical (nonce not random?)")
	}
}

// Round trip (data side): seal → open returns the plaintext at various sizes.
func TestSealOpenRoundTrip(t *testing.T) {
	_, dek := testKeys(t)
	cases := []struct {
		name  string
		plain []byte
	}{
		{"empty", []byte{}},
		{"one byte", []byte{0x42}},
		{"utf8", []byte("café ☕ € — merchant: Längste Bäckerei AG")},
		{"1 KB", bytes.Repeat([]byte("x"), 1024)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pack, err := Seal(dek, tc.plain)
			if err != nil {
				t.Fatalf("Seal: %v", err)
			}
			if len(pack) != PackMinSize+len(tc.plain) {
				t.Fatalf("pack = %d bytes, want %d", len(pack), PackMinSize+len(tc.plain))
			}
			got, err := Open(dek, pack)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if !bytes.Equal(got, tc.plain) {
				t.Fatal("round trip changed the plaintext")
			}
		})
	}
}

// Tamper detection: any bit flip — nonce, ciphertext, or tag — a wrong
// key, and truncation must all be rejected.
func TestTamperDetection(t *testing.T) {
	kek, dek := testKeys(t)

	pack, err := Seal(dek, []byte("secret data for tampering"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// Flip one bit in each pack region.
	for name, idx := range map[string]int{
		"nonce":      0,
		"ciphertext": 15,
		"tag":        len(pack) - 1,
	} {
		bad := append([]byte(nil), pack...)
		bad[idx] ^= 0x01
		if _, err := Open(dek, bad); !errors.Is(err, ErrAuth) {
			t.Errorf("flip in %s: err = %v, want ErrAuth", name, err)
		}
	}

	// Truncated pack.
	if _, err := Open(dek, pack[:PackMinSize-1]); !errors.Is(err, ErrMalformedPack) {
		t.Errorf("truncated pack: err = %v, want ErrMalformedPack", err)
	}

	// A wrong DEK fails authentication.
	other, err := NewDEK()
	if err != nil {
		t.Fatalf("NewDEK: %v", err)
	}
	if _, err := Open(other, pack); !errors.Is(err, ErrAuth) {
		t.Errorf("wrong DEK: err = %v, want ErrAuth", err)
	}

	// Same properties on the wrap side.
	wrapped, err := WrapDEK(kek, dek)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	badKek := append([]byte(nil), kek...)
	badKek[0] ^= 0x01
	if _, err := UnwrapDEK(badKek, wrapped); !errors.Is(err, ErrAuth) {
		t.Errorf("wrong KEK: err = %v, want ErrAuth", err)
	}
	if _, err := UnwrapDEK(kek, wrapped[:5]); !errors.Is(err, ErrMalformedPack) {
		t.Errorf("truncated wrapped DEK: err = %v, want ErrMalformedPack", err)
	}
}

// Rotation: Rewrap moves a wrapped DEK from oldKek to newKek; the old KEK
// no longer works, and data sealed under the DEK is unaffected.
func TestRewrapRotation(t *testing.T) {
	kek1, dek := testKeys(t)
	kek2 := randBytes(t, KEKSize)

	w1, err := WrapDEK(kek1, dek)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	w2, err := Rewrap(kek1, kek2, w1)
	if err != nil {
		t.Fatalf("Rewrap: %v", err)
	}

	got, err := UnwrapDEK(kek2, w2)
	if err != nil {
		t.Fatalf("UnwrapDEK(new KEK): %v", err)
	}
	if subtle.ConstantTimeCompare(got, dek) != 1 {
		t.Fatal("DEK after rotation != original")
	}
	if _, err := UnwrapDEK(kek1, w2); !errors.Is(err, ErrAuth) {
		t.Errorf("old KEK still unwraps the re-wrapped DEK: %v", err)
	}

	// Data sealed under the DEK opens after rotation (the DEK did not change).
	pack, err := Seal(dek, []byte("data"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := Open(got, pack); err != nil {
		t.Errorf("data pack fails to open with the rotated DEK: %v", err)
	}

	// Rewrap with the wrong old KEK fails.
	kek3 := randBytes(t, KEKSize)
	if _, err := Rewrap(kek3, kek2, w1); !errors.Is(err, ErrAuth) {
		t.Errorf("Rewrap with wrong old KEK: err = %v, want ErrAuth", err)
	}
}

func TestKeySizeErrors(t *testing.T) {
	if _, err := WrapDEK(make([]byte, 16), make([]byte, DEKSize)); err == nil {
		t.Error("16-byte KEK accepted")
	}
	if _, err := Seal(make([]byte, 16), []byte("x")); err == nil {
		t.Error("16-byte DEK accepted")
	}
}

func TestKEKFromHex(t *testing.T) {
	kek := randBytes(t, KEKSize)
	h := hex.EncodeToString(kek)

	got, err := KEKFromHex(h)
	if err != nil {
		t.Fatalf("KEKFromHex: %v", err)
	}
	if !bytes.Equal(got, kek) {
		t.Fatal("decoded KEK != original")
	}

	if _, err := KEKFromHex(h[:10]); err == nil {
		t.Error("short KEK accepted")
	}
	if _, err := KEKFromHex(strings.Repeat("g", KEKSize*2)); err == nil {
		t.Error("non-hex KEK accepted")
	}
}

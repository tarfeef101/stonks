// Package crypto implements the plan §5 key hierarchy:
//
//	KEK (32 B, from the KEK env var) ──AES-256-GCM──▶ wrapped per-user DEK (32 B)
//	DEK (32 B)                          ──AES-256-GCM──▶ data packs
//
// Both the wrapped DEK (users.wrapped_dek) and data packs use one layout:
//
//	[nonce ‖ ciphertext ‖ tag]    (12-byte nonce, 16-byte GCM tag)
//
// Tag verification is constant-time: the stdlib GCM Open compares the
// expected and actual tags with subtle.ConstantTimeCompare (verified in
// Go 1.27, crypto/cipher/gcm.go), and the package never re-implements tag
// comparison. No AAD is used. Stdlib only — no third-party crypto library.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
)

const (
	// KEKSize is the KEK size in bytes (env: 64 hex chars).
	KEKSize = 32
	// DEKSize is the per-user data-encryption key size in bytes.
	DEKSize = 32
)

// Pack layout: [nonce ‖ ciphertext ‖ tag].
const (
	nonceSize = 12
	tagSize   = 16 // GCM tag, equals AEAD.Overhead()
	// PackMinSize is the smallest valid pack: nonce + tag, empty ciphertext.
	PackMinSize = nonceSize + tagSize
)

var (
	// ErrMalformedPack: the pack is too short to hold nonce + tag.
	ErrMalformedPack = errors.New("crypto: malformed pack (want [nonce ‖ ciphertext ‖ tag], at least 28 bytes)")
	// ErrAuth: GCM tag verification failed — wrong key, or the nonce,
	// ciphertext, or tag was tampered with.
	ErrAuth = errors.New("crypto: authentication failed (wrong key or tampered data)")
	// ErrWrongDEKSize: an unwrapped key is not a 32-byte DEK.
	ErrWrongDEKSize = errors.New("crypto: unwrapped key is not a 32-byte DEK")
)

// KEKFromHex decodes the 64-hex-char env form of the KEK into 32 bytes.
func KEKFromHex(s string) ([]byte, error) {
	if len(s) != KEKSize*2 {
		return nil, fmt.Errorf("crypto: KEK must be %d hex chars (32 bytes), got %d", KEKSize*2, len(s))
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("crypto: KEK is not valid hex: %w", err)
	}
	return b, nil
}

// NewDEK returns a fresh random 32-byte per-user DEK.
func NewDEK() ([]byte, error) {
	dek := make([]byte, DEKSize)
	if _, err := rand.Read(dek); err != nil {
		return nil, fmt.Errorf("crypto: generate DEK: %w", err)
	}
	return dek, nil
}

// WrapDEK encrypts dek with kek (AES-256-GCM) and returns the pack stored
// in users.wrapped_dek.
func WrapDEK(kek, dek []byte) ([]byte, error) {
	a, err := newAEAD(kek)
	if err != nil {
		return nil, err
	}
	return seal(a, dek)
}

// UnwrapDEK is the inverse of WrapDEK.
func UnwrapDEK(kek, pack []byte) ([]byte, error) {
	a, err := newAEAD(kek)
	if err != nil {
		return nil, err
	}
	dek, err := open(a, pack)
	if err != nil {
		return nil, err
	}
	if len(dek) != DEKSize {
		return nil, ErrWrongDEKSize
	}
	return dek, nil
}

// Seal encrypts plaintext with the per-user DEK and returns the pack
// stored in an encrypted column.
func Seal(dek, plaintext []byte) ([]byte, error) {
	a, err := newAEAD(dek)
	if err != nil {
		return nil, err
	}
	return seal(a, plaintext)
}

// Open is the inverse of Seal. A tampered nonce/ciphertext/tag or a wrong
// DEK fails GCM tag verification (constant-time in the stdlib).
func Open(dek, pack []byte) ([]byte, error) {
	a, err := newAEAD(dek)
	if err != nil {
		return nil, err
	}
	return open(a, pack)
}

// Rewrap unwraps a wrapped DEK with oldKek and re-wraps it with newKek.
// This is the KEK-rotation primitive: run it over every users.wrapped_dek
// row. Data packs are unaffected — they are keyed by the DEK, which does
// not change, so rotation only rewrites the wrapped DEK column.
func Rewrap(oldKek, newKek, wrapped []byte) ([]byte, error) {
	dek, err := UnwrapDEK(oldKek, wrapped)
	if err != nil {
		return nil, fmt.Errorf("crypto: rewrap: %w", err)
	}
	return WrapDEK(newKek, dek)
}

// aeadCache caches built AES-256-GCM AEADs by raw key bytes so the hot
// paths (every Seal/Open, every DEK unwrap) skip cipher construction.
// cipher.AEAD is safe for concurrent use, so a shared instance is
// correct. All keys are 32 bytes (KEK and DEK alike), so the cache is
// bounded by the number of distinct keys in the process: one entry for
// the KEK plus one per user DEK ever unwrapped. Entries are never evicted
// because keys never change (see bootstrap's DEK cache for the only
// re-key path, which rewrites the pack, not the DEK).
var aeadCache sync.Map // map[string]cipher.AEAD

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != DEKSize {
		return nil, fmt.Errorf("crypto: key must be %d bytes, got %d", DEKSize, len(key))
	}
	if v, ok := aeadCache.Load(string(key)); ok {
		return v.(cipher.AEAD), nil
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: aes: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: gcm: %w", err)
	}
	// LoadOrStore: a concurrent call may have built the same AEAD
	// between our miss and store; either instance is equivalent.
	if v, loaded := aeadCache.LoadOrStore(string(key), gcm); loaded {
		return v.(cipher.AEAD), nil
	}
	return gcm, nil
}

// seal encrypts plaintext under a fresh random nonce. GCM Seal appends the
// tag to the ciphertext, so the result is exactly [nonce ‖ ciphertext ‖ tag].
func seal(a cipher.AEAD, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, a.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("crypto: generate nonce: %w", err)
	}
	return a.Seal(nonce, nonce, plaintext, nil), nil
}

// open splits the pack and verifies the GCM tag (constant-time in the
// stdlib). a.Open takes [ciphertext ‖ tag] and carves the tag off itself.
func open(a cipher.AEAD, pack []byte) ([]byte, error) {
	n := a.NonceSize()
	if len(pack) < n+a.Overhead() {
		return nil, ErrMalformedPack
	}
	nonce, rest := pack[:n], pack[n:]
	plaintext, err := a.Open(nil, nonce, rest, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAuth, err)
	}
	return plaintext, nil
}

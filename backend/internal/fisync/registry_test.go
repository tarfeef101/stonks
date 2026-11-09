package fisync

import (
	"context"
	"testing"
	"time"
)

// codeAdapter is a minimal InstitutionAdapter standing in for a real
// one: it only carries a code (Sync is never exercised here).
type codeAdapter struct {
	code string
}

func (a codeAdapter) Code() string { return a.code }

func (a codeAdapter) Sync(context.Context, TokenCredentials, time.Time) ([]IncomingTransaction, error) {
	return nil, nil
}

func TestRegistryGetHitMiss(t *testing.T) {
	zeta := codeAdapter{"zeta-bank"}
	r := NewRegistry(codeAdapter{"alpha-bank"}, zeta)
	if got, ok := r.Get("zeta-bank"); !ok || got != zeta {
		t.Fatalf("get zeta-bank: got %v (ok %v), want the registered adapter", got, ok)
	}
	if _, ok := r.Get("no-such-institution"); ok {
		t.Fatal("get no-such-institution: ok = true, want false")
	}
	if _, ok := r.Get(""); ok {
		t.Fatal("get empty code: ok = true, want false")
	}
}

func TestRegistryCodesSorted(t *testing.T) {
	r := NewRegistry(codeAdapter{"zeta-bank"}, codeAdapter{"mid-bank"}, codeAdapter{"alpha-bank"})
	got := r.Codes()
	want := []string{"alpha-bank", "mid-bank", "zeta-bank"}
	if len(got) != len(want) {
		t.Fatalf("codes: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("codes: got %v, want %v", got, want)
		}
	}
	// An empty registry lists nothing (and Codes is still a slice).
	empty := NewRegistry()
	if got := empty.Codes(); len(got) != 0 {
		t.Fatalf("empty registry codes: got %v, want none", got)
	}
}

func TestRegistryDuplicateCodePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate Code() did not panic (programmer error must be loud at startup)")
		}
	}()
	_ = NewRegistry(codeAdapter{"same-code"}, codeAdapter{"same-code"})
}

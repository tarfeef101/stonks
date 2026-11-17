package llm

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
)

// loopbackNet is the allowlist the httptest-based tests need: the
// test server listens on 127.0.0.1, which strict mode refuses
// without an explicit CIDR.
func loopbackNet() []*net.IPNet {
	nets, err := ParseEndpointCIDRs("127.0.0.1/32")
	if err != nil {
		panic(err)
	}
	return nets
}

func mustCIDRs(t *testing.T, s string) []*net.IPNet {
	t.Helper()
	nets, err := ParseEndpointCIDRs(s)
	if err != nil {
		t.Fatalf("ParseEndpointCIDRs(%q): %v", s, err)
	}
	return nets
}

// TestPolicyAlwaysRejectedSet: cloud metadata / link-local, IPv6
// loopback, and unspecified addresses are blocked in BOTH policy
// modes, even when the allowlist covers them (T-P0-1).
func TestPolicyAlwaysRejectedSet(t *testing.T) {
	// An allowlist spanning the ENTIRE always-rejected set: it must
	// not help.
	all := mustCIDRs(t, "169.254.0.0/16, fe80::/10, ::1/128, 0.0.0.0/32, ::/128")
	pols := []endpointPolicy{
		{strict: true, nets: all},
		{strict: false},
	}
	ips := []string{"169.254.169.254", "fe80::1", "::1", "0.0.0.0", "::"}
	for _, pol := range pols {
		for _, s := range []string{"http", "https"} {
			for _, ip := range ips {
				if pol.ok(s, net.ParseIP(ip)) {
					t.Errorf("strict=%v: %s://%s dialable, want always rejected", pol.strict, s, ip)
				}
			}
		}
	}
}

// TestPolicyStrictDeniesPrivateAndLoopback: strict mode with an
// EMPTY allowlist refuses private ranges and the IPv4 loopback for
// both schemes (T-P0-1).
func TestPolicyStrictDeniesPrivateAndLoopback(t *testing.T) {
	pol := endpointPolicy{strict: true}
	ips := []string{"172.17.0.5", "10.0.0.8", "192.168.1.10", "127.0.0.1"}
	for _, s := range []string{"http", "https"} {
		for _, ip := range ips {
			if pol.ok(s, net.ParseIP(ip)) {
				t.Errorf("strict/empty allowlist: %s://%s dialable, want refused", s, ip)
			}
		}
	}
}

// TestPolicyAllowlistAdmitsDockerRange: a CIDR admits its addresses
// in BOTH schemes (the dev llama service is plain http on the docker
// network); addresses outside the CIDR stay refused (T-P0-1).
func TestPolicyAllowlistAdmitsDockerRange(t *testing.T) {
	docker := mustCIDRs(t, "172.16.0.0/12")
	pol := endpointPolicy{strict: true, nets: docker}
	for _, s := range []string{"http", "https"} {
		if !pol.ok(s, net.ParseIP("172.17.0.5")) {
			t.Errorf("%s://172.17.0.5 refused, want admitted by 172.16.0.0/12", s)
		}
		if pol.ok(s, net.ParseIP("192.168.1.10")) {
			t.Errorf("%s://192.168.1.10 dialable, want refused (outside the allowlist)", s)
		}
	}
}

// TestPolicyStrictPublicRequiresTLS: strict mode dials a public
// address only over https (T-P0-1).
func TestPolicyStrictPublicRequiresTLS(t *testing.T) {
	pol := endpointPolicy{strict: true}
	if !pol.ok("https", net.ParseIP("8.8.8.8")) {
		t.Error("https://8.8.8.8 refused, want dialable")
	}
	if pol.ok("http", net.ParseIP("8.8.8.8")) {
		t.Error("http://8.8.8.8 dialable, want refused (plain http needs an allowlist entry)")
	}
}

// TestCategorizeOversizedBody: a response body past maxResponseBytes
// is truncated and reported as *http.MaxBytesError, and the request
// is made exactly once (the nudge retry exists for malformed JSON,
// not transport errors) (T-P0-1).
func TestCategorizeOversizedBody(t *testing.T) {
	var recs []recorded
	srv := newServer(t, &recs, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(make([]byte, maxResponseBytes+64))
	})
	p := New(srv.URL+"/v1", "", "m", WithEndpointAllowlist(loopbackNet()))
	_, err := p.Categorize(context.Background(), Request{User: "hi"})
	var mbe *http.MaxBytesError
	if !errors.As(err, &mbe) {
		t.Fatalf("Categorize error = %v, want *http.MaxBytesError", err)
	}
	if len(recs) != 1 {
		t.Fatalf("server saw %d requests, want exactly 1", len(recs))
	}
}

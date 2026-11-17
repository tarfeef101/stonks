package llm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
)

// errEndpointBlocked is returned when the endpoint policy refuses an
// address. It is the policy's only error: the text is deliberately
// generic (it is client-safe), while the wrapped detail — which host
// resolved to which blocked address — is what the server log shows.
var errEndpointBlocked = errors.New("endpoint blocked by policy")

// endpointPolicy vets an endpoint's addresses against the operator's
// allowlist before the client dials them (T-P0-1).
//
// Two modes:
//
//   - non-strict (the zero value, what a bare New runs): only the
//     always-rejected set is blocked, so the self-hosted dev default
//     (http://llama:8080/v1 on the docker network) and loopback
//     httptest servers keep working out of the box.
//
//   - strict (WithEndpointAllowlist): an address is dialable only if
//     it is allowlisted, or — https only — public. Plain http to a
//     non-allowlisted address is refused; private and loopback
//     addresses need an explicit CIDR.
//
// The always-rejected set — cloud metadata / link-local (169.254.0.0/16
// and fe80::/10), the IPv6 loopback ::1, and the unspecified 0.0.0.0
// / :: — is blocked in BOTH modes and can never be allowlisted.
// 127.0.0.1 is deliberately NOT in that set: it is the loopback
// test-server address and stays allowlistable under strict mode.
type endpointPolicy struct {
	strict bool
	nets   []*net.IPNet
}

// WithEndpointAllowlist arms the strict endpoint policy and admits
// the given CIDRs (typically the docker network's llama service,
// from FTRK_LLM_ENDPOINT_CIDRS). A nil or empty list still arms
// strict mode: then only public https endpoints are reachable.
func WithEndpointAllowlist(nets []*net.IPNet) Option {
	return func(p *Provider) {
		p.policy = endpointPolicy{strict: true, nets: nets}
	}
}

// alwaysRejected reports whether ip is in the set the policy blocks
// unconditionally (see endpointPolicy).
func alwaysRejected(ip net.IP) bool {
	if ip.IsLinkLocalUnicast() { // 169.254.0.0/16 and fe80::/10 (metadata)
		return true
	}
	if ip.IsLoopback() && ip.To4() == nil { // ::1 only; 127.0.0.1 is allowlistable
		return true
	}
	return ip.IsUnspecified() // 0.0.0.0 and ::
}

// inAllowlist reports whether ip falls inside one of the policy's
// CIDRs.
func (pol endpointPolicy) inAllowlist(ip net.IP) bool {
	for _, n := range pol.nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ok decides whether scheme://ip is dialable under the policy.
func (pol endpointPolicy) ok(scheme string, ip net.IP) bool {
	if alwaysRejected(ip) {
		return false
	}
	if !pol.strict {
		return true
	}
	if pol.inAllowlist(ip) {
		return true
	}
	if scheme == "http" {
		return false // plain http needs an explicit allowlist entry
	}
	return !ip.IsLoopback() && !ip.IsPrivate()
}

// checkHost vets host (a literal IP or a hostname) under the policy
// and returns the address(es) a dial may use. A hostname must
// resolve, and EVERY returned address must pass: a mixed A/AAAA
// answer with one poisoned record is not partially dialable. A
// blocked address yields errEndpointBlocked (wrapped with the
// offending address); a resolution failure or an empty answer
// yields a plain error.
func (pol endpointPolicy) checkHost(ctx context.Context, scheme, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		if err := pol.vet(scheme, host, ip); err != nil {
			return nil, err
		}
		return []net.IP{ip}, nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		if err := pol.vet(scheme, host, a.IP); err != nil {
			return nil, err
		}
		ips = append(ips, a.IP)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("%s resolved to no addresses", host)
	}
	return ips, nil
}

// vet wraps the policy decision for one address in the client-safe
// sentinel.
func (pol endpointPolicy) vet(scheme, host string, ip net.IP) error {
	if !pol.ok(scheme, ip) {
		return fmt.Errorf("%w: %s -> %s", errEndpointBlocked, host, ip)
	}
	return nil
}

// ParseEndpointCIDRs parses a comma-separated list of CIDRs and/or
// bare IPs (FTRK_LLM_ENDPOINT_CIDRS) into networks. A bare IP becomes
// a /32 (IPv4) or /128 (IPv6) network; an IPv4-mapped address is
// canonicalized to its IPv4 form first, so "::ffff:10.0.0.1" admits
// exactly 10.0.0.1/32 and not a /32 of the 128-bit spelling.
// Whitespace around entries is ignored, empty entries are skipped.
func ParseEndpointCIDRs(s string) ([]*net.IPNet, error) {
	var nets []*net.IPNet
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		cidr := entry
		if !strings.Contains(entry, "/") {
			ip := net.ParseIP(entry)
			if ip == nil {
				return nil, fmt.Errorf("%q is neither a CIDR nor an IP", entry)
			}
			if v4 := ip.To4(); v4 != nil {
				cidr = v4.String() + "/32"
			} else {
				cidr = entry + "/128"
			}
		}
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("%q is neither a CIDR nor an IP", entry)
		}
		nets = append(nets, n)
	}
	return nets, nil
}

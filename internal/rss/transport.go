/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package rss

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// BlockedAddressError is returned (wrapped by net/http as a *url.Error /
// *net.OpError) when the SSRF guard refuses to connect to a non-public
// address. It is a typed error so callers can tell this permanent feed
// misconfiguration -- a URL that resolves to a private/loopback/metadata
// address -- from a transient network failure without parsing Error()'s text.
type BlockedAddressError struct {
	Addr netip.Addr
}

func (e *BlockedAddressError) Error() string {
	return fmt.Sprintf("refusing to connect to non-public address %s", e.Addr)
}

// newDefaultHTTPClient returns a client with an explicit timeout and a dialer
// that rejects connections to loopback, link-local, private, and other
// non-public IP ranges. Feed URLs are supplied by namespace users via the
// FeedGroup CRD, so without this guard the controller can be used as an SSRF
// proxy into the cluster network or cloud metadata endpoints. If wrap is
// non-nil, it wraps the guarded transport (e.g. to add tracing) without
// replacing it.
//
// The check runs in net.Dialer.Control, i.e. against the exact address the
// socket is about to connect to, after name resolution. That closes the
// check-then-connect (DNS rebinding) gap a separate lookup would leave, and
// lets the dialer fall through every resolved address rather than only the
// first.
func newDefaultHTTPClient(wrap func(http.RoundTripper) http.RoundTripper) *http.Client {
	dialer := &net.Dialer{
		Timeout: 5 * time.Second,
		Control: guardDialControl,
	}

	// Built explicitly (mirroring http.DefaultTransport's tuning) rather than
	// cloned from the global, so nothing that replaces http.DefaultTransport
	// can change or break the guarded transport. Proxy is deliberately left
	// nil: with a proxy the dialer only ever connects to the proxy, so the IP
	// guard would never see the real target and an attacker-chosen feed URL
	// could reach internal hosts via the proxy.
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: defaultTimeout,
	}

	var rt http.RoundTripper = transport
	if wrap != nil {
		rt = wrap(rt)
	}

	return &http.Client{
		Timeout:   defaultTimeout,
		Transport: rt,
	}
}

// guardDialControl is a net.Dialer Control function that rejects any
// connection whose resolved remote address is not a public IP.
func guardDialControl(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return err
	}
	addr := ap.Addr().Unmap()
	if !isPublicIP(addr) {
		return &BlockedAddressError{Addr: addr}
	}
	return nil
}

// cgnatBlock is the shared address space carriers use for NAT
// (RFC 6598, 100.64.0.0/10). netip.Addr.IsPrivate doesn't cover it, but like
// RFC 1918 space it can route to internal infrastructure that a feed URL
// must not be able to reach.
var cgnatBlock = netip.MustParsePrefix("100.64.0.0/10")

// benchmarkingBlock is RFC 2544 network device benchmarking space
// (198.18.0.0/15). Not globally routable, but -- like CGNAT space -- seen in
// practice as an internal range for CNI/service-mesh addressing, so a feed
// URL must not be able to reach it either.
var benchmarkingBlock = netip.MustParsePrefix("198.18.0.0/15")

// reservedBlock is IANA "reserved for future use" space (RFC 1112,
// 240.0.0.0/4, historically "Class E"), which some CNIs/service meshes
// repurpose as an internal virtual range.
var reservedBlock = netip.MustParsePrefix("240.0.0.0/4")

// thisNetworkBlock is RFC 791 "this network" space (0.0.0.0/8): non-routable
// and meaningless as a connection target, but not covered by
// netip.Addr.IsUnspecified (which only matches the single all-zeros address)
// or any of the other checks below.
var thisNetworkBlock = netip.MustParsePrefix("0.0.0.0/8")

// nat64Prefix is the NAT64 "Well-Known Prefix" (RFC 6052, 64:ff9b::/96).
// Like the IPv4-mapped form Unmap() handles, addresses under it embed an
// IPv4 address in their low 32 bits, but Unmap() doesn't recognize this
// prefix -- without unwrapping it separately, a NAT64-synthesized address
// embedding a private or metadata-endpoint IPv4 address would be treated as
// an opaque, public IPv6 literal.
var nat64Prefix = netip.MustParsePrefix("64:ff9b::/96")

// sixToFourPrefix is the 6to4 anycast prefix (RFC 3056, 2002::/16), which
// like NAT64's prefix embeds an IPv4 address -- here in bits 16-48 rather
// than the low 32 bits -- that must be checked in its own right rather than
// treating the whole address as an opaque, public IPv6 literal.
var sixToFourPrefix = netip.MustParsePrefix("2002::/16")

func isPublicIP(ip netip.Addr) bool {
	// Unwrap IPv4-mapped IPv6 addresses (::ffff:a.b.c.d) so the checks below
	// (cgnatBlock.Contains in particular: an IPv4-mapped address never
	// matches an IPv4 prefix) see the actual IPv4 address instead of treating
	// it as an opaque IPv6 literal.
	ip = ip.Unmap()
	switch {
	case nat64Prefix.Contains(ip):
		b := ip.As16()
		ip = netip.AddrFrom4([4]byte(b[12:16]))
	case sixToFourPrefix.Contains(ip):
		b := ip.As16()
		ip = netip.AddrFrom4([4]byte(b[2:6]))
	}

	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsPrivate() || ip.IsMulticast() ||
		cgnatBlock.Contains(ip) || benchmarkingBlock.Contains(ip) ||
		reservedBlock.Contains(ip) || thisNetworkBlock.Contains(ip) {
		return false
	}
	return true
}

package rss

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestDefaultClient_RejectsLoopbackTarget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sampleRSS))
	}))
	defer srv.Close()

	// NewClient(nil) builds the SSRF-guarded default client, which must
	// refuse to connect to a loopback address such as a local test server
	// or a feed URL pointing at 127.0.0.1.
	c := NewClient(nil)
	_, err := c.FetchEntries(t.Context(), srv.URL, CacheValidators{})
	if err == nil {
		t.Fatal("expected error connecting to loopback address, got nil")
	}
}

func TestNewClientWithTransportWrap_WrapsWithoutBypassingGuard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sampleRSS))
	}))
	defer srv.Close()

	var wrapped bool
	wrap := func(rt http.RoundTripper) http.RoundTripper {
		wrapped = true
		return rt
	}

	// The wrap must be applied around the guarded transport, not instead of
	// it: a request to a loopback test server must still be rejected.
	c := NewClientWithTransportWrap(wrap)
	if !wrapped {
		t.Fatal("expected wrap to be invoked when building the default client")
	}
	_, err := c.FetchEntries(t.Context(), srv.URL, CacheValidators{})
	if err == nil {
		t.Fatal("expected error connecting to loopback address, got nil")
	}
}

func TestNewClientWithTransportWrap_NilWrapMatchesNewClient(t *testing.T) {
	c := NewClientWithTransportWrap(nil)
	if c.httpClient == nil {
		t.Fatal("expected a non-nil default http client")
	}
}

func TestIsPublicIP(t *testing.T) {
	cases := []struct {
		ip     string
		public bool
	}{
		{"8.8.8.8", true},
		{"1.1.1.1", true},
		{"127.0.0.1", false},
		{"169.254.169.254", false}, // cloud metadata endpoint
		{"10.0.0.1", false},
		{"192.168.1.1", false},
		{"172.16.0.1", false},
		{"0.0.0.0", false},
		{"::1", false},
		{"fe80::1", false},
		{"100.64.0.1", false},               // carrier-grade NAT (RFC 6598)
		{"100.127.255.254", false},          // top of the CGNAT block
		{"100.63.255.255", true},            // just below the CGNAT block
		{"::ffff:127.0.0.1", false},         // IPv4-mapped IPv6 loopback
		{"::ffff:10.0.0.1", false},          // IPv4-mapped IPv6 private
		{"::ffff:8.8.8.8", true},            // IPv4-mapped IPv6 public
		{"64:ff9b::169.254.169.254", false}, // NAT64 (RFC 6052) cloud metadata endpoint
		{"64:ff9b::10.0.0.1", false},        // NAT64 (RFC 6052) embedding a private address
		{"64:ff9b::8.8.8.8", true},          // NAT64 (RFC 6052) embedding a public address
		{"198.18.0.1", false},               // RFC 2544 benchmarking space
		{"198.19.255.255", false},           // top of the benchmarking block
		{"198.20.0.1", true},                // just above the benchmarking block
		{"240.0.0.1", false},                // reserved/"Class E" space
		{"255.255.255.255", false},          // limited broadcast
		{"0.0.0.1", false},                  // "this network" (RFC 791)
		{"2002:0a00:0001::", false},         // 6to4 (RFC 3056) embedding a private address
		{"2002:0808:0808::", true},          // 6to4 (RFC 3056) embedding a public address
	}
	for _, tc := range cases {
		ip, err := netip.ParseAddr(tc.ip)
		if err != nil {
			t.Fatalf("failed to parse test IP %q: %v", tc.ip, err)
		}
		if got := isPublicIP(ip); got != tc.public {
			t.Errorf("isPublicIP(%s) = %v, want %v", tc.ip, got, tc.public)
		}
	}
}

func TestDefaultClient_RejectsUnresolvableHost(t *testing.T) {
	// "invalid" is reserved by RFC 6761 to never resolve, so this reliably
	// exercises the DialContext's LookupIP error path (rather than the
	// isPublicIP rejection exercised by TestDefaultClient_RejectsLoopbackTarget).
	c := NewClient(nil)
	_, err := c.FetchEntries(t.Context(), "http://does-not-resolve.invalid/feed.xml", CacheValidators{})
	if err == nil {
		t.Fatal("expected error resolving an unresolvable host, got nil")
	}
}

func TestGuardDialControl(t *testing.T) {
	tests := []struct {
		address string
		wantErr bool
		// wantNamed, when set, must appear in the error text.
		wantNamed string
	}{
		{"93.184.216.34:443", false, ""},
		{"[2606:4700:4700::1111]:443", false, ""},
		{"127.0.0.1:80", true, ""},
		{"10.0.0.1:80", true, ""},
		{"169.254.169.254:80", true, ""},
		{"[::1]:80", true, ""},
		{"[::ffff:127.0.0.1]:80", true, "127.0.0.1"},
		{"[::ffff:10.1.2.3]:80", true, ""},
		{"not-an-address", true, ""},
	}
	for _, tt := range tests {
		err := guardDialControl("tcp", tt.address, nil)
		if gotErr := err != nil; gotErr != tt.wantErr {
			t.Errorf("guardDialControl(%q) error = %v, wantErr %v", tt.address, err, tt.wantErr)
			continue
		}
		if tt.wantNamed != "" && !strings.Contains(err.Error(), tt.wantNamed) {
			t.Errorf("guardDialControl(%q) error %v should name %q", tt.address, err, tt.wantNamed)
		}
	}
}

func TestDefaultClient_IgnoresProxyEnvironment(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://203.0.113.1:3128")
	t.Setenv("HTTPS_PROXY", "http://203.0.113.1:3128")
	t.Setenv("http_proxy", "http://203.0.113.1:3128")
	t.Setenv("https_proxy", "http://203.0.113.1:3128")

	c := newDefaultHTTPClient(nil)
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", c.Transport)
	}
	if tr.Proxy != nil {
		t.Fatal("transport must not use a proxy: it would bypass the IP guard")
	}
	if tr.IdleConnTimeout == 0 || !tr.ForceAttemptHTTP2 {
		t.Error("expected idle-conn timeout and HTTP/2 to be configured")
	}
}

func TestDefaultClient_LoopbackErrorNamesAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(sampleRSS))
	}))
	defer srv.Close()

	_, err := NewClient(nil).FetchEntries(t.Context(), srv.URL, CacheValidators{})
	if err == nil || !strings.Contains(err.Error(), "refusing to connect to non-public address 127.0.0.1") {
		t.Fatalf("expected non-public address error, got %v", err)
	}
}

// TestDefaultClient_LoopbackErrorIsTypedBlockedAddress asserts the guard's
// refusal survives http.Client's *url.Error / *net.OpError wrapping as a
// typed *BlockedAddressError, so callers can classify it (a permanent feed
// misconfiguration) without parsing the message.
func TestDefaultClient_LoopbackErrorIsTypedBlockedAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(sampleRSS))
	}))
	defer srv.Close()

	_, err := NewClient(nil).FetchEntries(t.Context(), srv.URL, CacheValidators{})
	blocked, ok := errors.AsType[*BlockedAddressError](err)
	if !ok {
		t.Fatalf("expected errors.AsType to find *BlockedAddressError, got %T: %v", err, err)
	}
	if want := netip.MustParseAddr("127.0.0.1"); blocked.Addr != want {
		t.Fatalf("BlockedAddressError.Addr = %v, want %v", blocked.Addr, want)
	}
}

// TestGuardDialControl_BlockedAddressErrorText pins the error message, which
// operators see in status conditions and logs, and the unmapped address
// carried by the typed error.
func TestGuardDialControl_BlockedAddressErrorText(t *testing.T) {
	err := guardDialControl("tcp", "[::ffff:10.1.2.3]:80", nil)
	blocked, ok := errors.AsType[*BlockedAddressError](err)
	if !ok {
		t.Fatalf("expected *BlockedAddressError, got %T: %v", err, err)
	}
	if want := netip.MustParseAddr("10.1.2.3"); blocked.Addr != want {
		t.Fatalf("Addr = %v, want %v (IPv4-mapped form unmapped)", blocked.Addr, want)
	}
	if got, want := err.Error(), "refusing to connect to non-public address 10.1.2.3"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

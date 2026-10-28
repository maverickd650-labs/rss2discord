package rss

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

const sampleRSS = `<?xml version="1.0"?>
<rss><channel>
<item><title>Hello</title><link>http://example.com/1</link><description>World</description><guid>1</guid></item>
</channel></rss>`

const testETag = `"abc123"`

const testLastModified = "Wed, 21 Oct 2015 07:28:00 GMT"

func TestFetchEntries_RejectsInvalidURLs(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantErr string
	}{
		{"non-http scheme", "ftp://example.com/feed.xml", "unsupported feed URL scheme"},
		{"blank URL", " \t ", "feed URL is empty"},
		{"unparsable URL", "://bad-url", "invalid feed URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewClient(&http.Client{})
			_, err := c.FetchEntries(t.Context(), tc.url, CacheValidators{})
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestFetchEntries_EnforcesSizeCap(t *testing.T) {
	oversized := strings.Repeat("a", maxFeedResponseBytes+1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(oversized))
	}))
	defer srv.Close()

	// Use a plain client so loopback test servers aren't rejected by the
	// SSRF guard, isolating this test to the size-cap behavior.
	c := NewClient(&http.Client{})
	_, err := c.FetchEntries(t.Context(), srv.URL, CacheValidators{})
	if err == nil {
		t.Fatal("expected error for oversized response, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds maximum allowed size") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFetchEntries_ParsesValidRSS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sampleRSS))
	}))
	defer srv.Close()

	c := NewClient(&http.Client{})
	result, err := c.FetchEntries(t.Context(), srv.URL, CacheValidators{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Entries) != 1 || result.Entries[0].Title != "Hello" {
		t.Fatalf("unexpected entries: %+v", result.Entries)
	}
}

func TestFetchEntries_ReturnsFeedTitle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<rss><channel><title>Example Blog</title><item><title>Hello</title></item></channel></rss>`))
	}))
	defer srv.Close()

	result, err := NewClient(&http.Client{}).FetchEntries(t.Context(), srv.URL, CacheValidators{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.FeedTitle != "Example Blog" {
		t.Fatalf("FeedTitle = %q, want %q", result.FeedTitle, "Example Blog")
	}
}

func TestFetchEntries_SendsETagAndLastModifiedValidators(t *testing.T) {
	var gotIfNoneMatch, gotIfModifiedSince string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIfNoneMatch = r.Header.Get("If-None-Match")
		gotIfModifiedSince = r.Header.Get("If-Modified-Since")
		_, _ = w.Write([]byte(sampleRSS))
	}))
	defer srv.Close()

	c := NewClient(&http.Client{})
	_, err := c.FetchEntries(t.Context(), srv.URL, CacheValidators{
		ETag:         testETag,
		LastModified: testLastModified,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotIfNoneMatch != testETag {
		t.Fatalf("expected If-None-Match to be sent, got %q", gotIfNoneMatch)
	}
	if gotIfModifiedSince != testLastModified {
		t.Fatalf("expected If-Modified-Since to be sent, got %q", gotIfModifiedSince)
	}
}

func TestFetchEntries_SendsUserAgent(t *testing.T) {
	var gotUserAgent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUserAgent = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(sampleRSS))
	}))
	defer srv.Close()

	c := NewClient(&http.Client{})
	_, err := c.FetchEntries(t.Context(), srv.URL, CacheValidators{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotUserAgent != userAgent {
		t.Fatalf("got User-Agent %q, want %q", gotUserAgent, userAgent)
	}
}

func TestFetchEntries_StoresValidatorsFromSuccessfulResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"new-etag"`)
		w.Header().Set("Last-Modified", "Thu, 22 Oct 2015 07:28:00 GMT")
		_, _ = w.Write([]byte(sampleRSS))
	}))
	defer srv.Close()

	c := NewClient(&http.Client{})
	result, err := c.FetchEntries(t.Context(), srv.URL, CacheValidators{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.NotModified {
		t.Fatal("expected NotModified to be false on a 200 response")
	}
	if result.ETag != `"new-etag"` {
		t.Fatalf("expected ETag to be captured, got %q", result.ETag)
	}
	if result.LastModified != "Thu, 22 Oct 2015 07:28:00 GMT" {
		t.Fatalf("expected Last-Modified to be captured, got %q", result.LastModified)
	}
}

func TestFetchEntries_HandlesNotModifiedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == testETag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		t.Fatal("expected request to carry the previously stored ETag")
	}))
	defer srv.Close()

	c := NewClient(&http.Client{})
	result, err := c.FetchEntries(t.Context(), srv.URL, CacheValidators{ETag: testETag})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.NotModified {
		t.Fatal("expected NotModified to be true on a 304 response")
	}
	if len(result.Entries) != 0 {
		t.Fatalf("expected no entries on a 304 response, got %+v", result.Entries)
	}
	if result.ETag != testETag {
		t.Fatalf("expected the prior ETag to be preserved, got %q", result.ETag)
	}
}

func TestFetchEntries_ReturnsErrorOnNon2xxStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("upstream feed is down"))
	}))
	defer srv.Close()

	c := NewClient(&http.Client{})
	_, err := c.FetchEntries(t.Context(), srv.URL, CacheValidators{})
	if err == nil {
		t.Fatal("expected error for non-2xx response, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("expected error to include the response status, got: %v", err)
	}
	if !strings.Contains(err.Error(), "upstream feed is down") {
		t.Fatalf("expected error to include the response body, got: %v", err)
	}
}

func TestFetchEntries_ReturnsErrorOnMalformedXML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<rss><channel><item><title>Unclosed"))
	}))
	defer srv.Close()

	c := NewClient(&http.Client{})
	_, err := c.FetchEntries(t.Context(), srv.URL, CacheValidators{})
	if err == nil {
		t.Fatal("expected error for malformed XML body, got nil")
	}
}

func TestFetchEntries_RefreshesValidatorsOnNotModified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A 304 can still carry a refreshed ETag/Last-Modified (e.g. when a
		// CDN rotates a weak validator without the underlying content
		// changing); the caller should store the refreshed validators rather
		// than blindly keeping whatever it sent.
		w.Header().Set("ETag", `"v2"`)
		w.Header().Set("Last-Modified", "Fri, 23 Oct 2015 07:28:00 GMT")
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	c := NewClient(&http.Client{})
	result, err := c.FetchEntries(t.Context(), srv.URL, CacheValidators{
		ETag:         `"v1"`,
		LastModified: testLastModified,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.NotModified {
		t.Fatal("expected NotModified to be true on a 304 response")
	}
	if result.ETag != `"v2"` {
		t.Fatalf("expected the refreshed ETag from the 304 response, got %q", result.ETag)
	}
	if result.LastModified != "Fri, 23 Oct 2015 07:28:00 GMT" {
		t.Fatalf("expected the refreshed Last-Modified from the 304 response, got %q", result.LastModified)
	}
}

func TestParseFeed_ExtractsImage(t *testing.T) {
	cases := []struct {
		name      string
		data      string
		wantImage string
	}{
		{
			name: "RSS enclosure",
			data: `<?xml version="1.0"?>
<rss><channel>
<item><title>Hello</title><link>http://example.com/1</link><description>World</description><guid>1</guid>
<enclosure url="http://example.com/pic.jpg" type="image/jpeg" /></item>
</channel></rss>`,
			wantImage: "http://example.com/pic.jpg",
		},
		{
			name: "media thumbnail",
			data: `<?xml version="1.0"?>
<rss xmlns:media="http://search.yahoo.com/mrss/"><channel>
<item><title>Hello</title><link>http://example.com/1</link><description>World</description><guid>1</guid>
<media:thumbnail url="http://example.com/thumb.jpg" /></item>
</channel></rss>`,
			wantImage: "http://example.com/thumb.jpg",
		},
		{
			name: "Atom enclosure",
			data: `<?xml version="1.0"?>
<feed xmlns="http://www.w3.org/2005/Atom">
<entry>
<id>1</id>
<title>Hello</title>
<link rel="alternate" href="http://example.com/1" />
<link rel="enclosure" href="http://example.com/pic.jpg" type="image/jpeg" />
<summary>World</summary>
</entry>
</feed>`,
			wantImage: "http://example.com/pic.jpg",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entries, _, err := parseFeed([]byte(tc.data))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(entries) != 1 {
				t.Fatalf("unexpected entries: %+v", entries)
			}
			if entries[0].Link != "http://example.com/1" {
				t.Fatalf("unexpected link: %q", entries[0].Link)
			}
			if entries[0].Image != tc.wantImage {
				t.Fatalf("unexpected image: %q", entries[0].Image)
			}
		})
	}
}

func TestFetchEntries_RespectsTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte(sampleRSS))
	}))
	defer srv.Close()

	c := NewClient(&http.Client{Timeout: 10 * time.Millisecond})
	_, err := c.FetchEntries(t.Context(), srv.URL, CacheValidators{})
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

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

func TestHTTPStatusError_ErrorWithoutBody(t *testing.T) {
	err := &HTTPStatusError{StatusCode: 503, Status: "503 Service Unavailable"}
	if got, want := err.Error(), "feed fetch failed: 503 Service Unavailable"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
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

type errorReadCloser struct{}

func (errorReadCloser) Read([]byte) (int, error) { return 0, errors.New("simulated body read failure") }
func (errorReadCloser) Close() error             { return nil }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFetchEntries_ReturnsErrorOnBodyReadFailure(t *testing.T) {
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       errorReadCloser{},
			Header:     make(http.Header),
		}, nil
	})

	c := NewClient(&http.Client{Transport: rt})
	_, err := c.FetchEntries(t.Context(), "http://example.com/feed.xml", CacheValidators{})
	if err == nil {
		t.Fatal("expected error reading response body, got nil")
	}
}

func TestParseFeed_DefaultBranchFallsBackToAtomAndFails(t *testing.T) {
	// The root element "RDF" is neither "rss" nor "feed", so parseFeed takes
	// the default branch; the document is otherwise malformed, so both the
	// RSS and the Atom decode attempts fail.
	data := []byte(`<rdf:RDF><channel><unclosed`)

	_, _, err := parseFeed(data)
	if err == nil {
		t.Fatal("expected error for malformed non-rss/feed document, got nil")
	}
}

func TestGuardDialControl(t *testing.T) {
	tests := []struct {
		address string
		wantErr bool
	}{
		{"93.184.216.34:443", false},
		{"[2606:4700:4700::1111]:443", false},
		{"127.0.0.1:80", true},
		{"10.0.0.1:80", true},
		{"169.254.169.254:80", true},
		{"[::1]:80", true},
		{"[::ffff:127.0.0.1]:80", true},
		{"[::ffff:10.1.2.3]:80", true},
		{"not-an-address", true},
	}
	for _, tt := range tests {
		err := guardDialControl("tcp", tt.address, nil)
		if (err != nil) != tt.wantErr {
			t.Errorf("guardDialControl(%q) error = %v, wantErr %v", tt.address, err, tt.wantErr)
		}
		if err != nil && tt.address == "[::ffff:127.0.0.1]:80" && !strings.Contains(err.Error(), "127.0.0.1") {
			t.Errorf("expected error to name the address, got %v", err)
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
	var blocked *BlockedAddressError
	if !errors.As(err, &blocked) {
		t.Fatalf("expected errors.As to find *BlockedAddressError, got %T: %v", err, err)
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
	var blocked *BlockedAddressError
	if !errors.As(err, &blocked) {
		t.Fatalf("expected *BlockedAddressError, got %T: %v", err, err)
	}
	if want := netip.MustParseAddr("10.1.2.3"); blocked.Addr != want {
		t.Fatalf("Addr = %v, want %v (IPv4-mapped form unmapped)", blocked.Addr, want)
	}
	if got, want := err.Error(), "refusing to connect to non-public address 10.1.2.3"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

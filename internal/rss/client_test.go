package rss

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
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
			if invalid, ok := errors.AsType[*InvalidURLError](err); !ok {
				t.Fatalf("expected *InvalidURLError, got %T: %v", err, err)
			} else if invalid.URL != tc.url {
				t.Fatalf("URL = %q, want %q", invalid.URL, tc.url)
			}
		})
	}
}

func TestFetchEntries_EnforcesSizeCap(t *testing.T) {
	oversized := strings.Repeat("a", maxFeedResponseBytes+1024)
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(oversized))
	}))

	// Use the test server's in-memory client rather than the default one, so
	// the SSRF guard (which has its own tests) doesn't get in the way and
	// this test stays isolated to the size-cap behavior.
	c := NewClient(srv.Client())
	_, err := c.FetchEntries(t.Context(), srv.URL, CacheValidators{})
	if err == nil {
		t.Fatal("expected error for oversized response, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds maximum allowed size") {
		t.Fatalf("unexpected error: %v", err)
	}
	tooLarge, ok := errors.AsType[*ResponseTooLargeError](err)
	if !ok {
		t.Fatalf("expected *ResponseTooLargeError, got %T: %v", err, err)
	}
	if tooLarge.Limit != maxFeedResponseBytes {
		t.Fatalf("Limit = %d, want %d", tooLarge.Limit, maxFeedResponseBytes)
	}
	if _, isParse := errors.AsType[*ParseError](err); isParse {
		t.Fatal("an oversized body must not be reported as a ParseError")
	}
}

func TestFetchEntries_ParseFailuresAreParseErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty body", ""},
		{"garbage", "this is not xml at all"},
		{"truncated", `<rss><channel><item><title>cut off`},
		{"unsupported charset", `<?xml version="1.0" encoding="x-no-such-charset"?><rss><channel></channel></rss>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			_, err := NewClient(srv.Client()).FetchEntries(t.Context(), srv.URL, CacheValidators{})
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if _, ok := errors.AsType[*ParseError](err); !ok {
				t.Fatalf("expected *ParseError, got %T: %v", err, err)
			}
			if _, ok := errors.AsType[*ResponseTooLargeError](err); ok {
				t.Fatal("a parse failure must not be a ResponseTooLargeError")
			}
		})
	}
}

func TestFetchEntries_ParsesValidRSS(t *testing.T) {
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sampleRSS))
	}))

	c := NewClient(srv.Client())
	result, err := c.FetchEntries(t.Context(), srv.URL, CacheValidators{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Entries) != 1 || result.Entries[0].Title != "Hello" {
		t.Fatalf("unexpected entries: %+v", result.Entries)
	}
}

func TestFetchEntries_ReturnsFeedTitle(t *testing.T) {
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<rss><channel><title>Example Blog</title><item><title>Hello</title></item></channel></rss>`))
	}))

	// Client must be called before srv.URL is read: it sets URL.
	c := NewClient(srv.Client())
	result, err := c.FetchEntries(t.Context(), srv.URL, CacheValidators{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.FeedTitle != "Example Blog" {
		t.Fatalf("FeedTitle = %q, want %q", result.FeedTitle, "Example Blog")
	}
}

func TestFetchEntries_SendsETagAndLastModifiedValidators(t *testing.T) {
	var gotIfNoneMatch, gotIfModifiedSince string
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIfNoneMatch = r.Header.Get("If-None-Match")
		gotIfModifiedSince = r.Header.Get("If-Modified-Since")
		_, _ = w.Write([]byte(sampleRSS))
	}))

	c := NewClient(srv.Client())
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
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUserAgent = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(sampleRSS))
	}))

	c := NewClient(srv.Client())
	_, err := c.FetchEntries(t.Context(), srv.URL, CacheValidators{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotUserAgent != userAgent {
		t.Fatalf("got User-Agent %q, want %q", gotUserAgent, userAgent)
	}
}

func TestFetchEntries_StoresValidatorsFromSuccessfulResponse(t *testing.T) {
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"new-etag"`)
		w.Header().Set("Last-Modified", "Thu, 22 Oct 2015 07:28:00 GMT")
		_, _ = w.Write([]byte(sampleRSS))
	}))

	c := NewClient(srv.Client())
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
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == testETag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		t.Fatal("expected request to carry the previously stored ETag")
	}))

	c := NewClient(srv.Client())
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
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("upstream feed is down"))
	}))

	c := NewClient(srv.Client())
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
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<rss><channel><item><title>Unclosed"))
	}))

	c := NewClient(srv.Client())
	_, err := c.FetchEntries(t.Context(), srv.URL, CacheValidators{})
	if err == nil {
		t.Fatal("expected error for malformed XML body, got nil")
	}
}

func TestFetchEntries_RefreshesValidatorsOnNotModified(t *testing.T) {
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A 304 can still carry a refreshed ETag/Last-Modified (e.g. when a
		// CDN rotates a weak validator without the underlying content
		// changing); the caller should store the refreshed validators rather
		// than blindly keeping whatever it sent.
		w.Header().Set("ETag", `"v2"`)
		w.Header().Set("Last-Modified", "Fri, 23 Oct 2015 07:28:00 GMT")
		w.WriteHeader(http.StatusNotModified)
	}))

	c := NewClient(srv.Client())
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

func TestFetchEntries_RespectsTimeout(t *testing.T) {
	// The handler's sleep and the client's timeout both run on the bubble's
	// fake clock, so this is instant and deterministic. The in-memory server
	// has to be created inside the bubble to be part of it.
	synctest.Test(t, func(t *testing.T) {
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(100 * time.Millisecond)
			_, _ = w.Write([]byte(sampleRSS))
		}))

		client := srv.Client()
		client.Timeout = 10 * time.Millisecond
		c := NewClient(client)
		_, err := c.FetchEntries(t.Context(), srv.URL, CacheValidators{})
		if err == nil {
			t.Fatal("expected timeout error, got nil")
		}
	})
}

func TestHTTPStatusError_ErrorWithoutBody(t *testing.T) {
	err := &HTTPStatusError{StatusCode: 503, Status: "503 Service Unavailable"}
	if got, want := err.Error(), "feed fetch failed: 503 Service Unavailable"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
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

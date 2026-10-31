package rss

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxFeedResponseBytes bounds how much of a feed response we will read into
// memory, to protect the controller from oversized or compression-bomb
// responses returned by a feed URL (which is fully operator-namespace
// controlled).
const maxFeedResponseBytes = 10 * 1024 * 1024 // 10MB

// defaultTimeout bounds how long a single feed fetch may take, since the
// feed URL is untrusted input and a slow/hanging server must not stall
// reconciliation indefinitely.
const defaultTimeout = 15 * time.Second

// userAgent identifies fetches as coming from this operator rather than Go's
// default "Go-http-client/1.1", which several CDNs (Cloudflare, Reddit, ...)
// block outright.
const userAgent = "rss2discord-operator (+https://github.com/maverickd650-labs/rss2discord)"

// HTTPStatusError is returned by FetchEntries when a feed responds with a
// non-2xx, non-304 status. Carrying the status code as a typed field (rather
// than only formatting it into the error string) lets callers classify the
// failure -- e.g. a permanent 404 vs. a transient 503 -- without parsing
// Error()'s text.
type HTTPStatusError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *HTTPStatusError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("feed fetch failed: %s", e.Status)
	}
	return fmt.Sprintf("feed fetch failed: %s: %s", e.Status, e.Body)
}

// ResponseTooLargeError is returned by FetchEntries when a feed response body
// exceeds the size cap. The cap is fixed, so the same feed keeps failing the
// same way until it shrinks.
type ResponseTooLargeError struct {
	// Limit is the maximum response size in bytes that was exceeded.
	Limit int64
}

func (e *ResponseTooLargeError) Error() string {
	return fmt.Sprintf("feed response exceeds maximum allowed size of %d bytes", e.Limit)
}

// InvalidURLError is returned by FetchEntries, before any request is made,
// for a feed URL that is empty, unparsable or not http(s). It is a feed
// misconfiguration that only a change to the URL fixes.
type InvalidURLError struct {
	// URL is the offending value as given.
	URL string
	// Msg is the human-readable reason; it is what Error returns.
	Msg string
	// Err is the underlying parse error, if any.
	Err error
}

func (e *InvalidURLError) Error() string { return e.Msg }

func (e *InvalidURLError) Unwrap() error { return e.Err }

// CacheValidators carries the conditional-GET validators returned by a
// previous fetch of a feed, so a later fetch can ask the server "has this
// changed since I last asked" instead of always re-downloading and
// re-parsing the full feed body.
type CacheValidators struct {
	ETag         string
	LastModified string
}

// FetchResult is the outcome of a conditional fetch. When NotModified is
// true (the server responded 304), Entries is nil and the caller should
// keep using whatever it already has; ETag/LastModified are carried through
// unchanged in that case so the caller can keep sending them next time.
type FetchResult struct {
	Entries []Entry
	// FeedTitle is the feed's own title (RSS/RDF channel title, Atom feed
	// title), or "" if it has none. Empty when NotModified.
	FeedTitle    string
	NotModified  bool
	ETag         string
	LastModified string
}

type Client struct {
	httpClient *http.Client
}

func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = newDefaultHTTPClient(nil)
	}

	return &Client{httpClient: httpClient}
}

// NewClientWithTransportWrap is like NewClient(nil), but passes the
// SSRF-guarded transport through wrap (e.g. otelhttp.NewTransport) before
// use. wrap wraps around the guarded transport, never replaces it, so the
// non-public-IP rejection in newDefaultHTTPClient still applies to every
// request. A nil wrap behaves exactly like NewClient(nil).
func NewClientWithTransportWrap(wrap func(http.RoundTripper) http.RoundTripper) *Client {
	return &Client{httpClient: newDefaultHTTPClient(wrap)}
}

// FetchEntries fetches and parses feedURL. If validators carries an ETag
// and/or LastModified from a previous fetch, they're sent as If-None-Match
// / If-Modified-Since so an unchanged feed costs a 304 instead of a full
// re-download and re-parse.
func (c *Client) FetchEntries(ctx context.Context, feedURL string, validators CacheValidators) (FetchResult, error) {
	if strings.TrimSpace(feedURL) == "" {
		return FetchResult{}, &InvalidURLError{URL: feedURL, Msg: "feed URL is empty"}
	}

	parsedURL, err := url.ParseRequestURI(feedURL)
	if err != nil {
		return FetchResult{}, &InvalidURLError{URL: feedURL, Msg: fmt.Sprintf("invalid feed URL %q: %v", feedURL, err), Err: err}
	}

	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return FetchResult{}, &InvalidURLError{URL: feedURL, Msg: fmt.Sprintf("unsupported feed URL scheme %q: only http/https are allowed", parsedURL.Scheme)}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsedURL.String(), nil)
	if err != nil {
		return FetchResult{}, err
	}
	req.Header.Set("Accept", "application/rss+xml, application/xml, text/xml, */*")
	req.Header.Set("User-Agent", userAgent)
	if validators.ETag != "" {
		req.Header.Set("If-None-Match", validators.ETag)
	}
	if validators.LastModified != "" {
		req.Header.Set("If-Modified-Since", validators.LastModified)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return FetchResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotModified {
		result := FetchResult{NotModified: true, ETag: validators.ETag, LastModified: validators.LastModified}
		if etag := resp.Header.Get("ETag"); etag != "" {
			result.ETag = etag
		}
		if lastModified := resp.Header.Get("Last-Modified"); lastModified != "" {
			result.LastModified = lastModified
		}
		return result, nil
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return FetchResult{}, &HTTPStatusError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       strings.TrimSpace(string(body)),
		}
	}

	limitedBody := io.LimitReader(resp.Body, maxFeedResponseBytes+1)
	data, err := io.ReadAll(limitedBody)
	if err != nil {
		return FetchResult{}, err
	}
	if len(data) > maxFeedResponseBytes {
		return FetchResult{}, &ResponseTooLargeError{Limit: maxFeedResponseBytes}
	}

	entries, feedTitle, err := parseFeed(data)
	if err != nil {
		return FetchResult{}, err
	}

	return FetchResult{
		Entries:      entries,
		FeedTitle:    feedTitle,
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
	}, nil
}

package rss

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/html/charset"
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
const userAgent = "rss2discord-operator (+https://github.com/maverickd650/rss2discord-operator)"

type Entry struct {
	ID          string
	Title       string
	Link        string
	Description string
	Image       string
	Author      string
	Categories  []string
	Published   time.Time
	// Seq is the entry's 0-based position in the feed document (0 = first
	// listed). RSS/Atom feeds list newest-first by convention, so a lower Seq
	// means more recent. It's the recency tiebreak used when Published is
	// missing or tied: many feeds omit per-entry dates entirely, and without a
	// fallback every such entry shares a zero timestamp and ordering collapses
	// to document order, which the consumer would otherwise read tail-first
	// (oldest) when picking the "most recent" entries.
	Seq int
}

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

// UnrecognizedFormatError is returned by parseFeed when the document's root
// element is none of the feed formats it understands (rss, feed, RDF). It's
// a distinct, permanent classification rather than a silent fall-through, so
// a feed in a format this parser doesn't support surfaces as a status
// condition instead of quietly posting zero entries forever.
type UnrecognizedFormatError struct {
	Root string
}

func (e *UnrecognizedFormatError) Error() string {
	return fmt.Sprintf("unrecognized feed format: root element <%s>", e.Root)
}

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
	Entries      []Entry
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

// FetchEntries fetches and parses feedURL. If validators carries an ETag
// and/or LastModified from a previous fetch, they're sent as If-None-Match
// / If-Modified-Since so an unchanged feed costs a 304 instead of a full
// re-download and re-parse.
func (c *Client) FetchEntries(ctx context.Context, feedURL string, validators CacheValidators) (FetchResult, error) {
	if strings.TrimSpace(feedURL) == "" {
		return FetchResult{}, fmt.Errorf("feed URL is empty")
	}

	parsedURL, err := url.ParseRequestURI(feedURL)
	if err != nil {
		return FetchResult{}, fmt.Errorf("invalid feed URL %q: %w", feedURL, err)
	}

	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return FetchResult{}, fmt.Errorf("unsupported feed URL scheme %q: only http/https are allowed", parsedURL.Scheme)
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
		return FetchResult{}, fmt.Errorf("feed response exceeds maximum allowed size of %d bytes", maxFeedResponseBytes)
	}

	entries, err := parseFeed(data)
	if err != nil {
		return FetchResult{}, err
	}

	return FetchResult{
		Entries:      entries,
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
	}, nil
}

type rssEnvelope struct {
	XMLName xml.Name   `xml:"rss"`
	Channel rssChannel `xml:"channel"`
}

type rssChannel struct {
	Items []rssItem `xml:"item"`
}

// rdfEnvelope is RSS 1.0's document shape: root <rdf:RDF>, with <item>
// elements listed as top-level siblings of <channel> rather than nested
// inside it (unlike RSS 2.0's rssChannel.Items). Items reuse the rssItem
// mapping -- RDF's title/link/description/dc:creator/dc:date fields are the
// same local names RSS 2.0 uses (dc:creator and dc:date match rssItem's
// Creator and DCDate regardless of namespace prefix).
type rdfEnvelope struct {
	XMLName xml.Name  `xml:"RDF"`
	Items   []rssItem `xml:"item"`
}

type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	Description string `xml:"description"`
	GUID        string `xml:"guid"`
	PubDate     string `xml:"pubDate"`
	Author      string `xml:"author"`
	// Creator matches Dublin Core's <dc:creator>, the de facto author field
	// for feeds (e.g. WordPress) that don't use RSS's own <author>.
	// encoding/xml matches by local name regardless of namespace prefix, so
	// this also covers <creator> without a dc: prefix.
	Creator        string        `xml:"creator"`
	Categories     []string      `xml:"category"`
	Enclosure      *rssEnclosure `xml:"enclosure"`
	MediaThumbnail *rssMediaURL  `xml:"thumbnail"`
	MediaContent   []rssMediaURL `xml:"content"`
	// DCDate matches Dublin Core's <dc:date>, RDF's (RSS 1.0) date field --
	// RDF items carry no <pubDate>. encoding/xml matches by local name
	// regardless of namespace prefix, so this also covers <date> without a
	// dc: prefix. Value is W3CDTF, a profile of RFC3339 that parseTime
	// already handles.
	DCDate string `xml:"date"`
}

// rssEnclosure is RSS's standard <enclosure url=".." type="image/..">,
// commonly used to attach a lead image to an item.
type rssEnclosure struct {
	URL  string `xml:"url,attr"`
	Type string `xml:"type,attr"`
}

// rssMediaURL covers the Media RSS namespace's <media:thumbnail> and
// <media:content>, both of which carry a url attribute and (for content)
// optional medium/type attributes identifying it as an image. encoding/xml
// matches by local name regardless of namespace prefix, so this also
// matches Atom's namespaced equivalents if a feed mixes vocabularies.
type rssMediaURL struct {
	URL    string `xml:"url,attr"`
	Medium string `xml:"medium,attr"`
	Type   string `xml:"type,attr"`
}

func (m rssMediaURL) isImage() bool {
	return strings.HasPrefix(m.Medium, "image") || strings.HasPrefix(m.Type, "image/")
}

// entryImage picks the first available image source from an RSS item's
// enclosure or Media RSS thumbnail/content elements.
func entryImage(enclosure *rssEnclosure, thumbnail *rssMediaURL, content []rssMediaURL) string {
	if enclosure != nil && strings.HasPrefix(enclosure.Type, "image/") && enclosure.URL != "" {
		return enclosure.URL
	}
	if thumbnail != nil && thumbnail.URL != "" {
		return thumbnail.URL
	}
	for _, c := range content {
		if c.isImage() && c.URL != "" {
			return c.URL
		}
	}
	return ""
}

type atomEntry struct {
	ID         string         `xml:"id"`
	Title      string         `xml:"title"`
	Links      []atomLink     `xml:"link"`
	Summary    string         `xml:"summary"`
	Content    string         `xml:"content"`
	Updated    string         `xml:"updated"`
	Published  string         `xml:"published"`
	Author     atomAuthor     `xml:"author"`
	Categories []atomCategory `xml:"category"`
	// Base is xml:base, an entry-level override of the feed's Base for
	// resolving this entry's relative link/enclosure hrefs. encoding/xml
	// matches "base,attr" by local name regardless of namespace prefix, so
	// this also covers the attribute written without an "xml:" prefix.
	Base string `xml:"base,attr"`
}

type atomAuthor struct {
	Name string `xml:"name"`
}

// atomCategory is Atom's <category term="..."/>; Term is the human-readable
// label (RSS's <category> has no separate machine label, so RSS and Atom
// categories are normalized to the same []string on Entry).
type atomCategory struct {
	Term string `xml:"term,attr"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
	Type string `xml:"type,attr"`
}

// primaryLink returns the entry's main article link: the "alternate" rel,
// or the first link if none is explicitly marked alternate (Atom defaults
// an unmarked link's rel to "alternate").
func primaryLink(links []atomLink) string {
	for _, l := range links {
		if l.Rel == "" || l.Rel == "alternate" {
			return l.Href
		}
	}
	if len(links) > 0 {
		return links[0].Href
	}
	return ""
}

// enclosureImage returns an Atom entry's image enclosure link, if any
// (<link rel="enclosure" type="image/..">).
func enclosureImage(links []atomLink) string {
	for _, l := range links {
		if l.Rel == "enclosure" && strings.HasPrefix(l.Type, "image/") {
			return l.Href
		}
	}
	return ""
}

type atomFeed struct {
	Entries []atomEntry `xml:"entry"`
	// Base is the feed-level xml:base, used to resolve an entry's relative
	// link/enclosure hrefs when the entry doesn't set its own.
	Base string `xml:"base,attr"`
}

// resolveAtomURL resolves ref against base per RFC 3986. Atom commonly
// supplies relative link hrefs (a bare path/slug) alongside an xml:base
// rather than a full URL; without resolving against it, the relative
// string is neither a usable link nor recognized as a URL by anything
// downstream (e.g. httpURLOrEmpty, or entry-identity normalization) that
// expects an absolute one. If ref is already absolute, or base is
// empty/unparseable, ref is returned unchanged.
func resolveAtomURL(base, ref string) string {
	if ref == "" || base == "" {
		return ref
	}
	refURL, err := url.Parse(ref)
	if err != nil || refURL.IsAbs() {
		return ref
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return ref
	}
	return baseURL.ResolveReference(refURL).String()
}

// trimCategories trims each category and drops any that are blank, so
// whitespace-only <category> elements don't surface as empty entries.
func trimCategories(raw []string) []string {
	categories := make([]string, 0, len(raw))
	for _, c := range raw {
		c = strings.TrimSpace(c)
		if c != "" {
			categories = append(categories, c)
		}
	}
	return categories
}

// unmarshalXML decodes data into v using encoding/xml, with a CharsetReader
// so feeds that declare a non-UTF-8 encoding (ISO-8859-1/windows-1252 are
// common among older or non-English-language publishers) decode instead of
// failing outright; encoding/xml has no charset support of its own.
func unmarshalXML(data []byte, v any) error {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	decoder.CharsetReader = charset.NewReaderLabel
	return decoder.Decode(v)
}

// rootElementName returns the local name of data's outermost XML element
// ("rss" or "feed", typically), reading only as far as that first start tag
// rather than decoding the whole document. parseFeed uses this to pick
// parseRSS vs. parseAtom without paying for a full Unmarshal twice -- once
// here and again in whichever of those it then calls.
func rootElementName(data []byte) (string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	decoder.CharsetReader = charset.NewReaderLabel
	for {
		tok, err := decoder.Token()
		if err != nil {
			return "", err
		}
		if start, ok := tok.(xml.StartElement); ok {
			return start.Name.Local, nil
		}
	}
}

func parseFeed(data []byte) ([]Entry, error) {
	root, err := rootElementName(data)
	if err != nil {
		return nil, err
	}

	switch strings.ToLower(root) {
	case "rss":
		return parseRSS(data)
	case "feed":
		return parseAtom(data)
	case "rdf":
		return parseRDF(data)
	default:
		return nil, &UnrecognizedFormatError{Root: root}
	}
}

func parseRSS(data []byte) ([]Entry, error) {
	var envelope rssEnvelope
	if err := unmarshalXML(data, &envelope); err != nil {
		return nil, err
	}

	return rssItemsToEntries(envelope.Channel.Items), nil
}

// parseRDF parses an RSS 1.0 (RDF) document. Unlike RSS 2.0, its <item>
// elements are top-level siblings of <channel> rather than nested inside it,
// so it needs its own envelope, but shares rssItem's field mapping and
// rssItemsToEntries' entry construction.
func parseRDF(data []byte) ([]Entry, error) {
	var envelope rdfEnvelope
	if err := unmarshalXML(data, &envelope); err != nil {
		return nil, err
	}

	return rssItemsToEntries(envelope.Items), nil
}

func rssItemsToEntries(items []rssItem) []Entry {
	entries := make([]Entry, 0, len(items))
	for i, item := range items {
		published, _ := parseTime(item.PubDate)
		if published.IsZero() {
			// RDF (RSS 1.0) items carry no <pubDate>; dc:date is their date
			// field instead.
			published, _ = parseTime(item.DCDate)
		}

		id := strings.TrimSpace(item.GUID)
		if id == "" {
			id = strings.TrimSpace(item.Link)
		}
		if id == "" {
			id = strings.TrimSpace(item.Title)
		}

		author := strings.TrimSpace(item.Author)
		if author == "" {
			author = strings.TrimSpace(item.Creator)
		}

		entries = append(entries, Entry{
			ID:          id,
			Title:       strings.TrimSpace(item.Title),
			Link:        strings.TrimSpace(item.Link),
			Description: strings.TrimSpace(item.Description),
			Image:       strings.TrimSpace(entryImage(item.Enclosure, item.MediaThumbnail, item.MediaContent)),
			Author:      author,
			Categories:  trimCategories(item.Categories),
			Published:   published,
			Seq:         i,
		})
	}

	return entries
}

func parseAtom(data []byte) ([]Entry, error) {
	var feed atomFeed
	if err := unmarshalXML(data, &feed); err != nil {
		return nil, err
	}

	feedBase := strings.TrimSpace(feed.Base)

	entries := make([]Entry, 0, len(feed.Entries))
	for i, item := range feed.Entries {
		published, _ := parseTime(item.Published)
		if published.IsZero() {
			published, _ = parseTime(item.Updated)
		}

		base := strings.TrimSpace(item.Base)
		if base == "" {
			base = feedBase
		}

		link := resolveAtomURL(base, primaryLink(item.Links))

		id := strings.TrimSpace(item.ID)
		if id == "" {
			id = strings.TrimSpace(link)
		}
		if id == "" {
			id = strings.TrimSpace(item.Title)
		}

		description := strings.TrimSpace(item.Summary)
		if description == "" {
			description = strings.TrimSpace(item.Content)
		}

		categoryTerms := make([]string, len(item.Categories))
		for i, c := range item.Categories {
			categoryTerms[i] = c.Term
		}

		entries = append(entries, Entry{
			ID:          id,
			Title:       strings.TrimSpace(item.Title),
			Link:        strings.TrimSpace(link),
			Description: description,
			Image:       strings.TrimSpace(resolveAtomURL(base, enclosureImage(item.Links))),
			Author:      strings.TrimSpace(item.Author.Name),
			Categories:  trimCategories(categoryTerms),
			Published:   published,
			Seq:         i,
		})
	}

	return entries, nil
}

func parseTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}

	// A failed time.Parse allocates a *ParseError, and RSS pubDates are
	// overwhelmingly RFC 1123 while Atom and RDF dates are ISO 8601, so trying
	// one flat list in a fixed order made nearly every RSS date pay for a
	// failed RFC 3339 attempt first. The two groups can't both match one
	// value (ISO dates start with a four-digit year and a dash), so try the
	// likelier group first and fall back to the other: the accepted inputs
	// and results are unchanged.
	first, second := rfcLayouts, isoLayouts
	if looksISO(value) {
		first, second = isoLayouts, rfcLayouts
	}
	for _, layouts := range [][]string{first, second} {
		for _, layout := range layouts {
			if t, err := time.Parse(layout, value); err == nil {
				return t.UTC(), nil
			}
		}
	}

	return time.Time{}, fmt.Errorf("unsupported time format %q", value)
}

// isoLayouts are the ISO 8601-style formats (RFC 3339 for Atom/RDF, plus
// two common space/date-only variants); rfcLayouts are the RFC 822/1123 and
// asctime formats RSS pubDate uses. Each keeps its relative order from the
// original single list.
var (
	isoLayouts = []string{
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02",
	}
	rfcLayouts = []string{
		time.RFC1123Z,
		time.RFC1123,
		time.RFC822Z,
		time.RFC822,
		time.ANSIC,
		"Mon, 2 Jan 2006 15:04:05 MST",
	}
)

// looksISO reports whether value starts like an ISO 8601 date: four digits
// and a dash.
func looksISO(value string) bool {
	if len(value) < 5 || value[4] != '-' {
		return false
	}
	for i := range 4 {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

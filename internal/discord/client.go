package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// defaultTimeout bounds how long a single webhook delivery may take.
const defaultTimeout = 10 * time.Second

// AllowedWebhookHosts restricts message delivery to Discord's own webhook
// hosts. The webhook URL is read from a Secret the operator can `get`
// cluster-wide, so without this guard a misconfigured or malicious secret
// could redirect message content (and implicitly, an SSRF-style POST) to an
// arbitrary host. Tests must never widen it: mock servers are addressed as
// real https://discord.com/... URLs on an in-memory network instead.
var AllowedWebhookHosts = map[string]bool{
	"discord.com":    true,
	"discordapp.com": true,
}

type Client struct {
	webhookURL  string
	httpClient  *http.Client
	rateLimiter *RateLimiter
}

func NewClient(webhookURL string) *Client {
	return NewClientWithHTTP(webhookURL, nil)
}

func NewClientWithHTTP(webhookURL string, httpClient *http.Client) *Client {
	return NewClientWithLimiter(webhookURL, httpClient, nil)
}

// NewClientWithLimiter is like NewClientWithHTTP, but also wires in a shared
// RateLimiter: a 429 seen on webhookURL by any Client built with the same
// limiter puts every one of them into cooldown, not just this one. Passing a
// nil limiter disables that cross-Client behavior (equivalent to
// NewClientWithHTTP).
// Surrounding URL whitespace is trimmed, and PTB/Canary hosts are normalized
// before choosing the shared rate-limit key. A nil httpClient uses NewHTTPClient;
// URL validation is deferred to SendMessage.
func NewClientWithLimiter(webhookURL string, httpClient *http.Client, limiter *RateLimiter) *Client {
	if httpClient == nil {
		httpClient = NewHTTPClient()
	}
	return &Client{webhookURL: normalizeWebhookURL(strings.TrimSpace(webhookURL)), httpClient: httpClient, rateLimiter: limiter}
}

// normalizeWebhookURL rewrites the host of a webhook URL copied from Discord's
// PTB or Canary client (ptb.discord.com, canary.discord.com) to discord.com,
// the same webhook endpoint. It deliberately runs before, and independently
// of, the AllowedWebhookHosts check: the allowlist is not broadened, so the
// request can still only ever go to an already-allowed host. Only an exact,
// port-less match is rewritten; lookalikes (evil.ptb.discord.com,
// ptb.discord.com.evil.com) pass through unchanged. The scheme is preserved;
// SendMessage rejects non-HTTPS URLs. Parse failures also pass through unchanged.
// Normalizing here also makes a PTB and a
// regular URL for the same webhook share one rate-limit key.
func normalizeWebhookURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Port() != "" {
		return raw
	}
	switch strings.ToLower(u.Hostname()) {
	case "ptb.discord.com", "canary.discord.com":
		u.Host = "discord.com"
		return u.String()
	}
	return raw
}

// NewHTTPClient returns the default HTTP client used for webhook delivery.
// Callers that build many *Client instances (one per FeedGroup webhook
// secret) should construct this once and share it via NewClientWithHTTP, so
// that connections to discord.com are pooled instead of rebuilt per client.
func NewHTTPClient() *http.Client {
	return NewHTTPClientWithTransportWrap(nil)
}

// NewHTTPClientWithTransportWrap is like NewHTTPClient, but passes the
// transport through wrap (e.g. otelhttp.NewTransport) before use. wrap wraps
// around the transport, never replaces it, so the redirect-refusal behavior
// below still applies to every request. A nil wrap behaves exactly like
// NewHTTPClient.
func NewHTTPClientWithTransportWrap(wrap func(http.RoundTripper) http.RoundTripper) *http.Client {
	var rt = http.DefaultTransport
	if wrap != nil {
		rt = wrap(rt)
	}

	return &http.Client{
		Timeout:   defaultTimeout,
		Transport: rt,
		// SendMessage checks the request URL's host against
		// AllowedWebhookHosts before sending, but http.Client.Do transparently
		// follows redirects by default, replaying the POST body (message
		// content) to whatever Location a response names -- unchecked against
		// that allowlist. Discord's webhook endpoint has no legitimate reason
		// to redirect, so refuse to follow one rather than trust it.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Embed describes a Discord embed ("bubble") to attach to a webhook message,
// matching the subset of Discord's embed object that feeds need: a colored
// side bar, optional thumbnail/image, author line, and footer.
type Embed struct {
	Title       string
	Description string
	URL         string
	// Color is a 24-bit RGB integer (0xRRGGBB), as used by Discord's API.
	Color        int
	Timestamp    string
	ThumbnailURL string
	// ImageURL is the large image shown below the embed's description, as
	// opposed to the small ThumbnailURL beside it.
	ImageURL   string
	AuthorName string
	FooterText string
}

// Message is a single Discord webhook delivery. Content is sent as a plain
// message; Embed, if set, renders as a colored bubble alongside it. Setting
// ThreadName posts the message as a new thread in a forum channel; ThreadID
// posts into an existing thread/forum post instead.
type Message struct {
	Content    string
	Embed      *Embed
	ThreadName string
	ThreadID   string
	Username   string
	AvatarURL  string
}

type discordPayload struct {
	Content         string                 `json:"content,omitempty"`
	Embeds          []discordEmbed         `json:"embeds,omitempty"`
	ThreadName      string                 `json:"thread_name,omitempty"`
	Username        string                 `json:"username,omitempty"`
	AvatarURL       string                 `json:"avatar_url,omitempty"`
	AllowedMentions discordAllowedMentions `json:"allowed_mentions"`
}

// discordAllowedMentions with an empty Parse list suppresses all @everyone,
// @here, role, and user mentions. Feed entry titles/descriptions are
// untrusted external text that ends up in the plain-content path, so
// without this a feed containing "@everyone" would actually ping the
// channel. Embeds are unaffected either way: Discord never parses mentions
// out of embed fields.
type discordAllowedMentions struct {
	Parse []string `json:"parse"`
}

type discordEmbed struct {
	Title       string              `json:"title,omitempty"`
	Description string              `json:"description,omitempty"`
	URL         string              `json:"url,omitempty"`
	Color       int                 `json:"color,omitempty"`
	Timestamp   string              `json:"timestamp,omitempty"`
	Thumbnail   *discordEmbedMedia  `json:"thumbnail,omitempty"`
	Image       *discordEmbedMedia  `json:"image,omitempty"`
	Author      *discordEmbedAuthor `json:"author,omitempty"`
	Footer      *discordEmbedFooter `json:"footer,omitempty"`
}

type discordEmbedMedia struct {
	URL string `json:"url"`
}

type discordEmbedAuthor struct {
	Name string `json:"name"`
}

type discordEmbedFooter struct {
	Text string `json:"text"`
}

// RateLimitError indicates Discord responded with HTTP 429 and how long the
// caller must wait before retrying, per the response's Retry-After header.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("discord webhook rate limited, retry after %s", e.RetryAfter)
}

// HTTPStatusError is returned by SendMessage when Discord responds with a
// non-2xx status other than 429 (which gets the more specific
// RateLimitError). Carrying the status code as a typed field lets callers
// classify the failure -- e.g. a 404 (webhook deleted) vs. a 5xx -- without
// parsing Error()'s text.
type HTTPStatusError struct {
	StatusCode int
	Status     string
	Detail     string
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("discord webhook returned %s%s", e.Status, e.Detail)
}

// maxCooldown is the finite ceiling for any wait derived from a response
// header: Retry-After on a 429 and X-RateLimit-Reset-After on a success. The
// header values are untrusted, and a cooldown is never shortened (see
// RateLimiter.cooldown), so an excessive deadline could not be repaired by a
// later response; it is bounded here, before it exists, instead.
const maxCooldown = time.Hour

// boundedSeconds converts a rate-limit header value -- a number of seconds,
// optionally fractional -- to a Duration in [0, maxCooldown]. It validates
// before converting, so the float-to-Duration conversion can never overflow:
// a missing, non-numeric, NaN or negative value (including -Inf) is rejected
// (ok is false), while +Inf and anything at or above maxCooldown clamp to it.
func boundedSeconds(value string) (d time.Duration, ok bool) {
	seconds, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || math.IsNaN(seconds) || seconds < 0 {
		return 0, false
	}
	if seconds >= maxCooldown.Seconds() {
		return maxCooldown, true
	}
	return time.Duration(seconds * float64(time.Second)), true
}

// parseRetryAfter parses Discord's Retry-After header (see boundedSeconds). It
// defaults to 1s if the header is missing or malformed, so callers always back
// off at least briefly.
func parseRetryAfter(value string) time.Duration {
	if d, ok := boundedSeconds(value); ok {
		return d
	}
	return time.Second
}

// preparedRequest is a message that has passed every check SendMessage makes
// before touching the network: it is valid, fitted to Discord's limits, bound
// for an allowed host over HTTPS, and already marshalled.
type preparedRequest struct {
	// limiterKey is the webhook URL as configured (not the thread-qualified
	// target), which keys the shared rate limiter so every message to one
	// webhook shares a bucket whatever its thread.
	limiterKey string
	// target is the URL the request is POSTed to: the webhook plus any
	// thread_id query. It carries the webhook token and so must never be put
	// in an error.
	target string
	body   []byte
}

// redactURLError strips the request URL from err. Both *url.Error producers
// we call (url.ParseRequestURI and http.Client.Do) embed the full URL --
// including the webhook token -- in Error(), and that text ends up in
// FeedGroup.Status, Conditions, and Kubernetes Events, all readable with much
// weaker RBAC than the Secret itself. It is the single place the rule "no
// error from prepareRequest or exchange contains the webhook URL" is enforced
// for wrapped errors; every other error here is built from constant text.
func redactURLError(err error) error {
	if urlErr, ok := errors.AsType[*url.Error](err); ok {
		return urlErr.Err
	}
	return err
}

// prepareRequest does all of SendMessage's pure work: validation, Fit, URL
// parsing, the HTTPS and AllowedWebhookHosts check, the thread query and
// payload marshalling. It touches neither the network nor a rate limiter, so
// a request it rejects (notably one for a disallowed host) can never reach
// either. Errors never contain the webhook URL (see redactURLError).
func prepareRequest(webhookURL string, msg Message) (preparedRequest, error) {
	if strings.TrimSpace(webhookURL) == "" {
		return preparedRequest{}, fmt.Errorf("discord webhook URL is empty")
	}
	// Apply every Discord length limit here so SendMessage is safe for any
	// caller; callers that already fitted the message pay almost nothing
	// because Fit is idempotent.
	msg, _ = Fit(msg)
	if strings.TrimSpace(msg.Content) == "" && msg.Embed == nil {
		return preparedRequest{}, fmt.Errorf("discord message content is empty")
	}

	parsedURL, err := url.ParseRequestURI(webhookURL)
	if err != nil {
		return preparedRequest{}, fmt.Errorf("invalid discord webhook URL: %w", redactURLError(err))
	}
	if parsedURL.Scheme != "https" || !AllowedWebhookHosts[strings.ToLower(parsedURL.Hostname())] {
		return preparedRequest{}, fmt.Errorf("discord webhook URL must be an https://discord.com or https://discordapp.com URL")
	}

	if strings.TrimSpace(msg.ThreadID) != "" {
		query := parsedURL.Query()
		query.Set("thread_id", msg.ThreadID)
		parsedURL.RawQuery = query.Encode()
	}

	payload := discordPayload{
		Content:         msg.Content,
		ThreadName:      msg.ThreadName,
		Username:        msg.Username,
		AvatarURL:       msg.AvatarURL,
		AllowedMentions: discordAllowedMentions{Parse: []string{}},
	}
	if msg.Embed != nil {
		payload.Embeds = []discordEmbed{embedToPayload(*msg.Embed)}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return preparedRequest{}, err
	}
	return preparedRequest{limiterKey: webhookURL, target: parsedURL.String(), body: body}, nil
}

// exchange sends an already-prepared request: it waits on and reserves the
// shared rate limiter, makes the HTTP call, maps the response status and
// records Discord's pacing headers. It performs no validation -- everything
// that decides whether the target is acceptable happened in prepareRequest.
// Errors never contain the webhook URL (see redactURLError).
func (c *Client) exchange(ctx context.Context, p preparedRequest) error {
	if c.rateLimiter != nil {
		if err := c.rateLimiter.wait(ctx, p.limiterKey); err != nil {
			return err
		}
		if remaining, cooling := c.rateLimiter.reserve(p.limiterKey); cooling {
			return &RateLimitError{RetryAfter: remaining}
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.target, bytes.NewReader(p.body))
	if err != nil {
		// A target prepareRequest built always parses, so only non-URL errors
		// (e.g. a nil Context) reach here, and they pass through unchanged.
		// Should a *url.Error ever appear it would carry the token-bearing
		// target, so it is redacted like Do's.
		return redactURLError(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("discord webhook request failed: %w", redactURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusTooManyRequests {
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		if c.rateLimiter != nil {
			c.rateLimiter.cooldown(p.limiterKey, retryAfter)
		}
		return &RateLimitError{RetryAfter: retryAfter}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HTTPStatusError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Detail:     decodeErrorDetail(resp.Body),
		}
	}

	c.notePacing(p.limiterKey, resp.Header)
	return nil
}

// SendMessage delivers msg to the webhook. Either Content or Embed (or both)
// must be set. SendMessageText is a convenience wrapper for the common
// plain-content-only case.
func (c *Client) SendMessage(ctx context.Context, msg Message) error {
	p, err := prepareRequest(c.webhookURL, msg)
	if err != nil {
		return err
	}
	return c.exchange(ctx, p)
}

// maxPaceWait is the longest a send will sleep for a webhook's bucket to
// refill. Discord's buckets reset within a couple of seconds; anything much
// longer is better surfaced as a rate limit so the reconcile backs off.
const maxPaceWait = 5 * time.Second

// notePacing reads Discord's X-RateLimit-* headers from a successful response
// and, if this send emptied the webhook's bucket, tells the shared limiter so
// the next send waits for the reset instead of earning a 429. key is the
// limiter bucket the send waited on and reserved, so pacing lands in the same
// bucket.
func (c *Client) notePacing(key string, h http.Header) {
	if c.rateLimiter == nil || strings.TrimSpace(h.Get("X-RateLimit-Remaining")) != "0" {
		return
	}
	d, ok := boundedSeconds(h.Get("X-RateLimit-Reset-After"))
	if !ok || d <= 0 {
		return
	}
	if d > maxPaceWait {
		c.rateLimiter.cooldown(key, d)
		return
	}
	c.rateLimiter.pace(key, d)
}

// decodeErrorDetail extracts Discord's machine-readable error code/message
// from an error response body so the surfaced error (and the FeedGroup's
// LastError/Event) says *why* a send failed -- "invalid webhook token" rather
// than a bare "400 Bad Request". Discord error bodies look like
// {"message":"...","code":50027,...}. Returns "" (so the caller's status line
// stands alone) when the body is empty or not the expected shape.
func decodeErrorDetail(body io.Reader) string {
	var parsed struct {
		Message string `json:"message"`
		Code    int    `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(body, 4096)).Decode(&parsed); err != nil {
		return ""
	}
	if parsed.Message == "" && parsed.Code == 0 {
		return ""
	}
	if parsed.Code != 0 {
		return fmt.Sprintf(": %s (code %d)", parsed.Message, parsed.Code)
	}
	return fmt.Sprintf(": %s", parsed.Message)
}

// SendMessageText is a convenience wrapper around SendMessage for plain,
// embed-less content.
func (c *Client) SendMessageText(ctx context.Context, content string) error {
	return c.SendMessage(ctx, Message{Content: content})
}

// embedToPayload converts e to the webhook payload, omitting empty media,
// author, and footer fields. ThumbnailURL and ImageURL are included
// independently when set. Length limits are applied beforehand, by Fit.
func embedToPayload(e Embed) discordEmbed {
	payload := discordEmbed{
		Title:       e.Title,
		Description: e.Description,
		URL:         e.URL,
		Color:       e.Color,
		Timestamp:   e.Timestamp,
	}
	if e.ThumbnailURL != "" {
		payload.Thumbnail = &discordEmbedMedia{URL: e.ThumbnailURL}
	}
	if e.ImageURL != "" {
		payload.Image = &discordEmbedMedia{URL: e.ImageURL}
	}
	if e.AuthorName != "" {
		payload.Author = &discordEmbedAuthor{Name: e.AuthorName}
	}
	if e.FooterText != "" {
		payload.Footer = &discordEmbedFooter{Text: e.FooterText}
	}
	return payload
}

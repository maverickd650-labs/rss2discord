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
// arbitrary host. Exported so tests can register a mock server's host.
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

// SendMessage delivers msg to the webhook. Either Content or Embed (or both)
// must be set. SendMessageText is a convenience wrapper for the common
// plain-content-only case.
func (c *Client) SendMessage(ctx context.Context, msg Message) error {
	if strings.TrimSpace(c.webhookURL) == "" {
		return fmt.Errorf("discord webhook URL is empty")
	}
	if strings.TrimSpace(msg.Content) == "" && msg.Embed == nil {
		return fmt.Errorf("discord message content is empty")
	}

	parsedURL, err := url.ParseRequestURI(c.webhookURL)
	if err != nil {
		// *url.Error's Error() embeds the raw URL -- including the webhook
		// token -- so surface only the underlying reason, as on the
		// transport-error path below.
		if urlErr, ok := errors.AsType[*url.Error](err); ok {
			err = urlErr.Err
		}
		return fmt.Errorf("invalid discord webhook URL: %w", err)
	}
	if parsedURL.Scheme != "https" || !AllowedWebhookHosts[strings.ToLower(parsedURL.Hostname())] {
		return fmt.Errorf("discord webhook URL must be an https://discord.com or https://discordapp.com URL")
	}

	if strings.TrimSpace(msg.ThreadID) != "" {
		query := parsedURL.Query()
		query.Set("thread_id", msg.ThreadID)
		parsedURL.RawQuery = query.Encode()
	}

	if c.rateLimiter != nil {
		if err := c.rateLimiter.wait(ctx, c.webhookURL); err != nil {
			return err
		}
		if remaining, cooling := c.rateLimiter.reserve(c.webhookURL); cooling {
			return &RateLimitError{RetryAfter: remaining}
		}
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
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, parsedURL.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// http.Client.Do wraps transport failures in a *url.Error whose
		// Error() includes the full request URL -- which contains the
		// webhook token. That string ends up in FeedGroup.Status,
		// Conditions, and Kubernetes Events, all readable with much
		// weaker RBAC than the Secret itself, so strip the URL before
		// it leaves this function.
		if urlErr, ok := errors.AsType[*url.Error](err); ok {
			return fmt.Errorf("discord webhook request failed: %w", urlErr.Err)
		}
		return fmt.Errorf("discord webhook request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusTooManyRequests {
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		if c.rateLimiter != nil {
			c.rateLimiter.cooldown(c.webhookURL, retryAfter)
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

	c.notePacing(resp.Header)
	return nil
}

// maxPaceWait is the longest a send will sleep for a webhook's bucket to
// refill. Discord's buckets reset within a couple of seconds; anything much
// longer is better surfaced as a rate limit so the reconcile backs off.
const maxPaceWait = 5 * time.Second

// notePacing reads Discord's X-RateLimit-* headers from a successful response
// and, if this send emptied the webhook's bucket, tells the shared limiter so
// the next send waits for the reset instead of earning a 429.
func (c *Client) notePacing(h http.Header) {
	if c.rateLimiter == nil || strings.TrimSpace(h.Get("X-RateLimit-Remaining")) != "0" {
		return
	}
	d, ok := boundedSeconds(h.Get("X-RateLimit-Reset-After"))
	if !ok || d <= 0 {
		return
	}
	if d > maxPaceWait {
		c.rateLimiter.cooldown(c.webhookURL, d)
		return
	}
	c.rateLimiter.pace(c.webhookURL, d)
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

// maxEmbedTotalLength is Discord's cap on the combined character count of
// title + description + footer.text + author.name (+ field name/value, which
// this package doesn't use) across all embeds in a single message. Discord
// rejects the whole webhook call with a 400 if exceeded, even though each
// field individually stays under its own per-field limit.
const maxEmbedTotalLength = 6000

// embedToPayload converts e to the webhook payload, clamping its description
// to the combined embed text budget and omitting empty media, author, and footer
// fields. ThumbnailURL and ImageURL are included independently when set.
func embedToPayload(e Embed) discordEmbed {
	e = clampEmbedTotalLength(e)
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

// EmbedTotalLengthOverflow reports how many runes of e.Description
// SendMessage will trim to satisfy Discord's combined embed length limit
// (maxEmbedTotalLength), without performing the trim. Exported so a caller
// that already measures its own per-field truncation for metrics (see
// internal/controller/feedgroup_controller.go's messageOverflowChars) can
// fold this clamp into the same count -- otherwise it happens invisibly
// inside SendMessage, after the caller has already taken its measurement.
func EmbedTotalLengthOverflow(e Embed) int {
	total := len([]rune(e.Title)) + len([]rune(e.Description)) + len([]rune(e.FooterText)) + len([]rune(e.AuthorName))
	if total <= maxEmbedTotalLength {
		return 0
	}
	return total - maxEmbedTotalLength
}

// clampEmbedTotalLength shrinks Description, the field with the most slack,
// until title+description+footer+author fits within maxEmbedTotalLength.
// Title/footer/author come from feed config and entry titles, which are
// short in practice, so trimming the (often long) description is the least
// surprising way to enforce the limit.
func clampEmbedTotalLength(e Embed) Embed {
	overflow := EmbedTotalLengthOverflow(e)
	if overflow == 0 {
		return e
	}

	descRunes := []rune(e.Description)
	newLen := max(len(descRunes)-overflow, 0)
	e.Description = truncateRunes(descRunes, newLen)
	return e
}

// truncateRunes trims runes to at most max, appending an ellipsis in place
// of the last truncated rune when anything was actually cut.
func truncateRunes(runes []rune, max int) string {
	if len(runes) <= max {
		return string(runes)
	}
	if max <= 0 {
		return ""
	}
	const ellipsis = "…"
	return string(runes[:max-len([]rune(ellipsis))]) + ellipsis
}

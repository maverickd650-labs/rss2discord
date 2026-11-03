package telemetry

import (
	"context"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// redactingExporter wraps a delegate SpanExporter and strips the Discord
// webhook token from any recorded request-URL span attribute before spans
// reach the delegate. otelhttp sets the full request URL -- including path
// -- as a span attribute via span.SetAttributes after the span starts (not
// as a start-time attribute), so redaction has to happen at export time,
// once every attribute otelhttp will ever add is already present. For
// outbound Discord webhook requests that URL's path carries the delivery
// token (https://discord.com/api/webhooks/<id>/<token>), which must never
// reach a trace backend.
type redactingExporter struct {
	next sdktrace.SpanExporter
}

func newRedactingExporter(next sdktrace.SpanExporter) *redactingExporter {
	return &redactingExporter{next: next}
}

func (e *redactingExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	redacted := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, s := range spans {
		redacted[i] = redactedSpan{s}
	}
	return e.next.ExportSpans(ctx, redacted)
}

func (e *redactingExporter) Shutdown(ctx context.Context) error { return e.next.Shutdown(ctx) }

// redactedSpan overrides Attributes() to redact webhook tokens; every other
// method is the embedded ReadOnlySpan's.
type redactedSpan struct {
	sdktrace.ReadOnlySpan
}

func (s redactedSpan) Attributes() []attribute.KeyValue {
	out := slices.Clone(s.ReadOnlySpan.Attributes())
	for i, attr := range out {
		// Don't gate on a fixed set of attribute keys (e.g. "url.full") --
		// a future otelhttp/semconv version could start recording the
		// request URL under a different key, and a key-based allowlist
		// would silently stop redacting it. Instead, inspect every
		// string-valued attribute: redactWebhookURL is a safe no-op on
		// anything that isn't a Discord webhook URL.
		if attr.Value.Type() != attribute.STRING {
			continue
		}
		if redacted, changed := redactWebhookURL(attr.Value.AsString()); changed {
			out[i] = attribute.String(string(attr.Key), redacted)
		}
	}
	return out
}

// redactWebhookURL replaces the token segment of a Discord webhook URL
// (.../api/webhooks/<id>/<token>[/...]) with a fixed placeholder. Returns
// the input unchanged (changed=false) if it doesn't look like a Discord
// webhook URL.
//
// A string url.Parse rejects can't reach a span today (both clients reject
// unparsable URLs before issuing a request, and otelhttp records url.full
// from the parsed request URL), but as defence in depth it falls back to a
// regexp that redacts the segment after /webhooks/<id>/.
// webhookTokenRE matches /webhooks/<id>/<token> and captures everything up to
// the token, which runs to the next '/', '?', '#' or end of string.
var webhookTokenRE = regexp.MustCompile(`(/webhooks/[^/?#]+/)[^/?#]+`)

func redactWebhookURL(raw string) (redacted string, changed bool) {
	u, err := url.Parse(raw)
	if err != nil {
		redacted = webhookTokenRE.ReplaceAllString(raw, "${1}REDACTED")
		return redacted, redacted != raw
	}

	segments := strings.Split(strings.Trim(u.Path, "/"), "/")
	// [... "api" "webhooks" <id> <token> ...]
	idx := slices.Index(segments, "webhooks")
	if idx == -1 || idx+2 >= len(segments) {
		return raw, false
	}

	segments[idx+2] = "REDACTED"
	u.Path = "/" + strings.Join(segments, "/")
	return u.String(), true
}

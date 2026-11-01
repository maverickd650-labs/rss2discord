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

package controller

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"

	"github.com/maverickd650-labs/rss2discord/internal/discord"
	"github.com/maverickd650-labs/rss2discord/internal/rss"
)

// failureClass identifies why a fetch or send attempt failed, independent of
// the free-text error string. It drives three things that would otherwise
// all have to agree on parsing the same message: the metric outcome label
// (metricReason), the status condition Reason (conditionReason), and whether
// the failure is one a feed/webhook can be expected to recover from on its
// own (permanent).
type failureClass struct {
	// metricReason is a low-cardinality, Prometheus-label-safe identifier
	// (e.g. "not_found") suffixed onto the fetch_error/send_error outcome.
	metricReason string
	// conditionReason is the matching CamelCase status condition Reason
	// (e.g. "HTTP404"), following Kubernetes condition reason conventions.
	conditionReason string
	// permanent means retrying on the normal/backoff schedule won't help:
	// the same request will keep failing the same way until the feed/webhook
	// configuration itself changes. Callers use this to stop counting
	// against RetryCount escalation once classification is available,
	// rather than only discovering "this isn't transient" after retries run
	// out.
	permanent bool
}

// Condition reason strings, following Kubernetes condition reason
// conventions (CamelCase, no spaces), and the matching lowercase
// metricReason identifiers. Both are named (rather than inlined into the
// failureClass literals below) so the *_test.go table tests can assert
// against the same constants instead of duplicating the literals, which
// would otherwise also trip goconst.
const (
	reasonHTTP404            = "HTTP404"
	reasonHTTP410            = "HTTP410"
	reasonRateLimited        = "RateLimited"
	reasonServerError        = "ServerError"
	reasonClientError        = "ClientError"
	reasonTimeout            = "Timeout"
	reasonDNSFailure         = "DNSFailure"
	reasonParseError         = "ParseError"
	reasonUnrecognizedFormat = "UnrecognizedFormat"
	reasonWebhookInvalid     = "WebhookInvalid"
	reasonBlockedAddress     = "BlockedAddress"
	reasonTLSError           = "TLSError"
	reasonResponseTooLarge   = "ResponseTooLarge"
	reasonInvalidURL         = "InvalidURL"
	reasonOther              = "Other"

	metricNotFound           = "not_found"
	metricGone               = "gone"
	metricRateLimited        = "rate_limited"
	metricServerError        = "server_error"
	metricClientError        = "client_error"
	metricTimeout            = "timeout"
	metricDNSFailure         = "dns_failure"
	metricParseError         = "parse_error"
	metricUnrecognizedFormat = "unrecognized_format"
	metricWebhookInvalid     = "webhook_invalid"
	metricBlockedAddress     = "blocked_address"
	metricTLSError           = "tls_error"
	metricResponseTooLarge   = "response_too_large"
	metricInvalidURL         = "invalid_url"
	metricOther              = "other"
)

var (
	classNotFound           = failureClass{metricNotFound, reasonHTTP404, true}
	classGone               = failureClass{metricGone, reasonHTTP410, true}
	classRateLimited        = failureClass{metricRateLimited, reasonRateLimited, false}
	classServerError        = failureClass{metricServerError, reasonServerError, false}
	classClientError        = failureClass{metricClientError, reasonClientError, true}
	classTimeout            = failureClass{metricTimeout, reasonTimeout, false}
	classDNSFailure         = failureClass{metricDNSFailure, reasonDNSFailure, false}
	classParseError         = failureClass{metricParseError, reasonParseError, true}
	classUnrecognizedFormat = failureClass{metricUnrecognizedFormat, reasonUnrecognizedFormat, true}
	classWebhookInvalid     = failureClass{metricWebhookInvalid, reasonWebhookInvalid, true}
	classBlockedAddress     = failureClass{metricBlockedAddress, reasonBlockedAddress, true}
	classTLSError           = failureClass{metricTLSError, reasonTLSError, true}
	classResponseTooLarge   = failureClass{metricResponseTooLarge, reasonResponseTooLarge, true}
	classInvalidURL         = failureClass{metricInvalidURL, reasonInvalidURL, true}
	classOther              = failureClass{metricOther, reasonOther, false}
)

// classifyFetchError maps an error returned by rss.Client.FetchEntries to the
// failureClass an operator (and Prometheus/kubectl) need to tell a feed that
// is down for a known, specific reason from one that's failing in some novel
// way that deserves attention.
func classifyFetchError(err error) failureClass {
	if statusErr, ok := errors.AsType[*rss.HTTPStatusError](err); ok {
		return classifyHTTPStatus(statusErr.StatusCode)
	}

	// Before classifyNetworkError: the SSRF guard's refusal and certificate
	// failures reach the caller wrapped in *url.Error / *net.OpError, which
	// are net.Errors, so they must be recognised by their typed cause first.
	if _, ok := errors.AsType[*rss.BlockedAddressError](err); ok {
		return classBlockedAddress
	}

	if isTLSCertificateError(err) {
		return classTLSError
	}

	if class, ok := classifyNetworkError(err); ok {
		return class
	}

	// Before ParseError: an unrecognized root element is reported as a
	// *rss.ParseError wrapping the *rss.UnrecognizedFormatError.
	if _, ok := errors.AsType[*rss.UnrecognizedFormatError](err); ok {
		return classUnrecognizedFormat
	}

	if _, ok := errors.AsType[*rss.ParseError](err); ok {
		return classParseError
	}

	if _, ok := errors.AsType[*rss.ResponseTooLargeError](err); ok {
		return classResponseTooLarge
	}

	if _, ok := errors.AsType[*rss.InvalidURLError](err); ok {
		return classInvalidURL
	}

	return classOther
}

// classifySendError maps an error returned by discord.Client.SendMessage to
// a failureClass. RateLimitError is handled separately by the caller (it
// already carries its own Retry-After-driven backoff), so this only needs to
// cover the remaining send failures.
func classifySendError(err error) failureClass {
	if _, ok := errors.AsType[*discord.RateLimitError](err); ok {
		return classRateLimited
	}

	if statusErr, ok := errors.AsType[*discord.HTTPStatusError](err); ok {
		if statusErr.StatusCode == 404 {
			// A deleted/regenerated webhook returns 404 from Discord's API,
			// distinct from a feed 404: it means the *destination*, not the
			// source, is gone.
			return classWebhookInvalid
		}
		return classifyHTTPStatus(statusErr.StatusCode)
	}

	// Discord's certificate is publicly trusted, so a verification failure
	// here means something between the operator and Discord (an egress
	// TLS-intercepting proxy, a missing corporate CA) is substituting its
	// own. That doesn't clear on a retry, only when the cluster's egress or
	// trust store is fixed, so treat it as permanent (exponential backoff
	// capped at 6h) rather than hammering Discord at full cadence.
	if isTLSCertificateError(err) {
		return classTLSError
	}

	if class, ok := classifyNetworkError(err); ok {
		return class
	}

	return classOther
}

// classifyHTTPStatus buckets a raw HTTP status code shared by both the fetch
// and send paths.
func classifyHTTPStatus(statusCode int) failureClass {
	switch {
	case statusCode == 404:
		return classNotFound
	case statusCode == 410:
		return classGone
	case statusCode == 429:
		return classRateLimited
	case statusCode >= 500:
		return classServerError
	case statusCode >= 400:
		return classClientError
	default:
		return classOther
	}
}

// isTLSCertificateError reports whether err is a server-certificate
// verification failure (untrusted issuer, wrong hostname, expired/invalid
// chain). crypto/tls wraps the x509 error in *tls.CertificateVerificationError
// on a client handshake, but the x509 types are checked too so a bare one
// surfaced through another path is still recognised. Other TLS failures
// (handshake timeouts, resets) are deliberately not matched: those are
// transient network faults.
func isTLSCertificateError(err error) bool {
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
		return true
	}
	if _, ok := errors.AsType[x509.UnknownAuthorityError](err); ok {
		return true
	}
	if _, ok := errors.AsType[x509.HostnameError](err); ok {
		return true
	}
	if _, ok := errors.AsType[x509.CertificateInvalidError](err); ok {
		return true
	}
	return false
}

// classifyNetworkError detects the lower-level transport failures (DNS,
// timeout) that both rss.Client and discord.Client surface as plain *url.Error
// / *net.DNSError / net.Error rather than a typed status error.
func classifyNetworkError(err error) (failureClass, bool) {
	if _, ok := errors.AsType[*net.DNSError](err); ok {
		return classDNSFailure, true
	}

	if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
		return classTimeout, true
	}

	return failureClass{}, false
}

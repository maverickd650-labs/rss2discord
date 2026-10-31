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
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"testing"
	"time"

	"github.com/maverickd650-labs/rss2discord/internal/discord"
	"github.com/maverickd650-labs/rss2discord/internal/rss"
)

// http404Status is the HTTP status string for a 404 Not Found response,
// shared across classify_test.go and processfeed_test.go to satisfy goconst.
const http404Status = "404 Not Found"

// doError wraps err the way http.Client.Do does for a failed request: a
// *url.Error around the transport error. dialErr additionally wraps it in the
// *net.OpError a dial failure (such as the SSRF guard's Control refusal)
// surfaces as.
func doError(err error, dialErr bool) error {
	if dialErr {
		err = &net.OpError{Op: "dial", Net: "tcp", Err: err}
	}
	return &url.Error{Op: "Get", URL: "https://feed.example/rss", Err: err}
}

// timeoutError is a minimal net.Error whose Timeout() reports true, standing
// in for the *url.Error a real client.Do timeout would surface (which itself
// wraps a timeout net.Error -- errors.As unwraps through that the same way).
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestClassifyFetchError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want failureClass
	}{
		{reasonHTTP404, &rss.HTTPStatusError{StatusCode: 404, Status: http404Status}, classNotFound},
		{reasonHTTP410, &rss.HTTPStatusError{StatusCode: 410, Status: "410 Gone"}, classGone},
		{reasonRateLimited, &rss.HTTPStatusError{StatusCode: 429, Status: "429 Too Many Requests"}, classRateLimited},
		{reasonServerError, &rss.HTTPStatusError{StatusCode: 503, Status: "503 Service Unavailable"}, classServerError},
		{reasonClientError, &rss.HTTPStatusError{StatusCode: 401, Status: "401 Unauthorized"}, classClientError},
		{"OtherStatusCode", &rss.HTTPStatusError{StatusCode: 100, Status: "100 Continue"}, classOther},
		{reasonTimeout, fmt.Errorf("fetch: %w", timeoutError{}), classTimeout},
		{reasonDNSFailure, fmt.Errorf("fetch: %w", &net.DNSError{Err: "no such host", Name: "example.invalid"}), classDNSFailure},
		{reasonParseError, fmt.Errorf("fetch: %w", &rss.ParseError{Err: io.EOF}), classParseError},
		{reasonUnrecognizedFormat, fmt.Errorf("fetch: %w", &rss.ParseError{Err: &rss.UnrecognizedFormatError{Root: "rdf"}}), classUnrecognizedFormat},
		{reasonResponseTooLarge, fmt.Errorf("fetch: %w", &rss.ResponseTooLargeError{Limit: 10}), classResponseTooLarge},
		{reasonInvalidURL, fmt.Errorf("fetch: %w", &rss.InvalidURLError{URL: "ftp://x", Msg: "unsupported feed URL scheme"}), classInvalidURL},
		{"BareXMLSyntaxErrorIsNotParseError", &xml.SyntaxError{Msg: "x"}, classOther},
		{reasonBlockedAddress, doError(&rss.BlockedAddressError{Addr: netip.MustParseAddr("10.0.0.1")}, true), classBlockedAddress},
		{"TLSCertificateVerification", doError(&tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, false), classTLSError},
		{"TLSUnknownAuthority", doError(x509.UnknownAuthorityError{}, false), classTLSError},
		{"TLSHostname", doError(x509.HostnameError{Host: "feed.example"}, false), classTLSError},
		{"TLSCertificateInvalid", doError(x509.CertificateInvalidError{Reason: x509.Expired}, false), classTLSError},
		{reasonOther, errors.New("boom"), classOther},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyFetchError(tc.err); got != tc.want {
				t.Fatalf("classifyFetchError(%v) = %+v, want %+v", tc.err, got, tc.want)
			}
		})
	}
}

func TestClassifySendError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want failureClass
	}{
		{reasonRateLimited, &discord.RateLimitError{RetryAfter: time.Second}, classRateLimited},
		{reasonWebhookInvalid, &discord.HTTPStatusError{StatusCode: 404, Status: http404Status}, classWebhookInvalid},
		{reasonHTTP410, &discord.HTTPStatusError{StatusCode: 410, Status: "410 Gone"}, classGone},
		{reasonServerError, &discord.HTTPStatusError{StatusCode: 500, Status: "500 Internal Server Error"}, classServerError},
		{reasonTimeout, fmt.Errorf("send: %w", timeoutError{}), classTimeout},
		{"TLSCertificateVerification", doError(&tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, false), classTLSError},
		{"TLSHostname", doError(x509.HostnameError{Host: "discord.com"}, false), classTLSError},
		{reasonOther, errors.New("boom"), classOther},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifySendError(tc.err); got != tc.want {
				t.Fatalf("classifySendError(%v) = %+v, want %+v", tc.err, got, tc.want)
			}
		})
	}
}

// TestDominantErrorReason_Deterministic asserts ties are broken by sort
// order rather than Go's randomized map iteration, since
// setFeedReachableCondition's output (and thus whether settleGroup
// thinks the status changed) must be stable across reconciles given the same
// input.
func TestDominantErrorReason_Deterministic(t *testing.T) {
	reasons := map[string]string{
		"https://a.example/feed": reasonHTTP404,
		"https://b.example/feed": reasonTimeout,
	}

	for range 20 {
		if got := dominantErrorReason(reasons); got != reasonHTTP404 {
			t.Fatalf("dominantErrorReason() = %q, want %q (alphabetically first on a tie)", got, reasonHTTP404)
		}
	}
}

func TestDominantErrorReason_MostFrequentWins(t *testing.T) {
	reasons := map[string]string{
		"https://a.example/feed": reasonTimeout,
		"https://b.example/feed": reasonTimeout,
		"https://c.example/feed": reasonHTTP404,
	}

	if got := dominantErrorReason(reasons); got != reasonTimeout {
		t.Fatalf("dominantErrorReason() = %q, want %q", got, reasonTimeout)
	}
}

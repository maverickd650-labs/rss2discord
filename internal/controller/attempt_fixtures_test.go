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
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/netip"
	"time"

	"k8s.io/client-go/tools/events"

	"github.com/maverickd650-labs/rss2discord/internal/discord"
	"github.com/maverickd650-labs/rss2discord/internal/rss"
)

// Shared fixtures of the feedAttempt tests: the clock and names every attempt
// runs under, the typed errors the production classifiers recognise, and the
// Event recorder helpers.

// attemptInstant is the time every attempt in these tests runs at.
var attemptInstant = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

const (
	attemptNS         = "attempt-ns"
	attemptURL        = "https://example.test/attempt.xml"
	attemptGeneration = 3
	attemptOld        = "old"
	testBoom          = "boom"

	// attemptInterval is the RetryInterval the backoff rows use:
	// backoff = 5m * 2^RetryCount, capped at 6h from RetryCount 7.
	attemptInterval = "5m"

	// The outcome labels more than one row expects, spelled out here rather
	// than derived so a rename shows up as a failure.
	labelFetchErrorNotFound    = "fetch_error_not_found"
	labelFetchErrorServerError = "fetch_error_server_error"
	labelRenderError           = "render_error"
	labelSendErrorTimeout      = "send_error_timeout"

	// The reasons the Events name the failing step by.
	eventFetchFailed  = "FetchFailed"
	eventSendFailed   = "SendFailed"
	eventRenderFailed = "RenderFailed"
)

// eventNamed is the fragments an Event naming reason must contain.
func eventNamed(reason string, fragments []string) []string {
	return append([]string{reason}, fragments...)
}

// causedError is an error whose text is fixed and whose cause is a real error
// of the kind the production code reports, so a row's text stays readable while
// record still classifies it from the typed cause.
type causedError struct {
	msg   string
	cause error
}

func (e causedError) Error() string { return e.msg }
func (e causedError) Unwrap() error { return e.cause }

// fetchFailure is the fetch error classifyFetchError maps to class.
func fetchFailure(class failureClass) error {
	causes := map[failureClass]error{
		classServerError:    &rss.HTTPStatusError{StatusCode: 503},
		classNotFound:       &rss.HTTPStatusError{StatusCode: 404},
		classGone:           &rss.HTTPStatusError{StatusCode: 410},
		classTimeout:        timeoutError{},
		classDNSFailure:     &net.DNSError{Err: "no such host", Name: "example.invalid"},
		classParseError:     &rss.ParseError{Err: io.EOF},
		classTLSError:       doError(x509.UnknownAuthorityError{}, false),
		classBlockedAddress: doError(&rss.BlockedAddressError{Addr: netip.MustParseAddr("10.0.0.1")}, true),
	}
	cause, ok := causes[class]
	if !ok {
		panic(fmt.Sprintf("no fetch error for class %+v", class))
	}
	return causedError{testBoom, cause}
}

// sendFailure is the send error classifySendError maps to class.
func sendFailure(class failureClass) error {
	causes := map[failureClass]error{
		classServerError:    &discord.HTTPStatusError{StatusCode: 503},
		classWebhookInvalid: &discord.HTTPStatusError{StatusCode: 404},
		classClientError:    &discord.HTTPStatusError{StatusCode: 400},
		classTimeout:        timeoutError{},
	}
	cause, ok := causes[class]
	if !ok {
		panic(fmt.Sprintf("no send error for class %+v", class))
	}
	return causedError{testBoom, cause}
}

// drainEvents returns the Events a FakeRecorder holds, oldest first.
func drainEvents(recorder *events.FakeRecorder) []string {
	var got []string
	for {
		select {
		case ev := <-recorder.Events:
			got = append(got, ev)
		default:
			return got
		}
	}
}

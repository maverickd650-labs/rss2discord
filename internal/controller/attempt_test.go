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
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"

	"github.com/maverickd650-labs/rss2discord/api/v1alpha1"
	"github.com/maverickd650-labs/rss2discord/internal/discord"
	"github.com/maverickd650-labs/rss2discord/internal/rss"
)

// attemptInstant is the time every attempt in these tests runs at.
var attemptInstant = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

const (
	attemptNS         = "attempt-ns"
	attemptURL        = "https://example.test/attempt.xml"
	attemptNow        = "2026-03-04T05:06:07Z" // attemptInstant, formatted
	attemptGeneration = 3
	attemptOld        = "old"
	testBoom          = "boom"

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
	var cause error
	switch class {
	case classServerError:
		cause = &rss.HTTPStatusError{StatusCode: 503}
	case classNotFound:
		cause = &rss.HTTPStatusError{StatusCode: 404}
	case classGone:
		cause = &rss.HTTPStatusError{StatusCode: 410}
	case classTimeout:
		cause = timeoutError{}
	case classDNSFailure:
		cause = &net.DNSError{Err: "no such host", Name: "example.invalid"}
	case classParseError:
		cause = &rss.ParseError{Err: io.EOF}
	case classTLSError:
		cause = doError(x509.UnknownAuthorityError{}, false)
	case classBlockedAddress:
		cause = doError(&rss.BlockedAddressError{Addr: netip.MustParseAddr("10.0.0.1")}, true)
	default:
		panic(fmt.Sprintf("no fetch error for class %+v", class))
	}
	return causedError{testBoom, cause}
}

// sendFailure is the send error classifySendError maps to class.
func sendFailure(class failureClass) error {
	var cause error
	switch class {
	case classServerError:
		cause = &discord.HTTPStatusError{StatusCode: 503}
	case classWebhookInvalid:
		cause = &discord.HTTPStatusError{StatusCode: 404}
	case classClientError:
		cause = &discord.HTTPStatusError{StatusCode: 400}
	case classTimeout:
		cause = timeoutError{}
	default:
		panic(fmt.Sprintf("no send error for class %+v", class))
	}
	return causedError{testBoom, cause}
}

// attemptCase is one row of the outcome matrix: the status and spec an outcome
// is recorded against, and everything record must do about it.
type attemptCase struct {
	name    string
	outcome attemptOutcome
	err     error

	// Before: the spec's retry limit and RetryInterval, and the failure state
	// the status already carries.
	retries       int32
	retryInterval string
	retryCount    int32
	failing       bool // LastError and BackoffUntil are set too

	want attemptWant
}

type attemptWant struct {
	retryCount int32
	// lastError is the exact LastError afterwards.
	lastError string
	// backoff is how far out BackoffUntil is, or 0 for none.
	backoff time.Duration
	// checked says LastChecked is advanced to the reconcile's time.
	checked bool
	// condition is the condition the outcome sets, or "" for none.
	condition string
	status    metav1.ConditionStatus
	reason    string
	message   string // a fragment of the condition message
	// label is the single outcome label that is incremented, or "" for none.
	label string
	// warnings are fragments the one Warning Event must contain, if any.
	warnings []string
	verdict  attemptVerdict
}

// attemptCases is the whole matrix of feedAttempt.record: every outcome kind,
// permanent and transient classes, and the RetryCount boundaries around the
// retry limit and the backoff cap.
func attemptCases() []attemptCase {
	boom := errors.New(testBoom)
	rateLimited := &discord.RateLimitError{RetryAfter: 7 * time.Second}
	const interval = "5m" // backoff = 5m * 2^RetryCount, capped at 6h from RetryCount 7
	retry := attemptVerdict{retry: true}
	pending := attemptVerdict{pending: true}
	retryPending := attemptVerdict{retry: true, pending: true}
	fetchFailed := attemptOutcome{kind: attemptFetchFailed}
	sendFailed := attemptOutcome{kind: attemptSendFailed}
	renderFailed := attemptOutcome{kind: attemptRenderFailed}
	exhausted := []string{"giving up after exhausting retries"}
	capped := []string{"backoff now capped at 6h0m0s"}

	return []attemptCase{
		{
			name: "fetch succeeded advances LastChecked and leaves a failure alone", outcome: attemptOutcome{kind: attemptFetchSucceeded},
			retries: 3, retryCount: 2, failing: true,
			want: attemptWant{
				retryCount: 2, lastError: attemptOld, backoff: time.Hour, checked: true,
				condition: v1alpha1.FeedConditionTypeReachable, status: metav1.ConditionTrue, reason: "FetchSucceeded",
			},
		},

		// A transient fetch failure retries until RetryCount reaches the limit,
		// announces it once at the limit, and stays quiet past it.
		{
			name: "transient fetch failure retries", outcome: fetchFailed, err: fetchFailure(classServerError), retries: 3,
			want: attemptWant{
				retryCount: 1, lastError: testBoom, label: labelFetchErrorServerError, verdict: retry,
				condition: v1alpha1.FeedConditionTypeReachable, status: metav1.ConditionFalse, reason: reasonServerError, message: testBoom,
			},
		},
		{
			name: "transient fetch failure below the limit is quiet", outcome: fetchFailed, err: fetchFailure(classTimeout), retries: 3, retryCount: 1,
			want: attemptWant{
				retryCount: 2, lastError: testBoom, label: "fetch_error_timeout", verdict: retry,
				condition: v1alpha1.FeedConditionTypeReachable, status: metav1.ConditionFalse, reason: reasonTimeout, message: testBoom,
			},
		},
		{
			name: "transient fetch failure announces exhaustion once, at the limit", outcome: fetchFailed, err: fetchFailure(classServerError),
			retries: 3, retryCount: 2,
			want: attemptWant{
				retryCount: 3, lastError: testBoom, label: labelFetchErrorServerError, warnings: eventNamed(eventFetchFailed, exhausted),
				condition: v1alpha1.FeedConditionTypeReachable, status: metav1.ConditionFalse, reason: reasonServerError, message: testBoom,
			},
		},
		{
			name: "transient fetch failure past the limit is quiet again", outcome: fetchFailed, err: fetchFailure(classServerError),
			retries: 3, retryCount: 3,
			want: attemptWant{
				retryCount: 4, lastError: testBoom, label: labelFetchErrorServerError,
				condition: v1alpha1.FeedConditionTypeReachable, status: metav1.ConditionFalse, reason: reasonServerError, message: testBoom,
			},
		},
		{
			name: "a retry limit below one counts as one", outcome: fetchFailed, err: fetchFailure(classDNSFailure), retries: 0,
			want: attemptWant{
				retryCount: 1, lastError: testBoom, label: "fetch_error_dns_failure", warnings: eventNamed(eventFetchFailed, exhausted),
				condition: v1alpha1.FeedConditionTypeReachable, status: metav1.ConditionFalse, reason: reasonDNSFailure, message: testBoom,
			},
		},

		// A permanent failure backs off immediately, never asks for a retry, and
		// announces the backoff only the first time it reaches its cap.
		{
			name: "permanent fetch failure backs off at once", outcome: fetchFailed, err: fetchFailure(classNotFound),
			retries: 3, retryInterval: interval,
			want: attemptWant{
				retryCount: 1, lastError: testBoom, backoff: 10 * time.Minute, label: labelFetchErrorNotFound,
				condition: v1alpha1.FeedConditionTypeReachable, status: metav1.ConditionFalse, reason: reasonHTTP404, message: testBoom,
			},
		},
		{
			name: "permanent fetch failure ignores the retry limit", outcome: fetchFailed, err: fetchFailure(classGone),
			retries: 1, retryInterval: interval, retryCount: 1,
			want: attemptWant{
				retryCount: 2, lastError: testBoom, backoff: 20 * time.Minute, label: "fetch_error_gone",
				condition: v1alpha1.FeedConditionTypeReachable, status: metav1.ConditionFalse, reason: reasonHTTP410, message: testBoom,
			},
		},
		{
			name: "permanent fetch failure just below the cap is quiet", outcome: fetchFailed, err: fetchFailure(classParseError),
			retries: 3, retryInterval: interval, retryCount: 5,
			want: attemptWant{
				retryCount: 6, lastError: testBoom, backoff: 320 * time.Minute, label: "fetch_error_parse_error",
				condition: v1alpha1.FeedConditionTypeReachable, status: metav1.ConditionFalse, reason: reasonParseError, message: testBoom,
			},
		},
		{
			name: "permanent fetch failure announces the cap the first time it is reached", outcome: fetchFailed, err: fetchFailure(classNotFound),
			retries: 3, retryInterval: interval, retryCount: 6,
			want: attemptWant{
				retryCount: 7, lastError: testBoom, backoff: maxPermanentBackoff, label: labelFetchErrorNotFound,
				warnings:  eventNamed(eventFetchFailed, capped),
				condition: v1alpha1.FeedConditionTypeReachable, status: metav1.ConditionFalse, reason: reasonHTTP404, message: testBoom,
			},
		},
		{
			name: "permanent fetch failure at the cap stays quiet", outcome: fetchFailed, err: fetchFailure(classNotFound),
			retries: 3, retryInterval: interval, retryCount: 7,
			want: attemptWant{
				retryCount: 8, lastError: testBoom, backoff: maxPermanentBackoff, label: labelFetchErrorNotFound,
				condition: v1alpha1.FeedConditionTypeReachable, status: metav1.ConditionFalse, reason: reasonHTTP404, message: testBoom,
			},
		},
		{
			name: "permanent failure with an enormous RetryCount is capped, not wrapped", outcome: fetchFailed, err: fetchFailure(classTLSError),
			retries: 3, retryInterval: interval, retryCount: 100,
			want: attemptWant{
				retryCount: 101, lastError: testBoom, backoff: maxPermanentBackoff, label: "fetch_error_tls_error",
				condition: v1alpha1.FeedConditionTypeReachable, status: metav1.ConditionFalse, reason: reasonTLSError, message: testBoom,
			},
		},
		{
			name: "an unset RetryInterval backs off from the 5m fallback", outcome: fetchFailed, err: fetchFailure(classBlockedAddress), retries: 3,
			want: attemptWant{
				retryCount: 1, lastError: testBoom, backoff: 10 * time.Minute, label: "fetch_error_blocked_address",
				condition: v1alpha1.FeedConditionTypeReachable, status: metav1.ConditionFalse, reason: reasonBlockedAddress, message: testBoom,
			},
		},

		// A send failure follows the same policy on the Delivered condition, and
		// leaves the entry pending whatever the policy decides.
		{
			name: "transient send failure retries and stays pending", outcome: sendFailed, err: sendFailure(classServerError), retries: 3,
			want: attemptWant{
				retryCount: 1, lastError: testBoom, label: "send_error_server_error", verdict: retryPending,
				condition: v1alpha1.FeedConditionTypeDelivered, status: metav1.ConditionFalse, reason: reasonServerError, message: testBoom,
			},
		},
		{
			name: "transient send failure announces exhaustion once, at the limit", outcome: sendFailed, err: sendFailure(classTimeout),
			retries: 3, retryCount: 2,
			want: attemptWant{
				retryCount: 3, lastError: testBoom, label: labelSendErrorTimeout, verdict: pending, warnings: eventNamed(eventSendFailed, exhausted),
				condition: v1alpha1.FeedConditionTypeDelivered, status: metav1.ConditionFalse, reason: reasonTimeout, message: testBoom,
			},
		},
		{
			name: "transient send failure past the limit is quiet and still pending", outcome: sendFailed, err: sendFailure(classTimeout),
			retries: 3, retryCount: 3,
			want: attemptWant{
				retryCount: 4, lastError: testBoom, label: labelSendErrorTimeout, verdict: pending,
				condition: v1alpha1.FeedConditionTypeDelivered, status: metav1.ConditionFalse, reason: reasonTimeout, message: testBoom,
			},
		},
		{
			name: "permanent send failure backs off at once without asking for a retry", outcome: sendFailed, err: sendFailure(classWebhookInvalid),
			retries: 3, retryInterval: interval,
			want: attemptWant{
				retryCount: 1, lastError: testBoom, backoff: 10 * time.Minute, label: "send_error_webhook_invalid", verdict: pending,
				condition: v1alpha1.FeedConditionTypeDelivered, status: metav1.ConditionFalse, reason: reasonWebhookInvalid, message: testBoom,
			},
		},
		{
			name: "permanent send failure announces the cap the first time it is reached", outcome: sendFailed, err: sendFailure(classClientError),
			retries: 3, retryInterval: interval, retryCount: 6,
			want: attemptWant{
				retryCount: 7, lastError: testBoom, backoff: maxPermanentBackoff, label: "send_error_client_error", verdict: pending,
				warnings:  eventNamed(eventSendFailed, capped),
				condition: v1alpha1.FeedConditionTypeDelivered, status: metav1.ConditionFalse, reason: reasonClientError, message: testBoom,
			},
		},

		// A render failure is deterministic: transient policy whatever the class
		// says, a metric of its own, and always pending.
		{
			name: "render failure retries", outcome: renderFailed, err: boom, retries: 3,
			want: attemptWant{
				retryCount: 1, lastError: testBoom, label: labelRenderError, verdict: retryPending,
				condition: v1alpha1.FeedConditionTypeDelivered, status: metav1.ConditionFalse, reason: reasonRenderError, message: testBoom,
			},
		},
		{
			name: "render failure never backs off, whatever error it carries", outcome: renderFailed, err: fetchFailure(classNotFound), retries: 3,
			want: attemptWant{
				retryCount: 1, lastError: testBoom, label: labelRenderError, verdict: retryPending,
				condition: v1alpha1.FeedConditionTypeDelivered, status: metav1.ConditionFalse, reason: reasonRenderError, message: testBoom,
			},
		},
		{
			name: "render failure announces exhaustion once, at the limit", outcome: renderFailed, err: boom, retries: 3, retryCount: 2,
			want: attemptWant{
				retryCount: 3, lastError: testBoom, label: labelRenderError, verdict: pending, warnings: eventNamed(eventRenderFailed, exhausted),
				condition: v1alpha1.FeedConditionTypeDelivered, status: metav1.ConditionFalse, reason: reasonRenderError, message: testBoom,
			},
		},
		{
			name: "render failure past the limit is quiet and still pending", outcome: renderFailed, err: boom, retries: 3, retryCount: 3,
			want: attemptWant{
				retryCount: 4, lastError: testBoom, label: labelRenderError, verdict: pending,
				condition: v1alpha1.FeedConditionTypeDelivered, status: metav1.ConditionFalse, reason: reasonRenderError, message: testBoom,
			},
		},

		// A rate limit spends no retry, waits as long as Discord said, and leaves
		// the entry pending.
		{
			name: "rate limit waits and spends no retry", outcome: sendFailed, err: rateLimited,
			retries: 3, retryCount: 1,
			want: attemptWant{
				retryCount: 1, lastError: rateLimited.Error(), label: "rate_limited",
				verdict:   attemptVerdict{retry: true, retryAfter: 7 * time.Second, pending: true},
				condition: v1alpha1.FeedConditionTypeDelivered, status: metav1.ConditionFalse, reason: reasonRateLimited, message: "retry after 7s",
			},
		},

		// Successes and skips.
		{
			name: "sent clears the failure", outcome: attemptOutcome{kind: attemptSent}, retries: 3, retryCount: 2, failing: true,
			want: attemptWant{
				label: "sent", condition: v1alpha1.FeedConditionTypeDelivered, status: metav1.ConditionTrue, reason: "MessageSent",
			},
		},
		{
			name: "a skipped entry clears the failure and warns", outcome: attemptOutcome{kind: attemptEntrySkipped, entry: "Poison title"}, err: boom,
			retries: 3, retryCount: 4, failing: true,
			want: attemptWant{
				label: "skipped", warnings: []string{reasonEntrySkipped, "Poison title", "exhausting retries"},
				condition: v1alpha1.FeedConditionTypeDelivered, status: metav1.ConditionFalse, reason: reasonEntrySkipped,
				message: `skipped entry "Poison title" after RenderFailed: boom`,
			},
		},
		{
			name: "a skipped backlog counts nothing and leaves a failure alone", outcome: attemptOutcome{kind: attemptBacklogSkipped, count: 4},
			retries: 3, retryCount: 2, failing: true,
			want: attemptWant{
				retryCount: 2, lastError: attemptOld, backoff: time.Hour,
				condition: v1alpha1.FeedConditionTypeDelivered, status: metav1.ConditionTrue, reason: reasonBacklogSkipped,
				message: "catchUpLimit is 0: skipped 4 existing entries",
			},
		},
		{
			name: "a config error is not counted or retried, and warns", outcome: attemptOutcome{kind: attemptConfigError}, err: boom, retries: 3,
			want: attemptWant{
				lastError: testBoom, warnings: []string{reasonConfigError, testBoom},
				condition: v1alpha1.FeedConditionTypeDelivered, status: metav1.ConditionFalse, reason: reasonConfigError, message: testBoom,
			},
		},
	}
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

func TestFeedAttempt_Record(t *testing.T) {
	for i, tc := range attemptCases() {
		t.Run(tc.name, func(t *testing.T) {
			// The attempt runs at attemptInstant whatever the wall clock says,
			// so BackoffUntil is checked to the second, not within a window.
			checkAttemptCase(t, fmt.Sprintf("attempt-fg-%d", i), tc)
		})
	}
}

// checkAttemptCase records tc's outcome on a fresh FeedGroup named name and
// checks every effect TestFeedAttempt_Record covers.
func checkAttemptCase(t *testing.T, name string, tc attemptCase) {
	t.Helper()
	forgetMetricsAfter(t, attemptNS, name)
	fg := &v1alpha1.FeedGroup{}
	fg.Namespace, fg.Name, fg.Generation = attemptNS, name, attemptGeneration
	fg.Spec.Retries, fg.Spec.RetryInterval = tc.retries, tc.retryInterval
	fs := &v1alpha1.FeedStatus{RSSUrl: attemptURL, RetryCount: tc.retryCount}
	if tc.failing {
		fs.LastError, fs.BackoffUntil = attemptOld, nextBackoffUntil(attemptInstant, time.Hour)
	}
	recorder := events.NewFakeRecorder(10)
	attempt := testAttempt(t, fg, fs, attemptURL, attemptInstant).withRecorder(recorder)

	verdict := attempt.record(tc.outcome, tc.err)

	if verdict != tc.want.verdict {
		t.Errorf("verdict = %+v, want %+v", verdict, tc.want.verdict)
	}
	if fs.RetryCount != tc.want.retryCount {
		t.Errorf("RetryCount = %d, want %d", fs.RetryCount, tc.want.retryCount)
	}
	if fs.LastError != tc.want.lastError {
		t.Errorf("LastError = %q, want %q", fs.LastError, tc.want.lastError)
	}
	assertBackoffUntil(t, fs.BackoffUntil, attemptInstant, tc.want.backoff)
	if checked := fs.LastChecked == attemptNow; checked != tc.want.checked || (!checked && fs.LastChecked != "") {
		t.Errorf("LastChecked = %q, want advanced=%t", fs.LastChecked, tc.want.checked)
	}
	assertAttemptCondition(t, fs, tc.want)
	assertOutcomeMetric(t, name, tc.want.label)
	assertAttemptEvents(t, drainEvents(recorder), tc.want.warnings)
}

// assertBackoffUntil checks that backoffUntil is exactly want after now, the
// (fake, unmoving) time record ran at, or unset when want is 0.
func assertBackoffUntil(t *testing.T, backoffUntil string, now time.Time, want time.Duration) {
	t.Helper()
	wantUntil := ""
	if want != 0 {
		wantUntil = nextBackoffUntil(now, want)
	}
	if backoffUntil != wantUntil {
		t.Errorf("BackoffUntil = %q, want %q (%v after %s)", backoffUntil, wantUntil, want, now.Format(time.RFC3339))
	}
}

func assertAttemptCondition(t *testing.T, fs *v1alpha1.FeedStatus, want attemptWant) {
	t.Helper()
	if want.condition == "" {
		if len(fs.Conditions) != 0 {
			t.Errorf("unexpected conditions %+v", fs.Conditions)
		}
		return
	}
	if len(fs.Conditions) != 1 {
		t.Errorf("got %d conditions, want only %s: %+v", len(fs.Conditions), want.condition, fs.Conditions)
	}
	cond := apimeta.FindStatusCondition(fs.Conditions, want.condition)
	if cond == nil {
		t.Errorf("no %s condition in %+v", want.condition, fs.Conditions)
		return
	}
	if cond.Status != want.status || cond.Reason != want.reason || cond.ObservedGeneration != attemptGeneration ||
		!strings.Contains(cond.Message, want.message) {
		t.Errorf("%s condition = %+v, want status %s reason %s message containing %q at generation %d",
			want.condition, cond, want.status, want.reason, want.message, attemptGeneration)
	}
}

// assertOutcomeMetric checks that exactly one outcome series of the group was
// incremented, once, and it is the wanted one -- or that none was.
func assertOutcomeMetric(t *testing.T, group, wantLabel string) {
	t.Helper()
	total := 0.0
	for _, label := range expectedOutcomes() {
		count := testutil.ToFloat64(feedOperationsTotal.WithLabelValues(attemptNS, group, attemptURL, label))
		total += count
		if want := label == wantLabel; (count == 1) != want {
			t.Errorf("outcome %q counted %v times, want counted=%t", label, count, want)
		}
	}
	wantTotal := 0.0
	if wantLabel != "" {
		wantTotal = 1
	}
	if total != wantTotal {
		t.Errorf("outcomes counted %v times in all, want %v", total, wantTotal)
	}
}

// assertAttemptEvents checks that record emitted at most one Event, a Warning,
// and that it carries every wanted fragment; no fragments means no Event.
func assertAttemptEvents(t *testing.T, got, want []string) {
	t.Helper()
	if len(want) == 0 {
		if len(got) != 0 {
			t.Errorf("unexpected Events %q", got)
		}
		return
	}
	if len(got) != 1 {
		t.Errorf("got %d Events %q, want exactly one containing %q", len(got), got, want)
		return
	}
	if !strings.HasPrefix(got[0], "Warning ") {
		t.Errorf("Event %q is not a Warning", got[0])
	}
	for _, fragment := range want {
		if !strings.Contains(got[0], fragment) {
			t.Errorf("Event %q lacks %q", got[0], fragment)
		}
	}
}

// A nil Recorder (unit tests that build the reconciler directly) must never
// panic, whichever outcome would have warned.
func TestFeedAttempt_NilRecorderIsSafe(t *testing.T) {
	for i, tc := range attemptCases() {
		t.Run(tc.name, func(t *testing.T) {
			name := fmt.Sprintf("attempt-nil-%d", i)
			forgetMetricsAfter(t, attemptNS, name)
			fg := &v1alpha1.FeedGroup{}
			fg.Namespace, fg.Name = attemptNS, name
			fg.Spec.Retries, fg.Spec.RetryInterval = tc.retries, tc.retryInterval
			fs := &v1alpha1.FeedStatus{RSSUrl: attemptURL, RetryCount: tc.retryCount}
			testAttempt(t, fg, fs, attemptURL, attemptInstant).record(tc.outcome, tc.err)
		})
	}
}

// The ConfigError Event is gated on the condition changing, since a config
// error carries no RetryCount to gate it on: it fires when the error first
// appears or its message changes, and not while it recurs.
func TestFeedAttempt_ConfigErrorWarnsOncePerDistinctError(t *testing.T) {
	name := "attempt-config-once"
	forgetMetricsAfter(t, attemptNS, name)
	fg := &v1alpha1.FeedGroup{}
	fg.Namespace, fg.Name = attemptNS, name
	fs := &v1alpha1.FeedStatus{RSSUrl: attemptURL}
	recorder := events.NewFakeRecorder(10)
	attempt := testAttempt(t, fg, fs, attemptURL, attemptInstant).withRecorder(recorder)
	configError := attemptOutcome{kind: attemptConfigError}

	for _, step := range []struct {
		err        string
		wantEvents int
	}{
		{"bad regex", 1}, {"bad regex", 0}, {"a different typo", 1}, {"a different typo", 0},
	} {
		attempt.record(configError, errors.New(step.err))
		if got := drainEvents(recorder); len(got) != step.wantEvents {
			t.Fatalf("config error %q: got Events %q, want %d", step.err, got, step.wantEvents)
		}
	}
}

func TestFeedAttempt_PastRetryLimit(t *testing.T) {
	for _, tc := range []struct {
		retries    int32
		retryCount int32
		want       bool
	}{
		{retries: 3, retryCount: 2}, {retries: 3, retryCount: 3}, {retries: 3, retryCount: 4, want: true},
		{retries: 0, retryCount: 1}, {retries: 0, retryCount: 2, want: true},
	} {
		fg := &v1alpha1.FeedGroup{}
		fg.Spec.Retries = tc.retries
		fs := &v1alpha1.FeedStatus{RetryCount: tc.retryCount}
		if got := testAttempt(t, fg, fs, attemptURL, attemptInstant).pastRetryLimit(); got != tc.want {
			t.Errorf("retries %d, RetryCount %d: pastRetryLimit = %t, want %t", tc.retries, tc.retryCount, got, tc.want)
		}
	}
}

func TestAttemptVerdict_FoldAndSettlement(t *testing.T) {
	retry := attemptVerdict{retry: true}
	wait := attemptVerdict{retry: true, retryAfter: 9 * time.Second, pending: true}
	shorter := attemptVerdict{retryAfter: 3 * time.Second}
	pending := attemptVerdict{pending: true}

	for _, tc := range []struct {
		name string
		a, b attemptVerdict
		want attemptVerdict
	}{
		{name: "zero folds to zero", want: attemptVerdict{}},
		{name: "a retry survives", a: retry, want: retry},
		{name: "the longest wait wins", a: wait, b: shorter, want: wait},
		{name: "the longest wait wins either way round", a: shorter, b: wait, want: wait},
		{name: "pending and retry combine", a: pending, b: retry, want: attemptVerdict{retry: true, pending: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.a.fold(tc.b); got != tc.want {
				t.Fatalf("fold = %+v, want %+v", got, tc.want)
			}
		})
	}

	if got := (attemptVerdict{retry: true, retryAfter: time.Second}).settlement(); got != syncAccountedFor {
		t.Errorf("settlement of a verdict with nothing pending = %v, want accountedFor", got)
	}
	if got := pending.settlement(); got != syncPending {
		t.Errorf("settlement of a pending verdict = %v, want pending", got)
	}
}

// Every kind either counts under exactly one outcome label the dashboard and
// alerts know about (see TestObservabilityContract) or under none; a kind
// added without being handled by record panics instead of going unrecorded.
func TestAttemptOutcome_MetricLabels(t *testing.T) {
	uncounted := map[attemptKind]bool{attemptFetchSucceeded: true, attemptBacklogSkipped: true, attemptConfigError: true}
	known := expectedOutcomes()
	for kind := attemptFetchSucceeded; kind < attemptKindEnd; kind++ {
		for _, class := range allFailureClasses {
			label := attemptOutcome{kind: kind}.metricLabel(class)
			switch {
			case uncounted[kind] && label != "":
				t.Fatalf("kind %d counts under %q, want none", kind, label)
			case !uncounted[kind] && !slices.Contains(known, label):
				t.Fatalf("kind %d counts under %q, which expectedOutcomes does not know", kind, label)
			}
		}
	}

	defer func() {
		if recover() == nil {
			t.Fatal("recording an unknown kind did not panic")
		}
	}()
	fg := &v1alpha1.FeedGroup{}
	testAttempt(t, fg, &v1alpha1.FeedStatus{}, attemptURL, attemptInstant).
		record(attemptOutcome{kind: attemptKindEnd}, nil)
}

func TestAttemptOutcomeResolve(t *testing.T) {
	rateLimited := &discord.RateLimitError{RetryAfter: time.Second}
	for _, tc := range []struct {
		name      string
		err       error
		wantKind  attemptKind
		wantClass failureClass
	}{
		{name: "rate limit", err: rateLimited, wantKind: attemptRateLimited, wantClass: classRateLimited},
		{name: "wrapped rate limit", err: fmt.Errorf("sending: %w", rateLimited), wantKind: attemptRateLimited, wantClass: classRateLimited},
		{name: "a 429 status is a counted failure", err: &discord.HTTPStatusError{StatusCode: 429}, wantKind: attemptSendFailed, wantClass: classRateLimited},
		{name: "deleted webhook", err: &discord.HTTPStatusError{StatusCode: 404}, wantKind: attemptSendFailed, wantClass: classWebhookInvalid},
		{name: "server error", err: &discord.HTTPStatusError{StatusCode: 502}, wantKind: attemptSendFailed, wantClass: classServerError},
		{name: "anything else", err: errors.New(testBoom), wantKind: attemptSendFailed, wantClass: classOther},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outcome, class := attemptOutcome{kind: attemptSendFailed}.resolve(tc.err)
			if outcome.kind != tc.wantKind || class != tc.wantClass {
				t.Fatalf("resolve = (%d, %+v), want (%d, %+v)", outcome.kind, class, tc.wantKind, tc.wantClass)
			}
		})
	}
}

// TestFeedAttempt_BackoffCountsFromWhenTheFailureIsRecorded: the backoff
// deadline counts from when the failure is recorded (the run's clock), not from
// when the reconcile started (LastChecked's time), so a slow fetch or a long
// rate-limit wait before the failure cannot eat into the backoff.
func TestFeedAttempt_BackoffCountsFromWhenTheFailureIsRecorded(t *testing.T) {
	fg := &v1alpha1.FeedGroup{}
	fg.Namespace, fg.Name, fg.Generation = attemptNS, "attempt-fg-late-clock", attemptGeneration
	fg.Spec.Retries, fg.Spec.RetryInterval = 3, "5m"
	fs := &v1alpha1.FeedStatus{RSSUrl: attemptURL}
	// Recorded two hours after the reconcile started: longer than the backoff.
	recorded := attemptInstant.Add(2 * time.Hour)
	attempt := testRun(t, fg, v1alpha1.FeedSpec{RSSUrl: attemptURL}, rss.FetchResult{}, nil, nil).
		withNow(attemptInstant).withClock(func() time.Time { return recorded }).newAttempt(fs)

	attempt.record(attemptOutcome{kind: attemptFetchFailed}, fetchFailure(classNotFound))

	assertBackoffUntil(t, fs.BackoffUntil, recorded, 10*time.Minute)
}

// TestFeedAttempt_FetchSuccessEndsOnlyAFetchPhaseRun asserts what a successful
// fetch does to the retry run in progress: a fetch-phase run (Reachable False)
// ends, with RetryCount and the backoff reset but LastError kept; a delivery
// run (Reachable True) and a feed never fetched are left alone.
func TestFeedAttempt_FetchSuccessEndsOnlyAFetchPhaseRun(t *testing.T) {
	const backoffUntil = "2026-01-01T01:00:00Z"
	reachable := func(status metav1.ConditionStatus) []metav1.Condition {
		return []metav1.Condition{{Type: v1alpha1.FeedConditionTypeReachable, Status: status}}
	}
	for _, tc := range []struct {
		name        string
		conditions  []metav1.Condition
		retryCount  int32
		backoff     string
		wantRetries int32
		wantBackoff string
	}{
		{"fetch run with retries", reachable(metav1.ConditionFalse), 5, "", 0, ""},
		{"fetch run in backoff", reachable(metav1.ConditionFalse), 2, backoffUntil, 0, ""},
		{"fetch run with only a backoff", reachable(metav1.ConditionFalse), 0, backoffUntil, 0, ""},
		{"delivery run", reachable(metav1.ConditionTrue), 2, "", 2, ""},
		{"delivery run in backoff", reachable(metav1.ConditionTrue), 2, backoffUntil, 2, backoffUntil},
		{"never fetched", nil, 2, "", 2, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &v1alpha1.FeedStatus{
				RSSUrl: attemptURL, Conditions: tc.conditions, RetryCount: tc.retryCount,
				BackoffUntil: tc.backoff, LastError: "earlier failure",
			}
			testAttempt(t, &v1alpha1.FeedGroup{}, fs, attemptURL, attemptInstant).
				record(attemptOutcome{kind: attemptFetchSucceeded}, nil)

			if fs.RetryCount != tc.wantRetries || fs.BackoffUntil != tc.wantBackoff {
				t.Errorf("RetryCount=%d BackoffUntil=%q, want %d and %q", fs.RetryCount, fs.BackoffUntil, tc.wantRetries, tc.wantBackoff)
			}
			if fs.LastError != "earlier failure" {
				t.Errorf("LastError = %q, want it kept", fs.LastError)
			}
			if !apimeta.IsStatusConditionTrue(fs.Conditions, v1alpha1.FeedConditionTypeReachable) {
				t.Error("Reachable should be True after a successful fetch")
			}
		})
	}
}

// TestFeedAttempt_FetchFailureEndsADeliveryRun asserts the reverse transition:
// a fetch failure while Reachable is True ends the delivery run in progress, so
// the fetch run starts at 1 (transient: still retried; permanent: backed off
// from 1), whereas a fetch failure inside a fetch run counts on and a feed never
// fetched keeps its count.
func TestFeedAttempt_FetchFailureEndsADeliveryRun(t *testing.T) {
	const staleBackoff = "2026-01-01T01:00:00Z"
	reachable := func(status metav1.ConditionStatus) []metav1.Condition {
		return []metav1.Condition{{Type: v1alpha1.FeedConditionTypeReachable, Status: status}}
	}
	for _, tc := range []struct {
		name        string
		conditions  []metav1.Condition
		retryCount  int32
		backoff     string
		class       failureClass
		wantRetries int32
		wantRetry   bool
	}{
		{"delivery run, transient fetch failure", reachable(metav1.ConditionTrue), 2, "", classServerError, 1, true},
		{"delivery run in backoff, transient fetch failure", reachable(metav1.ConditionTrue), 2, staleBackoff, classServerError, 1, true},
		{"delivery run, permanent fetch failure", reachable(metav1.ConditionTrue), 4, "", classNotFound, 1, false},
		{"fetch run counts on", reachable(metav1.ConditionFalse), 2, "", classServerError, 3, false},
		{"never fetched", nil, 1, "", classServerError, 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &v1alpha1.FeedStatus{
				RSSUrl: attemptURL, Conditions: tc.conditions, RetryCount: tc.retryCount, BackoffUntil: tc.backoff,
			}
			attempt := testAttempt(t, &v1alpha1.FeedGroup{Spec: v1alpha1.FeedGroupSpec{Retries: 3}}, fs, attemptURL, attemptInstant)
			verdict := attempt.record(attemptOutcome{kind: attemptFetchFailed}, fetchFailure(tc.class))

			if fs.RetryCount != tc.wantRetries {
				t.Errorf("RetryCount = %d, want %d", fs.RetryCount, tc.wantRetries)
			}
			if verdict.retry != tc.wantRetry {
				t.Errorf("verdict.retry = %v, want %v", verdict.retry, tc.wantRetry)
			}
			if tc.class == classNotFound {
				want := nextBackoffUntil(attempt.recordedAt().UTC(), permanentBackoffDuration(1, attempt.cadence.retry))
				if fs.BackoffUntil != want {
					t.Errorf("BackoffUntil = %q, want %q (backed off from 1)", fs.BackoffUntil, want)
				}
			} else if fs.BackoffUntil == staleBackoff {
				t.Error("the delivery run's backoff should be cleared")
			}
		})
	}
}

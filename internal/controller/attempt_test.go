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
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"

	"github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/discord"
)

const (
	attemptNS         = "attempt-ns"
	attemptURL        = "https://example.test/attempt.xml"
	attemptNow        = "2026-03-04T05:06:07Z"
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

// attemptCase is one row of the outcome matrix: the status and spec an outcome
// is recorded against, and everything record must do about it.
type attemptCase struct {
	name    string
	outcome attemptOutcome
	class   failureClass
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
			name: "transient fetch failure retries", outcome: fetchFailed, class: classServerError, err: boom, retries: 3,
			want: attemptWant{
				retryCount: 1, lastError: testBoom, label: labelFetchErrorServerError, verdict: retry,
				condition: v1alpha1.FeedConditionTypeReachable, status: metav1.ConditionFalse, reason: reasonServerError, message: testBoom,
			},
		},
		{
			name: "transient fetch failure below the limit is quiet", outcome: fetchFailed, class: classTimeout, err: boom, retries: 3, retryCount: 1,
			want: attemptWant{
				retryCount: 2, lastError: testBoom, label: "fetch_error_timeout", verdict: retry,
				condition: v1alpha1.FeedConditionTypeReachable, status: metav1.ConditionFalse, reason: reasonTimeout, message: testBoom,
			},
		},
		{
			name: "transient fetch failure announces exhaustion once, at the limit", outcome: fetchFailed, class: classServerError, err: boom,
			retries: 3, retryCount: 2,
			want: attemptWant{
				retryCount: 3, lastError: testBoom, label: labelFetchErrorServerError, warnings: eventNamed(eventFetchFailed, exhausted),
				condition: v1alpha1.FeedConditionTypeReachable, status: metav1.ConditionFalse, reason: reasonServerError, message: testBoom,
			},
		},
		{
			name: "transient fetch failure past the limit is quiet again", outcome: fetchFailed, class: classServerError, err: boom,
			retries: 3, retryCount: 3,
			want: attemptWant{
				retryCount: 4, lastError: testBoom, label: labelFetchErrorServerError,
				condition: v1alpha1.FeedConditionTypeReachable, status: metav1.ConditionFalse, reason: reasonServerError, message: testBoom,
			},
		},
		{
			name: "a retry limit below one counts as one", outcome: fetchFailed, class: classDNSFailure, err: boom, retries: 0,
			want: attemptWant{
				retryCount: 1, lastError: testBoom, label: "fetch_error_dns_failure", warnings: eventNamed(eventFetchFailed, exhausted),
				condition: v1alpha1.FeedConditionTypeReachable, status: metav1.ConditionFalse, reason: reasonDNSFailure, message: testBoom,
			},
		},

		// A permanent failure backs off immediately, never asks for a retry, and
		// announces the backoff only the first time it reaches its cap.
		{
			name: "permanent fetch failure backs off at once", outcome: fetchFailed, class: classNotFound, err: boom,
			retries: 3, retryInterval: interval,
			want: attemptWant{
				retryCount: 1, lastError: testBoom, backoff: 10 * time.Minute, label: labelFetchErrorNotFound,
				condition: v1alpha1.FeedConditionTypeReachable, status: metav1.ConditionFalse, reason: reasonHTTP404, message: testBoom,
			},
		},
		{
			name: "permanent fetch failure ignores the retry limit", outcome: fetchFailed, class: classGone, err: boom,
			retries: 1, retryInterval: interval, retryCount: 1,
			want: attemptWant{
				retryCount: 2, lastError: testBoom, backoff: 20 * time.Minute, label: "fetch_error_gone",
				condition: v1alpha1.FeedConditionTypeReachable, status: metav1.ConditionFalse, reason: reasonHTTP410, message: testBoom,
			},
		},
		{
			name: "permanent fetch failure just below the cap is quiet", outcome: fetchFailed, class: classParseError, err: boom,
			retries: 3, retryInterval: interval, retryCount: 5,
			want: attemptWant{
				retryCount: 6, lastError: testBoom, backoff: 320 * time.Minute, label: "fetch_error_parse_error",
				condition: v1alpha1.FeedConditionTypeReachable, status: metav1.ConditionFalse, reason: reasonParseError, message: testBoom,
			},
		},
		{
			name: "permanent fetch failure announces the cap the first time it is reached", outcome: fetchFailed, class: classNotFound, err: boom,
			retries: 3, retryInterval: interval, retryCount: 6,
			want: attemptWant{
				retryCount: 7, lastError: testBoom, backoff: maxPermanentBackoff, label: labelFetchErrorNotFound,
				warnings:  eventNamed(eventFetchFailed, capped),
				condition: v1alpha1.FeedConditionTypeReachable, status: metav1.ConditionFalse, reason: reasonHTTP404, message: testBoom,
			},
		},
		{
			name: "permanent fetch failure at the cap stays quiet", outcome: fetchFailed, class: classNotFound, err: boom,
			retries: 3, retryInterval: interval, retryCount: 7,
			want: attemptWant{
				retryCount: 8, lastError: testBoom, backoff: maxPermanentBackoff, label: labelFetchErrorNotFound,
				condition: v1alpha1.FeedConditionTypeReachable, status: metav1.ConditionFalse, reason: reasonHTTP404, message: testBoom,
			},
		},
		{
			name: "permanent failure with an enormous RetryCount is capped, not wrapped", outcome: fetchFailed, class: classTLSError, err: boom,
			retries: 3, retryInterval: interval, retryCount: 100,
			want: attemptWant{
				retryCount: 101, lastError: testBoom, backoff: maxPermanentBackoff, label: "fetch_error_tls_error",
				condition: v1alpha1.FeedConditionTypeReachable, status: metav1.ConditionFalse, reason: reasonTLSError, message: testBoom,
			},
		},
		{
			name: "an unset RetryInterval backs off from the 5m fallback", outcome: fetchFailed, class: classBlockedAddress, err: boom, retries: 3,
			want: attemptWant{
				retryCount: 1, lastError: testBoom, backoff: 10 * time.Minute, label: "fetch_error_blocked_address",
				condition: v1alpha1.FeedConditionTypeReachable, status: metav1.ConditionFalse, reason: reasonBlockedAddress, message: testBoom,
			},
		},

		// A send failure follows the same policy on the Delivered condition, and
		// leaves the entry pending whatever the policy decides.
		{
			name: "transient send failure retries and stays pending", outcome: sendFailed, class: classServerError, err: boom, retries: 3,
			want: attemptWant{
				retryCount: 1, lastError: testBoom, label: "send_error_server_error", verdict: retryPending,
				condition: v1alpha1.FeedConditionTypeDelivered, status: metav1.ConditionFalse, reason: reasonServerError, message: testBoom,
			},
		},
		{
			name: "transient send failure announces exhaustion once, at the limit", outcome: sendFailed, class: classTimeout, err: boom,
			retries: 3, retryCount: 2,
			want: attemptWant{
				retryCount: 3, lastError: testBoom, label: labelSendErrorTimeout, verdict: pending, warnings: eventNamed(eventSendFailed, exhausted),
				condition: v1alpha1.FeedConditionTypeDelivered, status: metav1.ConditionFalse, reason: reasonTimeout, message: testBoom,
			},
		},
		{
			name: "transient send failure past the limit is quiet and still pending", outcome: sendFailed, class: classTimeout, err: boom,
			retries: 3, retryCount: 3,
			want: attemptWant{
				retryCount: 4, lastError: testBoom, label: labelSendErrorTimeout, verdict: pending,
				condition: v1alpha1.FeedConditionTypeDelivered, status: metav1.ConditionFalse, reason: reasonTimeout, message: testBoom,
			},
		},
		{
			name: "permanent send failure backs off at once without asking for a retry", outcome: sendFailed, class: classWebhookInvalid, err: boom,
			retries: 3, retryInterval: interval,
			want: attemptWant{
				retryCount: 1, lastError: testBoom, backoff: 10 * time.Minute, label: "send_error_webhook_invalid", verdict: pending,
				condition: v1alpha1.FeedConditionTypeDelivered, status: metav1.ConditionFalse, reason: reasonWebhookInvalid, message: testBoom,
			},
		},
		{
			name: "permanent send failure announces the cap the first time it is reached", outcome: sendFailed, class: classClientError, err: boom,
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
			name: "render failure never backs off, whatever class it is handed", outcome: renderFailed, class: classNotFound, err: boom, retries: 3,
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
			name: "rate limit waits and spends no retry", outcome: attemptOutcome{kind: attemptRateLimited}, class: classRateLimited, err: rateLimited,
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
			name := fmt.Sprintf("attempt-fg-%d", i)
			defer deleteFeedGroupMetrics(attemptNS, name)
			fg := &v1alpha1.FeedGroup{}
			fg.Namespace, fg.Name, fg.Generation = attemptNS, name, attemptGeneration
			fg.Spec.Retries, fg.Spec.RetryInterval = tc.retries, tc.retryInterval
			fs := &v1alpha1.FeedStatus{RSSUrl: attemptURL, RetryCount: tc.retryCount}
			if tc.failing {
				fs.LastError, fs.BackoffUntil = attemptOld, nextBackoffUntil(time.Now().UTC(), time.Hour)
			}
			recorder := events.NewFakeRecorder(10)
			attempt := (&FeedGroupReconciler{Recorder: recorder}).newFeedAttempt(fg, fs, attemptURL, attemptNow)

			before := time.Now().UTC()
			verdict := attempt.record(tc.outcome, tc.class, tc.err)

			if verdict != tc.want.verdict {
				t.Errorf("verdict = %+v, want %+v", verdict, tc.want.verdict)
			}
			if fs.RetryCount != tc.want.retryCount {
				t.Errorf("RetryCount = %d, want %d", fs.RetryCount, tc.want.retryCount)
			}
			if fs.LastError != tc.want.lastError {
				t.Errorf("LastError = %q, want %q", fs.LastError, tc.want.lastError)
			}
			assertBackoffUntil(t, fs.BackoffUntil, before, tc.want.backoff)
			if checked := fs.LastChecked == attemptNow; checked != tc.want.checked || (!checked && fs.LastChecked != "") {
				t.Errorf("LastChecked = %q, want advanced=%t", fs.LastChecked, tc.want.checked)
			}
			assertAttemptCondition(t, fs, tc.want)
			assertOutcomeMetric(t, name, tc.want.label)
			assertAttemptEvents(t, drainEvents(recorder), tc.want.warnings)
		})
	}
}

func assertBackoffUntil(t *testing.T, backoffUntil string, from time.Time, want time.Duration) {
	t.Helper()
	if want == 0 {
		if backoffUntil != "" {
			t.Errorf("BackoffUntil = %q, want none", backoffUntil)
		}
		return
	}
	until, err := time.Parse(time.RFC3339, backoffUntil)
	if err != nil {
		t.Errorf("BackoffUntil %q is not RFC3339: %v", backoffUntil, err)
		return
	}
	if got := until.Sub(from); got < want-3*time.Second || got > want+time.Minute {
		t.Errorf("BackoffUntil is %v out, want about %v", got, want)
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
			defer deleteFeedGroupMetrics(attemptNS, name)
			fg := &v1alpha1.FeedGroup{}
			fg.Namespace, fg.Name = attemptNS, name
			fg.Spec.Retries, fg.Spec.RetryInterval = tc.retries, tc.retryInterval
			fs := &v1alpha1.FeedStatus{RSSUrl: attemptURL, RetryCount: tc.retryCount}
			(&FeedGroupReconciler{}).newFeedAttempt(fg, fs, attemptURL, attemptNow).record(tc.outcome, tc.class, tc.err)
		})
	}
}

// The ConfigError Event is gated on the condition changing, since a config
// error carries no RetryCount to gate it on: it fires when the error first
// appears or its message changes, and not while it recurs.
func TestFeedAttempt_ConfigErrorWarnsOncePerDistinctError(t *testing.T) {
	name := "attempt-config-once"
	defer deleteFeedGroupMetrics(attemptNS, name)
	fg := &v1alpha1.FeedGroup{}
	fg.Namespace, fg.Name = attemptNS, name
	fs := &v1alpha1.FeedStatus{RSSUrl: attemptURL}
	recorder := events.NewFakeRecorder(10)
	attempt := (&FeedGroupReconciler{Recorder: recorder}).newFeedAttempt(fg, fs, attemptURL, attemptNow)
	configError := attemptOutcome{kind: attemptConfigError}

	for _, step := range []struct {
		err        string
		wantEvents int
	}{
		{"bad regex", 1}, {"bad regex", 0}, {"a different typo", 1}, {"a different typo", 0},
	} {
		attempt.record(configError, noClass, errors.New(step.err))
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
		if got := (&FeedGroupReconciler{}).newFeedAttempt(fg, fs, attemptURL, attemptNow).pastRetryLimit(); got != tc.want {
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
	(&FeedGroupReconciler{}).newFeedAttempt(fg, &v1alpha1.FeedStatus{}, attemptURL, attemptNow).
		record(attemptOutcome{kind: attemptKindEnd}, noClass, nil)
}

func TestSendFailureOutcome(t *testing.T) {
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
			outcome, class := sendFailureOutcome(tc.err)
			if outcome.kind != tc.wantKind || class != tc.wantClass {
				t.Fatalf("sendFailureOutcome = (%d, %+v), want (%d, %+v)", outcome.kind, class, tc.wantKind, tc.wantClass)
			}
		})
	}
}

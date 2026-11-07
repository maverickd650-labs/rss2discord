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
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/maverickd650-labs/rss2discord/api/v1beta1"
	"github.com/maverickd650-labs/rss2discord/internal/discord"
)

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

// Verdicts and Event fragments the rows share.
var (
	verdictRetry        = attemptVerdict{retry: true}
	verdictPending      = attemptVerdict{pending: true}
	verdictRetryPending = attemptVerdict{retry: true, pending: true}
	exhaustedFragments  = []string{"giving up after exhausting retries"}
	cappedFragments     = []string{"backoff now capped at 6h0m0s"}
)

// attemptCases is the whole matrix of feedAttempt.record: every outcome kind,
// permanent and transient classes, and the RetryCount boundaries around the
// retry limit and the backoff cap.
func attemptCases() []attemptCase {
	return slices.Concat(
		transientFetchCases(),
		permanentFetchCases(),
		sendFailureCases(),
		renderFailureCases(),
		successAndSkipCases(),
	)
}

func transientFetchCases() []attemptCase {
	retry, exhausted := verdictRetry, exhaustedFragments
	fetchFailed := attemptOutcome{kind: attemptFetchFailed}

	return []attemptCase{
		// A transient fetch failure retries until RetryCount reaches the limit,
		// announces it once at the limit, and stays quiet past it.
		{
			name: "transient fetch failure retries", outcome: fetchFailed, err: fetchFailure(classServerError), retries: 3,
			want: attemptWant{
				retryCount: 1, lastError: testBoom, label: labelFetchErrorServerError, verdict: retry,
				condition: v1beta1.FeedConditionTypeReachable, status: metav1.ConditionFalse, reason: reasonServerError, message: testBoom,
			},
		},
		{
			name: "transient fetch failure below the limit is quiet", outcome: fetchFailed, err: fetchFailure(classTimeout), retries: 3, retryCount: 1,
			want: attemptWant{
				retryCount: 2, lastError: testBoom, label: "fetch_error_timeout", verdict: retry,
				condition: v1beta1.FeedConditionTypeReachable, status: metav1.ConditionFalse, reason: reasonTimeout, message: testBoom,
			},
		},
		{
			name: "transient fetch failure announces exhaustion once, at the limit", outcome: fetchFailed, err: fetchFailure(classServerError),
			retries: 3, retryCount: 2,
			want: attemptWant{
				retryCount: 3, lastError: testBoom, label: labelFetchErrorServerError, warnings: eventNamed(eventFetchFailed, exhausted),
				condition: v1beta1.FeedConditionTypeReachable, status: metav1.ConditionFalse, reason: reasonServerError, message: testBoom,
			},
		},
		{
			name: "transient fetch failure past the limit is quiet again", outcome: fetchFailed, err: fetchFailure(classServerError),
			retries: 3, retryCount: 3,
			want: attemptWant{
				retryCount: 4, lastError: testBoom, label: labelFetchErrorServerError,
				condition: v1beta1.FeedConditionTypeReachable, status: metav1.ConditionFalse, reason: reasonServerError, message: testBoom,
			},
		},
		{
			name: "a retry limit below one counts as one", outcome: fetchFailed, err: fetchFailure(classDNSFailure), retries: 0,
			want: attemptWant{
				retryCount: 1, lastError: testBoom, label: "fetch_error_dns_failure", warnings: eventNamed(eventFetchFailed, exhausted),
				condition: v1beta1.FeedConditionTypeReachable, status: metav1.ConditionFalse, reason: reasonDNSFailure, message: testBoom,
			},
		},
	}
}

func permanentFetchCases() []attemptCase {
	const interval = attemptInterval
	capped := cappedFragments
	fetchFailed := attemptOutcome{kind: attemptFetchFailed}

	return []attemptCase{
		// A permanent failure backs off immediately, never asks for a retry, and
		// announces the backoff only the first time it reaches its cap.
		{
			name: "permanent fetch failure backs off at once", outcome: fetchFailed, err: fetchFailure(classNotFound),
			retries: 3, retryInterval: interval,
			want: attemptWant{
				retryCount: 1, lastError: testBoom, backoff: 10 * time.Minute, label: labelFetchErrorNotFound,
				condition: v1beta1.FeedConditionTypeReachable, status: metav1.ConditionFalse, reason: reasonHTTP404, message: testBoom,
			},
		},
		{
			name: "permanent fetch failure ignores the retry limit", outcome: fetchFailed, err: fetchFailure(classGone),
			retries: 1, retryInterval: interval, retryCount: 1,
			want: attemptWant{
				retryCount: 2, lastError: testBoom, backoff: 20 * time.Minute, label: "fetch_error_gone",
				condition: v1beta1.FeedConditionTypeReachable, status: metav1.ConditionFalse, reason: reasonHTTP410, message: testBoom,
			},
		},
		{
			name: "permanent fetch failure just below the cap is quiet", outcome: fetchFailed, err: fetchFailure(classParseError),
			retries: 3, retryInterval: interval, retryCount: 5,
			want: attemptWant{
				retryCount: 6, lastError: testBoom, backoff: 320 * time.Minute, label: "fetch_error_parse_error",
				condition: v1beta1.FeedConditionTypeReachable, status: metav1.ConditionFalse, reason: reasonParseError, message: testBoom,
			},
		},
		{
			name: "permanent fetch failure announces the cap the first time it is reached", outcome: fetchFailed, err: fetchFailure(classNotFound),
			retries: 3, retryInterval: interval, retryCount: 6,
			want: attemptWant{
				retryCount: 7, lastError: testBoom, backoff: maxPermanentBackoff, label: labelFetchErrorNotFound,
				warnings:  eventNamed(eventFetchFailed, capped),
				condition: v1beta1.FeedConditionTypeReachable, status: metav1.ConditionFalse, reason: reasonHTTP404, message: testBoom,
			},
		},
		{
			name: "permanent fetch failure at the cap stays quiet", outcome: fetchFailed, err: fetchFailure(classNotFound),
			retries: 3, retryInterval: interval, retryCount: 7,
			want: attemptWant{
				retryCount: 8, lastError: testBoom, backoff: maxPermanentBackoff, label: labelFetchErrorNotFound,
				condition: v1beta1.FeedConditionTypeReachable, status: metav1.ConditionFalse, reason: reasonHTTP404, message: testBoom,
			},
		},
		{
			name: "permanent failure with an enormous RetryCount is capped, not wrapped", outcome: fetchFailed, err: fetchFailure(classTLSError),
			retries: 3, retryInterval: interval, retryCount: 100,
			want: attemptWant{
				retryCount: 101, lastError: testBoom, backoff: maxPermanentBackoff, label: "fetch_error_tls_error",
				condition: v1beta1.FeedConditionTypeReachable, status: metav1.ConditionFalse, reason: reasonTLSError, message: testBoom,
			},
		},
		{
			name: "an unset RetryInterval backs off from the 5m fallback", outcome: fetchFailed, err: fetchFailure(classBlockedAddress), retries: 3,
			want: attemptWant{
				retryCount: 1, lastError: testBoom, backoff: 10 * time.Minute, label: "fetch_error_blocked_address",
				condition: v1beta1.FeedConditionTypeReachable, status: metav1.ConditionFalse, reason: reasonBlockedAddress, message: testBoom,
			},
		},
	}
}

func sendFailureCases() []attemptCase {
	const interval = attemptInterval
	retryPending, pending := verdictRetryPending, verdictPending
	exhausted, capped := exhaustedFragments, cappedFragments
	sendFailed := attemptOutcome{kind: attemptSendFailed}

	return []attemptCase{
		// A send failure follows the same policy on the Delivered condition, and
		// leaves the entry pending whatever the policy decides.
		{
			name: "transient send failure retries and stays pending", outcome: sendFailed, err: sendFailure(classServerError), retries: 3,
			want: attemptWant{
				retryCount: 1, lastError: testBoom, label: "send_error_server_error", verdict: retryPending,
				condition: v1beta1.FeedConditionTypeDelivered, status: metav1.ConditionFalse, reason: reasonServerError, message: testBoom,
			},
		},
		{
			name: "transient send failure announces exhaustion once, at the limit", outcome: sendFailed, err: sendFailure(classTimeout),
			retries: 3, retryCount: 2,
			want: attemptWant{
				retryCount: 3, lastError: testBoom, label: labelSendErrorTimeout, verdict: pending, warnings: eventNamed(eventSendFailed, exhausted),
				condition: v1beta1.FeedConditionTypeDelivered, status: metav1.ConditionFalse, reason: reasonTimeout, message: testBoom,
			},
		},
		{
			name: "transient send failure past the limit is quiet and still pending", outcome: sendFailed, err: sendFailure(classTimeout),
			retries: 3, retryCount: 3,
			want: attemptWant{
				retryCount: 4, lastError: testBoom, label: labelSendErrorTimeout, verdict: pending,
				condition: v1beta1.FeedConditionTypeDelivered, status: metav1.ConditionFalse, reason: reasonTimeout, message: testBoom,
			},
		},
		{
			name: "permanent send failure backs off at once without asking for a retry", outcome: sendFailed, err: sendFailure(classWebhookInvalid),
			retries: 3, retryInterval: interval,
			want: attemptWant{
				retryCount: 1, lastError: testBoom, backoff: 10 * time.Minute, label: "send_error_webhook_invalid", verdict: pending,
				condition: v1beta1.FeedConditionTypeDelivered, status: metav1.ConditionFalse, reason: reasonWebhookInvalid, message: testBoom,
			},
		},
		{
			name: "permanent send failure announces the cap the first time it is reached", outcome: sendFailed, err: sendFailure(classClientError),
			retries: 3, retryInterval: interval, retryCount: 6,
			want: attemptWant{
				retryCount: 7, lastError: testBoom, backoff: maxPermanentBackoff, label: "send_error_client_error", verdict: pending,
				warnings:  eventNamed(eventSendFailed, capped),
				condition: v1beta1.FeedConditionTypeDelivered, status: metav1.ConditionFalse, reason: reasonClientError, message: testBoom,
			},
		},
	}
}

func renderFailureCases() []attemptCase {
	boom := errors.New(testBoom)
	retryPending, pending := verdictRetryPending, verdictPending
	exhausted := exhaustedFragments
	renderFailed := attemptOutcome{kind: attemptRenderFailed}

	return []attemptCase{
		// A render failure is deterministic: transient policy whatever the class
		// says, a metric of its own, and always pending.
		{
			name: "render failure retries", outcome: renderFailed, err: boom, retries: 3,
			want: attemptWant{
				retryCount: 1, lastError: testBoom, label: labelRenderError, verdict: retryPending,
				condition: v1beta1.FeedConditionTypeDelivered, status: metav1.ConditionFalse, reason: reasonRenderError, message: testBoom,
			},
		},
		{
			name: "render failure never backs off, whatever error it carries", outcome: renderFailed, err: fetchFailure(classNotFound), retries: 3,
			want: attemptWant{
				retryCount: 1, lastError: testBoom, label: labelRenderError, verdict: retryPending,
				condition: v1beta1.FeedConditionTypeDelivered, status: metav1.ConditionFalse, reason: reasonRenderError, message: testBoom,
			},
		},
		{
			name: "render failure announces exhaustion once, at the limit", outcome: renderFailed, err: boom, retries: 3, retryCount: 2,
			want: attemptWant{
				retryCount: 3, lastError: testBoom, label: labelRenderError, verdict: pending, warnings: eventNamed(eventRenderFailed, exhausted),
				condition: v1beta1.FeedConditionTypeDelivered, status: metav1.ConditionFalse, reason: reasonRenderError, message: testBoom,
			},
		},
		{
			name: "render failure past the limit is quiet and still pending", outcome: renderFailed, err: boom, retries: 3, retryCount: 3,
			want: attemptWant{
				retryCount: 4, lastError: testBoom, label: labelRenderError, verdict: pending,
				condition: v1beta1.FeedConditionTypeDelivered, status: metav1.ConditionFalse, reason: reasonRenderError, message: testBoom,
			},
		},
	}
}

// successAndSkipCases are the outcomes that clear or leave a failure alone, and
// the rate limit and config error that are neither counted nor retried.
func successAndSkipCases() []attemptCase {
	boom := errors.New(testBoom)
	rateLimited := &discord.RateLimitError{RetryAfter: 7 * time.Second}
	sendFailed := attemptOutcome{kind: attemptSendFailed}

	return []attemptCase{
		{
			name: "fetch succeeded advances LastChecked and leaves a failure alone", outcome: attemptOutcome{kind: attemptFetchSucceeded},
			retries: 3, retryCount: 2, failing: true,
			want: attemptWant{
				retryCount: 2, lastError: attemptOld, backoff: time.Hour, checked: true,
				condition: v1beta1.FeedConditionTypeReachable, status: metav1.ConditionTrue, reason: "FetchSucceeded",
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
				condition: v1beta1.FeedConditionTypeDelivered, status: metav1.ConditionFalse, reason: reasonRateLimited, message: "retry after 7s",
			},
		},

		// Successes and skips.
		{
			name: "sent clears the failure", outcome: attemptOutcome{kind: attemptSent}, retries: 3, retryCount: 2, failing: true,
			want: attemptWant{
				label: "sent", condition: v1beta1.FeedConditionTypeDelivered, status: metav1.ConditionTrue, reason: "MessageSent",
			},
		},
		{
			name: "a skipped entry clears the failure and warns", outcome: attemptOutcome{kind: attemptEntrySkipped, entry: "Poison title"}, err: boom,
			retries: 3, retryCount: 4, failing: true,
			want: attemptWant{
				label: "skipped", warnings: []string{reasonEntrySkipped, "Poison title", "exhausting retries"},
				condition: v1beta1.FeedConditionTypeDelivered, status: metav1.ConditionFalse, reason: reasonEntrySkipped,
				message: `skipped entry "Poison title" after RenderFailed: boom`,
			},
		},
		{
			name: "a skipped backlog counts nothing and leaves a failure alone", outcome: attemptOutcome{kind: attemptBacklogSkipped, count: 4},
			retries: 3, retryCount: 2, failing: true,
			want: attemptWant{
				retryCount: 2, lastError: attemptOld, backoff: time.Hour,
				condition: v1beta1.FeedConditionTypeDelivered, status: metav1.ConditionTrue, reason: reasonBacklogSkipped,
				message: "catchUpLimit is 0: skipped 4 existing entries",
			},
		},
		{
			name: "a config error is not counted or retried, and warns", outcome: attemptOutcome{kind: attemptConfigError}, err: boom, retries: 3,
			want: attemptWant{
				lastError: testBoom, warnings: []string{reasonConfigError, testBoom},
				condition: v1beta1.FeedConditionTypeDelivered, status: metav1.ConditionFalse, reason: reasonConfigError, message: testBoom,
			},
		},
	}
}

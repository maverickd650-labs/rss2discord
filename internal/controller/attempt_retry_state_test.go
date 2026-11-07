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
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/maverickd650-labs/rss2discord/api/v1beta1"
	"github.com/maverickd650-labs/rss2discord/internal/rss"
)

// Tests of the retry state feedAttempt.record drives: the retry limit, where
// the backoff counts from, and how a run of one phase ends in the other.

func TestFeedAttempt_PastRetryLimit(t *testing.T) {
	for _, tc := range []struct {
		retries    int32
		retryCount int32
		want       bool
	}{
		{retries: 3, retryCount: 2}, {retries: 3, retryCount: 3}, {retries: 3, retryCount: 4, want: true},
		{retries: 0, retryCount: 1}, {retries: 0, retryCount: 2, want: true},
	} {
		fg := &v1beta1.FeedGroup{}
		fg.Spec.Retries = new(tc.retries)
		fs := &v1beta1.FeedStatus{RetryCount: tc.retryCount}
		if got := testAttempt(t, fg, fs, attemptURL, attemptInstant).pastRetryLimit(); got != tc.want {
			t.Errorf("retries %d, RetryCount %d: pastRetryLimit = %t, want %t", tc.retries, tc.retryCount, got, tc.want)
		}
	}
}

// TestFeedAttempt_BackoffCountsFromWhenTheFailureIsRecorded: the backoff
// deadline counts from when the failure is recorded (the run's clock), not from
// when the reconcile started (LastChecked's time), so a slow fetch or a long
// rate-limit wait before the failure cannot eat into the backoff.
func TestFeedAttempt_BackoffCountsFromWhenTheFailureIsRecorded(t *testing.T) {
	fg := &v1beta1.FeedGroup{}
	fg.Namespace, fg.Name, fg.Generation = attemptNS, "attempt-fg-late-clock", attemptGeneration
	fg.Spec.Retries, fg.Spec.RetryInterval = new(int32(3)), "5m"
	fs := &v1beta1.FeedStatus{Name: testFeedName(attemptURL), URL: attemptURL}
	// Recorded two hours after the reconcile started: longer than the backoff.
	recorded := attemptInstant.Add(2 * time.Hour)
	attempt := testRun(t, fg, v1beta1.FeedSpec{Name: testFeedName(attemptURL), URL: attemptURL}, rss.FetchResult{}, nil, nil).
		withNow(attemptInstant).withClock(func() time.Time { return recorded }).newAttempt(fs)

	attempt.record(attemptOutcome{kind: attemptFetchFailed}, fetchFailure(classNotFound))

	assertBackoffUntil(t, fs.BackoffUntil, recorded, 10*time.Minute)
}

// TestFeedAttempt_FetchSuccessEndsOnlyAFetchPhaseRun asserts what a successful
// fetch does to the retry run in progress: a fetch-phase run (Reachable False)
// ends, with RetryCount and the backoff reset but LastError kept; a delivery
// run (Reachable True) and a feed never fetched are left alone.
func TestFeedAttempt_FetchSuccessEndsOnlyAFetchPhaseRun(t *testing.T) {
	backoffTime := metav1.NewTime(time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC))
	backoffUntil := &backoffTime
	for _, tc := range []struct {
		name        string
		conditions  []metav1.Condition
		retryCount  int32
		backoff     *metav1.Time
		wantRetries int32
		wantBackoff *metav1.Time
	}{
		{"fetch run with retries", reachableConditions(metav1.ConditionFalse), 5, nil, 0, nil},
		{"fetch run in backoff", reachableConditions(metav1.ConditionFalse), 2, backoffUntil, 0, nil},
		{"fetch run with only a backoff", reachableConditions(metav1.ConditionFalse), 0, backoffUntil, 0, nil},
		{"delivery run", reachableConditions(metav1.ConditionTrue), 2, nil, 2, nil},
		{"delivery run in backoff", reachableConditions(metav1.ConditionTrue), 2, backoffUntil, 2, backoffUntil},
		{"never fetched", nil, 2, nil, 2, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &v1beta1.FeedStatus{
				Name: testFeedName(attemptURL), URL: attemptURL, Conditions: tc.conditions, RetryCount: tc.retryCount,
				BackoffUntil: tc.backoff, LastError: "earlier failure",
			}
			testAttempt(t, &v1beta1.FeedGroup{}, fs, attemptURL, attemptInstant).
				record(attemptOutcome{kind: attemptFetchSucceeded}, nil)

			if fs.RetryCount != tc.wantRetries || !fs.BackoffUntil.Equal(tc.wantBackoff) {
				t.Errorf("RetryCount=%d BackoffUntil=%v, want %d and %v", fs.RetryCount, fs.BackoffUntil, tc.wantRetries, tc.wantBackoff)
			}
			if fs.LastError != "earlier failure" {
				t.Errorf("LastError = %q, want it kept", fs.LastError)
			}
			if !apimeta.IsStatusConditionTrue(fs.Conditions, v1beta1.FeedConditionTypeReachable) {
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
	staleTime := metav1.NewTime(time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC))
	staleBackoff := &staleTime
	for _, tc := range []struct {
		name        string
		conditions  []metav1.Condition
		retryCount  int32
		backoff     *metav1.Time
		class       failureClass
		wantRetries int32
		wantRetry   bool
	}{
		{"delivery run, transient fetch failure", reachableConditions(metav1.ConditionTrue), 2, nil, classServerError, 1, true},
		{"delivery run in backoff, transient fetch failure", reachableConditions(metav1.ConditionTrue), 2, staleBackoff, classServerError, 1, true},
		{"delivery run, permanent fetch failure", reachableConditions(metav1.ConditionTrue), 4, nil, classNotFound, 1, false},
		{"fetch run counts on", reachableConditions(metav1.ConditionFalse), 2, nil, classServerError, 3, false},
		{"never fetched", nil, 1, nil, classServerError, 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &v1beta1.FeedStatus{
				Name: testFeedName(attemptURL), URL: attemptURL, Conditions: tc.conditions, RetryCount: tc.retryCount, BackoffUntil: tc.backoff,
			}
			attempt := testAttempt(t, &v1beta1.FeedGroup{Spec: v1beta1.FeedGroupSpec{Retries: new(int32(3))}}, fs, attemptURL, attemptInstant)
			verdict := attempt.record(attemptOutcome{kind: attemptFetchFailed}, fetchFailure(tc.class))

			if fs.RetryCount != tc.wantRetries {
				t.Errorf("RetryCount = %d, want %d", fs.RetryCount, tc.wantRetries)
			}
			if verdict.retry != tc.wantRetry {
				t.Errorf("verdict.retry = %v, want %v", verdict.retry, tc.wantRetry)
			}
			if tc.class == classNotFound {
				want := nextBackoffUntil(attempt.recordedAt().UTC(), permanentBackoffDuration(1, attempt.cadence.retry))
				if !fs.BackoffUntil.Equal(want) {
					t.Errorf("BackoffUntil = %v, want %v (backed off from 1)", fs.BackoffUntil, want)
				}
			} else if fs.BackoffUntil != nil {
				t.Error("the delivery run's backoff should be cleared")
			}
		})
	}
}

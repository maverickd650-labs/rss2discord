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
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"

	"github.com/maverickd650-labs/rss2discord/api/v1beta1"
)

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
	fg := &v1beta1.FeedGroup{}
	fg.Namespace, fg.Name, fg.Generation = attemptNS, name, attemptGeneration
	fg.Spec.Retries, fg.Spec.RetryInterval = new(tc.retries), tc.retryInterval
	fs := &v1beta1.FeedStatus{Name: testFeedName(attemptURL), URL: attemptURL, RetryCount: tc.retryCount}
	if tc.failing {
		fs.LastError, fs.BackoffUntil = attemptOld, nextBackoffUntil(attemptInstant, time.Hour)
	}
	recorder := events.NewFakeRecorder(10)
	attempt := testAttempt(t, fg, fs, attemptURL, attemptInstant).withRecorder(recorder)

	verdict := attempt.record(tc.outcome, tc.err)

	if verdict != tc.want.verdict {
		t.Errorf("verdict = %+v, want %+v", verdict, tc.want.verdict)
	}
	assertFeedStatusAfterRecord(t, fs, tc.want)
	assertOutcomeMetric(t, name, tc.want.label)
	assertAttemptEvents(t, drainEvents(recorder), tc.want.warnings)
}

// assertFeedStatusAfterRecord checks the FeedStatus fields record owns.
func assertFeedStatusAfterRecord(t *testing.T, fs *v1beta1.FeedStatus, want attemptWant) {
	t.Helper()
	if fs.RetryCount != want.retryCount {
		t.Errorf("RetryCount = %d, want %d", fs.RetryCount, want.retryCount)
	}
	if fs.LastError != want.lastError {
		t.Errorf("LastError = %q, want %q", fs.LastError, want.lastError)
	}
	assertBackoffUntil(t, fs.BackoffUntil, attemptInstant, want.backoff)
	assertLastChecked(t, fs.LastChecked, want.checked)
	assertAttemptCondition(t, fs, want)
}

// A nil Recorder (unit tests that build the reconciler directly) must never
// panic, whichever outcome would have warned.
func TestFeedAttempt_NilRecorderIsSafe(t *testing.T) {
	for i, tc := range attemptCases() {
		t.Run(tc.name, func(t *testing.T) {
			name := fmt.Sprintf("attempt-nil-%d", i)
			forgetMetricsAfter(t, attemptNS, name)
			fg := &v1beta1.FeedGroup{}
			fg.Namespace, fg.Name = attemptNS, name
			fg.Spec.Retries, fg.Spec.RetryInterval = new(tc.retries), tc.retryInterval
			fs := &v1beta1.FeedStatus{Name: testFeedName(attemptURL), URL: attemptURL, RetryCount: tc.retryCount}
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
	fg := &v1beta1.FeedGroup{}
	fg.Namespace, fg.Name = attemptNS, name
	fs := &v1beta1.FeedStatus{Name: testFeedName(attemptURL), URL: attemptURL}
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

func TestFeedAttempt_ConfigErrorDoesNotRepeatOnGenerationChange(t *testing.T) {
	const differentConfigError = "different config error"
	fg := &v1beta1.FeedGroup{}
	fg.Namespace, fg.Name, fg.Generation = attemptNS, "attempt-config-generation", attemptGeneration
	fs := &v1beta1.FeedStatus{
		Name: testFeedName(attemptURL), URL: attemptURL,
		Conditions: []metav1.Condition{{
			Type: v1beta1.FeedConditionTypeDelivered, Status: metav1.ConditionFalse,
			Reason: reasonConfigError, Message: testBoom, ObservedGeneration: attemptGeneration - 1,
		}},
	}
	recorder := events.NewFakeRecorder(10)
	attempt := testAttempt(t, fg, fs, attemptURL, attemptInstant).withRecorder(recorder)

	attempt.record(attemptOutcome{kind: attemptConfigError}, errors.New(testBoom))
	if got := drainEvents(recorder); len(got) != 0 {
		t.Fatalf("generation-only change emitted Warning Events: %v", got)
	}
	condition := apimeta.FindStatusCondition(fs.Conditions, v1beta1.FeedConditionTypeDelivered)
	if condition == nil || condition.ObservedGeneration != attemptGeneration {
		t.Fatalf("condition = %+v, want ObservedGeneration %d", condition, attemptGeneration)
	}

	attempt.record(attemptOutcome{kind: attemptConfigError}, errors.New(differentConfigError))
	if got := drainEvents(recorder); len(got) != 1 {
		t.Fatalf("message change emitted %d Warning Events, want 1: %v", len(got), got)
	}
	attempt.record(attemptOutcome{kind: attemptConfigError}, errors.New(differentConfigError))
	if got := drainEvents(recorder); len(got) != 0 {
		t.Fatalf("unchanged config error emitted Warning Events: %v", got)
	}
}

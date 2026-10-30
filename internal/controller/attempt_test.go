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
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"

	"github.com/maverickd650/rss2discord-operator/api/v1alpha1"
)

func newAttemptFixture() (*feedGroupFixture, feedAttempt, *events.FakeRecorder) {
	fg := &v1alpha1.FeedGroup{ObjectMeta: metav1.ObjectMeta{Namespace: "attempt-ns", Name: "attempt-fg", Generation: 3}}
	fs := &v1alpha1.FeedStatus{RSSUrl: "https://example.test/attempt.xml"}
	recorder := events.NewFakeRecorder(10)
	r := &FeedGroupReconciler{Recorder: recorder}
	return &feedGroupFixture{fg: fg, fs: fs}, r.newFeedAttempt(fg, fs, fs.RSSUrl), recorder
}

type feedGroupFixture struct {
	fg *v1alpha1.FeedGroup
	fs *v1alpha1.FeedStatus
}

func TestFeedAttempt_Fail(t *testing.T) {
	fx, a, _ := newAttemptFixture()
	long := errors.New(strings.Repeat("x", maxStatusTextBytes+50))

	a.fail(v1alpha1.FeedConditionTypeReachable, "fetch_error_test", "HTTP404", long)

	if fx.fs.RetryCount != 1 {
		t.Fatalf("RetryCount = %d, want 1", fx.fs.RetryCount)
	}
	if fx.fs.LastError != errText(long) || len(fx.fs.LastError) > maxStatusTextBytes {
		t.Fatalf("LastError should be the clamped errText, got len %d", len(fx.fs.LastError))
	}
	cond := apimeta.FindStatusCondition(fx.fs.Conditions, v1alpha1.FeedConditionTypeReachable)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "HTTP404" || cond.ObservedGeneration != 3 {
		t.Fatalf("unexpected condition %+v", cond)
	}
	if cond.Message != clampText(long.Error(), maxConditionMessageBytes) {
		t.Fatalf("condition message should be err.Error() clamped to the condition limit")
	}
	if got := testutil.ToFloat64(feedOperationsTotal.WithLabelValues("attempt-ns", "attempt-fg", fx.fs.RSSUrl, "fetch_error_test")); got != 1 {
		t.Fatalf("metric = %v, want 1", got)
	}

	a.fail(v1alpha1.FeedConditionTypeReachable, "fetch_error_test", "HTTP404", errors.New("again"))
	if fx.fs.RetryCount != 2 || fx.fs.LastError != "again" {
		t.Fatalf("second failure: RetryCount=%d LastError=%q", fx.fs.RetryCount, fx.fs.LastError)
	}
}

func TestFeedAttempt_ClearFailure(t *testing.T) {
	fx, a, _ := newAttemptFixture()
	fx.fs.LastError, fx.fs.RetryCount, fx.fs.BackoffUntil = "stuck", 4, "2031-02-03T04:05:06Z"
	a.clearFailure()
	if fx.fs.LastError != "" || fx.fs.RetryCount != 0 || fx.fs.BackoffUntil != "" {
		t.Fatalf("not cleared: %+v", fx.fs)
	}
}

func TestFeedAttempt_AnnounceExhaustionFiresOnlyAtEquality(t *testing.T) {
	fx, a, recorder := newAttemptFixture()
	boom := errors.New("kaboom")
	for count, wantEvent := range map[int32]bool{1: false, 2: true, 3: false, 4: false} {
		fx.fs.RetryCount = count
		a.announceExhaustion(2, "FetchFailed", boom)
		select {
		case ev := <-recorder.Events:
			if !wantEvent {
				t.Fatalf("RetryCount %d: unexpected event %q", count, ev)
			}
			if !strings.Contains(ev, "FetchFailed") || !strings.Contains(ev, "giving up after exhausting retries") {
				t.Fatalf("event %q lacks reason/action", ev)
			}
		default:
			if wantEvent {
				t.Fatalf("RetryCount %d: expected an event", count)
			}
		}
	}
}

func TestFeedAttempt_NilRecorderIsSafe(t *testing.T) {
	fx := &v1alpha1.FeedStatus{RetryCount: 1}
	a := (&FeedGroupReconciler{}).newFeedAttempt(&v1alpha1.FeedGroup{}, fx, "u")
	a.announceExhaustion(1, "SendFailed", errors.New("x"))
}

func TestFeedAttempt_BackOffSetsBackoffUntil(t *testing.T) {
	fx, a, _ := newAttemptFixture()
	fx.fs.RetryCount = 1
	a.backOff("FetchFailed", errors.New("gone"))
	if fx.fs.BackoffUntil == "" {
		t.Fatal("BackoffUntil not set")
	}
}

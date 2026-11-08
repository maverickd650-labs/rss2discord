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
	"strings"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1beta1 "github.com/maverickd650-labs/rss2discord/api/v1beta1"
)

const (
	whyFeedErrors   = "FeedErrors"
	whyReconciled   = "Reconciled"
	whyAllReachable = "AllFeedsReachable"
	reasonHTTP500   = "HTTP500"
)

func feedWithReachable(url string, status metav1.ConditionStatus, reason, lastError string) v1beta1.FeedStatus {
	fs := v1beta1.FeedStatus{Name: testFeedName(url), URL: url, LastError: lastError}
	if status != "" {
		fs.Conditions = []metav1.Condition{{
			Type: v1beta1.FeedConditionTypeReachable, Status: status, Reason: reason,
		}}
	}
	return fs
}

// summaryCase is one TestSummarizeGroup scenario: the group's inputs and the
// Ready/FeedReachable conditions and InvalidInterval Event it must produce.
type summaryCase struct {
	name          string
	interval      string
	feeds         []v1beta1.FeedStatus
	webhookErr    error
	priorReady    *metav1.Condition
	wantReady     metav1.ConditionStatus
	wantReadyWhy  string
	wantReachable metav1.ConditionStatus
	wantReachWhy  string
	wantEmit      bool
	wantWarning   bool
}

// group builds the FeedGroup under test and the InvalidInterval message its
// spec produces (empty for a valid interval), which a prior Ready condition
// without a message of its own is given.
func (tc summaryCase) group() (fg *v1beta1.FeedGroup, wantMessage string) {
	fg = &v1beta1.FeedGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "fg", Generation: 3},
		Spec:       v1beta1.FeedGroupSpec{Interval: tc.interval},
		Status:     v1beta1.FeedGroupStatus{Feeds: tc.feeds},
	}
	if err := cadenceOf(&fg.Spec).err; err != nil {
		wantMessage = err.Error()
	}
	if tc.priorReady != nil {
		prior := *tc.priorReady
		if prior.Message == "" {
			prior.Message = wantMessage
		}
		fg.Status.Conditions = []metav1.Condition{prior}
	}
	return fg, wantMessage
}

// checkEmission asserts the InvalidInterval Event decision and its warning.
func (tc summaryCase) checkEmission(t *testing.T, warning string, emit bool, wantMessage string) {
	t.Helper()
	if emit != tc.wantEmit {
		t.Errorf("emit = %v, want %v", emit, tc.wantEmit)
	}
	wantWarning := ""
	if tc.wantWarning {
		wantWarning = wantMessage
	}
	if warning != wantWarning {
		t.Errorf("warning = %q, want %q", warning, wantWarning)
	}
}

// requireCondition asserts the named condition has the wanted status and
// reason, and returns it (nil when absent).
func requireCondition(t *testing.T, fg *v1beta1.FeedGroup, condType string, status metav1.ConditionStatus, reason string) *metav1.Condition {
	t.Helper()
	c := apimeta.FindStatusCondition(fg.Status.Conditions, condType)
	if c == nil || c.Status != status || c.Reason != reason {
		t.Errorf("%s = %+v, want status %s reason %s", condType, c, status, reason)
	}
	return c
}

// checkSummarizeIsIdempotent asserts that summarizing an unchanged status again
// is a no-op: same conditions, and never a repeat Event.
func checkSummarizeIsIdempotent(t *testing.T, fg *v1beta1.FeedGroup, webhookErr error) {
	t.Helper()
	before := fg.Status.DeepCopy()
	warning, emit := summarizeGroup(fg, cadenceOf(&fg.Spec), webhookErr)
	if emit || warning != "" {
		t.Errorf("second summarize emitted %q (emit=%v), want none", warning, emit)
	}
	if !conditionsEqual(before.Conditions, fg.Status.Conditions) {
		t.Errorf("second summarize changed conditions:\nbefore %+v\nafter  %+v", before.Conditions, fg.Status.Conditions)
	}
}

// TestSummarizeGroup covers the pure group summary: which Ready and
// FeedReachable conditions a status produces, which of them wins when several
// problems coexist, and when the InvalidInterval Event fires. No reconciler or
// recorder is involved.
func TestSummarizeGroup(t *testing.T) {
	const badInterval = "9999999999999h"
	webhookErr := errors.New("secret not found")

	tests := []summaryCase{
		{
			name:          "healthy group",
			feeds:         []v1beta1.FeedStatus{feedWithReachable("a", metav1.ConditionTrue, "FetchSucceeded", "")},
			wantReady:     metav1.ConditionTrue,
			wantReadyWhy:  whyReconciled,
			wantReachable: metav1.ConditionTrue,
			wantReachWhy:  whyAllReachable,
		},
		{
			name:          "feed error flips Ready but not FeedReachable when the feed was reachable",
			feeds:         []v1beta1.FeedStatus{feedWithReachable("a", metav1.ConditionTrue, "FetchSucceeded", "send failed")},
			wantReady:     metav1.ConditionFalse,
			wantReadyWhy:  whyFeedErrors,
			wantReachable: metav1.ConditionTrue,
			wantReachWhy:  whyAllReachable,
		},
		{
			name: "unreachable feed reports the dominant reason on both conditions",
			feeds: []v1beta1.FeedStatus{
				feedWithReachable("a", metav1.ConditionFalse, reasonHTTP404, "404"),
				feedWithReachable("b", metav1.ConditionFalse, reasonHTTP404, "404"),
				feedWithReachable("c", metav1.ConditionFalse, "Timeout", "timeout"),
			},
			wantReady:     metav1.ConditionFalse,
			wantReadyWhy:  whyFeedErrors,
			wantReachable: metav1.ConditionFalse,
			wantReachWhy:  reasonHTTP404,
		},
		{
			name:          "unknown or missing Reachable is not a failure",
			feeds:         []v1beta1.FeedStatus{feedWithReachable("a", metav1.ConditionUnknown, "Pending", ""), feedWithReachable("b", "", "", "")},
			wantReady:     metav1.ConditionTrue,
			wantReadyWhy:  whyReconciled,
			wantReachable: metav1.ConditionTrue,
			wantReachWhy:  whyAllReachable,
		},
		{
			name:          "invalid interval outranks feed errors and emits once",
			interval:      badInterval,
			feeds:         []v1beta1.FeedStatus{feedWithReachable("a", metav1.ConditionFalse, reasonHTTP500, "boom")},
			wantReady:     metav1.ConditionFalse,
			wantReadyWhy:  reasonInvalidInterval,
			wantReachable: metav1.ConditionFalse,
			wantReachWhy:  reasonHTTP500,
			wantEmit:      true,
			wantWarning:   true,
		},
		{
			name:          "webhook error outranks invalid interval and emits no Event",
			interval:      badInterval,
			feeds:         []v1beta1.FeedStatus{feedWithReachable("a", metav1.ConditionFalse, reasonHTTP500, "boom")},
			webhookErr:    webhookErr,
			wantReady:     metav1.ConditionFalse,
			wantReadyWhy:  "WebhookUnresolved",
			wantReachable: metav1.ConditionFalse,
			wantReachWhy:  reasonHTTP500,
		},
		{
			name:     "no Event when InvalidInterval is already recorded with the same message",
			interval: badInterval,
			// Message filled in by the test from cadenceOf.
			priorReady:    &metav1.Condition{Type: v1beta1.ConditionTypeReady, Status: metav1.ConditionFalse, Reason: reasonInvalidInterval},
			wantReady:     metav1.ConditionFalse,
			wantReadyWhy:  reasonInvalidInterval,
			wantReachable: metav1.ConditionTrue,
			wantReachWhy:  whyAllReachable,
		},
		{
			name:     "Event fires again when the InvalidInterval message changes",
			interval: badInterval,
			priorReady: &metav1.Condition{
				Type: v1beta1.ConditionTypeReady, Status: metav1.ConditionFalse,
				Reason: reasonInvalidInterval, Message: "spec.retryInterval: something else",
			},
			wantReady:     metav1.ConditionFalse,
			wantReadyWhy:  reasonInvalidInterval,
			wantReachable: metav1.ConditionTrue,
			wantReachWhy:  whyAllReachable,
			wantEmit:      true,
			wantWarning:   true,
		},
		{
			name:     "no Event when the interval is fixed",
			interval: "5m",
			priorReady: &metav1.Condition{
				Type: v1beta1.ConditionTypeReady, Status: metav1.ConditionFalse,
				Reason: reasonInvalidInterval, Message: "old",
			},
			wantReady:     metav1.ConditionTrue,
			wantReadyWhy:  whyReconciled,
			wantReachable: metav1.ConditionTrue,
			wantReachWhy:  whyAllReachable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fg, wantMessage := tc.group()

			warning, emit := summarizeGroup(fg, cadenceOf(&fg.Spec), tc.webhookErr)

			tc.checkEmission(t, warning, emit, wantMessage)
			ready := requireCondition(t, fg, v1beta1.ConditionTypeReady, tc.wantReady, tc.wantReadyWhy)
			requireCondition(t, fg, v1beta1.ConditionTypeFeedReachable, tc.wantReachable, tc.wantReachWhy)
			if ready != nil && ready.ObservedGeneration != fg.Generation {
				t.Errorf("Ready.ObservedGeneration = %d, want %d", ready.ObservedGeneration, fg.Generation)
			}

			checkSummarizeIsIdempotent(t, fg, tc.webhookErr)
		})
	}
}

// conditionKey is the part of a condition the summary is responsible for.
type conditionKey struct {
	Type, Reason, Message string
	Status                metav1.ConditionStatus
	ObservedGeneration    int64
}

func keyOf(c metav1.Condition) conditionKey {
	return conditionKey{c.Type, c.Reason, c.Message, c.Status, c.ObservedGeneration}
}

func conditionsEqual(a, b []metav1.Condition) bool {
	return slices.EqualFunc(a, b, func(x, y metav1.Condition) bool { return keyOf(x) == keyOf(y) })
}

// TestSetReadyCondition_InvalidIntervalPrecedence asserts InvalidInterval
// outranks the per-feed summary (the spec is wrong however the feeds fared)
// but yields to WebhookUnresolved (no feed was processed, so the webhook
// failure is the more fundamental problem).
func TestSetReadyCondition_InvalidIntervalPrecedence(t *testing.T) {
	intervalErr := cadenceOf(&v1beta1.FeedGroupSpec{Interval: overflowingInterval}).err
	webhookErr := errors.New("secret missing")

	readyReason := func(fg *v1beta1.FeedGroup) string {
		c := apimeta.FindStatusCondition(fg.Status.Conditions, v1beta1.ConditionTypeReady)
		if c == nil {
			t.Fatal("Ready condition not set")
		}
		return c.Reason
	}

	failing := &v1beta1.FeedGroup{Status: v1beta1.FeedGroupStatus{Feeds: []v1beta1.FeedStatus{{Name: testFeedName("u"), URL: "u", LastError: testBoom}}}}
	setReadyCondition(failing, nil, intervalErr)
	if got := readyReason(failing); got != reasonInvalidInterval {
		t.Fatalf("with a failing feed and a bad interval: reason = %q, want %q", got, reasonInvalidInterval)
	}

	both := &v1beta1.FeedGroup{}
	setReadyCondition(both, webhookErr, intervalErr)
	if got := readyReason(both); got != "WebhookUnresolved" {
		t.Fatalf("with a webhook failure and a bad interval: reason = %q, want WebhookUnresolved", got)
	}

	// A feed that panicked before handling anything was restored without a
	// failure, so the feeds alone would read healthy: the panic decides Ready.
	panicked := &v1beta1.FeedGroup{Status: v1beta1.FeedGroupStatus{Feeds: []v1beta1.FeedStatus{{Name: testFeedName("u"), URL: "u"}}}}
	setReadyCondition(panicked, &feedPanic{url: "u", value: testBoom, stack: []byte("goroutine 1")}, intervalErr)
	if got := readyReason(panicked); got != reasonReconcilePanicked {
		t.Fatalf("after a feed panicked: reason = %q, want %q", got, reasonReconcilePanicked)
	}
	if msg := apimeta.FindStatusCondition(panicked.Status.Conditions, v1beta1.ConditionTypeReady).Message; strings.Contains(msg, testBoom) || strings.Contains(msg, "goroutine") {
		t.Fatalf("Ready message %q carries the panic value or stack", msg)
	}

	// Correcting the interval clears the condition on the next reconcile.
	setReadyCondition(failing, nil, nil)
	if got := readyReason(failing); got != "FeedErrors" {
		t.Fatalf("after the interval is fixed: reason = %q, want FeedErrors", got)
	}
}

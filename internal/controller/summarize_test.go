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
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/maverickd650/rss2discord-operator/api/v1alpha1"
)

const (
	whyFeedErrors   = "FeedErrors"
	whyReconciled   = "Reconciled"
	whyAllReachable = "AllFeedsReachable"
	reasonHTTP500   = "HTTP500"
)

func feedWithReachable(url string, status metav1.ConditionStatus, reason, lastError string) v1alpha1.FeedStatus {
	fs := v1alpha1.FeedStatus{RSSUrl: url, LastError: lastError}
	if status != "" {
		fs.Conditions = []metav1.Condition{{
			Type: v1alpha1.FeedConditionTypeReachable, Status: status, Reason: reason,
		}}
	}
	return fs
}

// TestSummarizeGroup covers the pure group summary: which Ready and
// FeedReachable conditions a status produces, which of them wins when several
// problems coexist, and when the InvalidInterval Event fires. No reconciler or
// recorder is involved.
func TestSummarizeGroup(t *testing.T) {
	const badInterval = "9999999999999h"
	webhookErr := errors.New("secret not found")

	tests := []struct {
		name string
		// in
		interval      string
		feeds         []v1alpha1.FeedStatus
		webhookErr    error
		priorReady    *metav1.Condition
		wantReady     metav1.ConditionStatus
		wantReadyWhy  string
		wantReachable metav1.ConditionStatus
		wantReachWhy  string
		wantEmit      bool
		wantWarning   bool
	}{
		{
			name:          "healthy group",
			feeds:         []v1alpha1.FeedStatus{feedWithReachable("a", metav1.ConditionTrue, "FetchSucceeded", "")},
			wantReady:     metav1.ConditionTrue,
			wantReadyWhy:  whyReconciled,
			wantReachable: metav1.ConditionTrue,
			wantReachWhy:  whyAllReachable,
		},
		{
			name:          "feed error flips Ready but not FeedReachable when the feed was reachable",
			feeds:         []v1alpha1.FeedStatus{feedWithReachable("a", metav1.ConditionTrue, "FetchSucceeded", "send failed")},
			wantReady:     metav1.ConditionFalse,
			wantReadyWhy:  whyFeedErrors,
			wantReachable: metav1.ConditionTrue,
			wantReachWhy:  whyAllReachable,
		},
		{
			name: "unreachable feed reports the dominant reason on both conditions",
			feeds: []v1alpha1.FeedStatus{
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
			feeds:         []v1alpha1.FeedStatus{feedWithReachable("a", metav1.ConditionUnknown, "Pending", ""), feedWithReachable("b", "", "", "")},
			wantReady:     metav1.ConditionTrue,
			wantReadyWhy:  whyReconciled,
			wantReachable: metav1.ConditionTrue,
			wantReachWhy:  whyAllReachable,
		},
		{
			name:          "invalid interval outranks feed errors and emits once",
			interval:      badInterval,
			feeds:         []v1alpha1.FeedStatus{feedWithReachable("a", metav1.ConditionFalse, reasonHTTP500, "boom")},
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
			feeds:         []v1alpha1.FeedStatus{feedWithReachable("a", metav1.ConditionFalse, reasonHTTP500, "boom")},
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
			priorReady:    &metav1.Condition{Type: v1alpha1.ConditionTypeReady, Status: metav1.ConditionFalse, Reason: reasonInvalidInterval},
			wantReady:     metav1.ConditionFalse,
			wantReadyWhy:  reasonInvalidInterval,
			wantReachable: metav1.ConditionTrue,
			wantReachWhy:  whyAllReachable,
		},
		{
			name:     "Event fires again when the InvalidInterval message changes",
			interval: badInterval,
			priorReady: &metav1.Condition{
				Type: v1alpha1.ConditionTypeReady, Status: metav1.ConditionFalse,
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
				Type: v1alpha1.ConditionTypeReady, Status: metav1.ConditionFalse,
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
			fg := &v1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "fg", Generation: 3},
				Spec:       v1alpha1.FeedGroupSpec{Interval: tc.interval},
				Status:     v1alpha1.FeedGroupStatus{Feeds: tc.feeds},
			}
			wantMessage := ""
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

			warning, emit := summarizeGroup(fg, cadenceOf(&fg.Spec), tc.webhookErr)

			if emit != tc.wantEmit {
				t.Errorf("emit = %v, want %v", emit, tc.wantEmit)
			}
			if tc.wantWarning && warning != wantMessage {
				t.Errorf("warning = %q, want %q", warning, wantMessage)
			}
			if !tc.wantWarning && warning != "" {
				t.Errorf("warning = %q, want empty", warning)
			}

			ready := apimeta.FindStatusCondition(fg.Status.Conditions, v1alpha1.ConditionTypeReady)
			if ready == nil || ready.Status != tc.wantReady || ready.Reason != tc.wantReadyWhy {
				t.Errorf("Ready = %+v, want status %s reason %s", ready, tc.wantReady, tc.wantReadyWhy)
			}
			reachable := apimeta.FindStatusCondition(fg.Status.Conditions, v1alpha1.ConditionTypeFeedReachable)
			if reachable == nil || reachable.Status != tc.wantReachable || reachable.Reason != tc.wantReachWhy {
				t.Errorf("FeedReachable = %+v, want status %s reason %s", reachable, tc.wantReachable, tc.wantReachWhy)
			}
			if ready != nil && ready.ObservedGeneration != fg.Generation {
				t.Errorf("Ready.ObservedGeneration = %d, want %d", ready.ObservedGeneration, fg.Generation)
			}

			// Summarizing an unchanged status again is a no-op: same
			// conditions, and never a repeat Event.
			before := fg.Status.DeepCopy()
			warning, emit = summarizeGroup(fg, cadenceOf(&fg.Spec), tc.webhookErr)
			if emit || warning != "" {
				t.Errorf("second summarize emitted %q (emit=%v), want none", warning, emit)
			}
			if !conditionsEqual(before.Conditions, fg.Status.Conditions) {
				t.Errorf("second summarize changed conditions:\nbefore %+v\nafter  %+v", before.Conditions, fg.Status.Conditions)
			}
		})
	}
}

func conditionsEqual(a, b []metav1.Condition) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Type != b[i].Type || a[i].Status != b[i].Status || a[i].Reason != b[i].Reason ||
			a[i].Message != b[i].Message || a[i].ObservedGeneration != b[i].ObservedGeneration {
			return false
		}
	}
	return true
}

// TestSetReadyCondition_InvalidIntervalPrecedence asserts InvalidInterval
// outranks the per-feed summary (the spec is wrong however the feeds fared)
// but yields to WebhookUnresolved (no feed was processed, so the webhook
// failure is the more fundamental problem).
func TestSetReadyCondition_InvalidIntervalPrecedence(t *testing.T) {
	intervalErr := cadenceOf(&v1alpha1.FeedGroupSpec{Interval: overflowingInterval}).err
	webhookErr := errors.New("secret missing")

	readyReason := func(fg *v1alpha1.FeedGroup) string {
		c := apimeta.FindStatusCondition(fg.Status.Conditions, v1alpha1.ConditionTypeReady)
		if c == nil {
			t.Fatal("Ready condition not set")
		}
		return c.Reason
	}

	failing := &v1alpha1.FeedGroup{Status: v1alpha1.FeedGroupStatus{Feeds: []v1alpha1.FeedStatus{{RSSUrl: "u", LastError: testBoom}}}}
	setReadyCondition(failing, nil, intervalErr)
	if got := readyReason(failing); got != reasonInvalidInterval {
		t.Fatalf("with a failing feed and a bad interval: reason = %q, want %q", got, reasonInvalidInterval)
	}

	both := &v1alpha1.FeedGroup{}
	setReadyCondition(both, webhookErr, intervalErr)
	if got := readyReason(both); got != "WebhookUnresolved" {
		t.Fatalf("with a webhook failure and a bad interval: reason = %q, want WebhookUnresolved", got)
	}

	// A feed that panicked before handling anything was restored without a
	// failure, so the feeds alone would read healthy: the panic decides Ready.
	panicked := &v1alpha1.FeedGroup{Status: v1alpha1.FeedGroupStatus{Feeds: []v1alpha1.FeedStatus{{RSSUrl: "u"}}}}
	setReadyCondition(panicked, &feedPanic{url: "u", value: testBoom, stack: []byte("goroutine 1")}, intervalErr)
	if got := readyReason(panicked); got != reasonReconcilePanicked {
		t.Fatalf("after a feed panicked: reason = %q, want %q", got, reasonReconcilePanicked)
	}

	// Correcting the interval clears the condition on the next reconcile.
	setReadyCondition(failing, nil, nil)
	if got := readyReason(failing); got != "FeedErrors" {
		t.Fatalf("after the interval is fixed: reason = %q, want FeedErrors", got)
	}
}

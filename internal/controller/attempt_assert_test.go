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
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/maverickd650-labs/rss2discord/api/v1beta1"
)

// Assertions on the effects of feedAttempt.record, shared by the matrix and the
// phase tests.

// assertBackoffUntil checks that backoffUntil is exactly want after now, the
// (fake, unmoving) time record ran at, or unset when want is 0.
func assertBackoffUntil(t *testing.T, backoffUntil *metav1.Time, now time.Time, want time.Duration) {
	t.Helper()
	if want == 0 {
		if backoffUntil != nil && !backoffUntil.IsZero() {
			t.Errorf("BackoffUntil = %v, want unset", backoffUntil)
		}
		return
	}
	if backoffUntil == nil || backoffUntil.IsZero() {
		t.Fatalf("BackoffUntil is unset, want %v after %s", want, now.Format(time.RFC3339))
	}
	wantTime := now.Add(want)
	if !backoffUntil.Time.Equal(wantTime) {
		t.Errorf("BackoffUntil = %v, want %v (%v after %s)", backoffUntil.Time, wantTime, want, now.Format(time.RFC3339))
	}
}

// assertLastChecked checks LastChecked is advanced to attemptInstant when want
// is set, and left unset otherwise.
func assertLastChecked(t *testing.T, lastChecked *metav1.Time, want bool) {
	t.Helper()
	advanced := lastChecked != nil && lastChecked.Time.Equal(attemptInstant)
	switch {
	case want && !advanced:
		t.Errorf("LastChecked = %v, want advanced to %s", lastChecked, attemptInstant.Format(time.RFC3339))
	case !want && lastChecked != nil:
		t.Errorf("LastChecked = %v, want it left unset", lastChecked)
	}
}

// assertAttemptCondition checks the one condition the outcome sets, or that
// none was set.
func assertAttemptCondition(t *testing.T, fs *v1beta1.FeedStatus, want attemptWant) {
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
	for _, diff := range conditionDiffs(cond, want) {
		t.Errorf("%s condition: %s", want.condition, diff)
	}
}

// conditionDiffs names each field of cond that is not what want expects.
func conditionDiffs(cond *metav1.Condition, want attemptWant) []string {
	var diffs []string
	if cond.Status != want.status {
		diffs = append(diffs, "status = "+string(cond.Status)+", want "+string(want.status))
	}
	if cond.Reason != want.reason {
		diffs = append(diffs, "reason = "+cond.Reason+", want "+want.reason)
	}
	if cond.ObservedGeneration != attemptGeneration {
		diffs = append(diffs, "ObservedGeneration is not the group's generation")
	}
	if !strings.Contains(cond.Message, want.message) {
		diffs = append(diffs, "message "+cond.Message+" lacks "+want.message)
	}
	return diffs
}

// assertOutcomeMetric checks that exactly one outcome series of the group was
// incremented, once, and it is the wanted one -- or that none was.
func assertOutcomeMetric(t *testing.T, group, wantLabel string) {
	t.Helper()
	total := 0.0
	for _, label := range expectedOutcomes() {
		count := testutil.ToFloat64(feedOperationsTotal.WithLabelValues(attemptNS, group, testFeedName(attemptURL), label))
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
	assertOneWarning(t, got, want)
}

// assertOneWarning checks got is exactly one Warning Event containing every
// fragment of want.
func assertOneWarning(t *testing.T, got, want []string) {
	t.Helper()
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

// reachableConditions is a lone Reachable condition with the given status.
func reachableConditions(status metav1.ConditionStatus) []metav1.Condition {
	return []metav1.Condition{{Type: v1beta1.FeedConditionTypeReachable, Status: status}}
}

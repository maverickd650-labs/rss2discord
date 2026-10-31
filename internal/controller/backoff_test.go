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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/maverickd650/rss2discord-operator/api/v1alpha1"
)

// TestFeedBackoffExtend_CapCrossingFiresOnce walks consecutive failures and
// asserts firstCapped is true only for the failure whose backoff first reaches
// the cap, and that BackoffUntil is the exact backoff past now.
func TestFeedBackoffExtend_CapCrossingFiresOnce(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	base := 5 * time.Minute // 5m * 2^6 = 320m < 6h; 5m * 2^7 = 640m >= 6h
	fs := &v1alpha1.FeedStatus{}
	b := backoffOf(fs)

	var firstCappedAt []int32
	for fs.RetryCount = 1; fs.RetryCount <= 12; fs.RetryCount++ {
		if b.extend(base, now) {
			firstCappedAt = append(firstCappedAt, fs.RetryCount)
		}
		want := min(permanentBackoffDuration(int(fs.RetryCount), base), maxPermanentBackoff)
		assertBackoffUntil(t, fs.BackoffUntil, now, want)
	}
	if len(firstCappedAt) != 1 || firstCappedAt[0] != 7 {
		t.Fatalf("firstCapped fired at RetryCounts %v, want exactly [7]", firstCappedAt)
	}
}

// TestFeedBackoffExtend_CappedFromTheStart covers the cases where the very
// first failure is already capped (RetryCount <= 1): a non-positive base, or a
// base so large its first backoff exceeds the cap.
func TestFeedBackoffExtend_CappedFromTheStart(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, base := range []time.Duration{0, -time.Minute, 4 * time.Hour} {
		fs := &v1alpha1.FeedStatus{RetryCount: 1}
		if !backoffOf(fs).extend(base, now) {
			t.Errorf("base %v, RetryCount 1: want firstCapped", base)
		}
		fs.RetryCount = 2
		if backoffOf(fs).extend(base, now) {
			t.Errorf("base %v, RetryCount 2: want no second firstCapped", base)
		}
		assertBackoffUntil(t, fs.BackoffUntil, now, maxPermanentBackoff)
	}
}

// TestFeedBackoffExtend_OverflowGuard asserts a huge RetryCount (where 1<<n
// would wrap) still yields the cap, and is not a first crossing.
func TestFeedBackoffExtend_OverflowGuard(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, count := range []int32{62, 63, 64, 1 << 30} {
		fs := &v1alpha1.FeedStatus{RetryCount: count}
		if backoffOf(fs).extend(5*time.Minute, now) {
			t.Errorf("RetryCount %d: want no firstCapped, the cap was crossed long ago", count)
		}
		assertBackoffUntil(t, fs.BackoffUntil, now, maxPermanentBackoff)
	}
}

// TestFeedBackoffClear_ReArmsCapEvent asserts clear drops BackoffUntil and
// RetryCount (but not LastError), so a feed that recovers can fire the
// cap-crossing Event again.
func TestFeedBackoffClear_ReArmsCapEvent(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fs := &v1alpha1.FeedStatus{RetryCount: 7, LastError: "boom"}
	if !backoffOf(fs).extend(5*time.Minute, now) {
		t.Fatal("setup: RetryCount 7 should be the first capped failure")
	}
	backoffOf(fs).clear()
	if fs.BackoffUntil != "" || fs.RetryCount != 0 || fs.LastError != "boom" {
		t.Fatalf("after clear: %+v, want BackoffUntil and RetryCount reset, LastError kept", fs)
	}
	if backoffOf(fs).active(now) {
		t.Error("a cleared backoff must not be active")
	}
}

// TestFeedBackoffSendCaused pins the Reachable-based inference: a backoff is
// send-caused unless the feed's Reachable condition is False, and there must be
// a backoff at all.
func TestFeedBackoffSendCaused(t *testing.T) {
	reachable := func(status metav1.ConditionStatus) []metav1.Condition {
		return []metav1.Condition{{Type: v1alpha1.FeedConditionTypeReachable, Status: status}}
	}
	const until = "2026-01-01T18:00:00Z"
	cases := []struct {
		name string
		fs   v1alpha1.FeedStatus
		want bool
	}{
		{"no backoff", v1alpha1.FeedStatus{Conditions: reachable(metav1.ConditionTrue)}, false},
		{"fetch failure (Reachable False)", v1alpha1.FeedStatus{BackoffUntil: until, Conditions: reachable(metav1.ConditionFalse)}, false},
		{"send failure (Reachable True)", v1alpha1.FeedStatus{BackoffUntil: until, Conditions: reachable(metav1.ConditionTrue)}, true},
		{"no Reachable condition", v1alpha1.FeedStatus{BackoffUntil: until}, true},
		{"expired backoff still counts", v1alpha1.FeedStatus{BackoffUntil: "2000-01-01T00:00:00Z"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := backoffOf(&tc.fs).sendCaused(); got != tc.want {
				t.Errorf("sendCaused() = %v, want %v", got, tc.want)
			}
		})
	}
}

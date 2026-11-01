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

	"github.com/maverickd650-labs/rss2discord/api/v1alpha1"
)

// TestFeedBackoffExtend_CapCrossingFiresOnce walks consecutive failures and
// asserts firstCapped is true only for the failure whose backoff first reaches
// the cap, and that BackoffUntil is the exact backoff past now.
func TestFeedBackoffExtend_CapCrossingFiresOnce(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	base := 5 * time.Minute // 5m * 2^6 = 320m < 6h; 5m * 2^7 = 640m >= 6h
	fs := &v1alpha1.FeedStatus{}
	b := runOf(fs)

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
		if !runOf(fs).extend(base, now) {
			t.Errorf("base %v, RetryCount 1: want firstCapped", base)
		}
		fs.RetryCount = 2
		if runOf(fs).extend(base, now) {
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
		if runOf(fs).extend(5*time.Minute, now) {
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
	if !runOf(fs).extend(5*time.Minute, now) {
		t.Fatal("setup: RetryCount 7 should be the first capped failure")
	}
	runOf(fs).clear()
	if fs.BackoffUntil != "" || fs.RetryCount != 0 || fs.LastError != "boom" {
		t.Fatalf("after clear: %+v, want BackoffUntil and RetryCount reset, LastError kept", fs)
	}
	if runOf(fs).active(now) {
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
			if got := runOf(&tc.fs).sendCaused(); got != tc.want {
				t.Errorf("sendCaused() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPermanentBackoffDuration verifies the exponential formula and cap.
func TestPermanentBackoffDuration(t *testing.T) {
	base := 5 * time.Minute
	cases := []struct {
		retryCount int
		wantCapped bool
	}{
		{retryCount: 1, wantCapped: false}, // 5m * 2^1 = 10m
		{retryCount: 2, wantCapped: false}, // 5m * 2^2 = 20m
		{retryCount: 6, wantCapped: false}, // 5m * 2^6 = 320m < 6h
		{retryCount: 7, wantCapped: true},  // 5m * 2^7 = 640m > 6h
		{retryCount: 62, wantCapped: true}, // overflow guard
		{retryCount: 63, wantCapped: true}, // overflow guard
	}
	for _, tc := range cases {
		got := permanentBackoffDuration(tc.retryCount, base)
		if tc.wantCapped {
			if got != maxPermanentBackoff {
				t.Errorf("retryCount=%d: got %v, want cap %v", tc.retryCount, got, maxPermanentBackoff)
			}
		} else {
			if got >= maxPermanentBackoff {
				t.Errorf("retryCount=%d: got %v >= cap %v, expected uncapped", tc.retryCount, got, maxPermanentBackoff)
			}
			want := base * (1 << uint(tc.retryCount))
			if got != want {
				t.Errorf("retryCount=%d: got %v, want %v", tc.retryCount, got, want)
			}
		}
	}
}

// TestPermanentBackoffDuration_ClampsRetryCountBelowOne asserts a retryCount
// of zero (or negative) is treated the same as 1, so a caller can never get a
// larger backoff than the first retry by passing an unclamped count.
func TestPermanentBackoffDuration_ClampsRetryCountBelowOne(t *testing.T) {
	base := 5 * time.Minute
	want := permanentBackoffDuration(1, base)

	if got := permanentBackoffDuration(0, base); got != want {
		t.Errorf("retryCount=0: got %v, want %v (same as retryCount=1)", got, want)
	}
	if got := permanentBackoffDuration(-1, base); got != want {
		t.Errorf("retryCount=-1: got %v, want %v (same as retryCount=1)", got, want)
	}
}

// TestFeedBackoffActive verifies the active-feeds skip predicate: empty or
// malformed BackoffUntil values are treated as "not in backoff" so a bad
// value can never permanently wedge a feed, and the comparison is a strict
// "now before until".
func TestFeedBackoffActive(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name         string
		backoffUntil string
		want         bool
	}{
		{name: "empty", backoffUntil: "", want: false},
		{name: "malformed", backoffUntil: "not-a-timestamp", want: false},
		{name: "future", backoffUntil: "2026-01-01T00:00:01Z", want: true},
		{name: "past", backoffUntil: "2025-12-31T23:59:59Z", want: false},
		{name: "equal to now", backoffUntil: testReconcileTime, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := runOf(&v1alpha1.FeedStatus{BackoffUntil: tc.backoffUntil}).active(now); got != tc.want {
				t.Errorf("active(%q, %v) = %v, want %v", tc.backoffUntil, now, got, tc.want)
			}
		})
	}
}

// TestNextBackoffUntil verifies the RFC3339 formatting of a computed backoff
// expiry, and that the result round-trips through retryRun.active as expected.
func TestNextBackoffUntil(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	got := nextBackoffUntil(now, 10*time.Minute)
	want := "2026-01-01T00:10:00Z"
	if got != want {
		t.Fatalf("nextBackoffUntil() = %q, want %q", got, want)
	}
	if !runOf(&v1alpha1.FeedStatus{BackoffUntil: got}).active(now) {
		t.Fatalf("active(%q, %v) = false, want true (still in the future)", got, now)
	}
	if runOf(&v1alpha1.FeedStatus{BackoffUntil: got}).active(now.Add(10 * time.Minute)) {
		t.Fatalf("active(%q, %v) = true, want false (backoff expired)", got, now.Add(10*time.Minute))
	}
}

// TestClearPermanentBackoffs asserts the helper resets BackoffUntil and
// RetryCount on every feed, including those not in backoff, so the first
// permanentBackoffDuration call after a spec change uses a fresh RetryCount.
func TestClearPermanentBackoffs(t *testing.T) {
	fg, _ := newMetricsFeedGroup("clear-backoff-ns", "clear-backoff", "")
	fg.Spec.Feeds = append(fg.Spec.Feeds, v1alpha1.FeedSpec{RSSUrl: "https://other.example.com/feed.xml"})
	ensureFeedStatuses(fg)

	feedStatusFor(fg, exampleFeedURL).BackoffUntil = "2030-01-01T00:00:00Z"
	feedStatusFor(fg, exampleFeedURL).RetryCount = 7
	feedStatusFor(fg, "https://other.example.com/feed.xml").RetryCount = 2

	clearPermanentBackoffs(fg)

	for _, fs := range fg.Status.Feeds {
		if fs.BackoffUntil != "" {
			t.Errorf("feed %s: BackoffUntil = %q, want empty", fs.RSSUrl, fs.BackoffUntil)
		}
		if fs.RetryCount != 0 {
			t.Errorf("feed %s: RetryCount = %d, want 0", fs.RSSUrl, fs.RetryCount)
		}
	}
}

// reachableIs returns a FeedStatus whose Reachable condition has status, with
// the given run state.
func reachableIs(status metav1.ConditionStatus, retryCount int32, backoffUntil string) *v1alpha1.FeedStatus {
	return &v1alpha1.FeedStatus{
		RetryCount:   retryCount,
		BackoffUntil: backoffUntil,
		Conditions:   []metav1.Condition{{Type: v1alpha1.FeedConditionTypeReachable, Status: status}},
	}
}

// TestRetryRun_FetchSuccessEndsFetchPhaseRun asserts a fetch success ends a run
// whose phase is fetching (Reachable False), whether it left a count, a backoff
// or both, and does nothing to an idle feed.
func TestRetryRun_FetchSuccessEndsFetchPhaseRun(t *testing.T) {
	for name, fs := range map[string]*v1alpha1.FeedStatus{
		"count only":   reachableIs(metav1.ConditionFalse, 3, ""),
		"backoff only": reachableIs(metav1.ConditionFalse, 0, "2026-01-01T18:00:00Z"),
		"both":         reachableIs(metav1.ConditionFalse, 2, "2026-01-01T18:00:00Z"),
	} {
		t.Run(name, func(t *testing.T) {
			runOf(fs).fetchSucceeded()
			if fs.RetryCount != 0 || fs.BackoffUntil != "" {
				t.Errorf("run not ended: %+v", fs)
			}
		})
	}
}

// TestRetryRun_FetchSuccessKeepsDeliveryPhaseRun asserts a fetch success leaves
// a delivery-phase run (Reachable True, or not yet set) alone, so a failing
// entry's retries can exhaust.
func TestRetryRun_FetchSuccessKeepsDeliveryPhaseRun(t *testing.T) {
	const until = "2026-01-01T18:00:00Z"
	for name, fs := range map[string]*v1alpha1.FeedStatus{
		"Reachable True": reachableIs(metav1.ConditionTrue, 2, until),
		"no condition":   {RetryCount: 2, BackoffUntil: until},
	} {
		t.Run(name, func(t *testing.T) {
			runOf(fs).fetchSucceeded()
			if fs.RetryCount != 2 || fs.BackoffUntil != until {
				t.Errorf("delivery run was ended: %+v", fs)
			}
		})
	}
}

// TestRetryRun_FailCountsWithinAPhase asserts consecutive failures of one phase
// accumulate, and that the limit predicates track the count.
func TestRetryRun_FailCountsWithinAPhase(t *testing.T) {
	fs := reachableIs(metav1.ConditionFalse, 0, "")
	r := runOf(fs)
	r.fail(phaseFetch)
	r.fail(phaseFetch)
	if fs.RetryCount != 2 || !r.outstanding() {
		t.Fatalf("RetryCount = %d, want 2 and outstanding", fs.RetryCount)
	}
	if !r.belowLimit(3) || r.exhaustedNow(3) || r.pastLimit(3) {
		t.Error("count 2 of 3: want below the limit only")
	}
	r.fail(phaseFetch)
	if r.belowLimit(3) || !r.exhaustedNow(3) || r.pastLimit(3) {
		t.Error("count 3 of 3: want exhaustedNow only")
	}
	r.fail(phaseFetch)
	if r.belowLimit(3) || r.exhaustedNow(3) || !r.pastLimit(3) {
		t.Error("count 4 of 3: want pastLimit only")
	}
}

// TestRetryRun_FetchFailureRestartsDeliveryRun asserts a fetch failure while
// Reachable is True starts a new run at 1 and drops the delivery run's backoff,
// whereas delivery failures keep counting on, and a fetch failure while
// Reachable is False continues its own run.
func TestRetryRun_FetchFailureRestartsDeliveryRun(t *testing.T) {
	fs := reachableIs(metav1.ConditionTrue, 4, "2026-01-01T18:00:00Z")
	runOf(fs).fail(phaseDelivery)
	if fs.RetryCount != 5 {
		t.Fatalf("delivery failure: RetryCount = %d, want 5", fs.RetryCount)
	}
	runOf(fs).fail(phaseFetch)
	if fs.RetryCount != 1 || fs.BackoffUntil != "" {
		t.Errorf("fetch failure over a delivery run: %+v, want count 1 and no backoff", fs)
	}

	fetching := reachableIs(metav1.ConditionFalse, 4, "2026-01-01T18:00:00Z")
	runOf(fetching).fail(phaseFetch)
	if fetching.RetryCount != 5 || fetching.BackoffUntil == "" {
		t.Errorf("fetch failure over a fetch run: %+v, want count 5 and the backoff kept", fetching)
	}
}

// TestRetryRun_ClearReArmsFirstCapped asserts a cleared run reaches the cap as
// "first" again after the same number of failures.
func TestRetryRun_ClearReArmsFirstCapped(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fs := reachableIs(metav1.ConditionFalse, 0, "")
	r := runOf(fs)
	for round := 1; round <= 2; round++ {
		var capped []int32
		for range 10 {
			r.fail(phaseFetch)
			if r.extend(5*time.Minute, now) {
				capped = append(capped, fs.RetryCount)
			}
		}
		if len(capped) != 1 || capped[0] != 7 {
			t.Fatalf("round %d: firstCapped at %v, want [7]", round, capped)
		}
		r.clear()
	}
}

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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/maverickd650-labs/rss2discord/api/v1beta1"
)

// TestFeedBackoffExtend_CapCrossingFiresOnce walks consecutive failures and
// asserts firstCapped is true only for the failure whose backoff first reaches
// the cap, and that BackoffUntil is the exact backoff past now.
func TestFeedBackoffExtend_CapCrossingFiresOnce(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	base := 5 * time.Minute // 5m * 2^6 = 320m < 6h; 5m * 2^7 = 640m >= 6h
	fs := &v1beta1.FeedStatus{}
	state := retryStateOf(fs)

	var firstCappedAt []int32
	for fs.RetryCount = 1; fs.RetryCount <= 12; fs.RetryCount++ {
		if state.extend(base, now) {
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
		fs := &v1beta1.FeedStatus{RetryCount: 1}
		if !retryStateOf(fs).extend(base, now) {
			t.Errorf("base %v, RetryCount 1: want firstCapped", base)
		}
		fs.RetryCount = 2
		if retryStateOf(fs).extend(base, now) {
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
		fs := &v1beta1.FeedStatus{RetryCount: count}
		if retryStateOf(fs).extend(5*time.Minute, now) {
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
	fs := &v1beta1.FeedStatus{RetryCount: 7, LastError: "boom"}
	if !retryStateOf(fs).extend(5*time.Minute, now) {
		t.Fatal("setup: RetryCount 7 should be the first capped failure")
	}
	retryStateOf(fs).clear()
	if !resetKeepingLastError(fs, "boom") {
		t.Fatalf("after clear: %+v, want BackoffUntil and RetryCount reset, LastError kept", fs)
	}
	if retryStateOf(fs).active(now) {
		t.Error("a cleared backoff must not be active")
	}
}

// resetKeepingLastError reports whether fs has no backoff and no retry count
// but still carries lastError.
func resetKeepingLastError(fs *v1beta1.FeedStatus, lastError string) bool {
	return fs.BackoffUntil == nil && fs.RetryCount == 0 && fs.LastError == lastError
}

// TestFeedBackoffSendCaused pins the Reachable-based inference: a backoff is
// send-caused unless the feed's Reachable condition is False, and there must be
// a backoff at all.
func TestFeedBackoffSendCaused(t *testing.T) {
	reachable := func(status metav1.ConditionStatus) []metav1.Condition {
		return []metav1.Condition{{Type: v1beta1.FeedConditionTypeReachable, Status: status}}
	}
	until := metav1TimeRFC3339("2026-01-01T18:00:00Z")
	cases := []struct {
		name string
		fs   v1beta1.FeedStatus
		want bool
	}{
		{"no backoff", v1beta1.FeedStatus{Conditions: reachable(metav1.ConditionTrue)}, false},
		{"fetch failure (Reachable False)", v1beta1.FeedStatus{BackoffUntil: until, Conditions: reachable(metav1.ConditionFalse)}, false},
		{"send failure (Reachable True)", v1beta1.FeedStatus{BackoffUntil: until, Conditions: reachable(metav1.ConditionTrue)}, true},
		{"no Reachable condition", v1beta1.FeedStatus{BackoffUntil: until}, true},
		{"expired backoff still counts", v1beta1.FeedStatus{BackoffUntil: metav1TimeRFC3339("2000-01-01T00:00:00Z")}, true},
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
	wantOne := permanentBackoffDuration(1, base)
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
		if capped := got >= maxPermanentBackoff; capped != tc.wantCapped {
			t.Errorf("retryCount=%d: got %v, capped = %v, want capped = %v (cap %v)",
				tc.retryCount, got, capped, tc.wantCapped, maxPermanentBackoff)
		}
		if tc.wantCapped && got != maxPermanentBackoff {
			t.Errorf("retryCount=%d: got %v, want cap %v", tc.retryCount, got, maxPermanentBackoff)
		}
	}

	for _, retryCount := range []int{0, -1} {
		if got := permanentBackoffDuration(retryCount, base); got != wantOne {
			t.Errorf("retryCount=%d: got %v, want %v (same as retryCount=1)", retryCount, got, wantOne)
		}
	}
}

// TestFeedBackoffActive verifies the active-feeds skip predicate: nil or zero
// BackoffUntil values are treated as "not in backoff" so a bad value can never
// permanently wedge a feed, and the comparison is a strict "now before until".
func TestFeedBackoffActive(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	future := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC))
	past := metav1.NewTime(time.Date(2025, 12, 31, 23, 59, 59, 0, time.UTC))
	equal := metav1.NewTime(now)

	cases := []struct {
		name         string
		backoffUntil *metav1.Time
		want         bool
	}{
		{name: "nil", backoffUntil: nil, want: false},
		{name: "zero", backoffUntil: &metav1.Time{}, want: false},
		{name: "future", backoffUntil: &future, want: true},
		{name: "past", backoffUntil: &past, want: false},
		{name: "equal to now", backoffUntil: &equal, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryStateOf(&v1beta1.FeedStatus{BackoffUntil: tc.backoffUntil}).active(now); got != tc.want {
				t.Errorf("active(%v, %v) = %v, want %v", tc.backoffUntil, now, got, tc.want)
			}
		})
	}
}

// TestNextBackoffUntil verifies the computed backoff expiry as *metav1.Time,
// and that the result round-trips through retryState.active as expected.
func TestNextBackoffUntil(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	got := nextBackoffUntil(now, 10*time.Minute)
	want := now.Add(10 * time.Minute)
	if !got.Time.Equal(want) {
		t.Fatalf("nextBackoffUntil() = %v, want %v", got.Time, want)
	}
	if !retryStateOf(&v1beta1.FeedStatus{BackoffUntil: got}).active(now) {
		t.Fatalf("active(%v, %v) = false, want true (still in the future)", got, now)
	}
	if retryStateOf(&v1beta1.FeedStatus{BackoffUntil: got}).active(now.Add(10 * time.Minute)) {
		t.Fatalf("active(%v, %v) = true, want false (backoff expired)", got, now.Add(10*time.Minute))
	}
}

// TestClearPermanentBackoffs asserts the helper resets BackoffUntil and
// RetryCount across all feeds in the group.
func TestClearPermanentBackoffs(t *testing.T) {
	fg := &v1beta1.FeedGroup{
		Spec: v1beta1.FeedGroupSpec{
			Feeds: []v1beta1.FeedSpec{
				{Name: testFeedName(exampleFeedURL), URL: exampleFeedURL},
				{Name: testFeedName("https://other.example.com/feed.xml"), URL: "https://other.example.com/feed.xml"},
			},
		},
	}
	ensureFeedStatuses(fg)
	feedStatusFor(fg, testFeedName(exampleFeedURL)).BackoffUntil = metav1TimeRFC3339("2030-01-01T00:00:00Z")
	feedStatusFor(fg, testFeedName(exampleFeedURL)).RetryCount = 7
	feedStatusFor(fg, testFeedName("https://other.example.com/feed.xml")).RetryCount = 2

	clearPermanentBackoffs(fg)

	for _, fs := range fg.Status.Feeds {
		if fs.BackoffUntil != nil {
			t.Errorf("feed %s: BackoffUntil = %v, want nil", fs.URL, fs.BackoffUntil)
		}
		if fs.RetryCount != 0 {
			t.Errorf("feed %s: RetryCount = %d, want 0", fs.URL, fs.RetryCount)
		}
	}
}

// reachableIs returns a FeedStatus whose Reachable condition has status, with
// the given run state.
func reachableIs(status metav1.ConditionStatus, retryCount int32, backoffUntil string) *v1beta1.FeedStatus {
	return &v1beta1.FeedStatus{
		RetryCount:   retryCount,
		BackoffUntil: metav1TimeRFC3339(backoffUntil),
		Conditions:   []metav1.Condition{{Type: v1beta1.FeedConditionTypeReachable, Status: status}},
	}
}

// TestRetryRun_FetchPhaseCapturedByRunOf asserts a fetch-phase retryRun keeps
// the phase it captured when runOf built it, even if Reachable changes
// afterwards.
func TestRetryRun_FetchPhaseCapturedByRunOf(t *testing.T) {
	fetching := reachableIs(metav1.ConditionFalse, 4, "2026-01-01T18:00:00Z")
	run := runOf(fetching)
	fetching.Conditions[0].Status = metav1.ConditionTrue
	run.fetchSucceeded()
	if fetching.RetryCount != 0 || fetching.BackoffUntil != nil {
		t.Errorf("fetch run not ended from its captured phase: %+v", fetching)
	}
}

// TestRetryRun_DeliveryPhaseCapturedByRunOf asserts a delivery-phase retryRun
// keeps the phase it captured when runOf built it, even if Reachable changes
// afterwards.
func TestRetryRun_DeliveryPhaseCapturedByRunOf(t *testing.T) {
	delivering := reachableIs(metav1.ConditionTrue, 4, "2026-01-01T18:00:00Z")
	run := runOf(delivering)
	delivering.Conditions[0].Status = metav1.ConditionFalse
	if !run.sendCaused() {
		t.Error("delivery backoff reclassified after Reachable flipped to False")
	}
	run.fail(failureFetch)
	if delivering.RetryCount != 1 || delivering.BackoffUntil != nil {
		t.Errorf("delivery run not replaced from its captured phase: %+v", delivering)
	}
}

// TestRetryRun_FailOverUnknownPhase asserts a failure over a feed with no
// Reachable condition (unknown phase) just counts, and that a zero
// failurePhase is rejected.
func TestRetryRun_FailOverUnknownPhase(t *testing.T) {
	const until = "2026-01-01T18:00:00Z"
	unknown := &v1beta1.FeedStatus{RetryCount: 2, BackoffUntil: metav1TimeRFC3339(until)}
	runOf(unknown).fail(failureFetch)
	if unknown.RetryCount != 3 || !unknown.BackoffUntil.Time.Equal(mustParse(t, until)) {
		t.Errorf("fail over an unknown phase must only count: %+v", unknown)
	}

	defer func() {
		if recover() == nil {
			t.Fatal("fail with zero failurePhase did not panic")
		}
	}()
	runOf(unknown).fail(0)
}

// TestRetryRun_FetchSuccessEndsFetchPhaseRun asserts a fetch success ends a run
// whose Reachable condition is False, whether it left a count, a backoff or
// both, and does nothing to an idle feed.
func TestRetryRun_FetchSuccessEndsFetchPhaseRun(t *testing.T) {
	for name, fs := range map[string]*v1beta1.FeedStatus{
		"count only":   reachableIs(metav1.ConditionFalse, 3, ""),
		"backoff only": reachableIs(metav1.ConditionFalse, 0, "2026-01-01T18:00:00Z"),
		"both":         reachableIs(metav1.ConditionFalse, 2, "2026-01-01T18:00:00Z"),
	} {
		t.Run(name, func(t *testing.T) {
			runOf(fs).fetchSucceeded()
			if fs.RetryCount != 0 || fs.BackoffUntil != nil {
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
	for name, fs := range map[string]*v1beta1.FeedStatus{
		"Reachable True": reachableIs(metav1.ConditionTrue, 2, until),
		"no condition":   {RetryCount: 2, BackoffUntil: metav1TimeRFC3339(until)},
	} {
		t.Run(name, func(t *testing.T) {
			runOf(fs).fetchSucceeded()
			if fs.RetryCount != 2 || !fs.BackoffUntil.Time.Equal(mustParse(t, until)) {
				t.Errorf("delivery run was ended: %+v", fs)
			}
		})
	}
}

// limitPosition names where a feed's RetryCount sits relative to max, using
// the retryState predicates, so a test can compare one string.
func limitPosition(fs *v1beta1.FeedStatus, max int32) string {
	state := retryStateOf(fs)
	var positions []string
	if state.belowLimit(max) {
		positions = append(positions, "below")
	}
	if state.exhaustedNow(max) {
		positions = append(positions, "exhausted")
	}
	if state.pastLimit(max) {
		positions = append(positions, "past")
	}
	return strings.Join(positions, "+")
}

// TestRetryRun_FailCountsWithinAPhase asserts consecutive failures of one phase
// accumulate, while count predicates belong to phase-independent retry state.
func TestRetryRun_FailCountsWithinAPhase(t *testing.T) {
	const limit = 3
	fs := reachableIs(metav1.ConditionFalse, 0, "")
	for _, step := range []struct {
		wantCount    int32
		wantPosition string
	}{
		{2, "below"},
		{3, "exhausted"},
		{4, "past"},
	} {
		for fs.RetryCount < step.wantCount {
			runOf(fs).fail(failureFetch)
		}
		if fs.RetryCount != step.wantCount || !retryStateOf(fs).outstanding() {
			t.Fatalf("RetryCount = %d, want %d and outstanding", fs.RetryCount, step.wantCount)
		}
		if got := limitPosition(fs, limit); got != step.wantPosition {
			t.Errorf("count %d of %d: position %q, want %q only", fs.RetryCount, limit, got, step.wantPosition)
		}
	}
}

// TestRetryRun_FetchFailureRestartsDeliveryRun asserts a fetch failure while
// Reachable is True starts a new run at 1 and drops the delivery run's backoff,
// whereas delivery failures keep counting on, and a fetch failure while
// Reachable is False continues its own run.
func TestRetryRun_FetchFailureRestartsDeliveryRun(t *testing.T) {
	fs := reachableIs(metav1.ConditionTrue, 4, "2026-01-01T18:00:00Z")
	runOf(fs).fail(failureDelivery)
	if fs.RetryCount != 5 {
		t.Fatalf("delivery failure: RetryCount = %d, want 5", fs.RetryCount)
	}
	runOf(fs).fail(failureFetch)
	if fs.RetryCount != 1 || fs.BackoffUntil != nil {
		t.Errorf("fetch failure over a delivery run: %+v, want count 1 and no backoff", fs)
	}

	fetching := reachableIs(metav1.ConditionFalse, 4, "2026-01-01T18:00:00Z")
	runOf(fetching).fail(failureFetch)
	if fetching.RetryCount != 5 || fetching.BackoffUntil == nil {
		t.Errorf("fetch failure over a fetch run: %+v, want count 5 and the backoff kept", fetching)
	}
}

// TestRetryRun_ClearReArmsFirstCapped asserts a cleared run reaches the cap as
// "first" again after the same number of failures.
func TestRetryRun_ClearReArmsFirstCapped(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fs := reachableIs(metav1.ConditionFalse, 0, "")
	for round := 1; round <= 2; round++ {
		var capped []int32
		for range 10 {
			runOf(fs).fail(failureFetch)
			if retryStateOf(fs).extend(5*time.Minute, now) {
				capped = append(capped, fs.RetryCount)
			}
		}
		if len(capped) != 1 || capped[0] != 7 {
			t.Fatalf("round %d: firstCapped at %v, want [7]", round, capped)
		}
		retryStateOf(fs).clear()
	}
}

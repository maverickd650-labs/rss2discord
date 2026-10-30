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

	v1alpha1 "github.com/maverickd650/rss2discord-operator/api/v1alpha1"
)

// Whole-scenario checks of the group schedule: which feeds a reconcile
// fetches, and how long it then waits. Times are explicit; nothing here reads
// the wall clock.

var scheduleNow = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func stamp(offset time.Duration) string { return scheduleNow.Add(offset).Format(time.RFC3339) }

// scheduleGroup builds a group whose feeds are named by the keys of statuses
// (in the given order); a nil status leaves the feed without one.
func scheduleGroup(interval string, feeds ...v1alpha1.FeedStatus) *v1alpha1.FeedGroup {
	fg := &v1alpha1.FeedGroup{Spec: v1alpha1.FeedGroupSpec{Interval: interval, RetryInterval: "5m"}}
	for _, fs := range feeds {
		fg.Spec.Feeds = append(fg.Spec.Feeds, v1alpha1.FeedSpec{RSSUrl: fs.RSSUrl})
		fg.Status.Feeds = append(fg.Status.Feeds, fs)
	}
	return fg
}

func activeURLs(p feedPlan) []string {
	urls := make([]string, 0, len(p.active))
	for _, f := range p.active {
		urls = append(urls, f.RSSUrl)
	}
	return urls
}

func sameURLs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestPlanFeeds(t *testing.T) {
	healthy := func(url string, age time.Duration) v1alpha1.FeedStatus {
		return v1alpha1.FeedStatus{RSSUrl: url, LastChecked: stamp(-age)}
	}
	tests := []struct {
		name        string
		fg          func() *v1alpha1.FeedGroup
		force       bool
		wantActive  []string
		wantNextDue time.Duration
	}{
		{
			name: "healthy feed is not fetched early because a sibling fails",
			fg: func() *v1alpha1.FeedGroup {
				failing := healthy("failing", time.Minute)
				failing.LastError, failing.RetryCount = "upstream error", 1
				return scheduleGroup(defaultInterval, healthy("healthy", 10*time.Minute), failing)
			},
			wantActive:  []string{"failing"},
			wantNextDue: 20 * time.Minute,
		},
		{
			name: "backoff outranks always-due failure and adds no wake time",
			fg: func() *v1alpha1.FeedGroup {
				backed := healthy("backed", 2*time.Hour)
				backed.LastError, backed.RetryCount, backed.BackoffUntil = "404", 3, stamp(time.Minute)
				return scheduleGroup(defaultInterval, backed, healthy("healthy", 10*time.Minute))
			},
			wantActive:  nil,
			wantNextDue: 20 * time.Minute,
		},
		{
			name: "backoff expiring exactly now is due again",
			fg: func() *v1alpha1.FeedGroup {
				backed := healthy("backed", 2*time.Hour)
				backed.LastError, backed.RetryCount, backed.BackoffUntil = "404", 3, stamp(0)
				return scheduleGroup(defaultInterval, backed)
			},
			wantActive: []string{"backed"},
		},
		{
			name: "interval elapsing exactly now is due; one second earlier is not",
			fg: func() *v1alpha1.FeedGroup {
				return scheduleGroup(defaultInterval, healthy("exact", 30*time.Minute), healthy("early", 30*time.Minute-time.Second))
			},
			wantActive:  []string{"exact"},
			wantNextDue: time.Second,
		},
		{
			name: "spec change forces healthy feeds but not backed-off or paused ones",
			fg: func() *v1alpha1.FeedGroup {
				backed := healthy("backed", time.Minute)
				backed.BackoffUntil = stamp(time.Hour)
				fg := scheduleGroup(defaultInterval, healthy("healthy", time.Minute), backed, healthy("paused", time.Hour))
				fg.Spec.Feeds[2].Paused = true
				return fg
			},
			force:      true,
			wantActive: []string{"healthy"},
		},
		{
			name: "malformed timestamps are recoverable",
			fg: func() *v1alpha1.FeedGroup {
				badCheck := v1alpha1.FeedStatus{RSSUrl: "badcheck", LastChecked: "bad-last-checked"}
				badBackoff := healthy("badbackoff", 2*time.Hour)
				badBackoff.BackoffUntil = "bad-backoff-until"
				return scheduleGroup(defaultInterval, badCheck, badBackoff)
			},
			wantActive: []string{"badcheck", "badbackoff"},
		},
		{
			name: "unparsable interval falls back to the default for the due check",
			fg: func() *v1alpha1.FeedGroup {
				return scheduleGroup("not-a-duration", healthy("a", 10*time.Minute))
			},
			wantNextDue: fallbackInterval - 10*time.Minute,
		},
		{
			name: "soonest of several not-yet-due feeds wins",
			fg: func() *v1alpha1.FeedGroup {
				return scheduleGroup(defaultInterval, healthy("a", 5*time.Minute), healthy("b", 25*time.Minute), healthy("c", 10*time.Minute))
			},
			wantNextDue: 5 * time.Minute,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := planFeeds(tc.fg(), scheduleNow, tc.force)
			if urls := activeURLs(got); !sameURLs(urls, tc.wantActive) {
				t.Errorf("active = %v, want %v", urls, tc.wantActive)
			}
			if got.nextDue != tc.wantNextDue {
				t.Errorf("nextDue = %v, want %v", got.nextDue, tc.wantNextDue)
			}
		})
	}
}

func TestPlanFeeds_PausedAndEmptyURLNeverContributeWakeTime(t *testing.T) {
	fg := scheduleGroup(defaultInterval, v1alpha1.FeedStatus{RSSUrl: "paused", LastChecked: stamp(-time.Minute)})
	fg.Spec.Feeds[0].Paused = true
	fg.Spec.Feeds = append(fg.Spec.Feeds, v1alpha1.FeedSpec{})
	if got := planFeeds(fg, scheduleNow, false); len(got.active) != 0 || got.nextDue != 0 {
		t.Fatalf("plan = %+v, want nothing active and no wake time", got)
	}
}

func TestRequeueDecision(t *testing.T) {
	spec := &v1alpha1.FeedGroupSpec{Interval: "1h", RetryInterval: "2m"}
	plan := feedPlan{nextDue: 20 * time.Minute}

	t.Run("rate limit wins: exact, unjittered, ignores nextDue", func(t *testing.T) {
		d := plan.requeue(spec, true, 90*time.Minute)
		if d.jitter || d.interval != "" || d.fallback != 90*time.Minute || d.nextDue != 0 {
			t.Fatalf("decision = %+v", d)
		}
		if got := d.bound(90 * time.Minute); got != 90*time.Minute {
			t.Fatalf("bound = %v, want the Retry-After untouched", got)
		}
	})
	t.Run("retry uses RetryInterval with jitter", func(t *testing.T) {
		d := plan.requeue(spec, true, 0)
		if !d.jitter || d.interval != "2m" || d.fallback != fallbackRetryInterval {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("no retry uses Interval with jitter", func(t *testing.T) {
		d := plan.requeue(spec, false, 0)
		if !d.jitter || d.interval != "1h" || d.fallback != fallbackInterval {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("webhook failure waits RetryInterval", func(t *testing.T) {
		if d := retryRequeue(spec); !d.jitter || d.interval != "2m" || d.fallback != fallbackRetryInterval || d.nextDue != 0 {
			t.Fatalf("decision = %+v", d)
		}
	})
}

func TestRequeueDecision_Bound(t *testing.T) {
	tests := []struct {
		name    string
		nextDue time.Duration
		wait    time.Duration
		want    time.Duration
	}{
		{"no skipped feed", 0, time.Hour, time.Hour},
		{"nextDue shortens the wait", 20 * time.Minute, time.Hour, 20 * time.Minute},
		{"a shorter wait is kept", 20 * time.Minute, 5 * time.Minute, 5 * time.Minute},
		{"equal wait is kept", 20 * time.Minute, 20 * time.Minute, 20 * time.Minute},
		{"floored at the minimum interval", time.Second, time.Hour, minParsedInterval},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := (requeueDecision{nextDue: tc.nextDue}).bound(tc.wait); got != tc.want {
				t.Errorf("bound(%v) with nextDue %v = %v, want %v", tc.wait, tc.nextDue, got, tc.want)
			}
		})
	}
}

// Backoff only floors a permanently failing feed's delay: it adds no wake time,
// so with a group Interval longer than the backoff the effective retry is the
// first group wake after expiry. Pinned so a change to that is deliberate.
func TestBackoffAddsNoWakeTime(t *testing.T) {
	backed := v1alpha1.FeedStatus{
		RSSUrl: "backed", LastChecked: stamp(-time.Hour), LastError: "404", RetryCount: 1,
		BackoffUntil: stamp(10 * time.Minute),
	}
	plan := planFeeds(scheduleGroup(defaultInterval, backed), scheduleNow, false)
	d := plan.requeue(&v1alpha1.FeedGroupSpec{Interval: defaultInterval, RetryInterval: "5m"}, false, 0)
	if len(plan.active) != 0 || d.bound(30*time.Minute) != 30*time.Minute {
		t.Fatalf("active=%v wait=%v, want no fetch and the full 30m Interval", activeURLs(plan), d.bound(30*time.Minute))
	}
}

// wait parses the interval (falling back when it is empty or unparsable),
// jitters it only when asked, and then applies the nextDue bound.
func TestRequeueDecisionWait(t *testing.T) {
	const (
		waitInterval    = "20m"
		waitIntervalDur = 20 * time.Minute
		waitFallback    = 7 * time.Minute
		waitShort       = time.Minute
	)
	for _, tc := range []struct {
		name     string
		decision requeueDecision
		want     time.Duration
	}{
		{name: "parsed interval", decision: requeueDecision{interval: waitInterval, fallback: waitShort}, want: waitIntervalDur},
		{name: "empty interval uses fallback", decision: requeueDecision{fallback: waitFallback}, want: waitFallback},
		{name: "unparsable interval uses fallback", decision: requeueDecision{interval: "bogus", fallback: waitFallback}, want: waitFallback},
		{name: "sub-floor interval is floored", decision: requeueDecision{interval: "2s", fallback: time.Hour}, want: minParsedInterval},
		{name: "bounded by nextDue", decision: requeueDecision{interval: waitInterval, fallback: waitShort, nextDue: 3 * time.Minute}, want: 3 * time.Minute},
		{name: "nextDue never below the floor", decision: requeueDecision{interval: waitInterval, fallback: waitShort, nextDue: time.Second}, want: minParsedInterval},
		{name: "nextDue longer than the wait is ignored", decision: requeueDecision{fallback: waitShort, nextDue: time.Hour}, want: waitShort},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.decision.wait(); got != tc.want {
				t.Fatalf("wait = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("jitter only lengthens", func(t *testing.T) {
		base := waitIntervalDur
		for range 50 {
			got := (requeueDecision{interval: waitInterval, jitter: true}).wait()
			if got < base || got > base+time.Duration(float64(base)*requeueJitterMaxFactor) {
				t.Fatalf("wait = %v, want within [%v, +%.0f%%]", got, base, requeueJitterMaxFactor*100)
			}
		}
	})
}

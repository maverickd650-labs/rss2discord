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
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/maverickd650-labs/rss2discord/api/v1beta1"
	"github.com/maverickd650-labs/rss2discord/internal/discord"
	"github.com/maverickd650-labs/rss2discord/internal/rss"
)

const (
	passFeedA = "https://example.com/feed-a"
	passFeedB = "https://example.com/feed-b"
)

// urlFetcher is a FeedFetcher that answers every feed with the same result and
// records which URLs were fetched. fetchAll calls it from concurrent
// goroutines, so the record is guarded; read it only after reconcilePass
// returns.
type urlFetcher struct {
	result rss.FetchResult

	mu      sync.Mutex
	fetched []string
}

func (f *urlFetcher) FetchEntries(_ context.Context, url string, _ rss.CacheValidators) (rss.FetchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetched = append(f.fetched, url)
	return f.result, nil
}

// passGroup is a two-feed FeedGroup (feed-a, feed-b) with ensured statuses.
func passGroup(t *testing.T, name string) *v1beta1.FeedGroup {
	t.Helper()
	fg, _ := newMetricsFeedGroup("reconcile-pass", name, tmplTitle)
	forgetMetricsAfter(t, fg.Namespace, fg.Name)
	fg.Spec.Feeds = []v1beta1.FeedSpec{
		{Name: testFeedName(passFeedA), URL: passFeedA, Message: &v1beta1.MessageSpec{Content: tmplTitle}},
		{Name: testFeedName(passFeedB), URL: passFeedB, Message: &v1beta1.MessageSpec{Content: tmplTitle}},
	}
	ensureFeedStatuses(fg)
	return fg
}

var passNow = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// A Discord rate limit stops the fold: every feed shares the webhook, so the
// feeds after the 429 are not attempted, and the verdict carries the wait.
func TestReconcilePass_RateLimitStopsLaterFeeds(t *testing.T) {
	fg := passGroup(t, "fg-ratelimit")
	sender := &fakeSender{results: []error{&discord.RateLimitError{RetryAfter: 7 * time.Second}}}

	verdict, plan := reconcilePass(t.Context(), fg, &urlFetcher{result: oneEntryFetch()}, sender, nil,
		cadenceOf(&fg.Spec), passNow, false)

	if verdict.retryAfter != 7*time.Second || !verdict.retry {
		t.Fatalf("verdict = %+v, want a retry after 7s", verdict)
	}
	if len(sender.sent) != 1 {
		t.Fatalf("sender called %d times, want 1 (the second feed must not be attempted)", len(sender.sent))
	}
	if a := feedStatusFor(fg, testFeedName(passFeedA)); a.LastError == "" || a.LastChecked == nil {
		t.Errorf("feed-a status = %+v, want the rate limit recorded", a)
	}
	if b := feedStatusFor(fg, testFeedName(passFeedB)); b.LastChecked != nil || b.LastError != "" || b.RetryCount != 0 {
		t.Errorf("feed-b status = %+v, want untouched", b)
	}
	if len(plan.active) != 2 {
		t.Errorf("plan.active = %d feeds, want both planned", len(plan.active))
	}

	r := &FeedGroupReconciler{Client: fakeApplyClient()}
	res, err := r.openGroupSync(fg).settlePass(t.Context(), plan, verdict)
	if err != nil {
		t.Fatalf("settlePass error: %v", err)
	}
	if res.RequeueAfter != 7*time.Second {
		t.Errorf("settlePass RequeueAfter = %v, want 7s", res.RequeueAfter)
	}
}

func TestReconcilePass_WithoutRateLimitEveryFeedIsProcessed(t *testing.T) {
	fg := passGroup(t, "fg-all")
	sender := &fakeSender{}
	fetcher := &urlFetcher{result: oneEntryFetch()}

	verdict, _ := reconcilePass(t.Context(), fg, fetcher, sender, nil, cadenceOf(&fg.Spec), passNow, false)

	if verdict != (attemptVerdict{}) {
		t.Fatalf("verdict = %+v, want none", verdict)
	}
	if len(sender.sent) != 2 || len(fetcher.fetched) != 2 {
		t.Fatalf("sent %d, fetched %d, want 2 each", len(sender.sent), len(fetcher.fetched))
	}
	if got := feedStatusFor(fg, testFeedName(passFeedB)).LastChecked; got == nil || !got.Time.Equal(passNow) {
		t.Errorf("LastChecked = %v, want the pass's now", got)
	}
}

// Which feeds a pass fetches: a not-yet-due feed is skipped (and bounds the
// wake time) unless force is set; a feed in permanent backoff is skipped even
// when forced by the caller.
func TestReconcilePass_PlansWhichFeedsAreFetched(t *testing.T) {
	recent := metav1TimeRFC3339(passNow.Add(-time.Minute).Format(time.RFC3339))
	tests := []struct {
		name        string
		force       bool
		prepare     func(a, b *v1beta1.FeedStatus)
		mutate      func(fg *v1beta1.FeedGroup)
		wantFetched []string
		wantNextDue bool
	}{
		{name: "never checked feeds are fetched", wantFetched: []string{passFeedA, passFeedB}},
		{
			name: "a paused feed is skipped",
			mutate: func(fg *v1beta1.FeedGroup) {
				fg.Spec.Feeds[0].Suspend = true
			},
			wantFetched: []string{passFeedB},
		},
		{
			name: "recently checked feed waits for its interval",
			prepare: func(a, _ *v1beta1.FeedStatus) {
				a.LastChecked = recent
			},
			wantFetched: []string{passFeedB}, wantNextDue: true,
		},
		{
			name: "force fetches a recently checked feed",
			prepare: func(a, _ *v1beta1.FeedStatus) {
				a.LastChecked = recent
			},
			force:       true,
			wantFetched: []string{passFeedA, passFeedB},
		},
		{
			name: "a backed-off feed is skipped",
			prepare: func(_, b *v1beta1.FeedStatus) {
				b.LastError, b.RetryCount = "404", 1
				b.BackoffUntil = metav1TimeRFC3339(passNow.Add(time.Hour).Format(time.RFC3339))
			},
			force:       true,
			wantFetched: []string{passFeedA},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fg := passGroup(t, "fg-plan")
			if tc.prepare != nil {
				tc.prepare(feedStatusFor(fg, testFeedName(passFeedA)), feedStatusFor(fg, testFeedName(passFeedB)))
			}
			if tc.mutate != nil {
				tc.mutate(fg)
			}
			fetcher := &urlFetcher{result: rss.FetchResult{}}

			_, plan := reconcilePass(t.Context(), fg, fetcher, &fakeSender{}, nil, cadenceOf(&fg.Spec), passNow, tc.force)

			slices.Sort(fetcher.fetched)
			if !slices.Equal(fetcher.fetched, tc.wantFetched) {
				t.Errorf("fetched = %v, want %v", fetcher.fetched, tc.wantFetched)
			}
			if (plan.nextDue > 0) != tc.wantNextDue {
				t.Errorf("nextDue = %v, want a wake time: %v", plan.nextDue, tc.wantNextDue)
			}
		})
	}
}

// ensureFeedStatuses rebuilds Status.Feeds to the spec and reports the names it
// pruned, so openGroupSync can drop their metric series.
func TestEnsureFeedStatuses_ReturnsPrunedNames(t *testing.T) {
	fg := passGroup(t, "fg-prune")
	fg.Status.Feeds = append(fg.Status.Feeds, v1beta1.FeedStatus{Name: testFeedName("https://example.com/gone"), URL: "https://example.com/gone"})

	pruned := ensureFeedStatuses(fg)

	if !slices.Equal(pruned, []string{testFeedName("https://example.com/gone")}) {
		t.Fatalf("pruned = %v, want the removed feed", pruned)
	}
	if len(fg.Status.Feeds) != 2 || fg.Status.Feeds[0].URL != passFeedA || fg.Status.Feeds[1].URL != passFeedB {
		t.Fatalf("Status.Feeds = %+v, want the spec's feeds in order", fg.Status.Feeds)
	}
}

// A spec change (generation ahead of the observed one) resets permanent
// backoff for every feed; an unchanged spec leaves it alone. The snapshot is
// taken after the reset and is independent of later status changes.
func TestOpenGroupSync_SpecChangeClearsPermanentBackoff(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		generation, observed  int64
		wantChanged, wantKept bool
	}{
		{name: "spec changed", generation: 2, observed: 1, wantChanged: true},
		{name: "spec unchanged", generation: 2, observed: 2, wantKept: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fg := passGroup(t, "fg-backoff")
			fg.Generation, fg.Status.ObservedGeneration = tc.generation, tc.observed
			until := metav1TimeRFC3339(passNow.Add(time.Hour).Format(time.RFC3339))
			fs := feedStatusFor(fg, testFeedName(passFeedA))
			fs.LastError, fs.RetryCount, fs.BackoffUntil = "404", 4, until

			sync := (&FeedGroupReconciler{}).openGroupSync(fg)

			if sync.specChanged != tc.wantChanged {
				t.Errorf("specChanged = %v, want %v", sync.specChanged, tc.wantChanged)
			}
			got := feedStatusFor(fg, testFeedName(passFeedA))
			if kept := got.BackoffUntil.Equal(until) && got.RetryCount == 4; kept != tc.wantKept {
				t.Errorf("backoff kept = %v (BackoffUntil %v, RetryCount %d), want %v", kept, got.BackoffUntil, got.RetryCount, tc.wantKept)
			}
			snap := sync.original.Feeds[0]
			if !timePtrEqual(snap.BackoffUntil, got.BackoffUntil) || snap.RetryCount != got.RetryCount {
				t.Errorf("snapshot = %+v, want the post-reset status", snap)
			}
			got.RetryCount = 99
			if sync.original.Feeds[0].RetryCount == 99 {
				t.Error("snapshot aliases the live status")
			}
		})
	}
}

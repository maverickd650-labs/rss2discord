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

	"github.com/maverickd650-labs/rss2discord/api/v1alpha1"
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
func passGroup(t *testing.T, name string) *v1alpha1.FeedGroup {
	t.Helper()
	fg, _ := newMetricsFeedGroup("reconcile-pass", name, "{{.Title}}")
	forgetMetricsAfter(t, fg.Namespace, fg.Name)
	fg.Spec.Feeds = []v1alpha1.FeedSpec{
		{RSSUrl: passFeedA, Format: "{{.Title}}"},
		{RSSUrl: passFeedB, Format: "{{.Title}}"},
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
	if a := feedStatusFor(fg, passFeedA); a.LastError == "" || a.LastChecked == "" {
		t.Errorf("feed-a status = %+v, want the rate limit recorded", a)
	}
	if b := feedStatusFor(fg, passFeedB); b.LastChecked != "" || b.LastError != "" || b.RetryCount != 0 {
		t.Errorf("feed-b status = %+v, want untouched", b)
	}
	if len(plan.active) != 2 {
		t.Errorf("plan.active = %d feeds, want both planned", len(plan.active))
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
	if got := feedStatusFor(fg, passFeedB).LastChecked; got != passNow.Format(time.RFC3339) {
		t.Errorf("LastChecked = %q, want the pass's now", got)
	}
}

// Which feeds a pass fetches: a not-yet-due feed is skipped (and bounds the
// wake time) unless force is set; a feed in permanent backoff is skipped even
// when forced by the caller.
func TestReconcilePass_PlansWhichFeedsAreFetched(t *testing.T) {
	recent := passNow.Add(-time.Minute).Format(time.RFC3339)
	tests := []struct {
		name        string
		force       bool
		prepare     func(a, b *v1alpha1.FeedStatus)
		wantFetched []string
		wantNextDue bool
	}{
		{name: "never checked feeds are fetched", wantFetched: []string{passFeedA, passFeedB}},
		{
			name: "recently checked feed waits for its interval",
			prepare: func(a, _ *v1alpha1.FeedStatus) {
				a.LastChecked = recent
			},
			wantFetched: []string{passFeedB}, wantNextDue: true,
		},
		{
			name: "force fetches a recently checked feed",
			prepare: func(a, _ *v1alpha1.FeedStatus) {
				a.LastChecked = recent
			},
			force:       true,
			wantFetched: []string{passFeedA, passFeedB},
		},
		{
			name: "a backed-off feed is skipped",
			prepare: func(_, b *v1alpha1.FeedStatus) {
				b.LastError, b.RetryCount = "404", 1
				b.BackoffUntil = passNow.Add(time.Hour).Format(time.RFC3339)
			},
			force:       true,
			wantFetched: []string{passFeedA},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fg := passGroup(t, "fg-plan")
			if tc.prepare != nil {
				tc.prepare(feedStatusFor(fg, passFeedA), feedStatusFor(fg, passFeedB))
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

// prepareStatus rebuilds Status.Feeds to the spec and reports the URLs it
// pruned, so Reconcile can drop their metric series.
func TestPrepareStatus_ReturnsPrunedURLs(t *testing.T) {
	fg := passGroup(t, "fg-prune")
	fg.Status.Feeds = append(fg.Status.Feeds, v1alpha1.FeedStatus{RSSUrl: "https://example.com/gone"})

	prep := prepareStatus(fg)

	if !slices.Equal(prep.pruned, []string{"https://example.com/gone"}) {
		t.Fatalf("pruned = %v, want the removed feed", prep.pruned)
	}
	if len(fg.Status.Feeds) != 2 || fg.Status.Feeds[0].RSSUrl != passFeedA || fg.Status.Feeds[1].RSSUrl != passFeedB {
		t.Fatalf("Status.Feeds = %+v, want the spec's feeds in order", fg.Status.Feeds)
	}
}

// A spec change (generation ahead of the observed one) resets permanent
// backoff for every feed; an unchanged spec leaves it alone. The snapshot is
// taken after the reset and is independent of later status changes.
func TestPrepareStatus_SpecChangeClearsPermanentBackoff(t *testing.T) {
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
			until := passNow.Add(time.Hour).Format(time.RFC3339)
			fs := feedStatusFor(fg, passFeedA)
			fs.LastError, fs.RetryCount, fs.BackoffUntil = "404", 4, until

			prep := prepareStatus(fg)

			if prep.specChanged != tc.wantChanged {
				t.Errorf("specChanged = %v, want %v", prep.specChanged, tc.wantChanged)
			}
			got := feedStatusFor(fg, passFeedA)
			if kept := got.BackoffUntil == until && got.RetryCount == 4; kept != tc.wantKept {
				t.Errorf("backoff kept = %v (BackoffUntil %q, RetryCount %d), want %v", kept, got.BackoffUntil, got.RetryCount, tc.wantKept)
			}
			snap := prep.snapshot.Feeds[0]
			if snap.BackoffUntil != got.BackoffUntil || snap.RetryCount != got.RetryCount {
				t.Errorf("snapshot = %+v, want the post-reset status", snap)
			}
			got.RetryCount = 99
			if prep.snapshot.Feeds[0].RetryCount == 99 {
				t.Error("snapshot aliases the live status")
			}
		})
	}
}

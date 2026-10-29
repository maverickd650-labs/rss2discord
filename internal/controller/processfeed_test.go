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
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	v1alpha1 "github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/rss"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
)

// testReconcileTime is the fixed "now" string handed to processFeed in tests.
const testReconcileTime = "2026-01-01T00:00:00Z"

// TestProcessFeed_InvalidFilterRegex asserts a feed with an unparsable
// filter regex records the compile error on the feed's status and sends
// nothing, instead of panicking or silently matching everything.
func TestProcessFeed_InvalidFilterRegex(t *testing.T) {
	ctx := t.Context()
	ns, name := "processfeed-bad-regex", "fg-bad-regex"
	defer deleteFeedGroupMetrics(ns, name)

	discordServer := NewMockDiscordServer()
	defer discordServer.Close()

	fg, feed := newMetricsFeedGroup(ns, name, "")
	feed.Filter = &v1alpha1.Filter{Regex: "("}
	fg.Spec.Feeds[0] = feed
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	wantRetry, rateLimitRetryAfter := (&FeedGroupReconciler{}).processFeed(
		ctx, fg, feed, oneEntryFetch(), nil, client, testReconcileTime)

	if wantRetry {
		t.Fatal("expected no retry for a deterministic regex compile error")
	}
	if rateLimitRetryAfter != 0 {
		t.Fatalf("expected no rate-limit backoff, got %v", rateLimitRetryAfter)
	}
	if feedStatusFor(fg, feed.RSSUrl).LastError == "" {
		t.Fatal("expected LastError to be set for the invalid filter regex")
	}
	if discordServer.MessageCount() != 0 {
		t.Fatalf("expected no message sent, got %d", discordServer.MessageCount())
	}
}

// TestProcessFeed_FeedTitleReachesTemplate asserts the title the fetch
// returned for the feed is what {{.FeedTitle}} renders for its entries.
func TestProcessFeed_FeedTitleReachesTemplate(t *testing.T) {
	ctx := t.Context()
	ns, name := "processfeed-feed-title", "fg-feed-title"
	defer deleteFeedGroupMetrics(ns, name)

	discordServer := NewMockDiscordServer()
	defer discordServer.Close()

	fg, feed := newMetricsFeedGroup(ns, name, "[{{.FeedTitle}}] {{.Title}}")
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	fetchResult := oneEntryFetch()
	fetchResult.FeedTitle = "Example Blog"
	(&FeedGroupReconciler{}).processFeed(ctx, fg, feed, fetchResult, nil, client, testReconcileTime)

	bodies := discordServer.Bodies()
	if len(bodies) != 1 || !strings.Contains(bodies[0], "[Example Blog] ") {
		t.Fatalf("expected one message carrying the feed title, got %q", bodies)
	}
}

// TestProcessFeed_InvalidMessageTemplate asserts a feed whose Format fails
// to parse as a template is not retried -- like the filter-regex case above,
// a template compile error is a deterministic FeedSpec misconfiguration that
// won't resolve itself on the normal/retry schedule, so retrying would just
// pin the group at RetryInterval cadence forever -- and is recorded on
// status.
func TestProcessFeed_InvalidMessageTemplate(t *testing.T) {
	ctx := t.Context()
	ns, name := "processfeed-bad-template", "fg-bad-template"
	defer deleteFeedGroupMetrics(ns, name)

	discordServer := NewMockDiscordServer()
	defer discordServer.Close()

	fg, feed := newMetricsFeedGroup(ns, name, "{{.Unclosed")
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	wantRetry, _ := (&FeedGroupReconciler{}).processFeed(
		ctx, fg, feed, oneEntryFetch(), nil, client, testReconcileTime)

	if wantRetry {
		t.Fatal("expected no retry for a deterministic message template compile error")
	}
	if feedStatusFor(fg, feed.RSSUrl).LastError == "" {
		t.Fatal("expected LastError to be set for the invalid template")
	}
	if discordServer.MessageCount() != 0 {
		t.Fatalf("expected no message sent, got %d", discordServer.MessageCount())
	}
}

// TestProcessFeed_InvalidForumThreadNameTemplate asserts a feed with a
// valid message Format but an unparsable ForumThreadName template is not
// retried and the error is recorded, exercising the thread-name compile
// branch distinct from the message-template branch above.
func TestProcessFeed_InvalidForumThreadNameTemplate(t *testing.T) {
	ctx := t.Context()
	ns, name := "processfeed-bad-thread-name", "fg-bad-thread-name"
	defer deleteFeedGroupMetrics(ns, name)

	discordServer := NewMockDiscordServer()
	defer discordServer.Close()

	fg, feed := newMetricsFeedGroup(ns, name, "")
	feed.ForumThreadName = "{{.Unclosed"
	fg.Spec.Feeds[0] = feed
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	wantRetry, _ := (&FeedGroupReconciler{}).processFeed(
		ctx, fg, feed, oneEntryFetch(), nil, client, testReconcileTime)

	if wantRetry {
		t.Fatal("expected no retry for a deterministic forum thread name template compile error")
	}
	if feedStatusFor(fg, feed.RSSUrl).LastError == "" {
		t.Fatal("expected LastError to be set for the invalid forum thread name template")
	}
	if discordServer.MessageCount() != 0 {
		t.Fatalf("expected no message sent, got %d", discordServer.MessageCount())
	}
}

// TestProcessFeed_NotModifiedPersistsLastModified asserts a 304 response
// carrying a Last-Modified validator (rather than an ETag) is still stored,
// since only the ETag persist path had previously been exercised.
func TestProcessFeed_NotModifiedPersistsLastModified(t *testing.T) {
	ctx := t.Context()
	ns, name := "processfeed-not-modified-lm", "fg-not-modified-lm"
	defer deleteFeedGroupMetrics(ns, name)

	fg, feed := newMetricsFeedGroup(ns, name, "")
	fetchResult := rss.FetchResult{NotModified: true, LastModified: "Fri, 23 Oct 2015 07:28:00 GMT"}

	wantRetry, _ := (&FeedGroupReconciler{}).processFeed(
		ctx, fg, feed, fetchResult, nil, nil, testReconcileTime)

	if wantRetry {
		t.Fatal("expected no retry on a 304 response")
	}
	if got := feedStatusFor(fg, feed.RSSUrl).LastModified; got != fetchResult.LastModified {
		t.Fatalf("LastModified[%q] = %q, want %q", feed.RSSUrl, got, fetchResult.LastModified)
	}
}

// TestProcessFeed_LastCheckedTracksSuccessNotAttempts asserts LastChecked
// advances on a successful check -- including a 304 -- but not on a failed
// fetch, so it reads as "last time we confirmed this feed's state" rather
// than "last time we attempted to reach it".
func TestProcessFeed_LastCheckedTracksSuccessNotAttempts(t *testing.T) {
	ctx := t.Context()
	ns, name := "processfeed-lastchecked", "fg-lastchecked"
	defer deleteFeedGroupMetrics(ns, name)

	fg, feed := newMetricsFeedGroup(ns, name, "")

	(&FeedGroupReconciler{}).processFeed(
		ctx, fg, feed, rss.FetchResult{}, errors.New("boom"), nil, testReconcileTime)
	if got := feedStatusFor(fg, feed.RSSUrl).LastChecked; got != "" {
		t.Fatal("expected LastChecked not to be set after a fetch error")
	}

	(&FeedGroupReconciler{}).processFeed(
		ctx, fg, feed, rss.FetchResult{NotModified: true}, nil, nil, testReconcileTime)
	if got := feedStatusFor(fg, feed.RSSUrl).LastChecked; got != testReconcileTime {
		t.Fatalf("LastChecked after a 304 = %q, want it set to the check time", got)
	}
}

// TestProcessFeed_AlreadySentEntrySkipped asserts an entry already recorded
// in LastSent is not re-delivered, exercising the dedup short-circuit
// independently of any filter/template path.
func TestProcessFeed_AlreadySentEntrySkipped(t *testing.T) {
	ctx := t.Context()
	ns, name := "processfeed-dedup", "fg-dedup"
	defer deleteFeedGroupMetrics(ns, name)

	discordServer := NewMockDiscordServer()
	defer discordServer.Close()

	fg, feed := newMetricsFeedGroup(ns, name, "")
	fetchResult := oneEntryFetch()
	entryKey := computeEntryKey(fetchResult.Entries[0])
	feedStatusFor(fg, feed.RSSUrl).LastSent = map[string]string{entryKey: "2025-12-31T00:00:00Z"}
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	wantRetry, _ := (&FeedGroupReconciler{}).processFeed(
		ctx, fg, feed, fetchResult, nil, client, testReconcileTime)

	if wantRetry {
		t.Fatal("expected no retry for an already-sent entry")
	}
	if discordServer.MessageCount() != 0 {
		t.Fatalf("expected the already-sent entry to be skipped, got %d messages", discordServer.MessageCount())
	}
}

// TestProcessFeed_RecoversFromStaleErrorWithNoNewEntries asserts a feed that
// previously failed (LastError/RetryCount/BackoffUntil set) but now fetches
// successfully with only already-sent/filtered entries -- so nothing is
// pending -- has its error state cleared. Without this, a feed that recovers
// but happens to have nothing new to deliver would show a stale error (and
// keep the FeedGroup's Ready condition False) indefinitely, since neither
// the 304 nor the empty-entries branch runs for a fetch that did return
// entries, all of which were already handled.
func TestProcessFeed_RecoversFromStaleErrorWithNoNewEntries(t *testing.T) {
	ctx := t.Context()
	ns, name := "processfeed-recovers", "fg-recovers"
	defer deleteFeedGroupMetrics(ns, name)

	discordServer := NewMockDiscordServer()
	defer discordServer.Close()

	fg, feed := newMetricsFeedGroup(ns, name, "")
	fetchResult := oneEntryFetch()
	entryKey := computeEntryKey(fetchResult.Entries[0])
	fs := feedStatusFor(fg, feed.RSSUrl)
	fs.LastSent = map[string]string{entryKey: "2025-12-31T00:00:00Z"}
	fs.LastError = "previous attempt: connection refused"
	fs.RetryCount = 3
	fs.BackoffUntil = "2025-12-31T01:00:00Z"
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	wantRetry, _ := (&FeedGroupReconciler{}).processFeed(
		ctx, fg, feed, fetchResult, nil, client, testReconcileTime)

	if wantRetry {
		t.Fatal("expected no retry once the feed recovers")
	}
	got := feedStatusFor(fg, feed.RSSUrl)
	if got.LastError != "" {
		t.Fatalf("expected LastError to clear, got %q", got.LastError)
	}
	if got.RetryCount != 0 {
		t.Fatalf("expected RetryCount to reset, got %d", got.RetryCount)
	}
	if got.BackoffUntil != "" {
		t.Fatalf("expected BackoffUntil to clear, got %q", got.BackoffUntil)
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

// TestFeedInBackoff verifies the active-feeds skip predicate: empty or
// malformed BackoffUntil values are treated as "not in backoff" so a bad
// value can never permanently wedge a feed, and the comparison is a strict
// "now before until".
func TestFeedInBackoff(t *testing.T) {
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
			if got := feedInBackoff(tc.backoffUntil, now); got != tc.want {
				t.Errorf("feedInBackoff(%q, %v) = %v, want %v", tc.backoffUntil, now, got, tc.want)
			}
		})
	}
}

// TestNextBackoffUntil verifies the RFC3339 formatting of a computed backoff
// expiry, and that the result round-trips through feedInBackoff as expected.
func TestNextBackoffUntil(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	got := nextBackoffUntil(now, 10*time.Minute)
	want := "2026-01-01T00:10:00Z"
	if got != want {
		t.Fatalf("nextBackoffUntil() = %q, want %q", got, want)
	}
	if !feedInBackoff(got, now) {
		t.Fatalf("feedInBackoff(%q, %v) = false, want true (still in the future)", got, now)
	}
	if feedInBackoff(got, now.Add(10*time.Minute)) {
		t.Fatalf("feedInBackoff(%q, %v) = true, want false (backoff expired)", got, now.Add(10*time.Minute))
	}
}

// TestProcessFeed_PermanentFetchFailureSetsBackoff asserts a permanent fetch
// error (HTTP 404) sets BackoffUntil to an exponential offset, returns no
// wantRetry (the group's normal interval is unaffected), and does not set a
// capped backoff on the first few retries.
func TestProcessFeed_PermanentFetchFailureSetsBackoff(t *testing.T) {
	ctx := t.Context()
	ns, name := "processfeed-perm-backoff", "fg-perm-backoff"
	defer deleteFeedGroupMetrics(ns, name)

	fg, feed := newMetricsFeedGroup(ns, name, "")
	fg.Spec.RetryInterval = "5m"
	notFoundErr := &rss.HTTPStatusError{StatusCode: 404, Status: http404Status}

	before := time.Now().UTC()
	wantRetry, rateLimitRetryAfter := (&FeedGroupReconciler{}).processFeed(
		ctx, fg, feed, rss.FetchResult{}, notFoundErr, nil, testReconcileTime)
	after := time.Now().UTC()

	if wantRetry {
		t.Fatal("expected no group-level retry for a permanent fetch failure")
	}
	if rateLimitRetryAfter != 0 {
		t.Fatalf("expected no rate-limit backoff, got %v", rateLimitRetryAfter)
	}

	fs := feedStatusFor(fg, feed.RSSUrl)
	if fs.BackoffUntil == "" {
		t.Fatal("expected BackoffUntil to be set after a permanent failure")
	}
	until, err := time.Parse(time.RFC3339, fs.BackoffUntil)
	if err != nil {
		t.Fatalf("BackoffUntil %q is not RFC3339: %v", fs.BackoffUntil, err)
	}
	// RetryCount is 1 after first failure; backoff = 5m * 2^1 = 10m.
	// RFC3339 truncates to seconds, so expand the window by one second on
	// each side to avoid spurious failures when the wall clock sits near a
	// second boundary.
	wantMin := before.Add(10 * time.Minute).Truncate(time.Second)
	wantMax := after.Add(10 * time.Minute).Add(time.Second)
	if until.Before(wantMin) || until.After(wantMax) {
		t.Errorf("BackoffUntil %v outside expected window [%v, %v]", until, wantMin, wantMax)
	}
}

// TestProcessFeed_BlockedAddressSetsPermanentBackoff asserts a feed whose URL
// the SSRF guard refuses (a permanent misconfiguration) is classified
// BlockedAddress and put into exponential backoff instead of retrying at the
// flat RetryInterval forever.
func TestProcessFeed_BlockedAddressSetsPermanentBackoff(t *testing.T) {
	ctx := t.Context()
	ns, name := "processfeed-blocked-addr", "fg-blocked-addr"
	defer deleteFeedGroupMetrics(ns, name)

	fg, feed := newMetricsFeedGroup(ns, name, "")
	fg.Spec.RetryInterval = "5m"
	blockedErr := doError(&rss.BlockedAddressError{Addr: netip.MustParseAddr("169.254.169.254")}, true)

	wantRetry, rateLimitRetryAfter := (&FeedGroupReconciler{}).processFeed(
		ctx, fg, feed, rss.FetchResult{}, blockedErr, nil, testReconcileTime)

	if wantRetry {
		t.Fatal("expected no group-level retry for a permanent fetch failure")
	}
	if rateLimitRetryAfter != 0 {
		t.Fatalf("expected no rate-limit backoff, got %v", rateLimitRetryAfter)
	}

	fs := feedStatusFor(fg, feed.RSSUrl)
	if fs.BackoffUntil == "" {
		t.Fatal("expected BackoffUntil to be set for a blocked address")
	}
	cond := apimeta.FindStatusCondition(fs.Conditions, v1alpha1.FeedConditionTypeReachable)
	if cond == nil || cond.Reason != reasonBlockedAddress {
		t.Fatalf("Reachable condition = %+v, want reason %q", cond, reasonBlockedAddress)
	}
}

// TestProcessFeed_PermanentFetchFailureCapKeepsRetrying asserts that once the
// exponential backoff exceeds 6h it is capped at maxPermanentBackoff (the feed
// stays retryable rather than being disabled), the Warning Event fires the
// first time the cap is hit, and does not fire again on later capped retries.
func TestProcessFeed_PermanentFetchFailureCapKeepsRetrying(t *testing.T) {
	ctx := t.Context()
	ns, name := "processfeed-perm-cap", "fg-perm-cap"
	defer deleteFeedGroupMetrics(ns, name)

	fg, feed := newMetricsFeedGroup(ns, name, "")
	fg.Spec.RetryInterval = "5m"
	// RetryCount=6 means after the increment it becomes 7, and
	// 5m * 2^7 = 640m > 6h, which is the first capped retry.
	feedStatusFor(fg, feed.RSSUrl).RetryCount = 6
	notFoundErr := &rss.HTTPStatusError{StatusCode: 404, Status: http404Status}

	recorder := events.NewFakeRecorder(10)
	r := &FeedGroupReconciler{Recorder: recorder}
	before := time.Now().UTC()
	wantRetry, _ := r.processFeed(ctx, fg, feed, rss.FetchResult{}, notFoundErr, nil, testReconcileTime)

	if wantRetry {
		t.Fatal("expected no group-level retry for a capped permanent failure")
	}
	fs := feedStatusFor(fg, feed.RSSUrl)
	until, err := time.Parse(time.RFC3339, fs.BackoffUntil)
	if err != nil {
		t.Fatalf("BackoffUntil %q is not RFC3339: %v", fs.BackoffUntil, err)
	}
	if got := until.Sub(before); got < maxPermanentBackoff-time.Second || got > maxPermanentBackoff+time.Minute {
		t.Errorf("BackoffUntil is %v out, want ~%v", got, maxPermanentBackoff)
	}
	select {
	case <-recorder.Events:
	default:
		t.Fatal("expected Warning Event the first time the cap is reached")
	}

	// A further capped retry keeps the cap but does not re-fire the Event.
	r.processFeed(ctx, fg, feed, rss.FetchResult{}, notFoundErr, nil, testReconcileTime)
	select {
	case ev := <-recorder.Events:
		t.Fatalf("unexpected repeat Event: %s", ev)
	default:
	}
	if feedInBackoff(feedStatusFor(fg, feed.RSSUrl).BackoffUntil, time.Now().UTC().Add(maxPermanentBackoff+time.Minute)) {
		t.Error("feed should become retryable again once the capped backoff elapses")
	}
}

// TestFeedDue covers the per-feed poll-interval gate.
func TestFeedDue(t *testing.T) {
	const recentCheck = "2026-01-01T11:59:00Z"
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	interval := 30 * time.Minute
	cases := []struct {
		name  string
		fs    v1alpha1.FeedStatus
		force bool
		want  bool
	}{
		{name: "never checked", want: true},
		{name: "checked recently", fs: v1alpha1.FeedStatus{LastChecked: "2026-01-01T11:45:00Z"}, want: false},
		{name: "interval elapsed", fs: v1alpha1.FeedStatus{LastChecked: "2026-01-01T11:30:00Z"}, want: true},
		{name: "malformed LastChecked", fs: v1alpha1.FeedStatus{LastChecked: "garbage"}, want: true},
		{name: "recent but failing", fs: v1alpha1.FeedStatus{LastChecked: recentCheck, LastError: "boom"}, want: true},
		{name: "recent but retrying", fs: v1alpha1.FeedStatus{LastChecked: recentCheck, RetryCount: 1}, want: true},
		{name: "recent but spec changed", fs: v1alpha1.FeedStatus{LastChecked: recentCheck}, force: true, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := feedDue(&tc.fs, interval, now, tc.force); got != tc.want {
				t.Errorf("feedDue() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestProcessFeed_PermanentFailureClearedOnSuccess asserts that a successful
// fetch (including a 304) clears BackoffUntil set by a previous permanent
// failure.
func TestProcessFeed_PermanentFailureClearedOnSuccess(t *testing.T) {
	ctx := t.Context()
	ns, name := "processfeed-perm-clear", "fg-perm-clear"
	defer deleteFeedGroupMetrics(ns, name)

	fg, feed := newMetricsFeedGroup(ns, name, "")
	// Simulate a feed that was previously in permanent backoff.
	feedStatusFor(fg, feed.RSSUrl).BackoffUntil = "2030-01-01T00:00:00Z"
	feedStatusFor(fg, feed.RSSUrl).RetryCount = 3

	(&FeedGroupReconciler{}).processFeed(
		ctx, fg, feed, rss.FetchResult{NotModified: true}, nil, nil, testReconcileTime)

	fs := feedStatusFor(fg, feed.RSSUrl)
	if fs.BackoffUntil != "" {
		t.Errorf("BackoffUntil = %q, want empty after successful check", fs.BackoffUntil)
	}
	if fs.RetryCount != 0 {
		t.Errorf("RetryCount = %d, want 0 after successful check", fs.RetryCount)
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

// TestProcessFeed_RenderErrorSkipsEntryAfterRetriesExhausted asserts that an
// entry that deterministically fails to render blocks its feed only while
// retries last: once RetryCount has passed the limit it is skipped (recorded
// as handled, with an Event and the skipped outcome) and a later entry in the
// same feed is still delivered.
func TestProcessFeed_RenderErrorSkipsEntryAfterRetriesExhausted(t *testing.T) {
	ctx := t.Context()
	ns, name := "processfeed-skip-render", "fg-skip-render"
	defer deleteFeedGroupMetrics(ns, name)

	discordServer := NewMockDiscordServer()
	defer discordServer.Close()
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	// {{.Title.Nope}} fails at execution time (it indexes into a string); the
	// guard makes only the "bad" entry hit it, so the other one renders.
	fg, feed := newMetricsFeedGroup(ns, name, `{{if eq .Title "bad"}}{{.Title.Nope}}{{else}}ok {{.Title}}{{end}}`)
	fg.Spec.Retries = 2
	fetch := rss.FetchResult{Entries: []rss.Entry{
		{ID: "https://example.com/bad", Title: "bad", Seq: 1},
		{ID: "https://example.com/good", Title: "good", Seq: 0},
	}}
	recorder := events.NewFakeRecorder(10)
	r := &FeedGroupReconciler{Recorder: recorder}

	// Attempts 1 and 2 exhaust the retries and hold the feed on the bad entry.
	for range 2 {
		r.processFeed(ctx, fg, feed, fetch, nil, client, testReconcileTime)
	}
	fs := feedStatusFor(fg, feed.RSSUrl)
	if fs.LastSeenEntry != "" || discordServer.MessageCount() != 0 {
		t.Fatalf("entries must stay held while retries remain: seen=%q sent=%d", fs.LastSeenEntry, discordServer.MessageCount())
	}
	for len(recorder.Events) > 0 {
		<-recorder.Events
	}

	// The next reconcile gives up on the bad entry and sends the good one.
	r.processFeed(ctx, fg, feed, fetch, nil, client, testReconcileTime)

	if got := discordServer.MessageCount(); got != 1 {
		t.Fatalf("expected the entry behind the poison one to be sent, got %d messages", got)
	}
	fs = feedStatusFor(fg, feed.RSSUrl)
	if _, ok := fs.LastSent[computeEntryKey(fetch.Entries[0])]; !ok {
		t.Error("skipped entry should be recorded in LastSent so it isn't retried")
	}
	if fs.LastSeenEntry != "https://example.com/good" || fs.RetryCount != 0 || fs.LastError != "" {
		t.Errorf("unexpected status after skip+send: %+v", fs)
	}
	select {
	case ev := <-recorder.Events:
		if !strings.Contains(ev, reasonEntrySkipped) {
			t.Errorf("event %q should name %s", ev, reasonEntrySkipped)
		}
	default:
		t.Error("expected a SkippedEntry Event")
	}
}

// TestProcessFeed_BrokenTemplateNeverSkipsEntries asserts that when the
// template fails for every entry (a typo'd field, not bad content), retries
// being exhausted does not drain the feed into LastSent: the entries stay
// pending so fixing the template still delivers them.
func TestProcessFeed_BrokenTemplateNeverSkipsEntries(t *testing.T) {
	ctx := t.Context()
	ns, name := "processfeed-broken-template", "fg-broken-template"
	defer deleteFeedGroupMetrics(ns, name)

	discordServer := NewMockDiscordServer()
	defer discordServer.Close()
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	fg, feed := newMetricsFeedGroup(ns, name, "{{.Title.Nope}}") // fails for every entry
	fg.Spec.Retries = 1
	fetch := rss.FetchResult{Entries: []rss.Entry{
		{ID: "https://example.com/entry-a", Title: "a", Seq: 1},
		{ID: "https://example.com/entry-b", Title: "b", Seq: 0},
	}}
	fs := feedStatusFor(fg, feed.RSSUrl)
	fs.RetryCount = 5 // far past the retry limit

	recorder := events.NewFakeRecorder(10)
	(&FeedGroupReconciler{Recorder: recorder}).processFeed(ctx, fg, feed, fetch, nil, client, testReconcileTime)

	if len(fs.LastSent) != 0 || fs.LastSeenEntry != "" {
		t.Errorf("no entry may be marked handled when the template is broken for all: LastSent=%v seen=%q", fs.LastSent, fs.LastSeenEntry)
	}
	for len(recorder.Events) > 0 {
		if ev := <-recorder.Events; strings.Contains(ev, reasonEntrySkipped) {
			t.Errorf("unexpected skip event: %s", ev)
		}
	}
}

func TestClampText(t *testing.T) {
	if got := clampText(shortContent, 10); got != shortContent {
		t.Errorf("clampText(short) = %q", got)
	}
	long := strings.Repeat("é", 100) // 2 bytes per rune
	// Budgets 50-51 land the cut mid-rune for one parity and on a boundary
	// for the other, so both branches of the UTF-8 backoff run.
	for _, budget := range []int{50, 51} {
		got := clampText(long, budget)
		if len(got) > budget || !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
			t.Errorf("clampText(budget=%d) split a rune or overshot: len=%d valid=%v %q", budget, len(got), utf8.ValidString(got), got)
		}
	}
}

// TestEnsureFeedStatuses_NormalizesOversizedStoredValues asserts values
// persisted before the status fields were bounded are brought within the
// CRD limits on load (an oversized one would be rejected on every write),
// while the long watermark still matches the entry it was derived from.
func TestEnsureFeedStatuses_NormalizesOversizedStoredValues(t *testing.T) {
	longID := "https://example.com/" + strings.Repeat("p", 4000)
	fg, feed := newMetricsFeedGroup("normalize-ns", "normalize", "")
	fs := feedStatusFor(fg, feed.RSSUrl)
	fs.LastError = strings.Repeat("e", 30000)
	fs.LastSeenEntry = longID

	ensureFeedStatuses(fg)

	fs = feedStatusFor(fg, feed.RSSUrl)
	if len(fs.LastError) > maxStatusTextBytes {
		t.Errorf("LastError is %d bytes after load, want <= %d", len(fs.LastError), maxStatusTextBytes)
	}
	if len(fs.LastSeenEntry) > maxStatusTextBytes {
		t.Errorf("LastSeenEntry is %d bytes after load, want <= %d", len(fs.LastSeenEntry), maxStatusTextBytes)
	}
	if !entriesContainID([]rss.Entry{{ID: longID}}, fs.LastSeenEntry) {
		t.Error("normalized watermark no longer matches the entry it was derived from")
	}
	// A watermark freshly stored for the same entry is the same value.
	if fresh := clampWatermark(entryIdentity(rss.Entry{ID: longID})); fresh != fs.LastSeenEntry {
		t.Errorf("fresh watermark %q != normalized legacy watermark %q", fresh, fs.LastSeenEntry)
	}
}

// TestFeedGroupStatusWorstCaseStaysUnderBudget fills every bounded status
// field to its cap for a maximal (50-feed) group and checks the serialized
// status leaves room in etcd's ~1.5MB object limit for the spec.
func TestFeedGroupStatusWorstCaseStaysUnderBudget(t *testing.T) {
	var status v1alpha1.FeedGroupStatus
	for i := range 50 {
		fs := v1alpha1.FeedStatus{
			RSSUrl:        "https://example.com/" + strings.Repeat("u", 2048-21) + strconv.Itoa(i%10),
			LastChecked:   strings.Repeat("t", 64),
			LastSeenEntry: clampWatermark(strings.Repeat("s", 5000)),
			LastError:     clampText(strings.Repeat("e", 40000), maxStatusTextBytes),
			ETag:          strings.Repeat("g", 256),
			LastModified:  strings.Repeat("m", 64),
			BackoffUntil:  strings.Repeat("b", 64),
			LastSent:      map[string]string{},
		}
		for j := range maxLastSentPerFeed {
			fs.LastSent[hashIdentity(strconv.Itoa(j))] = testReconcileTime
		}
		for _, typ := range []string{v1alpha1.FeedConditionTypeReachable, v1alpha1.FeedConditionTypeDelivered} {
			setFeedCondition(&fs, typ, metav1.ConditionFalse, strings.Repeat("R", 64), strings.Repeat("m", 40000), 1)
		}
		status.Feeds = append(status.Feeds, fs)
	}
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	const budget = 800 * 1024
	if len(raw) > budget {
		t.Fatalf("worst-case status is %d bytes, over the %d budget", len(raw), budget)
	}
	t.Logf("worst-case status: %d bytes", len(raw))
}

// TestRecordConfigError_EventFiresOncePerDistinctError asserts the
// InvalidConfig Warning Event is emitted on the reconcile a config error first
// appears -- and not again while the same error persists -- since a
// deterministic misconfiguration has no RetryCount to gate it on.
func TestRecordConfigError_EventFiresOncePerDistinctError(t *testing.T) {
	fg, feed := newMetricsFeedGroup("config-error-ns", "config-error", "")
	fs := feedStatusFor(fg, feed.RSSUrl)
	recorder := events.NewFakeRecorder(10)
	r := &FeedGroupReconciler{Recorder: recorder}

	r.recordConfigError(fg, fs, feed.RSSUrl, errors.New("bad regex"))
	select {
	case ev := <-recorder.Events:
		if !strings.Contains(ev, reasonConfigError) || !strings.Contains(ev, "bad regex") {
			t.Errorf("event %q should name %s and the cause", ev, reasonConfigError)
		}
	default:
		t.Fatal("expected an Event the first time the config error appears")
	}

	r.recordConfigError(fg, fs, feed.RSSUrl, errors.New("bad regex"))
	select {
	case ev := <-recorder.Events:
		t.Fatalf("unexpected repeat Event for an unchanged error: %s", ev)
	default:
	}

	r.recordConfigError(fg, fs, feed.RSSUrl, errors.New("a different typo"))
	select {
	case <-recorder.Events:
	default:
		t.Error("a changed error message should fire a fresh Event")
	}
}

// With catchUpLimit 0, only a true first sync skips the
// backlog. A watermark that scrolled out of the feed's window falls back to
// the default cap instead, so genuinely new entries aren't silently dropped.
func TestProcessFeed_CatchUpLimitZeroOnlySkipsOnFirstSync(t *testing.T) {
	ctx := t.Context()

	run := func(t *testing.T, lastSeen string, limit *int32) (sent int, fs *v1alpha1.FeedStatus) {
		t.Helper()
		ns, name := "skip-backlog-unit", "fg-skip-"+lastSeen
		defer deleteFeedGroupMetrics(ns, name)
		discordServer := NewMockDiscordServer()
		defer discordServer.Close()

		fg, feed := newMetricsFeedGroup(ns, name, "")
		fg.Spec.CatchUpLimit = limit
		fg.Status.Feeds[0].LastSeenEntry = lastSeen
		client := discordServer.DiscordClientBuilder()(discordServer.URL())
		(&FeedGroupReconciler{}).processFeed(ctx, fg, feed, oneEntryFetch(), nil, client, "2026-01-01T00:00:00Z")
		return discordServer.MessageCount(), &fg.Status.Feeds[0]
	}

	t.Run("first sync sends nothing and sets the watermark", func(t *testing.T) {
		sent, fs := run(t, "", new(int32(0)))
		if sent != 0 || fs.LastSeenEntry != "https://example.com/article" {
			t.Fatalf("sent=%d watermark=%q", sent, fs.LastSeenEntry)
		}
	})
	t.Run("scrolled-out watermark still delivers", func(t *testing.T) {
		if sent, _ := run(t, "gone", new(int32(0))); sent != 1 {
			t.Fatalf("sent=%d, want 1", sent)
		}
	})
	t.Run("omitted limit delivers the backlog", func(t *testing.T) {
		if sent, _ := run(t, "", nil); sent != 1 {
			t.Fatalf("sent=%d, want 1", sent)
		}
	})
}

func TestProcessFeed_Initialization(t *testing.T) {
	for _, tc := range []struct {
		name        string
		result      rss.FetchResult
		fetchErr    error
		initialized bool
		wantSent    int
	}{
		{name: "empty fetch", initialized: true, wantSent: 1},
		{name: "not modified", result: rss.FetchResult{NotModified: true}, initialized: true, wantSent: 1},
		{name: "fetch failure", fetchErr: errors.New("fetch failed"), wantSent: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns, name := "initialization-unit", tc.name
			defer deleteFeedGroupMetrics(ns, name)
			discordServer := NewMockDiscordServer()
			defer discordServer.Close()

			fg, feed := newMetricsFeedGroup(ns, name, "")
			fg.Spec.CatchUpLimit = new(int32(0))
			client := discordServer.DiscordClientBuilder()(discordServer.URL())
			r := &FeedGroupReconciler{}
			r.processFeed(t.Context(), fg, feed, tc.result, tc.fetchErr, client, testReconcileTime)
			fs := feedStatusFor(fg, feed.RSSUrl)
			if fs.Initialized != tc.initialized || fs.LastSeenEntry != "" {
				t.Fatalf("initialized=%t watermark=%q", fs.Initialized, fs.LastSeenEntry)
			}

			r.processFeed(t.Context(), fg, feed, oneEntryFetch(), nil, client, testReconcileTime)
			if got := discordServer.MessageCount(); got != tc.wantSent {
				t.Fatalf("sent=%d, want %d", got, tc.wantSent)
			}
		})
	}
}

// After an empty first fetch with catchUpLimit 0, everything that shows up
// later is new, so none of it may be trimmed by the catch-up cap.
func TestProcessFeed_CatchUpLimitZeroDeliversAllAfterEmptyFirstSync(t *testing.T) {
	ns, name := "initialization-unit", "all-after-empty"
	defer deleteFeedGroupMetrics(ns, name)
	discordServer := NewMockDiscordServer()
	defer discordServer.Close()

	fg, feed := newMetricsFeedGroup(ns, name, "")
	fg.Spec.CatchUpLimit = new(int32(0))
	client := discordServer.DiscordClientBuilder()(discordServer.URL())
	r := &FeedGroupReconciler{}
	r.processFeed(t.Context(), fg, feed, rss.FetchResult{}, nil, client, testReconcileTime)

	const total = 2*defaultCatchUpLimit + 1
	entries := make([]rss.Entry, 0, total)
	for i := range total {
		id := fmt.Sprintf("https://example.com/new-%d", i)
		entries = append(entries, rss.Entry{ID: id, Title: id, Link: id, Seq: total - i})
	}
	r.processFeed(t.Context(), fg, feed, rss.FetchResult{Entries: entries}, nil, client, testReconcileTime)
	if got := discordServer.MessageCount(); got != total {
		t.Fatalf("sent=%d, want all %d", got, total)
	}
}

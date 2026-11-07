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
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/maverickd650-labs/rss2discord/api/v1beta1"
	"github.com/maverickd650-labs/rss2discord/internal/discord"
	"github.com/maverickd650-labs/rss2discord/internal/rss"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
)

// TestProcessFeed_NotModifiedPersistsLastModified asserts a 304 response
// carrying a Last-Modified validator (rather than an ETag) is still stored,
// since only the ETag persist path had previously been exercised.
func TestProcessFeed_NotModifiedPersistsLastModified(t *testing.T) {
	ctx := t.Context()
	ns, name := "processfeed-not-modified-lm", "fg-not-modified-lm"

	fg, feed := newMetricsFeedGroup(ns, name, "")
	fetchResult := rss.FetchResult{NotModified: true, LastModified: "Fri, 23 Oct 2015 07:28:00 GMT"}

	verdict := processFeed(ctx, testRun(t, fg, feed, fetchResult, nil, nil))
	wantRetry := verdict.retry

	if wantRetry {
		t.Fatal("expected no retry on a 304 response")
	}
	if got := feedStatusFor(fg, feed.Name).LastModified; got != fetchResult.LastModified {
		t.Fatalf("LastModified[%q] = %q, want %q", feed.URL, got, fetchResult.LastModified)
	}
}

// TestProcessFeed_LastCheckedTracksSuccessNotAttempts asserts LastChecked
// advances on a successful check -- including a 304 -- but not on a failed
// fetch, so it reads as "last time we confirmed this feed's state" rather
// than "last time we attempted to reach it".
func TestProcessFeed_LastCheckedTracksSuccessNotAttempts(t *testing.T) {
	ctx := t.Context()
	ns, name := "processfeed-lastchecked", "fg-lastchecked"

	fg, feed := newMetricsFeedGroup(ns, name, "")

	processFeed(ctx, testRun(t, fg, feed, rss.FetchResult{}, errors.New("boom"), nil))
	if got := feedStatusFor(fg, feed.Name).LastChecked; got != nil {
		t.Fatal("expected LastChecked not to be set after a fetch error")
	}

	processFeed(ctx, testRun(t, fg, feed, rss.FetchResult{NotModified: true}, nil, nil))
	if got := feedStatusFor(fg, feed.Name).LastChecked; got == nil || !got.Time.Equal(testReconcileInstant) {
		t.Fatalf("LastChecked after a 304 = %v, want it set to the check time", got)
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

	discordServer := NewMockDiscordServer(t)
	defer discordServer.Close()

	fg, feed := newMetricsFeedGroup(ns, name, "")
	fetchResult := oneEntryFetch()
	entryKey := computeEntryKey(fetchResult.Entries[0])
	fs := feedStatusFor(fg, feed.Name)
	fs.LastSent = map[string]metav1.Time{entryKey: ts("2025-12-31T00:00:00Z")}
	fs.LastError = "previous attempt: connection refused"
	fs.RetryCount = 3
	fs.BackoffUntil = metav1TimeRFC3339("2025-12-31T01:00:00Z")
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	verdict := processFeed(ctx, testRun(t, fg, feed, fetchResult, nil, client))
	wantRetry := verdict.retry

	if wantRetry {
		t.Fatal("expected no retry once the feed recovers")
	}
	got := feedStatusFor(fg, feed.Name)
	if got.LastError != "" {
		t.Fatalf("expected LastError to clear, got %q", got.LastError)
	}
	if got.RetryCount != 0 {
		t.Fatalf("expected RetryCount to reset, got %d", got.RetryCount)
	}
	if got.BackoffUntil != nil {
		t.Fatalf("expected BackoffUntil to clear, got %v", got.BackoffUntil)
	}
}

// TestProcessFeed_PermanentFetchFailureSetsBackoff asserts a permanent fetch
// error (HTTP 404) sets BackoffUntil to an exponential offset, returns no
// wantRetry (the group's normal interval is unaffected), and does not set a
// capped backoff on the first few retries.
func TestProcessFeed_PermanentFetchFailureSetsBackoff(t *testing.T) {
	ctx := t.Context()
	ns, name := "processfeed-perm-backoff", "fg-perm-backoff"

	fg, feed := newMetricsFeedGroup(ns, name, "")
	fg.Spec.RetryInterval = "5m"
	notFoundErr := &rss.HTTPStatusError{StatusCode: 404, Status: http404Status}

	verdict := processFeed(ctx, testRun(t, fg, feed, rss.FetchResult{}, notFoundErr, nil))
	wantRetry, rateLimitRetryAfter := verdict.retry, verdict.retryAfter

	if wantRetry {
		t.Fatal("expected no group-level retry for a permanent fetch failure")
	}
	if rateLimitRetryAfter != 0 {
		t.Fatalf("expected no rate-limit backoff, got %v", rateLimitRetryAfter)
	}

	fs := feedStatusFor(fg, feed.Name)
	if fs.BackoffUntil == nil {
		t.Fatal("expected BackoffUntil to be set after a permanent failure")
	}
	// RetryCount is 1 after first failure; backoff = 5m * 2^1 = 10m from the
	// run's clock.
	if want := nextBackoffUntil(testReconcileInstant, 10*time.Minute); !fs.BackoffUntil.Time.Equal(want.Time) {
		t.Errorf("BackoffUntil = %v, want %v", fs.BackoffUntil, want)
	}
}

// TestProcessFeed_BlockedAddressSetsPermanentBackoff asserts a feed whose URL
// the SSRF guard refuses (a permanent misconfiguration) is classified
// BlockedAddress and put into exponential backoff instead of retrying at the
// flat RetryInterval forever.
func TestProcessFeed_BlockedAddressSetsPermanentBackoff(t *testing.T) {
	ctx := t.Context()
	ns, name := "processfeed-blocked-addr", "fg-blocked-addr"

	fg, feed := newMetricsFeedGroup(ns, name, "")
	fg.Spec.RetryInterval = "5m"
	blockedErr := doError(&rss.BlockedAddressError{Addr: netip.MustParseAddr("169.254.169.254")}, true)

	verdict := processFeed(ctx, testRun(t, fg, feed, rss.FetchResult{}, blockedErr, nil))
	wantRetry, rateLimitRetryAfter := verdict.retry, verdict.retryAfter

	if wantRetry {
		t.Fatal("expected no group-level retry for a permanent fetch failure")
	}
	if rateLimitRetryAfter != 0 {
		t.Fatalf("expected no rate-limit backoff, got %v", rateLimitRetryAfter)
	}

	fs := feedStatusFor(fg, feed.Name)
	if fs.BackoffUntil == nil {
		t.Fatal("expected BackoffUntil to be set for a blocked address")
	}
	cond := apimeta.FindStatusCondition(fs.Conditions, v1beta1.FeedConditionTypeReachable)
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

	fg, feed := newMetricsFeedGroup(ns, name, "")
	fg.Spec.RetryInterval = "5m"
	// RetryCount=6 means after the increment it becomes 7, and
	// 5m * 2^7 = 640m > 6h, which is the first capped retry.
	feedStatusFor(fg, feed.Name).RetryCount = 6
	notFoundErr := &rss.HTTPStatusError{StatusCode: 404, Status: http404Status}

	recorder := events.NewFakeRecorder(10)
	verdict := processFeed(ctx, testRun(t, fg, feed, rss.FetchResult{}, notFoundErr, nil).withRecorder(recorder))
	wantRetry := verdict.retry

	if wantRetry {
		t.Fatal("expected no group-level retry for a capped permanent failure")
	}
	fs := feedStatusFor(fg, feed.Name)
	if want := nextBackoffUntil(testReconcileInstant, maxPermanentBackoff); !fs.BackoffUntil.Time.Equal(want.Time) {
		t.Errorf("BackoffUntil = %v, want %v", fs.BackoffUntil, want)
	}
	select {
	case <-recorder.Events:
	default:
		t.Fatal("expected Warning Event the first time the cap is reached")
	}

	// A further capped retry keeps the cap but does not re-fire the Event.
	processFeed(ctx, testRun(t, fg, feed, rss.FetchResult{}, notFoundErr, nil).withRecorder(recorder))
	select {
	case ev := <-recorder.Events:
		t.Fatalf("unexpected repeat Event: %s", ev)
	default:
	}
	capped := retryStateOf(feedStatusFor(fg, feed.Name))
	if !capped.active(testReconcileInstant.Add(maxPermanentBackoff - time.Minute)) {
		t.Error("feed should stay in backoff until the cap elapses")
	}
	if capped.active(testReconcileInstant.Add(maxPermanentBackoff + time.Minute)) {
		t.Error("feed should become retryable again once the capped backoff elapses")
	}
}

// TestProcessFeed_PermanentFailureClearedOnSuccess asserts that a successful
// fetch (including a 304) clears BackoffUntil set by a previous permanent
// failure.
func TestProcessFeed_PermanentFailureClearedOnSuccess(t *testing.T) {
	ctx := t.Context()
	ns, name := "processfeed-perm-clear", "fg-perm-clear"

	fg, feed := newMetricsFeedGroup(ns, name, "")
	// Simulate a feed that was previously in permanent backoff.
	feedStatusFor(fg, feed.Name).BackoffUntil = metav1TimeRFC3339("2030-01-01T00:00:00Z")
	feedStatusFor(fg, feed.Name).RetryCount = 3

	processFeed(ctx, testRun(t, fg, feed, rss.FetchResult{NotModified: true}, nil, nil))

	fs := feedStatusFor(fg, feed.Name)
	if fs.BackoffUntil != nil {
		t.Errorf("BackoffUntil = %v, want nil after successful check", fs.BackoffUntil)
	}
	if fs.RetryCount != 0 {
		t.Errorf("RetryCount = %d, want 0 after successful check", fs.RetryCount)
	}
}

// failFetchRun drives n failed fetches of the feed through processFeed, which
// leaves a fetch-phase retry run: Reachable False, RetryCount n and LastError.
func (f feedUnderTest) failFetchRun(t *testing.T, n int, fetchErr error) {
	t.Helper()
	for range n {
		processFeed(t.Context(), testRun(t, f.fg, f.feed, rss.FetchResult{}, fetchErr, nil))
	}
	fs := feedStatusFor(f.fg, f.feed.Name)
	if !apimeta.IsStatusConditionFalse(fs.Conditions, v1beta1.FeedConditionTypeReachable) ||
		fs.RetryCount != int32(n) || fs.LastError == "" {
		t.Fatalf("precondition: want a fetch-phase failure run of %d, got %+v", n, fs)
	}
}

// TestProcessFeed_FetchRecoveryStartsDeliveryRetriesFresh asserts a successful
// fetch ends a fetch-phase retry run: the first render failure afterwards is
// that entry's first retry, so the entry is held (not skipped as though its
// retries were already spent), a retry is requested, and no exhaustion Event
// fires early.
func TestProcessFeed_FetchRecoveryStartsDeliveryRetriesFresh(t *testing.T) {
	ns, name := "processfeed-fetch-then-render", "fg-fetch-then-render"

	discordServer := NewMockDiscordServer(t)
	defer discordServer.Close()
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	fg, feed := newMetricsFeedGroup(ns, name, `{{if eq .Title "bad"}}{{.Title.Nope}}{{else}}ok {{.Title}}{{end}}`)
	fg.Spec.Retries = new(int32(3))
	fetch := rss.FetchResult{Entries: []rss.Entry{
		{ID: "https://example.com/bad", Title: "bad", Seq: 1},
		{ID: "https://example.com/ok-entry", Title: "fine", Seq: 0},
	}}
	feedUnderTest{fg, feed}.failFetchRun(t, 5, errors.New("connection reset by peer"))

	recorder := events.NewFakeRecorder(10)
	verdict := processFeed(t.Context(), testRun(t, fg, feed, fetch, nil, client).withRecorder(recorder))

	fs := feedStatusFor(fg, feed.Name)
	if fs.RetryCount != 1 {
		t.Errorf("RetryCount = %d, want 1: the render failure is the first of a new run", fs.RetryCount)
	}
	if !verdict.retry {
		t.Error("verdict.retry = false, want a retry for the first render failure")
	}
	if len(fs.LastSent) != 0 || fs.LastSeenEntry != "" || discordServer.MessageCount() != 0 {
		t.Errorf("the bad entry must be held, not skipped: LastSent=%v seen=%q sent=%d",
			fs.LastSent, fs.LastSeenEntry, discordServer.MessageCount())
	}
	for len(recorder.Events) > 0 {
		if ev := <-recorder.Events; strings.Contains(ev, reasonEntrySkipped) {
			t.Errorf("unexpected skip event: %s", ev)
		}
	}
}

// TestProcessFeed_FetchRecoveryBacksOffSendFailureFromOne asserts a permanent
// send failure after a fetch-phase run backs off from RetryCount 1, not from
// the count the fetch failures left behind.
func TestProcessFeed_FetchRecoveryBacksOffSendFailureFromOne(t *testing.T) {
	ns, name := "processfeed-fetch-then-send", "fg-fetch-then-send"

	fg, feed := newMetricsFeedGroup(ns, name, "")
	fg.Spec.RetryInterval = "5m"
	feedUnderTest{fg, feed}.failFetchRun(t, 5, errors.New("connection reset by peer"))

	sender := &fakeSender{results: []error{&discord.HTTPStatusError{StatusCode: 400, Status: "400 Bad Request"}}}
	processFeed(t.Context(), testRun(t, fg, feed, oneEntryFetch(), nil, sender))

	fs := feedStatusFor(fg, feed.Name)
	if fs.RetryCount != 1 {
		t.Errorf("RetryCount = %d, want 1", fs.RetryCount)
	}
	if want := nextBackoffUntil(testReconcileInstant, 10*time.Minute); !fs.BackoffUntil.Time.Equal(want.Time) {
		t.Errorf("BackoffUntil = %v, want %v (5m * 2^1)", fs.BackoffUntil, want)
	}
}

// TestProcessFeed_DeliveryRunSurvivesSuccessfulFetches pins the other half: a
// render-failure run belongs to the delivery phase, so the successful fetch
// that precedes each retry must not reset it, or it could never exhaust.
func TestProcessFeed_DeliveryRunSurvivesSuccessfulFetches(t *testing.T) {
	ns, name := "processfeed-delivery-run", "fg-delivery-run"

	fg, feed := newMetricsFeedGroup(ns, name, "{{.Title.Nope}}")
	fg.Spec.Retries = new(int32(10))
	fetch := rss.FetchResult{Entries: []rss.Entry{{ID: "https://example.com/only-entry", Title: "only"}}}

	for want := int32(1); want <= 3; want++ {
		processFeed(t.Context(), testRun(t, fg, feed, fetch, nil, &fakeSender{}))
		if got := feedStatusFor(fg, feed.Name).RetryCount; got != want {
			t.Fatalf("after render failure %d: RetryCount = %d, want %d", want, got, want)
		}
	}
}

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
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/discord"
	"github.com/maverickd650/rss2discord-operator/internal/rss"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
)

// testReconcileTime is the fixed "now" string handed to processFeed in tests.
const testReconcileTime = "2026-01-01T00:00:00Z"

// testReconcileInstant is testReconcileTime as the time processFeed runs at.
var testReconcileInstant = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// TestProcessFeed_InvalidFilterRegex asserts a feed with an unparsable
// filter regex records the compile error on the feed's status and sends
// nothing, instead of panicking or silently matching everything.
func TestProcessFeed_InvalidFilterRegex(t *testing.T) {
	ctx := t.Context()
	ns, name := "processfeed-bad-regex", "fg-bad-regex"

	discordServer := NewMockDiscordServer(t)
	defer discordServer.Close()

	fg, feed := newMetricsFeedGroup(ns, name, "")
	feed.Filter = &v1alpha1.Filter{Regex: "("}
	fg.Spec.Feeds[0] = feed
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	verdict := processFeed(ctx, testRun(t, fg, feed, oneEntryFetch(), nil, client))
	wantRetry, rateLimitRetryAfter := verdict.retry, verdict.retryAfter

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

	discordServer := NewMockDiscordServer(t)
	defer discordServer.Close()

	fg, feed := newMetricsFeedGroup(ns, name, "[{{.FeedTitle}}] {{.Title}}")
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	fetchResult := oneEntryFetch()
	fetchResult.FeedTitle = "Example Blog"
	processFeed(ctx, testRun(t, fg, feed, fetchResult, nil, client))

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

	discordServer := NewMockDiscordServer(t)
	defer discordServer.Close()

	fg, feed := newMetricsFeedGroup(ns, name, "{{.Unclosed")
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	verdict := processFeed(ctx, testRun(t, fg, feed, oneEntryFetch(), nil, client))
	wantRetry := verdict.retry

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

	discordServer := NewMockDiscordServer(t)
	defer discordServer.Close()

	fg, feed := newMetricsFeedGroup(ns, name, "")
	feed.ForumThreadName = "{{.Unclosed"
	fg.Spec.Feeds[0] = feed
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	verdict := processFeed(ctx, testRun(t, fg, feed, oneEntryFetch(), nil, client))
	wantRetry := verdict.retry

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

	fg, feed := newMetricsFeedGroup(ns, name, "")
	fetchResult := rss.FetchResult{NotModified: true, LastModified: "Fri, 23 Oct 2015 07:28:00 GMT"}

	verdict := processFeed(ctx, testRun(t, fg, feed, fetchResult, nil, nil))
	wantRetry := verdict.retry

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

	fg, feed := newMetricsFeedGroup(ns, name, "")

	processFeed(ctx, testRun(t, fg, feed, rss.FetchResult{}, errors.New("boom"), nil))
	if got := feedStatusFor(fg, feed.RSSUrl).LastChecked; got != "" {
		t.Fatal("expected LastChecked not to be set after a fetch error")
	}

	processFeed(ctx, testRun(t, fg, feed, rss.FetchResult{NotModified: true}, nil, nil))
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

	discordServer := NewMockDiscordServer(t)
	defer discordServer.Close()

	fg, feed := newMetricsFeedGroup(ns, name, "")
	fetchResult := oneEntryFetch()
	entryKey := computeEntryKey(fetchResult.Entries[0])
	feedStatusFor(fg, feed.RSSUrl).LastSent = map[string]string{entryKey: "2025-12-31T00:00:00Z"}
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	verdict := processFeed(ctx, testRun(t, fg, feed, fetchResult, nil, client))
	wantRetry := verdict.retry

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

	discordServer := NewMockDiscordServer(t)
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

	verdict := processFeed(ctx, testRun(t, fg, feed, fetchResult, nil, client))
	wantRetry := verdict.retry

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

	fs := feedStatusFor(fg, feed.RSSUrl)
	if fs.BackoffUntil == "" {
		t.Fatal("expected BackoffUntil to be set after a permanent failure")
	}
	// RetryCount is 1 after first failure; backoff = 5m * 2^1 = 10m from the
	// run's clock.
	if want := nextBackoffUntil(testReconcileInstant, 10*time.Minute); fs.BackoffUntil != want {
		t.Errorf("BackoffUntil = %q, want %q", fs.BackoffUntil, want)
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

	fg, feed := newMetricsFeedGroup(ns, name, "")
	fg.Spec.RetryInterval = "5m"
	// RetryCount=6 means after the increment it becomes 7, and
	// 5m * 2^7 = 640m > 6h, which is the first capped retry.
	feedStatusFor(fg, feed.RSSUrl).RetryCount = 6
	notFoundErr := &rss.HTTPStatusError{StatusCode: 404, Status: http404Status}

	recorder := events.NewFakeRecorder(10)
	verdict := processFeed(ctx, testRun(t, fg, feed, rss.FetchResult{}, notFoundErr, nil).withRecorder(recorder))
	wantRetry := verdict.retry

	if wantRetry {
		t.Fatal("expected no group-level retry for a capped permanent failure")
	}
	fs := feedStatusFor(fg, feed.RSSUrl)
	if want := nextBackoffUntil(testReconcileInstant, maxPermanentBackoff); fs.BackoffUntil != want {
		t.Errorf("BackoffUntil = %q, want %q", fs.BackoffUntil, want)
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
	capped := runOf(feedStatusFor(fg, feed.RSSUrl))
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
	feedStatusFor(fg, feed.RSSUrl).BackoffUntil = "2030-01-01T00:00:00Z"
	feedStatusFor(fg, feed.RSSUrl).RetryCount = 3

	processFeed(ctx, testRun(t, fg, feed, rss.FetchResult{NotModified: true}, nil, nil))

	fs := feedStatusFor(fg, feed.RSSUrl)
	if fs.BackoffUntil != "" {
		t.Errorf("BackoffUntil = %q, want empty after successful check", fs.BackoffUntil)
	}
	if fs.RetryCount != 0 {
		t.Errorf("RetryCount = %d, want 0 after successful check", fs.RetryCount)
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

	discordServer := NewMockDiscordServer(t)
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

	// Attempts 1 and 2 exhaust the retries and hold the feed on the bad entry.
	for range 2 {
		processFeed(ctx, testRun(t, fg, feed, fetch, nil, client).withRecorder(recorder))
	}
	fs := feedStatusFor(fg, feed.RSSUrl)
	if fs.LastSeenEntry != "" || discordServer.MessageCount() != 0 {
		t.Fatalf("entries must stay held while retries remain: seen=%q sent=%d", fs.LastSeenEntry, discordServer.MessageCount())
	}
	for len(recorder.Events) > 0 {
		<-recorder.Events
	}

	// The next reconcile gives up on the bad entry and sends the good one.
	processFeed(ctx, testRun(t, fg, feed, fetch, nil, client).withRecorder(recorder))

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

	discordServer := NewMockDiscordServer(t)
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
	processFeed(ctx, testRun(t, fg, feed, fetch, nil, client).withRecorder(recorder))

	if len(fs.LastSent) != 0 || fs.LastSeenEntry != "" {
		t.Errorf("no entry may be marked handled when the template is broken for all: LastSent=%v seen=%q", fs.LastSent, fs.LastSeenEntry)
	}
	for len(recorder.Events) > 0 {
		if ev := <-recorder.Events; strings.Contains(ev, reasonEntrySkipped) {
			t.Errorf("unexpected skip event: %s", ev)
		}
	}
}

// TestRecordConfigError_EventFiresOncePerDistinctError asserts the
// InvalidConfig Warning Event is emitted on the reconcile a config error first
// appears -- and not again while the same error persists -- since a
// deterministic misconfiguration has no RetryCount to gate it on.
func TestRecordConfigError_EventFiresOncePerDistinctError(t *testing.T) {
	fg, feed := newMetricsFeedGroup("config-error-ns", "config-error", "")
	fs := feedStatusFor(fg, feed.RSSUrl)
	recorder := events.NewFakeRecorder(10)
	attempt := testAttempt(t, fg, fs, feed.RSSUrl, testReconcileInstant).withRecorder(recorder)
	configError := attemptOutcome{kind: attemptConfigError}

	attempt.record(configError, errors.New("bad regex"))
	select {
	case ev := <-recorder.Events:
		if !strings.Contains(ev, reasonConfigError) || !strings.Contains(ev, "bad regex") {
			t.Errorf("event %q should name %s and the cause", ev, reasonConfigError)
		}
	default:
		t.Fatal("expected an Event the first time the config error appears")
	}

	attempt.record(configError, errors.New("bad regex"))
	select {
	case ev := <-recorder.Events:
		t.Fatalf("unexpected repeat Event for an unchanged error: %s", ev)
	default:
	}

	attempt.record(configError, errors.New("a different typo"))
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
		discordServer := NewMockDiscordServer(t)
		defer discordServer.Close()

		fg, feed := newMetricsFeedGroup(ns, name, "")
		fg.Spec.CatchUpLimit = limit
		fg.Status.Feeds[0].LastSeenEntry = lastSeen
		client := discordServer.DiscordClientBuilder()(discordServer.URL())
		processFeed(ctx, testRun(t, fg, feed, oneEntryFetch(), nil, client))
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

// initializationUnitNS namespaces the metric series the initialization tests create.
const initializationUnitNS = "initialization-unit"

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
			ns, name := initializationUnitNS, tc.name
			discordServer := NewMockDiscordServer(t)
			defer discordServer.Close()

			fg, feed := newMetricsFeedGroup(ns, name, "")
			fg.Spec.CatchUpLimit = new(int32(0))
			client := discordServer.DiscordClientBuilder()(discordServer.URL())
			processFeed(t.Context(), testRun(t, fg, feed, tc.result, tc.fetchErr, client))
			fs := feedStatusFor(fg, feed.RSSUrl)
			if fs.Initialized != tc.initialized || fs.LastSeenEntry != "" {
				t.Fatalf("initialized=%t watermark=%q", fs.Initialized, fs.LastSeenEntry)
			}

			processFeed(t.Context(), testRun(t, fg, feed, oneEntryFetch(), nil, client))
			if got := discordServer.MessageCount(); got != tc.wantSent {
				t.Fatalf("sent=%d, want %d", got, tc.wantSent)
			}
		})
	}
}

// After an empty first fetch with catchUpLimit 0, everything that shows up
// later is new, so none of it may be trimmed by the catch-up cap.
func TestProcessFeed_CatchUpLimitZeroDeliversAllAfterEmptyFirstSync(t *testing.T) {
	ns, name := initializationUnitNS, "all-after-empty"
	discordServer := NewMockDiscordServer(t)
	defer discordServer.Close()

	fg, feed := newMetricsFeedGroup(ns, name, "")
	fg.Spec.CatchUpLimit = new(int32(0))
	client := discordServer.DiscordClientBuilder()(discordServer.URL())
	processFeed(t.Context(), testRun(t, fg, feed, rss.FetchResult{}, nil, client))

	const total = 2*defaultCatchUpLimit + 1
	entries := make([]rss.Entry, 0, total)
	for i := range total {
		id := fmt.Sprintf("https://example.com/new-%d", i)
		entries = append(entries, rss.Entry{ID: id, Title: id, Link: id, Seq: total - i})
	}
	processFeed(t.Context(), testRun(t, fg, feed, rss.FetchResult{Entries: entries}, nil, client))
	if got := discordServer.MessageCount(); got != total {
		t.Fatalf("sent=%d, want all %d", got, total)
	}
}

func TestProcessFeed_BacklogWatermarkSurvivesRestart(t *testing.T) {
	const (
		newestID = "newest"
		oldestID = "oldest"
		freshID  = "fresh"
	)
	for _, tc := range []struct {
		name     string
		dated    bool
		newestID string
	}{
		{name: "publication order", dated: true, newestID: newestID},
		{name: "undated document order", newestID: newestID},
		{name: "oversized identity", newestID: "guid-" + strings.Repeat("x", maxStatusTextBytes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fg, feed := newMetricsFeedGroup("backlog-restart", tc.name, "{{.Title}}")
			forgetMetricsAfter(t, fg.Namespace, fg.Name)
			fg.Generation = 7
			fg.Spec.CatchUpLimit = new(int32(0))
			fs := feedStatusFor(fg, feed.RSSUrl)
			fs.LastError, fs.RetryCount, fs.BackoffUntil = "old failure", 2, "2031-01-01T00:00:00Z"
			discordServer := NewMockDiscordServer(t)
			defer discordServer.Close()
			client := discordServer.DiscordClientBuilder()(discordServer.URL())
			entries := make([]rss.Entry, 2, 3)
			entries[0] = rss.Entry{ID: tc.newestID, Title: newestID, Seq: 0}
			entries[1] = rss.Entry{ID: oldestID, Title: oldestID, Seq: 1}
			if tc.dated {
				entries[0].Published = time.Unix(200, 0)
				entries[1].Published = time.Unix(100, 0)
				// Publication time takes priority over document position.
				entries[0].Seq, entries[1].Seq = 1, 0
			}
			fetch := rss.FetchResult{Entries: entries, ETag: `"skip-v1"`, LastModified: testReconcileTime}
			verdict := processFeed(t.Context(), testRun(t, fg, feed, fetch, nil, client))
			wantRetry, retryAfter := verdict.retry, verdict.retryAfter
			if wantRetry || retryAfter != 0 || discordServer.MessageCount() != 0 || len(fs.LastSent) != 0 {
				t.Fatalf("backlog was sent or retried: retry=%t after=%v sent=%d status=%+v", wantRetry, retryAfter, discordServer.MessageCount(), fs)
			}
			assertSkippedBacklogStatus(t, fg, fs, tc.newestID, fetch)

			// Reload status and replace the reconciler before checking the same
			// feed and then publishing an entry after the saved watermark.
			fg = fg.DeepCopy()
			ensureFeedStatuses(fg)
			processFeed(t.Context(), testRun(t, fg, feed, fetch, nil, client))
			if discordServer.MessageCount() != 0 {
				t.Fatal("reloading status caused the backlog to be sent")
			}
			for i := range entries {
				entries[i].Seq++
			}
			fresh := rss.Entry{ID: freshID, Title: freshID, Seq: 0}
			if tc.dated {
				fresh.Published = time.Unix(300, 0)
			}
			fetch.Entries = append(entries, fresh)
			processFeed(t.Context(), testRun(t, fg, feed, fetch, nil, client))
			if got := discordServer.Bodies(); len(got) != 1 || !strings.Contains(got[0], `"content":"fresh"`) {
				t.Fatalf("expected only the fresh entry, got %v", got)
			}
			fs = feedStatusFor(fg, feed.RSSUrl)
			condition := apimeta.FindStatusCondition(fs.Conditions, v1alpha1.FeedConditionTypeDelivered)
			if fs.LastSeenEntry != freshID || len(fs.LastSent) != 1 || condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason == reasonBacklogSkipped {
				t.Fatalf("delivery did not advance status: %+v", fs)
			}
			processFeed(t.Context(), testRun(t, fg, feed, fetch, nil, client))
			if discordServer.MessageCount() != 1 {
				t.Fatal("fresh entry was delivered twice")
			}
		})
	}
}

func assertSkippedBacklogStatus(t *testing.T, fg *v1alpha1.FeedGroup, fs *v1alpha1.FeedStatus, newestID string, fetch rss.FetchResult) {
	t.Helper()
	if !fs.Initialized || fs.LastChecked != testReconcileTime || fs.LastError != "" || fs.RetryCount != 0 || fs.BackoffUntil != "" ||
		fs.ETag != fetch.ETag || fs.LastModified != fetch.LastModified || len(fs.LastSeenEntry) > maxStatusTextBytes ||
		!ledgerResumes([]rss.Entry{{ID: newestID}}, fs.LastSeenEntry) {
		t.Fatalf("unexpected status after skipped backlog: %+v", fs)
	}
	condition := apimeta.FindStatusCondition(fs.Conditions, v1alpha1.FeedConditionTypeDelivered)
	if condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != reasonBacklogSkipped ||
		condition.ObservedGeneration != fg.Generation || !strings.Contains(condition.Message, "2 existing entries") {
		t.Fatalf("unexpected backlog condition: %+v", condition)
	}
}

func TestProcessFeed_CatchUpCapsAndLegacyWatermarks(t *testing.T) {
	for _, tc := range []struct {
		name        string
		limit       *int32
		lastSeen    string
		initialized bool
		wantStart   int
	}{
		{name: "omitted limit uses default", wantStart: 4},
		{name: "positive limit keeps most recent", limit: new(int32(3)), wantStart: 6},
		{name: "zero with missing watermark uses default cap", limit: new(int32(0)), lastSeen: scrolledOutWatermark, initialized: true, wantStart: 4},
		{name: "legacy missing watermark uses default cap", limit: new(int32(0)), lastSeen: scrolledOutWatermark, wantStart: 4},
		{name: "legacy matching watermark resumes", limit: new(int32(0)), lastSeen: "entry-1", wantStart: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fg, feed := newMetricsFeedGroup("backlog-caps", tc.name, "{{.Title}}")
			forgetMetricsAfter(t, fg.Namespace, fg.Name)
			fg.Spec.CatchUpLimit = tc.limit
			fs := feedStatusFor(fg, feed.RSSUrl)
			fs.Initialized, fs.LastSeenEntry = tc.initialized, tc.lastSeen
			discordServer := NewMockDiscordServer(t)
			defer discordServer.Close()
			client := discordServer.DiscordClientBuilder()(discordServer.URL())
			entries := make([]rss.Entry, 9)
			for i := range entries {
				id := fmt.Sprintf("entry-%d", i)
				entries[i] = rss.Entry{ID: id, Title: id, Seq: len(entries) - 1 - i}
			}
			verdict := processFeed(t.Context(), testRun(t, fg, feed, rss.FetchResult{Entries: entries}, nil, client))
			wantRetry, retryAfter := verdict.retry, verdict.retryAfter
			bodies := discordServer.Bodies()
			if wantRetry || retryAfter != 0 || len(bodies) != len(entries)-tc.wantStart {
				t.Fatalf("retry=%t after=%v sent=%d; want %d", wantRetry, retryAfter, len(bodies), len(entries)-tc.wantStart)
			}
			for i, body := range bodies {
				var payload struct {
					Content string `json:"content"`
				}
				if err := json.Unmarshal([]byte(body), &payload); err != nil || payload.Content != fmt.Sprintf("entry-%d", tc.wantStart+i) {
					t.Fatalf("message %d = %s, err=%v", i, body, err)
				}
			}
			if fs.LastSeenEntry != "entry-8" || !fs.Initialized {
				t.Fatalf("unexpected final watermark/initialization: %+v", fs)
			}
		})
	}
}

// A config error on a non-empty first fetch must not mark the feed
// initialized: once the config is fixed, catchUpLimit 0 still skips the
// backlog instead of delivering all of it.
// configErrURLPrefix keeps the entry URLs of the config-error test unique in the package.
const configErrURLPrefix = "https://config-error.example.test/"

func TestProcessFeed_ConfigErrorOnFirstSyncDoesNotInitialize(t *testing.T) {
	ns, name := initializationUnitNS, "config-error-first-sync"
	discordServer := NewMockDiscordServer(t)
	defer discordServer.Close()

	fg, feed := newMetricsFeedGroup(ns, name, "")
	fg.Spec.CatchUpLimit = new(int32(0))
	feed.Filter = &v1alpha1.Filter{Regex: "("} // does not compile
	fg.Spec.Feeds[0] = feed
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	entries := []rss.Entry{
		{ID: configErrURLPrefix + "newer", Title: "newer", Link: configErrURLPrefix + "newer", Seq: 2},
		{ID: configErrURLPrefix + "older", Title: "older", Link: configErrURLPrefix + "older", Seq: 1},
	}
	processFeed(t.Context(), testRun(t, fg, feed, rss.FetchResult{Entries: entries}, nil, client))
	if fs := feedStatusFor(fg, feed.RSSUrl); fs.Initialized || fs.LastSeenEntry != "" {
		t.Fatalf("config error left initialized=%t watermark=%q", fs.Initialized, fs.LastSeenEntry)
	}

	feed.Filter = nil
	fg.Spec.Feeds[0] = feed
	processFeed(t.Context(), testRun(t, fg, feed, rss.FetchResult{Entries: entries}, nil, client))
	if got := discordServer.MessageCount(); got != 0 {
		t.Fatalf("sent=%d after fixing config, want the backlog skipped", got)
	}
	if fs := feedStatusFor(fg, feed.RSSUrl); !fs.Initialized || fs.LastSeenEntry == "" {
		t.Fatalf("after fix initialized=%t watermark=%q", fs.Initialized, fs.LastSeenEntry)
	}
}

// Every compile path of the renderer's preparation -- message template, embed
// description template and forum thread-name template -- must leave a
// non-empty first fetch uninitialized, like a bad filter regex does.
func TestProcessFeed_RenderConfigErrorsOnFirstSyncDoNotInitialize(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(*v1alpha1.FeedSpec)
	}{
		{"message template", func(f *v1alpha1.FeedSpec) { f.Format = "{{.Open" }},
		{"embed description template", func(f *v1alpha1.FeedSpec) {
			f.Embed = &v1alpha1.EmbedSpec{Enabled: true, DescriptionFormat: "{{if .Title}}"}
		}},
		{"forum thread name template", func(f *v1alpha1.FeedSpec) { f.ForumThreadName = "{{end}}" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns, name := initializationUnitNS, "render-config-error-"+strings.ReplaceAll(tc.name, " ", "-")
			discordServer := NewMockDiscordServer(t)
			defer discordServer.Close()

			fg, feed := newMetricsFeedGroup(ns, name, "")
			fg.Spec.CatchUpLimit = new(int32(0))
			good := feed
			tc.break_(&feed)
			client := discordServer.DiscordClientBuilder()(discordServer.URL())

			entries := []rss.Entry{
				{ID: configErrURLPrefix + "n-" + name, Title: "newer", Link: configErrURLPrefix + "n-" + name, Seq: 2},
				{ID: configErrURLPrefix + "o-" + name, Title: "older", Link: configErrURLPrefix + "o-" + name, Seq: 1},
			}
			processFeed(t.Context(), testRun(t, fg, feed, rss.FetchResult{Entries: entries}, nil, client))
			if fs := feedStatusFor(fg, feed.RSSUrl); fs.Initialized || fs.LastSeenEntry != "" {
				t.Fatalf("config error left initialized=%t watermark=%q", fs.Initialized, fs.LastSeenEntry)
			}

			processFeed(t.Context(), testRun(t, fg, good, rss.FetchResult{Entries: entries}, nil, client))
			if got := discordServer.MessageCount(); got != 0 {
				t.Fatalf("sent=%d after fixing config, want the backlog skipped", got)
			}
			if fs := feedStatusFor(fg, feed.RSSUrl); !fs.Initialized || fs.LastSeenEntry == "" {
				t.Fatalf("after fix initialized=%t watermark=%q", fs.Initialized, fs.LastSeenEntry)
			}
		})
	}
}

// legacyStateNS namespaces the metric series the stateVersion migration tests create.
const legacyStateNS = "legacy-state"

// legacySentAt is the send time recorded for entries a legacy controller delivered.
const legacySentAt = "2025-12-30T00:00:00Z"

// legacyURLPrefix keeps the entry URLs of the migration tests unique in the package.
const legacyURLPrefix = "https://legacy-state.example.test/"

// linkEntry is an entry whose ID the parser took from its link, i the
// oldest-to-newest position that dates it.
func linkEntry(link string, i int) rss.Entry {
	return rss.Entry{ID: link, IDSource: rss.IDFromLink, Link: link, Title: link, Published: time.Unix(int64(1000+i), 0)}
}

// contentEntry is an entry with no GUID, link or title, as the parser
// returns it.
func contentEntry(description string, i int) rss.Entry {
	entry := rss.Entry{Description: description, Published: time.Unix(int64(1000+i), 0)}
	entry.ID, entry.IDSource = rss.ContentID(entry), rss.IDFromContent
	return entry
}

// guidEntry is an entry with a GUID, i the oldest-to-newest position that dates it.
func guidEntry(id string, i int) rss.Entry {
	return rss.Entry{ID: id, Title: id, Published: time.Unix(int64(1000+i), 0)}
}

// legacyKey is the LastSent key a controller before stateVersion 1 stored for entry.
func legacyKey(entry rss.Entry) string {
	return hashIdentity(legacyEntryIdentity(entry))
}

// newLegacyFeedGroup returns a FeedGroup whose feed status was written by a
// controller before stateVersion 1 and has already been checked.
func newLegacyFeedGroup(t *testing.T, name, format string) (*v1alpha1.FeedGroup, v1alpha1.FeedSpec, *v1alpha1.FeedStatus) {
	t.Helper()
	fg, feed := newMetricsFeedGroup(legacyStateNS, name, format)
	forgetMetricsAfter(t, fg.Namespace, fg.Name)
	fs := feedStatusFor(fg, feed.RSSUrl)
	fs.LastChecked = "2025-12-29T00:00:00Z"
	return fg, feed, fs
}

// sentContents returns the content of each message the mock Discord server received.
func sentContents(t *testing.T, server *MockDiscordServer) []string {
	t.Helper()
	bodies := server.Bodies()
	contents := make([]string, 0, len(bodies))
	for _, body := range bodies {
		var payload struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		contents = append(contents, payload.Content)
	}
	return contents
}

// processTwice runs processFeed on entries twice and fails unless the first
// run sent exactly want and the second run sent nothing more.
func processTwice(t *testing.T, fg *v1alpha1.FeedGroup, feed v1alpha1.FeedSpec, entries []rss.Entry, want ...string) {
	t.Helper()
	server := NewMockDiscordServer(t)
	defer server.Close()
	client := server.DiscordClientBuilder()(server.URL())
	for range 2 {
		// processFeed sorts entries in place, so hand each run its own copy.
		fetch := rss.FetchResult{Entries: slices.Clone(entries)}
		processFeed(t.Context(), testRun(t, fg, feed, fetch, nil, client))
	}
	if got := sentContents(t, server); !slices.Equal(got, want) {
		t.Fatalf("sent %q, want %q", got, want)
	}
	if fs := feedStatusFor(fg, feed.RSSUrl); fs.StateVersion != currentStateVersion {
		t.Fatalf("stateVersion=%d, want %d", fs.StateVersion, currentStateVersion)
	}
}

// Links that differ only by fragment or ref are distinct entries when they
// have no GUID, so neither is skipped as a duplicate of the other.
func TestProcessFeed_LinkFragmentAndRefAreDistinctIdentities(t *testing.T) {
	fg, feed := newMetricsFeedGroup(legacyStateNS, "fragment-and-ref", "{{.Title}}")
	forgetMetricsAfter(t, fg.Namespace, fg.Name)
	entries := []rss.Entry{
		linkEntry(legacyURLPrefix+"changelog#v1.2", 0),
		linkEntry(legacyURLPrefix+"changelog#v1.3", 1),
		linkEntry(legacyURLPrefix+"release?ref=v1.0", 2),
		linkEntry(legacyURLPrefix+"release?ref=v1.1&utm_source=feed", 3),
	}
	processTwice(t, fg, feed, entries,
		entries[0].Title, entries[1].Title, entries[2].Title, entries[3].Title)
	// A GUID that is a URL keeps the legacy normalization: no churn for it.
	if entryIdentity(guidEntry(legacyURLPrefix+"a#x", 0)) != legacyURLPrefix+"a" {
		t.Fatal("GUID identity no longer drops the fragment")
	}
}

// A feed that rotates the ref parameter or fragment on its GUID-less links
// between fetches (share tracking, CDN rewrites) must not repost unchanged
// entries, nor lose the watermark: both fall back to the link without them
// when no other entry shares it. A genuinely new entry is still delivered.
func TestProcessFeed_RotatingLinkTargetIsNotResent(t *testing.T) {
	fg, feed := newMetricsFeedGroup(legacyStateNS, "rotating-target", "{{.Title}}")
	forgetMetricsAfter(t, fg.Namespace, fg.Name)
	titled := func(link, title string, i int) rss.Entry {
		entry := linkEntry(legacyURLPrefix+link, i)
		entry.Title = title
		return entry
	}
	first := []rss.Entry{titled("rotate/post-1?ref=a", "one", 0), titled("rotate/post-2#c1", "two", 1)}
	processTwice(t, fg, feed, first, "one", "two")

	rotated := []rss.Entry{
		titled("rotate/post-1?ref=b", "one", 0),
		titled("rotate/post-2#c2", "two", 1),
		titled("rotate/post-3?ref=b", "three", 2),
	}
	processTwice(t, fg, feed, rotated, "three")
	if fs := feedStatusFor(fg, feed.RSSUrl); fs.LastSeenEntry != entryIdentity(rotated[2]) {
		t.Fatalf("watermark=%q, want %q", fs.LastSeenEntry, entryIdentity(rotated[2]))
	}
}

// A GUID-less link whose stripped form is another entry's GUID is a
// different entry, not a rotation of it, so it is delivered even though the
// GUID was already sent.
func TestProcessFeed_LinkStrippedToAnotherGUIDIsDelivered(t *testing.T) {
	fg, feed := newMetricsFeedGroup(legacyStateNS, "stripped-to-guid", "{{.Title}}")
	forgetMetricsAfter(t, fg.Namespace, fg.Name)
	guid := guidEntry(legacyURLPrefix+"guid-collide/post", 0)
	processTwice(t, fg, feed, []rss.Entry{guid}, guid.Title)
	link := linkEntry(legacyURLPrefix+"guid-collide/post#v2", 1)
	processTwice(t, fg, feed, []rss.Entry{guid, link}, link.Title)
}

// A GUID-less link is a new entry, not a rotation, when the only thing it
// shares a stripped form with is an already-sent GUID that has since left
// the feed: neither the GUID's dedup key nor its watermark may stand in for it.
func TestProcessFeed_LinkAfterGUIDWithSameBaseIsDelivered(t *testing.T) {
	fg, feed := newMetricsFeedGroup(legacyStateNS, "link-after-guid", "{{.Title}}")
	forgetMetricsAfter(t, fg.Namespace, fg.Name)
	guid := guidEntry(legacyURLPrefix+"after-guid/post", 0)
	processTwice(t, fg, feed, []rss.Entry{guid}, guid.Title)
	link := linkEntry(legacyURLPrefix+"after-guid/post#v2", 1)
	processTwice(t, fg, feed, []rss.Entry{link}, link.Title)
}

// The replay cap must hold across later polls too: a colliding group larger
// than LastSent's retention cap must not leak historical entries once
// pruning evicts what the migration recorded about them.
func TestProcessFeed_LegacyReplayCapSurvivesPruning(t *testing.T) {
	for _, limit := range []int32{0, defaultCatchUpLimit} {
		t.Run(fmt.Sprintf("limit %d", limit), func(t *testing.T) {
			fg, feed, fs := newLegacyFeedGroup(t, fmt.Sprintf("replay-pruning-%d", limit), "{{.Title}}")
			fg.Spec.CatchUpLimit = new(limit)
			entries := make([]rss.Entry, 0, maxLastSentPerFeed+10)
			for i := range maxLastSentPerFeed + 10 {
				entries = append(entries, linkEntry(fmt.Sprintf("%spruning/changelog#v%d", legacyURLPrefix, i), i))
			}
			fs.Initialized = true
			fs.LastSeenEntry = legacyEntryIdentity(entries[0])
			fs.LastSent[legacyKey(entries[0])] = legacySentAt
			want := make([]string, 0, limit)
			for _, entry := range entries[len(entries)-int(limit):] {
				want = append(want, entry.Title)
			}
			processTwice(t, fg, feed, entries, want...)
		})
	}
}

// The replay cap also holds when delivery is interrupted: a failed send
// leaves the watermark where the migration put it, past every entry the cap
// excluded, so the retries still deliver only the newest catchUpLimit. A new
// entry dated among the replayed ones is delivered too.
func TestProcessFeed_LegacyReplayCapSurvivesFailedSends(t *testing.T) {
	fg, feed, fs := newLegacyFeedGroup(t, "replay-failed-sends", "{{.Title}}")
	entries := make([]rss.Entry, 0, maxLastSentPerFeed+11)
	for i := range maxLastSentPerFeed + 10 {
		entries = append(entries, linkEntry(fmt.Sprintf("%sfailed/changelog#v%d", legacyURLPrefix, i), i))
	}
	// Dated among the replayed entries; it is new and must not be skipped.
	fresh := guidEntry("replay-failed-fresh", 0)
	fresh.Published = entries[len(entries)-3].Published.Add(time.Millisecond)
	entries = append(entries, fresh)
	fs.Initialized = true
	fs.LastSeenEntry = legacyEntryIdentity(entries[0])
	fs.LastSent[legacyKey(entries[0])] = legacySentAt

	server := NewMockDiscordServer(t)
	defer server.Close()
	client := server.DiscordClientBuilder()(server.URL())
	server.FailNextRequests(2, http.StatusInternalServerError)
	for range 4 {
		fetch := rss.FetchResult{Entries: slices.Clone(entries)}
		processFeed(t.Context(), testRun(t, fg, feed, fetch, nil, client))
	}
	want := make([]string, 0, defaultCatchUpLimit+1)
	for _, entry := range entries[len(entries)-1-defaultCatchUpLimit : len(entries)-3] {
		want = append(want, entry.Title)
	}
	want = append(want, fresh.Title, entries[len(entries)-3].Title, entries[len(entries)-2].Title)
	if got := sentContents(t, server); !slices.Equal(got, want) {
		t.Fatalf("sent %q, want %q", got, want)
	}
}

// Capping the replay moves the watermark past already-sent entries along
// with the skipped colliding ones, but never past an unsent entry outside
// the group: that entry is new, and colliding entries dated after it are
// delivered with it even beyond the cap.
func TestProcessFeed_LegacyReplayStopsAtNewEntry(t *testing.T) {
	fg, feed, fs := newLegacyFeedGroup(t, "replay-stops-at-new", "{{.Title}}")
	fg.Spec.CatchUpLimit = new(int32(2))
	sibling := func(i int) rss.Entry {
		return linkEntry(fmt.Sprintf("%sstops/changelog#v%d", legacyURLPrefix, i), i)
	}
	sentGUID := guidEntry("replay-stops-sent", 2)
	fresh := guidEntry("replay-stops-fresh", 4)
	entries := []rss.Entry{sibling(0), sibling(1), sentGUID, sibling(3), fresh, sibling(5), sibling(6), sibling(7)}
	fs.Initialized = true
	fs.LastSeenEntry = legacyEntryIdentity(entries[0])
	fs.LastSent[legacyKey(entries[0])] = legacySentAt
	fs.LastSent[legacyKey(sentGUID)] = legacySentAt
	processTwice(t, fg, feed, entries, fresh.Title, entries[5].Title, entries[6].Title, entries[7].Title)
}

// A watermark that kept a fragment or ref, and so came from a link, falls back
// to the link's stripped form only when it is unique in the fetch; with no such
// entry it reads as scrolled out.
func TestWatermarkIndex_LinkFallback(t *testing.T) {
	rotated := linkEntry(legacyURLPrefix+"index/post?ref=b", 0)
	sibling := linkEntry(legacyURLPrefix+"index/post?ref=c", 1)
	other := linkEntry(legacyURLPrefix+"index/other", 2)
	watermark := normalizeLinkIdentity(legacyURLPrefix + "index/post?ref=a")
	for _, tc := range []struct {
		name    string
		entries []rss.Entry
		want    int
	}{
		{name: "unique rotation", entries: []rss.Entry{other, rotated}, want: 1},
		{name: "shared stripped form", entries: []rss.Entry{rotated, sibling}, want: -1},
		{name: "no match", entries: []rss.Entry{other}, want: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &v1alpha1.FeedStatus{LastSeenEntry: watermark}
			if got := newDeliveryLedger(fs, tc.entries).resume(); got.index != tc.want || (got.state == watermarkFound) != (tc.want >= 0) {
				t.Fatalf("resume = %+v, want index %d", got, tc.want)
			}
		})
	}
}

// Legacy keys in LastSent and a legacy watermark, stored under the old
// fragment- and ref-stripped identities, still count after the upgrade:
// nothing already delivered is resent, the new entry is, and the status is
// rewritten in the current scheme.
func TestProcessFeed_LegacyKeysPresent(t *testing.T) {
	sent := []rss.Entry{
		guidEntry("guid-0", 0),
		linkEntry(legacyURLPrefix+"keys/post-1?ref=rss", 1),
		linkEntry(legacyURLPrefix+"keys/post-2#top", 2),
	}
	fresh := linkEntry(legacyURLPrefix+"keys/post-3?ref=rss", 3)
	entries := append(slices.Clone(sent), fresh)

	for _, tc := range []struct {
		name      string
		watermark string
	}{
		{name: "watermark in window", watermark: legacyEntryIdentity(sent[2])},
		// With the watermark gone only the LastSent keys stop the
		// catch-up from resending the delivered entries.
		{name: "watermark scrolled out", watermark: legacyURLPrefix + "keys/gone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fg, feed, fs := newLegacyFeedGroup(t, "keys-"+tc.name, "{{.Title}}")
			fs.LastSeenEntry = tc.watermark
			for _, entry := range sent {
				fs.LastSent[legacyKey(entry)] = legacySentAt
			}
			processTwice(t, fg, feed, entries, fresh.Title)

			if fs.LastSeenEntry != entryIdentity(fresh) {
				t.Fatalf("watermark=%q, want %q", fs.LastSeenEntry, entryIdentity(fresh))
			}
			for _, entry := range sent {
				if _, ok := fs.LastSent[computeEntryKey(entry)]; !ok {
					t.Fatalf("no current key for %q: %v", entry.ID, fs.LastSent)
				}
			}
			// A unique link's legacy key becomes its stripped key.
			for _, entry := range sent[1:] {
				if _, ok := fs.LastSent[legacyKey(entry)]; ok {
					t.Fatalf("legacy key for %q was kept", entry.ID)
				}
				if _, ok := fs.LastSent[strippedKey(legacyEntryIdentity(entry))]; !ok {
					t.Fatalf("no stripped key for %q", entry.ID)
				}
			}
		})
	}
}

// A legacy LastSent key whose entry is missing from the fetch that migrates
// the status can't be translated: a legacy key can't say whether it was a
// link's or a GUID's with the same URL, and reading it as the link's could
// drop a GUID for good. So if that entry comes back after the watermark it is
// replayed, once, rather than risk losing an entry.
func TestProcessFeed_LegacyKeyForAbsentEntryReplaysOnce(t *testing.T) {
	fg, feed, fs := newLegacyFeedGroup(t, "absent-legacy-key", "{{.Title}}")
	away := linkEntry(legacyURLPrefix+"absent/post-1?ref=rss", 2)
	present := linkEntry(legacyURLPrefix+"absent/post-2?ref=rss", 1)
	fs.Initialized = true
	fs.LastSeenEntry = legacyEntryIdentity(present)
	fs.LastSent[legacyKey(away)] = legacySentAt
	fs.LastSent[legacyKey(present)] = legacySentAt
	processTwice(t, fg, feed, []rss.Entry{present})
	processTwice(t, fg, feed, []rss.Entry{away, present}, away.Title)
}

// The upgrade bug an earlier identity change hit: a legacy watermark followed
// by more than defaultCatchUpLimit new entries must still resume after the
// watermark, delivering all of them, instead of reading as scrolled out and
// capping the catch-up (which dropped the oldest new entry for good).
func TestProcessFeed_LegacyWatermarkFollowedByMoreThanCatchUpLimit(t *testing.T) {
	fg, feed, fs := newLegacyFeedGroup(t, "watermark-then-many", "{{.Title}}")
	watermark := linkEntry(legacyURLPrefix+"many/w?ref=rss", 0)
	fs.Initialized = true
	fs.LastSeenEntry = legacyEntryIdentity(watermark)
	fs.LastSent[legacyKey(watermark)] = legacySentAt

	entries := []rss.Entry{watermark}
	var want []string
	for i := 1; i <= defaultCatchUpLimit+2; i++ {
		entry := linkEntry(fmt.Sprintf("%smany/post-%d?ref=rss", legacyURLPrefix, i), i)
		entries = append(entries, entry)
		want = append(want, entry.Title)
	}
	processTwice(t, fg, feed, entries, want...)
}

// Under the legacy scheme links differing only by fragment shared one
// identity, so a watermark or key for it can't say which entry it meant.
// Instead of guessing, the watermark resolves to the oldest of them and the
// newer ones are replayed: at most one of them was ever delivered.
func TestProcessFeed_LegacyCollidingIdentitiesReplay(t *testing.T) {
	fg, feed, fs := newLegacyFeedGroup(t, "colliding", "{{.Title}}")
	entries := []rss.Entry{
		linkEntry(legacyURLPrefix+"collide/changelog#v1.1", 0),
		linkEntry(legacyURLPrefix+"collide/changelog#v1.2", 1),
		linkEntry(legacyURLPrefix+"collide/changelog#v1.3", 2),
	}
	fs.Initialized = true
	fs.LastSeenEntry = legacyEntryIdentity(entries[0])
	fs.LastSent[legacyKey(entries[0])] = legacySentAt
	processTwice(t, fg, feed, entries, entries[1].Title, entries[2].Title)
	if _, ok := fs.LastSent[legacyKey(entries[0])]; ok {
		t.Fatal("ambiguous legacy key was kept")
	}
}

// The replay of colliding legacy entries is a backlog like any other, so it
// is capped at catchUpLimit: a GUID-less changelog whose entries all shared
// one legacy identity must not post every one of them on upgrade. An entry
// outside the colliding group is new and is still delivered.
func TestProcessFeed_LegacyCollidingReplayIsCapped(t *testing.T) {
	for _, limit := range []int32{defaultCatchUpLimit, 2, 0} {
		t.Run(fmt.Sprintf("limit %d", limit), func(t *testing.T) {
			fg, feed, fs := newLegacyFeedGroup(t, fmt.Sprintf("colliding-capped-%d", limit), "{{.Title}}")
			fg.Spec.CatchUpLimit = new(limit)
			entries := make([]rss.Entry, 0, 3*defaultCatchUpLimit+1)
			for i := range 3 * defaultCatchUpLimit {
				entries = append(entries, linkEntry(fmt.Sprintf("%scapped/changelog#v%d", legacyURLPrefix, i), i))
			}
			unrelated := linkEntry(legacyURLPrefix+"capped/unrelated", len(entries))
			entries = append(entries, unrelated)
			fs.Initialized = true
			fs.LastSeenEntry = legacyEntryIdentity(entries[0])
			fs.LastSent[legacyKey(entries[0])] = legacySentAt

			siblings := entries[1 : len(entries)-1]
			want := make([]string, 0, int(limit)+1)
			for _, entry := range siblings[len(siblings)-int(limit):] {
				want = append(want, entry.Title)
			}
			want = append(want, unrelated.Title)
			processTwice(t, fg, feed, entries, want...)
		})
	}
}

// Entries with no GUID, link or title get a non-empty identity, so a
// catchUpLimit 0 first sync stores a real watermark and the next fetch sends
// only what is new instead of the whole backlog.
func TestProcessFeed_EmptyIdentityEntriesKeepWatermark(t *testing.T) {
	fg, feed := newMetricsFeedGroup(legacyStateNS, "empty-identity", "{{.Description}}")
	forgetMetricsAfter(t, fg.Namespace, fg.Name)
	fg.Spec.CatchUpLimit = new(int32(0))
	backlog := []rss.Entry{contentEntry("old-1", 0), contentEntry("old-2", 1)}
	// Entries built without an ID get the same identity the parser assigns.
	unparsed := rss.Entry{Description: "old-2", Published: backlog[1].Published}
	if entryIdentity(unparsed) != entryIdentity(backlog[1]) {
		t.Fatal("entryIdentity and the parser disagree on an empty identity")
	}

	processTwice(t, fg, feed, backlog)
	fs := feedStatusFor(fg, feed.RSSUrl)
	if fs.LastSeenEntry == "" || !ledgerResumes(backlog, fs.LastSeenEntry) {
		t.Fatalf("backlog skip stored watermark %q", fs.LastSeenEntry)
	}
	fresh := contentEntry("new", 2)
	processTwice(t, fg, feed, append(slices.Clone(backlog), fresh), "new")
}

// LastSent keys that legacy controllers stored as hashIdentity("") for entries
// with no GUID, link or title: one such entry is unambiguous and is not resent;
// several can't be told apart, so the newer ones are replayed. A backlog skip
// that stored the empty watermark resumes after the entry it stood for, and
// with that entry gone is capped instead of sending everything.
func TestProcessFeed_LegacyEmptyIdentityKeys(t *testing.T) {
	skipped := func(fs *v1alpha1.FeedStatus) {
		fs.Initialized = true
		fs.Conditions = []metav1.Condition{{Type: v1alpha1.FeedConditionTypeDelivered, Status: metav1.ConditionTrue,
			Reason: reasonBacklogSkipped, LastTransitionTime: metav1.Unix(0, 0)}}
	}
	many := make([]rss.Entry, 0, 2*defaultCatchUpLimit)
	for i := range 2 * defaultCatchUpLimit {
		many = append(many, guidEntry(fmt.Sprintf("empty-many-%d", i), i+1))
	}
	tail := func(entries []rss.Entry, n int) []string {
		titles := make([]string, 0, n)
		for _, entry := range entries[len(entries)-n:] {
			titles = append(titles, entry.Title)
		}
		return titles
	}

	for _, tc := range []struct {
		name    string
		limit   int32
		setup   func(*v1alpha1.FeedStatus)
		entries []rss.Entry
		want    []string
	}{
		{
			name:  "single sent entry",
			limit: defaultCatchUpLimit,
			setup: func(fs *v1alpha1.FeedStatus) {
				fs.Initialized = true
				fs.LastSent[hashIdentity("")] = legacySentAt
			},
			entries: []rss.Entry{contentEntry("sent", 0), guidEntry("empty-next", 1)},
			want:    []string{"empty-next"},
		},
		{
			name:  "several entries replay the newer",
			limit: defaultCatchUpLimit,
			setup: func(fs *v1alpha1.FeedStatus) {
				fs.Initialized = true
				fs.LastSent[hashIdentity("")] = legacySentAt
			},
			entries: []rss.Entry{contentEntry("first", 0), contentEntry("second", 1)},
			want:    []string{"second"},
		},
		{
			name:    "skipped backlog resumes after its entry",
			setup:   skipped,
			entries: append([]rss.Entry{contentEntry("skipped", 0)}, many...),
			want:    tail(many, len(many)),
		},
		{
			name:    "skipped backlog entry gone is capped",
			setup:   skipped,
			entries: many,
			want:    tail(many, defaultCatchUpLimit),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fg, feed, fs := newLegacyFeedGroup(t, "empty-keys-"+tc.name, "{{.Title}}{{.Description}}")
			fg.Spec.CatchUpLimit = new(tc.limit)
			tc.setup(fs)
			processTwice(t, fg, feed, tc.entries, tc.want...)
			if _, ok := fs.LastSent[hashIdentity("")]; ok {
				t.Fatal("legacy empty-identity key was kept")
			}
		})
	}
}

// A status saved before initialized existed, by a controller that set
// LastChecked on every successful fetch: with catchUpLimit 0, no watermark
// and Initialized false it can't say whether the feed was empty or a config
// error held back its backlog, so the first fetch with entries is capped at
// defaultCatchUpLimit -- what those controllers delivered -- rather than
// skipped entirely or sent in full. A status this controller wrote in the
// same shape is a real first sync and still skips the backlog.
func TestProcessFeed_LegacyEmptyFeedStatus(t *testing.T) {
	entries := make([]rss.Entry, 0, 2*defaultCatchUpLimit)
	for i := range 2 * defaultCatchUpLimit {
		entries = append(entries, guidEntry(fmt.Sprintf("legacy-empty-%d", i), i))
	}
	newest := func(n int) []string {
		titles := make([]string, 0, n)
		for _, entry := range entries[len(entries)-n:] {
			titles = append(titles, entry.Title)
		}
		return titles
	}

	for _, tc := range []struct {
		name string
		// before runs one processFeed call ahead of the fetch with entries.
		before       *rss.FetchResult
		stateVersion int32
		badConfig    bool
		want         []string
	}{
		{name: "legacy status is capped", want: newest(defaultCatchUpLimit)},
		{name: "not modified settles nothing", before: &rss.FetchResult{NotModified: true}, want: newest(defaultCatchUpLimit)},
		{name: "config error settles nothing", before: &rss.FetchResult{Entries: entries[:1]}, badConfig: true,
			want: newest(defaultCatchUpLimit)},
		{name: "empty fetch settles it as empty", before: &rss.FetchResult{}, want: newest(len(entries))},
		{name: "current status skips the backlog", stateVersion: currentStateVersion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fg, feed, fs := newLegacyFeedGroup(t, "empty-feed-"+tc.name, "{{.Title}}")
			fg.Spec.CatchUpLimit = new(int32(0))
			fs.StateVersion = tc.stateVersion
			if tc.before != nil {
				broken := feed
				if tc.badConfig {
					broken.Filter = &v1alpha1.Filter{Regex: "("}
				}
				processFeed(t.Context(), testRun(t, fg, broken, *tc.before, nil, nil))
				if wantLegacy := tc.badConfig || tc.before.NotModified; wantLegacy != (fs.StateVersion == 0) ||
					wantLegacy == fs.Initialized {
					t.Fatalf("after first fetch stateVersion=%d initialized=%t", fs.StateVersion, fs.Initialized)
				}
			}
			processTwice(t, fg, feed, entries, tc.want...)
			if !fs.Initialized || fs.LastSeenEntry != entries[len(entries)-1].ID {
				t.Fatalf("initialized=%t watermark=%q", fs.Initialized, fs.LastSeenEntry)
			}
		})
	}
}

// skipProbeTemplate fails only for the entry titled "bad", so any other entry
// renders; a title over discord.MaxContentLength makes that entry overflow.
const (
	probePoisonTitle = "bad"
	probeKeepText    = "keep"
)

const skipProbeTemplate = `{{if eq .Title "bad"}}{{.Title.Nope}}{{else}}ok {{.Title}}{{end}}`

// runSkipProbe holds a poison entry past its retries, then runs the
// reconcile whose entryDelivery.otherEntryRenders probe lets the feed skip it. prepare may
// adjust the feed or seed status before the probing reconcile.
func runSkipProbe(
	t *testing.T, ns, name string, entries []rss.Entry, prepare func(*v1alpha1.FeedGroup, *v1alpha1.FeedSpec),
) (*MockDiscordServer, *v1alpha1.FeedStatus) {
	t.Helper()
	ctx := t.Context()
	discordServer := NewMockDiscordServer(t)
	t.Cleanup(discordServer.Close)
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	fg, feed := newMetricsFeedGroup(ns, name, skipProbeTemplate)
	fg.Spec.Retries = 2
	if prepare != nil {
		prepare(fg, &feed)
	}
	fetch := rss.FetchResult{Entries: entries}
	recorder := events.NewFakeRecorder(10)

	for range 2 {
		processFeed(ctx, testRun(t, fg, feed, fetch, nil, client).withRecorder(recorder))
	}
	processFeed(ctx, testRun(t, fg, feed, fetch, nil, client).withRecorder(recorder))

	return discordServer, feedStatusFor(fg, feed.RSSUrl)
}

// TestProcessFeed_SkipProbeDoesNotRecordOverflow asserts the diagnostic
// render in entryDelivery.otherEntryRenders adds no messageOverflowChars sample: a long
// healthy entry behind a skipped poison entry is observed once, when it is
// actually sent, not once more by the probe.
func TestProcessFeed_SkipProbeDoesNotRecordOverflow(t *testing.T) {
	long := strings.Repeat("g", discord.MaxContentLength+100)
	entries := []rss.Entry{
		{ID: "https://example.com/probe-bad", Title: probePoisonTitle, Seq: 1},
		{ID: "https://example.com/probe-long", Title: long, Seq: 0},
	}
	ns, name := "processfeed-probe-overflow", "fg-probe-overflow"
	server, fs := runSkipProbe(t, ns, name, entries, nil)

	if got := server.MessageCount(); got != 1 {
		t.Fatalf("messages sent = %d, want 1", got)
	}
	if got := messageOverflowSampleCount(t, ns, name); got != 1 {
		t.Fatalf("messageOverflowChars sample count = %d, want exactly 1 (the send)", got)
	}
	if fs.LastSeenEntry != entries[1].ID {
		t.Fatalf("LastSeenEntry = %q, want %q", fs.LastSeenEntry, entries[1].ID)
	}
}

// TestProcessFeed_SkipProbeOverflowOnAlreadySentSibling characterizes that an
// already-sent entry still counts as evidence the templates work (so the poison
// entry is skipped) while its overflow is not observed, because it is never
// sent again.
func TestProcessFeed_SkipProbeOverflowOnAlreadySentSibling(t *testing.T) {
	long := strings.Repeat("o", discord.MaxContentLength+100)
	entries := []rss.Entry{
		{ID: "https://example.com/probe-old", Title: long, Seq: 3},
		{ID: "https://example.com/probe-bad2", Title: probePoisonTitle, Seq: 2},
	}
	ns, name := "processfeed-probe-sent", "fg-probe-sent"
	server, fs := runSkipProbe(t, ns, name, entries, func(fg *v1alpha1.FeedGroup, feed *v1alpha1.FeedSpec) {
		st := feedStatusFor(fg, feed.RSSUrl)
		if st.LastSent == nil {
			st.LastSent = map[string]string{}
		}
		newDeliveryLedger(st, entries).markHandled(entries[0], testReconcileTime)
	})

	if got := server.MessageCount(); got != 0 {
		t.Fatalf("messages sent = %d, want 0 (the sibling is already sent)", got)
	}
	if fs.LastSeenEntry != entries[1].ID {
		t.Fatalf("poison entry should be skipped using the sent sibling as the only evidence: LastSeenEntry = %q", fs.LastSeenEntry)
	}
	if got := messageOverflowSampleCount(t, ns, name); got != 0 {
		t.Fatalf("messageOverflowChars sample count = %d, want 0", got)
	}
}

// TestProcessFeed_SkipProbeIgnoresFilteredSibling asserts a filtered-out entry
// is no evidence that the templates work: it would never be delivered, so its
// rendering says nothing about the entries that would. With it as the only
// sibling, the poison entry is held rather than skipped, and nothing is sent or
// observed.
func TestProcessFeed_SkipProbeIgnoresFilteredSibling(t *testing.T) {
	long := strings.Repeat("f", discord.MaxContentLength+100)
	entries := []rss.Entry{
		{ID: "https://example.com/probe-filtered", Title: long, Description: "drop", Seq: 3},
		{ID: "https://example.com/probe-bad3", Title: probePoisonTitle, Description: probeKeepText, Seq: 2},
	}
	ns, name := "processfeed-probe-filtered", "fg-probe-filtered"
	server, fs := runSkipProbe(t, ns, name, entries, func(_ *v1alpha1.FeedGroup, feed *v1alpha1.FeedSpec) {
		feed.Filter = &v1alpha1.Filter{Regex: probeKeepText}
	})

	if got := server.MessageCount(); got != 0 {
		t.Fatalf("messages sent = %d, want 0 (the sibling is filtered out)", got)
	}
	if fs.LastSeenEntry == entries[1].ID {
		t.Fatal("poison entry was skipped on the evidence of a filtered-out sibling; it must be held")
	}
	if got := outcomeCount(ns, name, outcomeSkipped); got != 0 {
		t.Fatalf("skipped outcomes = %v, want 0", got)
	}
	if got := messageOverflowSampleCount(t, ns, name); got != 0 {
		t.Fatalf("messageOverflowChars sample count = %d, want 0", got)
	}
}

// TestProcessFeed_OverflowRecordedPerSendAttempt pins the histogram's timing:
// the observation is made before SendMessage's outcome is known, so a send that
// fails and is retried counts once per attempt, not only for the delivery.
func TestProcessFeed_OverflowRecordedPerSendAttempt(t *testing.T) {
	ctx := t.Context()
	ns, name := "processfeed-overflow-attempts", "fg-overflow-attempts"
	discordServer := NewMockDiscordServer(t)
	defer discordServer.Close()
	discordServer.FailNextRequests(2, http.StatusInternalServerError)
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	fg, feed := newMetricsFeedGroup(ns, name, "")
	fg.Spec.Retries = 5
	fetch := rss.FetchResult{Entries: []rss.Entry{{
		ID: "https://example.com/attempts", Title: "t", Link: "https://example.com/attempts",
		Description: strings.Repeat("x", discord.MaxContentLength+500),
	}}}
	for range 3 {
		processFeed(ctx, testRun(t, fg, feed, fetch, nil, client))
	}

	if got := discordServer.MessageCount(); got != 1 {
		t.Fatalf("messages delivered = %d, want 1", got)
	}
	if got := messageOverflowSampleCount(t, ns, name); got != 3 {
		t.Fatalf("messageOverflowChars sample count = %d, want 3 (two failed attempts + delivery)", got)
	}
	// The default template renders 2548 runes (2500 description + 48 of
	// title, link and markup), 548 over the limit, on each of the 3 attempts.
	if got := messageOverflowSampleSum(t, ns, name); got != 3*548 {
		t.Fatalf("messageOverflowChars sample sum = %v, want %d", got, 3*548)
	}
}

// failFetchRun drives n failed fetches of the feed through processFeed, which
// leaves a fetch-phase retry run: Reachable False, RetryCount n and LastError.
func failFetchRun(t *testing.T, fg *v1alpha1.FeedGroup, feed v1alpha1.FeedSpec, n int, fetchErr error) {
	t.Helper()
	for range n {
		processFeed(t.Context(), testRun(t, fg, feed, rss.FetchResult{}, fetchErr, nil))
	}
	fs := feedStatusFor(fg, feed.RSSUrl)
	if !apimeta.IsStatusConditionFalse(fs.Conditions, v1alpha1.FeedConditionTypeReachable) ||
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
	fg.Spec.Retries = 3
	fetch := rss.FetchResult{Entries: []rss.Entry{
		{ID: "https://example.com/bad", Title: "bad", Seq: 1},
		{ID: "https://example.com/ok-entry", Title: "fine", Seq: 0},
	}}
	failFetchRun(t, fg, feed, 5, errors.New("connection reset by peer"))

	recorder := events.NewFakeRecorder(10)
	verdict := processFeed(t.Context(), testRun(t, fg, feed, fetch, nil, client).withRecorder(recorder))

	fs := feedStatusFor(fg, feed.RSSUrl)
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
	failFetchRun(t, fg, feed, 5, errors.New("connection reset by peer"))

	sender := &fakeSender{results: []error{&discord.HTTPStatusError{StatusCode: 400, Status: "400 Bad Request"}}}
	processFeed(t.Context(), testRun(t, fg, feed, oneEntryFetch(), nil, sender))

	fs := feedStatusFor(fg, feed.RSSUrl)
	if fs.RetryCount != 1 {
		t.Errorf("RetryCount = %d, want 1", fs.RetryCount)
	}
	if want := nextBackoffUntil(testReconcileInstant, 10*time.Minute); fs.BackoffUntil != want {
		t.Errorf("BackoffUntil = %q, want %q (5m * 2^1)", fs.BackoffUntil, want)
	}
}

// TestProcessFeed_DeliveryRunSurvivesSuccessfulFetches pins the other half: a
// render-failure run belongs to the delivery phase, so the successful fetch
// that precedes each retry must not reset it, or it could never exhaust.
func TestProcessFeed_DeliveryRunSurvivesSuccessfulFetches(t *testing.T) {
	ns, name := "processfeed-delivery-run", "fg-delivery-run"

	fg, feed := newMetricsFeedGroup(ns, name, "{{.Title.Nope}}")
	fg.Spec.Retries = 10
	fetch := rss.FetchResult{Entries: []rss.Entry{{ID: "https://example.com/only-entry", Title: "only"}}}

	for want := int32(1); want <= 3; want++ {
		processFeed(t.Context(), testRun(t, fg, feed, fetch, nil, &fakeSender{}))
		if got := feedStatusFor(fg, feed.RSSUrl).RetryCount; got != want {
			t.Fatalf("after render failure %d: RetryCount = %d, want %d", want, got, want)
		}
	}
}

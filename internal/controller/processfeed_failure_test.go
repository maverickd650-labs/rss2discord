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
	"fmt"
	"testing"
	"time"

	"github.com/maverickd650-labs/rss2discord/api/v1beta1"
	"github.com/maverickd650-labs/rss2discord/internal/discord"
	"github.com/maverickd650-labs/rss2discord/internal/rss"
	"k8s.io/client-go/tools/events"
)

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
	fg.Spec.Retries = new(int32(2))
	fetch := rss.FetchResult{Entries: []rss.Entry{
		{ID: "https://example.com/bad", Title: "bad", Seq: 1},
		{ID: "https://example.com/good", Title: "good", Seq: 0},
	}}
	recorder := events.NewFakeRecorder(10)

	// Attempts 1 and 2 exhaust the retries and hold the feed on the bad entry.
	for range 2 {
		processFeed(ctx, testRun(t, fg, feed, fetch, nil, client).withRecorder(recorder))
	}
	assertHeld(t, feedStatusFor(fg, feed.Name), discordServer.MessageCount())
	discardEvents(recorder)

	// The next reconcile gives up on the bad entry and sends the good one.
	processFeed(ctx, testRun(t, fg, feed, fetch, nil, client).withRecorder(recorder))

	if got := discordServer.MessageCount(); got != 1 {
		t.Fatalf("expected the entry behind the poison one to be sent, got %d messages", got)
	}
	fs := feedStatusFor(fg, feed.Name)
	if _, ok := fs.LastSent[computeEntryKey(fetch.Entries[0])]; !ok {
		t.Error("skipped entry should be recorded in LastSent so it isn't retried")
	}
	assertFeedState(t, fs, feedState{lastSeen: "https://example.com/good"})
	assertEventReason(t, recorder, reasonEntrySkipped)
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
	fg.Spec.Retries = new(int32(1))
	fetch := rss.FetchResult{Entries: []rss.Entry{
		{ID: "https://example.com/entry-a", Title: "a", Seq: 1},
		{ID: "https://example.com/entry-b", Title: "b", Seq: 0},
	}}
	fs := feedStatusFor(fg, feed.Name)
	fs.RetryCount = 5 // far past the retry limit

	recorder := events.NewFakeRecorder(10)
	processFeed(ctx, testRun(t, fg, feed, fetch, nil, client).withRecorder(recorder))

	assertHeld(t, fs, discordServer.MessageCount())
	assertNoSkipEvent(t, recorder)
}

func TestProcessFeed_RenderErrorPersistentEventFiresOnce(t *testing.T) {
	fg, feed := newMetricsFeedGroup("processfeed-render-event", "fg-render-event", "{{.Bogus}}")
	fetch := rss.FetchResult{Entries: []rss.Entry{{ID: "e1", Title: "Title 1"}}}
	sender := &fakeSender{}

	fs := assertEventFiresOnceAtLimit(t, fg, feed, fetch, sender, eventReasonRenderFailed)

	assertHeld(t, fs, len(sender.sent))
}

func TestProcessFeed_PersistentSendErrorEventFiresOnce(t *testing.T) {
	fg, feed := newMetricsFeedGroup("processfeed-send-event", "fg-send-event", "")
	sender := &fakeSender{results: []error{errInternalServer}}

	fs := assertEventFiresOnceAtLimit(t, fg, feed, oneEntryFetch(), sender, eventReasonSendFailed)

	assertDeliveredReason(t, fs, reasonServerError)
}

// assertEventFiresOnceAtLimit processes the feed three times with two retries
// allowed and fails unless the first run asks for a retry, the second spends
// the last one and emits the Event for reason, and the third, still failing,
// asks for nothing and emits no second Event. It returns the feed's status.
func assertEventFiresOnceAtLimit(
	t *testing.T, fg *v1beta1.FeedGroup, feed v1beta1.FeedSpec, fetch rss.FetchResult, sender *fakeSender, reason string,
) *v1beta1.FeedStatus {
	t.Helper()
	fg.Spec.Retries = new(int32(2))
	fg.Spec.Feeds[0] = feed
	recorder := events.NewFakeRecorder(10)
	run := func() attemptVerdict {
		return processFeed(t.Context(), testRun(t, fg, feed, fetch, nil, sender).withRecorder(recorder))
	}

	assertRetry(t, run(), true)
	fs := feedStatusFor(fg, feed.Name)
	assertRetryCount(t, fs, 1)
	assertNoEvent(t, recorder)

	assertRetry(t, run(), false)
	assertRetryCount(t, fs, 2)
	assertEventReason(t, recorder, reason)

	assertRetry(t, run(), false)
	assertRetryCount(t, fs, 3)
	assertNoEvent(t, recorder)
	return fs
}

func TestProcessFeed_SendFailureHaltsDeliveryOrder(t *testing.T) {
	fg, feed := newMetricsFeedGroup("processfeed-send-halt", "fg-send-halt", tmplTitle)
	fg.Spec.Retries = new(int32(3))
	fg.Spec.Feeds[0] = feed
	fetch := rss.FetchResult{Entries: []rss.Entry{
		{ID: "older-article", Title: "Older Article", Seq: 1},
		{ID: "newer-article", Title: "Newer Article", Seq: 0},
	}}

	sender := &fakeSender{results: []error{errInternalServer}}
	processFeed(t.Context(), testRun(t, fg, feed, fetch, nil, sender))

	if len(sender.sent) != 1 {
		t.Fatalf("sender called %d times, want 1 (newer must not be attempted)", len(sender.sent))
	}
	fs := feedStatusFor(fg, feed.Name)
	if fs.LastSeenEntry != "" {
		t.Fatalf("LastSeenEntry = %q, want empty (must not advance past failed entry)", fs.LastSeenEntry)
	}

	sender2 := &fakeSender{}
	processFeed(t.Context(), testRun(t, fg, feed, fetch, nil, sender2))

	if len(sender2.sent) != 2 {
		t.Fatalf("sender called %d times, want 2", len(sender2.sent))
	}
	if sender2.sent[0].Content != "Older Article" || sender2.sent[1].Content != "Newer Article" {
		t.Fatalf("sent = [%q, %q], want [Older Article, Newer Article]", sender2.sent[0].Content, sender2.sent[1].Content)
	}
	if fs.LastSeenEntry != "newer-article" {
		t.Fatalf("LastSeenEntry = %q, want newer-article", fs.LastSeenEntry)
	}
}

func TestProcessFeed_SendFailureDoesNotStoreETag(t *testing.T) {
	fg, feed := newMetricsFeedGroup("processfeed-send-etag", "fg-send-etag", "")
	fg.Spec.Retries = new(int32(2))
	fg.Spec.Feeds[0] = feed
	fetch := oneEntryFetch()
	fetch.ETag = `"v1"`

	sender := &fakeSender{results: []error{errInternalServer}}
	for range 2 {
		processFeed(t.Context(), testRun(t, fg, feed, fetch, nil, sender))
	}

	fs := feedStatusFor(fg, feed.Name)
	if fs.RetryCount != 2 {
		t.Fatalf("RetryCount = %d, want 2", fs.RetryCount)
	}
	if fs.ETag != "" {
		t.Fatalf("ETag = %q, want empty while entry is unsent", fs.ETag)
	}

	sender2 := &fakeSender{}
	processFeed(t.Context(), testRun(t, fg, feed, fetch, nil, sender2))

	if len(sender2.sent) != 1 {
		t.Fatalf("sender called %d times, want 1", len(sender2.sent))
	}
	if fs.ETag != `"v1"` {
		t.Fatalf("ETag = %q, want \"v1\"", fs.ETag)
	}
}

func TestProcessFeed_PermanentSendFailureBackoff(t *testing.T) {
	fg, feed := newMetricsFeedGroup("processfeed-perm-send", "fg-perm-send", "")
	fg.Spec.Retries = new(int32(3))
	fg.Spec.RetryInterval = "5m"
	fg.Spec.Feeds[0] = feed

	sender := &fakeSender{results: []error{&discord.HTTPStatusError{StatusCode: 404, Status: "404 Not Found"}}}
	recorder := events.NewFakeRecorder(10)

	verdict := processFeed(t.Context(), testRun(t, fg, feed, oneEntryFetch(), nil, sender).withRecorder(recorder))

	if verdict.retry {
		t.Fatal("verdict.retry = true, want false for permanent send failure")
	}
	fs := feedStatusFor(fg, feed.Name)
	if fs.RetryCount != 1 {
		t.Fatalf("RetryCount = %d, want 1", fs.RetryCount)
	}
	if want := nextBackoffUntil(testReconcileInstant, 10*time.Minute); !fs.BackoffUntil.Time.Equal(want.Time) {
		t.Fatalf("BackoffUntil = %v, want %v", fs.BackoffUntil, want)
	}
	select {
	case ev := <-recorder.Events:
		t.Fatalf("unexpected event before backoff cap: %s", ev)
	default:
	}

	r := &FeedGroupReconciler{Client: fakeApplyClient()}
	res, err := r.openGroupSync(fg).settlePass(t.Context(), feedPlan{active: []v1beta1.FeedSpec{feed}}, verdict)
	if err != nil {
		t.Fatalf("settlePass error: %v", err)
	}
	if res.RequeueAfter < 30*time.Minute || res.RequeueAfter > 33*time.Minute {
		t.Fatalf("settlePass RequeueAfter = %v, want within [30m, 33m]", res.RequeueAfter)
	}
}

func TestProcessFeed_RateLimitStopsRemainingEntries(t *testing.T) {
	fg, feed := newMetricsFeedGroup("processfeed-rl-stop", "fg-rl-stop", "")
	fg.Spec.CatchUpLimit = new(int32(5))
	fg.Spec.Feeds[0] = feed

	entries := make([]rss.Entry, 5)
	for i := range entries {
		id := fmt.Sprintf("guid-%d", i)
		entries[i] = rss.Entry{ID: id, Title: id, Seq: 4 - i}
	}

	sender := &fakeSender{results: []error{&discord.RateLimitError{RetryAfter: 2 * time.Second}}}
	verdict := processFeed(t.Context(), testRun(t, fg, feed, rss.FetchResult{Entries: entries}, nil, sender))

	if len(sender.sent) != 1 {
		t.Fatalf("sender called %d times, want exactly 1 attempt before rate-limit stop", len(sender.sent))
	}
	if verdict.retryAfter != 2*time.Second {
		t.Fatalf("verdict.retryAfter = %v, want 2s", verdict.retryAfter)
	}
	if !verdict.retry {
		t.Fatal("verdict.retry = false, want true")
	}
}

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
	"strings"
	"testing"
	"time"

	"github.com/maverickd650-labs/rss2discord/api/v1beta1"
	"github.com/maverickd650-labs/rss2discord/internal/discord"
	"github.com/maverickd650-labs/rss2discord/internal/rss"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
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
	fs := feedStatusFor(fg, feed.Name)
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
	fs = feedStatusFor(fg, feed.Name)
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
	fg.Spec.Retries = new(int32(1))
	fetch := rss.FetchResult{Entries: []rss.Entry{
		{ID: "https://example.com/entry-a", Title: "a", Seq: 1},
		{ID: "https://example.com/entry-b", Title: "b", Seq: 0},
	}}
	fs := feedStatusFor(fg, feed.Name)
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

func TestProcessFeed_RenderErrorPersistentEventFiresOnce(t *testing.T) {
	fg, feed := newMetricsFeedGroup("processfeed-render-event", "fg-render-event", "{{.Bogus}}")
	fg.Spec.Retries = new(int32(2))
	fg.Spec.Feeds[0] = feed
	fetch := rss.FetchResult{Entries: []rss.Entry{{ID: "e1", Title: "Title 1"}}}
	recorder := events.NewFakeRecorder(10)
	sender := &fakeSender{}

	verdict := processFeed(t.Context(), testRun(t, fg, feed, fetch, nil, sender).withRecorder(recorder))
	if !verdict.retry {
		t.Fatal("verdict.retry = false, want true")
	}
	fs := feedStatusFor(fg, feed.Name)
	if fs.RetryCount != 1 {
		t.Fatalf("RetryCount = %d, want 1", fs.RetryCount)
	}
	if len(recorder.Events) != 0 {
		t.Fatalf("unexpected event before retries exhausted: %d", len(recorder.Events))
	}

	verdict = processFeed(t.Context(), testRun(t, fg, feed, fetch, nil, sender).withRecorder(recorder))
	if verdict.retry {
		t.Fatal("verdict.retry = true, want false once retries are exhausted")
	}
	if fs.RetryCount != 2 {
		t.Fatalf("RetryCount = %d, want 2", fs.RetryCount)
	}
	select {
	case ev := <-recorder.Events:
		if !strings.Contains(ev, eventReasonRenderFailed) {
			t.Fatalf("event = %q, want reason %s", ev, eventReasonRenderFailed)
		}
	default:
		t.Fatal("expected RenderFailed event once retries exhausted")
	}

	verdict = processFeed(t.Context(), testRun(t, fg, feed, fetch, nil, sender).withRecorder(recorder))
	if verdict.retry {
		t.Fatal("verdict.retry = true, want false once retries are exhausted")
	}
	if fs.RetryCount != 3 {
		t.Fatalf("RetryCount = %d, want 3", fs.RetryCount)
	}
	select {
	case ev := <-recorder.Events:
		t.Fatalf("unexpected duplicate event: %s", ev)
	default:
	}
	if fs.LastSeenEntry != "" {
		t.Fatalf("LastSeenEntry = %q, want empty (entry held)", fs.LastSeenEntry)
	}
}

func TestProcessFeed_PersistentSendErrorEventFiresOnce(t *testing.T) {
	fg, feed := newMetricsFeedGroup("processfeed-send-event", "fg-send-event", "")
	fg.Spec.Retries = new(int32(2))
	fg.Spec.Feeds[0] = feed
	fetch := oneEntryFetch()
	recorder := events.NewFakeRecorder(10)
	sender := &fakeSender{results: []error{errInternalServer}}

	verdict := processFeed(t.Context(), testRun(t, fg, feed, fetch, nil, sender).withRecorder(recorder))
	if !verdict.retry {
		t.Fatal("verdict.retry = false, want true")
	}
	fs := feedStatusFor(fg, feed.Name)
	if fs.RetryCount != 1 {
		t.Fatalf("RetryCount = %d, want 1", fs.RetryCount)
	}
	if len(recorder.Events) != 0 {
		t.Fatalf("unexpected event before retries exhausted: %d", len(recorder.Events))
	}

	verdict = processFeed(t.Context(), testRun(t, fg, feed, fetch, nil, sender).withRecorder(recorder))
	if verdict.retry {
		t.Fatal("verdict.retry = true, want false once retries are exhausted")
	}
	if fs.RetryCount != 2 {
		t.Fatalf("RetryCount = %d, want 2", fs.RetryCount)
	}
	select {
	case ev := <-recorder.Events:
		if !strings.Contains(ev, eventReasonSendFailed) {
			t.Fatalf("event = %q, want reason %s", ev, eventReasonSendFailed)
		}
	default:
		t.Fatal("expected SendFailed event once retries exhausted")
	}
	delivered := apimeta.FindStatusCondition(fs.Conditions, v1beta1.FeedConditionTypeDelivered)
	if delivered == nil || delivered.Reason != reasonServerError {
		t.Fatalf("Delivered condition = %v, want Reason %s", delivered, reasonServerError)
	}

	verdict = processFeed(t.Context(), testRun(t, fg, feed, fetch, nil, sender).withRecorder(recorder))
	if verdict.retry {
		t.Fatal("verdict.retry = true, want false once retries are exhausted")
	}
	if fs.RetryCount != 3 {
		t.Fatalf("RetryCount = %d, want 3", fs.RetryCount)
	}
	select {
	case ev := <-recorder.Events:
		t.Fatalf("unexpected duplicate event: %s", ev)
	default:
	}
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

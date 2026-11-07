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
	"strings"
	"testing"

	"github.com/maverickd650-labs/rss2discord/api/v1beta1"
	"github.com/maverickd650-labs/rss2discord/internal/rss"
	"k8s.io/client-go/tools/events"
)

// TestProcessFeed_InvalidFilterRegex asserts a feed with an unparsable
// filter regex records the compile error on the feed's status and sends
// nothing, instead of panicking or silently matching everything.
func TestProcessFeed_InvalidFilterRegex(t *testing.T) {
	ctx := t.Context()
	ns, name := "processfeed-bad-regex", "fg-bad-regex"

	discordServer := NewMockDiscordServer(t)
	defer discordServer.Close()

	fg, feed := newMetricsFeedGroup(ns, name, "")
	feed.Filter = &v1beta1.Filter{Include: &v1beta1.FilterRules{Regex: "("}}
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
	if feedStatusFor(fg, feed.Name).LastError == "" {
		t.Fatal("expected LastError to be set for the invalid filter regex")
	}
	if discordServer.MessageCount() != 0 {
		t.Fatalf("expected no message sent, got %d", discordServer.MessageCount())
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

	fg, feed := newMetricsFeedGroup(ns, name, tmplUnclosed)
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	verdict := processFeed(ctx, testRun(t, fg, feed, oneEntryFetch(), nil, client))
	wantRetry := verdict.retry

	if wantRetry {
		t.Fatal("expected no retry for a deterministic message template compile error")
	}
	if feedStatusFor(fg, feed.Name).LastError == "" {
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
	feed.ForumPost = &v1beta1.ForumPostSpec{Name: tmplUnclosed}
	fg.Spec.Feeds[0] = feed
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	verdict := processFeed(ctx, testRun(t, fg, feed, oneEntryFetch(), nil, client))
	wantRetry := verdict.retry

	if wantRetry {
		t.Fatal("expected no retry for a deterministic forum thread name template compile error")
	}
	if feedStatusFor(fg, feed.Name).LastError == "" {
		t.Fatal("expected LastError to be set for the invalid forum thread name template")
	}
	if discordServer.MessageCount() != 0 {
		t.Fatalf("expected no message sent, got %d", discordServer.MessageCount())
	}
}

// TestRecordConfigError_EventFiresOncePerDistinctError asserts the
// InvalidConfig Warning Event is emitted on the reconcile a config error first
// appears -- and not again while the same error persists -- since a
// deterministic misconfiguration has no RetryCount to gate it on.
func TestRecordConfigError_EventFiresOncePerDistinctError(t *testing.T) {
	fg, feed := newMetricsFeedGroup("config-error-ns", "config-error", "")
	fs := feedStatusFor(fg, feed.Name)
	recorder := events.NewFakeRecorder(10)
	attempt := testAttempt(t, fg, fs, feed.URL, testReconcileInstant).withRecorder(recorder)
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

// configErrURLPrefix keeps the entry URLs of the config-error tests unique in the package.
const configErrURLPrefix = "https://config-error.example.test/"

// A config error on a non-empty first fetch must not mark the feed
// initialized: once the config is fixed, catchUpLimit 0 still skips the
// backlog instead of delivering all of it.

func TestProcessFeed_ConfigErrorOnFirstSyncDoesNotInitialize(t *testing.T) {
	ns, name := initializationUnitNS, "config-error-first-sync"
	discordServer := NewMockDiscordServer(t)
	defer discordServer.Close()

	fg, feed := newMetricsFeedGroup(ns, name, "")
	fg.Spec.CatchUpLimit = new(int32(0))
	feed.Filter = &v1beta1.Filter{Include: &v1beta1.FilterRules{Regex: "("}} // does not compile
	fg.Spec.Feeds[0] = feed
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	entries := []rss.Entry{
		{ID: configErrURLPrefix + "newer", Title: "newer", Link: configErrURLPrefix + "newer", Seq: 2},
		{ID: configErrURLPrefix + "older", Title: "older", Link: configErrURLPrefix + "older", Seq: 1},
	}
	processFeed(t.Context(), testRun(t, fg, feed, rss.FetchResult{Entries: entries}, nil, client))
	if fs := feedStatusFor(fg, feed.Name); fs.Initialized || fs.LastSeenEntry != "" {
		t.Fatalf("config error left initialized=%t watermark=%q", fs.Initialized, fs.LastSeenEntry)
	}

	feed.Filter = nil
	fg.Spec.Feeds[0] = feed
	processFeed(t.Context(), testRun(t, fg, feed, rss.FetchResult{Entries: entries}, nil, client))
	if got := discordServer.MessageCount(); got != 0 {
		t.Fatalf("sent=%d after fixing config, want the backlog skipped", got)
	}
	if fs := feedStatusFor(fg, feed.Name); !fs.Initialized || fs.LastSeenEntry == "" {
		t.Fatalf("after fix initialized=%t watermark=%q", fs.Initialized, fs.LastSeenEntry)
	}
}

// Every compile path of the renderer's preparation -- message template, embed
// description template and forum thread-name template -- must leave a
// non-empty first fetch uninitialized, like a bad filter regex does.
func TestProcessFeed_RenderConfigErrorsOnFirstSyncDoNotInitialize(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(*v1beta1.FeedSpec)
	}{
		{"message template", func(f *v1beta1.FeedSpec) { f.Message = &v1beta1.MessageSpec{Content: "{{.Open"} }},
		{"embed description template", func(f *v1beta1.FeedSpec) {
			f.Message = &v1beta1.MessageSpec{Embed: &v1beta1.EmbedSpec{Description: "{{if .Title}}"}}
		}},
		{"forum thread name template", func(f *v1beta1.FeedSpec) { f.ForumPost = &v1beta1.ForumPostSpec{Name: "{{end}}"} }},
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
			if fs := feedStatusFor(fg, feed.Name); fs.Initialized || fs.LastSeenEntry != "" {
				t.Fatalf("config error left initialized=%t watermark=%q", fs.Initialized, fs.LastSeenEntry)
			}

			processFeed(t.Context(), testRun(t, fg, good, rss.FetchResult{Entries: entries}, nil, client))
			if got := discordServer.MessageCount(); got != 0 {
				t.Fatalf("sent=%d after fixing config, want the backlog skipped", got)
			}
			if fs := feedStatusFor(fg, feed.Name); !fs.Initialized || fs.LastSeenEntry == "" {
				t.Fatalf("after fix initialized=%t watermark=%q", fs.Initialized, fs.LastSeenEntry)
			}
		})
	}
}

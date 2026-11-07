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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/maverickd650-labs/rss2discord/api/v1beta1"
	"github.com/maverickd650-labs/rss2discord/internal/rss"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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
	feedStatusFor(fg, feed.Name).LastSent = map[string]metav1.Time{entryKey: ts("2025-12-31T00:00:00Z")}
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

// With catchUpLimit 0, only a true first sync skips the
// backlog. A watermark that scrolled out of the feed's window falls back to
// the default cap instead, so genuinely new entries aren't silently dropped.
func TestProcessFeed_CatchUpLimitZeroOnlySkipsOnFirstSync(t *testing.T) {
	ctx := t.Context()

	run := func(t *testing.T, lastSeen string, limit *int32) (sent int, fs *v1beta1.FeedStatus) {
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
			fs := feedStatusFor(fg, feed.Name)
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
	for _, tc := range []struct {
		name     string
		dated    bool
		newestID string
	}{
		{name: "publication order", dated: true, newestID: restartNewestID},
		{name: "undated document order", newestID: restartNewestID},
		{name: "oversized identity", newestID: "guid-" + strings.Repeat("x", maxStatusTextBytes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) { runBacklogRestart(t, tc.name, tc.dated, tc.newestID) })
	}
}

const (
	restartNewestID = "newest"
	restartOldestID = "oldest"
	restartFreshID  = "fresh"
)

// restartBacklog is the two-entry backlog of a first sync, newest first by
// document position or, when dated, by publication time (which wins over
// position, so the positions are swapped).
func restartBacklog(dated bool, newestID string) []rss.Entry {
	entries := make([]rss.Entry, 2, 3)
	entries[0] = rss.Entry{ID: newestID, Title: restartNewestID, Seq: 0}
	entries[1] = rss.Entry{ID: restartOldestID, Title: restartOldestID, Seq: 1}
	if dated {
		entries[0].Published = time.Unix(200, 0)
		entries[1].Published = time.Unix(100, 0)
		entries[0].Seq, entries[1].Seq = 1, 0
	}
	return entries
}

// runBacklogRestart skips an existing backlog on the first sync, checks the
// status survives a reload (a restarted controller) without sending any of
// it, then delivers only an entry published after the saved watermark.
func runBacklogRestart(t *testing.T, name string, dated bool, newestID string) {
	fg, feed := newMetricsFeedGroup("backlog-restart", name, tmplTitle)
	forgetMetricsAfter(t, fg.Namespace, fg.Name)
	fg.Generation = 7
	fg.Spec.CatchUpLimit = new(int32(0))
	fs := feedStatusFor(fg, feed.Name)
	fs.LastError, fs.RetryCount, fs.BackoffUntil = "old failure", 2, metav1TimeRFC3339("2031-01-01T00:00:00Z")
	discordServer := NewMockDiscordServer(t)
	defer discordServer.Close()
	client := discordServer.DiscordClientBuilder()(discordServer.URL())
	entries := restartBacklog(dated, newestID)
	fetch := rss.FetchResult{Entries: entries, ETag: `"skip-v1"`, LastModified: testReconcileTime}

	verdict := processFeed(t.Context(), testRun(t, fg, feed, fetch, nil, client))
	if verdict.retry || verdict.retryAfter != 0 || discordServer.MessageCount() != 0 || len(fs.LastSent) != 0 {
		t.Fatalf("backlog was sent or retried: retry=%t after=%v sent=%d status=%+v", verdict.retry, verdict.retryAfter, discordServer.MessageCount(), fs)
	}
	assertSkippedBacklogStatus(t, fg, newestID, fetch)

	// Reload status before checking the same feed again.
	fg = fg.DeepCopy()
	ensureFeedStatuses(fg)
	processFeed(t.Context(), testRun(t, fg, feed, fetch, nil, client))
	if discordServer.MessageCount() != 0 {
		t.Fatal("reloading status caused the backlog to be sent")
	}

	// Then publish an entry after the saved watermark.
	for i := range entries {
		entries[i].Seq++
	}
	fresh := rss.Entry{ID: restartFreshID, Title: restartFreshID, Seq: 0}
	if dated {
		fresh.Published = time.Unix(300, 0)
	}
	fetch.Entries = append(entries, fresh)
	processFeed(t.Context(), testRun(t, fg, feed, fetch, nil, client))
	assertOnlyFreshSent(t, discordServer, feedStatusFor(fg, feed.Name))

	processFeed(t.Context(), testRun(t, fg, feed, fetch, nil, client))
	if discordServer.MessageCount() != 1 {
		t.Fatal("fresh entry was delivered twice")
	}
}

// assertOnlyFreshSent fails unless the fresh entry was the only message sent
// and fs shows delivery advancing past the BacklogSkipped state.
func assertOnlyFreshSent(t *testing.T, server *MockDiscordServer, fs *v1beta1.FeedStatus) {
	t.Helper()
	if got := server.Bodies(); len(got) != 1 || !strings.Contains(got[0], `"content":"fresh"`) {
		t.Fatalf("expected only the fresh entry, got %v", got)
	}
	condition := apimeta.FindStatusCondition(fs.Conditions, v1beta1.FeedConditionTypeDelivered)
	advanced := condition != nil && condition.Status == metav1.ConditionTrue && condition.Reason != reasonBacklogSkipped
	if fs.LastSeenEntry != restartFreshID || len(fs.LastSent) != 1 || !advanced {
		t.Fatalf("delivery did not advance status: %+v", fs)
	}
}

// assertSkippedBacklogStatus fails unless the feed's status is what skipping
// an existing backlog leaves: settled, clean, carrying the fetch's validators
// and a watermark that resumes at newestID, with the BacklogSkipped condition.
func assertSkippedBacklogStatus(t *testing.T, fg *v1beta1.FeedGroup, newestID string, fetch rss.FetchResult) {
	t.Helper()
	fs := feedStatusFor(fg, fg.Spec.Feeds[0].Name)
	assertSettledClean(t, fs)
	if fs.ETag != fetch.ETag || fs.LastModified != fetch.LastModified {
		t.Fatalf("validators not persisted after skipped backlog: %+v", fs)
	}
	if len(fs.LastSeenEntry) > maxStatusTextBytes || !ledgerResumes([]rss.Entry{{ID: newestID}}, fs.LastSeenEntry) {
		t.Fatalf("watermark does not resume at %q: %+v", newestID, fs)
	}
	condition := apimeta.FindStatusCondition(fs.Conditions, v1beta1.FeedConditionTypeDelivered)
	if !isBacklogSkipped(condition, fg.Generation) {
		t.Fatalf("unexpected backlog condition: %+v", condition)
	}
}

// assertSettledClean fails unless fs is initialized, checked at the test's
// reconcile time and free of any error, retry or backoff.
func assertSettledClean(t *testing.T, fs *v1beta1.FeedStatus) {
	t.Helper()
	checkedNow := fs.LastChecked != nil && fs.LastChecked.Time.Equal(testReconcileInstant)
	if !fs.Initialized || !checkedNow {
		t.Fatalf("feed not initialized and checked at the reconcile time: %+v", fs)
	}
	if fs.LastError != "" || fs.RetryCount != 0 || fs.BackoffUntil != nil {
		t.Fatalf("stale failure state left after skipped backlog: %+v", fs)
	}
}

// isBacklogSkipped reports whether c is the True BacklogSkipped Delivered
// condition for generation, counting both skipped entries.
func isBacklogSkipped(c *metav1.Condition, generation int64) bool {
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != reasonBacklogSkipped {
		return false
	}
	return c.ObservedGeneration == generation && strings.Contains(c.Message, "2 existing entries")
}

func TestProcessFeed_CatchUpCapsAndWatermarks(t *testing.T) {
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
		{name: "zero with missing watermark before initialization uses default cap", limit: new(int32(0)), lastSeen: scrolledOutWatermark, wantStart: 4},
		{name: "zero with matching watermark resumes", limit: new(int32(0)), lastSeen: "entry-1", wantStart: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fg, feed := newMetricsFeedGroup("backlog-caps", tc.name, tmplTitle)
			forgetMetricsAfter(t, fg.Namespace, fg.Name)
			fg.Spec.CatchUpLimit = tc.limit
			fs := feedStatusFor(fg, feed.Name)
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

func TestProcessFeed_NoDateCatchUpAndAdvance(t *testing.T) {
	const (
		articleB = "Article B"
		articleC = "Article C"
	)
	fg, feed := newMetricsFeedGroup("processfeed-nodate", "fg-nodate", tmplTitle)
	fg.Spec.CatchUpLimit = new(int32(2))
	fg.Spec.Feeds[0] = feed

	entries := []rss.Entry{
		{ID: "c", Title: articleC, Seq: 0},
		{ID: "b", Title: articleB, Seq: 1},
		{ID: "a", Title: "Article A", Seq: 2},
	}

	sender := &fakeSender{}
	processFeed(t.Context(), testRun(t, fg, feed, rss.FetchResult{Entries: slices.Clone(entries)}, nil, sender))

	if len(sender.sent) != 2 {
		t.Fatalf("sent %d messages, want 2 (newest B and C)", len(sender.sent))
	}
	if sender.sent[0].Content != articleB || sender.sent[1].Content != articleC {
		t.Fatalf("sent = [%q, %q], want [%s, %s]", sender.sent[0].Content, sender.sent[1].Content, articleB, articleC)
	}
	fs := feedStatusFor(fg, feed.Name)
	if fs.LastSeenEntry != "c" {
		t.Fatalf("LastSeenEntry = %q, want 'c'", fs.LastSeenEntry)
	}

	fresh := []rss.Entry{
		{ID: "d", Title: "Article D", Seq: 0},
		{ID: "c", Title: articleC, Seq: 1},
		{ID: "b", Title: articleB, Seq: 2},
		{ID: "a", Title: "Article A", Seq: 3},
	}
	sender2 := &fakeSender{}
	processFeed(t.Context(), testRun(t, fg, feed, rss.FetchResult{Entries: fresh}, nil, sender2))

	if len(sender2.sent) != 1 {
		t.Fatalf("sent %d messages, want 1 (only fresh entry D)", len(sender2.sent))
	}
	if sender2.sent[0].Content != "Article D" {
		t.Fatalf("sent = %q, want Article D", sender2.sent[0].Content)
	}
	if fs.LastSeenEntry != "d" {
		t.Fatalf("LastSeenEntry = %q, want 'd'", fs.LastSeenEntry)
	}
}

func TestProcessFeed_ScrolledOutWatermarkFallsBackToCatchUp(t *testing.T) {
	const rep2ID = "replacement-2"
	fg, feed := newMetricsFeedGroup("processfeed-stale-watermark", "fg-stale-watermark", tmplTitle)
	fg.Spec.CatchUpLimit = new(int32(5))
	fg.Spec.Feeds[0] = feed

	sender := &fakeSender{}
	firstFetch := rss.FetchResult{Entries: []rss.Entry{
		{ID: "original-article", Title: "Original Article", Seq: 0},
	}}
	processFeed(t.Context(), testRun(t, fg, feed, firstFetch, nil, sender))

	if len(sender.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(sender.sent))
	}
	fs := feedStatusFor(fg, feed.Name)
	if fs.LastSeenEntry != "original-article" {
		t.Fatalf("LastSeenEntry = %q, want original-article", fs.LastSeenEntry)
	}

	sender2 := &fakeSender{}
	secondFetch := rss.FetchResult{Entries: []rss.Entry{
		{ID: rep2ID, Title: "Replacement Article 2", Seq: 0},
		{ID: "replacement-1", Title: "Replacement Article 1", Seq: 1},
	}}
	processFeed(t.Context(), testRun(t, fg, feed, secondFetch, nil, sender2))

	if len(sender2.sent) != 2 {
		t.Fatalf("sent %d messages, want 2 replacements", len(sender2.sent))
	}
	if fs.LastSeenEntry != rep2ID {
		t.Fatalf("LastSeenEntry = %q, want %s", fs.LastSeenEntry, rep2ID)
	}

	sender3 := &fakeSender{}
	thirdFetch := rss.FetchResult{Entries: []rss.Entry{
		{ID: "replacement-3", Title: "Replacement Article 3", Seq: 0},
		{ID: rep2ID, Title: "Replacement Article 2", Seq: 1},
		{ID: "replacement-1", Title: "Replacement Article 1", Seq: 2},
	}}
	processFeed(t.Context(), testRun(t, fg, feed, thirdFetch, nil, sender3))

	if len(sender3.sent) != 1 || sender3.sent[0].Content != "Replacement Article 3" {
		t.Fatalf("sent %v, want exactly Replacement Article 3", sender3.sent)
	}
	if fs.LastSeenEntry != "replacement-3" {
		t.Fatalf("LastSeenEntry = %q, want replacement-3", fs.LastSeenEntry)
	}
}

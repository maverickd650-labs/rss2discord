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
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	v1alpha1 "github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/rss"
	"k8s.io/client-go/tools/events"
)

// Delivery settlement: a fetched document is accounted for only when nothing
// from it is still pending, and only then may its validators be persisted and
// an ambiguous legacy status be initialized and settled. These tests drive
// processFeed through whole transition sequences, reloading the status from
// its serialized form between passes as a new reconcile (or a restarted
// controller) would.

const (
	settlementNS   = "settlement"
	validatorsOld  = `"v1"`
	validatorsNext = `"v2"`
)

// reloadStatus replaces fg's status with a copy read back from its JSON form,
// then renormalizes it the way Reconcile does before processing feeds.
func reloadStatus(t *testing.T, fg *v1alpha1.FeedGroup) *v1alpha1.FeedStatus {
	t.Helper()
	raw, err := json.Marshal(fg.Status)
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	var reloaded v1alpha1.FeedGroupStatus
	if err := json.Unmarshal(raw, &reloaded); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	fg.Status = reloaded
	ensureFeedStatuses(fg)
	return feedStatusFor(fg, exampleFeedURL)
}

// An ambiguous legacy status under catchUpLimit 0 is capped at
// defaultCatchUpLimit in memory only. A first send that fails must leave the
// status ambiguous, so the retry (here after a reload) is capped the same way
// instead of delivering the whole backlog.
func TestProcessFeed_AmbiguousCapSurvivesFailedFirstSend(t *testing.T) {
	entries := make([]rss.Entry, 0, 2*defaultCatchUpLimit)
	for i := range 2 * defaultCatchUpLimit {
		entries = append(entries, guidEntry(fmt.Sprintf("ambiguous-failed-%d", i), i))
	}
	fg, feed, _ := newLegacyFeedGroup(t, "ambiguous-failed-first-send", "{{.Title}}")
	fg.Spec.CatchUpLimit = new(int32(0))
	server := NewMockDiscordServer()
	defer server.Close()
	client := server.DiscordClientBuilder()(server.URL())
	server.FailNextRequests(1, http.StatusInternalServerError)
	r := &FeedGroupReconciler{}
	fetch := func() rss.FetchResult { return rss.FetchResult{Entries: slices.Clone(entries), ETag: validatorsOld} }

	r.processFeed(t.Context(), fg, feed, fetch(), nil, client, testReconcileTime)
	fs := reloadStatus(t, fg)
	if fs.Initialized || fs.StateVersion != 0 || fs.ETag != "" || server.MessageCount() != 0 {
		t.Fatalf("after the failed send: initialized=%t stateVersion=%d etag=%q sent=%d, want a still-ambiguous status",
			fs.Initialized, fs.StateVersion, fs.ETag, server.MessageCount())
	}

	r.processFeed(t.Context(), fg, feed, fetch(), nil, client, testReconcileTime)
	fs = reloadStatus(t, fg)
	r.processFeed(t.Context(), fg, feed, fetch(), nil, client, testReconcileTime)

	want := make([]string, 0, defaultCatchUpLimit)
	for _, entry := range entries[len(entries)-defaultCatchUpLimit:] {
		want = append(want, entry.Title)
	}
	if got := sentContents(t, server); !slices.Equal(got, want) {
		t.Fatalf("sent %q, want only the capped %q", got, want)
	}
	if !fs.Initialized || fs.StateVersion != currentStateVersion || fs.ETag != validatorsOld ||
		fs.LastSeenEntry != entries[len(entries)-1].ID {
		t.Fatalf("settled status = %+v", fs)
	}
}

// pendingCase is one way an entry stays unresolved after a fetch whose
// validators differ from the stored ones.
type pendingCase struct {
	name string
	// prepare makes the next processFeed calls leave the new entry pending.
	prepare func(server *MockDiscordServer, feed *v1alpha1.FeedSpec)
	// fix lets the entry through.
	fix func(feed *v1alpha1.FeedSpec)
}

// Validators from a fetch that left anything pending must not replace the
// stored ones -- not even after retries are exhausted -- and the stored ones
// must survive rather than be cleared. Once the entry is resolved, the new
// validators are stored. Every pending state must also keep the feed due.
func TestProcessFeed_ValidatorsWithheldWhilePending(t *testing.T) {
	for _, tc := range []pendingCase{
		{
			name: "send error",
			prepare: func(server *MockDiscordServer, _ *v1alpha1.FeedSpec) {
				server.FailNextRequests(4, http.StatusInternalServerError)
			},
			fix: func(*v1alpha1.FeedSpec) {},
		},
		{
			name:    "render error",
			prepare: func(_ *MockDiscordServer, feed *v1alpha1.FeedSpec) { feed.Format = "{{.Title.Nope}}" },
			fix:     func(feed *v1alpha1.FeedSpec) { feed.Format = "{{.Title}}" },
		},
		{
			name:    "config error",
			prepare: func(_ *MockDiscordServer, feed *v1alpha1.FeedSpec) { feed.Filter = &v1alpha1.Filter{Regex: "("} },
			fix:     func(feed *v1alpha1.FeedSpec) { feed.Filter = nil },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := "validators-" + tc.name
			fg, feed := newMetricsFeedGroup(settlementNS, name, "{{.Title}}")
			defer deleteFeedGroupMetrics(fg.Namespace, fg.Name)
			fg.Spec.Retries = 2
			server := NewMockDiscordServer()
			defer server.Close()
			client := server.DiscordClientBuilder()(server.URL())

			sentBefore := guidEntry("validators-sent", 0)
			fresh := guidEntry("validators-fresh", 1)
			fs := feedStatusFor(fg, feed.RSSUrl)
			fs.Initialized = true
			fs.StateVersion = currentStateVersion
			fs.LastSeenEntry = entryIdentity(sentBefore)
			fs.LastSent[computeEntryKey(sentBefore)] = "2025-12-31T00:00:00Z"
			fs.ETag = validatorsOld
			fs.LastModified = "Wed, 31 Dec 2025 00:00:00 GMT"

			tc.prepare(server, &feed)
			r := &FeedGroupReconciler{Recorder: events.NewFakeRecorder(20)}
			now := time.Now().UTC()
			fetch := func() rss.FetchResult {
				return rss.FetchResult{
					Entries: []rss.Entry{fresh, sentBefore},
					ETag:    validatorsNext, LastModified: "Thu, 01 Jan 2026 00:00:00 GMT",
				}
			}
			// Past the retry limit, so exhaustion is covered too.
			for pass := range 4 {
				r.processFeed(t.Context(), fg, feed, fetch(), nil, client, now.Format(time.RFC3339))
				fs = reloadStatus(t, fg)
				if fs.ETag != validatorsOld || fs.LastModified != "Wed, 31 Dec 2025 00:00:00 GMT" {
					t.Fatalf("pass %d: validators = %q / %q, want the stored ones kept", pass, fs.ETag, fs.LastModified)
				}
				if !feedDue(fs, 24*time.Hour, now, false) {
					t.Fatalf("pass %d: a feed with an unresolved entry must stay due", pass)
				}
			}
			if server.MessageCount() != 0 {
				t.Fatalf("sent %d messages while the entry was pending", server.MessageCount())
			}

			tc.fix(&feed)
			r.processFeed(t.Context(), fg, feed, fetch(), nil, client, now.Format(time.RFC3339))
			fs = reloadStatus(t, fg)
			if server.MessageCount() != 1 {
				t.Fatalf("sent %d messages after the fix, want 1", server.MessageCount())
			}
			if fs.ETag != validatorsNext || fs.LastModified != "Thu, 01 Jan 2026 00:00:00 GMT" ||
				fs.LastError != "" || fs.RetryCount != 0 {
				t.Fatalf("after recovery: %+v", fs)
			}
		})
	}
}

// A poison entry that is skipped is accounted for, so a pass that sends
// everything after it persists the new validators; a pass whose later send
// fails does not, but keeps the skip.
func TestProcessFeed_ValidatorsAfterSkippedEntry(t *testing.T) {
	for _, tc := range []struct {
		name          string
		failLaterSend bool
		wantETag      string
	}{
		{name: "later send succeeds", wantETag: validatorsNext},
		{name: "later send fails", failLaterSend: true, wantETag: validatorsOld},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fg, feed := newMetricsFeedGroup(settlementNS, "skip-"+tc.name, skipProbeTemplate)
			defer deleteFeedGroupMetrics(fg.Namespace, fg.Name)
			fg.Spec.Retries = 2
			server := NewMockDiscordServer()
			defer server.Close()
			client := server.DiscordClientBuilder()(server.URL())
			r := &FeedGroupReconciler{Recorder: events.NewFakeRecorder(20)}

			bad := rss.Entry{ID: "https://example.com/skip-bad", Title: probePoisonTitle, Seq: 1}
			good := rss.Entry{ID: "https://example.com/skip-good", Title: "good", Seq: 0}
			fs := feedStatusFor(fg, feed.RSSUrl)
			fs.Initialized = true
			fs.StateVersion = currentStateVersion
			fs.ETag = validatorsOld
			fetch := func() rss.FetchResult {
				return rss.FetchResult{Entries: []rss.Entry{bad, good}, ETag: validatorsNext}
			}

			for range 2 {
				r.processFeed(t.Context(), fg, feed, fetch(), nil, client, testReconcileTime)
				fs = reloadStatus(t, fg)
			}
			if fs.ETag != validatorsOld {
				t.Fatalf("validators = %q while the poison entry was held, want %q", fs.ETag, validatorsOld)
			}

			if tc.failLaterSend {
				server.FailNextRequests(1, http.StatusInternalServerError)
			}
			r.processFeed(t.Context(), fg, feed, fetch(), nil, client, testReconcileTime)
			fs = reloadStatus(t, fg)
			if fs.ETag != tc.wantETag {
				t.Fatalf("validators after the skip pass = %q, want %q", fs.ETag, tc.wantETag)
			}
			if _, ok := fs.LastSent[computeEntryKey(bad)]; !ok {
				t.Fatal("the skipped entry must stay recorded across the failed send")
			}

			if tc.failLaterSend {
				r.processFeed(t.Context(), fg, feed, fetch(), nil, client, testReconcileTime)
				fs = reloadStatus(t, fg)
			}
			if server.MessageCount() != 1 || fs.ETag != validatorsNext || fs.LastSeenEntry != good.ID {
				t.Fatalf("final: sent=%d etag=%q watermark=%q", server.MessageCount(), fs.ETag, fs.LastSeenEntry)
			}
		})
	}
}

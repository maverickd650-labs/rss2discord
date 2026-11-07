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
	"net/http"
	"strings"
	"testing"

	"github.com/maverickd650-labs/rss2discord/api/v1beta1"
	"github.com/maverickd650-labs/rss2discord/internal/discord"
	"github.com/maverickd650-labs/rss2discord/internal/rss"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
)

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
	t *testing.T, ns, name string, entries []rss.Entry, prepare func(*v1beta1.FeedGroup, *v1beta1.FeedSpec),
) (*MockDiscordServer, *v1beta1.FeedStatus) {
	t.Helper()
	ctx := t.Context()
	discordServer := NewMockDiscordServer(t)
	t.Cleanup(discordServer.Close)
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	fg, feed := newMetricsFeedGroup(ns, name, skipProbeTemplate)
	fg.Spec.Retries = new(int32(2))
	if prepare != nil {
		prepare(fg, &feed)
	}
	fetch := rss.FetchResult{Entries: entries}
	recorder := events.NewFakeRecorder(10)

	for range 2 {
		processFeed(ctx, testRun(t, fg, feed, fetch, nil, client).withRecorder(recorder))
	}
	processFeed(ctx, testRun(t, fg, feed, fetch, nil, client).withRecorder(recorder))

	return discordServer, feedStatusFor(fg, feed.Name)
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
	server, fs := runSkipProbe(t, ns, name, entries, func(fg *v1beta1.FeedGroup, feed *v1beta1.FeedSpec) {
		st := feedStatusFor(fg, feed.Name)
		if st.LastSent == nil {
			st.LastSent = map[string]metav1.Time{}
		}
		newDeliveryLedger(st, entries).markHandled(entries[0], ts(testReconcileTime))
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
	server, fs := runSkipProbe(t, ns, name, entries, func(_ *v1beta1.FeedGroup, feed *v1beta1.FeedSpec) {
		feed.Filter = &v1beta1.Filter{Include: &v1beta1.FilterRules{Regex: probeKeepText}}
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
	fg.Spec.Retries = new(int32(5))
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

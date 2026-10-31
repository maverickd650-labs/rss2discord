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

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/discord"
	"github.com/maverickd650/rss2discord-operator/internal/rss"
)

// deliveryEntry is an entry that renders under the plain "{{.Title}}" format;
// a blank title makes that format render empty content, which build rejects.
func deliveryEntry(n int, title string) rss.Entry {
	link := fmt.Sprintf("https://example.com/article-%d", n)
	return rss.Entry{
		ID:        link,
		Link:      link,
		Title:     title,
		Published: time.Date(2026, 1, 1, n, 0, 0, 0, time.UTC),
	}
}

// newTestDelivery opens and begins a feedSync over entries the way processFeed
// does and returns an entryDelivery over it, with the feed's status.
func newTestDelivery(t *testing.T, ns, name string, entries []rss.Entry, sender MessageSender, retryCount int32) (entryDelivery, *v1alpha1.FeedStatus) {
	t.Helper()
	fg, feed := newMetricsFeedGroup(ns, name, "{{.Title}}")
	fg.Spec.Retries = 3
	fg.Spec.CatchUpLimit = new(int32(len(entries)))
	fs := feedStatusFor(fg, feed.RSSUrl)
	fs.RetryCount = retryCount

	renderer, err := newFeedRenderer(fg, &feed)
	if err != nil {
		t.Fatalf("newFeedRenderer: %v", err)
	}
	syncer := openFeedSync(fs, rss.FetchResult{Entries: entries}, feedSyncSpecFor(fg, testReconcileTime))
	syncer.begin()
	attempt := (&FeedGroupReconciler{}).newFeedAttempt(fg, fs, feed.RSSUrl, testReconcileTime)
	return entryDelivery{
		renderer: renderer,
		syncer:   syncer,
		attempt:  attempt,
		sender:   sender,
		metrics:  metricsFor(fg),
	}, fs
}

func outcomeCount(ns, name, outcome string) float64 {
	return testutil.ToFloat64(feedOperationsTotal.WithLabelValues(ns, name, exampleFeedURL, outcome))
}

func TestEntryDelivery_SendsAndMarksEveryEntryHandled(t *testing.T) {
	ns, name := "delivery-all", "fg-delivery-all"
	defer groupMetrics{namespace: ns, name: name}.forget()

	entries := []rss.Entry{deliveryEntry(1, "one"), deliveryEntry(2, "two"), deliveryEntry(3, "three")}
	sender := &fakeSender{}
	d, fs := newTestDelivery(t, ns, name, entries, sender, 0)

	verdict := d.run(t.Context())

	if verdict != (attemptVerdict{}) {
		t.Fatalf("verdict = %+v, want none", verdict)
	}
	if len(sender.sent) != 3 {
		t.Fatalf("sent %d messages, want 3", len(sender.sent))
	}
	for _, e := range entries {
		if !d.syncer.sent(e) {
			t.Fatalf("entry %q not marked handled", e.Title)
		}
	}
	if fs.LastSeenEntry == "" {
		t.Fatal("watermark not advanced")
	}
	if got := outcomeCount(ns, name, outcomeSent); got != 3 {
		t.Fatalf("sent outcomes = %v, want 3", got)
	}
}

func TestEntryDelivery_SendFailureStopsTheScan(t *testing.T) {
	ns, name := "delivery-5xx", "fg-delivery-5xx"
	defer groupMetrics{namespace: ns, name: name}.forget()

	entries := []rss.Entry{deliveryEntry(1, "one"), deliveryEntry(2, "two"), deliveryEntry(3, "three")}
	boom := &discord.HTTPStatusError{StatusCode: 500, Status: "500 Internal Server Error"}
	sender := &fakeSender{results: []error{nil, boom}}
	d, fs := newTestDelivery(t, ns, name, entries, sender, 0)

	verdict := d.run(t.Context())

	if !verdict.retry || !verdict.pending {
		t.Fatalf("verdict = %+v, want a pending retry", verdict)
	}
	if len(sender.sent) != 2 {
		t.Fatalf("sender called %d times, want 2 (the third entry must wait)", len(sender.sent))
	}
	if !d.syncer.sent(entries[0]) || d.syncer.sent(entries[1]) || d.syncer.sent(entries[2]) {
		t.Fatalf("LastSent = %v, want only the first entry handled", fs.LastSent)
	}
	if got := outcomeCount(ns, name, sendErrorOutcome(classServerError)); got != 1 {
		t.Fatalf("send_error_server_error outcomes = %v, want 1", got)
	}
}

func TestEntryDelivery_RateLimitStopsTheScanAndReturnsRetryAfter(t *testing.T) {
	ns, name := "delivery-429", "fg-delivery-429"
	defer groupMetrics{namespace: ns, name: name}.forget()

	entries := []rss.Entry{deliveryEntry(1, "one"), deliveryEntry(2, "two")}
	sender := &fakeSender{results: []error{&discord.RateLimitError{RetryAfter: 9 * time.Second}}}
	d, _ := newTestDelivery(t, ns, name, entries, sender, 0)

	verdict := d.run(t.Context())

	if verdict.retryAfter != 9*time.Second || !verdict.pending {
		t.Fatalf("verdict = %+v, want a pending retryAfter of 9s", verdict)
	}
	if len(sender.sent) != 1 {
		t.Fatalf("sender called %d times, want 1", len(sender.sent))
	}
	if d.syncer.sent(entries[0]) || d.syncer.sent(entries[1]) {
		t.Fatal("no entry should be handled after a rate limit on the first")
	}
	if got := outcomeCount(ns, name, outcomeRateLimited); got != 1 {
		t.Fatalf("rate_limited outcomes = %v, want 1", got)
	}
}

func TestEntryDelivery_RenderFailureBeforeRetryLimitStopsTheScan(t *testing.T) {
	ns, name := "delivery-render-early", "fg-delivery-render-early"
	defer groupMetrics{namespace: ns, name: name}.forget()

	// The poison entry has a blank title, so the format renders empty content.
	entries := []rss.Entry{deliveryEntry(1, "one"), deliveryEntry(2, ""), deliveryEntry(3, "three")}
	sender := &fakeSender{}
	d, fs := newTestDelivery(t, ns, name, entries, sender, 0)

	verdict := d.run(t.Context())

	if !verdict.retry || !verdict.pending {
		t.Fatalf("verdict = %+v, want a pending retry", verdict)
	}
	if len(sender.sent) != 1 {
		t.Fatalf("sender called %d times, want 1 (the scan stops at the poison entry)", len(sender.sent))
	}
	if !d.syncer.sent(entries[0]) || d.syncer.sent(entries[1]) || d.syncer.sent(entries[2]) {
		t.Fatalf("LastSent = %v, want only the first entry handled", fs.LastSent)
	}
	if got := outcomeCount(ns, name, outcomeRenderError); got != 1 {
		t.Fatalf("render_error outcomes = %v, want 1", got)
	}
	if got := outcomeCount(ns, name, outcomeSkipped); got != 0 {
		t.Fatalf("skipped outcomes = %v, want 0", got)
	}
}

func TestEntryDelivery_RenderFailurePastRetryLimitSkipsWhenASiblingRenders(t *testing.T) {
	ns, name := "delivery-render-skip", "fg-delivery-render-skip"
	defer groupMetrics{namespace: ns, name: name}.forget()

	// The poison entry comes first: a successful send clears RetryCount, so
	// it must be the failure that is recorded against the exhausted retries.
	entries := []rss.Entry{deliveryEntry(1, ""), deliveryEntry(2, "two"), deliveryEntry(3, "three")}
	sender := &fakeSender{}
	// Retries (3) were already exhausted: the failure recorded now goes past.
	d, _ := newTestDelivery(t, ns, name, entries, sender, 3)

	verdict := d.run(t.Context())

	if verdict != (attemptVerdict{}) {
		t.Fatalf("verdict = %+v, want none: the scan carried on past the skipped entry", verdict)
	}
	if len(sender.sent) != 2 {
		t.Fatalf("sender called %d times, want 2 (entries two and three)", len(sender.sent))
	}
	for _, e := range entries {
		if !d.syncer.sent(e) {
			t.Fatalf("entry %q not handled; the skipped entry must be marked handled too", e.ID)
		}
	}
	if got := outcomeCount(ns, name, outcomeSkipped); got != 1 {
		t.Fatalf("skipped outcomes = %v, want 1", got)
	}
	if got := outcomeCount(ns, name, outcomeRenderError); got != 1 {
		t.Fatalf("render_error outcomes = %v, want 1", got)
	}
}

func TestEntryDelivery_RenderFailurePastRetryLimitHoldsWhenNothingRenders(t *testing.T) {
	ns, name := "delivery-render-hold", "fg-delivery-render-hold"
	defer groupMetrics{namespace: ns, name: name}.forget()

	// Every entry fails to render: the template is the problem, not the entry.
	entries := []rss.Entry{deliveryEntry(1, ""), deliveryEntry(2, "")}
	sender := &fakeSender{}
	d, _ := newTestDelivery(t, ns, name, entries, sender, 3)

	verdict := d.run(t.Context())

	if !verdict.pending {
		t.Fatalf("verdict = %+v, want pending: the scan holds", verdict)
	}
	if len(sender.sent) != 0 {
		t.Fatalf("sender called %d times, want 0", len(sender.sent))
	}
	if d.syncer.sent(entries[0]) || d.syncer.sent(entries[1]) {
		t.Fatal("no entry may be marked handled when nothing renders")
	}
	if got := outcomeCount(ns, name, outcomeSkipped); got != 0 {
		t.Fatalf("skipped outcomes = %v, want 0", got)
	}
}

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
	"net/http"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1beta1 "github.com/maverickd650-labs/rss2discord/api/v1beta1"
	"github.com/maverickd650-labs/rss2discord/internal/discord"
	"github.com/maverickd650-labs/rss2discord/internal/rss"
)

// newMetricsFeedGroup builds a minimal FeedGroup (status maps initialized)
// keyed by a unique namespace/name so each test's metric series starts from
// zero, avoiding cross-test accumulation in the shared registry.
func newMetricsFeedGroup(namespace, name, format string) (*v1beta1.FeedGroup, v1beta1.FeedSpec) {
	feed := v1beta1.FeedSpec{Name: testFeedName(exampleFeedURL), URL: exampleFeedURL}
	if format != "" {
		feed.Message = &v1beta1.MessageSpec{Content: format}
	}
	fg := &v1beta1.FeedGroup{}
	fg.Namespace = namespace
	fg.Name = name
	fg.Spec.Feeds = []v1beta1.FeedSpec{feed}
	// Match the API server's default for an omitted catchUpLimit.
	fg.Spec.CatchUpLimit = new(int32(defaultCatchUpLimit))
	ensureFeedStatuses(fg)
	return fg, feed
}

// oneEntryFetch is a fetch result carrying a single sendable entry.
func oneEntryFetch() rss.FetchResult {
	const articleURL = "https://example.com/article"
	return rss.FetchResult{Entries: []rss.Entry{{
		ID:          articleURL,
		Title:       "Example entry",
		Description: "Example entry body",
		Link:        articleURL,
	}}}
}

// TestProcessFeed_RecordsOutcomeMetric exercises each terminal path of
// processFeed and asserts feedOperationsTotal is incremented under the right
// outcome label -- the labels the dashboard and PrometheusRule alerts depend
// on, which previously had no test coverage at all.
func TestProcessFeed_RecordsOutcomeMetric(t *testing.T) {
	ctx := t.Context()

	cases := []struct {
		name     string
		outcome  string
		format   string
		withFeed bool
		fetchErr error
		setup    func(d *MockDiscordServer)
	}{
		{name: "sent", outcome: outcomeSent, withFeed: true},
		{name: "fetch_error", outcome: fetchErrorOutcome(classOther), fetchErr: errors.New("boom")},
		{
			name:     "send_error",
			outcome:  sendErrorOutcome(classServerError),
			withFeed: true,
			setup:    func(d *MockDiscordServer) { d.FailNextRequests(1, http.StatusInternalServerError) },
		},
		{
			name:     "rate_limited",
			outcome:  outcomeRateLimited,
			withFeed: true,
			setup:    func(d *MockDiscordServer) { d.FailNextRequestsRateLimited(1, "1") },
		},
		{
			// A template that references a field absent from the render data
			// parses fine but fails at execution, driving the render-error path.
			name:     "render_error",
			outcome:  outcomeRenderError,
			withFeed: true,
			format:   tmplMissingField,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns, name := "metrics-"+tc.name, "fg-"+tc.name

			discordServer := NewMockDiscordServer(t)
			defer discordServer.Close()
			if tc.setup != nil {
				tc.setup(discordServer)
			}

			fg, feed := newMetricsFeedGroup(ns, name, tc.format)
			client := discordServer.DiscordClientBuilder()(discordServer.URL())

			var fetchResult rss.FetchResult
			if tc.withFeed {
				fetchResult = oneEntryFetch()
			}
			processFeed(ctx, testRun(t, fg, feed, fetchResult, tc.fetchErr, client))

			assertOperationCount(t, seriesKey{ns, name, testFeedName(exampleFeedURL)}, tc.outcome, 1)
		})
	}
}

// TestProcessFeed_SentRecordsDurationAndFreshness asserts a successful
// delivery both observes the send-duration histogram and advances the
// last-success freshness gauge.
func TestProcessFeed_SentRecordsDurationAndFreshness(t *testing.T) {
	ctx := t.Context()
	ns, name := "metrics-sent-extra", "fg-sent-extra"

	discordServer := NewMockDiscordServer(t)
	defer discordServer.Close()

	fg, feed := newMetricsFeedGroup(ns, name, "")
	// The send duration is observed by the timing adapter Reconcile wraps
	// the client in, so processFeed is handed the same wrapping.
	client := newTimedSender(discordServer.DiscordClientBuilder()(discordServer.URL()), ns, name)

	processFeed(ctx, testRun(t, fg, feed, oneEntryFetch(), nil, client))

	if got := testutil.ToFloat64(feedLastSuccessTimestamp.WithLabelValues(ns, name)); got <= 0 {
		t.Fatalf("feedLastSuccessTimestamp = %v, want > 0", got)
	}
	if got := histogramSampleCount(t, ns, name, operationSend); got != 1 {
		t.Fatalf("send duration sample count = %d, want 1", got)
	}
}

// TestProcessFeed_QuietFetchAdvancesFreshness asserts that a successful
// check with nothing new to send -- a 304, or a fetch that returned zero
// entries -- still advances feedLastSuccessTimestamp. Before this, the gauge
// only moved on an actual Discord delivery, so a feed that's simply caught up
// (rather than broken) would eventually trip a staleness alert.
func TestProcessFeed_QuietFetchAdvancesFreshness(t *testing.T) {
	ctx := t.Context()

	cases := []struct {
		name        string
		fetchResult rss.FetchResult
	}{
		{name: "not_modified", fetchResult: rss.FetchResult{NotModified: true}},
		{name: "no_new_entries", fetchResult: rss.FetchResult{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns, name := "metrics-quiet-"+tc.name, "fg-quiet-"+tc.name

			fg, feed := newMetricsFeedGroup(ns, name, "")
			processFeed(ctx, testRun(t, fg, feed, tc.fetchResult, nil, nil))

			if got := testutil.ToFloat64(feedLastSuccessTimestamp.WithLabelValues(ns, name)); got <= 0 {
				t.Fatalf("feedLastSuccessTimestamp = %v, want > 0", got)
			}
		})
	}
}

// TestProcessFeed_RecordsMessageOverflow asserts that sending an entry whose
// rendered content exceeds Discord's message length limit records an
// observation on messageOverflowChars, so an operator can see how often a
// FeedGroup's content is actually getting cut off.
func TestProcessFeed_RecordsMessageOverflow(t *testing.T) {
	ctx := t.Context()
	ns, name := "metrics-overflow", "fg-overflow"

	discordServer := NewMockDiscordServer(t)
	defer discordServer.Close()

	fg, feed := newMetricsFeedGroup(ns, name, "")
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	const articleURL = "https://example.com/long-article"
	fetchResult := rss.FetchResult{Entries: []rss.Entry{{
		ID:          articleURL,
		Title:       "Example entry",
		Description: strings.Repeat("x", discord.MaxContentLength+500),
		Link:        articleURL,
	}}}

	processFeed(ctx, testRun(t, fg, feed, fetchResult, nil, client))

	if got := messageOverflowSampleCount(t, ns, name); got != 1 {
		t.Fatalf("messageOverflowChars sample count = %d, want 1", got)
	}
}

// TestProcessFeed_NoOverflowMetricWhenContentFits asserts that sending an
// entry whose rendered content fits within Discord's limits does not record
// an observation, so the histogram's count reflects only actual overflows.
func TestProcessFeed_NoOverflowMetricWhenContentFits(t *testing.T) {
	ctx := t.Context()
	ns, name := "metrics-no-overflow", "fg-no-overflow"

	discordServer := NewMockDiscordServer(t)
	defer discordServer.Close()

	fg, feed := newMetricsFeedGroup(ns, name, "")
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	processFeed(ctx, testRun(t, fg, feed, oneEntryFetch(), nil, client))

	if got := messageOverflowSampleCount(t, ns, name); got != 0 {
		t.Fatalf("messageOverflowChars sample count = %d, want 0", got)
	}
}

// TestProcessFeed_RecordsMessageOverflowFromEmbedTotalLengthClamp asserts
// that messageOverflowChars also counts truncation caused by Discord's
// combined embed length limit (title+description+footer+author <= 6000),
// which discord.Fit enforces after the per-field truncation. Every field here
// stays within its own limit (and the CRD's), so only the combined-length
// clamp can be responsible for the recorded overflow.
func TestProcessFeed_RecordsMessageOverflowFromEmbedTotalLengthClamp(t *testing.T) {
	ctx := t.Context()
	ns, name := "metrics-embed-total-overflow", "fg-embed-total-overflow"

	discordServer := NewMockDiscordServer(t)
	defer discordServer.Close()

	fg, feed := newMetricsFeedGroup(ns, name, "")
	feed.Message = &v1beta1.MessageSpec{Embed: &v1beta1.EmbedSpec{
		Author: &v1beta1.EmbedAuthor{Name: strings.Repeat("a", discord.MaxEmbedAuthorNameLength)},
		Footer: &v1beta1.EmbedFooter{Text: strings.Repeat("f", discord.MaxEmbedFooterTextLength)},
	}}
	fetch := oneEntryFetch()
	fetch.Entries[0].Description = strings.Repeat("d", discord.MaxEmbedDescriptionLength)
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	processFeed(ctx, testRun(t, fg, feed, fetch, nil, client))

	if got := messageOverflowSampleCount(t, ns, name); got != 1 {
		t.Fatalf("messageOverflowChars sample count = %d, want 1", got)
	}
	title := len([]rune(fetch.Entries[0].Title))
	want := float64(title + discord.MaxEmbedDescriptionLength + discord.MaxEmbedFooterTextLength +
		discord.MaxEmbedAuthorNameLength - discord.MaxEmbedTotalLength)
	if got := messageOverflowSampleSum(t, ns, name); got != want {
		t.Fatalf("messageOverflowChars sample sum = %v, want %v", got, want)
	}
}

// TestProcessFeed_NamedFeedRecordsMetricUnderName asserts that processFeed
// increments feedOperationsTotal under feed.Name, never the URL.
func TestProcessFeed_NamedFeedRecordsMetricUnderName(t *testing.T) {
	ctx := t.Context()
	const (
		feedName = "process-named-feed"
		feedURL  = "https://example.com/process-named.xml"
	)
	ns, name := "metrics-process-named", "fg-process-named"
	forgetMetricsAfter(t, ns, name)

	discordServer := NewMockDiscordServer(t)
	defer discordServer.Close()

	fg := &v1beta1.FeedGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: v1beta1.FeedGroupSpec{
			Feeds: []v1beta1.FeedSpec{
				{Name: feedName, URL: feedURL},
			},
		},
	}
	ensureFeedStatuses(fg)

	client := discordServer.DiscordClientBuilder()(discordServer.URL())
	feed := fg.Spec.Feeds[0]
	processFeed(ctx, testRun(t, fg, feed, oneEntryFetch(), nil, client))

	if got := testutil.ToFloat64(feedOperationsTotal.WithLabelValues(ns, name, feedName, outcomeSent)); got != 1 {
		t.Errorf("feedOperationsTotal with feedName = %v, want 1", got)
	}
	if got := testutil.ToFloat64(feedOperationsTotal.WithLabelValues(ns, name, feedURL, outcomeSent)); got != 0 {
		t.Errorf("feedOperationsTotal with feedURL = %v, want 0", got)
	}
}

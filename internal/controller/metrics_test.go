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
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/rss"
)

// Failure messages shared by the helpers that read a histogram series back.
const (
	getHistogramMetricFmt   = "get histogram metric: %v"
	writeHistogramMetricFmt = "write histogram metric: %v"
)

// newMetricsFeedGroup builds a minimal FeedGroup (status maps initialized)
// keyed by a unique namespace/name so each test's metric series starts from
// zero, avoiding cross-test accumulation in the shared registry.
func newMetricsFeedGroup(namespace, name, format string) (*v1alpha1.FeedGroup, v1alpha1.FeedSpec) {
	feed := v1alpha1.FeedSpec{RSSUrl: exampleFeedURL, Format: format}
	fg := &v1alpha1.FeedGroup{}
	fg.Namespace = namespace
	fg.Name = name
	fg.Spec.Feeds = []v1alpha1.FeedSpec{feed}
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

// histogramSampleCount reads the observation count of a single
// feedRequestDuration series.
func histogramSampleCount(t *testing.T, namespace, name, operation string) uint64 {
	t.Helper()
	obs, err := feedRequestDuration.GetMetricWithLabelValues(namespace, name, operation)
	if err != nil {
		t.Fatalf(getHistogramMetricFmt, err)
	}
	var m dto.Metric
	if err := obs.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf(writeHistogramMetricFmt, err)
	}
	return m.GetHistogram().GetSampleCount()
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
			format:   "{{.DoesNotExist}}",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns, name := "metrics-"+tc.name, "fg-"+tc.name
			defer deleteFeedGroupMetrics(ns, name)

			discordServer := NewMockDiscordServer()
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
			(&FeedGroupReconciler{}).processFeed(ctx, fg, feed, fetchResult, tc.fetchErr, client, "2026-01-01T00:00:00Z")

			if got := testutil.ToFloat64(feedOperationsTotal.WithLabelValues(ns, name, exampleFeedURL, tc.outcome)); got != 1 {
				t.Fatalf("feedOperationsTotal{outcome=%q} = %v, want 1", tc.outcome, got)
			}
		})
	}
}

// TestProcessFeed_SentRecordsDurationAndFreshness asserts a successful
// delivery both observes the send-duration histogram and advances the
// last-success freshness gauge.
func TestProcessFeed_SentRecordsDurationAndFreshness(t *testing.T) {
	ctx := t.Context()
	ns, name := "metrics-sent-extra", "fg-sent-extra"
	defer deleteFeedGroupMetrics(ns, name)

	discordServer := NewMockDiscordServer()
	defer discordServer.Close()

	fg, feed := newMetricsFeedGroup(ns, name, "")
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	(&FeedGroupReconciler{}).processFeed(ctx, fg, feed, oneEntryFetch(), nil, client, "2026-01-01T00:00:00Z")

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
			defer deleteFeedGroupMetrics(ns, name)

			fg, feed := newMetricsFeedGroup(ns, name, "")
			(&FeedGroupReconciler{}).processFeed(ctx, fg, feed, tc.fetchResult, nil, nil, "2026-01-01T00:00:00Z")

			if got := testutil.ToFloat64(feedLastSuccessTimestamp.WithLabelValues(ns, name)); got <= 0 {
				t.Fatalf("feedLastSuccessTimestamp = %v, want > 0", got)
			}
		})
	}
}

// messageOverflowSampleCount reads the observation count of a single
// messageOverflowChars series.
func messageOverflowSampleCount(t *testing.T, namespace, name string) uint64 {
	t.Helper()
	obs, err := messageOverflowChars.GetMetricWithLabelValues(namespace, name)
	if err != nil {
		t.Fatalf(getHistogramMetricFmt, err)
	}
	var m dto.Metric
	if err := obs.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf(writeHistogramMetricFmt, err)
	}
	return m.GetHistogram().GetSampleCount()
}

// messageOverflowSampleSum reads the sum of observed values of a single
// messageOverflowChars series.
func messageOverflowSampleSum(t *testing.T, namespace, name string) float64 {
	t.Helper()
	obs, err := messageOverflowChars.GetMetricWithLabelValues(namespace, name)
	if err != nil {
		t.Fatalf(getHistogramMetricFmt, err)
	}
	var m dto.Metric
	if err := obs.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf(writeHistogramMetricFmt, err)
	}
	return m.GetHistogram().GetSampleSum()
}

// TestProcessFeed_RecordsMessageOverflow asserts that sending an entry whose
// rendered content exceeds Discord's message length limit records an
// observation on messageOverflowChars, so an operator can see how often a
// FeedGroup's content is actually getting cut off.
func TestProcessFeed_RecordsMessageOverflow(t *testing.T) {
	ctx := t.Context()
	ns, name := "metrics-overflow", "fg-overflow"
	defer deleteFeedGroupMetrics(ns, name)

	discordServer := NewMockDiscordServer()
	defer discordServer.Close()

	fg, feed := newMetricsFeedGroup(ns, name, "")
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	const articleURL = "https://example.com/long-article"
	fetchResult := rss.FetchResult{Entries: []rss.Entry{{
		ID:          articleURL,
		Title:       "Example entry",
		Description: strings.Repeat("x", maxDiscordMessageLength+500),
		Link:        articleURL,
	}}}

	(&FeedGroupReconciler{}).processFeed(ctx, fg, feed, fetchResult, nil, client, "2026-01-01T00:00:00Z")

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
	defer deleteFeedGroupMetrics(ns, name)

	discordServer := NewMockDiscordServer()
	defer discordServer.Close()

	fg, feed := newMetricsFeedGroup(ns, name, "")
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	(&FeedGroupReconciler{}).processFeed(ctx, fg, feed, oneEntryFetch(), nil, client, "2026-01-01T00:00:00Z")

	if got := messageOverflowSampleCount(t, ns, name); got != 0 {
		t.Fatalf("messageOverflowChars sample count = %d, want 0", got)
	}
}

// TestProcessFeed_RecordsMessageOverflowFromEmbedTotalLengthClamp asserts
// that messageOverflowChars also counts truncation caused by Discord's
// combined embed length limit (title+description+footer+author <= 6000),
// which is enforced inside the discord package (clampEmbedTotalLength)
// rather than by the per-field truncation buildDiscordMessage measures
// directly. Title and Description here individually stay well under their
// own limits, so only the combined-length clamp can be responsible for any
// recorded overflow.
func TestProcessFeed_RecordsMessageOverflowFromEmbedTotalLengthClamp(t *testing.T) {
	ctx := t.Context()
	ns, name := "metrics-embed-total-overflow", "fg-embed-total-overflow"
	defer deleteFeedGroupMetrics(ns, name)

	discordServer := NewMockDiscordServer()
	defer discordServer.Close()

	fg, feed := newMetricsFeedGroup(ns, name, "")
	feed.Embed = &v1alpha1.EmbedSpec{
		Enabled:    true,
		AuthorName: strings.Repeat("a", 3000),
		FooterText: strings.Repeat("f", 3500),
	}
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	(&FeedGroupReconciler{}).processFeed(ctx, fg, feed, oneEntryFetch(), nil, client, "2026-01-01T00:00:00Z")

	if got := messageOverflowSampleCount(t, ns, name); got != 1 {
		t.Fatalf("messageOverflowChars sample count = %d, want 1", got)
	}
}

// assertNativeOnlyHistogram fails the test unless m has a native histogram
// schema and no classic buckets, i.e. it isn't exposed twice (once as
// classic, once as native) for the same observations.
func assertNativeOnlyHistogram(t *testing.T, m *dto.Metric) {
	t.Helper()
	h := m.GetHistogram()
	if len(h.GetBucket()) != 0 {
		t.Fatalf("classic buckets present (%d), want none -- Buckets should be unset so this is native-only", len(h.GetBucket()))
	}
	if h.Schema == nil {
		t.Fatal("native histogram schema missing, want NativeHistogramBucketFactor to enable it")
	}
}

// assertSeriesNativeOnly checks that the series of vec with the given label
// values is exposed as a native-only histogram.
func assertSeriesNativeOnly(t *testing.T, vec *prometheus.HistogramVec, labelValues ...string) {
	t.Helper()
	obs, err := vec.GetMetricWithLabelValues(labelValues...)
	if err != nil {
		t.Fatalf(getHistogramMetricFmt, err)
	}
	var m dto.Metric
	if err := obs.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf(writeHistogramMetricFmt, err)
	}
	assertNativeOnlyHistogram(t, &m)
}

// TestFeedRequestDuration_NativeOnlyHistogram guards the native-only
// exposition config on feedRequestDuration: no classic buckets (so it isn't
// duplicated as a classic + native pair of dashboard panels) while the
// native histogram schema is populated.
func TestFeedRequestDuration_NativeOnlyHistogram(t *testing.T) {
	ns, name := "metrics-native-only", "fg-native-only"
	defer deleteFeedGroupMetrics(ns, name)

	feedRequestDuration.WithLabelValues(ns, name, operationFetch).Observe(0.2)

	assertSeriesNativeOnly(t, feedRequestDuration, ns, name, operationFetch)
}

// TestFeedGroupReconcileDuration_NativeOnlyHistogram mirrors
// TestFeedRequestDuration_NativeOnlyHistogram for feedGroupReconcileDuration.
func TestFeedGroupReconcileDuration_NativeOnlyHistogram(t *testing.T) {
	ns, name := "metrics-reconcile-native-only", "fg-reconcile-native-only"
	defer deleteFeedGroupMetrics(ns, name)

	feedGroupReconcileDuration.WithLabelValues(ns, name).Observe(0.2)

	assertSeriesNativeOnly(t, feedGroupReconcileDuration, ns, name)
}

// TestMessageOverflowChars_NativeOnlyHistogram mirrors
// TestFeedRequestDuration_NativeOnlyHistogram for messageOverflowChars.
func TestMessageOverflowChars_NativeOnlyHistogram(t *testing.T) {
	ns, name := "metrics-overflow-native-only", "fg-overflow-native-only"
	defer deleteFeedGroupMetrics(ns, name)

	messageOverflowChars.WithLabelValues(ns, name).Observe(42)

	assertSeriesNativeOnly(t, messageOverflowChars, ns, name)
}

// TestDeleteFeedGroupMetrics confirms a deleted FeedGroup's series are
// dropped, so a removed group can't leave a stale freshness reading behind.
func TestDeleteFeedGroupMetrics(t *testing.T) {
	ns, name := "metrics-delete", "fg-delete"

	feedOperationsTotal.WithLabelValues(ns, name, exampleFeedURL, outcomeSent).Inc()
	feedRequestDuration.WithLabelValues(ns, name, operationSend).Observe(0.1)
	feedLastSuccessTimestamp.WithLabelValues(ns, name).Set(123)
	feedHealthy.WithLabelValues(ns, name, exampleFeedURL).Set(1)
	feedLastCheckedTimestamp.WithLabelValues(ns, name, exampleFeedURL).Set(123)
	messageOverflowChars.WithLabelValues(ns, name).Observe(42)
	feedGroupReconcileDuration.WithLabelValues(ns, name).Observe(0.5)

	deleteFeedGroupMetrics(ns, name)

	// After deletion, re-fetching a series via WithLabelValues recreates it at
	// zero; a non-zero read would mean the original series was never removed.
	if got := testutil.ToFloat64(feedOperationsTotal.WithLabelValues(ns, name, exampleFeedURL, outcomeSent)); got != 0 {
		t.Fatalf("feedOperationsTotal after delete = %v, want 0", got)
	}
	if got := testutil.ToFloat64(feedLastSuccessTimestamp.WithLabelValues(ns, name)); got != 0 {
		t.Fatalf("feedLastSuccessTimestamp after delete = %v, want 0", got)
	}
	// The per-feed gauges are checked by presence rather than value, since a
	// recreated gauge reads 0 and 0 is a meaningful feedHealthy value.
	if _, ok := feedGaugeValue(feedHealthy, ns, name, exampleFeedURL); ok {
		t.Fatal("feedHealthy series still present after delete")
	}
	if _, ok := feedGaugeValue(feedLastCheckedTimestamp, ns, name, exampleFeedURL); ok {
		t.Fatal("feedLastCheckedTimestamp series still present after delete")
	}
	if got := histogramSampleCount(t, ns, name, operationSend); got != 0 {
		t.Fatalf("send duration sample count after delete = %d, want 0", got)
	}
	if got := messageOverflowSampleCount(t, ns, name); got != 0 {
		t.Fatalf("message overflow sample count after delete = %d, want 0", got)
	}
	if got := reconcileDurationSampleCount(t, ns, name); got != 0 {
		t.Fatalf("reconcile duration sample count after delete = %d, want 0", got)
	}
	deleteFeedGroupMetrics(ns, name)
}

// TestEnsureFeedStatuses_DropsRemovedFeedMetrics confirms that removing a
// feed from a FeedGroup's spec drops that feed's rss_url series, while the
// series of feeds still in the spec are kept.
func TestEnsureFeedStatuses_DropsRemovedFeedMetrics(t *testing.T) {
	ns, name := "metrics-prune", "fg-prune"
	const removedURL = "https://removed.example.com/feed.xml"
	defer deleteFeedGroupMetrics(ns, name)

	feedOperationsTotal.WithLabelValues(ns, name, exampleFeedURL, outcomeSent).Inc()
	feedOperationsTotal.WithLabelValues(ns, name, removedURL, outcomeSent).Inc()
	for _, url := range []string{exampleFeedURL, removedURL} {
		feedHealthy.WithLabelValues(ns, name, url).Set(0)
		feedLastCheckedTimestamp.WithLabelValues(ns, name, url).Set(123)
	}

	feedGroup := &v1alpha1.FeedGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       v1alpha1.FeedGroupSpec{Feeds: []v1alpha1.FeedSpec{{RSSUrl: exampleFeedURL}}},
		Status: v1alpha1.FeedGroupStatus{Feeds: []v1alpha1.FeedStatus{
			{RSSUrl: exampleFeedURL},
			{RSSUrl: removedURL},
		}},
	}
	ensureFeedStatuses(feedGroup)

	if got := testutil.ToFloat64(feedOperationsTotal.WithLabelValues(ns, name, removedURL, outcomeSent)); got != 0 {
		t.Fatalf("removed feed's series = %v, want 0 (dropped)", got)
	}
	if got := testutil.ToFloat64(feedOperationsTotal.WithLabelValues(ns, name, exampleFeedURL, outcomeSent)); got != 1 {
		t.Fatalf("kept feed's series = %v, want 1", got)
	}

	for _, vec := range []*prometheus.GaugeVec{feedHealthy, feedLastCheckedTimestamp} {
		if _, ok := feedGaugeValue(vec, ns, name, removedURL); ok {
			t.Fatal("removed feed's per-feed gauge series still present")
		}
		if _, ok := feedGaugeValue(vec, ns, name, exampleFeedURL); !ok {
			t.Fatal("kept feed's per-feed gauge series was dropped")
		}
	}
}

// feedGaugeValue reads the series of a per-feed gauge vec without creating
// it, reporting whether it exists. WithLabelValues can't be used for presence
// checks because it creates the series it is asked for.
func feedGaugeValue(vec *prometheus.GaugeVec, namespace, name, rssURL string) (float64, bool) {
	ch := make(chan prometheus.Metric, 64)
	go func() {
		vec.Collect(ch)
		close(ch)
	}()
	var (
		value float64
		found bool
	)
	for metric := range ch {
		var m dto.Metric
		if err := metric.Write(&m); err != nil {
			continue
		}
		labels := map[string]string{}
		for _, lp := range m.GetLabel() {
			labels[lp.GetName()] = lp.GetValue()
		}
		if labels[labelNamespace] == namespace && labels[labelName] == name && labels[labelFeedURL] == rssURL {
			value, found = m.GetGauge().GetValue(), true
		}
	}
	return value, found
}

// TestRecordFeedStateMetrics covers the per-feed health/freshness gauges
// derived from persisted status: healthy tracks LastError (including for a
// feed in permanent backoff, which is what keeps alerts alive while its
// operations counter is flat), the timestamp parses LastChecked, and series
// are omitted -- not zeroed -- when there is nothing truthful to report.
func TestRecordFeedStateMetrics(t *testing.T) {
	const (
		healthyURL   = "https://example.com/healthy.xml"
		backoffURL   = "https://example.com/backoff.xml"
		neverURL     = "https://example.com/never.xml"
		badStampURL  = "https://example.com/badstamp.xml"
		pausedURL    = "https://example.com/paused.xml"
		checkedStamp = "2026-03-04T05:06:07Z"
	)
	ns, name := "metrics-feedstate", "fg-feedstate"
	defer deleteFeedGroupMetrics(ns, name)

	fg := &v1alpha1.FeedGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: v1alpha1.FeedGroupSpec{Feeds: []v1alpha1.FeedSpec{
			{RSSUrl: healthyURL}, {RSSUrl: backoffURL}, {RSSUrl: neverURL},
			{RSSUrl: badStampURL}, {RSSUrl: pausedURL, Paused: true},
		}},
		Status: v1alpha1.FeedGroupStatus{Feeds: []v1alpha1.FeedStatus{
			{RSSUrl: healthyURL, LastChecked: checkedStamp},
			{RSSUrl: backoffURL, LastChecked: checkedStamp, LastError: "http 404", RetryCount: 9, BackoffUntil: "2099-01-01T00:00:00Z"},
			// Never checked: LastChecked is optional and empty.
			{RSSUrl: neverURL},
			{RSSUrl: badStampURL, LastChecked: "not-a-time"},
			{RSSUrl: pausedURL, LastChecked: checkedStamp, LastError: "http 404"},
		}},
	}
	// Series that existed before the feed was paused / lost its timestamp must
	// be removed, not left frozen.
	feedHealthy.WithLabelValues(ns, name, pausedURL).Set(0)
	feedLastCheckedTimestamp.WithLabelValues(ns, name, pausedURL).Set(1)
	feedLastCheckedTimestamp.WithLabelValues(ns, name, badStampURL).Set(1)

	recordFeedStateMetrics(fg)

	wantStamp := float64(time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC).Unix())
	cases := []struct {
		url         string
		wantHealthy float64 // -1: no series expected
		wantStamp   float64 // -1: no series expected
	}{
		{healthyURL, 1, wantStamp},
		{backoffURL, 0, wantStamp},
		{neverURL, 1, -1},
		{badStampURL, 1, -1},
		{pausedURL, -1, -1},
	}
	for _, tc := range cases {
		check := func(vec *prometheus.GaugeVec, what string, want float64) {
			got, ok := feedGaugeValue(vec, ns, name, tc.url)
			switch {
			case want < 0 && ok:
				t.Errorf("%s %s = %v, want no series", what, tc.url, got)
			case want >= 0 && !ok:
				t.Errorf("%s %s: no series, want %v", what, tc.url, want)
			case want >= 0 && got != want:
				t.Errorf("%s %s = %v, want %v", what, tc.url, got, want)
			}
		}
		check(feedHealthy, "feedHealthy", tc.wantHealthy)
		check(feedLastCheckedTimestamp, "feedLastCheckedTimestamp", tc.wantStamp)
	}

	// A feed that recovers flips back to 1 on the next pass.
	fg.Status.Feeds[1].LastError = ""
	recordFeedStateMetrics(fg)
	if got, _ := feedGaugeValue(feedHealthy, ns, name, backoffURL); got != 1 {
		t.Errorf("feedHealthy after recovery = %v, want 1", got)
	}
}

// TestRequeueWithStatus_RecordsFeedStateOnNoOpReconcile asserts the per-feed
// gauges are refreshed even when requeueWithStatus short-circuits because
// status is unchanged -- the common case for a feed idling in backoff -- so
// they're populated after an operator restart without waiting for a change.
func TestRequeueWithStatus_RecordsFeedStateOnNoOpReconcile(t *testing.T) {
	ns, name := "metrics-noop", "fg-noop"
	defer deleteFeedGroupMetrics(ns, name)

	fg, _ := newMetricsFeedGroup(ns, name, "")
	fg.Status.Feeds[0].LastError = "http 410"
	fg.Status.Feeds[0].LastChecked = "2026-03-04T05:06:08Z"
	// Apply the idempotent condition/generation updates requeueWithStatus makes
	// up front so the snapshot below equals the final status and no API write
	// (and therefore no client) is needed.
	fg.Status.ObservedGeneration = fg.Generation
	setReadyCondition(fg, nil, nil)
	setFeedReachableCondition(fg)
	original := fg.Status.DeepCopy()

	if _, err := (&FeedGroupReconciler{}).requeueWithStatus(t.Context(), fg, original, "30m", 30*time.Minute, nil, false); err != nil {
		t.Fatalf("requeueWithStatus: %v", err)
	}

	if got, ok := feedGaugeValue(feedHealthy, ns, name, exampleFeedURL); !ok || got != 0 {
		t.Fatalf("feedHealthy = %v (present=%v), want 0", got, ok)
	}
	if _, ok := feedGaugeValue(feedLastCheckedTimestamp, ns, name, exampleFeedURL); !ok {
		t.Fatal("feedLastCheckedTimestamp series missing")
	}
}

// reconcileDurationSampleCount reads the observation count of a single
// feedGroupReconcileDuration series.
func reconcileDurationSampleCount(t *testing.T, namespace, name string) uint64 {
	t.Helper()
	count, err := reconcileDurationSampleCountErr(namespace, name)
	if err != nil {
		t.Fatalf(getHistogramMetricFmt, err)
	}
	return count
}

// reconcileDurationSampleCountErr is the error-returning core of
// reconcileDurationSampleCount, usable from contexts (like Ginkgo specs)
// that don't have a *testing.T to call Fatalf on.
func reconcileDurationSampleCountErr(namespace, name string) (uint64, error) {
	obs, err := feedGroupReconcileDuration.GetMetricWithLabelValues(namespace, name)
	if err != nil {
		return 0, err
	}
	var m dto.Metric
	if err := obs.(prometheus.Metric).Write(&m); err != nil {
		return 0, err
	}
	return m.GetHistogram().GetSampleCount(), nil
}

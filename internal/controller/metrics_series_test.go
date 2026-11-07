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
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1beta1 "github.com/maverickd650-labs/rss2discord/api/v1beta1"
)

const otherFeedURL = "https://other.example.com/feed.xml"

// seriesLabels is the full label set of one series of vec for a feed.
func seriesLabels(vec metricVec, scope metricScope, key seriesKey) prometheus.Labels {
	labels := prometheus.Labels(key.groupSelector())
	switch {
	case vec == metricVec(feedOperationsTotal):
		labels[labelFeed], labels[labelOutcome] = key.feed, outcomeSent
	case vec == metricVec(feedRequestDuration):
		labels[labelOperation] = operationSend
	case scope == scopePerFeed:
		labels[labelFeed] = key.feed
	}
	return labels
}

// populateAllMetrics creates, for every vector in allMetrics, one series per
// feed of the group (the example feed and otherFeedURL).
func populateAllMetrics(t *testing.T, group seriesKey) {
	t.Helper()
	for _, e := range allMetrics {
		for _, url := range []string{exampleFeedURL, otherFeedURL} {
			touchSeries(t, e.vec, seriesLabels(e.vec, e.scope, group.withFeed(url)))
		}
	}
}

// assertEveryMetricSeries fails for each vector in allMetrics whose series
// matching sel are present when none are wanted, or absent when some are.
func assertEveryMetricSeries(t *testing.T, sel map[string]string, wantPresent bool, msg string) {
	t.Helper()
	for i, e := range allMetrics {
		if got := countSeries(e.vec, sel); (got > 0) != wantPresent {
			t.Errorf("allMetrics[%d] (%T): %s (%d series match)", i, e.vec, msg, got)
		}
	}
}

// TestGroupMetrics_ForgetDropsEverySeries walks allMetrics, so a vector added
// to the table is covered automatically: forget drops every vector's series for
// the group.
func TestGroupMetrics_ForgetDropsEverySeries(t *testing.T) {
	group := seriesKey{namespace: "metrics-forget", group: "fg-forget"}
	forgetMetricsAfter(t, group.namespace, group.group)
	populateAllMetrics(t, group)
	assertEveryMetricSeries(t, group.groupSelector(), true, "no series was created")

	groupMetrics{namespace: group.namespace, name: group.group}.forget()

	assertEveryMetricSeries(t, group.groupSelector(), false, "series survive forget")
}

// vectorsOfScope is the allMetrics vectors of the given scope.
func vectorsOfScope(scope metricScope) []metricVec {
	var vecs []metricVec
	for _, e := range allMetrics {
		if e.scope == scope {
			vecs = append(vecs, e.vec)
		}
	}
	return vecs
}

// assertSeriesCounts fails for each vec unless it has series matching sel
// exactly when wantPresent.
func assertSeriesCounts(t *testing.T, vecs []metricVec, sel map[string]string, wantPresent bool) {
	t.Helper()
	for _, vec := range vecs {
		if got := countSeries(vec, sel); (got > 0) != wantPresent {
			t.Errorf("%T: %d series match %v, want present=%v", vec, got, sel, wantPresent)
		}
	}
}

// TestGroupMetrics_ForgetFeedDropsOnlyThatFeed asserts forgetFeed drops exactly
// the per-feed vectors' series for one feed while leaving the group's other
// series and the sibling feed's alone.
func TestGroupMetrics_ForgetFeedDropsOnlyThatFeed(t *testing.T) {
	group := seriesKey{namespace: "metrics-forget-feed", group: "fg-forget-feed"}
	forgetMetricsAfter(t, group.namespace, group.group)
	populateAllMetrics(t, group)
	forgotten, sibling := group.withFeed(exampleFeedURL), group.withFeed(otherFeedURL)

	groupMetrics{namespace: group.namespace, name: group.group}.forgetFeed(exampleFeedURL)

	perFeed, perGroup := vectorsOfScope(scopePerFeed), vectorsOfScope(scopePerGroup)
	assertSeriesCounts(t, perFeed, forgotten.feedSelector(), false)
	assertSeriesCounts(t, perFeed, sibling.feedSelector(), true)
	assertSeriesCounts(t, perGroup, group.groupSelector(), true)
}

// requireGaugeSeries fails unless the presence of the key's feed series in a
// per-feed gauge matches want.
func requireGaugeSeries(t *testing.T, vec *prometheus.GaugeVec, key seriesKey, want bool) {
	t.Helper()
	if _, ok := key.gaugeValue(vec); ok != want {
		t.Fatalf("series for %s present=%v, want %v", key.feed, ok, want)
	}
}

// TestEnsureFeedStatuses_ReportsPrunedFeeds confirms that removing a feed from
// a FeedGroup's spec is reported to the caller (which drops that feed's
// series via forgetFeed), that ensureFeedStatuses itself leaves
// Prometheus alone, and that forgetting the reported feed keeps the series of
// feeds still in the spec.
func TestEnsureFeedStatuses_ReportsPrunedFeeds(t *testing.T) {
	ns, name := "metrics-prune", "fg-prune"
	const removedURL = "https://removed.example.com/feed.xml"
	group := seriesKey{namespace: ns, group: name}
	kept, removed := group.forFeedURL(exampleFeedURL), group.forFeedURL(removedURL)
	keptName, removedName := kept.feed, removed.feed
	handle := groupMetrics{namespace: ns, name: name}
	defer handle.forget()

	handle.operation(keptName, outcomeSent)
	handle.operation(removedName, outcomeSent)
	for _, feedName := range []string{keptName, removedName} {
		feedHealthy.WithLabelValues(ns, name, feedName).Set(0)
		feedLastCheckedTimestamp.WithLabelValues(ns, name, feedName).Set(123)
	}

	feedGroup := &v1beta1.FeedGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       v1beta1.FeedGroupSpec{Feeds: []v1beta1.FeedSpec{{Name: keptName, URL: exampleFeedURL}}},
		Status: v1beta1.FeedGroupStatus{Feeds: []v1beta1.FeedStatus{
			{Name: keptName, URL: exampleFeedURL},
			{Name: removedName, URL: removedURL},
		}},
	}
	pruned := ensureFeedStatuses(feedGroup)

	if len(pruned) != 1 || pruned[0] != removedName {
		t.Fatalf("ensureFeedStatuses pruned = %v, want [%s]", pruned, removedName)
	}
	requireGaugeSeries(t, feedHealthy, removed, true) // ensureFeedStatuses must not touch Prometheus

	for _, feedName := range pruned {
		handle.forgetFeed(feedName)
	}

	assertOperationCount(t, removed, outcomeSent, 0)
	assertOperationCount(t, kept, outcomeSent, 1)
	for _, vec := range []*prometheus.GaugeVec{feedHealthy, feedLastCheckedTimestamp} {
		requireGaugeSeries(t, vec, removed, false)
		requireGaugeSeries(t, vec, kept, true)
	}
}

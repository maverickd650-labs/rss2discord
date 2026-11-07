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

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1beta1 "github.com/maverickd650-labs/rss2discord/api/v1beta1"
)

const otherFeedURL = "https://other.example.com/feed.xml"

// seriesLabels is the full label set of one series of vec for a feed.
func seriesLabels(vec metricVec, scope metricScope, ns, name, url string) prometheus.Labels {
	labels := prometheus.Labels{labelNamespace: ns, labelName: name}
	switch {
	case vec == metricVec(feedOperationsTotal):
		labels[labelFeed], labels[labelOutcome] = url, outcomeSent
	case vec == metricVec(feedRequestDuration):
		labels[labelOperation] = operationSend
	case scope == scopePerFeed:
		labels[labelFeed] = url
	}
	return labels
}

// populateAllMetrics creates, for every vector in allMetrics, one series per
// feed of the group (the example feed and otherFeedURL).
func populateAllMetrics(t *testing.T, ns, name string) {
	t.Helper()
	for _, e := range allMetrics {
		for _, url := range []string{exampleFeedURL, otherFeedURL} {
			touchSeries(t, e.vec, seriesLabels(e.vec, e.scope, ns, name, url))
		}
	}
}

func groupSelector(ns, name string) map[string]string {
	return map[string]string{labelNamespace: ns, labelName: name}
}

func feedSelector(ns, name, url string) map[string]string {
	return map[string]string{labelNamespace: ns, labelName: name, labelFeed: url}
}

// TestGroupMetrics_ForgetDropsEverySeries walks allMetrics, so a vector added
// to the table is covered automatically: forget drops every vector's series for
// the group.
func TestGroupMetrics_ForgetDropsEverySeries(t *testing.T) {
	ns, name := "metrics-forget", "fg-forget"
	forgetMetricsAfter(t, ns, name)
	populateAllMetrics(t, ns, name)
	for i, e := range allMetrics {
		if countSeries(e.vec, groupSelector(ns, name)) == 0 {
			t.Fatalf("allMetrics[%d] (%T): no series was created", i, e.vec)
		}
	}

	groupMetrics{namespace: ns, name: name}.forget()

	for i, e := range allMetrics {
		if got := countSeries(e.vec, groupSelector(ns, name)); got != 0 {
			t.Errorf("allMetrics[%d] (%T): %d series survive forget", i, e.vec, got)
		}
	}
}

// TestGroupMetrics_ForgetFeedDropsOnlyThatFeed asserts forgetFeed drops exactly
// the per-feed vectors' series for one feed while leaving the group's other
// series and the sibling feed's alone.
func TestGroupMetrics_ForgetFeedDropsOnlyThatFeed(t *testing.T) {
	ns, name := "metrics-forget-feed", "fg-forget-feed"
	forgetMetricsAfter(t, ns, name)
	populateAllMetrics(t, ns, name)

	groupMetrics{namespace: ns, name: name}.forgetFeed(exampleFeedURL)

	for i, e := range allMetrics {
		label := fmt.Sprintf("allMetrics[%d] (%T)", i, e.vec)
		switch e.scope {
		case scopePerFeed:
			if got := countSeries(e.vec, feedSelector(ns, name, exampleFeedURL)); got != 0 {
				t.Errorf("%s: %d series for the forgotten feed survive", label, got)
			}
			if countSeries(e.vec, feedSelector(ns, name, otherFeedURL)) == 0 {
				t.Errorf("%s: the sibling feed's series were dropped", label)
			}
		case scopePerGroup:
			if countSeries(e.vec, groupSelector(ns, name)) == 0 {
				t.Errorf("%s: per-group series were dropped by forgetFeed", label)
			}
		}
	}
}

// requireGaugeSeries fails unless the presence of the feed's series in a
// per-feed gauge matches want.
func requireGaugeSeries(t *testing.T, vec *prometheus.GaugeVec, ns, name, feedName string, want bool, msg string) {
	t.Helper()
	if _, ok := feedGaugeValue(vec, ns, name, feedName); ok != want {
		t.Fatalf("%s: series for %s present=%v, want %v", msg, feedName, ok, want)
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
	keptName, removedName := testFeedName(exampleFeedURL), testFeedName(removedURL)
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
	requireGaugeSeries(t, feedHealthy, ns, name, removedName, true, "ensureFeedStatuses must not touch Prometheus")

	for _, feedName := range pruned {
		handle.forgetFeed(feedName)
	}

	assertOperationCount(t, ns, name, removedName, outcomeSent, 0)
	assertOperationCount(t, ns, name, keptName, outcomeSent, 1)
	for _, vec := range []*prometheus.GaugeVec{feedHealthy, feedLastCheckedTimestamp} {
		requireGaugeSeries(t, vec, ns, name, removedName, false, "removed feed's per-feed gauge")
		requireGaugeSeries(t, vec, ns, name, keptName, true, "kept feed's per-feed gauge")
	}
}

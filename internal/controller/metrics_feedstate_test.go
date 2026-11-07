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
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1beta1 "github.com/maverickd650-labs/rss2discord/api/v1beta1"
)

const (
	healthyURL  = "https://example.com/healthy.xml"
	backoffURL  = "https://example.com/backoff.xml"
	neverURL    = "https://example.com/never.xml"
	badStampURL = "https://example.com/badstamp.xml"
	pausedURL   = "https://example.com/paused.xml"

	// noSeries as an expected gauge value means no series should exist.
	noSeries = -1.0
)

// assertGaugeSeries checks one feed's series in a per-feed gauge: want is the
// expected value, or noSeries when the series must be absent.
func assertGaugeSeries(t *testing.T, vec *prometheus.GaugeVec, what, ns, name, url string, want float64) {
	t.Helper()
	got, ok := feedGaugeValue(vec, ns, name, testFeedName(url))
	if want == noSeries {
		if ok {
			t.Errorf("%s %s = %v, want no series", what, url, got)
		}
		return
	}
	if !ok {
		t.Errorf("%s %s: no series, want %v", what, url, want)
		return
	}
	if got != want {
		t.Errorf("%s %s = %v, want %v", what, url, got, want)
	}
}

// newFeedStateFixture builds a FeedGroup with one feed per state the per-feed
// gauges distinguish (healthy, failing in permanent backoff, never checked,
// unparsable timestamp, paused), and seeds series that must be removed, not
// left frozen, once the feed is paused / loses its timestamp.
func newFeedStateFixture(t *testing.T, ns, name string) *v1beta1.FeedGroup {
	t.Helper()
	forgetMetricsAfter(t, ns, name)
	checkedStamp := metav1TimeRFC3339("2026-03-04T05:06:07Z")

	fg := &v1beta1.FeedGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: v1beta1.FeedGroupSpec{Feeds: []v1beta1.FeedSpec{
			{Name: testFeedName(healthyURL), URL: healthyURL}, {Name: testFeedName(backoffURL), URL: backoffURL}, {Name: testFeedName(neverURL), URL: neverURL},
			{Name: testFeedName(badStampURL), URL: badStampURL}, {Name: testFeedName(pausedURL), URL: pausedURL, Suspend: true},
		}},
		Status: v1beta1.FeedGroupStatus{Feeds: []v1beta1.FeedStatus{
			{Name: testFeedName(healthyURL), URL: healthyURL, LastChecked: checkedStamp},
			{Name: testFeedName(backoffURL), URL: backoffURL, LastChecked: checkedStamp, LastError: "http 404", RetryCount: 9, BackoffUntil: metav1TimeRFC3339("2099-01-01T00:00:00Z")},
			// Never checked: LastChecked is optional and empty.
			{Name: testFeedName(neverURL), URL: neverURL},
			{Name: testFeedName(badStampURL), URL: badStampURL, LastChecked: &metav1.Time{}},
			{Name: testFeedName(pausedURL), URL: pausedURL, LastChecked: checkedStamp, LastError: "http 404"},
		}},
	}
	feedHealthy.WithLabelValues(ns, name, testFeedName(pausedURL)).Set(0)
	feedLastCheckedTimestamp.WithLabelValues(ns, name, testFeedName(pausedURL)).Set(1)
	feedLastCheckedTimestamp.WithLabelValues(ns, name, testFeedName(badStampURL)).Set(1)
	return fg
}

// TestRecordFeedStateMetrics covers the per-feed health/freshness gauges
// derived from persisted status: healthy tracks LastError (including for a
// feed in permanent backoff, which is what keeps alerts alive while its
// operations counter is flat), the timestamp parses LastChecked, and series
// are omitted -- not zeroed -- when there is nothing truthful to report.
func TestRecordFeedStateMetrics(t *testing.T) {
	ns, name := "metrics-feedstate", "fg-feedstate"
	fg := newFeedStateFixture(t, ns, name)

	metricsFor(fg).syncFeedState(fg)

	wantStamp := float64(time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC).Unix())
	cases := []struct {
		url         string
		wantHealthy float64
		wantStamp   float64
	}{
		{healthyURL, 1, wantStamp},
		{backoffURL, 0, wantStamp},
		{neverURL, 1, noSeries},
		{badStampURL, 1, noSeries},
		{pausedURL, noSeries, noSeries},
	}
	for _, tc := range cases {
		assertGaugeSeries(t, feedHealthy, "feedHealthy", ns, name, tc.url, tc.wantHealthy)
		assertGaugeSeries(t, feedLastCheckedTimestamp, "feedLastCheckedTimestamp", ns, name, tc.url, tc.wantStamp)
	}
}

// TestRecordFeedStateMetrics_RecoveryFlipsHealthy asserts a feed that recovers
// flips back to healthy on the next pass.
func TestRecordFeedStateMetrics_RecoveryFlipsHealthy(t *testing.T) {
	ns, name := "metrics-feedstate-recover", "fg-feedstate-recover"
	fg := newFeedStateFixture(t, ns, name)
	metricsFor(fg).syncFeedState(fg)

	fg.Status.Feeds[1].LastError = ""
	metricsFor(fg).syncFeedState(fg)

	assertGaugeSeries(t, feedHealthy, "feedHealthy after recovery", ns, name, backoffURL, 1)
}

// TestRecordFeedStateMetrics_SuspendedGroupDropsSeries asserts group
// suspension drops per-feed metrics like paused feeds.
func TestRecordFeedStateMetrics_SuspendedGroupDropsSeries(t *testing.T) {
	ns, name := "metrics-feedstate-suspend", "fg-feedstate-suspend"
	fg := newFeedStateFixture(t, ns, name)
	metricsFor(fg).syncFeedState(fg)

	fg.Spec.Suspend = true
	metricsFor(fg).syncFeedState(fg)

	assertGaugeSeries(t, feedHealthy, "feedHealthy (suspended group)", ns, name, healthyURL, noSeries)
	assertGaugeSeries(t, feedLastCheckedTimestamp, "feedLastCheckedTimestamp (suspended group)", ns, name, healthyURL, noSeries)
}

// TestRequeueWithStatus_RecordsFeedStateOnNoOpReconcile asserts the per-feed
// gauges are refreshed even when groupSync settlement short-circuits because
// status is unchanged -- the common case for a feed idling in backoff -- so
// they're populated after an operator restart without waiting for a change.
func TestRequeueWithStatus_RecordsFeedStateOnNoOpReconcile(t *testing.T) {
	ns, name := "metrics-noop", "fg-noop"
	forgetMetricsAfter(t, ns, name)

	fg, _ := newMetricsFeedGroup(ns, name, "")
	fg.Status.Feeds[0].LastError = "http 410"
	fg.Status.Feeds[0].LastChecked = metav1TimeRFC3339("2026-03-04T05:06:08Z")
	// Apply the idempotent condition/generation updates groupSync settlement makes
	// up front so the snapshot openGroupSync captures equals the final status and no API write
	// (and therefore no client) is needed.
	fg.Status.ObservedGeneration = fg.Generation
	setReadyCondition(fg, nil, nil)
	setFeedReachableCondition(fg)

	sync := (&FeedGroupReconciler{}).openGroupSync(fg)
	if _, err := sync.settlePass(t.Context(), feedPlan{nextDue: 30 * time.Minute}, attemptVerdict{}); err != nil {
		t.Fatalf("settlePass: %v", err)
	}

	assertGaugeSeries(t, feedHealthy, "feedHealthy", ns, name, exampleFeedURL, 0)
	assertGaugeSeries(t, feedLastCheckedTimestamp, "feedLastCheckedTimestamp", ns, name, exampleFeedURL, float64(time.Date(2026, 3, 4, 5, 6, 8, 0, time.UTC).Unix()))
}

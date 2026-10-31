package controller

import "testing"

// A status entry without an rssUrl has no label set to report under, so
// syncFeedState skips it instead of creating a series with an empty url.
func TestSyncFeedState_SkipsStatusWithoutURL(t *testing.T) {
	ns, name := "metrics-nourl", "fg-nourl"
	defer groupMetrics{namespace: ns, name: name}.forget()

	fg, _ := newMetricsFeedGroup(ns, name, "")
	// Set before the copy, so the empty-url entry has a valid timestamp too and
	// the timestamp assertion below would see a series if the skip were gone.
	fg.Status.Feeds[0].LastChecked = "2026-03-04T05:06:07Z"
	fg.Status.Feeds = append(fg.Status.Feeds, fg.Status.Feeds[0])
	fg.Status.Feeds[1].RSSUrl = ""

	metricsFor(fg).syncFeedState(fg)

	if _, ok := feedGaugeValue(feedHealthy, ns, name, ""); ok {
		t.Error("feedHealthy has a series for an empty url, want none")
	}
	if _, ok := feedGaugeValue(feedLastCheckedTimestamp, ns, name, ""); ok {
		t.Error("feedLastCheckedTimestamp has a series for an empty url, want none")
	}
	if got, ok := feedGaugeValue(feedHealthy, ns, name, exampleFeedURL); !ok || got != 1 {
		t.Errorf("feedHealthy for the real feed = %v (present=%v), want 1", got, ok)
	}
}

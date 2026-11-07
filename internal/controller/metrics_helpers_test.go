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
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
)

// observerVec is the part of a *prometheus.HistogramVec the readers below use.
type observerVec interface {
	GetMetricWithLabelValues(lvs ...string) (prometheus.Observer, error)
}

// readHistogramErr reads back a single histogram series, usable from contexts
// (like Ginkgo specs) that don't have a *testing.T to call Fatal on.
func readHistogramErr(vec observerVec, lvs ...string) (*dto.Histogram, error) {
	obs, err := vec.GetMetricWithLabelValues(lvs...)
	if err != nil {
		return nil, fmt.Errorf("get histogram metric: %w", err)
	}
	var m dto.Metric
	if err := obs.(prometheus.Metric).Write(&m); err != nil {
		return nil, fmt.Errorf("write histogram metric: %w", err)
	}
	return m.GetHistogram(), nil
}

// readHistogram is readHistogramErr failing the test on error.
func readHistogram(t *testing.T, vec observerVec, lvs ...string) *dto.Histogram {
	t.Helper()
	h, err := readHistogramErr(vec, lvs...)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// histogramSampleCount reads the observation count of a single
// feedRequestDuration series.
func histogramSampleCount(t *testing.T, namespace, name, operation string) uint64 {
	t.Helper()
	return readHistogram(t, feedRequestDuration, namespace, name, operation).GetSampleCount()
}

// messageOverflowSampleCount reads the observation count of a single
// messageOverflowChars series.
func messageOverflowSampleCount(t *testing.T, namespace, name string) uint64 {
	t.Helper()
	return readHistogram(t, messageOverflowChars, namespace, name).GetSampleCount()
}

// messageOverflowSampleSum reads the sum of observed values of a single
// messageOverflowChars series.
func messageOverflowSampleSum(t *testing.T, namespace, name string) float64 {
	t.Helper()
	return readHistogram(t, messageOverflowChars, namespace, name).GetSampleSum()
}

// reconcileDurationSampleCountErr reads the observation count of a single
// feedGroupReconcileDuration series, usable from contexts (like Ginkgo specs)
// that don't have a *testing.T to call Fatalf on.
func reconcileDurationSampleCountErr(namespace, name string) (uint64, error) {
	h, err := readHistogramErr(feedGroupReconcileDuration, namespace, name)
	if err != nil {
		return 0, err
	}
	return h.GetSampleCount(), nil
}

// seriesKey identifies a FeedGroup's series (feed is empty for a group-level
// one), so helpers take one value instead of several strings.
type seriesKey struct {
	namespace, group, feed string
}

// forFeedURL is the key of the feed that testFeedName derives from url.
func (k seriesKey) forFeedURL(url string) seriesKey {
	k.feed = testFeedName(url)
	return k
}

// groupSelector matches every series of the group.
func (k seriesKey) groupSelector() map[string]string {
	return map[string]string{labelNamespace: k.namespace, labelName: k.group}
}

// feedSelector matches every series of the key's feed.
func (k seriesKey) feedSelector() map[string]string {
	sel := k.groupSelector()
	sel[labelFeed] = k.feed
	return sel
}

// assertOperationCount fails unless feedOperationsTotal for the key's feed and
// outcome reads want.
func assertOperationCount(t *testing.T, key seriesKey, outcome string, want float64) {
	t.Helper()
	if got := testutil.ToFloat64(feedOperationsTotal.WithLabelValues(key.namespace, key.group, key.feed, outcome)); got != want {
		t.Fatalf("feedOperationsTotal{feed=%q,outcome=%q} = %v, want %v", key.feed, outcome, got, want)
	}
}

// collectMetrics gathers every series of c without creating any
// (WithLabelValues would).
func collectMetrics(c prometheus.Collector) []*dto.Metric {
	ch := make(chan prometheus.Metric, 64)
	go func() {
		c.Collect(ch)
		close(ch)
	}()
	var out []*dto.Metric
	for metric := range ch {
		m := &dto.Metric{}
		if metric.Write(m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// labelsOf is a series' labels as a map.
func labelsOf(m *dto.Metric) map[string]string {
	labels := map[string]string{}
	for _, lp := range m.GetLabel() {
		labels[lp.GetName()] = lp.GetValue()
	}
	return labels
}

// hasLabels reports whether got carries every label in want.
func hasLabels(got, want map[string]string) bool {
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

// countSeries counts the series of c whose labels include all of want.
func countSeries(c prometheus.Collector, want map[string]string) int {
	count := 0
	for _, m := range collectMetrics(c) {
		if hasLabels(labelsOf(m), want) {
			count++
		}
	}
	return count
}

// feedGaugeValue reads the series of a per-feed gauge vec without creating
// it, reporting whether it exists. WithLabelValues can't be used for presence
// checks because it creates the series it is asked for.
func feedGaugeValue(vec *prometheus.GaugeVec, namespace, name, feedID string) (float64, bool) {
	return seriesKey{namespace, name, feedID}.gaugeValue(vec)
}

// gaugeValue is feedGaugeValue for a key.
func (k seriesKey) gaugeValue(vec *prometheus.GaugeVec) (float64, bool) {
	want := k.feedSelector()
	for _, m := range collectMetrics(vec) {
		if hasLabels(labelsOf(m), want) {
			return m.GetGauge().GetValue(), true
		}
	}
	return 0, false
}

// touchSeries creates one series in vec for labels, whatever kind of vector it
// is, so the forget test can cover every entry of allMetrics without naming
// them.
func touchSeries(t *testing.T, vec metricVec, labels prometheus.Labels) {
	t.Helper()
	switch v := vec.(type) {
	case *prometheus.CounterVec:
		v.With(labels).Inc()
	case *prometheus.GaugeVec:
		v.With(labels).Set(1)
	case *prometheus.HistogramVec:
		v.With(labels).Observe(1)
	default:
		t.Fatalf("touchSeries: unsupported vector type %T", vec)
	}
}

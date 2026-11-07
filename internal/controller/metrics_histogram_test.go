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
func histogramSampleCount(t *testing.T, key seriesKey, operation string) uint64 {
	t.Helper()
	return readHistogram(t, feedRequestDuration, key.namespace, key.group, operation).GetSampleCount()
}

// messageOverflowSampleCount reads the observation count of a single
// messageOverflowChars series.
func messageOverflowSampleCount(t *testing.T, key seriesKey) uint64 {
	t.Helper()
	return readHistogram(t, messageOverflowChars, key.namespace, key.group).GetSampleCount()
}

// messageOverflowSampleSum reads the sum of observed values of a single
// messageOverflowChars series.
func messageOverflowSampleSum(t *testing.T, key seriesKey) float64 {
	t.Helper()
	return readHistogram(t, messageOverflowChars, key.namespace, key.group).GetSampleSum()
}

// reconcileDurationSampleCountErr reads the observation count of a single
// feedGroupReconcileDuration series, usable from contexts (like Ginkgo specs)
// that don't have a *testing.T to call Fatalf on.
func reconcileDurationSampleCountErr(key seriesKey) (uint64, error) {
	h, err := readHistogramErr(feedGroupReconcileDuration, key.namespace, key.group)
	if err != nil {
		return 0, err
	}
	return h.GetSampleCount(), nil
}

// assertSeriesNativeOnly fails the test unless the series of vec with the given
// label values has a native histogram schema and no classic buckets, i.e. it
// isn't exposed twice (once as classic, once as native) for the same
// observations.
func assertSeriesNativeOnly(t *testing.T, vec *prometheus.HistogramVec, labelValues ...string) {
	t.Helper()
	h := readHistogram(t, vec, labelValues...)
	if len(h.GetBucket()) != 0 {
		t.Fatalf("classic buckets present (%d), want none -- Buckets should be unset so this is native-only", len(h.GetBucket()))
	}
	if h.Schema == nil {
		t.Fatal("native histogram schema missing, want NativeHistogramBucketFactor to enable it")
	}
}

// TestFeedRequestDuration_NativeOnlyHistogram guards the native-only
// exposition config on feedRequestDuration: no classic buckets (so it isn't
// duplicated as a classic + native pair of dashboard panels) while the
// native histogram schema is populated.
func TestFeedRequestDuration_NativeOnlyHistogram(t *testing.T) {
	ns, name := "metrics-native-only", "fg-native-only"
	forgetMetricsAfter(t, ns, name)

	feedRequestDuration.WithLabelValues(ns, name, operationFetch).Observe(0.2)

	assertSeriesNativeOnly(t, feedRequestDuration, ns, name, operationFetch)
}

// TestFeedGroupReconcileDuration_NativeOnlyHistogram mirrors
// TestFeedRequestDuration_NativeOnlyHistogram for feedGroupReconcileDuration.
func TestFeedGroupReconcileDuration_NativeOnlyHistogram(t *testing.T) {
	ns, name := "metrics-reconcile-native-only", "fg-reconcile-native-only"
	forgetMetricsAfter(t, ns, name)

	feedGroupReconcileDuration.WithLabelValues(ns, name).Observe(0.2)

	assertSeriesNativeOnly(t, feedGroupReconcileDuration, ns, name)
}

// TestMessageOverflowChars_NativeOnlyHistogram mirrors
// TestFeedRequestDuration_NativeOnlyHistogram for messageOverflowChars.
func TestMessageOverflowChars_NativeOnlyHistogram(t *testing.T) {
	ns, name := "metrics-overflow-native-only", "fg-overflow-native-only"
	forgetMetricsAfter(t, ns, name)

	messageOverflowChars.WithLabelValues(ns, name).Observe(42)

	assertSeriesNativeOnly(t, messageOverflowChars, ns, name)
}

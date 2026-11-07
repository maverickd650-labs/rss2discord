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
)

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

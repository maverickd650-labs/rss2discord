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

	"github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/rss"
	"k8s.io/client-go/tools/events"
)

// testRun is a feedRun, whose metric series are dropped when the test ends, for fg's feed with the given fetch and sender, at
// testReconcileTime, with no Event recorder and fg's own cadence. Tests that
// care about Events or the clock use withRecorder / withNow.
func testRun(t testing.TB, fg *v1alpha1.FeedGroup, feed v1alpha1.FeedSpec, result rss.FetchResult, fetchErr error, sender MessageSender) feedRun {
	t.Helper()
	forgetMetricsAfter(t, fg.Namespace, fg.Name)
	return feedRun{
		group:   fg,
		feed:    feed,
		fetched: fetchOutcome{result: result, err: fetchErr},
		sender:  sender,
		cadence: cadenceOf(&fg.Spec),
		now:     testReconcileTime,
	}
}

func (run feedRun) withRecorder(recorder events.EventRecorder) feedRun {
	run.recorder = recorder
	return run
}

func (run feedRun) withNow(now string) feedRun {
	run.now = now
	return run
}

// testAttempt is the feedAttempt for fg's feed at url, with no Event recorder.
func testAttempt(t testing.TB, fg *v1alpha1.FeedGroup, fs *v1alpha1.FeedStatus, url, now string) feedAttempt {
	return testRun(t, fg, v1alpha1.FeedSpec{RSSUrl: url}, rss.FetchResult{}, nil, nil).withNow(now).newAttempt(fs)
}

func (a feedAttempt) withRecorder(recorder events.EventRecorder) feedAttempt {
	a.recorder = recorder
	return a
}

// forgetMetricsAfter drops the metric series of the group (ns, name) when the
// test ends, so a test's series never leak into another's.
func forgetMetricsAfter(t testing.TB, ns, name string) {
	t.Helper()
	t.Cleanup(groupMetrics{namespace: ns, name: name}.forget)
}

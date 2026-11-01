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
	"strings"
	"testing"
	"time"

	"github.com/maverickd650-labs/rss2discord/api/v1alpha1"
)

// TestCadenceOf pins how a spec's Interval, RetryInterval and Retries resolve:
// blank takes the fallback, a valid value is floored, an unparsable one takes
// the fallback and is reported in err (interval before retryInterval), and
// retries have a minimum of one. The overflowing values pass the CRD's Pattern
// and MaxLength but are rejected by time.ParseDuration.
func TestCadenceOf(t *testing.T) {
	cases := []struct {
		name         string
		spec         v1alpha1.FeedGroupSpec
		wantPoll     time.Duration
		wantRetry    time.Duration
		wantMax      int32
		wantContains string // empty means no error expected
	}{
		{name: "empty uses fallbacks", wantPoll: fallbackInterval, wantRetry: fallbackRetryInterval, wantMax: 1},
		{name: "blank uses fallbacks", spec: v1alpha1.FeedGroupSpec{Interval: "  ", RetryInterval: " "}, wantPoll: fallbackInterval, wantRetry: fallbackRetryInterval, wantMax: 1},
		{name: "valid values parsed", spec: v1alpha1.FeedGroupSpec{Interval: "90s", RetryInterval: "1m", Retries: 3}, wantPoll: 90 * time.Second, wantRetry: time.Minute, wantMax: 3},
		// "1ns" (or "0s") passes the CRD's Pattern but would requeue in a tight
		// loop; the floor applies to both fields.
		{name: "tiny durations are floored", spec: v1alpha1.FeedGroupSpec{Interval: "1ns", RetryInterval: "0s"}, wantPoll: minParsedInterval, wantRetry: minParsedInterval, wantMax: 1},
		{name: "unparsable interval", spec: v1alpha1.FeedGroupSpec{Interval: notParsable, RetryInterval: "1m"}, wantPoll: fallbackInterval, wantRetry: time.Minute, wantMax: 1, wantContains: `spec.interval: invalid duration "` + notParsable + `"`},
		{name: "unparsable retryInterval", spec: v1alpha1.FeedGroupSpec{Interval: "30m", RetryInterval: notParsable}, wantPoll: 30 * time.Minute, wantRetry: fallbackRetryInterval, wantMax: 1, wantContains: `spec.retryInterval: invalid duration "` + notParsable + `"`},
		{name: "overflowing interval", spec: v1alpha1.FeedGroupSpec{Interval: overflowingInterval}, wantPoll: fallbackInterval, wantRetry: fallbackRetryInterval, wantMax: 1, wantContains: `spec.interval: invalid duration "` + overflowingInterval + `"`},
		{name: "overflowing retryInterval", spec: v1alpha1.FeedGroupSpec{Interval: "30m", RetryInterval: overflowingInterval}, wantPoll: 30 * time.Minute, wantRetry: fallbackRetryInterval, wantMax: 1, wantContains: `spec.retryInterval: invalid duration "` + overflowingInterval + `"`},
		{name: "both invalid reports interval first", spec: v1alpha1.FeedGroupSpec{Interval: notParsable, RetryInterval: notParsable}, wantPoll: fallbackInterval, wantRetry: fallbackRetryInterval, wantMax: 1, wantContains: "spec.interval"},
		{name: "negative retries minimum one", spec: v1alpha1.FeedGroupSpec{Retries: -1}, wantPoll: fallbackInterval, wantRetry: fallbackRetryInterval, wantMax: 1},
		{name: "zero retries minimum one", spec: v1alpha1.FeedGroupSpec{Retries: 0}, wantPoll: fallbackInterval, wantRetry: fallbackRetryInterval, wantMax: 1},
		{name: "one retry", spec: v1alpha1.FeedGroupSpec{Retries: 1}, wantPoll: fallbackInterval, wantRetry: fallbackRetryInterval, wantMax: 1},
		{name: "positive retries kept", spec: v1alpha1.FeedGroupSpec{Retries: 5}, wantPoll: fallbackInterval, wantRetry: fallbackRetryInterval, wantMax: 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cadenceOf(&tc.spec)
			if got.poll != tc.wantPoll || got.retry != tc.wantRetry || got.maxRetries != tc.wantMax {
				t.Fatalf("cadence = {poll %v, retry %v, maxRetries %d}, want {%v, %v, %d}",
					got.poll, got.retry, got.maxRetries, tc.wantPoll, tc.wantRetry, tc.wantMax)
			}
			if tc.wantContains == "" {
				if got.err != nil {
					t.Fatalf("unexpected error: %v", got.err)
				}
				return
			}
			if got.err == nil || !strings.Contains(got.err.Error(), tc.wantContains) {
				t.Fatalf("err = %v, want it to contain %q", got.err, tc.wantContains)
			}
			if !strings.HasSuffix(got.err.Error(), "; using the default instead") {
				t.Fatalf("err = %v, want the default-instead suffix", got.err)
			}
		})
	}
}

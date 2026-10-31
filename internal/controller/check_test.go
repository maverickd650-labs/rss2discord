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

	"github.com/maverickd650-labs/rss2discord/api/v1alpha1"
)

// The scheduler counts a retrying feed as failing; the Ready condition and the
// health gauge do not. The two questions must stay distinct.
func TestFeedCheck_FailedVersusFailedOrRetrying(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		fs                    v1alpha1.FeedStatus
		failed, failedOrRetry bool
	}{
		{name: "clean"},
		{name: "LastError set", fs: v1alpha1.FeedStatus{LastError: testBoom}, failed: true, failedOrRetry: true},
		{name: "retrying without error", fs: v1alpha1.FeedStatus{RetryCount: 2}, failedOrRetry: true},
		{name: "error and retrying", fs: v1alpha1.FeedStatus{LastError: testBoom, RetryCount: 1}, failed: true, failedOrRetry: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := checkOf(&tc.fs)
			if got := c.failed(); got != tc.failed {
				t.Errorf("failed() = %v, want %v", got, tc.failed)
			}
			if got := c.failedOrRetrying(); got != tc.failedOrRetry {
				t.Errorf("failedOrRetrying() = %v, want %v", got, tc.failedOrRetry)
			}
		})
	}
}

func TestFeedCheck_LastCheckedAndUntilDue(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	interval := 30 * time.Minute

	for _, lastChecked := range []string{"", "garbage"} {
		c := checkOf(&v1alpha1.FeedStatus{LastChecked: lastChecked})
		if _, ok := c.lastChecked(); ok {
			t.Errorf("lastChecked(%q) ok, want not ok", lastChecked)
		}
		if _, ok := c.untilDue(interval, now); ok {
			t.Errorf("untilDue(%q) ok, want not ok", lastChecked)
		}
	}

	for _, tc := range []struct {
		lastChecked string
		want        time.Duration
	}{
		{"2026-01-01T11:45:00Z", 15 * time.Minute},
		{"2026-01-01T11:30:00Z", 0},
		{"2026-01-01T11:00:00Z", -30 * time.Minute},
	} {
		c := checkOf(&v1alpha1.FeedStatus{LastChecked: tc.lastChecked})
		last, ok := c.lastChecked()
		if !ok || !last.Equal(mustParse(t, tc.lastChecked)) {
			t.Errorf("lastChecked(%q) = %v, %v", tc.lastChecked, last, ok)
		}
		if got, ok := c.untilDue(interval, now); !ok || got != tc.want {
			t.Errorf("untilDue(%q) = %v, %v; want %v", tc.lastChecked, got, ok, tc.want)
		}
	}
}

func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

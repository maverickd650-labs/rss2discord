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
	"time"

	"github.com/maverickd650/rss2discord-operator/api/v1alpha1"
)

// feedCheck is the last check of one feed, as FeedStatus records it: whether it
// left a failure and when it last succeeded. It is a view over the status, in
// the style of feedBackoff, and the one place that parses LastChecked and says
// what a failing feed is. Two questions are deliberately separate: the
// scheduler counts a feed that is still retrying as failing (failedOrRetrying),
// while the Ready condition and the feed_healthy gauge report only what status
// says now (failed).
type feedCheck struct {
	fs *v1alpha1.FeedStatus
}

// checkOf returns the last check of fs.
func checkOf(fs *v1alpha1.FeedStatus) feedCheck { return feedCheck{fs: fs} }

// failed reports whether the last check left an error in status. It is what
// Ready's FeedErrors count and the feed_healthy gauge (0) mean.
func (c feedCheck) failed() bool {
	return c.fs.LastError != ""
}

// failedOrRetrying reports whether the feed has an outstanding failure for
// scheduling: an error in status, or a retry run still counted. Such a feed is
// always due (see feedDue). It is wider than failed, which a feed mid-retry
// with a cleared LastError does not satisfy.
func (c feedCheck) failedOrRetrying() bool {
	return c.failed() || c.fs.RetryCount > 0
}

// lastChecked is the time of the last successful check, and false when the feed
// has never been checked or LastChecked does not parse.
func (c feedCheck) lastChecked() (time.Time, bool) {
	last, err := time.Parse(time.RFC3339, c.fs.LastChecked)
	return last, err == nil
}

// untilDue is how long until a full interval has elapsed since the last
// successful check, as of now; zero or negative when it already has. ok is false
// when there is no usable last check.
func (c feedCheck) untilDue(interval time.Duration, now time.Time) (until time.Duration, ok bool) {
	last, ok := c.lastChecked()
	if !ok {
		return 0, false
	}
	return last.Add(interval).Sub(now), true
}

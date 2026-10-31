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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
)

// maxPermanentBackoff is the ceiling for per-feed exponential backoff on
// permanent fetch failures (e.g. HTTP 404). Once the computed backoff would
// exceed this, the feed keeps being retried at this cadence indefinitely:
// "permanent" classifications (a 403 from a WAF blip, a 404 during a site
// migration) are often only long-lived, and a few requests a day is a cheap
// price for a feed that recovers on its own without a spec edit.
const maxPermanentBackoff = 6 * time.Hour

// feedBackoff is the permanent-failure backoff of one feed: the one place that
// reads, writes or clears FeedStatus.BackoffUntil and does the backoff math.
// It is a view over the status, not a copy, so changes land on the FeedStatus.
// The backoff is driven by fs.RetryCount, which the failure being recorded has
// already incremented. RetryCount counts one phase's retry run (see
// feedAttempt.record), so the backoff never inherits the other phase's count.
type feedBackoff struct {
	fs *v1alpha1.FeedStatus
}

// backoffOf returns the backoff of fs.
func backoffOf(fs *v1alpha1.FeedStatus) feedBackoff { return feedBackoff{fs: fs} }

// set reports whether a backoff is recorded at all, expired or not: an
// expired backoff still belongs to the run of failures that set it.
func (b feedBackoff) set() bool { return b.fs.BackoffUntil != "" }

// active reports whether the backoff is still in the future as of now. A
// malformed or empty BackoffUntil is treated as "not in backoff" so a bad value
// can never permanently wedge a feed without a spec change.
func (b feedBackoff) active(now time.Time) bool {
	if b.fs.BackoffUntil == "" {
		return false
	}
	until, err := time.Parse(time.RFC3339, b.fs.BackoffUntil)
	if err != nil {
		return false
	}
	return now.Before(until)
}

// extend sets BackoffUntil to now plus the exponential backoff for the current
// RetryCount (base * 2^RetryCount, capped at maxPermanentBackoff; see
// permanentBackoffDuration). It reports firstCapped: whether this is the first
// failure whose backoff reaches the cap (the previous failure's backoff, at
// RetryCount-1, was still below it). That is true once per run of failures, so
// the caller can fire its Event exactly once rather than on every capped
// retry. RetryCount returns to 0 when the backoff is cleared, which re-arms it.
func (b feedBackoff) extend(base time.Duration, now time.Time) (firstCapped bool) {
	count := int(b.fs.RetryCount)
	backoff := permanentBackoffDuration(count, base)
	if backoff >= maxPermanentBackoff {
		firstCapped = count <= 1 || permanentBackoffDuration(count-1, base) < maxPermanentBackoff
		backoff = maxPermanentBackoff
	}
	b.fs.BackoffUntil = nextBackoffUntil(now, backoff)
	return firstCapped
}

// clear ends the backoff and resets RetryCount with it: leaving RetryCount at
// its old value would make the next extend compute base*2^oldRetryCount, which
// jumps straight back to maxPermanentBackoff on the first retry. It ends a
// retry run (see feedAttempt.record). LastError is not touched (see
// clearFailure).
func (b feedBackoff) clear() {
	b.fs.BackoffUntil = ""
	b.fs.RetryCount = 0
}

// sendCaused reports whether the feed is backed off because delivery to its
// webhook failed, as opposed to its own URL failing to fetch. It is inferred
// from the Reachable condition: a fetch failure sets Reachable False, whereas a
// send or render failure leaves it True (the fetch succeeded), and a later
// successful fetch sets it True again (ending a fetch-phase run and its
// backoff, see feedAttempt.record). So a backoff with Reachable not False is a
// send backoff, which replacing the webhook may fix. This must stay in step
// with where extend is called (feedAttempt.failed): the fetch path sets
// Reachable, the send path sets Delivered.
func (b feedBackoff) sendCaused() bool {
	return b.fs.BackoffUntil != "" &&
		!apimeta.IsStatusConditionFalse(b.fs.Conditions, v1alpha1.FeedConditionTypeReachable)
}

// nextBackoffUntil formats the RFC3339 timestamp for a feed's next permanent
// backoff expiry, backoff duration past now.
func nextBackoffUntil(now time.Time, backoff time.Duration) string {
	return now.Add(backoff).Format(time.RFC3339)
}

// permanentBackoffDuration returns the exponential backoff for a permanent
// fetch failure at retryCount (1-based, already incremented before call).
// The formula is base * 2^retryCount, capped at maxPermanentBackoff, with
// retryCount floored at 1. A non-positive base returns maxPermanentBackoff.
func permanentBackoffDuration(retryCount int, base time.Duration) time.Duration {
	if retryCount < 1 {
		retryCount = 1
	}
	// Avoid overflow: time.Duration is int64, so 1<<63 wraps negative.
	// Guard the shift itself first, then check whether base*shift exceeds
	// the cap before multiplying -- otherwise large retryCount values
	// silently produce zero or negative durations.
	if retryCount >= 63 {
		return maxPermanentBackoff
	}
	shift := time.Duration(1 << uint(retryCount))
	if base <= 0 || shift > maxPermanentBackoff/base {
		return maxPermanentBackoff
	}
	return base * shift
}

// clearPermanentBackoffs clears the backoff (and RetryCount, see
// feedBackoff.clear) of every feed in the group. Called when the FeedGroup spec
// generation advances, so an edit gives feeds in backoff an immediate fresh
// attempt.
func clearPermanentBackoffs(feedGroup *v1alpha1.FeedGroup) {
	for i := range feedGroup.Status.Feeds {
		backoffOf(&feedGroup.Status.Feeds[i]).clear()
	}
}

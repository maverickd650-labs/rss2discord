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
	"strings"
	"time"

	"github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/util/wait"
)

// maxPermanentBackoff is the ceiling for per-feed exponential backoff on
// permanent fetch failures (e.g. HTTP 404). Once the computed backoff would
// exceed this, the feed keeps being retried at this cadence indefinitely:
// "permanent" classifications (a 403 from a WAF blip, a 404 during a site
// migration) are often only long-lived, and a few requests a day is a cheap
// price for a feed that recovers on its own without a spec edit.
const maxPermanentBackoff = 6 * time.Hour

// requeueJitterMaxFactor bounds the jitter added to a periodic (interval or
// retry-interval driven) requeue: up to 10% longer than the base duration,
// per wait.Jitter's contract. Without this, FeedGroups created together with
// the same interval requeue in lockstep forever -- every one of their
// fetches (and, for FeedGroups sharing a webhook, every Discord send) lands
// in the same instant, indefinitely.
const requeueJitterMaxFactor = 0.1

// jitterDuration adds up to requeueJitterMaxFactor extra to duration when
// enabled is true, and returns duration unchanged otherwise. Split out from
// requeueWithStatus so it can be unit-tested without a real client.Client
// (requeueWithStatus's applyStatus call needs one).
func jitterDuration(duration time.Duration, enabled bool) time.Duration {
	if !enabled {
		return duration
	}
	return wait.Jitter(duration, requeueJitterMaxFactor)
}

// minParsedInterval floors any interval/retryInterval this parses, whether
// from FeedGroup spec or already-persisted status. The CRD's Pattern rejects
// a negative literal, but a tiny positive one ("1ns") still validates and,
// fed straight into ctrl.Result{RequeueAfter: ...}, would requeue in a tight
// loop that hammers every feed host on each pass; this floor is the backstop
// for that, and also for objects admitted under an older, less strict schema.
const minParsedInterval = 10 * time.Second

func parseDurationWithDefault(value string, fallback time.Duration) (time.Duration, error) {
	if strings.TrimSpace(value) == "" {
		return fallback, nil
	}

	duration, err := time.ParseDuration(value)
	if err != nil {
		return fallback, fmt.Errorf("invalid duration %q: %w", value, err)
	}

	return max(duration, minParsedInterval), nil
}

func maxRetryCount(specRetries int32) int32 {
	if specRetries < 1 {
		return 1
	}
	return specRetries
}

// feedDue reports whether fs should be fetched this reconcile. A feed is due
// when its spec just changed (force), it has never been checked, it has an
// outstanding failure (those follow the RetryInterval cadence the group's
// requeue already encodes), or a full interval has elapsed since its last
// successful check. An unparsable LastChecked counts as due.
func feedDue(fs *v1alpha1.FeedStatus, interval time.Duration, now time.Time, force bool) bool {
	if force || fs.LastChecked == "" || fs.LastError != "" || fs.RetryCount > 0 {
		return true
	}
	last, err := time.Parse(time.RFC3339, fs.LastChecked)
	if err != nil {
		return true
	}
	return !now.Before(last.Add(interval))
}

// feedInBackoff reports whether backoffUntil (a FeedStatus.BackoffUntil
// RFC3339 timestamp) is still in the future relative to now. A malformed or
// empty backoffUntil is treated as "not in backoff" so a bad value can never
// permanently wedge a feed without a spec change.
func feedInBackoff(backoffUntil string, now time.Time) bool {
	if backoffUntil == "" {
		return false
	}
	until, err := time.Parse(time.RFC3339, backoffUntil)
	if err != nil {
		return false
	}
	return now.Before(until)
}

// nextBackoffUntil formats the RFC3339 timestamp for a feed's next permanent
// backoff expiry, backoff duration past now.
func nextBackoffUntil(now time.Time, backoff time.Duration) string {
	return now.Add(backoff).Format(time.RFC3339)
}

// permanentBackoffDuration returns the exponential backoff for a permanent
// fetch failure at retryCount (1-based, already incremented before call).
// The formula is base * 2^retryCount, capped at maxPermanentBackoff. Callers
// check whether the result >= maxPermanentBackoff to decide whether to set the
// sentinel vs. a concrete timestamp.
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

// clearPermanentBackoffs resets BackoffUntil and RetryCount on every feed in
// the group. Called when the FeedGroup spec generation advances, so an edit
// gives feeds in backoff an immediate fresh attempt. RetryCount is reset
// alongside BackoffUntil because leaving it at its old value would cause the
// next permanentBackoffDuration call to compute a backoff that immediately
// jumps to maxPermanentBackoff on the very first retry after the edit.
func clearPermanentBackoffs(feedGroup *v1alpha1.FeedGroup) {
	for i := range feedGroup.Status.Feeds {
		feedGroup.Status.Feeds[i].BackoffUntil = ""
		feedGroup.Status.Feeds[i].RetryCount = 0
	}
}

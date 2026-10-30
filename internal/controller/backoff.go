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

// parseDurationWithDefault parses value as a Go duration and floors parsed
// values at minParsedInterval. Whitespace-only input returns fallback without
// error; invalid input returns fallback and a wrapped parse error. The fallback
// is returned unchanged, without applying the floor.
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

// fallbackInterval and fallbackRetryInterval are the waits used when the spec's
// Interval/RetryInterval is empty or unparsable. They are the single source of
// these fallbacks for polling, requeueing and permanent-failure backoff.
const (
	fallbackInterval      = 30 * time.Minute
	fallbackRetryInterval = 5 * time.Minute
)

// feedPlan is the outcome of deciding which feeds a reconcile fetches.
type feedPlan struct {
	// active are the feeds to fetch, in spec order.
	active []v1alpha1.FeedSpec
	// nextDue is how long until the soonest feed skipped as not-yet-due becomes
	// due; zero when no feed was skipped for that reason.
	nextDue time.Duration
}

// planFeeds decides, as of now, which of the group's feeds to fetch. Paused
// feeds and feeds without a URL are skipped. A feed in permanent-failure
// backoff is skipped next, before the due check: backoff outranks the "failing
// feeds are always due" rule, and a backed-off feed contributes no wake time.
// A feed that is not yet due is skipped and bounds nextDue, so the requeue
// cannot sleep past the moment it becomes due (e.g. after a restart
// mid-interval). force (a spec change) makes every non-backed-off feed due. An
// unparsable Interval falls back to fallbackInterval here; requeueWithStatus
// reports it.
func planFeeds(feedGroup *v1alpha1.FeedGroup, now time.Time, force bool) feedPlan {
	pollInterval, _ := parseDurationWithDefault(feedGroup.Spec.Interval, fallbackInterval)
	plan := feedPlan{active: make([]v1alpha1.FeedSpec, 0, len(feedGroup.Spec.Feeds))}
	for _, feed := range feedGroup.Spec.Feeds {
		if feed.RSSUrl == "" || feed.Paused {
			continue
		}
		fs := feedStatusFor(feedGroup, feed.RSSUrl)
		if fs != nil && feedInBackoff(fs.BackoffUntil, now) {
			continue
		}
		if fs != nil && !feedDue(fs, pollInterval, now, force) {
			if last, err := time.Parse(time.RFC3339, fs.LastChecked); err == nil {
				if until := last.Add(pollInterval).Sub(now); plan.nextDue == 0 || until < plan.nextDue {
					plan.nextDue = until
				}
			}
			continue
		}
		plan.active = append(plan.active, feed)
	}
	return plan
}

// requeueDecision is how long to wait before the next reconcile, as the inputs
// wait turns into a duration: an interval (with the fallback for one that is
// empty or unparsable), whether to jitter it, and the bound a not-yet-due feed
// puts on it.
type requeueDecision struct {
	interval string
	fallback time.Duration
	// jitter is false only for an externally dictated wait (Discord's
	// Retry-After), which is already the exact minimum Discord demands.
	jitter  bool
	nextDue time.Duration
}

// retryRequeue is the decision after a reconcile that could not attempt any
// feed (e.g. the webhook secret is unusable): wait RetryInterval.
func retryRequeue(spec *v1alpha1.FeedGroupSpec) requeueDecision {
	return requeueDecision{interval: spec.RetryInterval, fallback: fallbackRetryInterval, jitter: true}
}

// requeue decides the wait after feeds were processed. A Discord rate limit
// overrides everything: the exact Retry-After, unjittered, regardless of
// sibling feeds. Otherwise the wait is RetryInterval if any feed wants a retry
// and Interval if not, jittered, and shortened to the plan's nextDue by bound.
func (p feedPlan) requeue(spec *v1alpha1.FeedGroupSpec, wantRetry bool, rateLimitRetryAfter time.Duration) requeueDecision {
	if rateLimitRetryAfter > 0 {
		return requeueDecision{fallback: rateLimitRetryAfter}
	}
	if wantRetry {
		d := retryRequeue(spec)
		d.nextDue = p.nextDue
		return d
	}
	return requeueDecision{interval: spec.Interval, fallback: fallbackInterval, jitter: true, nextDue: p.nextDue}
}

// wait is the final wait before the next reconcile: interval parsed (falling
// back to fallback when it is empty or unparsable; the parse error itself is
// reported on the Ready condition, see invalidIntervalError), jittered when
// d.jitter is set, then bounded by d.nextDue.
func (d requeueDecision) wait() time.Duration {
	delay, _ := parseDurationWithDefault(d.interval, d.fallback)
	return d.bound(jitterDuration(delay, d.jitter))
}

// bound shortens delay to the soonest not-yet-due feed, never below
// minParsedInterval. A delay already shorter than nextDue is left alone.
func (d requeueDecision) bound(delay time.Duration) time.Duration {
	if d.nextDue > 0 && d.nextDue < delay {
		return max(d.nextDue, minParsedInterval)
	}
	return delay
}

// maxRetryCount returns the configured retry limit, with a minimum of one.
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

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
	"k8s.io/apimachinery/pkg/util/wait"
)

// requeueJitterMaxFactor bounds the jitter added to a periodic (interval or
// retry-interval driven) requeue: up to 10% longer than the base duration,
// per wait.Jitter's contract. Without this, FeedGroups created together with
// the same interval requeue in lockstep forever -- every one of their
// fetches (and, for FeedGroups sharing a webhook, every Discord send) lands
// in the same instant, indefinitely.
const requeueJitterMaxFactor = 0.1

// jitterDuration adds up to requeueJitterMaxFactor extra to duration when
// enabled is true, and returns duration unchanged otherwise. Split out from
// settleGroup so it can be unit-tested without a real client.Client
// (settleGroup's applyStatus call needs one).
func jitterDuration(duration time.Duration, enabled bool) time.Duration {
	if !enabled {
		return duration
	}
	return wait.Jitter(duration, requeueJitterMaxFactor)
}

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
// mid-interval). force (a spec change) makes every non-backed-off feed due. The
// poll interval is cad.poll; an unparsable Interval is reported by
// settleGroup (cad.err).
func planFeeds(feedGroup *v1alpha1.FeedGroup, cad cadence, now time.Time, force bool) feedPlan {
	pollInterval := cad.poll
	plan := feedPlan{active: make([]v1alpha1.FeedSpec, 0, len(feedGroup.Spec.Feeds))}
	for _, feed := range feedGroup.Spec.Feeds {
		if feed.RSSUrl == "" || feed.Paused {
			continue
		}
		fs := feedStatusFor(feedGroup, feed.RSSUrl)
		if fs != nil && backoffOf(fs).active(now) {
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
// wait turns into a duration: the base delay (already resolved by the cadence,
// or an externally dictated Retry-After), whether to jitter it, and the bound a
// not-yet-due feed puts on it.
type requeueDecision struct {
	delay time.Duration
	// jitter is false only for an externally dictated wait (Discord's
	// Retry-After), which is already the exact minimum Discord demands.
	jitter  bool
	nextDue time.Duration
}

// retryRequeue is the decision after a reconcile that could not attempt any
// feed (e.g. the webhook secret is unusable): wait the cadence's retry
// interval.
func retryRequeue(cad cadence) requeueDecision {
	return requeueDecision{delay: cad.retry, jitter: true}
}

// requeue decides the wait after feeds were processed. A Discord rate limit
// overrides everything: the exact Retry-After, unjittered, regardless of
// sibling feeds. Otherwise the wait is the retry interval if any feed wants a
// retry and the poll interval if not, jittered, and shortened to the plan's
// nextDue by bound.
func (p feedPlan) requeue(cad cadence, wantRetry bool, rateLimitRetryAfter time.Duration) requeueDecision {
	if rateLimitRetryAfter > 0 {
		return requeueDecision{delay: rateLimitRetryAfter}
	}
	if wantRetry {
		d := retryRequeue(cad)
		d.nextDue = p.nextDue
		return d
	}
	return requeueDecision{delay: cad.poll, jitter: true, nextDue: p.nextDue}
}

// wait is the final wait before the next reconcile: delay jittered when
// d.jitter is set, then bounded by d.nextDue.
func (d requeueDecision) wait() time.Duration {
	return d.bound(jitterDuration(d.delay, d.jitter))
}

// bound shortens delay to the soonest not-yet-due feed, never below
// minParsedInterval. A delay already shorter than nextDue is left alone.
func (d requeueDecision) bound(delay time.Duration) time.Duration {
	if d.nextDue > 0 && d.nextDue < delay {
		return max(d.nextDue, minParsedInterval)
	}
	return delay
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

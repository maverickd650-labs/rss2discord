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
	"context"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/maverickd650/rss2discord-operator/internal/rss"
)

// entryDelivery is the per-entry send loop of one feed's sync: it scans the
// entries of syncer (see feedSync.entries), sending each not-yet-sent entry
// that passes the filter and recording every outcome with attempt. It owns the
// policy that ends or continues the scan -- stop on the first failure, skip an
// entry that keeps failing to render -- so that policy is written once, here,
// rather than split across the renderer, the sync and the attempt.
type entryDelivery struct {
	renderer *feedRenderer
	syncer   *feedSync
	attempt  feedAttempt
	sender   MessageSender
	metrics  groupMetrics
}

// run scans the entries. The first rendering or delivery failure ends the
// scan, unless it is a render failure that qualifies for skipping (see
// renderFailed), and its verdict is the result; a scan that gets through every
// entry has the zero verdict.
func (d entryDelivery) run(ctx context.Context) attemptVerdict {
	log := logf.FromContext(ctx)
	feed := d.renderer.feed

	for _, entry := range d.syncer.entries() {
		if d.syncer.sent(entry) {
			continue
		}

		if !d.renderer.matches(entry) {
			continue
		}

		discordMessage, overflow, err := d.renderer.build(entry)
		if err != nil {
			verdict, skipped := d.renderFailed(ctx, entry, err)
			if skipped {
				continue
			}

			// Stop processing this feed's remaining entries for this
			// reconcile rather than continuing to the next one: entries are
			// sent oldest-to-newest, and a later entry's success advances
			// LastSeenEntry past everything up to it. If we kept going past
			// this failed entry, a subsequent success would push the watermark
			// past it too, and the next reconcile's forward-scan would then
			// treat it as "already handled" and never attempt it again --
			// silently dropping it for good. Stopping here keeps the watermark
			// from ever skipping an unresolved entry; it just means later
			// entries in the same feed wait behind this one instead of jumping
			// ahead. The same goes for a failed send below, and a rate limit.
			return verdict
		}

		// Recorded per send attempt, before the outcome is known, so a retried
		// entry counts again; renders that are never sent (see
		// otherEntryRenders) are not observed.
		if overflow > 0 {
			d.metrics.overflow(overflow)
		}

		err = d.sender.SendMessage(ctx, discordMessage)
		if err != nil {
			log.Error(err, "failed to send Discord message", "url", feed.RSSUrl)
			// Discord rate limits are per-webhook, so once one entry is
			// limited every further POST this reconcile would 429 too: the
			// scan stops, the unsent entries stay pending (validators aren't
			// persisted) and are retried after the requeue backs off for the
			// verdict's retryAfter.
			outcome, class := sendFailureOutcome(err)
			return d.attempt.record(outcome, class, err)
		}

		d.syncer.markHandled(entry)
		d.attempt.record(attemptOutcome{kind: attemptSent}, noClass, nil)
	}

	return attemptVerdict{}
}

// renderFailed applies the render-failure policy to entry, which failed to
// render with err. It records the failure and, when the entry qualifies for
// skipping, marks it handled and records the skip, reporting skipped = true so
// the scan carries on. Otherwise the scan must stop with the returned verdict.
func (d entryDelivery) renderFailed(ctx context.Context, entry rss.Entry, err error) (verdict attemptVerdict, skipped bool) {
	log := logf.FromContext(ctx)
	feed := d.renderer.feed

	log.Error(err, "failed to render Discord message", "url", feed.RSSUrl)
	// A render error is deterministic for a given entry+template, so
	// once retries are exhausted, retrying forever would just spin
	// at RetryInterval without ever making progress. The attempt
	// surfaces an Event instead of looping indefinitely, the
	// reconcile RetryCount first reaches maxRetries.
	verdict = d.attempt.record(attemptOutcome{kind: attemptRenderFailed}, noClass, err)
	if d.attempt.pastRetryLimit() && d.renderer.otherEntryRenders(d.syncer.window(), entry) {
		// Retries were exhausted a full reconcile ago and the entry
		// still can't render, yet a sibling entry renders fine with
		// the same templates: the problem is this entry's content, it
		// never will render, and holding the watermark here would
		// block every newer entry behind it forever. Drop it and carry
		// on. If no entry renders, the templates themselves are broken
		// and skipping would just drain the feed, so keep holding.
		log.Error(err, "skipping entry after exhausting retries", "url", feed.RSSUrl, "entry", entryIdentity(entry))
		d.syncer.markHandled(entry)
		d.attempt.record(attemptOutcome{kind: attemptEntrySkipped, entry: entry.Title}, noClass, err)
		return attemptVerdict{}, true
	}
	return verdict, false
}

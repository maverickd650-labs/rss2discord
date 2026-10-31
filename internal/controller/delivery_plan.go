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
	"github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/rss"
)

// defaultCatchUpLimit is the cap used when a watermark has scrolled out of a
// feed's window, and by limitCatchUp for any non-positive limit. On a true
// first sync an explicit catchUpLimit of 0 means "skip the backlog" instead
// (see feedSync.skipBacklog); an omitted field is defaulted to 5 by the API
// server.
const defaultCatchUpLimit = 5

// catchUpLimitOf is the effective catch-up limit of spec: Spec.CatchUpLimit, or
// defaultCatchUpLimit when it is unset. It is the only code that reads
// Spec.CatchUpLimit; everything downstream takes the resolved int.
func catchUpLimitOf(spec v1alpha1.FeedGroupSpec) int {
	if spec.CatchUpLimit != nil {
		return int(*spec.CatchUpLimit)
	}
	return defaultCatchUpLimit
}

// limitCatchUp trims entries to at most limit, keeping the most recent ones
// (entries is assumed sorted ascending by Published). A non-positive limit
// falls back to defaultCatchUpLimit rather than disabling the cap, so a feed
// can never dump its entire backlog on first reconcile. (An explicit
// catchUpLimit of 0 is handled earlier, by skipBacklog, on a true first
// sync only.)
func limitCatchUp(entries []rss.Entry, limit int) []rss.Entry {
	if limit <= 0 {
		limit = defaultCatchUpLimit
	}
	if len(entries) <= limit {
		return entries
	}
	return entries[len(entries)-limit:]
}

// deliveryInputs is what planDelivery decides from: the outcome of the
// watermark lookup and the feed's catch-up configuration and history.
type deliveryInputs struct {
	// entries is the whole fetch, sorted oldest-to-newest.
	entries []rss.Entry
	// watermark is where the feed's watermark stands in entries (see
	// deliveryLedger.resume).
	watermark watermarkLookup
	// catchUpLimit is the effective spec limit (the default when unset).
	catchUpLimit int
	// legacy is the feed's legacy state; its catch-up cap applies to an
	// ambiguous status (see legacyState.capCatchUp).
	legacy legacyState
	// wasInitialized is fs.Initialized before this reconcile.
	wasInitialized bool
}

// deliveryPlan says which entries entryDelivery should scan and how.
type deliveryPlan struct {
	// scan is the entries to scan, in order, starting at resumeAt.
	scan     []rss.Entry
	resumeAt int
	// skipBacklog says to record the newest of the whole fetch as the
	// watermark instead of sending anything (scan is then nil).
	skipBacklog bool
	// cappedAmbiguousLegacy says the catch-up was capped at
	// defaultCatchUpLimit because the status is an ambiguous legacy one.
	cappedAmbiguousLegacy bool
}

// entries returns the entries the send loop should consider, oldest-to-newest:
// the scan from just past the watermark.
func (p deliveryPlan) entries() []rss.Entry {
	return p.scan[p.resumeAt:]
}

// renderEvidence is the entries whose rendering counts as evidence that the
// feed's templates work, for entryDelivery.otherEntryRenders: the whole scan,
// untrimmed past-watermark entries included (before the watermark and already
// sent ones passed the filter when they were delivered), but not the ones the
// catch-up trim left out.
func (p deliveryPlan) renderEvidence() []rss.Entry {
	return p.scan
}

// planDelivery decides, without side effects, what to send after the
// watermark lookup.
//
// A stored watermark that was not found (scrolled out of the feed's window, or
// the feed changed how it identifies the entry) is not a first sync: it
// falls through to the catch-up cap rather than silently dropping new entries.
// With no watermark at all and catchUpLimit 0, a true first sync (not yet
// initialized) skips the backlog; a feed already fetched while empty delivers
// everything, since all of it was published after that first sync; an ambiguous
// legacy status is capped at defaultCatchUpLimit (see legacyState).
func planDelivery(in deliveryInputs) deliveryPlan {
	if in.watermark.state == watermarkFound {
		return deliveryPlan{scan: in.entries, resumeAt: in.watermark.index + 1}
	}
	if in.catchUpLimit == 0 && in.watermark.state == watermarkNone {
		if scan, capped := in.legacy.capCatchUp(in.entries); capped {
			return deliveryPlan{scan: scan, cappedAmbiguousLegacy: true}
		}
		if !in.wasInitialized {
			return deliveryPlan{skipBacklog: true}
		}
		return deliveryPlan{scan: in.entries}
	}
	return deliveryPlan{scan: limitCatchUp(in.entries, in.catchUpLimit)}
}

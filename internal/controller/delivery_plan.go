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

import "github.com/maverickd650/rss2discord-operator/internal/rss"

// deliveryInputs is what planDelivery decides from: the outcome of the
// watermark lookup and the feed's catch-up configuration and history.
type deliveryInputs struct {
	// entries is the whole fetch, sorted oldest-to-newest.
	entries []rss.Entry
	// hadWatermark is whether fs.LastSeenEntry was non-empty.
	hadWatermark bool
	// watermarkIndex is the index in entries of the watermark's entry, and
	// found whether there is one (see deliveryLedger.resume).
	watermarkIndex int
	found          bool
	// catchUpLimit is the effective spec limit (the default when unset).
	catchUpLimit int
	// legacyAmbiguous is legacyState.ambiguous.
	legacyAmbiguous bool
	// wasInitialized is fs.Initialized before this reconcile.
	wasInitialized bool
}

// deliveryPlan says which entries sendNewEntries should scan and how.
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
	if in.hadWatermark && in.found {
		return deliveryPlan{scan: in.entries, resumeAt: in.watermarkIndex + 1}
	}
	if in.catchUpLimit == 0 && !in.hadWatermark {
		switch {
		case in.legacyAmbiguous:
			return deliveryPlan{
				scan:                  limitCatchUp(in.entries, defaultCatchUpLimit),
				cappedAmbiguousLegacy: true,
			}
		case !in.wasInitialized:
			return deliveryPlan{skipBacklog: true}
		}
		return deliveryPlan{scan: in.entries}
	}
	return deliveryPlan{scan: limitCatchUp(in.entries, in.catchUpLimit)}
}

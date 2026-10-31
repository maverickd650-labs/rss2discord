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

// previouslyIdentified returns entry carrying the ID earlier releases picked for
// it, and whether that is a different one (see rss.Entry.PreviousID).
func previouslyIdentified(entry rss.Entry) (rss.Entry, bool) {
	if entry.PreviousID == "" {
		return entry, false
	}
	entry.ID, entry.IDSource = entry.PreviousID, entry.PreviousIDSource
	return entry, true
}

// migrateIdentities is the one entry point that brings a feed's stored
// identities (the LastSeenEntry watermark and the LastSent keys) in line with
// how entries, sorted oldest-to-newest, are identified now. It runs the two
// migrations in order -- first legacy's stateVersion 0 translation (a no-op for
// a current status; see migrateLegacyIdentities for replayLimit), then the move
// off PreviousID identities (migratePreviousIdentities) -- through the one
// deliveryLedger of the fetch, so neither writes a stripped LastSent key a
// sibling entry shares. The legacy pass has to go first: it reads the previous
// ID as the legacy identity and writes under the current one, which is where
// the second pass then finds it. Both are idempotent and run on every fetch.
func migrateIdentities(ledger *deliveryLedger, legacy *legacyState, replayLimit int) {
	legacy.migrate(ledger, replayLimit)
	migratePreviousIdentities(ledger)
}

// migratePreviousIdentities carries a feed's stored identities over for the
// Atom entries whose ID the parser now picks differently from earlier releases
// (rss.Entry.PreviousID): the watermark and the dedup keys, which are keyed by
// the old identity, move to the new one, keeping their send times. Without it
// such an entry would look new and be sent again, and a watermark naming the
// old identity would match nothing and fall back to the capped catch-up.
//
// It is idempotent and does nothing for an entry whose identity did not change,
// so it runs on every fetch. An old identity that more than one entry shares, or
// that is another entry's current identity, is left alone, as is a move onto an
// identity more than one entry now has: it can't be told which entry the stored
// value stands for.
func migratePreviousIdentities(ledger *deliveryLedger) {
	for i := range ledger.entries {
		if ledger.carriesOver(i) {
			ledger.moveWatermark(ledger.record(i).previous, i)
			ledger.rekeySent(i)
		}
	}
}

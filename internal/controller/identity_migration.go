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
	apimeta "k8s.io/apimachinery/pkg/api/meta"

	"github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/rss"
)

// currentStateVersion is the FeedStatus.StateVersion this controller writes.
// Version 0 (absent) identities dropped a link-derived ID's fragment and ref
// parameter and were empty for an entry with no GUID, link or title; see
// migrateLegacyIdentities.
const currentStateVersion = 1

// legacyEntryIdentity is entryIdentity as controllers before stateVersion 1
// computed it: every URL normalized with normalizeIdentity, and an empty
// identity for an entry with no GUID, link or title.
func legacyEntryIdentity(entry rss.Entry) string {
	entry, _ = previouslyIdentified(entry)
	if entry.ID == "" || entry.IDSource == rss.IDFromContent {
		return ""
	}
	return normalizeIdentity(entry.ID)
}

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
// off PreviousID identities (migratePreviousIdentities) -- over one
// strippedOwnership of the fetch, so neither writes a stripped LastSent key a
// sibling entry shares. The legacy pass has to go first: it reads the previous
// ID as the legacy identity and writes under the current one, which is where
// the second pass then finds it. Both are idempotent and run on every fetch.
func migrateIdentities(fs *v1alpha1.FeedStatus, entries []rss.Entry, legacy *legacyState, replayLimit int) {
	owners := newStrippedOwnership(entries)
	legacy.migrate(fs, entries, replayLimit, owners)
	migratePreviousIdentities(fs, entries, owners)
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
func migratePreviousIdentities(fs *v1alpha1.FeedStatus, entries []rss.Entry, owners strippedOwnership) {
	type move struct{ previous, current rss.Entry }
	var moves []move
	shared := make(map[string]int)
	currentCounts := make(map[string]int, len(entries))
	for _, entry := range entries {
		currentCounts[entryIdentity(entry)]++
		if previous, changed := previouslyIdentified(entry); changed {
			moves = append(moves, move{previous: previous, current: entry})
			shared[entryIdentity(previous)]++
		}
	}

	for _, m := range moves {
		old, now := entryIdentity(m.previous), entryIdentity(m.current)
		if old == now || shared[old] > 1 || currentCounts[old] > 0 || currentCounts[now] > 1 {
			continue
		}
		if fs.LastSeenEntry == clampWatermark(old) {
			fs.LastSeenEntry = clampWatermark(now)
		}
		rekeySent(fs.LastSent, m.previous, m.current, owners)
	}
}

// legacyState is what processFeed tracks for a FeedStatus written before
// stateVersion 1. The status is marked current once nothing in it still
// needs the legacy reading: its identities were translated (which needs a
// fetch with entries) and an ambiguous missing watermark was settled.
type legacyState struct {
	// legacy is set for a status written before stateVersion 1.
	legacy bool
	// identities is set when LastSeenEntry or LastSent hold legacy
	// identities for migrateLegacyIdentities to translate.
	identities bool
	// emptyWatermark is set when an absent LastSeenEntry stands for an entry
	// whose legacy identity was empty: the last one sent, or the newest one
	// a backlog skip recorded.
	emptyWatermark bool
	// ambiguous is set for a checked status with no watermark that can't
	// say whether the feed was empty when last checked (controllers before
	// initialized existed set LastChecked on every successful fetch), had a
	// backlog a config error kept from being skipped, or had a legacy empty
	// watermark. Under catchUpLimit 0 the first would deliver everything and
	// the second nothing, so processFeed caps the catch-up at
	// defaultCatchUpLimit instead: what those controllers delivered.
	ambiguous bool
	// migrated and settled record that identities and ambiguous were
	// resolved by this processFeed call.
	migrated, settled bool
}

// readLegacyState reports what fs, before this reconcile changes it, still
// needs from the legacy reading.
func readLegacyState(fs *v1alpha1.FeedStatus) legacyState {
	if fs.StateVersion >= currentStateVersion {
		return legacyState{}
	}
	sentEmptyIdentity := identitySent(fs.LastSent, "")
	delivered := apimeta.FindStatusCondition(fs.Conditions, v1alpha1.FeedConditionTypeDelivered)
	noWatermark := fs.LastSeenEntry == ""
	emptyWatermark := noWatermark &&
		(sentEmptyIdentity || (delivered != nil && delivered.Reason == reasonBacklogSkipped))
	return legacyState{
		legacy:         true,
		identities:     !noWatermark || len(fs.LastSent) > 0,
		emptyWatermark: emptyWatermark,
		ambiguous:      noWatermark && fs.LastChecked != "" && (!fs.Initialized || emptyWatermark),
	}
}

// migrate translates fs's legacy identities using entries, sorted
// oldest-to-newest; see migrateLegacyIdentities for replayLimit. It is a
// no-op for a current status.
func (s *legacyState) migrate(fs *v1alpha1.FeedStatus, entries []rss.Entry, replayLimit int, owners strippedOwnership) {
	if !s.legacy {
		return
	}
	migrateLegacyIdentities(fs, entries, s.emptyWatermark, replayLimit, owners)
	s.migrated = true
}

// finish marks fs current once nothing in it needs the legacy reading.
func (s *legacyState) finish(fs *v1alpha1.FeedStatus) {
	if s.legacy && (!s.identities || s.migrated) && (!s.ambiguous || s.settled) {
		fs.StateVersion = currentStateVersion
	}
}

// migrateLegacyIdentities rewrites a stateVersion 0 watermark and dedup keys
// into the current identity scheme, using entries (sorted oldest-to-newest)
// to recover which entry each legacy value stood for. For an unshared legacy
// identity, changed dedup keys retain their recorded send time.
//
// A legacy identity can stand for several entries in entries (links that
// differed only by fragment or ref, or several entries with no GUID, link or
// title). Rather than guess which one was meant, the watermark resolves to the
// oldest of them and their shared dedup key is dropped, so the newer ones are
// replayed instead of skipped -- under the legacy scheme at most one of them
// was ever delivered. That replay is a backlog, so only the newest
// replayLimit of them after the watermark are left to send: the watermark
// moves past the older ones, so a feed whose entries all collided can't post
// its whole history on upgrade. A replayLimit of 0 skips that replay. It never
// moves past an unsent entry outside the group, so colliding entries dated after
// such a (new) entry are sent even beyond the cap.
//
// emptyWatermark says an absent LastSeenEntry stands for a
// legacy empty identity rather than no watermark at all. Legacy values that
// match no entry are left as they are: a watermark that matches nothing
// falls back to the catch-up cap either way.
func migrateLegacyIdentities(fs *v1alpha1.FeedStatus, entries []rss.Entry, emptyWatermark bool, replayLimit int, owners strippedOwnership) {
	current := make([]string, len(entries))
	legacy := make([]string, len(entries))
	sharing := make(map[string]int, len(entries))
	for i, entry := range entries {
		current[i], legacy[i] = entryIdentity(entry), legacyEntryIdentity(entry)
		sharing[legacy[i]]++
	}

	watermark := -1
	if fs.LastSeenEntry != "" || emptyWatermark {
		for i := range entries {
			if clampWatermark(legacy[i]) == fs.LastSeenEntry {
				fs.LastSeenEntry = clampWatermark(current[i])
				watermark = i
				break
			}
		}
	}

	// A legacy identity shared by entries whose identities now differ is one
	// whose dedup key is dropped below, so its entries replay.
	colliding := make(map[string]bool)
	for i := range entries {
		if sharing[legacy[i]] > 1 && current[i] != legacy[i] {
			colliding[legacy[i]] = true
		}
	}

	for i := range entries {
		if current[i] == legacy[i] {
			continue
		}
		sentAt, ok := sentTime(fs.LastSent, legacy[i])
		if !ok {
			continue
		}
		if sharing[legacy[i]] == 1 {
			// A link's legacy identity is its stripped form; like
			// deliveryLedger.markHandled, record it only when it is this
			// entry's own and unique in the fetch, or a sibling sharing it
			// would later be taken as sent.
			setSent(fs.LastSent, current[i], owners.ownStripped(entries[i], legacy[i]), sentAt)
		}
		forgetSent(fs.LastSent, legacy[i])
	}

	// With no watermark found, limitCatchUp already caps the whole fetch.
	if watermark < 0 {
		return
	}
	sent := func(i int) bool {
		return identitySent(fs.LastSent, current[i])
	}
	replay := 0
	for i := watermark + 1; i < len(entries); i++ {
		if colliding[legacy[i]] && !sent(i) {
			replay++
		}
	}
	// Skip the oldest of them by moving the watermark past them, which
	// LastSent's pruning can't undo. It also moves past entries already
	// sent, but stops at the first other unsent entry: that one is new, and
	// so are any colliding entries dated after it.
	skip := replay - replayLimit
	moved := watermark
	for i := watermark + 1; i < len(entries) && skip > 0; i++ {
		switch {
		case sent(i):
		case colliding[legacy[i]]:
			skip--
		default:
			skip = 0
			continue
		}
		moved = i
	}
	if moved > watermark {
		fs.LastSeenEntry = clampWatermark(current[moved])
	}
}

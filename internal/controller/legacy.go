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

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

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

// migrate translates the legacy identities in the ledger's status using its
// entries, sorted oldest-to-newest; see migrateLegacyIdentities for replayLimit. It is a
// no-op for a current status.
func (s *legacyState) migrate(ledger *deliveryLedger, replayLimit int) {
	if !s.legacy {
		return
	}
	migrateLegacyIdentities(ledger, s.emptyWatermark, replayLimit)
	s.migrated = true
}

// blocksInitialize says a sync that has not accounted for the fetch must not
// mark fs initialized: an ambiguous legacy status stays uninitialized (and
// capped, see capCatchUp) until a sync settles it.
func (s *legacyState) blocksInitialize() bool {
	return s.ambiguous
}

// capCatchUp is the legacy cap on the catch-up window: for an ambiguous status
// under catchUpLimit 0 (the caller's condition) it keeps the newest
// defaultCatchUpLimit of entries, in memory only. capped is false, and entries
// is returned as is, for any other status.
func (s *legacyState) capCatchUp(entries []rss.Entry) (scan []rss.Entry, capped bool) {
	if !s.ambiguous {
		return entries, false
	}
	return limitCatchUp(entries, defaultCatchUpLimit), true
}

// logLegacyCatchUpCap is feedSync.apply's legacy step after begin: it logs,
// for the feed at url, that this sync's catch-up was capped at
// defaultCatchUpLimit because fs is an ambiguous legacy status (see
// capCatchUp). It is a no-op for any other sync.
func (s *feedSync) logLegacyCatchUpCap(ctx context.Context, url string) {
	if s.cappedAmbiguousLegacy {
		logf.FromContext(ctx).Info("Capped catch-up for a FeedStatus written before stateVersion 1",
			"url", url, "limit", defaultCatchUpLimit)
	}
}

// settleOnClose is feedSync.close's legacy step. A sync that ends empty or
// accounted for settles an ambiguous status (a 304 repeats a fetch it can't
// account for, and a pending or failed sync has not resolved it), then fs is
// marked current once nothing in it still needs the legacy reading.
func (s *legacyState) settleOnClose(fs *v1alpha1.FeedStatus, verdict syncVerdict) {
	if verdict == syncEmpty || verdict == syncAccountedFor {
		s.settled = true
	}
	if s.legacy && (!s.identities || s.migrated) && (!s.ambiguous || s.settled) {
		fs.StateVersion = currentStateVersion
	}
}

// migrateLegacyIdentities rewrites a stateVersion 0 watermark and dedup keys
// into the current identity scheme, using the ledger's entries (sorted
// oldest-to-newest) to recover which entry each legacy value stood for. For an unshared legacy
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
func migrateLegacyIdentities(ledger *deliveryLedger, emptyWatermark bool, replayLimit int) {
	entries := ledger.entries
	current := make([]string, len(entries))
	legacy := make([]string, len(entries))
	sharing := make(map[string]int, len(entries))
	for i, entry := range entries {
		current[i], legacy[i] = entryIdentity(entry), legacyEntryIdentity(entry)
		sharing[legacy[i]]++
	}

	watermark := -1
	if ledger.fs.LastSeenEntry != "" || emptyWatermark {
		for i := range entries {
			if ledger.moveWatermark(legacy[i], current[i]) {
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
		sentAt, ok := ledger.sentAt(legacy[i])
		if !ok {
			continue
		}
		if sharing[legacy[i]] == 1 {
			// A link's legacy identity is its stripped form; like
			// deliveryLedger.markHandled, record it only when it is this
			// entry's own and unique in the fetch, or a sibling sharing it
			// would later be taken as sent.
			ledger.setSent(current[i], ledger.ownStripped(entries[i], legacy[i]), sentAt)
		}
		ledger.forgetSent(legacy[i])
	}

	// With no watermark found, limitCatchUp already caps the whole fetch.
	if watermark < 0 {
		return
	}
	sent := func(i int) bool {
		return ledger.sentIdentity(current[i])
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
		ledger.setWatermark(current[moved])
	}
}

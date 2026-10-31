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
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"slices"
	"strings"

	"github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/rss"
)

// maxLastSentPerFeed bounds how many sent-entry hashes are retained per feed
// in Status.LastSent. Without a cap this map grows by one key per sent
// message forever, bloating the FeedGroup status subresource indefinitely.
//
// The cap is kept deliberately modest because the whole status object lives in
// etcd, which has a ~1MB object soft limit: at Feeds.MaxItems (50) this cap
// bounds LastSent at 50*maxLastSentPerFeed {64-hex-hash: RFC3339} pairs, so the
// product is the real budget to watch, not this number alone. LastSent is only
// a dedup backstop -- LastSeenEntry is the primary watermark -- so it just needs
// to cover the entries currently in a feed's window (typically 10-50), not its
// whole history.
const maxLastSentPerFeed = 50

// clampWatermark returns id unchanged when it fits in FeedStatus.LastSeenEntry,
// otherwise a stable digest of it. Applied on both the store and compare
// sides, so a long identity still matches itself across reconciles.
func clampWatermark(id string) string {
	if len(id) <= maxStatusTextBytes {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// The LastSent key scheme lives in this file alone: an entry is recorded under
// computeEntryKey(entry) and, when its stripped link identity is unique in the
// fetch, under strippedKey of that too. The deliveryLedger's identity-level
// methods (sentAt, sentIdentity, setSent, forgetSent, rekeySent, moveWatermark,
// setWatermark) are how the identity migrations re-key LastSent and move the
// watermark without building keys themselves.

// computeEntryKey hashes an entry's identity into a fixed-length key for
// Status.LastSent. It intentionally does not also fold in Link or Title the
// way an earlier version did: those commonly churn (a rotated tracking
// parameter, an edited headline) on an otherwise-unchanged article, and
// folding them into the key made every such edit look like a brand new
// entry and get re-sent as a duplicate -- defeating the point of having a
// stable GUID-based identity in the first place.
func computeEntryKey(entry rss.Entry) string {
	return hashIdentity(entryIdentity(entry))
}

// hashIdentity is computeEntryKey for an identity the caller already
// resolved, so a loop over entries doesn't normalize each URL twice.
func hashIdentity(identity string) string {
	hash := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(hash[:])
}

// pruneLastSent caps a feed's sent-entry dedup map at max entries, dropping
// the oldest (by recorded RFC3339 send timestamp, which sorts lexically;
// ties broken by key) first. Without this, LastSent grows by one key per sent message forever.
func pruneLastSent(sent map[string]string, max int) {
	if len(sent) <= max {
		return
	}

	keys := slices.SortedFunc(maps.Keys(sent), func(a, b string) int {
		// Timestamps have one-second resolution, so ties are common; break
		// them on the key so the dropped subset is deterministic.
		return cmp.Or(strings.Compare(sent[a], sent[b]), strings.Compare(a, b))
	})

	for _, key := range keys[:len(keys)-max] {
		delete(sent, key)
	}
}

// strippedKeyPrefix keeps the LastSent keys deliveryLedger.markHandled writes for stripped
// link identities apart from entry keys: a GUID-identified entry whose ID is
// exactly a link's stripped form must not be mistaken for that link.
const strippedKeyPrefix = "stripped:"

// strippedKey is the LastSent key for a stripped link identity.
func strippedKey(stripped string) string {
	return hashIdentity(strippedKeyPrefix + stripped)
}

// strippedCounts counts how many of one fetch's entries each identity could
// stand for: an entry's strippedLinkIdentity, and its own identity when that
// differs. The stripped form only stands in for an entry when no other entry
// in the fetch shares it: entries that differ only by fragment or ref (a
// changelog's #v1.2 and #v1.3), or a link whose stripped form is another
// entry's GUID, are told apart by their full identities alone, while an entry
// whose ref merely rotated still matches what was recorded for it.
type strippedCounts map[string]int

// countStripped indexes entries for the deliveryLedger.
func countStripped(entries []rss.Entry) strippedCounts {
	counts := make(strippedCounts)
	for _, entry := range entries {
		stripped := strippedLinkIdentity(entry)
		if stripped != "" {
			counts[stripped]++
		}
		if identity := entryIdentity(entry); identity != stripped {
			counts[identity]++
		}
	}
	return counts
}

// unique returns entry's stripped identity when no other entry in the fetch
// shares it, else "".
func (c strippedCounts) unique(entry rss.Entry) string {
	if stripped := strippedLinkIdentity(entry); stripped != "" && c[stripped] == 1 {
		return stripped
	}
	return ""
}

// entryKeys returns the LastSent keys that record entry: its own, and, when
// its stripped link identity is unique in the fetch, that identity's too.
func (c strippedCounts) entryKeys(entry rss.Entry) []string {
	keys := []string{computeEntryKey(entry)}
	if unique := c.unique(entry); unique != "" {
		keys = append(keys, strippedKey(unique))
	}
	return keys
}

// The identity migrations (identity_migration.go) rewrite stored keys for
// entries that did not exist under their current identity when the key was
// written, so each asks the ledger (ownStripped, ownsPrevious, rekeySent) before
// writing a stripped key: a stripped key stands in for any entry whose stripped
// link identity matches it, so one recorded for an identity another entry also
// has would later mark that sibling, alone in the feed, as sent.

// countPrevious counts, per stripped identity, the entries of one fetch whose
// previous ID (rss.Entry.PreviousID) was a link with that stripped identity.
func countPrevious(entries []rss.Entry) map[string]int {
	previous := make(map[string]int)
	for _, entry := range entries {
		if old, changed := previouslyIdentified(entry); changed {
			if stripped := strippedLinkIdentity(old); stripped != "" {
				previous[stripped]++
			}
		}
	}
	return previous
}

// identitySent reports whether lastSent holds the entry key of identity.
func identitySent(lastSent map[string]string, identity string) bool {
	_, ok := lastSent[hashIdentity(identity)]
	return ok
}

// moveSentKey renames a LastSent key, keeping its send time, or just drops it
// when to is empty. A key already recorded under to is not overwritten.
func moveSentKey(lastSent map[string]string, from, to string) {
	sentAt, ok := lastSent[from]
	if !ok {
		return
	}
	delete(lastSent, from)
	if to == "" {
		return
	}
	if _, exists := lastSent[to]; !exists {
		lastSent[to] = sentAt
	}
}

// watermarkState is the three ways a feed's watermark can stand against a fetch.
type watermarkState int

const (
	// watermarkNone: the feed has no watermark (fs.LastSeenEntry is empty).
	watermarkNone watermarkState = iota
	// watermarkFound: the watermark stands for an entry of the fetch.
	watermarkFound
	// watermarkLost: there is a watermark but no entry of the fetch it stands
	// for -- it scrolled out of the feed's window, or the feed changed how it
	// identifies the entry.
	watermarkLost
)

// watermarkLookup is the outcome of looking a feed's watermark up in a fetch.
type watermarkLookup struct {
	state watermarkState
	// index is the position in the fetch of the watermark's entry when state
	// is watermarkFound, else -1.
	index int
}

// deliveryLedger is a feed's delivery bookkeeping over one fetch, and the single
// identity index of it: the FeedStatus it reads and writes (the LastSeenEntry
// watermark and the LastSent dedup keys) together with the whole fetch's
// entries and the stripped-identity counts they depend on. It is built once per
// fetch, over the sorted, untrimmed entries and before the identity migrations,
// which only change fs, so the counts are the same before and after them. An
// entry's stripped identity is only unique if it is across the feed, however the
// entries are later trimmed for catch-up. Everything that reads or writes the
// watermark and LastSent -- delivery, the migrations, the watermark lookup --
// goes through it.
type deliveryLedger struct {
	fs *v1alpha1.FeedStatus
	// entries is the whole fetch, sorted oldest-to-newest. The ledger never
	// changes it.
	entries []rss.Entry
	counts  strippedCounts
	// previous counts, per stripped identity, the entries whose previous ID was
	// a link with that stripped identity (countPrevious).
	previous map[string]int
}

// newDeliveryLedger builds the ledger for fs over entries, the whole fetch
// sorted oldest-to-newest and before any catch-up trim.
func newDeliveryLedger(fs *v1alpha1.FeedStatus, entries []rss.Entry) *deliveryLedger {
	return &deliveryLedger{fs: fs, entries: entries, counts: countStripped(entries), previous: countPrevious(entries)}
}

// resume looks fs.LastSeenEntry up in the fetch: watermarkNone for an empty
// watermark, else watermarkFound at the first entry whose identity matches it
// exactly or, for a watermark that kept a fragment or ref and so came from a
// link, the entry whose unique stripped identity matches the watermark's, else
// watermarkLost. A GUID's identity never keeps either, so a link can't take over
// the watermark of a GUID that has left the feed.
func (l *deliveryLedger) resume() watermarkLookup {
	watermark := l.fs.LastSeenEntry
	if watermark == "" {
		return watermarkLookup{state: watermarkNone, index: -1}
	}
	lost := watermarkLookup{state: watermarkLost, index: -1}
	for i, entry := range l.entries {
		if clampWatermark(entryIdentity(entry)) == watermark {
			return watermarkLookup{state: watermarkFound, index: i}
		}
	}
	stripped := normalizeIdentity(watermark)
	if stripped == watermark {
		return lost
	}
	stripped = clampWatermark(stripped)
	for i, entry := range l.entries {
		if unique := l.counts.unique(entry); unique != "" && clampWatermark(unique) == stripped {
			return watermarkLookup{state: watermarkFound, index: i}
		}
	}
	return lost
}

// ownStripped returns stripped, a link identity a migration recovered for
// entry, when it is entry's own stripped identity and unique in the fetch (the
// only stripped key markHandled would record for it), else "". A stripped
// identity recovered from the entry's previous ID is not its own.
func (l *deliveryLedger) ownStripped(entry rss.Entry, stripped string) string {
	if stripped != "" && l.counts.unique(entry) == stripped {
		return stripped
	}
	return ""
}

// ownsPrevious reports whether the stripped key of previous, a moved entry as
// earlier releases identified it, can only have been recorded for that entry:
// no other moved entry had the same stripped identity as its previous one, and
// no current entry has it as its identity or stripped identity.
func (l *deliveryLedger) ownsPrevious(previous rss.Entry) bool {
	stripped := strippedLinkIdentity(previous)
	return stripped != "" && l.previous[stripped] == 1 && l.counts[stripped] == 0
}

// moveWatermark moves the watermark from the entry identified by from to the one
// identified by to, when it stands for from, and reports whether it did. The
// comparison and the stored value are both clamped like every other watermark.
func (l *deliveryLedger) moveWatermark(from, to string) bool {
	if l.fs.LastSeenEntry != clampWatermark(from) {
		return false
	}
	l.fs.LastSeenEntry = clampWatermark(to)
	return true
}

// setWatermark moves the watermark to the entry identified by identity.
func (l *deliveryLedger) setWatermark(identity string) {
	l.fs.LastSeenEntry = clampWatermark(identity)
}

// sentAt returns when LastSent recorded identity as sent, under its entry key.
func (l *deliveryLedger) sentAt(identity string) (string, bool) {
	sentAt, ok := l.fs.LastSent[hashIdentity(identity)]
	return sentAt, ok
}

// sentIdentity reports whether LastSent holds the entry key of identity.
func (l *deliveryLedger) sentIdentity(identity string) bool {
	return identitySent(l.fs.LastSent, identity)
}

// setSent records identity in LastSent at sentAt under its entry key and, when
// stripped is not empty, under that stripped link identity's key too,
// overwriting what was there.
func (l *deliveryLedger) setSent(identity, stripped, sentAt string) {
	l.fs.LastSent[hashIdentity(identity)] = sentAt
	if stripped != "" {
		l.fs.LastSent[strippedKey(stripped)] = sentAt
	}
}

// forgetSent drops identity's entry key from LastSent. Its stripped key, if
// any, is left alone.
func (l *deliveryLedger) forgetSent(identity string) {
	delete(l.fs.LastSent, hashIdentity(identity))
}

// rekeySent moves the LastSent keys recorded for previous, an entry as earlier
// releases identified it, onto current, the same entry as now identified,
// keeping their send times; see moveSentKey. The entry key moves to the new
// identity's. The stripped key, which only a link-derived previous identity
// has, moves to the new stripped identity's when current is link-derived too
// and that stripped identity is unique in the fetch: a key under a shared
// stripped identity would later mark a sibling that is alone in the feed as
// already sent, the same reason markHandled only writes it for a unique entry.
// Otherwise, when ownsPrevious says no other entry can claim the old stripped
// identity, it falls back to the new entry key, so a record that pruning left
// only under the stripped key still dedups the entry; and failing that it is
// dropped.
func (l *deliveryLedger) rekeySent(previous, current rss.Entry) {
	moveSentKey(l.fs.LastSent, computeEntryKey(previous), computeEntryKey(current))
	if oldStripped := strippedLinkIdentity(previous); oldStripped != "" {
		target := ""
		if newStripped := l.counts.unique(current); newStripped != "" {
			target = strippedKey(newStripped)
		} else if l.ownsPrevious(previous) {
			target = computeEntryKey(current)
		}
		moveSentKey(l.fs.LastSent, strippedKey(oldStripped), target)
	}
}

// sent reports whether entry was already delivered: recorded in LastSent under
// its own key, or under its unique stripped identity's.
func (l *deliveryLedger) sent(entry rss.Entry) bool {
	for _, key := range l.counts.entryKeys(entry) {
		if _, ok := l.fs.LastSent[key]; ok {
			return true
		}
	}
	return false
}

// markHandled records entry as delivered (or given up on) at now and moves the
// watermark to it. It is recorded under its own key and, when it is unique in
// the fetch, its stripped identity's too, so a later rotation of its fragment or
// ref still finds it.
func (l *deliveryLedger) markHandled(entry rss.Entry, now string) {
	for _, key := range l.counts.entryKeys(entry) {
		l.fs.LastSent[key] = now
	}
	l.setWatermark(entryIdentity(entry))
}

// markNewest moves the watermark to the newest entry of the fetch without
// recording anything as sent. The fetch must be non-empty.
func (l *deliveryLedger) markNewest() {
	l.setWatermark(entryIdentity(l.entries[len(l.entries)-1]))
}

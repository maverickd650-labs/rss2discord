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
// fetch, under strippedKey of that too. The deliveryLedger computes each
// entry's keys once, in its identityRecord, and its entry-level methods (sent,
// markHandled, rekeySent, moveWatermark, setWatermark) are how delivery and the
// identity migrations record, re-key and move the watermark without building
// keys themselves.

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

// identityRecord is everything the ledger derives from one entry, computed once
// (at construction, for the entries of the fetch) so that sent, markHandled,
// resume and the migrations read it instead of re-parsing the entry's URLs.
type identityRecord struct {
	// identity is entryIdentity(entry), mark its watermark form (clampWatermark)
	// and key its LastSent entry key (computeEntryKey).
	identity, mark, key string
	// stripped is strippedLinkIdentity(entry): "" unless the ID came from the
	// entry's link.
	stripped string
	// unique is stripped when no other entry in the fetch can stand for it,
	// else "". The stripped form only stands in for an entry when it is
	// unique: entries that differ only by fragment or ref (a changelog's #v1.2
	// and #v1.3), or a link whose stripped form is another entry's GUID, are
	// told apart by their full identities alone, while an entry whose ref
	// merely rotated still matches what was recorded for it.
	unique string
	// keys are the LastSent keys that record the entry: key, and, when unique
	// is not empty, strippedKey(unique).
	keys []string
	// moved is set when the parser's PreviousID gives the entry a different ID
	// than earlier releases picked (previouslyIdentified). The fields below
	// describe that earlier identification and are empty otherwise.
	moved bool
	// previous is the entry's identity as earlier releases computed it,
	// previousKey its entry key, previousStripped its stripped link identity
	// ("" for a previous ID that was not a link) and previousStrippedKey the
	// stripped key of that.
	previous, previousKey, previousStripped, previousStrippedKey string
}

// describeEntry derives the part of entry's identityRecord that does not
// depend on the rest of the fetch; settle completes it.
func describeEntry(entry rss.Entry) identityRecord {
	rec := identityRecord{identity: entryIdentity(entry), stripped: strippedLinkIdentity(entry)}
	rec.mark = clampWatermark(rec.identity)
	rec.key = hashIdentity(rec.identity)
	if previous, moved := previouslyIdentified(entry); moved {
		rec.moved = true
		rec.previous = entryIdentity(previous)
		rec.previousKey = hashIdentity(rec.previous)
		if rec.previousStripped = strippedLinkIdentity(previous); rec.previousStripped != "" {
			rec.previousStrippedKey = strippedKey(rec.previousStripped)
		}
	}
	return rec
}

// entryRef is what determines an entry's identityRecord, so entries sharing one
// share a record: the ID and previous ID with their sources, and for an entry
// with no ID the content hash its identity is derived from. It is how sent and
// markHandled, which are handed an entry, find its record without parsing its
// URLs again.
type entryRef struct {
	id, previousID, content  string
	idSource, previousSource rss.IDSource
}

func refOf(entry rss.Entry) entryRef {
	ref := entryRef{id: entry.ID, previousID: entry.PreviousID, idSource: entry.IDSource, previousSource: entry.PreviousIDSource}
	if entry.ID == "" {
		ref.content = rss.ContentID(entry)
	}
	return ref
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
// entries, one identityRecord per entry and every uniqueness count the
// identity rules depend on. It is built once per fetch, over the sorted,
// untrimmed entries and before the identity migrations, which only change fs,
// so the records and counts are the same before and after them. An entry's
// stripped identity is only unique if it is across the feed, however the
// entries are later trimmed for catch-up. Everything that reads or writes the
// watermark and LastSent -- delivery, the migrations, the watermark lookup --
// goes through it, and every "is this identity unique" rule is decided here.
type deliveryLedger struct {
	fs *v1alpha1.FeedStatus
	// entries is the whole fetch, sorted oldest-to-newest. The ledger never
	// changes it.
	entries []rss.Entry
	// ids[i] is the identityRecord of entries[i]; byRef finds a record from
	// an entry.
	ids   []identityRecord
	byRef map[entryRef]int
	// claims counts how many of the fetch's entries each identity could stand
	// for: an entry's stripped identity, and its own identity when that differs.
	claims map[string]int
	// identities counts the entries per full identity (identityRecord.identity).
	identities map[string]int
	// previousIdentities counts the moved entries per previous full identity,
	// previousStripped per previous stripped identity.
	previousIdentities, previousStripped map[string]int
}

// newDeliveryLedger builds the ledger for fs over entries, the whole fetch
// sorted oldest-to-newest and before any catch-up trim.
func newDeliveryLedger(fs *v1alpha1.FeedStatus, entries []rss.Entry) *deliveryLedger {
	l := &deliveryLedger{
		fs:                 fs,
		entries:            entries,
		ids:                make([]identityRecord, len(entries)),
		byRef:              make(map[entryRef]int, len(entries)),
		claims:             make(map[string]int),
		identities:         make(map[string]int, len(entries)),
		previousIdentities: make(map[string]int),
		previousStripped:   make(map[string]int),
	}
	for i, entry := range entries {
		rec := describeEntry(entry)
		l.ids[i] = rec
		l.byRef[refOf(entry)] = i // entries sharing a ref share a record
		l.identities[rec.identity]++
		if rec.stripped != "" {
			l.claims[rec.stripped]++
		}
		if rec.identity != rec.stripped {
			l.claims[rec.identity]++
		}
		if rec.moved {
			l.previousIdentities[rec.previous]++
			if rec.previousStripped != "" {
				l.previousStripped[rec.previousStripped]++
			}
		}
	}
	// Uniqueness needs every entry counted, so the records are completed after.
	for i := range l.ids {
		l.settle(&l.ids[i])
	}
	return l
}

// settle completes rec once the fetch is counted: its unique stripped identity
// and the LastSent keys that record it.
func (l *deliveryLedger) settle(rec *identityRecord) {
	if rec.stripped != "" && l.claims[rec.stripped] == 1 {
		rec.unique = rec.stripped
	}
	rec.keys = []string{rec.key}
	if rec.unique != "" {
		rec.keys = append(rec.keys, strippedKey(rec.unique))
	}
}

// record returns entries[i]'s identityRecord.
func (l *deliveryLedger) record(i int) identityRecord { return l.ids[i] }

// recordOf returns entry's identityRecord: the one built for it when it is an
// entry of the fetch, else one derived now against the fetch's counts (an entry
// from outside the fetch is unique when exactly one entry of the fetch could
// stand for its stripped identity).
func (l *deliveryLedger) recordOf(entry rss.Entry) identityRecord {
	if i, ok := l.byRef[refOf(entry)]; ok {
		return l.ids[i]
	}
	rec := describeEntry(entry)
	l.settle(&rec)
	return rec
}

// resume looks fs.LastSeenEntry up in the fetch: watermarkNone for an empty
// watermark, else watermarkFound at the first entry whose identity matches it
// exactly or, for a watermark that kept a fragment or ref and so came from a
// link, the entry whose unique stripped identity matches the watermark's, else
// watermarkLost. A GUID's identity never keeps either, so a link can't take over
// the watermark of a GUID that has left the feed.
func (l *deliveryLedger) resume() watermarkLookup {
	if !l.hasWatermark() {
		return watermarkLookup{state: watermarkNone, index: -1}
	}
	watermark := l.fs.LastSeenEntry
	lost := watermarkLookup{state: watermarkLost, index: -1}
	for i := range l.ids {
		if l.ids[i].mark == watermark {
			return watermarkLookup{state: watermarkFound, index: i}
		}
	}
	stripped := normalizeIdentity(watermark)
	if stripped == watermark {
		return lost
	}
	stripped = clampWatermark(stripped)
	for i := range l.ids {
		if unique := l.ids[i].unique; unique != "" && clampWatermark(unique) == stripped {
			return watermarkLookup{state: watermarkFound, index: i}
		}
	}
	return lost
}

// The identity migrations (identity_migration.go, legacy.go) rewrite stored
// keys for entries that did not exist under their current identity when the key
// was written, so each asks the ledger before writing a stripped key (the
// record's unique form, ownsPrevious, rekeySent): a stripped key stands in for
// any entry whose stripped link identity matches it, so one recorded for an
// identity another entry also has would later mark that sibling, alone in the
// feed, as sent. The two ownership rules differ on purpose and both live here:
// carriesOver decides on full identities (may a stored entry key move?),
// ownsPrevious on stripped ones (may a stored stripped key stand for one entry?).

// ownsPrevious reports whether the stripped key of entries[i]'s previous
// identity (entry i is a moved entry: earlier releases identified it
// differently) can only have been recorded for that entry: no other moved entry
// had the same stripped identity as its previous one, and no current entry has
// it as its identity or stripped identity.
func (l *deliveryLedger) ownsPrevious(i int) bool {
	stripped := l.ids[i].previousStripped
	return stripped != "" && l.previousStripped[stripped] == 1 && l.claims[stripped] == 0
}

// carriesOver reports whether what is stored under entries[i]'s previous
// identity can be moved onto its current one: the identity did change, and the
// move is unambiguous -- no other moved entry had the same previous identity,
// no current entry has it as its identity, and no other entry has the new
// identity. Otherwise it can't be told which entry the stored value stands for.
func (l *deliveryLedger) carriesOver(i int) bool {
	rec := l.ids[i]
	return rec.moved && rec.previous != rec.identity &&
		l.previousIdentities[rec.previous] <= 1 &&
		l.identities[rec.previous] == 0 &&
		l.identities[rec.identity] <= 1
}

// moveWatermark moves the watermark from the identity from, as a migration
// recovered it, to the one of entries[to], when it stands for from, and reports
// whether it did. The comparison and the stored value are both clamped like
// every other watermark.
func (l *deliveryLedger) moveWatermark(from string, to int) bool {
	if l.fs.LastSeenEntry != clampWatermark(from) {
		return false
	}
	l.setWatermark(to)
	return true
}

// hasWatermark reports whether the feed has a watermark stored.
func (l *deliveryLedger) hasWatermark() bool {
	return l.fs.LastSeenEntry != ""
}

// setWatermark moves the watermark to entries[i].
func (l *deliveryLedger) setWatermark(i int) {
	l.fs.LastSeenEntry = l.ids[i].mark
}

// identitySent reports whether lastSent holds the entry key of identity. It is
// the key scheme's query for a status read before any fetch, when there is no
// ledger yet (readLegacyState).
func identitySent(lastSent map[string]string, identity string) bool {
	_, ok := lastSent[hashIdentity(identity)]
	return ok
}

// forgetIdentity drops the entry key of identity, a stored identity a
// migration recovered, from LastSent and returns when it was recorded. Any
// stripped key is left alone.
func (l *deliveryLedger) forgetIdentity(identity string) (sentAt string, ok bool) {
	key := hashIdentity(identity)
	if sentAt, ok = l.sentAtKey(key); ok {
		delete(l.fs.LastSent, key)
	}
	return sentAt, ok
}

// recordSentAt records entries[i] in LastSent at sentAt under its entry key,
// overwriting what was there, and under the stripped key of stripped, a link
// identity a migration recovered for it, only when that is the entry's own
// stripped identity and unique in the fetch -- the only stripped key
// markHandled would record for it. A stripped identity recovered from the
// entry's previous ID is not its own, and one a sibling shares would later
// mark that sibling as sent.
func (l *deliveryLedger) recordSentAt(i int, sentAt, stripped string) {
	rec := l.ids[i]
	l.recordKey(rec.key, sentAt)
	if stripped != "" && rec.unique == stripped {
		l.recordKey(strippedKey(stripped), sentAt)
	}
}

// entryKeySent reports whether LastSent holds entries[i]'s entry key (not its
// stripped key).
func (l *deliveryLedger) entryKeySent(i int) bool {
	_, ok := l.sentAtKey(l.ids[i].key)
	return ok
}

// sentAtKey returns when LastSent recorded key as sent.
func (l *deliveryLedger) sentAtKey(key string) (string, bool) {
	sentAt, ok := l.fs.LastSent[key]
	return sentAt, ok
}

// recordKey writes key to LastSent at sentAt, overwriting what was there.
func (l *deliveryLedger) recordKey(key, sentAt string) {
	l.fs.LastSent[key] = sentAt
}

// moveSent renames a LastSent key; see moveSentKey.
func (l *deliveryLedger) moveSent(from, to string) {
	moveSentKey(l.fs.LastSent, from, to)
}

// rekeySent moves the LastSent keys recorded for entries[i]'s previous
// identity, as earlier releases identified it, onto its current one, keeping
// their send times; see moveSentKey. The entry key moves to the new
// identity's. The stripped key, which only a link-derived previous identity
// has, moves to the new stripped identity's when the entry is link-derived too
// and that stripped identity is unique in the fetch: a key under a shared
// stripped identity would later mark a sibling that is alone in the feed as
// already sent, the same reason markHandled only writes it for a unique entry.
// Otherwise, when ownsPrevious says no other entry can claim the old stripped
// identity, it falls back to the new entry key, so a record that pruning left
// only under the stripped key still dedups the entry; and failing that it is
// dropped.
func (l *deliveryLedger) rekeySent(i int) {
	rec := l.ids[i]
	l.moveSent(rec.previousKey, rec.key)
	if rec.previousStrippedKey != "" {
		target := ""
		if rec.unique != "" {
			target = strippedKey(rec.unique)
		} else if l.ownsPrevious(i) {
			target = rec.key
		}
		l.moveSent(rec.previousStrippedKey, target)
	}
}

// sent reports whether entry was already delivered: recorded in LastSent under
// its own key, or under its unique stripped identity's.
func (l *deliveryLedger) sent(entry rss.Entry) bool {
	for _, key := range l.recordOf(entry).keys {
		if _, ok := l.sentAtKey(key); ok {
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
	rec := l.recordOf(entry)
	for _, key := range rec.keys {
		l.recordKey(key, now)
	}
	l.fs.LastSeenEntry = rec.mark
}

// markNewest moves the watermark to the newest entry of the fetch without
// recording anything as sent. The fetch must be non-empty.
func (l *deliveryLedger) markNewest() {
	l.setWatermark(len(l.entries) - 1)
}

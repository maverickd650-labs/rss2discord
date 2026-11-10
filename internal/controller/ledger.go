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

	"github.com/maverickd650-labs/rss2discord/api/v1beta1"
	"github.com/maverickd650-labs/rss2discord/internal/rss"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// maxLastSentPerFeed bounds how many sent-entry hashes are retained per feed
// in Status.LastSent. Without a cap this map grows by one key per sent
// message forever, bloating the FeedGroup status subresource indefinitely.
//
// The cap is kept deliberately modest because the whole status object lives in
// etcd, which has a ~1MB object soft limit: at Feeds.MaxItems (50) this cap
// bounds LastSent at 50*maxLastSentPerFeed {64-hex-hash: timestamp} pairs, so the
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
// hashIdentity of its resolved identity and, when its stripped link identity
// is unique in the fetch, under strippedKey of that too. The deliveryLedger
// computes each entry's keys once, in its identityRecord, and its entry-level
// methods (sent, markHandled, markNewest) are how delivery records entries and
// moves the watermark without building keys themselves.

// hashIdentity hashes an identity the caller already
// resolved, so a loop over entries doesn't normalize each URL twice.
func hashIdentity(identity string) string {
	hash := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(hash[:])
}

// pruneLastSent caps a feed's sent-entry dedup map at max entries, dropping
// the oldest (by recorded send time; ties broken by key) first. Without this,
// LastSent grows by one key per sent message forever.
func pruneLastSent(sent map[string]metav1.Time, max int) {
	if len(sent) <= max {
		return
	}

	keys := slices.SortedFunc(maps.Keys(sent), func(a, b string) int {
		// Timestamps have one-second resolution, so ties are common; break
		// them on the key so the dropped subset is deterministic.
		return cmp.Or(sent[a].Compare(sent[b].Time), strings.Compare(a, b))
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

// identityRecord is everything the ledger derives from one entry, computed once
// (at construction, for the entries of the fetch) so that sent, markHandled,
// and resume read it instead of re-parsing URLs.
type identityRecord struct {
	// identity is entryIdentity(entry), mark its watermark form (clampWatermark)
	// and key its LastSent entry key (hashIdentity).
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
}

// describeEntry derives the part of entry's identityRecord that does not
// depend on the rest of the fetch; settle completes it.
func describeEntry(entry rss.Entry) identityRecord {
	rec := identityRecord{identity: entryIdentity(entry), stripped: strippedLinkIdentity(entry)}
	rec.mark = clampWatermark(rec.identity)
	rec.key = hashIdentity(rec.identity)
	return rec
}

// entryRef is what determines an entry's identityRecord, so entries sharing one
// share a record: the ID with its source, and for an entry with no ID the
// content hash its identity is derived from. It is how sent and
// markHandled, which are handed an entry, find its record without parsing its
// URLs again.
type entryRef struct {
	id, content string
	idSource    rss.IDSource
}

func refOf(entry rss.Entry) entryRef {
	ref := entryRef{id: entry.ID, idSource: entry.IDSource}
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
// untrimmed entries and before catch-up trimming. An entry's stripped identity
// is only unique if it is across the feed, however the entries are later
// trimmed for catch-up. Everything that reads or writes the watermark and
// LastSent -- delivery, the watermark lookup -- goes through it, and every "is
// this identity unique" rule is decided here.
type deliveryLedger struct {
	fs *v1beta1.FeedStatus
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
}

// newDeliveryLedger builds the ledger for fs over entries, the whole fetch
// sorted oldest-to-newest and before any catch-up trim.
func newDeliveryLedger(fs *v1beta1.FeedStatus, entries []rss.Entry) *deliveryLedger {
	l := &deliveryLedger{
		fs:      fs,
		entries: entries,
		ids:     make([]identityRecord, len(entries)),
		byRef:   make(map[entryRef]int, len(entries)),
		claims:  make(map[string]int),
	}
	for i, entry := range entries {
		rec := describeEntry(entry)
		l.ids[i] = rec
		l.byRef[refOf(entry)] = i // entries sharing a ref share a record
		if rec.stripped != "" {
			l.claims[rec.stripped]++
		}
		if rec.identity != rec.stripped {
			l.claims[rec.identity]++
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

// identity returns entry's resolved identity from the ledger's cached record.
func (l *deliveryLedger) identity(entry rss.Entry) string {
	return l.recordOf(entry).identity
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

// hasWatermark reports whether the feed has a watermark stored.
func (l *deliveryLedger) hasWatermark() bool {
	return l.fs.LastSeenEntry != ""
}

// setWatermark moves the watermark to entries[i].
func (l *deliveryLedger) setWatermark(i int) {
	l.fs.LastSeenEntry = l.ids[i].mark
}

// sentAtKey returns when LastSent recorded key as sent.
func (l *deliveryLedger) sentAtKey(key string) (metav1.Time, bool) {
	sentAt, ok := l.fs.LastSent[key]
	return sentAt, ok
}

// recordKey writes key to LastSent at sentAt, overwriting what was there.
func (l *deliveryLedger) recordKey(key string, sentAt metav1.Time) {
	l.fs.LastSent[key] = sentAt
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
func (l *deliveryLedger) markHandled(entry rss.Entry, now metav1.Time) {
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

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

	neturl "net/url"

	apimeta "k8s.io/apimachinery/pkg/api/meta"

	"github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/rss"
)

// currentStateVersion is the FeedStatus.StateVersion this controller writes.
// Version 0 (absent) identities dropped a link-derived ID's fragment and ref
// parameter and were empty for an entry with no GUID, link or title; see
// migrateLegacyIdentities.
const currentStateVersion = 1

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

// trackingQueryParams lists query-string parameters added by analytics or
// sharing tools that commonly get appended, rotated, or dropped between
// fetches of an otherwise-unchanged article (FeedBurner/WordPress are
// common sources). They're stripped in normalizeIdentity so churn limited
// to these parameters doesn't make an unchanged article look new.
var trackingQueryParams = map[string]bool{
	"fbclid":  true,
	"gclid":   true,
	"mc_eid":  true,
	"mc_cid":  true,
	"igshid":  true,
	"ref_src": true,
	"ref":     true,
	"mkt_tok": true,
	"_hsenc":  true,
	"_hsmi":   true,
	"spm":     true,
}

// schemeHTTP/schemeHTTPS are the only URL schemes normalizeIdentity and
// httpURLOrEmpty treat as a URL rather than opaque text.
const (
	schemeHTTP  = "http"
	schemeHTTPS = "https"
)

// normalizeIdentity normalizes id when it's an http(s) URL (a GUID is
// frequently just the entry's permalink): lowercases the scheme/host, drops
// the fragment, and strips known tracking query parameters, so that churn
// limited to those doesn't change the result. Anything that isn't a URL (an
// opaque GUID, or a Title used as a last-resort identity) is returned
// unchanged, since there's no generically safe way to normalize arbitrary
// text.
func normalizeIdentity(id string) string {
	return normalizeURLIdentity(id, false)
}

// normalizeLinkIdentity is normalizeIdentity for an ID the parser took from
// the entry's link because it had no GUID. It keeps the fragment and the ref
// parameter: with no GUID they are often all that tells entries apart (a
// changelog's #v1.2 and #v1.3 anchors, or ?ref=v1.0 and ?ref=v1.1).
func normalizeLinkIdentity(id string) string {
	return normalizeURLIdentity(id, true)
}

// normalizeURLIdentity implements normalizeIdentity and, with keepTarget,
// normalizeLinkIdentity. When keepTarget is true, the fragment and ref query
// parameter are preserved while other known tracking parameters are removed.
// Invalid URLs and identities without an http(s) scheme are returned unchanged.
func normalizeURLIdentity(id string, keepTarget bool) string {
	// Opaque GUIDs and title fallbacks are common and can't be URLs we
	// normalize, so skip the (allocating) parse for anything that doesn't
	// start with an http(s) scheme.
	if len(id) < 7 || !strings.EqualFold(id[:4], schemeHTTP) {
		return id
	}
	parsed, err := neturl.Parse(id)
	if err != nil || (parsed.Scheme != schemeHTTP && parsed.Scheme != schemeHTTPS) {
		return id
	}

	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	if !keepTarget {
		parsed.Fragment = ""
	}

	if query := parsed.Query(); len(query) > 0 {
		for param := range query {
			lower := strings.ToLower(param)
			if keepTarget && lower == "ref" {
				continue
			}
			if trackingQueryParams[lower] || strings.HasPrefix(lower, "utm_") {
				query.Del(param)
			}
		}
		parsed.RawQuery = query.Encode()
	}

	return parsed.String()
}

// entryIdentity resolves entry's stable identity, used for both the
// catch-up watermark (LastSeenEntry) and per-entry dedup (LastSent):
// entry.ID itself (already resolved via GUID -> Link -> Title -> ContentID
// precedence by the RSS/Atom parser), normalized when it's a URL. Link-derived
// URL IDs retain their fragment and ref parameter. An entry with no ID gets the
// parser's ContentID, so the identity is never empty.
func entryIdentity(entry rss.Entry) string {
	switch {
	case entry.ID == "":
		return rss.ContentID(entry)
	case entry.IDSource == rss.IDFromLink:
		return normalizeLinkIdentity(entry.ID)
	default:
		return normalizeIdentity(entry.ID)
	}
}

// legacyEntryIdentity is entryIdentity as controllers before stateVersion 1
// computed it: every URL normalized with normalizeIdentity, and an empty
// identity for an entry with no GUID, link or title.
func legacyEntryIdentity(entry rss.Entry) string {
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
	_, sentEmptyIdentity := fs.LastSent[hashIdentity("")]
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
func (s *legacyState) migrate(fs *v1alpha1.FeedStatus, entries []rss.Entry, replayLimit int) {
	if !s.legacy {
		return
	}
	migrateLegacyIdentities(fs, entries, s.emptyWatermark, replayLimit)
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
// to recover which entry each legacy value stood for. Only entries whose
// identity changed need translating; a shared legacy key is dropped even if
// another entry, such as one identified by a GUID, still uses that identity.
//
// A legacy identity can stand for several entries in entries (links that
// differed only by fragment or ref, or several entries with no GUID, link or
// title). Rather than guess which one was meant, the watermark resolves to the
// oldest of them and their shared dedup key is dropped, so the newer ones are
// replayed instead of skipped -- under the legacy scheme at most one of them
// was ever delivered. That replay is a backlog, so only the newest
// replayLimit of them after the watermark are left to send: the watermark
// moves past the older ones, so a feed whose entries all collided can't post
// its whole history on upgrade. It never moves past an unsent entry outside
// the group, so colliding entries dated after such a (new) entry are sent. emptyWatermark says an absent LastSeenEntry stands for a
// legacy empty identity rather than no watermark at all. Legacy values that
// match no entry are left as they are: a watermark that matches nothing
// falls back to the catch-up cap either way.
func migrateLegacyIdentities(fs *v1alpha1.FeedStatus, entries []rss.Entry, emptyWatermark bool, replayLimit int) {
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
		legacyKey := hashIdentity(legacy[i])
		sentAt, ok := fs.LastSent[legacyKey]
		if !ok {
			continue
		}
		if sharing[legacy[i]] == 1 {
			fs.LastSent[hashIdentity(current[i])] = sentAt
			if entries[i].IDSource == rss.IDFromLink {
				// A link's legacy identity is its stripped form; see recordSent.
				fs.LastSent[strippedKey(legacy[i])] = sentAt
			}
		}
		delete(fs.LastSent, legacyKey)
	}

	// With no watermark found, limitCatchUp already caps the whole fetch.
	if watermark < 0 {
		return
	}
	sent := func(i int) bool {
		_, ok := fs.LastSent[hashIdentity(current[i])]
		return ok
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

// strippedLinkIdentity is a link-derived entry's identity without the
// fragment and ref parameter that entryIdentity keeps, or "" for an entry
// whose ID didn't come from its link. Some feeds rotate those between
// fetches (share-tracking plugins, CDN rewrites) on an otherwise unchanged
// article, so dedup and the watermark fall back to this form; see
// strippedCounts.
func strippedLinkIdentity(entry rss.Entry) string {
	if entry.IDSource != rss.IDFromLink || entry.ID == "" {
		return ""
	}
	return normalizeIdentity(entry.ID)
}

// strippedKeyPrefix keeps the LastSent keys recordSent writes for stripped
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

// countStripped indexes entries for watermarkIndex, entrySent and recordSent.
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

// watermarkIndex returns the index in entries of the entry watermark (a
// LastSeenEntry value) stands for, or -1: the first entry whose identity
// matches it exactly, else, for a watermark that kept a fragment or ref and
// so came from a link, the entry whose unique stripped identity matches the
// watermark's. A GUID's identity never keeps either, so a link can't take
// over the watermark of a GUID that has left the feed.
func watermarkIndex(entries []rss.Entry, watermark string, counts strippedCounts) int {
	for i, entry := range entries {
		if clampWatermark(entryIdentity(entry)) == watermark {
			return i
		}
	}
	stripped := normalizeIdentity(watermark)
	if stripped == watermark {
		return -1
	}
	stripped = clampWatermark(stripped)
	for i, entry := range entries {
		if unique := counts.unique(entry); unique != "" && clampWatermark(unique) == stripped {
			return i
		}
	}
	return -1
}

// entrySent reports whether lastSent records entry: under its own key, or
// under its unique stripped identity's.
func entrySent(lastSent map[string]string, entry rss.Entry, counts strippedCounts) bool {
	if _, ok := lastSent[computeEntryKey(entry)]; ok {
		return true
	}
	if unique := counts.unique(entry); unique != "" {
		_, ok := lastSent[strippedKey(unique)]
		return ok
	}
	return false
}

// recordSent records entry in lastSent at now under its own key and, when it
// is unique in the fetch, its stripped identity's too, so a later rotation of
// its fragment or ref still finds it.
func recordSent(lastSent map[string]string, entry rss.Entry, counts strippedCounts, now string) {
	lastSent[computeEntryKey(entry)] = now
	if unique := counts.unique(entry); unique != "" {
		lastSent[strippedKey(unique)] = now
	}
}

// entriesContainID reports whether id, as stored in LastSeenEntry, stands for
// any of entries; see watermarkIndex.
func entriesContainID(entries []rss.Entry, id string) bool {
	return watermarkIndex(entries, id, countStripped(entries)) >= 0
}

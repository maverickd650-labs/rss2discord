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

// normalizeIdentity normalizes id when it's an http(s) URL (an entry's ID
// is frequently just its permalink, since the RSS/Atom parser falls back to
// Link, then Title, when a feed has no GUID): lowercases the scheme/host,
// drops the fragment, and strips known tracking query parameters. When
// fromLink is true, fragments and ref parameters are retained to identify distinct
// entries when no GUID is available. Anything that isn't a
// URL (an opaque GUID, or a Title used as a last-resort identity) is
// returned unchanged, since there's no generically safe way to normalize
// arbitrary text.
func normalizeIdentity(id string, fromLink bool) string {
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
	if !fromLink {
		parsed.Fragment = ""
	}

	if query := parsed.Query(); len(query) > 0 {
		for param := range query {
			if fromLink && strings.EqualFold(param, "ref") {
				continue
			}
			if trackingQueryParams[strings.ToLower(param)] || strings.HasPrefix(strings.ToLower(param), "utm_") {
				query.Del(param)
			}
		}
		parsed.RawQuery = query.Encode()
	}

	return parsed.String()
}

// entryIdentity resolves entry's stable identity, used for both the
// catch-up watermark (LastSeenEntry) and per-entry dedup (LastSent):
// entry.ID itself (already resolved via GUID -> Link -> Title precedence by
// the RSS/Atom parser), normalized according to its source when it's a URL.
func entryIdentity(entry rss.Entry) string {
	return normalizeIdentity(entry.ID, entry.IDFromLink)
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

// entriesContainID reports whether id matches any entry's identity after
// clampWatermark, as stored in LastSeenEntry.
func entriesContainID(entries []rss.Entry, id string) bool {
	for _, entry := range entries {
		if clampWatermark(entryIdentity(entry)) == id {
			return true
		}
	}
	return false
}

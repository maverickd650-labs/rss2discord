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
	"strings"

	neturl "net/url"

	"github.com/maverickd650-labs/rss2discord/internal/rss"
)

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

// normalizeURLIdentity lowercases an HTTP(S) URL's scheme and host and removes
// tracking query parameters. keepTarget preserves the fragment and ref parameter;
// otherwise both are removed. Invalid URLs and non-HTTP(S) identities are returned
// unchanged.
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
// IDs retain the fragment and ref parameter; other IDs use normalizeIdentity.
// An entry with no ID gets the parser's ContentID, so the identity is never empty.
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

// strippedLinkIdentity is a link-derived entry's identity without the
// fragment and ref parameter that entryIdentity keeps, or "" for an entry
// whose ID didn't come from its link. Some feeds rotate those between
// fetches (share-tracking plugins, CDN rewrites) on an otherwise unchanged
// article, so dedup and the watermark fall back to this form; see
// deliveryLedger (identityRecord.unique).
func strippedLinkIdentity(entry rss.Entry) string {
	if entry.IDSource != rss.IDFromLink || entry.ID == "" {
		return ""
	}
	return normalizeIdentity(entry.ID)
}

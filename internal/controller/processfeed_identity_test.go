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
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/maverickd650-labs/rss2discord/api/v1beta1"
	"github.com/maverickd650-labs/rss2discord/internal/rss"
)

// identityStateNS namespaces the metric series the identity tests create.
const identityStateNS = "identity-state"

// identityURLPrefix keeps the entry URLs of the identity tests unique in the package.
const identityURLPrefix = "https://identity-state.example.test/"

// linkEntry is an entry whose ID the parser took from its link, i the
// oldest-to-newest position that dates it.
func linkEntry(link string, i int) rss.Entry {
	return rss.Entry{ID: link, IDSource: rss.IDFromLink, Link: link, Title: link, Published: time.Unix(int64(1000+i), 0)}
}

// contentEntry is an entry with no GUID, link or title, as the parser
// returns it.
func contentEntry(description string, i int) rss.Entry {
	entry := rss.Entry{Description: description, Published: time.Unix(int64(1000+i), 0)}
	entry.ID, entry.IDSource = rss.ContentID(entry), rss.IDFromContent
	return entry
}

// guidEntry is an entry with a GUID, i the oldest-to-newest position that dates it.
func guidEntry(id string, i int) rss.Entry {
	return rss.Entry{ID: id, Title: id, Published: time.Unix(int64(1000+i), 0)}
}

// sentContents returns the content of each message the mock Discord server received.
func sentContents(t *testing.T, server *MockDiscordServer) []string {
	t.Helper()
	bodies := server.Bodies()
	contents := make([]string, 0, len(bodies))
	for _, body := range bodies {
		var payload struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		contents = append(contents, payload.Content)
	}
	return contents
}

// processTwice runs processFeed on entries twice and fails unless the first
// run sent exactly want and the second run sent nothing more.
func (f feedUnderTest) processTwice(t *testing.T, entries []rss.Entry, want ...string) {
	t.Helper()
	server := NewMockDiscordServer(t)
	defer server.Close()
	client := server.DiscordClientBuilder()(server.URL())
	for range 2 {
		// processFeed sorts entries in place, so hand each run its own copy.
		fetch := rss.FetchResult{Entries: slices.Clone(entries)}
		processFeed(t.Context(), testRun(t, f.fg, f.feed, fetch, nil, client))
	}
	if got := sentContents(t, server); !slices.Equal(got, want) {
		t.Fatalf("sent %q, want %q", got, want)
	}
	if fs := feedStatusFor(f.fg, f.feed.Name); fs.StateVersion != currentStateVersion {
		t.Fatalf("stateVersion=%d, want %d", fs.StateVersion, currentStateVersion)
	}
}

// Links that differ only by fragment or ref are distinct entries when they
// have no GUID, so neither is skipped as a duplicate of the other.
func TestProcessFeed_LinkFragmentAndRefAreDistinctIdentities(t *testing.T) {
	fg, feed := newMetricsFeedGroup(identityStateNS, "fragment-and-ref", tmplTitle)
	forgetMetricsAfter(t, fg.Namespace, fg.Name)
	entries := []rss.Entry{
		linkEntry(identityURLPrefix+"changelog#v1.2", 0),
		linkEntry(identityURLPrefix+"changelog#v1.3", 1),
		linkEntry(identityURLPrefix+"release?ref=v1.0", 2),
		linkEntry(identityURLPrefix+"release?ref=v1.1&utm_source=feed", 3),
	}
	feedUnderTest{fg, feed}.processTwice(t, entries,
		entries[0].Title, entries[1].Title, entries[2].Title, entries[3].Title)
	// A GUID that is a URL keeps its normalization: no churn for it.
	if entryIdentity(guidEntry(identityURLPrefix+"a#x", 0)) != identityURLPrefix+"a" {
		t.Fatal("GUID identity no longer drops the fragment")
	}
}

// A feed that rotates the ref parameter or fragment on its GUID-less links
// between fetches (share tracking, CDN rewrites) must not repost unchanged
// entries, nor lose the watermark: both fall back to the link without them
// when no other entry shares it. A genuinely new entry is still delivered.
func TestProcessFeed_RotatingLinkTargetIsNotResent(t *testing.T) {
	fg, feed := newMetricsFeedGroup(identityStateNS, "rotating-target", tmplTitle)
	forgetMetricsAfter(t, fg.Namespace, fg.Name)
	titled := func(link, title string, i int) rss.Entry {
		entry := linkEntry(identityURLPrefix+link, i)
		entry.Title = title
		return entry
	}
	first := []rss.Entry{titled("rotate/post-1?ref=a", "one", 0), titled("rotate/post-2#c1", "two", 1)}
	feedUnderTest{fg, feed}.processTwice(t, first, "one", "two")

	rotated := []rss.Entry{
		titled("rotate/post-1?ref=b", "one", 0),
		titled("rotate/post-2#c2", "two", 1),
		titled("rotate/post-3?ref=b", "three", 2),
	}
	feedUnderTest{fg, feed}.processTwice(t, rotated, "three")
	if fs := feedStatusFor(fg, feed.Name); fs.LastSeenEntry != entryIdentity(rotated[2]) {
		t.Fatalf("watermark=%q, want %q", fs.LastSeenEntry, entryIdentity(rotated[2]))
	}
}

// A GUID-less link whose stripped form is another entry's GUID is a
// different entry, not a rotation of it, so it is delivered even though the
// GUID was already sent.
func TestProcessFeed_LinkStrippedToAnotherGUIDIsDelivered(t *testing.T) {
	fg, feed := newMetricsFeedGroup(identityStateNS, "stripped-to-guid", tmplTitle)
	forgetMetricsAfter(t, fg.Namespace, fg.Name)
	guid := guidEntry(identityURLPrefix+"guid-collide/post", 0)
	feedUnderTest{fg, feed}.processTwice(t, []rss.Entry{guid}, guid.Title)
	link := linkEntry(identityURLPrefix+"guid-collide/post#v2", 1)
	feedUnderTest{fg, feed}.processTwice(t, []rss.Entry{guid, link}, link.Title)
}

// A GUID-less link is a new entry, not a rotation, when the only thing it
// shares a stripped form with is an already-sent GUID that has since left
// the feed: neither the GUID's dedup key nor its watermark may stand in for it.
func TestProcessFeed_LinkAfterGUIDWithSameBaseIsDelivered(t *testing.T) {
	fg, feed := newMetricsFeedGroup(identityStateNS, "link-after-guid", tmplTitle)
	forgetMetricsAfter(t, fg.Namespace, fg.Name)
	guid := guidEntry(identityURLPrefix+"after-guid/post", 0)
	feedUnderTest{fg, feed}.processTwice(t, []rss.Entry{guid}, guid.Title)
	link := linkEntry(identityURLPrefix+"after-guid/post#v2", 1)
	feedUnderTest{fg, feed}.processTwice(t, []rss.Entry{link}, link.Title)
}

// A watermark that kept a fragment or ref, and so came from a link, falls back
// to the link's stripped form only when it is unique in the fetch; with no such
// entry it reads as scrolled out.
func TestWatermarkIndex_LinkFallback(t *testing.T) {
	rotated := linkEntry(identityURLPrefix+"index/post?ref=b", 0)
	sibling := linkEntry(identityURLPrefix+"index/post?ref=c", 1)
	other := linkEntry(identityURLPrefix+"index/other", 2)
	watermark := normalizeLinkIdentity(identityURLPrefix + "index/post?ref=a")
	for _, tc := range []struct {
		name    string
		entries []rss.Entry
		want    int
	}{
		{name: "unique rotation", entries: []rss.Entry{other, rotated}, want: 1},
		{name: "shared stripped form", entries: []rss.Entry{rotated, sibling}, want: -1},
		{name: "no match", entries: []rss.Entry{other}, want: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &v1beta1.FeedStatus{LastSeenEntry: watermark}
			if got := newDeliveryLedger(fs, tc.entries).resume(); got.index != tc.want || (got.state == watermarkFound) != (tc.want >= 0) {
				t.Fatalf("resume = %+v, want index %d", got, tc.want)
			}
		})
	}
}

// Entries with no GUID, link or title get a non-empty identity, so a
// catchUpLimit 0 first sync stores a real watermark and the next fetch sends
// only what is new instead of the whole backlog.
func TestProcessFeed_EmptyIdentityEntriesKeepWatermark(t *testing.T) {
	fg, feed := newMetricsFeedGroup(identityStateNS, "empty-identity", "{{.Description}}")
	forgetMetricsAfter(t, fg.Namespace, fg.Name)
	fg.Spec.CatchUpLimit = new(int32(0))
	backlog := []rss.Entry{contentEntry("old-1", 0), contentEntry("old-2", 1)}
	// Entries built without an ID get the same identity the parser assigns.
	unparsed := rss.Entry{Description: "old-2", Published: backlog[1].Published}
	if entryIdentity(unparsed) != entryIdentity(backlog[1]) {
		t.Fatal("entryIdentity and the parser disagree on an empty identity")
	}

	feedUnderTest{fg, feed}.processTwice(t, backlog)
	fs := feedStatusFor(fg, feed.Name)
	if fs.LastSeenEntry == "" || !ledgerResumes(backlog, fs.LastSeenEntry) {
		t.Fatalf("backlog skip stored watermark %q", fs.LastSeenEntry)
	}
	fresh := contentEntry("new", 2)
	feedUnderTest{fg, feed}.processTwice(t, append(slices.Clone(backlog), fresh), "new")
}

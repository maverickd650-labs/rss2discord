package controller

import (
	"slices"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/rss"
)

const (
	prevIDNew     = "urn:previous-identity:new"
	prevIDOld     = "urn:previous-identity:old"
	prevIDOther   = "urn:previous-identity:other"
	prevLinkNew   = "https://previous-identity.example.test/new"
	prevLinkOld   = "https://previous-identity.example.test/old"
	prevSentAt    = "2025-12-31T00:00:00Z"
	prevSentLater = "2026-01-01T00:00:00Z"
)

// changedID is an entry whose ID the parser now picks differently: id is its
// current ID, previous the one earlier releases picked.
func changedID(id string, source rss.IDSource, previous string, previousSource rss.IDSource) rss.Entry {
	return rss.Entry{ID: id, IDSource: source, PreviousID: previous, PreviousIDSource: previousSource, Title: id}
}

func TestMigratePreviousIdentities(t *testing.T) {
	guid := func() rss.Entry { return changedID(prevIDNew, rss.IDFromGUID, prevIDOld, rss.IDFromGUID) }
	for _, tc := range []struct {
		name      string
		entries   []rss.Entry
		watermark string
		sent      map[string]string
		wantMark  string
		wantSent  map[string]string
	}{
		{
			name:      "guid moves the watermark and its dedup key",
			entries:   []rss.Entry{guid()},
			watermark: prevIDOld,
			sent:      map[string]string{hashIdentity(prevIDOld): prevSentAt},
			wantMark:  prevIDNew,
			wantSent:  map[string]string{hashIdentity(prevIDNew): prevSentAt},
		},
		{
			name:      "a watermark that names another entry is untouched",
			entries:   []rss.Entry{guid()},
			watermark: prevIDOther,
			sent:      map[string]string{hashIdentity(prevIDOld): prevSentAt, hashIdentity(prevIDOther): prevSentLater},
			wantMark:  prevIDOther,
			wantSent:  map[string]string{hashIdentity(prevIDNew): prevSentAt, hashIdentity(prevIDOther): prevSentLater},
		},
		{
			name:      "an existing key for the new identity is not overwritten",
			entries:   []rss.Entry{guid()},
			watermark: prevIDOld,
			sent:      map[string]string{hashIdentity(prevIDOld): prevSentAt, hashIdentity(prevIDNew): prevSentLater},
			wantMark:  prevIDNew,
			wantSent:  map[string]string{hashIdentity(prevIDNew): prevSentLater},
		},
		{
			name:      "link to link moves the stripped key too",
			entries:   []rss.Entry{changedID(prevLinkNew, rss.IDFromLink, prevLinkOld, rss.IDFromLink)},
			watermark: prevLinkOld,
			sent:      map[string]string{hashIdentity(prevLinkOld): prevSentAt, strippedKey(prevLinkOld): prevSentAt},
			wantMark:  prevLinkNew,
			wantSent:  map[string]string{hashIdentity(prevLinkNew): prevSentAt, strippedKey(prevLinkNew): prevSentAt},
		},
		{
			name:      "link to guid drops the old stripped key",
			entries:   []rss.Entry{changedID(prevIDNew, rss.IDFromGUID, prevLinkOld, rss.IDFromLink)},
			watermark: prevLinkOld,
			sent:      map[string]string{hashIdentity(prevLinkOld): prevSentAt, strippedKey(prevLinkOld): prevSentAt},
			wantMark:  prevIDNew,
			wantSent:  map[string]string{hashIdentity(prevIDNew): prevSentAt},
		},
		{
			name:      "an entry whose identity did not change is untouched",
			entries:   []rss.Entry{{ID: prevIDNew, PreviousID: prevIDNew}},
			watermark: prevIDNew,
			sent:      map[string]string{hashIdentity(prevIDNew): prevSentAt},
			wantMark:  prevIDNew,
			wantSent:  map[string]string{hashIdentity(prevIDNew): prevSentAt},
		},
		{
			name:      "an entry without a previous ID is untouched",
			entries:   []rss.Entry{{ID: prevIDNew}},
			watermark: prevIDOld,
			sent:      map[string]string{hashIdentity(prevIDOld): prevSentAt},
			wantMark:  prevIDOld,
			wantSent:  map[string]string{hashIdentity(prevIDOld): prevSentAt},
		},
		{
			name:      "an old identity two entries share is left alone",
			entries:   []rss.Entry{guid(), changedID(prevIDOther, rss.IDFromGUID, prevIDOld, rss.IDFromGUID)},
			watermark: prevIDOld,
			sent:      map[string]string{hashIdentity(prevIDOld): prevSentAt},
			wantMark:  prevIDOld,
			wantSent:  map[string]string{hashIdentity(prevIDOld): prevSentAt},
		},
		{
			name:      "an old identity that is another entry's current one is left alone",
			entries:   []rss.Entry{guid(), {ID: prevIDOld}},
			watermark: prevIDOld,
			sent:      map[string]string{hashIdentity(prevIDOld): prevSentAt},
			wantMark:  prevIDOld,
			wantSent:  map[string]string{hashIdentity(prevIDOld): prevSentAt},
		},
		{
			name:      "no recorded sends is fine",
			entries:   []rss.Entry{guid()},
			watermark: prevIDOld,
			wantMark:  prevIDNew,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &v1alpha1.FeedStatus{LastSeenEntry: tc.watermark, LastSent: tc.sent}
			// Idempotent: a second pass over the migrated status changes nothing.
			for range 2 {
				migratePreviousIdentities(fs, slices.Clone(tc.entries))
			}
			if fs.LastSeenEntry != tc.wantMark {
				t.Errorf("LastSeenEntry = %q, want %q", fs.LastSeenEntry, tc.wantMark)
			}
			if len(fs.LastSent) != len(tc.wantSent) {
				t.Errorf("LastSent = %v, want %v", fs.LastSent, tc.wantSent)
			}
			for key, want := range tc.wantSent {
				if got := fs.LastSent[key]; got != want {
					t.Errorf("LastSent[%s] = %q, want %q", key, got, want)
				}
			}
		})
	}
}

// A watermark too long for the status field is stored as a digest, on both
// sides of the move.
func TestMigratePreviousIdentities_LongWatermark(t *testing.T) {
	oldID := prevLinkOld + strings.Repeat("a", maxStatusTextBytes)
	entry := changedID(prevIDNew, rss.IDFromGUID, oldID, rss.IDFromGUID)
	fs := &v1alpha1.FeedStatus{LastSeenEntry: clampWatermark(normalizeIdentity(oldID))}

	migratePreviousIdentities(fs, []rss.Entry{entry})

	if want := clampWatermark(prevIDNew); fs.LastSeenEntry != want {
		t.Fatalf("LastSeenEntry = %q, want %q", fs.LastSeenEntry, want)
	}
}

// A status written before stateVersion 1 holds identities from the old parser's
// pick, so its legacy identity must be derived from the previous ID.
func TestLegacyEntryIdentity_UsesThePreviousID(t *testing.T) {
	entry := changedID(prevIDNew, rss.IDFromGUID, prevIDOld, rss.IDFromGUID)
	if got := legacyEntryIdentity(entry); got != prevIDOld {
		t.Fatalf("legacyEntryIdentity = %q, want %q", got, prevIDOld)
	}
}

// End to end through processFeed: a feed whose stored watermark and dedup keys
// name the old identity does not resend the entry whose ID the parser now
// picks differently, and carries on from there.
func TestProcessFeed_PreviousIdentityIsNotResent(t *testing.T) {
	fg, feed := newMetricsFeedGroup(legacyStateNS, "previous-identity", "{{.Title}}")
	defer deleteFeedGroupMetrics(fg.Namespace, fg.Name)
	fs := feedStatusFor(fg, feed.RSSUrl)
	fs.LastChecked, fs.Initialized, fs.StateVersion = "2025-12-29T00:00:00Z", true, currentStateVersion
	fs.LastSeenEntry = prevIDOld
	fs.LastSent = map[string]string{hashIdentity(prevIDOld): prevSentAt}

	moved := changedID(prevIDNew, rss.IDFromGUID, prevIDOld, rss.IDFromGUID)
	moved.Published = time.Unix(1000, 0)
	fresh := guidEntry(prevIDOther, 1)

	processTwice(t, fg, feed, []rss.Entry{moved, fresh}, fresh.Title)

	if fs := feedStatusFor(fg, feed.RSSUrl); fs.LastSeenEntry != prevIDOther {
		t.Fatalf("LastSeenEntry = %q, want %q", fs.LastSeenEntry, prevIDOther)
	} else if _, ok := fs.LastSent[hashIdentity(prevIDNew)]; !ok {
		t.Fatalf("the moved entry's dedup key is missing: %v", fs.LastSent)
	}
}

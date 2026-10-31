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
			name: "link to link onto a stripped identity a sibling shares drops the stripped key",
			entries: []rss.Entry{
				changedID(prevLinkNew+"#a", rss.IDFromLink, prevLinkOld, rss.IDFromLink),
				{ID: prevLinkNew + "#b", IDSource: rss.IDFromLink},
			},
			watermark: prevLinkOld,
			sent:      map[string]string{hashIdentity(prevLinkOld): prevSentAt, strippedKey(prevLinkOld): prevSentAt},
			wantMark:  clampWatermark(entryIdentity(rss.Entry{ID: prevLinkNew + "#a", IDSource: rss.IDFromLink})),
			wantSent: map[string]string{
				computeEntryKey(rss.Entry{ID: prevLinkNew + "#a", IDSource: rss.IDFromLink}): prevSentAt,
			},
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
			name: "two entries moving onto the same new identity are left alone",
			entries: []rss.Entry{
				changedID(prevIDNew, rss.IDFromGUID, prevIDOld, rss.IDFromGUID),
				changedID(prevIDNew, rss.IDFromGUID, prevIDOther, rss.IDFromGUID),
			},
			watermark: prevIDOld,
			sent:      map[string]string{hashIdentity(prevIDOld): prevSentAt, hashIdentity(prevIDOther): prevSentLater},
			wantMark:  prevIDOld,
			wantSent:  map[string]string{hashIdentity(prevIDOld): prevSentAt, hashIdentity(prevIDOther): prevSentLater},
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
				migratePreviousIdentities(fs, slices.Clone(tc.entries), newStrippedOwnership(tc.entries))
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

	migratePreviousIdentities(fs, []rss.Entry{entry}, newStrippedOwnership([]rss.Entry{entry}))

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
	forgetMetricsAfter(t, fg.Namespace, fg.Name)
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

// A sibling that is alone under a stripped identity must not be taken as sent
// because a migration recorded that stripped identity for another entry. Here
// the sibling shares the new stripped identity of a moved entry.
func TestMigratePreviousIdentities_SharedStrippedTargetNotRecorded(t *testing.T) {
	moved := changedID(prevLinkNew+"#a", rss.IDFromLink, prevLinkOld, rss.IDFromLink)
	sibling := rss.Entry{ID: prevLinkNew + "#b", IDSource: rss.IDFromLink, Title: "sibling"}
	fs := &v1alpha1.FeedStatus{LastSent: map[string]string{
		hashIdentity(prevLinkOld): prevSentAt, strippedKey(prevLinkOld): prevSentAt,
	}}
	migratePreviousIdentities(fs, []rss.Entry{moved, sibling}, newStrippedOwnership([]rss.Entry{moved, sibling}))

	// The moved entry leaves the feed; the sibling is now alone under the
	// stripped identity.
	if newDeliveryLedger(fs, []rss.Entry{sibling}).sent(sibling) {
		t.Fatalf("sibling taken as sent; LastSent = %v", fs.LastSent)
	}
}

// The legacy migration records a link's legacy (stripped) identity only when it
// is the entry's own unique stripped identity: an entry whose legacy identity
// came from its previous ID must not leave a key a sibling would later match.
func TestMigrateLegacyIdentities_StrippedKeyOnlyForOwnUniqueIdentity(t *testing.T) {
	const (
		ownLink    = "https://previous-identity.example.test/own"
		sharedLink = "https://previous-identity.example.test/shared"
	)
	moved := changedID(ownLink, rss.IDFromLink, sharedLink+"#p", rss.IDFromLink)
	sibling := changedID(sharedLink+"#b", rss.IDFromLink, "urn:previous-identity:sibling", rss.IDFromGUID)
	entries := []rss.Entry{moved, sibling}
	legacy := legacyEntryIdentity(moved)
	if legacy != normalizeIdentity(sharedLink+"#p") {
		t.Fatalf("legacy identity = %q, test assumption broken", legacy)
	}
	fs := &v1alpha1.FeedStatus{LastSent: map[string]string{hashIdentity(legacy): prevSentAt}}
	migrateLegacyIdentities(fs, slices.Clone(entries), false, defaultCatchUpLimit, newStrippedOwnership(entries))

	if _, ok := fs.LastSent[computeEntryKey(moved)]; !ok {
		t.Errorf("moved entry's key not carried over; LastSent = %v", fs.LastSent)
	}
	if _, ok := fs.LastSent[strippedKey(legacy)]; ok {
		t.Errorf("stripped key %q recorded for a legacy identity that is not the entry's own", legacy)
	}
	if newDeliveryLedger(fs, []rss.Entry{sibling}).sent(sibling) {
		t.Errorf("sibling taken as sent; LastSent = %v", fs.LastSent)
	}
}

// When pruning has left a moved entry's send recorded only under its old
// stripped key and its new stripped identity is shared, that record falls back
// to the entry's new key, so the entry is not sent again; but only when no
// other entry can claim the old stripped identity.
func TestMigratePreviousIdentities_SharedTargetFallsBackToEntryKey(t *testing.T) {
	moved := changedID(prevLinkNew+"#a", rss.IDFromLink, prevLinkOld, rss.IDFromLink)
	sibling := rss.Entry{ID: prevLinkNew + "#b", IDSource: rss.IDFromLink, Title: "sibling"}
	for _, tc := range []struct {
		name      string
		entries   []rss.Entry
		wantMoved bool
	}{
		{name: "sole owner keeps its record", entries: []rss.Entry{moved, sibling}, wantMoved: true},
		{
			name: "an entry now under the old stripped identity makes it ambiguous",
			entries: []rss.Entry{moved, sibling,
				{ID: prevLinkOld + "#z", IDSource: rss.IDFromLink, Title: "claimant"}},
		},
		{
			name: "another entry previously under it makes it ambiguous",
			entries: []rss.Entry{moved, sibling,
				changedID(prevIDOther, rss.IDFromGUID, prevLinkOld+"#y", rss.IDFromLink)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The full key was pruned; only the stripped key is left.
			fs := &v1alpha1.FeedStatus{LastSent: map[string]string{strippedKey(prevLinkOld): prevSentAt}}
			migratePreviousIdentities(fs, slices.Clone(tc.entries), newStrippedOwnership(tc.entries))

			if _, ok := fs.LastSent[strippedKey(prevLinkOld)]; ok {
				t.Errorf("old stripped key kept; LastSent = %v", fs.LastSent)
			}
			if _, ok := fs.LastSent[strippedKey(normalizeIdentity(prevLinkNew))]; ok {
				t.Errorf("shared new stripped key recorded; LastSent = %v", fs.LastSent)
			}
			got, ok := fs.LastSent[computeEntryKey(moved)]
			if ok != tc.wantMoved || (ok && got != prevSentAt) {
				t.Errorf("moved entry key = %q (present %v), want present %v at %q", got, ok, tc.wantMoved, prevSentAt)
			}
			if newDeliveryLedger(fs, []rss.Entry{sibling}).sent(sibling) {
				t.Errorf("sibling taken as sent; LastSent = %v", fs.LastSent)
			}
		})
	}
}

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
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/maverickd650-labs/rss2discord/api/v1alpha1"
	"github.com/maverickd650-labs/rss2discord/internal/rss"
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

// ownershipLink is a link-derived entry with id as both ID and title.
func ownershipLink(id string) rss.Entry {
	return rss.Entry{ID: id, IDSource: rss.IDFromLink, Title: id}
}

// indexOfEntry is where entry sits in entries, which must hold it.
func indexOfEntry(entries []rss.Entry, entry rss.Entry) int {
	return slices.IndexFunc(entries, func(e rss.Entry) bool { return refOf(e) == refOf(entry) })
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
			fs := &v1alpha1.FeedStatus{LastSeenEntry: tc.watermark, LastSent: maps.Clone(tc.sent)}
			// Check both the initial migration and its idempotent second pass.
			for pass := range 2 {
				newDeliveryLedger(fs, tc.entries)
				if fs.LastSeenEntry != tc.wantMark {
					t.Errorf("pass %d: LastSeenEntry = %q, want %q", pass+1, fs.LastSeenEntry, tc.wantMark)
				}
				if !maps.Equal(fs.LastSent, tc.wantSent) {
					t.Errorf("pass %d: LastSent = %v, want %v", pass+1, fs.LastSent, tc.wantSent)
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

	newDeliveryLedger(fs, []rss.Entry{entry})

	if want := clampWatermark(prevIDNew); fs.LastSeenEntry != want {
		t.Fatalf("LastSeenEntry = %q, want %q", fs.LastSeenEntry, want)
	}
}

// End to end through processFeed: a feed whose stored watermark and dedup keys
// name the old identity does not resend the entry whose ID the parser now
// picks differently, and carries on from there.
func TestProcessFeed_PreviousIdentityIsNotResent(t *testing.T) {
	fg, feed := newMetricsFeedGroup(identityStateNS, "previous-identity", "{{.Title}}")
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
	newDeliveryLedger(fs, []rss.Entry{moved, sibling})

	// The moved entry leaves the feed; the sibling is now alone under the
	// stripped identity.
	if newDeliveryLedger(fs, []rss.Entry{sibling}).sent(sibling) {
		t.Fatalf("sibling taken as sent; LastSent = %v", fs.LastSent)
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
			newDeliveryLedger(fs, tc.entries)

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

// TestStrippedOwnership_OwnsPrevious covers the question the previous-identity
// migration asks: can the old stripped key only be this one moved entry's?
func TestStrippedOwnership_OwnsPrevious(t *testing.T) {
	mover := changedID(prevLinkNew+"#a", rss.IDFromLink, prevLinkOld, rss.IDFromLink)
	moverUniqueA := changedID(prevLinkNew+"#a", rss.IDFromLink, prevLinkOld+"#x", rss.IDFromLink)
	moverUniqueB := changedID(prevLinkNew+"#b", rss.IDFromLink, prevLinkOld+"#y", rss.IDFromLink)
	sibling := ownershipLink(prevLinkNew + "#b")
	otherMover := changedID(prevIDOther, rss.IDFromGUID, prevLinkOld+"#y", rss.IDFromLink)
	claimant := ownershipLink(prevLinkOld + "#z")
	guidClaimant := rss.Entry{ID: normalizeIdentity(prevLinkOld), IDSource: rss.IDFromGUID}
	guidMover := changedID(prevIDNew, rss.IDFromGUID, prevIDOld, rss.IDFromGUID)

	for _, tc := range []struct {
		name    string
		entries []rss.Entry
		mover   rss.Entry
		want    bool
	}{
		{name: "the only mover off that stripped identity", entries: []rss.Entry{mover, sibling}, mover: mover, want: true},
		{name: "another mover had the same stripped identity", entries: []rss.Entry{mover, sibling, otherMover}, mover: mover},
		{name: "distinct new identities do not disambiguate a shared previous stripped identity", entries: []rss.Entry{moverUniqueA, moverUniqueB}, mover: moverUniqueA},
		{name: "a current entry has it as its stripped identity", entries: []rss.Entry{mover, sibling, claimant}, mover: mover},
		{name: "a current entry has it as its identity", entries: []rss.Entry{mover, sibling, guidClaimant}, mover: mover},
		{name: "a previous ID without a stripped form", entries: []rss.Entry{guidMover}, mover: guidMover},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := newDeliveryLedger(&v1alpha1.FeedStatus{}, tc.entries).ownsPrevious(indexOfEntry(tc.entries, tc.mover)); got != tc.want {
				t.Errorf("ownsPrevious = %v, want %v", got, tc.want)
			}
		})
	}
}

// A previous stripped identity claimed by another moved or current entry is
// ambiguous even when a mover's new stripped identity is unique. No entry may
// inherit its key.
func TestMigratePreviousIdentities_SharedPreviousStrippedIdentityWithUniqueTargets(t *testing.T) {
	movedA := changedID(prevLinkNew+"#a", rss.IDFromLink, prevLinkOld+"#x", rss.IDFromLink)
	movedB := changedID(prevLinkNew+"#b", rss.IDFromLink, prevLinkOld+"#y", rss.IDFromLink)
	claimant := ownershipLink(prevLinkOld + "#z")

	for _, tc := range []struct {
		name    string
		entries []rss.Entry
	}{
		{name: "two movers share the previous stripped identity", entries: []rss.Entry{movedA, movedB}},
		{name: "a current entry claims the previous stripped identity", entries: []rss.Entry{movedA, claimant}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &v1alpha1.FeedStatus{LastSent: map[string]string{
				strippedKey(prevLinkOld): prevSentAt,
			}}
			ledger := newDeliveryLedger(fs, tc.entries)

			if _, ok := fs.LastSent[strippedKey(prevLinkOld)]; ok {
				t.Fatalf("ambiguous previous stripped key kept; LastSent = %v", fs.LastSent)
			}
			for _, entry := range tc.entries {
				if ledger.sent(entry) {
					t.Errorf("entry %q inherited an ambiguous previous stripped key; LastSent = %v", entry.ID, fs.LastSent)
				}
				if _, ok := fs.LastSent[computeEntryKey(entry)]; ok {
					t.Errorf("entry %q unexpectedly received a sent key; LastSent = %v", entry.ID, fs.LastSent)
				}
				if _, ok := fs.LastSent[strippedKey(normalizeIdentity(entry.ID))]; ok {
					t.Errorf("entry %q unexpectedly received a stripped key; LastSent = %v", entry.ID, fs.LastSent)
				}
			}
		})
	}
}

// The previous-identity pass only moves a stripped LastSent key onto a unique
// stripped identity, so a sibling alone under the identity the moved entry
// left must not read as sent.
func TestMigratePreviousIdentities_SiblingNeverInheritsAStrippedKey(t *testing.T) {
	// The sibling now shares the moved entry's new stripped identity, and the
	// full key was pruned, leaving only the old stripped key.
	moved := changedID(prevLinkNew+"#a", rss.IDFromLink, prevLinkOld, rss.IDFromLink)
	sibling := ownershipLink(prevLinkNew + "#b")
	fs := currentStatus()
	fs.LastSent = map[string]string{strippedKey(prevLinkOld): prevSentAt}
	ledger := newDeliveryLedger(fs, []rss.Entry{moved, sibling})

	if _, ok := fs.LastSent[strippedKey(normalizeIdentity(prevLinkNew))]; ok {
		t.Errorf("shared stripped key recorded; LastSent = %v", fs.LastSent)
	}
	if !ledger.sent(moved) || ledger.sent(sibling) {
		t.Errorf("sent(moved), sent(sibling) = %v, %v; want true, false", ledger.sent(moved), ledger.sent(sibling))
	}
}

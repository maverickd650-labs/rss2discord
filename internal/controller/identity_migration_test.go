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
	"testing"

	"github.com/maverickd650-labs/rss2discord/api/v1alpha1"
	"github.com/maverickd650-labs/rss2discord/internal/rss"
)

const (
	ownLinkBase    = "https://ownership.example.test/own"
	sharedLinkBase = "https://ownership.example.test/shared"
)

// ownershipLink is a link-derived entry with id as both ID and title.
func ownershipLink(id string) rss.Entry {
	return rss.Entry{ID: id, IDSource: rss.IDFromLink, Title: id}
}

// indexOfEntry is where entry sits in entries, which must hold it.
func indexOfEntry(entries []rss.Entry, entry rss.Entry) int {
	return slices.IndexFunc(entries, func(e rss.Entry) bool { return refOf(e) == refOf(entry) })
}

// TestStrippedOwnership_OwnStripped covers the question the previous-identity migration
// asks through recordSentAt: may this stripped identity be recorded for this
// entry? The entry key is always written; the stripped key only when want is set.
func TestStrippedOwnership_OwnStripped(t *testing.T) {
	alone := ownershipLink(ownLinkBase + "#frag")
	twinA, twinB := ownershipLink(sharedLinkBase+"#a"), ownershipLink(sharedLinkBase+"#b")
	guidIsLink := rss.Entry{ID: normalizeIdentity(ownLinkBase + "/g"), IDSource: rss.IDFromGUID}
	linkOverGUID := ownershipLink(ownLinkBase + "/g#x")

	for _, tc := range []struct {
		name     string
		entries  []rss.Entry
		entry    rss.Entry
		stripped string
		want     string
	}{
		{name: "own unique stripped identity", entries: []rss.Entry{alone}, entry: alone, stripped: normalizeIdentity(ownLinkBase), want: normalizeIdentity(ownLinkBase)},
		{name: "a stripped identity a sibling shares", entries: []rss.Entry{twinA, twinB}, entry: twinA, stripped: normalizeIdentity(sharedLinkBase)},
		{name: "a stripped identity that is another entry's own identity", entries: []rss.Entry{linkOverGUID, guidIsLink}, entry: linkOverGUID, stripped: normalizeIdentity(ownLinkBase + "/g")},
		{name: "a stripped identity recovered from the previous ID is not the entry's own", entries: []rss.Entry{alone}, entry: alone, stripped: normalizeIdentity(sharedLinkBase)},
		{name: "no stripped identity", entries: []rss.Entry{alone}, entry: alone},
		{name: "an entry without a link identity", entries: []rss.Entry{guidIsLink}, entry: guidIsLink, stripped: guidIsLink.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &v1alpha1.FeedStatus{LastSent: map[string]string{}}
			ledger := newDeliveryLedger(fs, tc.entries)
			i := indexOfEntry(tc.entries, tc.entry)
			ledger.recordSentAt(i, "t", tc.stripped)
			want := map[string]string{ledger.record(i).key: "t"}
			if tc.want != "" {
				want[strippedKey(tc.want)] = "t"
			}
			if !maps.Equal(fs.LastSent, want) {
				t.Errorf("recordSentAt wrote %v, want %v", fs.LastSent, want)
			}
		})
	}
}

// TestStrippedOwnership_OwnsPrevious covers the question the previous-identity
// migration asks: can the old stripped key only be this one moved entry's?
func TestStrippedOwnership_OwnsPrevious(t *testing.T) {
	mover := changedID(prevLinkNew+"#a", rss.IDFromLink, prevLinkOld, rss.IDFromLink)
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
	s := openFeedSync(fs, syncFetch(moved, sibling), syncSpec(defaultCatchUpLimit))
	s.begin()

	if _, ok := fs.LastSent[strippedKey(normalizeIdentity(prevLinkNew))]; ok {
		t.Errorf("shared stripped key recorded; LastSent = %v", fs.LastSent)
	}
	if !s.sent(moved) || s.sent(sibling) {
		t.Errorf("sent(moved), sent(sibling) = %v, %v; want true, false", s.sent(moved), s.sent(sibling))
	}
}

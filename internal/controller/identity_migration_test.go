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
	"testing"

	"github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/rss"
)

const (
	ownLinkBase    = "https://ownership.example.test/own"
	sharedLinkBase = "https://ownership.example.test/shared"
)

// ownershipLink is a link-derived entry with id as both ID and title.
func ownershipLink(id string) rss.Entry {
	return rss.Entry{ID: id, IDSource: rss.IDFromLink, Title: id}
}

// TestStrippedOwnership_OwnStripped covers the question the legacy migration
// asks: may this stripped identity be recorded for this entry?
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
			if got := newStrippedOwnership(tc.entries).ownStripped(tc.entry, tc.stripped); got != tc.want {
				t.Errorf("ownStripped = %q, want %q", got, tc.want)
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
			previous, _ := previouslyIdentified(tc.mover)
			if got := newStrippedOwnership(tc.entries).ownsPrevious(previous); got != tc.want {
				t.Errorf("ownsPrevious = %v, want %v", got, tc.want)
			}
		})
	}
}

// The legacy pass records a stripped LastSent key only for an entry's own
// unique stripped identity, and the previous-identity pass only moves one onto
// a unique one. Run together through migrateIdentities over one fetch, a
// sibling alone under the identity both of them left must not read as sent.
func TestMigrateIdentities_SiblingNeverInheritsAStrippedKey(t *testing.T) {
	moved := changedID(ownLinkBase, rss.IDFromLink, sharedLinkBase+"#p", rss.IDFromLink)
	sibling := changedID(sharedLinkBase+"#b", rss.IDFromLink, prevIDOld, rss.IDFromGUID)
	entries := []rss.Entry{moved, sibling}
	legacy := legacyEntryIdentity(moved)

	t.Run("legacy status", func(t *testing.T) {
		fs := &v1alpha1.FeedStatus{RSSUrl: exampleFeedURL, LastSent: map[string]string{hashIdentity(legacy): prevSentAt}}
		s := openFeedSync(fs, syncFetch(entries...), syncSpec(defaultCatchUpLimit))
		s.begin()

		if got := fs.LastSent[computeEntryKey(moved)]; got != prevSentAt {
			t.Errorf("moved entry's key = %q, want the carried-over %q; LastSent = %v", got, prevSentAt, fs.LastSent)
		}
		if _, ok := fs.LastSent[strippedKey(legacy)]; ok {
			t.Errorf("stripped key %q recorded for an identity that is not the entry's own", legacy)
		}
		if !s.sent(moved) || s.sent(sibling) {
			t.Errorf("sent(moved), sent(sibling) = %v, %v; want true, false", s.sent(moved), s.sent(sibling))
		}
	})

	t.Run("current status", func(t *testing.T) {
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
	})
}

// migrateIdentities translates a legacy status first: the legacy identity it
// finds under a stateVersion 0 key is the entry's previous ID's stripped form,
// which the previous-identity pass could not match, and it is idempotent.
func TestMigrateIdentities_LegacyThenPrevious(t *testing.T) {
	moved := changedID(prevIDNew, rss.IDFromGUID, prevLinkOld+"#frag", rss.IDFromLink)
	legacy := legacyEntryIdentity(moved)
	if legacy != normalizeIdentity(prevLinkOld) {
		t.Fatalf("legacy identity = %q, test assumption broken", legacy)
	}
	fs := &v1alpha1.FeedStatus{
		LastSeenEntry: legacy,
		LastSent:      map[string]string{hashIdentity(legacy): prevSentAt, strippedKey(legacy): prevSentAt},
	}
	state := readLegacyState(fs)
	for range 2 {
		migrateIdentities(fs, []rss.Entry{moved}, &state, defaultCatchUpLimit)
	}

	if !state.migrated {
		t.Error("the legacy pass did not run for a stateVersion 0 status")
	}
	if fs.LastSeenEntry != prevIDNew {
		t.Errorf("LastSeenEntry = %q, want %q", fs.LastSeenEntry, prevIDNew)
	}
	if got := fs.LastSent[hashIdentity(prevIDNew)]; got != prevSentAt {
		t.Errorf("new key = %q, want %q; LastSent = %v", got, prevSentAt, fs.LastSent)
	}
	if _, ok := fs.LastSent[hashIdentity(legacy)]; ok {
		t.Errorf("legacy key kept; LastSent = %v", fs.LastSent)
	}
}

// For a current status migrateIdentities runs only the previous-identity pass.
func TestMigrateIdentities_CurrentStatusSkipsTheLegacyPass(t *testing.T) {
	moved := changedID(prevIDNew, rss.IDFromGUID, prevIDOld, rss.IDFromGUID)
	fs := &v1alpha1.FeedStatus{
		StateVersion:  currentStateVersion,
		LastSeenEntry: prevIDOld,
		LastSent:      map[string]string{hashIdentity(prevIDOld): prevSentAt},
	}
	state := readLegacyState(fs)
	migrateIdentities(fs, []rss.Entry{moved}, &state, defaultCatchUpLimit)

	if state.migrated {
		t.Error("the legacy pass ran for a current status")
	}
	if fs.LastSeenEntry != prevIDNew || fs.LastSent[hashIdentity(prevIDNew)] != prevSentAt || len(fs.LastSent) != 1 {
		t.Errorf("LastSeenEntry = %q, LastSent = %v; want the previous-identity move", fs.LastSeenEntry, fs.LastSent)
	}
}

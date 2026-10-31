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
	"testing"

	"github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/rss"
)

// ledgerResumes reports whether watermark, as stored in LastSeenEntry, stands
// for any of entries.
func ledgerResumes(entries []rss.Entry, watermark string) bool {
	fs := &v1alpha1.FeedStatus{LastSeenEntry: watermark}
	return newDeliveryLedger(fs, entries).resume(entries).state == watermarkFound
}

func TestDeliveryLedger_Resume(t *testing.T) {
	const idA = "guid-a"
	a := rss.Entry{ID: idA}
	b := rss.Entry{ID: "guid-b"}
	c := rss.Entry{ID: "guid-c"}
	longID := strings.Repeat("x", maxStatusTextBytes+10)
	for _, tc := range []struct {
		name      string
		entries   []rss.Entry
		watermark string
		wantState watermarkState
		wantIndex int
	}{
		{name: "match in the middle", entries: []rss.Entry{a, b, c}, watermark: "guid-b", wantState: watermarkFound, wantIndex: 1},
		{name: "match first", entries: []rss.Entry{a, b}, watermark: idA, wantState: watermarkFound, wantIndex: 0},
		{name: "scrolled out", entries: []rss.Entry{a, b}, watermark: "guid-z", wantState: watermarkLost, wantIndex: -1},
		{name: "empty watermark", entries: []rss.Entry{a, b}, wantState: watermarkNone, wantIndex: -1},
		{name: "no entries", watermark: idA, wantState: watermarkLost, wantIndex: -1},
		{name: "clamped long identity", entries: []rss.Entry{a, {ID: longID}}, watermark: clampWatermark(longID), wantState: watermarkFound, wantIndex: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &v1alpha1.FeedStatus{LastSeenEntry: tc.watermark}
			got := newDeliveryLedger(fs, tc.entries).resume(tc.entries)
			if got != (watermarkLookup{tc.wantState, tc.wantIndex}) {
				t.Fatalf("resume = %+v, want {state %d, index %d}", got, tc.wantState, tc.wantIndex)
			}
		})
	}
}

// ledgerTestNow is the send timestamp the ledger tests record.
const ledgerTestNow = "2026-01-02T03:04:05Z"

func TestDeliveryLedger_MarkHandledAndSent(t *testing.T) {
	unique := linkEntry(legacyURLPrefix+"index/post?ref=a", 0)
	rotated := linkEntry(legacyURLPrefix+"index/post?ref=b", 1)
	siblingA := linkEntry(legacyURLPrefix+"index/post?ref=c", 2)
	siblingB := linkEntry(legacyURLPrefix+"index/post?ref=d", 3)
	other := linkEntry(legacyURLPrefix+"index/other", 4)
	guid := rss.Entry{ID: "guid-1"}

	for _, tc := range []struct {
		name string
		// fetch is the whole fetch the ledger is built over; handled is marked.
		fetch   []rss.Entry
		handled rss.Entry
		// probe is checked with sent afterwards.
		probe    rss.Entry
		wantSent bool
		// wantResume is whether the new watermark finds an entry in fetch.
		wantResume bool
		// wantKeys is how many LastSent keys markHandled should have written.
		wantKeys int
	}{
		{name: "guid entry records its own key", fetch: []rss.Entry{guid, other}, handled: guid, probe: guid, wantSent: true, wantResume: true, wantKeys: 1},
		{name: "unique link also records stripped key", fetch: []rss.Entry{unique, other}, handled: unique, probe: unique, wantSent: true, wantResume: true, wantKeys: 2},
		{name: "rotated ref still found via stripped key", fetch: []rss.Entry{rotated, other}, handled: unique, probe: rotated, wantSent: true, wantResume: true, wantKeys: 2},
		{name: "shared stripped form records only own key", fetch: []rss.Entry{siblingA, siblingB}, handled: siblingA, probe: siblingB, wantResume: true, wantKeys: 1},
		{name: "unrelated entry is not sent", fetch: []rss.Entry{guid, other}, handled: guid, probe: other, wantResume: true, wantKeys: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &v1alpha1.FeedStatus{LastSent: map[string]string{}}
			ledger := newDeliveryLedger(fs, tc.fetch)
			if ledger.sent(tc.handled) {
				t.Fatal("entry reads as sent before being handled")
			}
			ledger.markHandled(tc.handled, ledgerTestNow)
			if got := ledger.sent(tc.probe); got != tc.wantSent {
				t.Fatalf("sent(probe) = %v, want %v", got, tc.wantSent)
			}
			if len(fs.LastSent) != tc.wantKeys {
				t.Fatalf("LastSent has %d keys, want %d: %v", len(fs.LastSent), tc.wantKeys, fs.LastSent)
			}
			for key, at := range fs.LastSent {
				if at != ledgerTestNow {
					t.Fatalf("LastSent[%s] = %q, want %q", key, at, ledgerTestNow)
				}
			}
			if want := clampWatermark(entryIdentity(tc.handled)); fs.LastSeenEntry != want {
				t.Fatalf("LastSeenEntry = %q, want %q", fs.LastSeenEntry, want)
			}
			if found := ledger.resume(tc.fetch).state == watermarkFound; found != tc.wantResume {
				t.Fatalf("resume found = %v after markHandled, want %v", found, tc.wantResume)
			}
		})
	}
}

func TestDeliveryLedger_MarkHandledClampsLongIdentity(t *testing.T) {
	long := rss.Entry{ID: strings.Repeat("y", maxStatusTextBytes*2)}
	fs := &v1alpha1.FeedStatus{LastSent: map[string]string{}}
	ledger := newDeliveryLedger(fs, []rss.Entry{long})
	ledger.markHandled(long, ledgerTestNow)
	if len(fs.LastSeenEntry) > maxStatusTextBytes {
		t.Fatalf("LastSeenEntry is %d bytes, want <= %d", len(fs.LastSeenEntry), maxStatusTextBytes)
	}
	if !ledgerResumes([]rss.Entry{long}, fs.LastSeenEntry) {
		t.Fatal("clamped watermark no longer resumes at its entry")
	}
}

func TestDeliveryLedger_MarkNewest(t *testing.T) {
	entries := []rss.Entry{{ID: "guid-old"}, {ID: "guid-new"}}
	fs := &v1alpha1.FeedStatus{LastSent: map[string]string{}}
	newDeliveryLedger(fs, entries).markNewest(entries)
	if fs.LastSeenEntry != "guid-new" {
		t.Fatalf("LastSeenEntry = %q, want guid-new", fs.LastSeenEntry)
	}
	if len(fs.LastSent) != 0 {
		t.Fatalf("markNewest recorded sent keys: %v", fs.LastSent)
	}
}

// The counts cover the fetch the ledger was built over, not whatever slice is
// scanned later, so trimming the scan for catch-up does not make a shared
// stripped identity look unique.
func TestDeliveryLedger_CountsCoverWholeFetch(t *testing.T) {
	siblingA := linkEntry(legacyURLPrefix+"index/post?ref=c", 0)
	siblingB := linkEntry(legacyURLPrefix+"index/post?ref=d", 1)
	fetch := []rss.Entry{siblingA, siblingB}
	fs := &v1alpha1.FeedStatus{LastSent: map[string]string{}}
	ledger := newDeliveryLedger(fs, fetch)
	ledger.markHandled(siblingB, ledgerTestNow) // as if only the trimmed tail were scanned
	if ledger.sent(siblingA) {
		t.Fatal("a sibling sharing the stripped identity was treated as sent")
	}
}

func TestPruneLastSent(t *testing.T) {
	t.Run("ties on timestamp are broken deterministically by key", func(t *testing.T) {
		const ts = "2024-01-01T00:00:00Z"
		for range 50 {
			sent := map[string]string{}
			for _, k := range []string{"e", "b", "g", "a", "f", "c", "d"} {
				sent[k] = ts
			}
			pruneLastSent(sent, 3)
			if len(sent) != 3 {
				t.Fatalf("expected 3 entries, got %d", len(sent))
			}
			for _, k := range []string{"e", "f", "g"} {
				if _, ok := sent[k]; !ok {
					t.Fatalf("expected %q to be kept, got %v", k, sent)
				}
			}
		}
	})

	t.Run("under cap unchanged", func(t *testing.T) {
		sent := map[string]string{"a": "2024-01-01T00:00:00Z", "b": "2024-01-02T00:00:00Z"}
		pruneLastSent(sent, 5)
		if len(sent) != 2 {
			t.Fatalf("expected map unchanged, got %d entries", len(sent))
		}
	})

	t.Run("drops oldest by timestamp", func(t *testing.T) {
		sent := map[string]string{
			"oldest": "2024-01-01T00:00:00Z",
			"middle": "2024-01-02T00:00:00Z",
			"newest": "2024-01-03T00:00:00Z",
		}
		pruneLastSent(sent, 2)
		if len(sent) != 2 {
			t.Fatalf("expected 2 entries after prune, got %d", len(sent))
		}
		if _, ok := sent["oldest"]; ok {
			t.Fatal("expected the oldest entry to be pruned")
		}
		if _, ok := sent["newest"]; !ok {
			t.Fatal("expected the newest entry to be retained")
		}
	})
}

func TestComputeEntryKey(t *testing.T) {
	const entryLink = "http://x"
	a := computeEntryKey(rss.Entry{ID: "1", Link: entryLink, Title: "T"})
	b := computeEntryKey(rss.Entry{ID: "1", Link: entryLink, Title: "T"})
	c := computeEntryKey(rss.Entry{ID: "2", Link: entryLink, Title: "T"})

	if a != b {
		t.Fatal("expected identical entries to produce the same key")
	}
	if a == c {
		t.Fatal("expected differing IDs to produce different keys")
	}
	if len(a) != 64 {
		t.Fatalf("expected 64-char hex sha256, got %d chars", len(a))
	}
}

func TestComputeEntryKey_IgnoresLinkAndTitleChurn(t *testing.T) {
	// A feed editing a headline or rotating a tracking parameter on an
	// otherwise-unchanged article must not look like a new entry, as long
	// as its GUID-derived ID is unchanged (the failure mode this guards
	// against: a feed like the Guardian's that live-edits articles in
	// place would otherwise get re-sent as a "new" duplicate every time the
	// headline changed).
	original := rss.Entry{ID: "churn-test-entry", Link: "https://example.com/churn", Title: "Original Title"}
	edited := rss.Entry{ID: "churn-test-entry", Link: "https://example.com/churn-edited?utm_source=feed", Title: "Updated Title"}

	if computeEntryKey(original) != computeEntryKey(edited) {
		t.Fatal("expected entries sharing an ID to produce the same key despite a link and title edit")
	}
}

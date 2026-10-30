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
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/rss"
)

// These tests drive a feedSync -- open, begin, close -- with in-memory entries.
// There is no Discord server and no API server: the send loop is played by the
// test, which marks entries handled (a send that worked) or closes with
// syncPending (one that did not).

const (
	syncTestNow      = "2026-02-03T04:05:06Z"
	syncTestModified = "Thu, 01 Jan 2026 00:00:00 GMT"
	syncTestOldMod   = "Wed, 31 Dec 2025 00:00:00 GMT"
	// syncTestTitle is the feed title of every fetch; syncTestLegacyChecked is
	// when a legacy status was last checked.
	syncTestTitle         = "Sync Feed"
	syncTestLegacyChecked = "2025-12-29T00:00:00Z"
	// scrolledOutWatermark is a LastSeenEntry that no longer appears in the feed.
	scrolledOutWatermark = "scrolled-out"
)

// syncEntries returns n GUID entries, oldest first.
func syncEntries(n int) []rss.Entry {
	entries := make([]rss.Entry, n)
	for i := range entries {
		entries[i] = guidEntry(fmt.Sprintf("sync-entry-%02d", i), i)
	}
	return entries
}

func syncIDs(entries []rss.Entry) []string {
	ids := make([]string, len(entries))
	for i, entry := range entries {
		ids[i] = entry.ID
	}
	return ids
}

// syncFetch is a fetch of entries (copied, since begin sorts in place) that
// carries the "next" validators.
func syncFetch(entries ...rss.Entry) rss.FetchResult {
	return rss.FetchResult{
		Entries:      slices.Clone(entries),
		FeedTitle:    syncTestTitle,
		ETag:         validatorsNext,
		LastModified: syncTestModified,
	}
}

func syncSpec(catchUpLimit int) feedSyncSpec {
	return feedSyncSpec{catchUpLimit: catchUpLimit, now: syncTestNow}
}

// currentStatus is a feed status in the current identity scheme that was
// initialized by an earlier sync and holds the "old" validators.
func currentStatus() *v1alpha1.FeedStatus {
	return &v1alpha1.FeedStatus{
		RSSUrl:        exampleFeedURL,
		Initialized:   true,
		StateVersion:  currentStateVersion,
		LastChecked:   "2026-01-01T00:00:00Z",
		ETag:          validatorsOld,
		LastModified:  syncTestOldMod,
		LastSent:      map[string]string{},
		LastSeenEntry: "",
	}
}

// ambiguousLegacyStatus is a status written before stateVersion 1 that was
// checked but has no watermark and was never marked initialized: it can't say
// whether the feed was empty when last checked (see legacyState.ambiguous).
func ambiguousLegacyStatus() *v1alpha1.FeedStatus {
	return &v1alpha1.FeedStatus{
		RSSUrl:      exampleFeedURL,
		LastChecked: syncTestLegacyChecked,
		LastSent:    map[string]string{},
	}
}

// failing marks fs as stuck on a failure that a settled sync would clear.
func failing(fs *v1alpha1.FeedStatus) *v1alpha1.FeedStatus {
	fs.LastError, fs.RetryCount, fs.BackoffUntil = testBoom, 3, "2031-02-03T04:05:06Z"
	return fs
}

func assertFailureCleared(t *testing.T, fs *v1alpha1.FeedStatus, want bool) {
	t.Helper()
	cleared := fs.LastError == "" && fs.RetryCount == 0 && fs.BackoffUntil == ""
	if cleared != want {
		t.Fatalf("failure state: LastError=%q RetryCount=%d BackoffUntil=%q, cleared=%t want %t",
			fs.LastError, fs.RetryCount, fs.BackoffUntil, cleared, want)
	}
}

func assertValidators(t *testing.T, fs *v1alpha1.FeedStatus, etag, lastModified string) {
	t.Helper()
	if fs.ETag != etag || fs.LastModified != lastModified {
		t.Fatalf("validators = %q / %q, want %q / %q", fs.ETag, fs.LastModified, etag, lastModified)
	}
}

func expectPanic(t *testing.T, what string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatalf("%s did not panic", what)
		}
	}()
	fn()
}

// An ambiguous legacy status under catchUpLimit 0 is capped at
// defaultCatchUpLimit in memory only. A first send that fails (close with
// syncPending) must leave it ambiguous, so the retry is capped the same way
// instead of delivering the whole backlog; the cap only settles once the fetch
// is accounted for.
func TestFeedSync_FailedFirstSendKeepsTheAmbiguousLegacyCap(t *testing.T) {
	entries := syncEntries(2 * defaultCatchUpLimit)
	want := syncIDs(entries[len(entries)-defaultCatchUpLimit:])
	fs := ambiguousLegacyStatus()

	first := openFeedSync(fs, syncFetch(entries...), syncSpec(0))
	first.begin()
	if !first.cappedAmbiguousLegacy {
		t.Fatal("an ambiguous legacy status under catchUpLimit 0 should be capped")
	}
	if got := syncIDs(first.entries()); !slices.Equal(got, want) {
		t.Fatalf("first attempt considers %q, want the capped %q", got, want)
	}
	first.close(syncPending) // the send failed

	if fs.Initialized || fs.StateVersion != 0 {
		t.Fatalf("after the failed send: initialized=%t stateVersion=%d, want a still-ambiguous status", fs.Initialized, fs.StateVersion)
	}
	assertValidators(t, fs, "", "")

	retry := openFeedSync(fs, syncFetch(entries...), syncSpec(0))
	retry.begin()
	if got := syncIDs(retry.entries()); !slices.Equal(got, want) {
		t.Fatalf("retry considers %q, want the same capped %q", got, want)
	}
	for _, entry := range retry.entries() {
		if retry.sent(entry) {
			t.Fatalf("%s reads as sent before being handled", entry.ID)
		}
		retry.markHandled(entry)
	}
	retry.close(syncAccountedFor)

	if !fs.Initialized || fs.StateVersion != currentStateVersion {
		t.Fatalf("after the retry: initialized=%t stateVersion=%d, want a settled status", fs.Initialized, fs.StateVersion)
	}
	assertValidators(t, fs, validatorsNext, syncTestModified)
	if fs.LastSeenEntry != entries[len(entries)-1].ID {
		t.Fatalf("LastSeenEntry = %q, want the newest entry", fs.LastSeenEntry)
	}
}

// A status that is not ambiguous is initialized and settled even when
// something is pending: it keeps no cap that a failed send could lose.
func TestFeedSync_PendingStillInitializesAStatusThatIsNotAmbiguous(t *testing.T) {
	fs := &v1alpha1.FeedStatus{RSSUrl: exampleFeedURL, LastSent: map[string]string{}} // never checked
	s := openFeedSync(fs, syncFetch(syncEntries(3)...), syncSpec(defaultCatchUpLimit))
	s.begin()
	s.close(syncPending)

	if !fs.Initialized || fs.StateVersion != currentStateVersion {
		t.Fatalf("initialized=%t stateVersion=%d, want both set for a never-checked status", fs.Initialized, fs.StateVersion)
	}
	assertValidators(t, fs, "", "")
}

// Validators from a document that left anything pending must not replace the
// stored ones -- they must survive, not be cleared -- and a failure must not be
// cleared either. Once the document is accounted for they are stored.
func TestFeedSync_ValidatorsAreOnlyPersistedWhenNothingIsPending(t *testing.T) {
	entries := syncEntries(3)
	fs := failing(currentStatus())

	pending := openFeedSync(fs, syncFetch(entries...), syncSpec(defaultCatchUpLimit))
	pending.begin()
	pending.markHandled(pending.entries()[0])
	pending.close(syncPending)
	assertValidators(t, fs, validatorsOld, syncTestOldMod)
	assertFailureCleared(t, fs, false)
	if !fs.Initialized {
		t.Fatal("an initialized status stays initialized")
	}

	done := openFeedSync(fs, syncFetch(entries...), syncSpec(defaultCatchUpLimit))
	done.begin()
	for _, entry := range done.entries() {
		if !done.sent(entry) {
			done.markHandled(entry)
		}
	}
	done.close(syncAccountedFor)
	assertValidators(t, fs, validatorsNext, syncTestModified)
	assertFailureCleared(t, fs, true)
}

func TestFeedSync_NotModified(t *testing.T) {
	t.Run("confirms and stores the validators", func(t *testing.T) {
		fs := failing(&v1alpha1.FeedStatus{RSSUrl: exampleFeedURL, LastSent: map[string]string{}})
		fetch := rss.FetchResult{NotModified: true, ETag: validatorsNext}
		s := openFeedSync(fs, fetch, syncSpec(defaultCatchUpLimit))
		verdict, ok := s.withoutEntries()
		if !ok || verdict != syncNotModified {
			t.Fatalf("withoutEntries = (%v, %t), want (syncNotModified, true)", verdict, ok)
		}
		s.close(verdict)
		if !fs.Initialized || fs.StateVersion != currentStateVersion {
			t.Fatalf("initialized=%t stateVersion=%d", fs.Initialized, fs.StateVersion)
		}
		assertValidators(t, fs, validatorsNext, "")
		assertFailureCleared(t, fs, true)
	})

	t.Run("settles nothing for an ambiguous legacy status", func(t *testing.T) {
		fs := ambiguousLegacyStatus()
		s := openFeedSync(fs, rss.FetchResult{NotModified: true, ETag: validatorsNext}, syncSpec(0))
		s.close(syncNotModified)
		if fs.Initialized || fs.StateVersion != 0 {
			t.Fatalf("initialized=%t stateVersion=%d, want the ambiguity kept", fs.Initialized, fs.StateVersion)
		}
		assertValidators(t, fs, validatorsNext, "")
	})
}

func TestFeedSync_Empty(t *testing.T) {
	t.Run("settles even an ambiguous legacy status", func(t *testing.T) {
		fs := failing(ambiguousLegacyStatus())
		s := openFeedSync(fs, rss.FetchResult{ETag: validatorsNext, LastModified: syncTestModified}, syncSpec(0))
		verdict, ok := s.withoutEntries()
		if !ok || verdict != syncEmpty {
			t.Fatalf("withoutEntries = (%v, %t), want (syncEmpty, true)", verdict, ok)
		}
		s.close(verdict)
		if !fs.Initialized || fs.StateVersion != currentStateVersion {
			t.Fatalf("initialized=%t stateVersion=%d, want an empty feed to settle it: anything after is new", fs.Initialized, fs.StateVersion)
		}
		assertValidators(t, fs, validatorsNext, syncTestModified)
		assertFailureCleared(t, fs, true)
	})

	t.Run("a fetch with entries is not one", func(t *testing.T) {
		s := openFeedSync(currentStatus(), syncFetch(syncEntries(1)...), syncSpec(defaultCatchUpLimit))
		if verdict, ok := s.withoutEntries(); ok {
			t.Fatalf("withoutEntries = %v, want false", verdict)
		}
	})
}

// A fetch failure or a configuration error accounts for nothing: no
// initialization, no validators, no cleared failure -- and a legacy status that
// still needs its identities migrated keeps its old stateVersion.
func TestFeedSync_FailedAndMisconfiguredFetchesSettleNothing(t *testing.T) {
	for _, verdict := range []syncVerdict{syncFetchFailed, syncConfigError} {
		t.Run(fmt.Sprint(verdict), func(t *testing.T) {
			fs := failing(&v1alpha1.FeedStatus{
				RSSUrl:        exampleFeedURL,
				LastChecked:   syncTestLegacyChecked,
				LastSeenEntry: "legacy-watermark",
				ETag:          validatorsOld,
				LastSent:      map[string]string{"legacy-key": legacySentAt},
			})
			before := *fs.DeepCopy()
			openFeedSync(fs, syncFetch(syncEntries(2)...), syncSpec(defaultCatchUpLimit)).close(verdict)

			if fs.Initialized || fs.StateVersion != 0 {
				t.Fatalf("initialized=%t stateVersion=%d", fs.Initialized, fs.StateVersion)
			}
			assertValidators(t, fs, validatorsOld, "")
			assertFailureCleared(t, fs, false)
			if fs.LastSeenEntry != before.LastSeenEntry || !slices.Equal(slices.Sorted(maps.Keys(fs.LastSent)), slices.Sorted(maps.Keys(before.LastSent))) {
				t.Fatalf("identities changed without a begin: %+v", fs)
			}
		})
	}
}

func TestFeedSync_BeginStampsTitleSortsAndPlansFromTheWatermark(t *testing.T) {
	entries := syncEntries(4)
	fs := currentStatus()
	fs.LastSeenEntry = entries[1].ID

	shuffled := []rss.Entry{entries[3], entries[0], entries[2], entries[1]}
	s := openFeedSync(fs, syncFetch(shuffled...), syncSpec(defaultCatchUpLimit))
	s.begin()

	if got, want := syncIDs(s.entries()), syncIDs(entries[2:]); !slices.Equal(got, want) {
		t.Fatalf("entries() = %q, want only those past the watermark: %q", got, want)
	}
	if got, want := syncIDs(s.window()), syncIDs(entries); !slices.Equal(got, want) {
		t.Fatalf("window() = %q, want the whole sorted fetch %q", got, want)
	}
	for _, entry := range s.window() {
		if entry.FeedTitle != syncTestTitle {
			t.Fatalf("entry %s has FeedTitle %q", entry.ID, entry.FeedTitle)
		}
	}
}

func TestFeedSync_CatchUp(t *testing.T) {
	all := syncEntries(8)
	for _, tc := range []struct {
		name      string
		watermark string
		limit     int
		want      []rss.Entry
	}{
		{name: "no watermark keeps the newest", limit: 3, want: all[5:]},
		{name: "a limit above the feed size keeps everything", limit: 20, want: all},
		{name: "a watermark that scrolled out falls back to the cap", watermark: scrolledOutWatermark, limit: 2, want: all[6:]},
		{name: "a watermark in the feed resumes after it, uncapped", watermark: all[1].ID, limit: 2, want: all[2:]},
		{name: "limit 0 on an initialized status delivers everything", limit: 0, want: all},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := currentStatus()
			fs.LastSeenEntry = tc.watermark
			s := openFeedSync(fs, syncFetch(all...), syncSpec(tc.limit))
			s.begin()
			if got, want := syncIDs(s.entries()), syncIDs(tc.want); !slices.Equal(got, want) {
				t.Fatalf("entries() = %q, want %q", got, want)
			}
			s.close(syncPending)
		})
	}
}

// With catchUpLimit 0 a first sync skips the backlog: the newest entry becomes
// the watermark, nothing is sent, and later syncs resume right behind it.
func TestFeedSync_BacklogSkipOnAFirstSync(t *testing.T) {
	all := syncEntries(6)
	fs := &v1alpha1.FeedStatus{RSSUrl: exampleFeedURL, LastSent: map[string]string{}}

	first := openFeedSync(fs, syncFetch(all[:4]...), syncSpec(0))
	first.begin()
	if len(first.entries()) != 0 {
		t.Fatalf("a skipped backlog leaves %q to send", syncIDs(first.entries()))
	}
	if fs.LastSeenEntry != all[3].ID || len(fs.LastSent) != 0 {
		t.Fatalf("LastSeenEntry=%q LastSent=%v, want the newest entry as watermark and nothing recorded as sent", fs.LastSeenEntry, fs.LastSent)
	}
	if first.skippedBacklog != 4 {
		t.Fatalf("skippedBacklog = %d, want the 4 existing entries", first.skippedBacklog)
	}
	first.close(syncAccountedFor)
	if !fs.Initialized {
		t.Fatal("a skipped backlog is accounted for")
	}

	second := openFeedSync(fs, syncFetch(all...), syncSpec(0))
	second.begin()
	if got, want := syncIDs(second.entries()), syncIDs(all[4:]); !slices.Equal(got, want) {
		t.Fatalf("second sync considers %q, want only the new %q", got, want)
	}
	if second.skippedBacklog != 0 {
		t.Fatalf("second sync skipped %d entries, want none", second.skippedBacklog)
	}
}

// The ledger counts how many entries share a stripped link identity over the
// whole fetch, before the catch-up trim: with a limit of 1 only the newer
// sibling is scanned, yet handling it must not make the older one read as sent.
func TestFeedSync_LedgerIsBuiltOverTheUntrimmedFetch(t *testing.T) {
	siblingA := linkEntry(legacyURLPrefix+"sync/post?ref=a", 0)
	siblingB := linkEntry(legacyURLPrefix+"sync/post?ref=b", 1)
	s := openFeedSync(currentStatus(), syncFetch(siblingA, siblingB), syncSpec(1))
	s.begin()

	if got, want := syncIDs(s.entries()), []string{siblingB.ID}; !slices.Equal(got, want) {
		t.Fatalf("entries() = %q, want only the trimmed tail %q", got, want)
	}
	s.markHandled(siblingB)
	if s.sent(siblingA) {
		t.Fatal("a sibling sharing the stripped identity was treated as sent")
	}
	if !s.sent(siblingB) {
		t.Fatal("the handled entry does not read as sent")
	}
}

// An entry whose identity earlier releases computed differently carries its
// watermark and dedup key over before the watermark lookup, so it neither
// resumes at the capped catch-up nor is sent again.
func TestFeedSync_PreviousIdentitiesAreMigratedBeforeThePlan(t *testing.T) {
	moved := changedID(prevIDNew, rss.IDFromGUID, prevIDOld, rss.IDFromGUID)
	fresh := guidEntry("sync-previous-fresh", 5)
	fs := currentStatus()
	fs.LastSeenEntry = prevIDOld
	fs.LastSent[hashIdentity(prevIDOld)] = prevSentAt

	s := openFeedSync(fs, syncFetch(fresh, moved), syncSpec(0))
	s.begin()

	if fs.LastSeenEntry != prevIDNew {
		t.Fatalf("LastSeenEntry = %q, want the new identity %q", fs.LastSeenEntry, prevIDNew)
	}
	if at, ok := fs.LastSent[hashIdentity(prevIDNew)]; !ok || at != prevSentAt {
		t.Fatalf("LastSent = %v, want the old send time under the new key", fs.LastSent)
	}
	if !s.sent(moved) {
		t.Fatal("the migrated entry reads as unsent")
	}
	if got, want := syncIDs(s.entries()), []string{fresh.ID}; !slices.Equal(got, want) {
		t.Fatalf("entries() = %q, want only the entry past the migrated watermark", got)
	}
}

// A legacy status's identities are translated as part of begin, and its
// stateVersion advances only when the fetch is accounted for as well.
func TestFeedSync_LegacyIdentitiesAreMigratedByBegin(t *testing.T) {
	link := linkEntry(legacyURLPrefix+"sync/legacy?ref=x", 0)
	fresh := linkEntry(legacyURLPrefix+"sync/fresh?ref=x", 1)
	fs := &v1alpha1.FeedStatus{
		RSSUrl:        exampleFeedURL,
		Initialized:   true,
		LastChecked:   syncTestLegacyChecked,
		LastSeenEntry: legacyEntryIdentity(link),
		LastSent:      map[string]string{legacyKey(link): legacySentAt},
	}

	s := openFeedSync(fs, syncFetch(link, fresh), syncSpec(defaultCatchUpLimit))
	s.begin()
	if fs.LastSeenEntry != entryIdentity(link) {
		t.Fatalf("LastSeenEntry = %q, want the current identity %q", fs.LastSeenEntry, entryIdentity(link))
	}
	if got, want := syncIDs(s.entries()), []string{fresh.ID}; !slices.Equal(got, want) {
		t.Fatalf("entries() = %q, want %q", got, want)
	}
	s.markHandled(fresh)
	s.close(syncAccountedFor)
	if fs.StateVersion != currentStateVersion {
		t.Fatalf("StateVersion = %d, want %d once migrated and accounted for", fs.StateVersion, currentStateVersion)
	}
}

func TestFeedSync_ClosePrunesLastSent(t *testing.T) {
	for _, verdict := range []syncVerdict{syncPending, syncAccountedFor} {
		t.Run(fmt.Sprint(verdict), func(t *testing.T) {
			fs := currentStatus()
			for i := range maxLastSentPerFeed + 5 {
				fs.LastSent[fmt.Sprintf("key-%03d", i)] = fmt.Sprintf("2026-01-01T00:00:%02dZ", i%60)
			}
			s := openFeedSync(fs, syncFetch(syncEntries(2)...), syncSpec(defaultCatchUpLimit))
			s.begin()
			s.close(verdict)
			if len(fs.LastSent) != maxLastSentPerFeed {
				t.Fatalf("LastSent has %d keys, want %d", len(fs.LastSent), maxLastSentPerFeed)
			}
		})
	}
}

// The ordering and settlement rules hold only if every sync is closed exactly
// once with a verdict that fits how far it got.
func TestFeedSync_MisuseIsLoud(t *testing.T) {
	open := func() *feedSync {
		return openFeedSync(currentStatus(), syncFetch(syncEntries(2)...), syncSpec(defaultCatchUpLimit))
	}
	expectPanic(t, "closing twice", func() {
		s := open()
		s.close(syncFetchFailed)
		s.close(syncFetchFailed)
	})
	expectPanic(t, "closing without a verdict", func() { open().close(0) })
	expectPanic(t, "closing with an unknown verdict", func() { open().close(syncAccountedFor + 1) })
	expectPanic(t, "closing a sync that never began as pending", func() { open().close(syncPending) })
	expectPanic(t, "closing a sync that never began as accounted for", func() { open().close(syncAccountedFor) })
	expectPanic(t, "beginning twice", func() {
		s := open()
		s.begin()
		s.begin()
	})
	expectPanic(t, "returning without a verdict", func() {
		s := open()
		var verdict syncVerdict
		defer s.closeOnReturn(&verdict)
	})
}

// processFeed defers closeOnReturn. A panic raised before any verdict was set
// must surface unchanged, not be replaced by close's "invalid verdict" panic,
// so controller-runtime logs the real fault; a return with a verdict still
// closes the sync.
func TestFeedSync_CloseOnReturnKeepsTheOriginalPanic(t *testing.T) {
	open := func() *feedSync {
		return openFeedSync(currentStatus(), syncFetch(syncEntries(2)...), syncSpec(defaultCatchUpLimit))
	}

	const original = "render exploded"
	s := open()
	got := func() (recovered any) {
		defer func() { recovered = recover() }()
		var verdict syncVerdict
		defer s.closeOnReturn(&verdict)
		panic(original)
	}()
	if got != original {
		t.Fatalf("recovered %v, want the original panic %q", got, original)
	}
	if s.closed {
		t.Fatal("a sync that panicked before a verdict was set should not be closed")
	}

	s = open()
	func() {
		verdict := syncFetchFailed
		defer s.closeOnReturn(&verdict)
	}()
	if !s.closed {
		t.Fatal("a return with a verdict should close the sync")
	}
}

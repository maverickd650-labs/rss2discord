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
	"fmt"
	"slices"

	"github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/rss"
)

// defaultCatchUpLimit is the cap used when a watermark has scrolled out of a
// feed's window, and by limitCatchUp for any non-positive limit. On a true
// first sync an explicit catchUpLimit of 0 means "skip the backlog" instead
// (see feedSync.skipBacklog); an omitted field is defaulted to 5 by the API
// server.
const defaultCatchUpLimit = 5

// A feedSync applies one fetch of one feed to that feed's FeedStatus. It is the
// single owner of two things that used to be spread over every return path of
// processFeed:
//
//   - the ORDER in which a fetch is applied: legacy state is read before
//     anything changes; entries are stamped and sorted; both identity
//     migrations run; only then is the delivery ledger built, and it is built
//     over the whole fetch, before the catch-up trim, so an entry's stripped
//     identity is only unique if it is across the feed; the watermark lookup
//     and planDelivery follow, then the catch-up limit and the backlog skip.
//   - SETTLEMENT: whether the fetch is accounted for, which is the only thing
//     that decides FeedStatus.Initialized, the legacy stateVersion, whether the
//     response's validators are stored, whether a failure is cleared, and
//     LastSent pruning (see close).
//
// A sync is opened with openFeedSync, optionally begun with begin (only a fetch
// that has entries to send is), and closed exactly once with the syncVerdict
// that says how the attempt ended. Closing twice, or closing with a verdict
// that doesn't fit how far the sync got, panics: a settlement rule that is
// applied inconsistently silently loses entries (see the settlement tests).
type feedSync struct {
	fs    *v1alpha1.FeedStatus
	fetch rss.FetchResult
	spec  feedSyncSpec

	// legacy is what fs, as it was before this sync, still needs from the
	// legacy reading; see legacyState.
	legacy legacyState
	// wasInitialized is fs.Initialized before this sync.
	wasInitialized bool

	began  bool
	closed bool
	ledger *deliveryLedger
	// scan is the entries the send loop may consider, in order, starting at
	// resumeAt; set by begin.
	scan     []rss.Entry
	resumeAt int
	// cappedAmbiguousLegacy says the catch-up was capped at
	// defaultCatchUpLimit because fs is an ambiguous legacy status; set by
	// begin.
	cappedAmbiguousLegacy bool
	// skippedBacklog is how many existing entries the begin step skipped over, for a
	// first sync with catchUpLimit 0 (see skipBacklog); 0 otherwise. The
	// caller records the BacklogSkipped outcome.
	skippedBacklog int
}

// feedSyncSpec is what a sync needs from the FeedGroup and the reconcile.
type feedSyncSpec struct {
	// catchUpLimit is the effective spec limit (defaultCatchUpLimit when unset).
	catchUpLimit int
	// now is the RFC3339 timestamp recorded for entries handled in this sync.
	now string
}

// feedSyncSpecFor derives the spec of a sync of one of group's feeds.
func feedSyncSpecFor(group *v1alpha1.FeedGroup, now string) feedSyncSpec {
	limit := defaultCatchUpLimit
	if group.Spec.CatchUpLimit != nil {
		limit = int(*group.Spec.CatchUpLimit)
	}
	return feedSyncSpec{catchUpLimit: limit, now: now}
}

// syncVerdict says how the processing of a fetch ended, and so what close
// settles. The zero value is deliberately not a verdict.
type syncVerdict int

const (
	// syncFetchFailed: the fetch failed, so there is no document to account for.
	syncFetchFailed syncVerdict = iota + 1
	// syncNotModified: a 304 confirmed the stored validators are current.
	syncNotModified
	// syncEmpty: the document has no entries.
	syncEmpty
	// syncConfigError: the feed's filter or templates don't compile; nothing
	// from the document was looked at.
	syncConfigError
	// syncPending: some entry from the document is still unresolved -- failed
	// to render or send, or Discord rate limited the webhook -- whether or not
	// its retries are exhausted.
	syncPending
	// syncAccountedFor: every entry from the document was sent, filtered out,
	// already sent, or skipped.
	syncAccountedFor
)

func (v syncVerdict) String() string {
	switch v {
	case syncFetchFailed:
		return "fetchFailed"
	case syncNotModified:
		return "notModified"
	case syncEmpty:
		return "empty"
	case syncConfigError:
		return "configError"
	case syncPending:
		return "pending"
	case syncAccountedFor:
		return "accountedFor"
	}
	return fmt.Sprintf("syncVerdict(%d)", int(v))
}

// openFeedSync opens the sync of fetch into fs. It only reads fs: nothing
// changes until begin or close.
func openFeedSync(fs *v1alpha1.FeedStatus, fetch rss.FetchResult, spec feedSyncSpec) *feedSync {
	return &feedSync{
		fs:             fs,
		fetch:          fetch,
		spec:           spec,
		legacy:         readLegacyState(fs),
		wasInitialized: fs.Initialized,
	}
}

// withoutEntries reports the verdict of a fetch that has nothing to send,
// because it was a 304 or the document is empty. It returns false for a fetch
// with entries.
func (s *feedSync) withoutEntries() (syncVerdict, bool) {
	switch {
	case s.fetch.NotModified:
		return syncNotModified, true
	case len(s.fetch.Entries) == 0:
		return syncEmpty, true
	}
	return 0, false
}

// begin applies the fetch's entries to fs up to the point of sending: it
// stamps the feed's title on each entry, sorts them, runs both identity
// migrations, builds the ledger over the whole fetch, plans the delivery
// (catch-up limit included) and, for a first sync with catchUpLimit 0, skips the
// backlog. It must be called at most once, and only after the feed's
// configuration is known to be usable: a configuration error must leave fs as it
// was (see syncConfigError), so that fixing it replays the same first sync
// instead of delivering the whole backlog.
func (s *feedSync) begin() {
	if s.began {
		panic("feedSync: begin called twice")
	}
	s.began = true

	entries := s.fetch.Entries
	// Stamp the feed's title on each entry so templates can use {{.FeedTitle}}
	// without threading it through entryDelivery/feedRenderer.build.
	for i := range entries {
		entries[i].FeedTitle = s.fetch.FeedTitle
	}

	// Sort oldest-to-newest so the catch-up window (limitCatchUp keeps the
	// tail) and the watermark (markHandled advances LastSeenEntry to the last
	// entry it records) both track the *newest* entries. Published is the
	// primary key, but many feeds omit per-entry dates, leaving every entry
	// with a zero timestamp; in that case fall back to document order, where a
	// lower Seq is newer (feeds list newest-first). Tie-break so a lower Seq
	// sorts later (toward the newest tail) -- without this, date-less feeds
	// would catch-up on their oldest entries and park the watermark there,
	// then never deliver anything newer.
	slices.SortStableFunc(entries, func(a, b rss.Entry) int {
		if c := a.Published.Compare(b.Published); c != 0 {
			return c
		}
		return cmp.Compare(b.Seq, a.Seq)
	})

	migrateIdentities(s.fs, entries, &s.legacy, s.spec.catchUpLimit)

	// Built over the whole fetch, before any catch-up trim below, so an
	// entry's stripped identity is only unique if it is across the feed.
	s.ledger = newDeliveryLedger(s.fs, entries)
	plan := planDelivery(deliveryInputs{
		entries:         entries,
		watermark:       s.ledger.resume(entries),
		catchUpLimit:    s.spec.catchUpLimit,
		legacyAmbiguous: s.legacy.ambiguous,
		wasInitialized:  s.wasInitialized,
	})
	s.cappedAmbiguousLegacy = plan.cappedAmbiguousLegacy
	if plan.skipBacklog {
		s.skipBacklog(entries)
	}
	s.scan, s.resumeAt = plan.scan, plan.resumeAt
}

// entries returns the entries the send loop should consider, oldest-to-newest:
// the plan's scan from just past the watermark. It is only valid after begin.
func (s *feedSync) entries() []rss.Entry {
	return s.scan[s.resumeAt:]
}

// window returns every entry of the plan's scan, including those before the
// watermark that entries leaves out. It is only valid after begin.
func (s *feedSync) window() []rss.Entry {
	return s.scan
}

// sent reports whether entry was already delivered; see deliveryLedger.sent.
func (s *feedSync) sent(entry rss.Entry) bool {
	return s.ledger.sent(entry)
}

// markHandled records entry as delivered (or given up on) at the sync's time
// and moves the watermark to it.
func (s *feedSync) markHandled(entry rss.Entry) {
	s.ledger.markHandled(entry, s.spec.now)
}

// skipBacklog records the newest of entries (sorted oldest-to-newest) as the
// feed's watermark without sending anything. entries must be non-empty; later
// scans resume after this entry rather than comparing publication times. The
// identity is the same one markHandled would have stored after sending that
// entry, so the next forward-scan resumes right behind it.
func (s *feedSync) skipBacklog(entries []rss.Entry) {
	s.ledger.markNewest(entries)
	s.skippedBacklog = len(entries)
}

// limitCatchUp trims entries to at most limit, keeping the most recent ones
// (entries is assumed sorted ascending by Published). A non-positive limit
// falls back to defaultCatchUpLimit rather than disabling the cap, so a feed
// can never dump its entire backlog on first reconcile. (An explicit
// catchUpLimit of 0 is handled earlier, by skipBacklog, on a true first
// sync only.)
func limitCatchUp(entries []rss.Entry, limit int) []rss.Entry {
	if limit <= 0 {
		limit = defaultCatchUpLimit
	}
	if len(entries) <= limit {
		return entries
	}
	return entries[len(entries)-limit:]
}

// close settles what the verdict allows and ends the sync; it is the only place
// that decides these, and it must run exactly once.
//
//   - Initialized: set once the feed's configuration is known to be usable and,
//     for an ambiguous legacy status, only once the document is accounted for.
//     Marking it earlier would let a config error on a non-empty first fetch
//     leave Initialized true with no watermark, so a fixed config would deliver
//     the whole backlog despite catchUpLimit 0.
//   - Legacy settlement: an ambiguous legacy status is capped in memory only
//     (see planDelivery), so it is settled once the fetch is accounted for, not
//     before: settling it earlier would let a failed first send leave a
//     current, watermark-less status that no longer gets the cap, and the next
//     attempt would deliver the whole backlog. A 304 repeats a fetch an
//     ambiguous legacy status can't account for, so it settles nothing.
//   - Validators: stored only when nothing is pending. If a send fails, storing
//     the new ETag would make the next conditional GET return 304 before the
//     unsent entry is retried, silently dropping it. pending (not "should
//     retry") is the right gate: retrying stops once an entry's retries are
//     exhausted or it enters permanent backoff, though the entry is still unsent.
//   - Failure state (LastError, RetryCount, BackoffUntil): cleared when the
//     document is accounted for, so a feed that recovered but happens to have
//     nothing new to send doesn't keep reading as failing.
//   - LastSent: pruned after a sync that sent or looked at entries.
//
// The legacy stateVersion is advanced last, and only when nothing in fs still
// needs the legacy reading (see legacyState.finish).
func (s *feedSync) close(verdict syncVerdict) {
	if s.closed {
		panic("feedSync: closed twice")
	}
	s.closed = true

	switch verdict {
	case syncFetchFailed, syncConfigError:
		// Nothing was accounted for.
	case syncNotModified:
		// A 304 confirms the feed has no new entries, just as much a
		// successful check as one that found something to send.
		if !s.legacy.ambiguous {
			s.fs.Initialized = true
		}
		s.persistValidators()
		clearFailure(s.fs)
	case syncEmpty:
		// Nothing to send, so it's always safe to persist. An empty feed
		// also settles an ambiguous legacy status: anything after it is new.
		s.settle()
		s.persistValidators()
		clearFailure(s.fs)
	case syncPending:
		s.mustHaveBegun(verdict)
		if !s.legacy.ambiguous {
			s.settle()
		}
		s.prune()
	case syncAccountedFor:
		s.mustHaveBegun(verdict)
		s.settle()
		s.persistValidators()
		clearFailure(s.fs)
		s.prune()
	default:
		panic(fmt.Sprintf("feedSync: closed with invalid verdict %v", verdict))
	}

	s.legacy.finish(s.fs)
}

// closeOnReturn is processFeed's deferred close and must be deferred directly,
// since recover only works in the deferred call itself. A return that set no
// verdict still panics in close, so a forgotten verdict stays loud. A panic
// raised before any verdict was set (while rendering, sending or recording) is
// re-raised unchanged and close is skipped; otherwise close's own "invalid
// verdict" panic would replace it, and controller-runtime would log a missing
// verdict instead of the real fault. Status isn't written on a panic path, so
// skipping settlement there loses nothing.
func (s *feedSync) closeOnReturn(verdict *syncVerdict) {
	if *verdict == 0 {
		if p := recover(); p != nil {
			panic(p)
		}
	}
	s.close(*verdict)
}

// settle marks fs initialized and its ambiguous legacy reading resolved.
func (s *feedSync) settle() {
	s.fs.Initialized = true
	s.legacy.settled = true
}

// persistValidators stores the response's cache validators.
func (s *feedSync) persistValidators() {
	if s.fetch.ETag != "" {
		s.fs.ETag = s.fetch.ETag
	}
	if s.fetch.LastModified != "" {
		s.fs.LastModified = s.fetch.LastModified
	}
}

func (s *feedSync) prune() {
	pruneLastSent(s.fs.LastSent, maxLastSentPerFeed)
}

func (s *feedSync) mustHaveBegun(verdict syncVerdict) {
	if !s.began {
		panic(fmt.Sprintf("feedSync: closed with verdict %v before begin", verdict))
	}
}

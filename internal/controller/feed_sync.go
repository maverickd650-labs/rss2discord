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
	"context"
	"fmt"
	"slices"
	"time"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

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
// single owner of two things:
//
//   - the ORDER in which a fetch is applied: legacy state is read before
//     anything changes; entries are stamped and sorted; the delivery ledger is
//     built once, over the whole sorted fetch and before the catch-up trim, so
//     an entry's stripped identity is only unique if it is across the feed;
//     both identity migrations run through it; the watermark lookup and
//     planDelivery follow, then the catch-up limit and the backlog skip.
//   - SETTLEMENT: whether the fetch is accounted for, which is the only thing
//     that decides FeedStatus.Initialized, the legacy stateVersion, whether the
//     response's validators are stored, whether a failure is cleared, and
//     LastSent pruning (see close).
//
// A sync is opened with openFeedSync and run by apply, which is the whole
// sequence -- record the fetch, check the configuration, begin, send -- and
// derives the syncVerdict from how far that got, then closes the sync with it as
// its last act: exactly once, and not deferred, so a panic anywhere in the
// sequence leaves the status unsettled (the status isn't written on a panic path,
// so skipping settlement there loses nothing, and the real fault is what
// controller-runtime logs). Nothing outside apply begins or closes a sync, so
// the verdict can't be forgotten, doubled or mismatched; close's settlement
// table is unit-tested per verdict (see the settlement tests).
type feedSync struct {
	fs    *v1alpha1.FeedStatus
	fetch rss.FetchResult
	spec  feedSyncSpec

	// legacy is what fs, as it was before this sync, still needs from the
	// legacy reading; see legacyState.
	legacy legacyState
	// wasInitialized is fs.Initialized before this sync.
	wasInitialized bool

	ledger *deliveryLedger
	// scan is the plan's whole window, in order: the entries the send loop may
	// consider start at resumeAt, and the ones before it are kept as render
	// evidence (see renderEvidence); set by begin.
	scan     []rss.Entry
	resumeAt int
	// cappedAmbiguousLegacy says the catch-up was capped at
	// defaultCatchUpLimit because fs is an ambiguous legacy status; set by
	// begin, and logged by apply.
	cappedAmbiguousLegacy bool
	// skippedBacklog is how many existing entries the begin step skipped over, for a
	// first sync with catchUpLimit 0 (see skipBacklog); 0 otherwise. apply
	// records the BacklogSkipped outcome.
	skippedBacklog int
}

// feedSyncSpec is what a sync needs from the FeedGroup and the reconcile.
type feedSyncSpec struct {
	// catchUpLimit is the effective spec limit (defaultCatchUpLimit when unset).
	catchUpLimit int
	// now is the reconcile's clock reading; entries handled in this sync are
	// recorded at its RFC3339 form.
	now time.Time
}

// feedSyncSpecFor derives the spec of a sync of one of group's feeds.
func feedSyncSpecFor(group *v1alpha1.FeedGroup, now time.Time) feedSyncSpec {
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

// apply runs the whole sync of run's fetch into fs and closes it, returning the
// verdict the reconcile folds. attempt is the feed's feedAttempt (run.newAttempt),
// through which every outcome is recorded; the sequence and what ends it:
//
//   - a failed fetch is recorded and the sync closes syncFetchFailed;
//   - a successful fetch is recorded (a 304 is as much a successful check as one
//     that found something to send), and one with nothing to send closes with
//     its withoutEntries verdict;
//   - the feed's render configuration is checked before the sync begins, so a
//     config error on a non-empty first fetch leaves fs untouched and can't turn
//     the next attempt into a "feed was empty" delivery of its whole backlog
//     (syncConfigError);
//   - begin, the capped-legacy log line, the backlog-skip outcome, then the
//     entryDelivery send loop, whose attemptVerdict decides syncPending or
//     syncAccountedFor.
//
// close is the last thing apply does and is not deferred: a panic while
// rendering, sending or recording propagates unchanged and settles nothing.
func (s *feedSync) apply(ctx context.Context, run feedRun, attempt feedAttempt) attemptVerdict {
	log := logf.FromContext(ctx)
	feed := run.feed

	if err := run.fetched.err; err != nil {
		// LastChecked deliberately doesn't advance here: it tracks the last
		// *successful* check, so a feed stuck failing shows an aging
		// LastChecked rather than looking freshly checked every retry.
		log.Error(err, "failed to fetch RSS feed", "url", feed.RSSUrl)
		verdict := attempt.record(attemptOutcome{kind: attemptFetchFailed}, err)
		s.close(syncFetchFailed)
		return verdict
	}

	// A 304 is just as much a successful check as one that found something to
	// send -- it shouldn't read as stale just because nothing changed. This does
	// give up the optimization of skipping the status write when nothing
	// else changed (settleGroup diffs on the whole status), so a
	// quiet feed now costs one status write per poll interval instead of
	// zero.
	attempt.record(attemptOutcome{kind: attemptFetchSucceeded}, nil)

	if v, done := s.withoutEntries(); done {
		s.close(v)
		return attemptVerdict{}
	}

	// Every config check happens in one step, before the sync begins, so a
	// config error on a non-empty first fetch can't turn the next attempt into
	// a "feed was empty" delivery of its whole backlog.
	renderer, err := newFeedRenderer(run.group, &feed)
	if err != nil {
		log.Error(err, "invalid feed render configuration", "url", feed.RSSUrl)
		verdict := attempt.record(attemptOutcome{kind: attemptConfigError}, err)
		s.close(syncConfigError)
		return verdict
	}

	s.begin()
	if s.cappedAmbiguousLegacy {
		log.Info("Capped catch-up for a FeedStatus written before stateVersion 1",
			"url", feed.RSSUrl, "limit", defaultCatchUpLimit)
	}
	if s.skippedBacklog > 0 {
		attempt.record(attemptOutcome{kind: attemptBacklogSkipped, count: s.skippedBacklog}, nil)
	}

	verdict := entryDelivery{
		renderer: renderer,
		syncer:   s,
		attempt:  attempt,
		sender:   run.sender,
		metrics:  metricsFor(run.group),
	}.run(ctx)
	s.close(verdict.settlement())
	return verdict
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
// stamps the feed's title on each entry, sorts them, builds the ledger over the
// whole fetch, runs both identity migrations through it, plans the delivery
// (catch-up limit included) and, for a first sync with catchUpLimit 0, skips the
// backlog. apply calls it once, and only after the feed's configuration is
// known to be usable: a configuration error must leave fs as it was (see
// syncConfigError), so that fixing it replays the same first sync instead of
// delivering the whole backlog.
func (s *feedSync) begin() {
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

	// The one identity index of this fetch: built over the whole sorted fetch,
	// before the migrations (which only change fs) and any catch-up trim below,
	// so an entry's stripped identity is only unique if it is across the feed.
	s.ledger = newDeliveryLedger(s.fs, entries)
	migrateIdentities(s.ledger, &s.legacy, s.spec.catchUpLimit)
	plan := planDelivery(deliveryInputs{
		entries:        entries,
		watermark:      s.ledger.resume(),
		catchUpLimit:   s.spec.catchUpLimit,
		legacy:         s.legacy,
		wasInitialized: s.wasInitialized,
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

// renderEvidence is the entries whose rendering counts as evidence that the
// feed's templates work, for entryDelivery.otherEntryRenders: the plan's whole
// scan, untrimmed past-watermark entries included (before the watermark and
// already sent ones passed the filter when they were delivered), but not the
// ones the catch-up trim left out. It is only valid after begin.
func (s *feedSync) renderEvidence() []rss.Entry {
	return s.scan
}

// sent reports whether entry was already delivered; see deliveryLedger.sent.
func (s *feedSync) sent(entry rss.Entry) bool {
	return s.ledger.sent(entry)
}

// markHandled records entry as delivered (or given up on) at the sync's time
// and moves the watermark to it.
func (s *feedSync) markHandled(entry rss.Entry) {
	s.ledger.markHandled(entry, s.spec.now.Format(time.RFC3339))
}

// skipBacklog records the newest of entries (sorted oldest-to-newest) as the
// feed's watermark without sending anything. entries must be non-empty; later
// scans resume after this entry rather than comparing publication times. The
// identity is the same one markHandled would have stored after sending that
// entry, so the next forward-scan resumes right behind it.
func (s *feedSync) skipBacklog(entries []rss.Entry) {
	s.ledger.markNewest()
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
// that decides these, and apply runs it exactly once, last.
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
// needs the legacy reading (see legacyState.settleOnClose).
func (s *feedSync) close(verdict syncVerdict) {
	switch verdict {
	case syncFetchFailed, syncConfigError:
		// Nothing was accounted for.
	case syncNotModified:
		// A 304 confirms the feed has no new entries, just as much a
		// successful check as one that found something to send.
		if !s.legacy.blocksInitialize() {
			s.fs.Initialized = true
		}
		s.persistValidators()
		clearFailure(s.fs)
	case syncEmpty:
		// Nothing to send, so it's always safe to persist. An empty feed
		// also settles an ambiguous legacy status: anything after it is new.
		s.fs.Initialized = true
		s.persistValidators()
		clearFailure(s.fs)
	case syncPending:
		if !s.legacy.blocksInitialize() {
			s.fs.Initialized = true
		}
		s.prune()
	case syncAccountedFor:
		s.fs.Initialized = true
		s.persistValidators()
		clearFailure(s.fs)
		s.prune()
	default:
		panic(fmt.Sprintf("feedSync: closed with invalid verdict %v", verdict))
	}

	s.legacy.settleOnClose(s.fs, verdict)
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

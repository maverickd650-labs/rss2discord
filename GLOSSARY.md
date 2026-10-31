# Glossary

Terms used in `internal/controller` (and `internal/msgtemplate`), with the names the code gives them.

**Feed sync** (`feedSync`, `feed_sync.go`)
: The application of one fetch of one feed to that feed's `FeedStatus`. It is opened
  with `openFeedSync`, begun with `begin` once the feed's filter and templates are known to
  compile, and closed exactly once with a `syncVerdict`. It owns the order in which a
  fetch is applied (title stamping, sort, delivery ledger over the untrimmed fetch,
  identity migrations, watermark lookup, delivery plan, catch-up limit, backlog skip) and is
  the only place that settles the status. It hands the send loop the entries to consider
  (`entries`) and two ledger operations (`sent`, `markHandled`).

**FeedStatus settlement** (`feedSync.close`)
: Deciding, from how a sync ended, what the status may now claim: `Initialized`, the
  legacy `StateVersion`, the stored ETag/Last-Modified validators, a cleared failure
  (`LastError`, `RetryCount`, the feed backoff) and `LastSent` pruning. A fetch is
  *accounted for* when every entry from it was sent, filtered out, already sent or
  skipped; only then are validators stored, because a conditional GET that comes back 304
  would otherwise strand an unsent entry.

**Sync verdict** (`syncVerdict`)
: How a sync ended: `syncFetchFailed`, `syncNotModified`, `syncEmpty`, `syncConfigError`,
  `syncPending` (some entry is still unresolved, even if its retries are exhausted) or
  `syncAccountedFor`.

**Catch-up** (`catchUpLimit`, `limitCatchUp`, `defaultCatchUpLimit`)
: How much of a feed's existing entries are delivered when there is no usable watermark:
  the newest `catchUpLimit` of them, falling back to `defaultCatchUpLimit` (5) for a
  non-positive limit. An explicit limit of 0 on a true first sync skips the backlog
  instead (`BacklogSkipped`): the newest entry becomes the watermark and nothing is sent.

**Delivery ledger** (`deliveryLedger`, `ledger.go`)
: A feed's delivery bookkeeping over one fetch, and the single identity index of it: the
  `LastSeenEntry` watermark and the `LastSent` dedup keys, the whole sorted fetch, and the
  stripped-identity counts they depend on. `feedSync.begin` builds it once per fetch, after the
  sort and before the identity migrations (which change only the status, so the counts are the
  same on either side), over the whole untrimmed fetch, so an entry's stripped identity is only
  unique if it is across the feed, however the entries are later trimmed. It alone writes the
  watermark and `LastSent` (apart from status normalization and pruning) and owns the key scheme
  -- an entry's own key (`computeEntryKey`) plus, when its stripped link identity is unique in
  the fetch, the stripped key (`strippedKey`). The migrations go through its methods
  (`moveWatermark`, `setWatermark`, `setSent`, `forgetSent`, `rekeySent`) rather than building
  keys or clamping watermarks themselves.

**Identity migration** (`migrateIdentities`, `identity_migration.go`)
: Bringing a feed's stored identities (the watermark and the `LastSent` keys) in line with how
  entries are identified now, on every fetch, from `feedSync.begin`. It is the one entry point
  for two passes, run in this order: the legacy pass (`migrateLegacyIdentities`, in `legacy.go`, only for a
  status written before `stateVersion` 1) and the previous-identity pass
  (`migratePreviousIdentities`, for entries whose `rss.Entry.PreviousID` differs from their ID).
  Both take the fetch's delivery ledger, read its entries, and write only through it. Before
  writing a stripped key they ask the ledger's `ownStripped` (is this stripped identity the
  entry's own and unique in the fetch?) and `ownsPrevious` (can an old stripped key only have
  been this one moved entry's?). A stripped key a sibling shares is never recorded, or the
  sibling, alone under it later, would be taken as sent.

**Watermark** (`FeedStatus.LastSeenEntry`)
: The identity of the last entry handled; a later scan resumes just past it.

**Watermark lookup** (`watermarkLookup`, `deliveryLedger.resume`, `ledger.go`)
: Where a feed's watermark stands against the ledger's fetch, as one value: `watermarkNone` (no
  watermark stored), `watermarkFound` (it stands for the entry at `index`, matched by
  identity or by unique stripped identity) or `watermarkLost` (stored, but no entry of the
  fetch it stands for). `planDelivery` decides from it: found resumes past the entry; lost
  falls through to the catch-up cap, unlike none, which may skip the backlog.

**Ambiguous legacy status** (`legacyState.ambiguous`, `legacy.go`)
: A `FeedStatus` written before `stateVersion` 1, checked but with no watermark, that can't
  say whether the feed was empty when last checked. Under `catchUpLimit: 0` it is capped
  at `defaultCatchUpLimit` in memory only, so it stays ambiguous until a sync accounts for
  the fetch.

**Reconcile pass** (`reconcilePass`, `feedgroup_controller.go`)
: The pure middle of a reconcile, once the webhook is resolved: plan the due feeds, fetch them
  concurrently and fold each through `processFeed`, stopping after a Discord rate limit. It does
  no Kubernetes I/O and returns the folded `attemptVerdict` and the `feedPlan`. `prepareStatus`
  is its counterpart before it (normalize status, reset backoffs on a spec change, snapshot);
  `settleGroup` is the write after it.

**Group cadence** (`cadence`, `cadence.go`)
: A FeedGroup's timing resolved once from its spec by `cadenceOf`: `poll` (`Spec.Interval`),
  `retry` (`Spec.RetryInterval`, also the permanent-failure backoff base) and `maxRetries`
  (`Spec.Retries`, at least 1). Parsed durations are floored at `minParsedInterval`; blank
  or unparsable ones take `fallbackInterval`/`fallbackRetryInterval`. `err` carries the
  first unparsable field (interval before retryInterval) for the Ready condition's
  `InvalidInterval`. `planFeeds`, `requeueDecision`, `feedAttempt` and `summarizeGroup` all
  read it; none parses those spec fields itself.

**Feed run** (`feedRun`, `feedgroup_controller.go`)
: Everything one feed's handling in one reconcile needs, as one value: the group, the feed's
  spec, its `fetchOutcome`, the `MessageSender`, the group's `cadence`, the Event recorder (nil
  drops Events), the reconcile's timestamp (for `LastChecked` and handled entries) and a clock read when an outcome is recorded (the permanent-failure backoff deadline and the success gauge; nil is `time.Now`). `newAttempt` opens the `feedAttempt` from it.

**Feed attempt** (`feedAttempt`, `attempt.go`)
: The outcomes of processing one feed in one reconcile -- a fetch succeeding or failing,
  an entry failing to render, failing to send, being rate limited, sent or skipped, a
  skipped backlog, a config error -- each applied through the single entry point
  `record(outcome, err)`, which classifies the error itself (a fetch failure by
  `classifyFetchError`, a send failure by `classifySendError`, a `*discord.RateLimitError`
  becoming the rate-limited outcome), so callers only report what went wrong. It owns the outcome metric label, the `Reachable` /
  `Delivered` condition, `LastError`, `RetryCount`, permanent backoff, the once-only
  persistent-failure Events and the retry decision.

**Retry run** (`RetryCount`, `BackoffUntil`)
: The consecutive failures of one phase of a feed's sync -- fetch (`Reachable` False) or
  delivery, meaning render and send (`Reachable` True, `Delivered` False). A run belongs to
  its phase: a successful fetch ends a fetch-phase run (`RetryCount` and the backoff reset)
  but not a delivery run. The rule is written once, in `feedAttempt.record`.

**Entry delivery** (`entryDelivery`, `delivery.go`)
: The per-entry send loop of one feed's sync. `run` scans the entries the feed sync hands it,
  skipping those already sent or filtered out, rendering and sending the rest, and recording
  each outcome with the feed attempt. It owns when the scan ends -- the first render, send or
  rate-limit failure stops it, so the watermark never moves past an unresolved entry -- and
  the render-failure policy (`renderFailed`): an entry that still fails to render after its
  retries are exhausted is skipped (marked handled, recorded as skipped) only if another entry
  of the window renders (`entryDelivery.otherEntryRenders`, which owns that scan: the window counts
  entries before the watermark, already sent and filtered-out ones, but not those the catch-up
  trim left out), otherwise the scan holds. It is the only caller of
  `groupMetrics.overflow`. Its result is an attempt verdict.

**Attempt verdict** (`attemptVerdict`)
: What recording an outcome means for the rest of the reconcile: `retry` (come back after
  `RetryInterval`), `retryAfter` (how long Discord asked the webhook to be left alone) and
  `pending` (an entry is still unresolved, even if its retries are exhausted). `fold`
  combines verdicts across entries and feeds, and `settlement` turns the send loop's
  verdict into the feed sync's close verdict: `pending` closes it `syncPending`,
  otherwise `syncAccountedFor`.

**Permanent failure** (`failureClass.permanent`)
: A failure retrying on the normal schedule won't fix (a 404, bad XML, a deleted webhook).
  It backs off exponentially from its first occurrence, capped at 6h, and never asks for a
  faster retry; one Event announces the first time the backoff reaches the cap. A
  transient failure instead retries until `RetryCount` reaches the retry limit, and one
  Event announces that.

**Feed backoff** (`feedBackoff`, `backoff.go`)
: A feed's permanent-failure backoff, as a view over its `FeedStatus` (`backoffOf(fs)`) and the
  only code that touches `BackoffUntil`. `extend(base, now)` sets it to `base * 2^RetryCount`
  capped at `maxPermanentBackoff` (6h) and reports `firstCapped`, true only for the failure
  whose backoff first reaches the cap (the caller fires the `BackoffCapped` Event once);
  `active(now)` is what `planFeeds` skips on (a malformed value is not a backoff);
  `clear()` ends it and zeroes `RetryCount` so the next backoff starts from the bottom; and
  `sendCaused()` says the backoff came from a send failure (so a changed webhook Secret
  clears it), inferred from the feed's `Reachable` condition not being False.

**Display entry** (`displayEntry`, `displayOf`, `display.go`)
: An `rss.Entry` as Discord shows it: title, description, feed title, link, image,
  published (RFC 3339 and Unix), author and categories, built once per entry by `displayOf`.
  Both the embed (`feedRenderer.build`) and the message templates (`templateData`) read from
  it and never sanitize raw entry fields themselves, so the per-field rules (`stripHTML`,
  `httpURLOrEmpty`, in `sanitize.go`) are readable in one place. The body is kept raw and
  stripped by `content()` only when a template references `{{.Content}}`.

**Message template sandbox** (`msgtemplate`, `internal/msgtemplate`)
: The only place user-supplied message templates are parsed or run. `Compile` trims,
  falls back when blank, parses with the helper funcs and rejects unbounded constructs
  (`range`, `define`/`template`, `with`, variable assignment, string-building builtins);
  it returns an opaque `Template` whose `Execute` writes through a size-capped buffer and
  returns no partial output on error. No other production package may import
  `text/template` (`TestOnlyTheSandboxImportsTextTemplate`). The controller maps an entry
  to `templateData` and calls `Execute`.

# Glossary

Terms used in `internal/controller`, with the names the code gives them.

**Feed sync** (`feedSync`, `feed_sync.go`)
: The application of one fetch of one feed to that feed's `FeedStatus`. It is opened
  with `openFeedSync`, begun with `begin` once the feed's filter and templates are known to
  compile, and closed exactly once with a `syncVerdict`. It owns the order in which a
  fetch is applied (title stamping, sort, identity migrations, delivery ledger over the
  untrimmed fetch, watermark lookup, delivery plan, catch-up limit, backlog skip) and is
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
: A feed's delivery bookkeeping over one fetch: the `LastSeenEntry` watermark and the
  `LastSent` dedup keys, plus the stripped-identity counts they depend on. The counts are
  taken once, over the whole untrimmed fetch, so an entry's stripped identity is only
  unique if it is across the feed, however the entries are later trimmed. It alone owns
  the `LastSent` key scheme -- an entry's own key (`computeEntryKey`) plus, when its
  stripped link identity is unique in the fetch, the stripped key (`strippedKey`) -- and
  the identity migrations (`identity_migration.go`) re-key through its helpers
  (`rekeySent`, `setSent`, `forgetSent`) rather than building keys themselves.

**Watermark** (`FeedStatus.LastSeenEntry`)
: The identity of the last entry handled; a later scan resumes just past it.

**Watermark lookup** (`watermarkLookup`, `deliveryLedger.resume`, `ledger.go`)
: Where a feed's watermark stands against one fetch, as one value: `watermarkNone` (no
  watermark stored), `watermarkFound` (it stands for the entry at `index`, matched by
  identity or by unique stripped identity) or `watermarkLost` (stored, but no entry of the
  fetch it stands for). `planDelivery` decides from it: found resumes past the entry; lost
  falls through to the catch-up cap, unlike none, which may skip the backlog.

**Ambiguous legacy status** (`legacyState.ambiguous`)
: A `FeedStatus` written before `stateVersion` 1, checked but with no watermark, that can't
  say whether the feed was empty when last checked. Under `catchUpLimit: 0` it is capped
  at `defaultCatchUpLimit` in memory only, so it stays ambiguous until a sync accounts for
  the fetch.

**Feed attempt** (`feedAttempt`, `attempt.go`)
: The outcomes of processing one feed in one reconcile -- a fetch succeeding or failing,
  an entry failing to render, failing to send, being rate limited, sent or skipped, a
  skipped backlog, a config error -- each applied through the single entry point
  `record(outcome, failureClass, err)`. It owns the outcome metric label, the `Reachable` /
  `Delivered` condition, `LastError`, `RetryCount`, permanent backoff, the once-only
  persistent-failure Events and the retry decision.

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

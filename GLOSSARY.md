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
  (`LastError`, `RetryCount`, `BackoffUntil`) and `LastSent` pruning. A fetch is
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

**Delivery ledger** (`deliveryLedger`, `identity.go`)
: A feed's delivery bookkeeping over one fetch: the `LastSeenEntry` watermark and the
  `LastSent` dedup keys, plus the stripped-identity counts they depend on. The counts are
  taken once, over the whole untrimmed fetch, so an entry's stripped identity is only
  unique if it is across the feed, however the entries are later trimmed.

**Watermark** (`FeedStatus.LastSeenEntry`)
: The identity of the last entry handled; a later scan resumes just past it.

**Ambiguous legacy status** (`legacyState.ambiguous`)
: A `FeedStatus` written before `stateVersion` 1, checked but with no watermark, that can't
  say whether the feed was empty when last checked. Under `catchUpLimit: 0` it is capped
  at `defaultCatchUpLimit` in memory only, so it stays ambiguous until a sync accounts for
  the fetch.

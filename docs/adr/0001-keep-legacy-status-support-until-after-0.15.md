# 1. Keep pre-stateVersion-1 status support until after 0.15

Date: 2026-10-31

## Status

Accepted; carried out by the change that removed `internal/controller/legacy.go`.

## Context

Release 0.14.0 (2026-10-30) introduced `FeedStatus.StateVersion`. Statuses written by earlier
controllers (`stateVersion` 0) stored entry identities differently: fragments and `ref`
parameters were dropped, and an entry with no GUID, link or title got an empty identity. Some of
them also can't tell whether the feed was empty when it was last checked ("ambiguous legacy
status", see `GLOSSARY.md`). `internal/controller/legacy.go` reads these statuses, migrates their
identities and caps an ambiguous status's catch-up.

Running a current controller does not by itself convert every status.
`legacyState.settleOnClose` stamps `stateVersion: 1` on any sync once nothing legacy remains to
resolve. A status that stores legacy identities (a watermark or `LastSent` keys) needs a sync that
migrated them first, which takes a fetch with entries that passes the config check. An ambiguous
status also needs a sync that ended empty or accounted for. A status with neither is stamped on
its next sync, whatever the outcome. So feeds like these can keep `stateVersion` 0 indefinitely:

- a quiet feed that keeps answering 304;
- a feed that keeps failing to fetch, or has a config error;
- an ambiguous feed whose deliveries keep failing.

If legacy support were removed while such a status exists, unmigrated `LastSent` keys would stop
matching (re-posting already-delivered entries), and an ambiguous status under `catchUpLimit: 0`
would lose its cap (posting its whole backlog, or nothing).

## Decision

Keep legacy support through the 0.15 release line. Remove it in a later, breaking (`feat!:`)
release.

Until then, architecture reviews should not propose removing it. They may propose keeping it
isolated: everything legacy-specific stays in `legacy.go`, reached from the rest of the controller
only through the calls CLAUDE.md lists.

## Consequences

- The removal PR deletes `legacy.go`, its call sites and its tests. Its release notes tell users
  to upgrade through 0.14.x or 0.15.x first, and to confirm that no feed is still on the old
  format:

  ```bash
  set -o pipefail
  kubectl get feedgroups -A -o json | jq -r '
    .items[] | . as $g | .status.feeds[]? | select((.stateVersion // 0) < 1)
    | "\($g.metadata.namespace)/\($g.metadata.name)  \(.rssUrl)"'
  ```

  An empty output with exit status 0 means that cluster is safe to upgrade past the removal; with
  `pipefail`, a failed `kubectl` call exits non-zero instead of looking like an empty result.
- Until then, every change to identity, the delivery ledger or settlement has to keep the legacy
  path working and tested.

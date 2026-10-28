# rss2discord-operator — Improvement Plan

Review date: 2026-10-28, against `main` @ `0626bcb`. No open issues or feature PRs existed at review time
(only the Renovate dashboard and a release-please PR).

This file is written to be handed to an implementation subagent. **Pick item IDs from the menu**, then
hand the subagent this file plus the chosen IDs (see "Prompt for the subagent" at the bottom).

---

## Menu

| ID  | Type          | Title                                                                    | Size | Risk | Main files |
|-----|---------------|--------------------------------------------------------------------------|------|------|------------|
| B1  | Bug           | Namespaced RSS elements (`atom:link`, `media:title`, …) clobber core fields | M    | Med  | `internal/rss/client.go` |
| B2  | Bug           | Atom `type="xhtml"` title/summary/content parse as empty                 | S    | Low  | `internal/rss/client.go` |
| B3  | Bug           | Date-less entries render as `0001-01-01T00:00:00Z`                       | S    | Low  | `internal/controller/feedgroup_controller.go` |
| B4  | Bug           | Unparsable interval skips the status write → duplicate sends forever    | S    | Low  | `internal/controller/feedgroup_controller.go` |
| B5  | Bug           | Blocked-address (SSRF guard) and TLS errors are classified "Other"/transient | S | Med  | `internal/rss/client.go`, `internal/controller/classify.go` |
| O1  | Observability | Per-feed health/freshness gauges + alerts that still fire during permanent backoff | L | Med | `metrics.go`, chart alerts/dashboard, promtool tests |
| V1  | API hardening | Tighten CRD validation (bounds, duration CEL, URL/ID patterns)          | S    | Med  | `api/v1alpha1/feedgroup_types.go` |
| F1  | Feature       | YouTube / Media RSS support (`media:group` thumbnail + description)     | M    | Low  | `internal/rss/client.go` |
| F2  | Feature       | More template fields: `.Content`, `.Image`, `.FeedTitle`, `.PublishedUnix` | M | Low  | rss + controller render path |
| F3  | Feature       | Template helper functions (`truncate`, `default`, `discordTime`, …)     | S    | Low  | `internal/controller/feedgroup_controller.go` |
| F4  | Feature       | Embed large-image option (`embed.imageStyle: thumbnail\|image`)         | S    | Med  | types, controller, `internal/discord/client.go` |
| F5  | Feature       | Accept `ptb.`/`canary.discord.com` webhook URLs by rewriting to `discord.com` | S | Med | `internal/discord/client.go` |
| F6  | Feature (API) | `catchUpLimit: 0` means "skip backlog, start from newest"               | S    | Med  | types, controller |
| C1  | Chart/ops     | First-class chart values for concurrency/OTLP/log level; revisit memory limit | S | Low | `dist/chart/values.yaml`, `manager.yaml`, golden tests |
| R1  | Refactor      | Split 1.7k-line controller file; bundle `sendNewEntries`' 13 params     | M    | Low  | `internal/controller/` |
| D1  | Docs          | Correct minimum Kubernetes version; document new features               | S    | Low  | `README.md`, `QUICKSTART.md` |

**Suggested picks if you only want a few:** B1, B3, B4, B5, O1 (real bugs and a monitoring hole), then F1 + F2
(the most user-visible feature gaps for a feed→Discord bridge).

**Dependencies / ordering**
- B4 and V1 overlap (V1's CEL duration check also rejects the input that triggers B4); do B4 regardless — it is
  the runtime backstop for objects admitted under an older schema.
- B1 → B2 → F1 → F2 all touch the parser structs in `internal/rss/client.go`; do them in that order, one branch
  each, or as one combined branch.
- F2 and F3 both touch `renderTemplate`/`compileTemplate`; do F2 first.
- R1 is a pure move. Do it **last** (or first, alone) so it doesn't conflict with every other branch.
- D1 should be updated by each feature item as it lands; the standalone D1 item covers only the version fix.

**Considered and not recommended**
- *Watching the webhook Secret so rotations reconcile immediately.* It needs `list`/`watch` on Secrets
  cluster-wide, which `cmd/main.go` deliberately avoids (`Cache.DisableFor: Secret`). The current behaviour
  (picked up on the next interval/retry) is an acceptable trade-off.
- *A validating admission webhook to pre-check templates/regexes.* Adds cert-manager/TLS plumbing for a
  failure that already surfaces as a `ConfigError` condition + one Event.

---

## Ground rules for the implementer (read before any item)

1. Read `CLAUDE.md` and `AGENTS.md`. They override anything here.
2. **Tooling:** all checks go through `mise` (`mise run test`, `mise run lint-fix`, `mise run manifests generate`,
   `mise run helm-chart-refresh`, `mise run promtool-rules`). If `mise` is not installed, install it
   (`curl https://mise.run | sh`, then `~/.local/bin/mise install` in the repo) rather than calling
   `go test`/`golangci-lint` directly. Single tests: `go test ./internal/rss/ -run TestName -v` is fine
   after `mise run manifests generate`.
3. **Never edit generated files** (`config/crd/bases/*`, `config/rbac/role.yaml`, `zz_generated.*`,
   `api/*/applyconfiguration/**`, `PROJECT`). Regenerate them.
4. **SSRF guards:** any change to `internal/rss/client.go` or `internal/discord/client.go` must be reviewed by the
   `security-reviewer` subagent before you push, and must be called out in the PR body (the PR template asks).
   Never relax `isPublicIP`, `guardDialControl`, `AllowedWebhookHosts`, or the no-redirect policy. Tests reach
   local servers only via the existing helpers (`testRSSClient()`, registering into `discord.AllowedWebhookHosts`
   in `*_test.go`).
5. **New metric outcomes / failure classes:** add the class to `allFailureClasses` (see
   `observability_contract_test.go`), and check whether the Grafana dashboard
   (`dist/chart/dashboards/feedgroup-overview.json`) and `prometheus-rule.yaml` need a panel/alert.
6. **Status fields:** new per-feed state goes on `FeedStatus`; after changing status types run
   `mise run generate` (`TestStatusApplyConfigurationCoversEveryField` enforces it).
7. Match the surrounding style: the codebase comments heavily on *why*; keep that density, but don't narrate.
8. **Commits:** Conventional Commits (`fix(rss): …`, `feat(controller): …`, `feat!:` for breaking API semantics).
   One item per branch/PR unless told otherwise. Fill in `.github/PULL_REQUEST_TEMPLATE.md`.
   Never write bare `owner/repo#123` references anywhere.
9. **Definition of done for every item:** `mise run lint-fix` clean, `mise run test` green, plus
   `mise run promtool-rules` if alerts changed and `go test ./test/chart/...` (golden files) if the chart changed.
   Each bug fix ships with a regression test that fails before the fix.

---

## Items

### B1 — Namespaced RSS elements clobber core fields

**Problem.** `encoding/xml` matches struct tags by *local name regardless of namespace*, and for non-slice
fields the last match wins. In `rssItem` (`internal/rss/client.go:347`), `Link xml:"link"`,
`Title xml:"title"`, `Author xml:"author"`, `Description xml:"description"` therefore also match
`<atom:link>`, `<media:title>`, `<itunes:author>`, `<media:description>` etc. Verified with a probe:

```xml
<item><title>Real</title><link>https://a/1</link><atom:link href="https://a/self" rel="self"/>
      <media:title>MediaTitle</media:title><description>short</description></item>
```
parses as `Title:"MediaTitle"`, `Link:""` and — because Link is empty and there's no GUID — `ID:"MediaTitle"`.
So the post has the wrong title, no link, and an unstable identity (dedup keyed on title).

**Change.**
- Introduce a small helper type, e.g.
  ```go
  type nsText struct {
      XMLName xml.Name
      Text    string `xml:",chardata"`
  }
  ```
  and change the affected `rssItem` fields to `[]nsText` (`title`, `link`, `description`, `author`, `guid`,
  `pubDate`).
- Add `pickText(cands []nsText) string` with preference: first non-blank value whose `XMLName.Space` is `""`
  → then the RSS 1.0 namespace `http://purl.org/rss/1.0/` (RDF items live in that default namespace, so
  they must keep working) → otherwise first non-blank of any namespace (keeps today's lenient fallback).
  Elements with blank chardata (e.g. self-closing `<atom:link href=…/>`) are skipped.
- `Creator xml:"creator"` and `DCDate xml:"date"` are intentionally namespace-agnostic; leave them.
- Use `pickText` in `rssItemsToEntries` (`client.go:581`).

**Tests** (`internal/rss/parse_test.go`, table-driven like its neighbours):
- the probe above → Title "Real", Link "https://a/1", ID "https://a/1";
- `<atom:link>` *before* `<link>` (order independence);
- `<itunes:author>` alongside `<author>` → `<author>` wins; only `<itunes:author>` → it is used;
- an RDF document still parses (existing RDF tests must stay green);
- add the probe as a fuzz seed in `internal/rss/fuzz_test.go`.
- Run `mise run test` and the bench in `internal/rss/bench_test.go` (`go test -bench . ./internal/rss/`)
  to confirm no large regression.

**Security note:** touches `client.go` → run `security-reviewer`. The dial guard is untouched.

---

### B2 — Atom `type="xhtml"` content is dropped

**Problem.** For Atom text constructs with `type="xhtml"` the value is a child `<div>`; a plain `string` field
only captures direct chardata, so `title`, `summary` and `content` come out empty (verified). The entry then
posts with an empty title/description.

**Change.** In `atomEntry` (`client.go:410`) replace `Title`, `Summary`, `Content` with:
```go
type atomText struct {
    Type  string `xml:"type,attr"`
    Text  string `xml:",chardata"`
    Inner string `xml:",innerxml"`
}
func (t atomText) value() string // Inner if Type=="xhtml", else Text
```
`innerxml` for `xhtml` yields raw markup which `stripHTML` in the controller already turns into plain text.
Do **not** use `innerxml` for `type="html"` (it would return still-escaped `&lt;p&gt;`).

**Tests:** xhtml title/summary/content each produce the expected text after `stripHTML`; `type="html"` and
`type="text"` unchanged; existing Atom tests green.

---

### B3 — Date-less entries render as year 0001

**Problem.** Many feeds omit dates (the code already sorts by `Seq` for that case), but
`buildDiscordMessage` sets `Embed.Timestamp: entry.Published.UTC().Format(time.RFC3339)`
(`feedgroup_controller.go:1494`) and `renderTemplate` sets `Published` the same way (`:1449`), so a zero time
is sent as `0001-01-01T00:00:00Z` — Discord shows a "1/1/0001" footer, templates print it verbatim.

**Change.** Add `formatPublished(t time.Time) string` that returns `""` for `t.IsZero()`, use it in both places.
`discordEmbed.Timestamp` is already `omitempty`, so the field disappears from the payload.

**Tests:** in `processfeed_test.go`/`payload_test.go` style — zero `Published` → embed JSON has no
`timestamp` key; `{{.Published}}` renders `""`; non-zero unchanged.

---

### B4 — Unparsable interval loses status → duplicate sends

**Problem.** `requeueWithStatus` (`feedgroup_controller.go:943`) parses the interval *before* writing status
and returns early on error. The CRD pattern admits values that `time.ParseDuration` rejects on overflow
(e.g. `interval: "9999999999999h"` — 14 chars, passes the regex and MaxLength; verified). The reconcile has already
sent messages by then; the status (LastSent/LastSeenEntry/LastChecked) is discarded, controller-runtime
retries with error backoff, the feed is "due" again, and the same entries are re-sent on every retry.

**Change.**
- In `requeueWithStatus`, on a parse error keep going with `fallback` as the duration, write status as normal,
  and surface the problem: set `Ready=False` with reason `InvalidInterval` (message names the field/value)
  — e.g. pass the parse error into `setReadyCondition` alongside `webhookErr`, or set it after. Return
  `ctrl.Result{RequeueAfter: fallback}` with **no** error (returning an error would just hot-loop).
- Emit one Warning Event on transition (reuse the `setFeedCondition`-style "changed" gating, or
  `apimeta.SetStatusCondition`'s return value) so it is visible in `kubectl describe`.
- Do the same review for `RetryInterval` (it flows through the same function).

**Tests:** envtest/unit test with `Interval: "9999999999999h"` and a feed with one new entry: first reconcile
sends once and persists `LastSent`; second reconcile sends nothing; Ready reason is `InvalidInterval`.
(Construct the object directly in the test — V1 may later reject it at admission; if V1 lands first, bypass
by calling the reconciler helpers on an in-memory object as existing unit tests do.)

---

### B5 — Classify blocked-address and TLS failures

**Problem.** `classifyFetchError` (`classify.go:110`) has no case for the SSRF guard's refusal
(`guardDialControl`, `client.go:176`, returns a plain `fmt.Errorf`) or for certificate errors. Both fall to
`classOther`, which is **transient**: a feed pointing at a private IP (a permanent misconfiguration) is retried
every RetryInterval forever, and the condition reason "Other" doesn't tell the user why.

**Change.**
- In `internal/rss`, add an exported typed error, e.g. `type BlockedAddressError struct{ Addr netip.Addr }`,
  and return it from `guardDialControl`. **Behaviour of the guard must be identical** — only the error type
  changes. Confirm via `errors.As` that it survives `net.OpError`/`url.Error` wrapping.
- In `classify.go` add:
  - `classBlockedAddress = {metric "blocked_address", reason "BlockedAddress", permanent: true}`
  - `classTLSError = {metric "tls_error", reason "TLSError", permanent: true}` matched via
    `errors.As` for `*tls.CertificateVerificationError`, `x509.UnknownAuthorityError`,
    `x509.HostnameError`, `x509.CertificateInvalidError`. (Permanent → exponential backoff capped at 6h,
    which still recovers on its own once the cert is fixed.)
  - Order: check these before `classifyNetworkError`.
- Add both to `allFailureClasses`. Existing alert/dashboard regexes (`fetch_error_.+`) already cover them —
  verify with `observability_contract_test.go`.

**Tests:** `classify_test.go` table rows for each (construct the wrapped errors the way `http.Client.Do`
returns them); a `processFeed` test showing a blocked address sets `BackoffUntil`.

**Security note:** touches `client.go` guard code → run `security-reviewer`; the PR must state the guard's
accept/reject set is unchanged (the existing `isPublicIP` tests are the proof).

---

### O1 — Per-feed health gauges; alerts that work during backoff

**Problem.**
1. All three alerts are `rate(...[15m]) > 0 for: 15m`. A feed in permanent backoff is only retried every
   10m, 20m, 40m … 6h, so its error counter stops increasing within any 15m window and the alert resolves —
   **a feed stuck on 404/410/bad XML goes silent in monitoring** exactly when it most needs attention.
2. `rss2discord_feed_last_success_timestamp_seconds` is per *FeedGroup*: one healthy feed keeps it fresh while
   a sibling is dead. It also isn't populated after a restart until a feed becomes due again.
3. There is no staleness alert at all.

**Change.**
- Add two per-feed gauges in `metrics.go` (labels `namespace,name,rss_url`):
  - `rss2discord_feed_healthy` — 1 if the feed's `LastError == ""`, else 0. (Paused feeds: omit the series,
    or add a `paused` gauge — choose omit, and document it.)
  - `rss2discord_feed_last_checked_timestamp_seconds` — parsed from `FeedStatus.LastChecked`.
- Set both from `Status.Feeds` in one helper (e.g. `recordFeedStateMetrics(feedGroup)`) called from
  `requeueWithStatus` just before the DeepEqual short-circuit, so values come from persisted state: they
  survive restarts and are set even for feeds skipped as not-due or in backoff.
- Extend `deleteFeedMetrics` / `deleteFeedGroupMetrics` to drop the new series (there are tests for the
  existing ones — mirror them).
- Keep the existing group-level gauge (dashboards use it); mention it in docs as group-level.
- `prometheus-rule.yaml`: add
  - `RSS2DiscordFeedUnhealthy`: `rss2discord_feed_healthy == 0` for a new value `prometheusRule.unhealthyFor`
    (default `30m`);
  - `RSS2DiscordFeedStale`: `time() - rss2discord_feed_last_checked_timestamp_seconds > <staleAfter>`
    (new value `prometheusRule.staleAfter`, default `6h` seconds-equivalent; document it should exceed the
    largest `interval`).
  Both new values go in `values.yaml` (hand-tuned file — edit directly; do **not** run a raw
  `kubebuilder edit --force`).
- Add a dashboard panel (table of unhealthy feeds) to `feedgroup-overview.json`.
- Update `test/promrules/tests.yaml` with cases for both alerts, including one modelling a feed in backoff
  (counter flat for hours while `feed_healthy == 0`) to prove the hole is closed.
- Update chart golden files (`test/chart/testdata/prometheus_rule_enabled.golden.yaml`, dashboard golden).

**Tests:** metrics_test.go for set/delete; `mise run promtool-rules`; `go test ./test/chart/...`.

---

### V1 — Tighten CRD validation

**Problem / change** (all in `api/v1alpha1/feedgroup_types.go`, then `mise run manifests generate` and
`mise run helm-chart-refresh`):

| Field | Today | Add |
|---|---|---|
| `retries` (`:184`) | any int32, negatives accepted | `Minimum=0`, `Maximum=100` |
| `catchUpLimit` (`:217`) | any int32 | `Minimum=0`, `Maximum=100` (coordinate with F6) |
| `interval` | regex only; `1ns` and overflow values pass | CEL `duration(self) >= duration('1m')` |
| `retryInterval` | same | CEL `duration(self) >= duration('10s')` |
| `avatarURL` (`:179`) | free text | `Pattern=^https?://` (Discord rejects anything else → every send fails) |
| `forumThreadID` (`:124`) | free text | `Pattern=^[0-9]+$` (Discord snowflake) |

Notes:
- CEL `duration()` does not accept `µs`; drop `µs` from the interval `Pattern` so the regex and CEL agree.
- CEL `duration()` errors on overflow, so this also blocks B4's trigger at admission.
- Existing objects that violate a new rule: Kubernetes CRD validation ratcheting (on by default in recent
  versions) lets unchanged fields through on update; mention this in the PR and in `CHANGELOG` notes.
  Use `fix:` not `feat!:` unless you choose to treat it as breaking — call it out for the reviewer.
- Add envtest cases in `feedgroup_controller_test.go` (it already has admission-rejection tests for
  `username`) for each rule: one rejected, one accepted.

---

### F1 — YouTube / Media RSS support

**Problem.** YouTube channel feeds (`https://www.youtube.com/feeds/videos.xml?channel_id=…`) — probably the
most common thing people pipe into Discord — are Atom with the thumbnail and description nested in
`<media:group>`:
```xml
<entry><id>yt:video:abc</id><title>…</title><link rel="alternate" href="https://www.youtube.com/watch?v=abc"/>
  <media:group><media:thumbnail url="https://i.ytimg.com/vi/abc/hqdefault.jpg"/>
    <media:description>…</media:description></media:group></entry>
```
`atomEntry` only reads image enclosures and summary/content, so these entries post with no image and an empty
description. RSS items can also nest media in `<media:group>`, which `rssItem` ignores.

**Change.**
- Add `type mediaGroup struct { Thumbnails []rssMediaURL `xml:"thumbnail"`; Contents []rssMediaURL `xml:"content"`; Description string `xml:"description"` }`
  with field `MediaGroup *mediaGroup `xml:"group"`` on both `atomEntry` and `rssItem`.
  (`group` is unambiguous enough; if B1 has landed, consider matching it with namespace
  `http://search.yahoo.com/mrss/`.)
- Atom: also read direct `<media:thumbnail>` on the entry. Image precedence: enclosure link →
  media:thumbnail → media:group thumbnail → media:group image content.
- Description fallback: summary → content → media:group description.
- RSS: extend `entryImage` to consider the group; description fallback to group description when empty.
- Add `internal/rss/testdata/youtube.xml` (hand-written, minimal, no real channel data needed) and a test.

---

### F2 — More template fields

**Problem.** Templates only get Title/Description/Link/Published/Author/Categories (`renderTemplate`,
`feedgroup_controller.go:1437`). Users can't reference the image, the full post body, the feed's own name
(useful when one webhook carries several feeds), or a Discord dynamic timestamp.

**Change.**
- `rss.Entry`: add `Content string` — RSS `<content:encoded>` (match local name `encoded`; with B1's helper,
  prefer the `http://purl.org/rss/1.0/modules/content/` namespace) and Atom `<content>` when a `<summary>`
  also exists. Keep `Description` semantics unchanged.
- `rss.FetchResult`: add `FeedTitle string` — RSS `channel/title`, Atom `feed/title`, RDF `channel/title`.
  Thread it into `processFeed` → `sendNewEntries` → `buildDiscordMessage` → `renderTemplate` (if R1 has
  landed, through the bundled struct).
- Template data gains: `Content` (stripHTML'd), `Image` (passed through `httpURLOrEmpty`), `FeedTitle`,
  `PublishedUnix` (int64, `0` when Published is zero — pairs with B3), enabling `<t:{{.PublishedUnix}}:R>`.
- Update the placeholder list in the `Format` doc comment on `FeedGroupSpec` (it feeds the CRD description →
  `mise run manifests`) and the README "Message template" section.

**Tests:** parse tests for content:encoded and feed titles (RSS/Atom/RDF); render tests for each new field;
existing fuzz targets still pass.

---

### F3 — Template helper functions

**Change.** In `compileTemplate`, attach a `template.FuncMap` before `Parse`:
`truncate(n int, s string)` (rune-safe, ellipsis — reuse `truncateMessage`), `default(def, v string)`,
`lower`, `upper`, `trim`, `replace(old, new, s string)`, `discordTime(style string, t int64)` →
`<t:%d:%s>` (validate style ∈ `t T d D f F R`, else return error). Pipeline-friendly arg order
(`{{.Description | truncate 200}}`).
- Keep the set small and pure: no env, file, network or reflection helpers.
- Document in README with two examples.

**Tests:** each function, including bad `discordTime` style producing a render error (and thus the existing
`RenderError` path), and that an unknown function is a `ConfigError` at compile time.

---

### F4 — Embed large image option

**Change.**
- `EmbedSpec`: add `ImageStyle string` with `+kubebuilder:validation:Enum=thumbnail;image`, default
  `thumbnail` (current behaviour).
- `discord.Embed`: add `ImageURL`; `discordEmbed`: add `Image *discordEmbedMedia `json:"image,omitempty"``;
  map it in `embedToPayload`.
- In `buildDiscordMessage`, put `httpURLOrEmpty(entry.Image)` in `ImageURL` or `ThumbnailURL` per style.
- `mise run manifests generate`, README Embed table.

**Tests:** payload test for both styles; envtest that an invalid enum is rejected.
**Security note:** touches `internal/discord/client.go` (payload only) → run `security-reviewer`.

---

### F5 — Accept `ptb.` / `canary.` Discord webhook URLs

**Problem.** Users who copy a webhook URL from Discord's PTB/Canary clients get
`https://ptb.discord.com/api/webhooks/…` / `https://canary.discord.com/…`; `SendMessage` rejects these as
non-allowlisted and every send fails with a confusing error.

**Change (no allowlist broadening).** Before the allowlist check in `SendMessage`, if the hostname is
`ptb.discord.com` or `canary.discord.com` (exact, case-insensitive), rewrite `parsedURL.Host` to `discord.com`.
The request then only ever goes to an already-allowed host, so the egress surface is unchanged. Keep the
rewrite out of `AllowedWebhookHosts`. Rate-limiter keys use `c.webhookURL`; normalize that too (in
`NewClientWithLimiter`) so a PTB and a normal URL for the same webhook share a cooldown.

**Tests:** both variants are sent to the mock server; `evil.ptb.discord.com`, `ptb.discord.com.evil.com`,
and `http://ptb.discord.com` are still rejected.
**Security note:** must go through `security-reviewer`; call it out in the PR.

---

### F6 — `catchUpLimit: 0` = skip backlog

**Problem.** `limitCatchUp` (`feedgroup_controller.go:1598`) treats `<= 0` as "use default 5", so there is no
way to add a feed and *not* post its backlog — a common ask when adding a busy feed.

**Change.** The CRD `+default=5` is applied by the API server both on admission and when reading old objects,
so an explicit `0` is distinguishable from "unset" on objects from the API. Make `0` mean: send nothing on
first sync, set `LastSeenEntry` to the newest entry's identity, mark Delivered condition reason
`BacklogSkipped`. Negative stays rejected (V1) / treated as default.
- This changes semantics for anyone who set `0` explicitly → use `feat!:` and a CHANGELOG note, **or** add a
  new boolean `skipBacklog` instead if you prefer no break. **Ask the user which before implementing.**
- Many unit tests construct `FeedGroupSpec{}` literals with `CatchUpLimit` zero: audit them — the helper
  builders in `helpers_test.go` should set 5 explicitly.

**Tests:** new feed with 10 entries and `catchUpLimit: 0` → 0 sends, watermark = newest; next fetch with one
new entry → exactly 1 send.

---

### C1 — Chart values and memory headroom

**Change.**
- `values.yaml` + `templates/manager/manager.yaml` (both hand-tuned; edit directly): add
  `manager.maxConcurrentReconciles` (→ `--max-concurrent-feedgroup-reconciles`, omitted when unset),
  `manager.otlpEndpoint` (→ `--otlp-endpoint`, omitted when empty), `manager.logLevel` (→ `--zap-log-level`).
  Keep `manager.args` working for anything else.
- Memory: the default limit is `128Mi`, while one reconcile may buffer up to `maxConcurrentFetches (10) ×
  maxFeedResponseBytes (10MB)` plus XML decode overhead, and more with concurrent reconciles. Either raise the
  default limit to `256Mi` or document the sizing rule in `values.yaml` next to `resources`. Prefer documenting
  + raising; don't change the Go constants in this item.
- Update chart golden files (`test/chart/testdata/manager_defaults.golden.yaml` etc.) and add a golden case
  with the new values set. Regenerate `dist/install.yaml` only if `config/` changed.

---

### R1 — Split the controller file

**Problem.** `feedgroup_controller.go` is ~1,735 lines mixing reconcile flow, rendering/HTML stripping, identity
normalization and backoff maths; `sendNewEntries` takes 13 parameters and `buildDiscordMessage` 7.

**Change (pure move + one small struct, no behaviour change).**
- New files in `internal/controller/`: `render.go` (templates, `stripHTML`, `buildDiscordMessage`,
  `truncateMessage`, colour/URL helpers), `identity.go` (`normalizeIdentity`, `entryIdentity`, hashing,
  watermark clamp, `pruneLastSent`), `backoff.go` (duration parsing, `feedDue`, backoff helpers),
  `status.go` (`ensureFeedStatuses`, conditions, `applyStatus`, `requeueWithStatus`).
- Introduce `type feedRenderer struct { group *v1alpha1.FeedGroup; feed *v1alpha1.FeedSpec; embed *v1alpha1.EmbedSpec; content, description, threadName *template.Template; filter *regexp.Regexp }`
  built once in `processFeed`, with methods `matches(entry)` and `build(entry)`; pass it instead of the
  loose parameters to `sendNewEntries`, `otherEntryRenders`, `buildDiscordMessage`.
- Move matching tests (`striphtml_test.go` already exists) only if trivial; don't rewrite tests.
- Verify with `git diff --stat` that the change is moves plus the struct, and that coverage in `cover.out`
  is unchanged.

---

### D1 — Docs: minimum Kubernetes version

**Problem.** README says "Kubernetes 1.26+", but the CRD uses CEL `x-kubernetes-validations` including
`lowerAscii()` from the CEL strings extension, and `exists_one`; verify the earliest version where all CEL
features used by `config/crd/bases/*.yaml` (and any added by V1, e.g. `duration()`) are available and
enabled by default, and state that version (and the version e2e runs against — `KIND_NODE_IMAGE` in
`.mise/config.toml`) in README and QUICKSTART. Cite the Kubernetes docs page you used in the PR body (full
URL, not `owner/repo#n`).

---

## Prompt for the subagent

Copy, fill in the IDs, and give it to the subagent (model: sonnet):

> You are implementing items **<IDs>** from `IMPROVEMENT_PLAN.md` in `/home/user/rss2discord-operator`.
> Read `CLAUDE.md`, `AGENTS.md`, then the "Ground rules" section and your items in the plan. For each item,
> in the plan's dependency order:
> 1. Create a branch `improve/<id>-<slug>` from up-to-date `main`.
> 2. Write the regression/feature test first and confirm it fails (for bugs).
> 3. Implement exactly what the item describes; don't widen scope. If something in the plan turns out to be
>    wrong (line numbers drift, a claim doesn't reproduce), stop and report rather than improvising a
>    different design. Item F6 requires asking the user a question before implementing.
> 4. Run the codegen tasks the item names, then `mise run lint-fix` and `mise run test` (plus
>    `mise run promtool-rules` / `go test ./test/chart/...` where the item touches alerts or the chart).
> 5. If you touched `internal/rss/client.go` or `internal/discord/client.go`, run the `security-reviewer`
>    agent on the diff and include its verdict in the PR body.
> 6. Commit with a Conventional Commit message, push, and open a PR using the repo's PR template.
> Report back per item: branch, PR link, test results, and anything you deviated from or couldn't do.

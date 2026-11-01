# rss2discord-operator

[![codecov](https://codecov.io/gh/maverickd650-labs/rss2discord/graph/badge.svg)](https://codecov.io/gh/maverickd650-labs/rss2discord)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/maverickd650-labs/rss2discord/badge)](https://scorecard.dev/viewer/?uri=github.com/maverickd650-labs/rss2discord)
[![Tests](https://github.com/maverickd650-labs/rss2discord/actions/workflows/test.yml/badge.svg)](https://github.com/maverickd650-labs/rss2discord/actions/workflows/test.yml)
[![Lint](https://github.com/maverickd650-labs/rss2discord/actions/workflows/lint.yml/badge.svg)](https://github.com/maverickd650-labs/rss2discord/actions/workflows/lint.yml)
[![Latest release](https://img.shields.io/github/v/release/maverickd650-labs/rss2discord)](https://github.com/maverickd650-labs/rss2discord/releases)
[![License](https://img.shields.io/github/license/maverickd650-labs/rss2discord)](LICENSE)

A Kubernetes operator that watches RSS feeds and posts new entries to Discord via webhooks. Feeds are configured declaratively with a `FeedGroup` custom resource.

> **Note:** This project is vibecoded — built quickly with heavy AI assistance. It does include specific hardening (SSRF guards on both the RSS fetch and Discord webhook paths, a response-size cap, `@everyone`/`@here` mention suppression, `javascript:`/`data:` URI stripping on links and images, bounded message templates, and Discord API length clamping), but it hasn't had a third-party security review, so read the code before trusting it with anything important.

## Features

- Define RSS feeds as Kubernetes CRDs
- Post updates to Discord webhooks
- Filter entries by regex or keywords
- Customize the message format per feed group or per feed
- Render entries as native Discord embeds (colored bubble, thumbnail, author/footer) instead of plain text
- Post into forum channels, either as a new thread per entry or into an existing thread
- Override the webhook's display name/avatar per feed group
- Conditional GET (ETag / If-Modified-Since) on RSS fetches — skips re-downloading and re-parsing unchanged feeds
- Configurable check interval and retry behavior
- Prometheus metrics, an optional ServiceMonitor, PrometheusRule alerts, and a Grafana dashboard for per-outcome feed processing

## Installing

Requires a Kubernetes 1.29+ cluster and a Discord webhook URL. The CRD relies on CEL validation rules (`x-kubernetes-validations`: `lowerAscii()`, `exists_one`, `duration()`), which are GA and always enabled from 1.29; they were beta (on by default) from 1.25, but 1.29 is the oldest version this project targets. CI runs the end-to-end tests against Kubernetes 1.37.

### Helm

```bash
helm install rss2discord-operator ./dist/chart \
  --namespace rss2discord-operator-system \
  --create-namespace
```

### kubectl

```bash
kubectl apply -f dist/install.yaml
```

### mise

```bash
IMG=my-registry/rss2discord-operator:v0.1.0 mise run deploy
# or
IMG=my-registry/rss2discord-operator:v0.1.0 mise run helm-deploy
```

## Usage

1. Create a secret with your Discord webhook URL:

```bash
kubectl create secret generic discord-webhook \
  -n default \
  --from-literal=url='https://discord.com/api/webhooks/YOUR_WEBHOOK_ID/YOUR_TOKEN'
```

2. Create a `FeedGroup`:

```yaml
apiVersion: rss2discord.maverickd650.dev/v1alpha1
kind: FeedGroup
metadata:
  name: tech-news
  namespace: default
spec:
  discordWebhookSecretRef:
    name: discord-webhook
    key: url
  interval: "30m"
  retries: 3
  retryInterval: "5m"
  format: |
    **{{.Title}}**
    {{.Description}}
    [Read more]({{.Link}})
  feeds:
    - rssUrl: "https://news.ycombinator.com/rss"
      filter:
        keywords:
          - kubernetes
          - golang
    - rssUrl: "https://www.reddit.com/r/golang/.rss"
      paused: false
```

To render entries as embeds (with a colored bubble and thumbnail) instead of plain text, and to post each entry as a new forum thread:

```yaml
spec:
  embed:
    enabled: true
    color: "#5865F2"
    footerText: "via tech-news"
  feeds:
    - rssUrl: "https://news.ycombinator.com/rss"
      forumThreadName: "{{.Title}}"
```

```bash
kubectl apply -f feedgroup.yaml
```

3. Watch it work:

```bash
kubectl logs -n rss2discord-operator-system -l control-plane=controller-manager -f
```

## Configuration reference

### FeedGroup spec

| Field | Type | Default | Description |
| --- | --- | --- | --- |
| `discordWebhookSecretRef` | `SecretKeySelector` | required | Secret containing the Discord webhook URL |
| `interval` | `string` | `30m` | How often to check feeds. At least `1m`; Go duration syntax (`ns`, `us`, `ms`, `s`, `m`, `h`) |
| `retries` | `int` | `3` | Retries for failed operations (0–100) |
| `retryInterval` | `string` | `5m` | Delay between retries. At least `10s` |
| `format` | `string` | see below | Discord message template (used when `embed` is not enabled) |
| `embed` | `*Embed` | optional | Default embed config for all feeds in the group |
| `username` | `string` | optional | Overrides the webhook's display name. Rejected at `kubectl apply` time (CEL validation) if it contains "clyde" or "discord", or is over 80 characters — Discord's own webhook API rejects these |
| `avatarURL` | `string` | optional | Overrides the webhook's avatar. Must start with `http://` or `https://` |
| `catchUpLimit` | `int` | `5` | Caps how many entries are sent when a feed has no usable watermark: on its first sync, or when its recorded last-seen entry has scrolled out of the feed (0–100). A normal fetch that finds the watermark is never capped. Set `0` to send no backlog: the first non-empty sync records the newest entry and only later entries are delivered. A feed that was empty on its first sync then delivers everything that appears later, uncapped; a watermark that later scrolls out falls back to the default cap of 5. Omitting the field keeps `5`. |
| `feeds` | `[]Feed` | required | RSS feeds to monitor |

### Feed

| Field | Type | Default | Description |
| --- | --- | --- | --- |
| `rssUrl` | `string` | required | RSS feed URL |
| `filter` | `*Filter` | optional | Filter rules for entries |
| `format` | `string` | optional | Overrides the group's format for this feed |
| `embed` | `*Embed` | optional | Overrides the group's embed config for this feed |
| `forumThreadName` | `string` | optional | Template for a new forum post's title; set on FeedGroups whose webhook targets a forum channel |
| `forumThreadID` | `string` | optional | Posts into an existing forum thread instead of creating a new one (takes precedence over `forumThreadName`). Digits only (a Discord ID) |
| `paused` | `bool` | `false` | Stop processing this feed without removing it |

### Filter

| Field | Type | Description |
| --- | --- | --- |
| `regex` | `string` | Regex matched against title/description |
| `keywords` | `[]string` | Keywords matched against title/description (OR) |

### Embed

When `enabled`, a feed's messages are sent as a native Discord embed (the colored bubble UI) instead of plain text. The embed's title/link/timestamp come directly from the entry; only the description is templated.

| Field | Type | Default | Description |
| --- | --- | --- | --- |
| `enabled` | `bool` | `false` | Render messages as embeds instead of plain text |
| `color` | `string` | none | Side-bar color, as hex (`#5865F2` or `5865F2`) |
| `descriptionFormat` | `string` | `{{.Description}}` | Template for the embed description |
| `authorName` | `string` | optional | Shown on the embed's author line |
| `footerText` | `string` | optional | Shown in the embed's footer |
| `imageStyle` | `string` | `thumbnail` | `thumbnail` shows the entry image small beside the description; `image` shows it large below |

If the feed entry has a lead image (an RSS `<enclosure>` or Media RSS `<media:thumbnail>`/`<media:content>`, or an Atom `<link rel="enclosure">`), it's attached automatically as the embed's thumbnail (or as the large image with `imageStyle: image`).

### Message template

Default (used only when `embed.enabled` is false):

```YAML
**{{.Title}}**
{{.Description}}
[Read more]({{.Link}})
```

Available fields:

| Field | Description |
|---|---|
| `.Title` | Entry title (HTML stripped) |
| `.Description` | Entry summary (HTML stripped) |
| `.Content` | Full entry body (HTML stripped): RSS `<content:encoded>`, or Atom `<content>`. Empty if the feed has none. For an Atom entry without a `<summary>`, this is the same text as `.Description`. Long bodies are cut to Discord's message limit, so you'll usually want something like `{{.Description}}` instead unless you know the feed's posts are short |
| `.Link` | Entry URL |
| `.Image` | Lead image URL (an enclosure or Media RSS thumbnail), or empty if there isn't one or it isn't an `http(s)` URL |
| `.Published` | Publish time as RFC 3339 UTC, or empty if the entry has no date |
| `.PublishedUnix` | Publish time as Unix seconds, or `0` if the entry has no date |
| `.Author` | Entry author |
| `.Categories` | Entry categories, comma-separated |
| `.FeedTitle` | The feed's own title (RSS/RDF `<channel><title>`, Atom `<feed><title>`; HTML stripped), useful when one webhook carries several feeds |

`.PublishedUnix` is meant for Discord's dynamic timestamps, which render in each reader's own time zone: `<t:{{.PublishedUnix}}:R>` shows "3 hours ago". An entry without a date has `.PublishedUnix` of `0`, which Discord would render as 1970, so guard it:

```YAML
{{if .PublishedUnix}}Posted <t:{{.PublishedUnix}}:R>{{end}}
```

For example, `[{{.FeedTitle}}] **{{.Title}}**` prefixes each message with the feed it came from.

### Template functions

Templates can also call a few helpers, written pipeline-style:

| Function | Description |
|---|---|
| `truncate N` | Cut to at most N characters, ending in `…` when it cuts: `{{.Description \| truncate 200}}` |
| `default "x"` | Use `x` when the value is blank: `{{.Author \| default "unknown"}}` |
| `lower`, `upper`, `trim` | Case conversion and whitespace trimming |
| `replace "old" "new"` | Replace every occurrence: `{{.Title \| replace "[PR] " ""}}` |
| `discordTime "R"` | Discord dynamic timestamp from `.PublishedUnix`; styles are `t T d D f F R`. Renders empty for an undated entry: `{{.PublishedUnix \| discordTime "R"}}` |

An unknown function is a config error; an invalid `discordTime` style fails that entry's render.
A `replace` result larger than 1 MiB also fails that entry's render, including intermediate pipeline values.

### Webhook URLs

Webhook URLs copied from Discord's PTB or Canary clients (`ptb.discord.com`, `canary.discord.com`) are accepted and sent to `discord.com`; only Discord's own hosts are ever contacted.

### Forum channels

If `discordWebhookSecretRef` points at a webhook created for a forum channel, set `forumThreadName` on a feed to create a new forum post per entry (templated, same placeholders as `format`), or `forumThreadID` to post into one specific existing thread instead.

## Development

Tooling is managed by [mise](https://mise.jdx.dev) — it pins every tool version (Go, golangci-lint, controller-gen, kustomize, helm, kind, etc.) in [`.mise/config.toml`](.mise/config.toml), so local development matches CI exactly. Docker is the only host prerequisite.

```bash
curl https://mise.run | sh   # one-time: install mise
mise install                 # install the pinned toolchain
mise tasks                   # list available tasks
```

```bash
mise run build       # build the manager binary
mise run test        # unit tests
mise run lint        # lint
mise run lint-fix    # lint with autofix
mise run test-e2e    # e2e tests, needs a Kind cluster
```

Releases are managed by [release-please](https://github.com/googleapis/release-please), which reads the **header line** of each commit on `main` to decide what goes in the changelog. Squash-merging a PR uses the PR title as that header — so when bundling a real fix inside an otherwise-`chore`-typed PR (e.g. a Renovate dependency bump), give the PR a `fix:`-prefixed title, not the bot's default, or the fix silently won't be picked up.

After changing CRD types or RBAC markers:

```bash
mise run manifests   # regenerate CRDs and RBAC
mise run generate    # regenerate DeepCopy methods
```

Build and push an image:

```bash
export IMG=my-registry/rss2discord-operator:v0.1.0
mise run docker-build
mise run docker-push
```

## Project layout

```
api/v1alpha1/          FeedGroup CRD types
internal/controller/   Reconciliation logic
internal/discord/      Discord webhook client
internal/rss/          RSS feed client
config/                Kubernetes manifests (CRDs, RBAC, kustomize)
dist/                  Generated install.yaml and Helm chart
test/e2e/              End-to-end tests
cmd/main.go            Entry point
```

## Helm chart

Located in `dist/chart/`. Key values in `dist/chart/values.yaml`:

```yaml
manager:
  image:
    repository: ghcr.io/maverickd650-labs/rss2discord
    # tag: ""   # defaults to the chart's appVersion
  # maxConcurrentReconciles: 1   # --max-concurrent-feedgroup-reconciles
  # otlpEndpoint: ""             # --otlp-endpoint (tracing off when empty)
  # logLevel: ""                 # --zap-log-level: debug, info, error or an integer
  resources:
    limits:
      cpu: 500m
      memory: 256Mi
    requests:
      cpu: 10m
      memory: 64Mi
```

One reconcile can buffer up to 10 feed responses of 10MB each, so scale `resources.limits.memory` with `maxConcurrentReconciles` and the size of your feeds (`GOMEMLIMIT` follows the limit). Responses over 10MB are rejected, and only the first 1000 entries of a feed (the newest, since feeds list newest-first) are read.

Resource names drop the redundant `-controller-manager` suffix — with a release named `rss2discord-operator`, the Deployment, ServiceAccount, and Service are all named `rss2discord-operator` (selection still happens on the `control-plane: controller-manager` label). The `kubectl apply -f dist/install.yaml` path keeps the longer `rss2discord-operator-controller-manager` names, so prefer the label selector in commands that target the pod.

### Restricting the operator to some namespaces

By default the operator gets a ClusterRole: it watches FeedGroups in every namespace and may `get` Secrets cluster-wide, by name only (it reads just the webhook Secret a FeedGroup names). It has no `list` or `watch` on Secrets, so a stolen token cannot enumerate them, and it does not watch webhook Secrets: a replaced webhook URL is picked up at the feed's next scheduled reconcile rather than at once. **For tenant deployments, install it namespace-scoped explicitly**, listing the namespaces it serves:

```bash
helm install rss2discord-operator ./dist/chart \
  --namespace rss2discord-operator-system --create-namespace \
  --set 'rbac.watchNamespaces={team-a,team-b}'
```

The chart then grants the manager's permissions as a Role and RoleBinding in each listed namespace only (no ClusterRole) and starts it with `--watch-namespaces=team-a,team-b`, so the watch scope and the grant always match. A namespace-scoped install also gets `list` and `watch` on Secrets in those namespaces and runs with `--watch-webhook-secrets`, so a webhook Secret change retries its feeds immediately. Cluster-wide list/watch on Secrets is available only by opting in with `rbac.clusterWideSecretWatch: true`, which lets the service account read every Secret's data in the cluster. FeedGroups in other namespaces are ignored. Blank entries are ignored and every other entry must be a valid namespace name; an invalid entry, or a list with only blank entries, fails the install rather than falling back to cluster-wide. (Leaving `rbac.watchNamespaces` empty, the default, is the cluster-wide install.) `rbac.clusterWideSecretWatch` must be a boolean: a string such as `--set-string rbac.clusterWideSecretWatch=false` fails the install. `rbac.namespaced: true` is shorthand for the release namespace alone.

**The RBAC grant is the credential boundary, not `--watch-namespaces`.** The flag only limits what the operator watches; what a stolen service-account token can read is whatever its Roles and ClusterRoles allow. Check the grant itself:

```bash
sa=system:serviceaccount:rss2discord-operator-system:rss2discord-operator
kubectl auth can-i list secrets --as="$sa" -n team-a          # yes (namespace-scoped install)
kubectl auth can-i list secrets --as="$sa" -n some-other-ns   # no
kubectl auth can-i list secrets --as="$sa" --all-namespaces   # no
```

In namespace-scoped mode the chart still installs two cluster-scoped roles, neither of which reaches Secrets: `metrics-auth-role` (create TokenReviews and SubjectAccessReviews, for authenticating metrics scrapes) and `metrics-reader` (`get` on the `/metrics` URL, bound to no one by the chart).

**Narrowing an existing installation:** `helm upgrade` with `rbac.watchNamespaces` set deletes the release's cluster-wide `manager-role` ClusterRole and ClusterRoleBinding, because they are no longer in the rendered release. A chart test (`TestChartUpgradeToWatchNamespacesRevokesClusterWideSecretAccess`) runs exactly this upgrade, from `rbac.clusterWideSecretWatch: true`, against a real API server and checks with SubjectAccessReviews that the account can no longer read Secrets outside the listed namespaces. Run the `kubectl auth can-i` checks above after the upgrade anyway, and remove any binding to the service account that was created outside the chart. With `kubectl apply -f dist/install.yaml` nothing is pruned: replace the `manager-role` ClusterRole/ClusterRoleBinding with per-namespace Role/RoleBindings, pass `--watch-namespaces`, and delete the cluster-wide pair yourself (`kubectl delete clusterrolebinding rss2discord-operator-manager-rolebinding` and `kubectl delete clusterrole rss2discord-operator-manager-role`).

Common commands:

```bash
helm status rss2discord-operator -n rss2discord-operator-system
helm history rss2discord-operator -n rss2discord-operator-system
helm rollback rss2discord-operator -n rss2discord-operator-system
helm uninstall rss2discord-operator -n rss2discord-operator-system
```

### Upgrades and rollbacks

Each feed's status records the format of its delivery state in `status.feeds[].stateVersion`. This release no longer reads feed statuses written before `stateVersion` 1 (releases before 0.14), so upgrade through 0.14.x or 0.15.x first. Before upgrading past that point, confirm that no feed is still on the old format; an empty output with exit status 0 means the cluster is safe to upgrade:

```bash
set -o pipefail
kubectl get feedgroups -A -o json | jq -r '
  .items[] | . as $g | .status.feeds[]? | select((.stateVersion // 0) < 1)
  | "\($g.metadata.namespace)/\($g.metadata.name)  \(.rssUrl)"'
```

A feed that does appear there still holds delivery state in the old format, which this release no longer migrates, so it would resend entries whose stored keys no longer match. The check is conservative: a feed that has never settled a sync since upgrading to 0.14 or later (for example, one whose fetch keeps failing) also keeps `stateVersion` 0 and is listed. Let it complete a successful sync on 0.15.x, or confirm it holds no delivery state, before upgrading.

A release before 0.14 drops `stateVersion` on its status writes, so after rolling back that far, upgrade through 0.15.x again before this release.

Rolling back to a release that migrates `stateVersion` 0 statuses (0.14.x or 0.15.x) is safe, but feeds without GUIDs may post a few duplicates:

- **Entries with a GUID** are identified the same way by every release, so they are not resent.
- **Entries without a GUID whose link has a `#fragment` or `?ref=` parameter** are identified by the older release without them. It doesn't recognise the newer watermark or keys for these entries, so on its first fetch it resends up to `catchUpLimit` of the newest ones (5 when `catchUpLimit` is 0, as older releases treat it). It also goes back to treating links that differ only by fragment or `ref` as one entry.
- **Entries with no GUID, link or title** all look the same to an older release, so at most one of them is resent.

## Observability

The controller exports these Prometheus metrics, all labeled by `namespace` and `name` (the FeedGroup):

| Metric | Type | Extra labels | What it tells you |
| --- | --- | --- | --- |
| `rss2discord_feed_operations_total` | counter | `rss_url`, `outcome` (`sent`, `fetch_error`, `send_error`, `render_error`, `rate_limited`) | Send-success ratios and a per-feed error breakdown (which feed in the group, and why) |
| `rss2discord_feed_request_duration_seconds` | histogram | `operation` (`fetch`, `send`) | Latency of the operator's outbound RSS fetches and Discord sends (e.g. a feed host hanging up to its timeout) |
| `rss2discord_feedgroup_reconcile_duration_seconds` | histogram | — | Wall-clock time of a full reconcile (every feed's fetch and send), so a FeedGroup creeping toward its requeue interval shows up before it starts missing it |
| `rss2discord_message_overflow_chars` | histogram | — | Characters trimmed from a rendered Discord message before it was clamped to fit Discord's length limits; only recorded on actual overflow, so `histogram_count(rate(rss2discord_message_overflow_chars[5m]))` answers "how often does this FeedGroup's content get cut off" |
| `rss2discord_feed_last_success_timestamp_seconds` | gauge | — | Unix time of the last successful check of this FeedGroup's feeds (a 304 or a fetch with nothing new still counts); group-level, so one healthy feed keeps it fresh while a sibling is dead (use the per-feed gauges below for that) |
| `rss2discord_feed_healthy` | gauge | `rss_url` | Per feed: `1` if the feed's persisted status has no `lastError`, else `0`. Read from status on every reconcile, so a feed in permanent backoff (retried only every 10m ... 6h, error counter flat in between) keeps reporting `0`, and it is populated right after an operator restart. **Paused feeds have no series** |
| `rss2discord_feed_last_checked_timestamp_seconds` | gauge | `rss_url` | Per feed: Unix time of the last successful check (`status.feeds[].lastChecked`). **Absent for a feed that has never been checked, and for paused feeds**, rather than exported as `0` |

Series for a FeedGroup are removed from the registry when that FeedGroup is deleted, so a removed group can't leave a stale freshness reading behind.

All three histograms (`rss2discord_feed_request_duration_seconds`, `rss2discord_feedgroup_reconcile_duration_seconds`, `rss2discord_message_overflow_chars`) are exposed as **native-only** histograms -- no classic `le` buckets, just the native exponential representation (`NativeHistogramBucketFactor` set, `Buckets` left unset in [metrics.go](internal/controller/metrics.go)). This keeps the bundled dashboard from carrying duplicate classic + native panels for the same data, but it means there's no `_bucket` fallback: if the scrape doesn't carry the native representation, these three metrics expose only `_count`/`_sum` with no `histogram_quantile` support at all.

To actually get resolution out of these metrics:

- **`prometheus.scrapeNativeHistograms` must stay `true`** (the chart default, requires `prometheus.enabled`). It has the `ServiceMonitor` set `scrapeProtocols`/`scrapeClassicHistograms`/`scrapeNativeHistograms` on the `ServiceMonitor`'s `spec` (top-level `ServiceMonitorSpec` fields, not per-endpoint) so Prometheus negotiates protobuf and pulls the native representation. Setting it `false` doesn't fall back to classic buckets for these metrics -- there are none -- it just stops scraping resolution for them.
- **Prometheus must support it too.** Native histograms require Prometheus >= v2.45, and are only stable (no feature flag needed) since v3.8; on v2.45–v3.7 you also need to start Prometheus with `--enable-feature=native-histograms`.
- **Query it directly by metric name** — there's no `_bucket` suffix or `le` label to sum over. `histogram_quantile(0.95, rate(rss2discord_feed_request_duration_seconds[5m]))` against the bare metric name gives you the p95.
- **Grafana needs a recent-enough version** (>= v9.4, ideally >= v11 for heatmap support) with the Prometheus data source's native histogram support enabled to render the dashboard's "Performance" row; older Grafana will show those panels empty rather than erroring.

The metrics endpoint is enabled by default (`metrics.enabled`, served on `:8443`). The remaining pieces are opt-in via chart values and each requires the relevant operator to be installed in the cluster:

| Value | Default | What it does |
| --- | --- | --- |
| `prometheus.enabled` | `false` | Installs a `ServiceMonitor` so prometheus-operator scrapes the metrics endpoint |
| `prometheus.scrapeNativeHistograms` | `true` | With `prometheus.enabled`, has the `ServiceMonitor` negotiate protobuf so Prometheus actually gets resolution out of the operator's native-only histograms (`feed_request_duration_seconds`, `feedgroup_reconcile_duration_seconds`, `message_overflow_chars`); without it these three metrics expose `_count`/`_sum` only |
| `prometheusRule.enabled` | `false` | Installs a `PrometheusRule` alerting on sustained `fetch_error` / `send_error` / `rate_limited` per feed (annotations name the failing feed URL and, for fetch/send errors, the failure reason). Also alerts per feed on state rather than error rate, so these keep firing during permanent backoff: `RSS2DiscordFeedUnhealthy` (`rss2discord_feed_healthy == 0` for `prometheusRule.unhealthyFor`, default `30m`) and `RSS2DiscordFeedStale` (last successful check older than `prometheusRule.staleAfterSeconds` **seconds**, default `21600` = 6h; keep it above your largest FeedGroup `interval` and the 6h maximum permanent backoff). Tune with `prometheusRule.rateInterval`, `.for`, `.unhealthyFor`, `.staleAfterSeconds`, and `.severity` |
| `grafanaDashboard.enabled` | `false` | Ships a ConfigMap holding the dashboard JSON plus a `GrafanaDashboard` custom resource for the [grafana-operator](https://github.com/grafana/grafana-operator) (referencing it via `configMapRef`), with rows for executive summary (success rate, failing/stale feed counts), service health, action-required triage (failing FeedGroups, top erroring feeds), feed freshness, performance (fetch/send latency p50/p95/p99 + heatmap, reconcile duration, message overflow size -- all native histogram panels), and operator (controller-runtime/workqueue) health. Target a specific Grafana instance with `grafanaDashboard.instanceSelector`; the dashboard's `${DS_PROMETHEUS}` placeholder is resolved to a real datasource via `grafanaDashboard.datasources` |

```bash
helm install rss2discord-operator ./dist/chart \
  --namespace rss2discord-operator-system --create-namespace \
  --set prometheus.enabled=true \
  --set prometheusRule.enabled=true \
  --set grafanaDashboard.enabled=true
```

`grafanaDashboard.enabled=true` on its own assumes:

- the [grafana-operator](https://github.com/grafana/grafana-operator) is installed in the cluster (its `GrafanaDashboard` CRD must exist), and
- it manages a `Grafana` CR with a datasource named `prometheus` in it.

If either isn't true, override these on install:

```bash
helm install rss2discord-operator ./dist/chart \
  --namespace rss2discord-operator-system --create-namespace \
  --set grafanaDashboard.enabled=true \
  --set grafanaDashboard.instanceSelector.matchLabels.dashboards=grafana \
  --set 'grafanaDashboard.datasources[0].inputName=DS_PROMETHEUS' \
  --set 'grafanaDashboard.datasources[0].datasourceName=my-prometheus-datasource-name'
```

- `grafanaDashboard.instanceSelector` must match the labels on the `Grafana` CR(s) you want this dashboard synced into — left empty (the default), it matches every `Grafana` CR the operator manages, which is fine with a single instance.
- `grafanaDashboard.datasources` resolves the dashboard's `${DS_PROMETHEUS}` placeholder to a real datasource: the operator does a literal text substitution of `${inputName}` → `datasourceName` in the dashboard JSON before pushing it to Grafana, so `datasourceName` must be the **name** of an existing datasource in your Grafana instance, not its UID.
- If your `Grafana` CR lives in a different namespace than this release, `grafanaDashboard.allowCrossNamespaceImport` (default `true`) is what lets the operator's `GrafanaDashboard` controller, running wherever it runs, pick up the `ConfigMap`/CR pair from this release's namespace.

The dashboard JSON lives at [`dist/chart/dashboards/feedgroup-overview.json`](dist/chart/dashboards/feedgroup-overview.json) if you'd rather import it manually.

## Troubleshooting

Check logs:

```bash
kubectl logs -n rss2discord-operator-system -l control-plane=controller-manager -f
```

Check FeedGroup status:

```bash
kubectl describe feedgroup my-feedgroup -n default
kubectl get feedgroup my-feedgroup -n default -o yaml
```

Common issues:

- `LastError: webhook - discord webhook URL is empty` — the secret or key in `discordWebhookSecretRef` is wrong.
- FeedGroup not updating — check `interval` and the operator logs.
- Messages formatted wrong — check template placeholders against the [available fields](#message-template).
- A feed that keeps returning a "permanent" error (e.g. HTTP 404/403) is retried with exponential backoff capped at 6h, so it recovers on its own once the source is back; editing the spec retries it immediately.
- A deleted or regenerated Discord webhook (HTTP 404 on send) backs the feed off the same way. Putting the new webhook URL in the Secret named by `discordWebhookSecretRef` clears its send backoff: any update to that Secret, including one to its data only, changes its `resourceVersion`, which the operator compares on every reconcile. With `--watch-webhook-secrets` (on for a namespace-scoped install, or with `rbac.clusterWideSecretWatch`) it watches the Secret's metadata (never its data) and retries immediately; otherwise the change is noticed at the next scheduled reconcile.
- An entry that can't be rendered is skipped (Warning Event `EntrySkipped`, `skipped` metric outcome) once the group's `retries` are exhausted, so it doesn't block newer entries — but only if other entries in the feed do render. If the template fails for every entry, they are held until you fix it.

## References

- [Kubebuilder Documentation](https://book.kubebuilder.io)
- [Operator Pattern](https://kubernetes.io/docs/concepts/extend-kubernetes/operator/)
- [Discord Webhook Documentation](https://discord.com/developers/docs/resources/webhook)

## License

Apache License, Version 2.0. See [LICENSE](LICENSE).

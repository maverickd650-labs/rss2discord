# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A Kubernetes operator (kubebuilder / controller-runtime, Go) that watches RSS/Atom feeds
described by `FeedGroup` custom resources and posts new entries to Discord via incoming
webhooks. Single API group, single kind: `FeedGroup` (`rss2discord.maverickd650.dev/v1alpha1`).

`AGENTS.md` is the detailed companion guide — it covers kubebuilder/CLI scaffolding workflows,
API/controller marker conventions, and the project's hardening rationale in depth. Read it
when scaffolding new APIs/webhooks or touching the security-sensitive paths below.

## Commands

Tasks are defined in `.mise/config.toml` and run via `mise run <task>`. **This is the single
source of truth — CI installs the pinned tool versions with `mise` and runs the same tasks, so
local == CI.** Do not invoke `go test`/`golangci-lint` directly; use the tasks (they wire up
codegen, envtest assets, and the custom lint binary).

- `mise run test` — unit tests (runs `manifests`, `generate`, `fmt`, `vet` first; uses envtest = real K8s API server + etcd; `-race -coverprofile cover.out`). Excludes `/e2e`.
- `mise run test-e2e` — e2e tests against a throwaway Kind cluster (created/deleted automatically; never run against a real cluster).
- `mise run lint` / `lint-fix` — golangci-lint. Note: this project builds a **custom** golangci-lint binary (`.custom-gcl.yml` adds the `logcheck` module plugin), so the `lint` task depends on `golangci-lint-custom` which produces `bin/golangci-lint-custom`.
- `mise run build` — build manager binary to `bin/manager`.
- `mise run run` — run the controller locally against the current kubeconfig context.
- `mise run manifests` — regenerate CRDs + RBAC from `+kubebuilder` markers.
- `mise run generate` — regenerate `zz_generated.deepcopy.go` and the server-side-apply types in `api/v1alpha1/applyconfiguration/`.
- `mise run helm-chart-refresh` — regenerate `dist/chart/` from `config/` (kubebuilder's `helm/v2-alpha` plugin), preserving this chart's hand-tuned templates. Run after CRD/RBAC/manager changes; see AGENTS.md for what it preserves and why.

Run a single test (after `mise run manifests generate` so codegen is current):
```bash
go test ./internal/controller/ -run TestName -v          # one Go test
go test ./internal/controller/ -run TestX -args -ginkgo.focus="substring"   # focus a Ginkgo spec (suite_test.go uses Ginkgo+Gomega)
```

After editing `*_types.go` or markers, run `mise run manifests generate`. After editing any
`*.go`, run `mise run lint-fix` and `mise run test`.

## Architecture

The reconcile loop is: for each `FeedGroup`, fetch every feed → filter → template/render →
send to Discord → record per-feed delivery state in `FeedGroup.Status`. Three internal packages:

- `internal/rss` (`client.go`) — fetches and parses feeds. Conditional GET via ETag/Last-Modified, response-size cap. An Atom document's vocabulary is decided once from its root namespace: in Atom 1.0 the identity/link/date elements are matched by namespace (`atomEntry`) so extension `<ext:id>`/`<ext:link>` can't displace them; other documents are read by local name (`looseAtomEntry`). When this changes an entry's ID versus earlier releases the parser reports it as `Entry.PreviousID`, and the controller (`migratePreviousIdentities`) carries stored identities over.
- `internal/discord` (`client.go`) — builds and sends webhook messages; rate-limit/error handling, `@everyone`/`@here` suppression via `allowed_mentions`, length clamping. Non-http(s) link/image stripping (`javascript:`/`data:`) lives in `internal/controller/render.go` (`httpURLOrEmpty`), not here. Message templates reject `range`/`define`/`template`/`with`, variable assignment and string-building builtins (`printf`, `html`, ...), and cap output size.
- `internal/controller` — `FeedGroupReconciler` (`feedgroup_controller.go`) orchestrates everything, plus `feed_sync.go` (a `feedSync` applies one fetch to a `FeedStatus`: it owns the order of the steps and the single settlement point), `attempt.go` (a `feedAttempt` owns outcome → metric/condition/Event/retry), `metrics.go` (Prometheus) and `classify.go` (error → outcome classification).

Cross-cutting designs that span multiple files (change these carefully):

- **SSRF guards — do not weaken or duplicate.** RSS URLs and the webhook secret are user-supplied. `internal/rss/client.go`'s `newDefaultHTTPClient` uses a custom `DialContext` that resolves the host and rejects any non-public IP (loopback, link-local, private, unspecified, multicast, CGNAT) via `isPublicIP`. `internal/discord/client.go` only sends to HTTPS hosts in `AllowedWebhookHosts` (Discord domains). Tests that need to hit local `httptest` servers use the unguarded helpers in `internal/controller/*_test.go` (`testRSSClient()`, registering into `discord.AllowedWebhookHosts`) — never relax the production guards for tests. (A `security-reviewer` subagent guards these two files.)

- **A fetch is applied by a `feedSync`, and settled in exactly one place.** `processFeed` opens one (`openFeedSync`), calls `begin` once the feed's render config is known good, and closes it exactly once with a `syncVerdict` (`close` panics on a second close or a verdict that doesn't fit — `processFeed` closes via `defer`). `begin` owns the order: stamp title → stable sort → legacy + previous-identity migrations → delivery ledger over the *untrimmed* fetch → watermark lookup → `planDelivery` (catch-up limit, backlog skip). `close` is the only code that sets `Initialized`, the legacy `StateVersion`, persists ETag/Last-Modified, clears `LastError`/`RetryCount`/`BackoffUntil` and prunes `LastSent`. Don't settle anywhere else, and don't call `begin` before the config check: a config error must leave the status untouched. Terms are in `GLOSSARY.md`.
- **Per-feed status is a slice, not maps.** `FeedGroup.Status.Feeds []FeedStatus` (keyed by `rssUrl`); each struct holds `LastChecked`/`LastSeenEntry`/`LastSent`/`LastError`/`ETag`/`LastModified`/`RetryCount`/`Conditions` together. `ensureFeedStatuses` rebuilds the slice every reconcile (in `Spec.Feeds` order), initializing new feeds and pruning removed ones in one pass; `feedStatusFor` looks one up by URL. New per-feed state goes on `FeedStatus`, not a new parallel map.

- **Status is written with server-side apply.** `applyStatus` converts `FeedGroup.Status` to the generated `api/v1alpha1/applyconfiguration` types and calls `Status().Apply` as field owner `feedgroup-controller`. After changing `FeedGroupStatus`/`FeedStatus`, run `mise run generate` — `TestStatusApplyConfigurationCoversEveryField` fails if the apply configuration is stale (a missing field would otherwise be silently dropped from every status write).

- **Metrics use a single outcome label.** Every fetch/send attempt increments `feedOperationsTotal` (only in `feedAttempt.record`, via `attemptOutcome.metricLabel`) with exactly one `outcome` (`outcomeSent`, `outcomeFetchError`, `outcomeSendError`, `outcomeRenderError`, `outcomeRateLimited`). `fetch_error`/`send_error` are sub-classified by cause in `classify.go` (e.g. `fetch_error_not_found`) — `outcomeFetchError`/`outcomeSendError` are only ever label *prefixes*, never recorded raw. The Grafana dashboard (`dist/chart/dashboards/feedgroup-overview.json`) and PrometheusRule alerts match these with anchored regexes, so a new outcome usually needs a dashboard panel/alert too.

- **Failure classification → status conditions.** `classify.go` maps an error to a `failureClass`: a Prometheus-safe `metricReason`, a CamelCase `conditionReason` (e.g. `HTTP404`), and a `permanent` flag. `conditionReason` becomes the `Reason` on a feed's `Reachable` (fetch) or `Delivered` (render/send) condition; the group-level `FeedReachable` condition summarizes the most common failure — so `kubectl get feedgroup -o yaml` explains *why* a feed is down. Persistent-failure Events fire exactly once — for a transient failure when `RetryCount` first reaches the retry limit (`==`, not `>=`), for a permanent fetch/send failure (which backs off immediately) when its backoff first hits the cap — so an ongoing failure doesn't re-fire every reconcile. That retry policy is written once, in `feedAttempt.record` (`attempt.go`), which every fetch/render/send/skip/config outcome goes through; it returns an `attemptVerdict` (`retry`, `retryAfter`, `pending`) that `Reconcile` folds across feeds and `processFeed` turns into the `feedSync` close verdict (`pending` → `syncPending`). Don't increment the metric, set `Reachable`/`Delivered`, touch `LastError`/`RetryCount`/`BackoffUntil` or emit these Events anywhere else.

- **`dist/chart/` has hand-tuned files `mise run helm-chart-refresh` knows to preserve.** `templates/manager/manager.yaml`, `templates/metrics/controller-manager-metrics-service.yaml`, and `templates/prometheus/controller-manager-metrics-monitor.yaml` carry shortened resource-name suffixes and (the ServiceMonitor) the native-histogram `scrapeProtocols`/`scrapeNativeHistograms` block; `templates/rbac/manager-role.yaml` and `manager-rolebinding.yaml` render a Role/RoleBinding per `rbac.watchNamespaces` entry (with `manager.yaml` passing the same `--watch-namespaces`, plus `--watch-webhook-secrets` and a chart-only list/watch Secrets rule when namespace-scoped or `rbac.clusterWideSecretWatch` is set), so after changing `+kubebuilder:rbac` markers copy the new rules from `config/rbac/role.yaml` into `manager-role.yaml` by hand (`TestChartManagerRoleRulesMatchConfig` fails until you do); `values.yaml` and `templates/_helpers.tpl` carry the `prometheusRule`/`grafanaDashboard`/`prometheus.scrapeNativeHistograms` values and the `controllerManagerName` helper; `templates/prometheus/grafana-dashboard.yaml`, `templates/prometheus/prometheus-rule.yaml`, and `dashboards/*.json` are wholly custom (no kubebuilder equivalent). Hand-editing `dist/chart/` directly is fine, but don't follow it with a raw `kubebuilder edit --plugins=helm/v2-alpha --force` — that regenerates `values.yaml`/`_helpers.tpl` from scratch and discards those edits. Use `mise run helm-chart-refresh` instead.

## Do not edit (auto-generated)

`config/crd/bases/*.yaml`, `config/rbac/role.yaml`, `**/zz_generated.*.go`, `api/*/applyconfiguration/**` (from `mise run manifests`/`generate`), and `PROJECT` (kubebuilder). Never delete `// +kubebuilder:scaffold:*` markers. Use `kubebuilder create api`/`create webhook` to scaffold — don't hand-create those files. Edit `config/samples/*` (example CRs) freely.

## Never write bare cross-repo issue/PR references

Don't write `owner/repo#123` (e.g. `kubernetes-sigs/kubebuilder#4809`) anywhere that becomes a commit
message, PR title/body, or comment in this repo — not even when citing an upstream issue as
background/rationale. GitHub auto-links that exact syntax and creates a "mentioned this issue"
notification on the *other* repo, pinging its maintainers about something they have no connection
to and didn't consent to. This isn't hypothetical: it happened once already (a PR description cited
an upstream kubebuilder issue this way, got squash-merged into `main`, and had to be scrubbed via a
history rewrite — see PR for this fix).

If you need to cite an external issue/PR, write it so GitHub won't autolink it: use the full URL
(`https://github.com/owner/repo/issues/123`) or break the pattern (`owner/repo issue 123`), never the
bare `owner/repo#123` shorthand.

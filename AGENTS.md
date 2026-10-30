# rss2discord-operator - AI Agent Guide

## Project Structure

**Single-group layout (default):**
```
cmd/main.go                    Manager entry (registers controllers/webhooks)
api/<version>/*_types.go       CRD schemas (+kubebuilder markers)
api/<version>/zz_generated.*   Auto-generated (DO NOT EDIT)
api/<version>/applyconfiguration/  Generated SSA ApplyConfigurations (DO NOT EDIT)
internal/controller/*          Reconciliation logic
internal/webhook/*             Validation/defaulting (if present)
config/crd/bases/*             Generated CRDs (DO NOT EDIT)
config/rbac/role.yaml          Generated RBAC (DO NOT EDIT)
config/samples/*               Example CRs (edit these)
.mise/config.toml              Tool versions + dev tasks (run via `mise run <task>`)
PROJECT                        Kubebuilder metadata Auto-generated (DO NOT EDIT)
```

**Multi-group layout** (for projects with multiple API groups):
```
api/<group>/<version>/*_types.go       CRD schemas by group
internal/controller/<group>/*          Controllers by group
internal/webhook/<group>/<version>/*   Webhooks by group and version (if present)
```

Multi-group layout organizes APIs by group name (e.g., `batch`, `apps`). Check the `PROJECT` file for `multigroup: true`.

**To convert to multi-group layout:**
1. Run: `kubebuilder edit --multigroup=true`
2. Move APIs: `mkdir -p api/<group> && mv api/<version> api/<group>/`
3. Move controllers: `mkdir -p internal/controller/<group> && mv internal/controller/*.go internal/controller/<group>/`
4. Move webhooks (if present): `mkdir -p internal/webhook/<group> && mv internal/webhook/<version> internal/webhook/<group>/`
5. Update import paths in all files
6. Fix `path` in `PROJECT` file for each resource
7. Update test suite CRD paths (add one more `..` to relative paths)

## This Project

rss2discord-operator watches RSS/Atom feeds described by `FeedGroup` custom resources and posts new entries to Discord via incoming webhooks. Each reconcile fetches every feed in a group, filters/templates/renders new entries into Discord messages, and tracks per-feed-URL delivery state (`LastSeenEntry`, `LastSent`, `RetryCount`, conditional-GET validators, etc.) in `FeedGroup.Status`.

Three internal packages do the real work:
- `internal/rss` — fetches and parses feeds (conditional GET via ETag/Last-Modified, response-size cap).
- `internal/discord` — sends webhook messages (message building, rate-limit/error handling, content sanitization).
- `internal/controller` — the `FeedGroupReconciler`: orchestrates fetch → filter → template → send → status for each `FeedGroup`, plus the Prometheus metrics in `metrics.go`.

**SSRF guards — do not weaken or duplicate these.** Both outbound paths are deliberately locked down because `FeedGroup.Spec` (RSS URLs) and the webhook secret are user-supplied input that could otherwise be used to probe internal networks:
- `internal/rss/client.go`'s `newDefaultHTTPClient` dials through a custom `DialContext` that resolves the host and checks every resulting IP with `isPublicIP`, rejecting loopback, link-local, private, unspecified, multicast, and CGNAT ranges before connecting.
- `internal/discord/client.go` only sends to hosts in `AllowedWebhookHosts` (Discord's own webhook domains) over HTTPS — this blocks both SSRF and webhook-URL domain confusion.
- Tests that need to hit local `httptest` servers must use the unguarded client/host registration helpers in `internal/controller/feedgroup_controller_test.go` (`testRSSClient()`, `discord.AllowedWebhookHosts[...] = true`) rather than weakening the production guards.

**Metrics use an outcome-label model**, defined in `internal/controller/metrics.go`: every feed fetch/send attempt increments `feedOperationsTotal` with the feed's `rss_url` (one FeedGroup can have up to 50 feeds, so namespace/name alone can't identify which one) and exactly one `outcome` label (`outcomeSent`, `outcomeFetchError`, `outcomeSendError`, `outcomeRenderError`, `outcomeRateLimited`). `fetch_error`/`send_error` are further diversified by failure cause via `fetchErrorOutcome`/`sendErrorOutcome` in `internal/controller/classify.go` (e.g. `fetch_error_not_found`, `send_error_timeout`) — `outcomeFetchError`/`outcomeSendError` themselves are only ever used as that label's prefix, never recorded directly. Adding a new failure mode means adding a new `failureClass` (or `outcome*` constant, for a failure mode outside the fetch/send classification) and a call at the point it's detected — the Grafana dashboard (`dist/chart/dashboards/feedgroup-overview.json`) and the PrometheusRule alerts both key off these labels with `outcome=~"fetch_error_.+|..."` (fully anchored, since Prometheus label regex matches the whole value), so a new outcome should usually get a corresponding dashboard panel/alert too. The PrometheusRule's fetch/send-error alerts extract a human-readable `reason` label from the diversified `outcome` value via `label_replace` (e.g. `fetch_error_not_found` → `reason="not_found"`), so the alert annotation can name both the failing feed and why, without inventing a second metric label that would duplicate `outcome`.

**Failure classification** (`internal/controller/classify.go`) maps a fetch/send error to a `failureClass`: a Prometheus-safe `metricReason`, a CamelCase status condition `conditionReason` (e.g. `HTTP404`), and whether it's `permanent` (won't resolve by itself). `conditionReason` becomes the `Reason` on that feed's `Reachable` (fetch) or `Delivered` (render/send) condition in `FeedGroup.Status.Feeds[i].Conditions`, and the group-level `FeedReachable` condition summarizes the most common `Reachable` failure across feeds -- so `kubectl get feedgroup -o yaml` shows *why* a feed is down without a log dive. Persistent-failure Events (`r.recordPersistentFailure`) fire exactly once: for a transient failure, when `RetryCount` first reaches the feed's retry limit (`==`, not `>=`/`<`); for a permanent one (fetch or send alike), which backs off exponentially from the first failure without spending retries or setting `wantRetry`, when that backoff first reaches its 6h cap (`applyPermanentBackoff`). Either way, a feed stuck on the same ongoing failure (e.g. a permanent 404) must not re-fire the Event every reconcile thereafter.

**Per-feed status lives in `FeedGroup.Status.Feeds []FeedStatus`** (keyed by `rssUrl`), not in parallel `map[string]...` fields on `FeedGroupStatus` -- one struct per feed holds its `LastChecked`/`LastSeenEntry`/`LastSent`/`LastError`/`ETag`/`LastModified`/`RetryCount`/`Conditions` together. `internal/controller/feedgroup_controller.go`'s `ensureFeedStatuses` rebuilds this slice every reconcile (in `Spec.Feeds` order) from the previous slice, which both initializes new feeds and prunes removed ones in one pass; `feedStatusFor` looks up a single feed's entry by URL. Adding a new per-feed status field means adding it to `FeedStatus`, not adding a new map.

## Critical Rules

### Never Edit These (Auto-Generated)
- `config/crd/bases/*.yaml` - from `mise run manifests`
- `config/rbac/role.yaml` - from `mise run manifests`
- `config/webhook/manifests.yaml` - from `mise run manifests`
- `**/zz_generated.*.go` - from `mise run generate`
- `api/*/applyconfiguration/**` - from `mise run generate` (server-side-apply types used by `applyStatus`)
- `PROJECT` - from `kubebuilder [OPTIONS]`

### Never Remove Scaffold Markers
Do NOT delete `// +kubebuilder:scaffold:*` comments. CLI injects code at these markers.

### Keep Project Structure
Do not move files around. The CLI expects files in specific locations.

### Always Use CLI Commands
Always use `kubebuilder create api` and `kubebuilder create webhook` to scaffold. Do NOT create files manually.

### E2E Tests Require an Isolated Kind Cluster
The e2e tests are designed to validate the solution in an isolated environment (similar to GitHub Actions CI).
Ensure you run them against a dedicated [Kind](https://kind.sigs.k8s.io/) cluster (not your “real” dev/prod cluster).

## After Making Changes

**After editing `*_types.go` or markers:**
```
mise run manifests  # Regenerate CRDs/RBAC from markers
mise run generate   # Regenerate DeepCopy methods
```

**After editing `*.go` files:**
```
mise run lint-fix   # Auto-fix code style
mise run test       # Run unit tests
```

## CLI Commands Cheat Sheet

### Create API (your own types)
```bash
kubebuilder create api --group <group> --version <version> --kind <Kind>
```

### Deploy Image Plugin (scaffold to deploy/manage ANY container image)

Generate a controller that deploys and manages a container image (nginx, redis, memcached, your app, etc.):

```bash
# Example: deploying memcached
kubebuilder create api --group example.com --version v1alpha1 --kind Memcached \
  --image=memcached:alpine \
  --plugins=deploy-image.go.kubebuilder.io/v1-alpha
```

Scaffolds good-practice code: reconciliation logic, status conditions, finalizers, RBAC. Use as a reference implementation.


### Create Webhooks
```bash
# Validation + defaulting
kubebuilder create webhook --group <group> --version <version> --kind <Kind> \
  --defaulting --programmatic-validation

# Conversion webhook (for multi-version APIs)
kubebuilder create webhook --group <group> --version v1 --kind <Kind> \
  --conversion --spoke v2
```

### Controller for Core Kubernetes Types
```bash
# Watch Pods
kubebuilder create api --group core --version v1 --kind Pod \
  --controller=true --resource=false

# Watch Deployments
kubebuilder create api --group apps --version v1 --kind Deployment \
  --controller=true --resource=false
```

### Controller for External Types (e.g., from other operators)

Watch resources from external APIs (cert-manager, Argo CD, Istio, etc.):

```bash
# Example: watching cert-manager Certificate resources
kubebuilder create api \
  --group cert-manager --version v1 --kind Certificate \
  --controller=true --resource=false \
  --external-api-path=github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1 \
  --external-api-domain=io \
  --external-api-module=github.com/cert-manager/cert-manager
```

**Note:** Use `--external-api-module=<module>@<version>` only if you need a specific version. Otherwise, omit `@<version>` to use what's in go.mod.

### Webhook for External Types

```bash
# Example: validating external resources
kubebuilder create webhook \
  --group cert-manager --version v1 --kind Issuer \
  --defaulting \
  --external-api-path=github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1 \
  --external-api-domain=io \
  --external-api-module=github.com/cert-manager/cert-manager
```

## Testing & Development

```bash
mise run test              # Run unit tests (uses envtest: real K8s API + etcd)
mise run run               # Run locally (uses current kubeconfig context)
```

Tests use **Ginkgo + Gomega** (BDD style). Check `suite_test.go` for setup.

## Deployment Workflow

```bash
# 1. Regenerate manifests
mise run manifests generate

# 2. Build & deploy
export IMG=<registry>/<project>:tag
IMG=$IMG mise run docker-build docker-push  # Or: kind load docker-image $IMG --name <cluster>
IMG=$IMG mise run deploy

# 3. Test
kubectl apply -k config/samples/

# 4. Debug
kubectl logs -n <project>-system -l control-plane=controller-manager -c manager -f
```

### API Design

**Key markers for** `api/<version>/*_types.go`:

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"

// On fields:
// +kubebuilder:validation:Required
// +kubebuilder:validation:Minimum=1
// +kubebuilder:validation:MaxLength=100
// +kubebuilder:validation:Pattern="^[a-z]+$"
// +kubebuilder:default="value"
```

- **Use** `metav1.Condition` for status (not custom string fields)
- **Use predefined types**: `metav1.Time` instead of `string` for dates
- **Follow K8s API conventions**: Standard field names (`spec`, `status`, `metadata`)

### Controller Design

**RBAC markers in** `internal/controller/*_controller.go`:

```go
// +kubebuilder:rbac:groups=mygroup.example.com,resources=mykinds,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mygroup.example.com,resources=mykinds/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=mygroup.example.com,resources=mykinds/finalizers,verbs=update
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
```

**Implementation rules:**
- **Idempotent reconciliation**: Safe to run multiple times
- **Re-fetch before updates**: `r.Get(ctx, req.NamespacedName, obj)` before `r.Update` to avoid conflicts
- **Structured logging**: `log := log.FromContext(ctx); log.Info("msg", "key", val)`
- **Owner references**: Enable automatic garbage collection (`SetControllerReference`)
- **Watch secondary resources**: Use `.Owns()` or `.Watches()`, not just `RequeueAfter`
- **Finalizers**: Clean up external resources (buckets, VMs, DNS entries)

### Logging

**Follow Kubernetes logging message style guidelines:**

- Start from a capital letter
- Do not end the message with a period
- Active voice: subject present (`"Deployment could not create Pod"`) or omitted (`"Could not create Pod"`)
- Past tense: `"Could not delete Pod"` not `"Cannot delete Pod"`
- Specify object type: `"Deleted Pod"` not `"Deleted"`
- Balanced key-value pairs

```go
log.Info("Starting reconciliation")
log.Info("Created Deployment", "name", deploy.Name)
log.Error(err, "Failed to create Pod", "name", name)
```

**Reference:** https://github.com/kubernetes/community/blob/master/contributors/devel/sig-instrumentation/logging.md#message-style-guidelines

### Webhooks
- **Create all types together**: `--defaulting --programmatic-validation --conversion`
- **When`--force`is used**: Backup custom logic first, then restore after scaffolding
- **For multi-version APIs**: Use hub-and-spoke pattern (`--conversion --spoke v2`)
  - Hub version: Usually oldest stable version (v1)
  - Spoke versions: Newer versions that convert to/from hub (v2, v3)
  - Example: `--group crew --version v1 --kind Captain --conversion --spoke v2` (v1 is hub, v2 is spoke)

### Learning from Examples

The **deploy-image plugin** scaffolds a complete controller following good practices. Use it as a reference implementation:

```bash
kubebuilder create api --group example --version v1alpha1 --kind MyApp \
  --image=<your-image> --plugins=deploy-image.go.kubebuilder.io/v1-alpha
```

Generated code includes: status conditions (`metav1.Condition`), finalizers, owner references, events, idempotent reconciliation.

## Distribution Options

### Option 1: YAML Bundle (Kustomize)

```bash
# Generate dist/install.yaml from Kustomize manifests
IMG=<registry>/<project>:tag mise run build-installer
```

**Key points:**
- The `dist/install.yaml` is generated from Kustomize manifests (CRDs, RBAC, Deployment)
- Commit this file to your repository for easy distribution
- Users only need `kubectl` to install (no additional tools required)

**Example:** Users install with a single command:
```bash
kubectl apply -f https://raw.githubusercontent.com/<org>/<repo>/<tag>/dist/install.yaml
```

### Option 2: Helm Chart

This repo already has a generated `dist/chart/`; the commands below are for generating one
from scratch in a new project (`--output-dir` to use a different directory than `dist`):
```bash
kubebuilder edit --plugins=helm/v2-alpha                      # Generates dist/chart/ (default)
kubebuilder edit --plugins=helm/v2-alpha --output-dir=charts  # Generates charts/chart/
```
To refresh this repo's existing chart, use `mise run helm-chart-refresh` instead — see below.

**For development:**
```bash
IMG=<registry>/<project>:<tag> mise run helm-deploy       # Deploy manager via Helm
IMG=$IMG HELM_EXTRA_ARGS="--set ..." mise run helm-deploy # Deploy with custom values
mise run helm-status                                      # Show release status
mise run helm-uninstall                                   # Remove release
mise run helm-history                                     # View release history
mise run helm-rollback                                    # Rollback to previous version
```

**For end users/production:**
```bash
helm install my-release ./<output-dir>/chart/ --namespace <ns> --create-namespace
```

**Refreshing the chart:** whenever `api/v1alpha1/*_types.go`, `config/manager`, or `config/rbac`
change, `dist/chart/` needs regenerating to match. Run:
```bash
mise run helm-chart-refresh
```
This wraps `kubebuilder edit --plugins=helm/v2-alpha` and prints a `git diff --stat` of what
changed, so review that before committing. Don't run the raw `kubebuilder edit` command by
hand — it shells out to `make manifests`/`make generate`/`make build-installer` internally and
this project has no Makefile (mise tasks are the source of truth instead), so the mise task
shims one in for the duration of the run.

Without `--force`, the plugin already leaves `Chart.yaml`, `values.yaml`, `_helpers.tpl`,
`NOTES.txt`, `.helmignore`, and `templates/network-policy/*` untouched on every run (confirmed
empirically, not just from `--help` text) — that's where this
chart's hand-tuned `values.yaml` sections (`prometheus.scrapeNativeHistograms`,
`prometheusRule.*`, `grafanaDashboard.*`) and the `controllerManagerName` helper in
`_helpers.tpl` live. The plugin also never touches files it doesn't recognize at all —
`templates/prometheus/grafana-dashboard.yaml`, `templates/prometheus/prometheus-rule.yaml`, and
`dashboards/*.json` are wholly custom and survive any regen untouched.

The one gap: three templates the plugin *does* always regenerate, because it doesn't know
about this project's shortened resource-name suffixes or the native-histogram ServiceMonitor
fields — `templates/manager/manager.yaml`, `templates/metrics/controller-manager-metrics-service.yaml`,
and `templates/prometheus/controller-manager-metrics-monitor.yaml`. `mise run
helm-chart-refresh` backs these three up before regenerating and restores them afterward,
printing a diff of what upstream kubebuilder would have changed so you can tell if a future
kubebuilder/plugin upgrade altered their stock shape underneath the hand edits.

Never pass `--force`: it additionally regenerates `values.yaml` and `_helpers.tpl` from
scratch, discarding the hand-tuned sections above. (It still doesn't touch the wholly-custom
prometheus/grafana files or `Chart.yaml` — the plugin only ever overwrites files in its own
known scaffold set, `--force` or not.) If you ever do need to rebuild `values.yaml`/`manager.yaml`
from a clean slate, you'll need to manually re-port the hand-tuned sections back in afterward.

### Publish Container Image

```bash
export IMG=<registry>/<project>:<version>
IMG=$IMG mise run docker-build docker-push
```

### Code Intelligence

Prefer LSP over Grep/Glob/Read for code navigation:
- `goToDefinition` / `goToImplementation` to jump to source
- `findReferences` to see all usages across the codebase
- `workspaceSymbol` to find where something is defined
- `documentSymbol` to list all symbols in a file
- `hover` for type info without reading the file
- `incomingCalls` / `outgoingCalls` for call hierarchy

Before renaming or changing a function signature, use
`findReferences` to find all call sites first.

Use Grep/Glob only for text/pattern searches (comments,
strings, config values) where LSP doesn't help.

After writing or editing code, check LSP diagnostics before
moving on. Fix any type errors or missing imports immediately.

## References

### Essential Reading
- **Kubebuilder Book**: https://book.kubebuilder.io (comprehensive guide)
- **controller-runtime FAQ**: https://github.com/kubernetes-sigs/controller-runtime/blob/main/FAQ.md (common patterns and questions)
- **Good Practices**: https://book.kubebuilder.io/reference/good-practices.html (why reconciliation is idempotent, status conditions, etc.)
- **Logging Conventions**: https://github.com/kubernetes/community/blob/master/contributors/devel/sig-instrumentation/logging.md#message-style-guidelines (message style, verbosity levels)

### API Design & Implementation
- **API Conventions**: https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md
- **Operator Pattern**: https://kubernetes.io/docs/concepts/extend-kubernetes/operator/
- **Markers Reference**: https://book.kubebuilder.io/reference/markers.html

### Tools & Libraries
- **controller-runtime**: https://github.com/kubernetes-sigs/controller-runtime
- **controller-tools**: https://github.com/kubernetes-sigs/controller-tools
- **Kubebuilder Repo**: https://github.com/kubernetes-sigs/kubebuilder

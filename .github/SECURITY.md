# Security Policy

## Supported Versions

This project releases continuously from `main` (see [Releases](https://github.com/maverickd650-labs/rss2discord/releases)).
Only the latest released version is supported — please upgrade before reporting an issue.

## Reporting a Vulnerability

Please report security vulnerabilities privately using
[GitHub's private vulnerability reporting](https://github.com/maverickd650-labs/rss2discord/security/advisories/new)
(Security tab → "Report a vulnerability"). Do not open a public issue for security reports.

You should receive an initial response within a few days. If the issue is confirmed, we'll work
with you on a fix and coordinate disclosure timing before a public advisory/release.

## Scope

As noted in the [README](../README.md), this project is built with heavy AI assistance and has not
had a third-party security review — read the code before trusting it with anything sensitive.
Areas of particular interest for reports:

- SSRF guards in [`internal/rss/transport.go`](../internal/rss/transport.go) and
  [`internal/rss/client.go`](../internal/rss/client.go) (feed fetching) and
  [`internal/discord/client.go`](../internal/discord/client.go) (webhook delivery) — these resolve
  and validate destination IPs/hosts before making requests.
- Webhook secret handling and log/error redaction.
- Discord message sanitization (`@everyone`/`@here` suppression, non-http(s) link/image
  stripping, length clamping) and message-template execution limits.

Known trade-off: feed URLs (`spec.feeds[].rssUrl`) are not treated as secrets. They appear in
controller logs, Events, `FeedGroup.status`, Prometheus metric labels and trace attributes, and
fetch errors may echo them. Don't put credentials or tokens in a feed URL's query string or
userinfo unless everyone who can read those surfaces is trusted with them. Only the Discord
webhook URL (kept in a Secret) is redacted.

Secret access: by default the operator's ClusterRole can only `get` Secrets, by name; it has no
`list` or `watch`, so its token cannot enumerate Secrets. Watching webhook Secrets (to retry a
feed as soon as its webhook Secret is replaced, `--watch-webhook-secrets`) needs `list` and
`watch`, so the chart enables it only where the grant is scoped -- a namespace-scoped install
(`rbac.watchNamespaces`/`rbac.namespaced`) -- or on explicit opt-in with
`rbac.clusterWideSecretWatch`, a known trade-off for single-tenant clusters. The watch is
metadata-only and its cache strips annotations and managedFields (`kubectl apply` copies a
Secret's data into an annotation), and Secret data is only ever read uncached, by name. Those
limit what the operator holds in memory, **not what its credential is authorized to do**: with
`rbac.clusterWideSecretWatch`, a stolen service-account token can list every Secret's data. For
tenant deployments, install with the chart's `rbac.watchNamespaces` (see the README): the
grant becomes a Role in each listed namespace and the manager's `--watch-namespaces` is set to the same list, so both are scoped
together. Treat the resulting RBAC grants as the boundary, not the watch arguments, and verify
them with `kubectl auth can-i ... --as=system:serviceaccount:<ns>:<sa>` -- including after
narrowing an existing installation, where `helm upgrade` removes the cluster-wide manager
binding (covered by a chart test against a real API server) but a hand-applied
`dist/install.yaml` is not pruned.

## Supply Chain

Dependencies are kept current via Renovate, GitHub Actions are pinned to commit SHAs, and the
repo runs CodeQL and `govulncheck` on every push/PR plus a weekly schedule. Release artifacts
include a generated SBOM and are signed with cosign.

# Contributing

Thanks for considering a contribution. This is a small, single-maintainer project — issues and
PRs are welcome, but please open an issue before starting on anything non-trivial so we can agree
on the approach first.

## Setup

This project uses [mise](https://mise.jdx.dev/) to pin tool versions (Go, golangci-lint,
kubebuilder, kind, etc.) — install it, then `mise install` from the repo root. CI installs the
same pinned versions and runs the same tasks, so **local == CI**.

```bash
mise install
mise run test
```

See `../.mise/config.toml` for the full task list, and the [CLAUDE.md](../CLAUDE.md) "Commands" section
for the most commonly used ones. Don't invoke `go test`/`golangci-lint` directly — the `mise`
tasks wire up codegen, envtest assets, and a custom lint binary first.

## Making a change

1. After editing `*_types.go` or `+kubebuilder` markers: `mise run manifests generate`.
2. After editing any `*.go`: `mise run lint-fix` then `mise run test`.
3. If `config/` (CRDs/RBAC/manager) changed: `mise run helm-chart-refresh` to regenerate
   `dist/chart/` without clobbering its hand-tuned templates.
4. `mise run test-e2e` runs against a throwaway Kind cluster it creates and deletes automatically
   — never point it at a real cluster.

See [AGENTS.md](../AGENTS.md) for kubebuilder scaffolding workflows (new APIs/webhooks/controllers)
and the architectural rationale behind the SSRF guards, status model, and metrics conventions.

## Security-sensitive paths

`internal/rss/transport.go` (the guarded dialer), `internal/rss/client.go` (the URL scheme check,
the response-size cap and the guarded default client) and `internal/discord/client.go` carry SSRF guards (the RSS feed URL and
Discord webhook URL are both user-supplied). Don't weaken or duplicate these checks, and don't
relax them "just for tests" — test helpers in `internal/controller/*_test.go` already provide
unguarded variants for hitting local `httptest` servers. See [SECURITY.md](SECURITY.md) to report
a vulnerability rather than opening a public issue.

## Commit messages / PRs

Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/)
(`feat:`, `fix:`, `chore:`, `docs:`, etc., with `!` for breaking changes, e.g. `feat!:`) —
[release-please](https://github.com/googleapis/release-please) uses these to version releases and
generate `CHANGELOG.md` automatically, so this isn't just a style preference.

Branch protection on `main` requires PRs to pass Lint, Test, and E2E Tests before merging — see
the [PR template](PULL_REQUEST_TEMPLATE.md) checklist for what to run locally first.

**Never write a bare `owner/repo#123` reference** (e.g. `kubernetes-sigs/kubebuilder#4809`) in a
commit message, PR title/body, or comment — even just to cite an upstream issue for context.
GitHub autolinks that exact syntax and posts a cross-reference notification on the *other* repo's
issue, pinging its maintainers about a repo they have nothing to do with. If you need to cite an
external issue, use the full URL or phrase it as `owner/repo issue 123` instead.

## Code of Conduct

This project follows the [Contributor Covenant](https://github.com/maverickd650-labs/.github/blob/main/CODE_OF_CONDUCT.md).

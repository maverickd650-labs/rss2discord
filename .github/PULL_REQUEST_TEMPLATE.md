## Summary

<!-- What does this change do, and why? -->

## Checklist

- [ ] `mise run lint-fix` and `mise run test` pass locally
- [ ] If `*_types.go` or markers changed: `mise run manifests generate` was run and the diff is included
- [ ] If `config/`, CRDs, or RBAC changed: `mise run helm-chart-refresh` was run and `dist/chart` is current
- [ ] Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `chore:`, etc.) — release-please uses these to version and changelog
- [ ] Touches `internal/rss/transport.go`, `internal/rss/client.go` or `internal/discord/client.go`? Called out below, since these carry the SSRF guards.

## Notes for reviewers

<!-- Anything that needs extra attention, e.g. security-sensitive paths, breaking changes (use `feat!:`/`fix!:`). -->

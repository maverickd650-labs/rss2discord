#!/bin/bash
# Installs the mise-pinned toolchain (see .mise/config.toml) for Claude Code cloud
# sessions so `mise run test` / `mise run lint-fix` work from the first prompt.
set -euo pipefail

if [ "${CLAUDE_CODE_REMOTE:-}" != "true" ]; then
  exit 0
fi

cd "${CLAUDE_PROJECT_DIR:-.}"

export PATH="$HOME/.local/bin:$PATH"

# Install mise itself (needs mise.run + github.com reachable from the environment)
if ! command -v mise >/dev/null 2>&1; then
  curl -fsSL https://mise.run | sh
fi

# Make mise and its shims available to every later command in the session
if [ -n "${CLAUDE_ENV_FILE:-}" ]; then
  echo 'export PATH="$HOME/.local/bin:$HOME/.local/share/mise/shims:$PATH"' >> "$CLAUDE_ENV_FILE"
fi
export PATH="$HOME/.local/share/mise/shims:$PATH"

# bin/ is git-ignored and holds the custom lint binary, so the hash file lives
# there and disappears with it.
mkdir -p bin
hashes=bin/.session-start.sha256
if ! sha256sum --check --status "$hashes" 2>/dev/null; then
  # Delete the old hashes first, so a failed install runs again next time
  rm -f "$hashes"

  mise trust --yes .mise/config.toml
  mise install --yes
  # Custom golangci-lint (logcheck plugin) used by `mise run lint`
  mise run golangci-lint-custom
  go mod download

  sha256sum .claude/hooks/session-start.sh .mise/config.toml .mise/mise.lock .custom-gcl.yml go.mod go.sum > "$hashes" || rm -f "$hashes"
fi

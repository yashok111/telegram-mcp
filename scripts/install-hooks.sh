#!/usr/bin/env bash
# Install a pre-commit hook that gates commits on gofmt, vet, lint, test:fast.
# Symlinks scripts/pre-commit into .git/hooks so updates flow without re-install.
#
# Usage (run from anywhere):
#   bash scripts/install-hooks.sh

set -euo pipefail

cd "$(dirname "$0")/.."  # repo root

if [[ ! -d .git ]]; then
  echo "no .git/ found — run this from inside the telegram-mcp clone" >&2
  exit 1
fi

mkdir -p .git/hooks
ln -sf ../../scripts/pre-commit .git/hooks/pre-commit
chmod +x scripts/pre-commit

echo "pre-commit hook installed → .git/hooks/pre-commit → scripts/pre-commit"

#!/usr/bin/env bash
# Install curated skills for the telegram-mcp repo into .agents/skills/.
# Non-interactive: passes -y and targets the `universal` agent (skills CLI),
# so files land in <repo>/.agents/skills/ — never in .claude/skills/.
# Re-run after a fresh clone to repopulate (lockfile: skills-lock.json).
#
# Project: local Go MCP server bridging Telegram <-> Claude Code.
# Stack: Go 1.26 + telego v1 + mark3labs/mcp-go + Linux/amd64 single binary.
# Source registry: https://www.skills.sh/
#
# Usage (run from anywhere):
#   bash scripts/install-skills.sh                 # install everything
#   bash scripts/install-skills.sh --dry-run       # print commands only
#   bash scripts/install-skills.sh anthropics
#   bash scripts/install-skills.sh superpowers
#   bash scripts/install-skills.sh mattpocock

set -euo pipefail

REPO_ANTHROPICS="https://github.com/anthropics/skills"
REPO_SUPERPOWERS="https://github.com/obra/superpowers"
REPO_MATTPOCOCK="https://github.com/mattpocock/skills"

# anthropics/skills — mcp-builder is the headline match (this entire repo IS
# an MCP server). skill-creator covers future custom skills for project conventions.
ANTHROPICS_SKILLS=(
  mcp-builder
  skill-creator
)

# obra/superpowers — full dev-workflow pack. Plan → TDD → debug → subagent flow
# → parallelism → worktrees → verify → branch finish → code review request/receive.
# Brainstorming for design spikes (e.g. permission-flow refactor, Markdown escaping).
SUPERPOWERS_SKILLS=(
  brainstorming
  writing-plans
  executing-plans
  test-driven-development
  systematic-debugging
  subagent-driven-development
  dispatching-parallel-agents
  using-git-worktrees
  verification-before-completion
  finishing-a-development-branch
  requesting-code-review
  receiving-code-review
)

# mattpocock/skills — companion pairs to obra's TDD + debug, plus
# improve-codebase-architecture for the 819-LOC bot.go (will want carving up),
# grill-me for harsher self-review, grill-with-docs when we need to verify
# claims against telego/mcp-go upstream docs.
MATTPOCOCK_SKILLS=(
  tdd
  diagnose
  grill-me
  grill-with-docs
  improve-codebase-architecture
)

DRY_RUN=0
TARGETS=(
  "anthropics"
  "superpowers"
  "mattpocock"
)
SELECTED=()

for arg in "$@"; do
  case "$arg" in
    --dry-run|-n) DRY_RUN=1 ;;
    anthropics|superpowers|mattpocock)
      SELECTED+=("$arg")
      ;;
    -h|--help)
      sed -n '2,20p' "$0"
      exit 0
      ;;
    *)
      echo "unknown arg: $arg" >&2
      exit 2
      ;;
  esac
done

if [[ ${#SELECTED[@]} -gt 0 ]]; then
  TARGETS=("${SELECTED[@]}")
fi

run() {
  local q=""
  for a in "$@"; do
    if [[ "$a" == *" "* ]]; then
      q+=" \"$a\""
    else
      q+=" $a"
    fi
  done
  echo "+${q}"
  if [[ $DRY_RUN -eq 0 ]]; then
    "$@"
  fi
}

install_set() {
  local repo="$1"
  shift
  local -a skills=("$@")
  echo
  echo "=== $repo (${#skills[@]} skills) ==="
  # -y skips prompts; -a universal pins the universal agent dir → .agents/skills/.
  run npx -y -p skills skills add "$repo" -y -a universal --skill "${skills[@]}"
}

cd "$(dirname "$0")/.."  # cwd → repo root (skills land in .agents/skills/)

for t in "${TARGETS[@]}"; do
  case "$t" in
    anthropics)  install_set "$REPO_ANTHROPICS"  "${ANTHROPICS_SKILLS[@]}" ;;
    superpowers) install_set "$REPO_SUPERPOWERS" "${SUPERPOWERS_SKILLS[@]}" ;;
    mattpocock)  install_set "$REPO_MATTPOCOCK"  "${MATTPOCOCK_SKILLS[@]}" ;;
  esac
done

echo
echo "done. skills in .agents/skills/. lockfile: skills-lock.json"

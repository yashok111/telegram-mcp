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
#   bash scripts/install-skills.sh samber
#   bash scripts/install-skills.sh netresearch
#   bash scripts/install-skills.sh jetbrains

set -euo pipefail

REPO_ANTHROPICS="https://github.com/anthropics/skills"
REPO_SUPERPOWERS="https://github.com/obra/superpowers"
REPO_MATTPOCOCK="https://github.com/mattpocock/skills"
REPO_SAMBER="https://github.com/samber/cc-skills-golang"
REPO_NETRESEARCH="https://github.com/netresearch/go-development-skill"
REPO_JETBRAINS="https://github.com/JetBrains/go-modern-guidelines"

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

# samber/cc-skills-golang — 42-skill catalogue; cherry-picked the subset that
# matches this project (single-binary MCP server, goroutines for poll +
# approvalLoop, ctx.Done() shutdown, error wrapping with %w, secret handling
# via .env, internal/ layout, BotAPI/Notifier interfaces). Dropped:
# cli/cobra/viper, database, graphql, grpc, samber libs, DI frameworks,
# swagger, benchmark, perf — none of which we use.
SAMBER_SKILLS=(
  golang-concurrency
  golang-context
  golang-error-handling
  golang-testing
  golang-stretchr-testify
  golang-code-style
  golang-naming
  golang-modernize
  golang-structs-interfaces
  golang-safety
  golang-security
  golang-troubleshooting
  golang-lint
  golang-project-layout
  golang-observability
  golang-documentation
)

# netresearch/go-development-skill — single bundled skill: enterprise Go
# patterns (resilient services, testing, linting, API design, fuzzing,
# modernization). Broader than samber's narrow-topic skills; pairs well.
NETRESEARCH_SKILLS=(
  go-development
)

# JetBrains/go-modern-guidelines — auto-detects go.mod version (we're on
# 1.26) and applies modern idioms up to that release. Authoritative on
# slices/maps stdlib, range-over-func, generics, etc.
JETBRAINS_SKILLS=(
  use-modern-go
)

# --- Notes on what we DIDN'T add ---
# Telegram-specific: nothing useful. AlexSKuznetsov/claude-skill-telegram is
# a reminders skill for telegram-bot users, not a guide for bot authors.
# RichardAtCT, linuz90, seedprod, gmotyl — all apps, not authoring guides.
# MCP-protocol-specific: anthropics/mcp-builder (above) is the only proper
# match. microsoft/skills, intellectronica/skillz, etc. are MCP servers that
# load skills, not skills for building MCP servers.

DRY_RUN=0
TARGETS=(
  "anthropics"
  "superpowers"
  "mattpocock"
  "samber"
  "netresearch"
  "jetbrains"
)
SELECTED=()

for arg in "$@"; do
  case "$arg" in
    --dry-run|-n) DRY_RUN=1 ;;
    anthropics|superpowers|mattpocock|samber|netresearch|jetbrains)
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
    samber)      install_set "$REPO_SAMBER"      "${SAMBER_SKILLS[@]}" ;;
    netresearch) install_set "$REPO_NETRESEARCH" "${NETRESEARCH_SKILLS[@]}" ;;
    jetbrains)   install_set "$REPO_JETBRAINS"   "${JETBRAINS_SKILLS[@]}" ;;
  esac
done

echo
echo "done. skills in .agents/skills/. lockfile: skills-lock.json"

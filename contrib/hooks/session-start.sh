#!/usr/bin/env bash
# contrib/hooks/session-start.sh
#
# Claude Code SessionStart hook: emits a small additionalContext block telling
# the agent its own Telegram shim alias + cc_session_id.
#
# Install in ~/.claude/settings.json:
#   "hooks": {
#     "SessionStart": [
#       { "hooks": [ { "type": "command", "command": "/abs/path/to/telegram-mcp self --hook" } ] }
#     ]
#   }
#
# CC pipes the hook payload as JSON on stdin (we don't need to inspect it — the
# binary reads CLAUDE_CODE_SESSION_ID from env, which CC sets for hooks).
# Output: JSON {"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"..."}}
#
# This script exists only as a convenience wrapper / lookup-path. The same
# behavior is available by pointing the hook directly at `telegram-mcp self --hook`.

set -euo pipefail

BIN="${TELEGRAM_MCP_BIN:-$(command -v telegram-mcp || true)}"
if [[ -z "${BIN}" ]]; then
  # Fall back to a no-op so a missing binary never blocks CC startup.
  printf '{}\n'
  exit 0
fi

exec "${BIN}" self --hook

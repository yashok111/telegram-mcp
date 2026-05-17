# systemd --user unit

When the bot is invoked **as an MCP server**, Claude Code spawns it on demand
and `PR_SET_PDEATHSIG` shuts it down when the session ends. That is the
intended primary mode.

This unit is for the **standalone** case — you want the bot to keep accepting
DMs (and replying via the `/telegram:access` skill) even when no Claude Code
session is running. Useful on a VPS that wants the pairing flow to survive
between sessions.

## Install

```bash
mkdir -p ~/.config/systemd/user
cp contrib/systemd/telegram-mcp.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now telegram-mcp.service
loginctl enable-linger "$USER"   # keeps user units running after logout
```

## Operate

```bash
systemctl --user status telegram-mcp
journalctl --user -u telegram-mcp -f
systemctl --user restart telegram-mcp
```

## Caveats

- **Token collision** — when both this unit AND Claude Code's MCP spawn are
  running, both poll the same bot token and one gets 409 Conflict. Pick one.
  If you want the unit, do not register the bot as an MCP server in Claude
  Code (skip `claude mcp add telegram ...`).
- **`ExecStart`** assumes `~/projects/telegram-mcp/bin/telegram-mcp`. Adjust
  the path if you installed elsewhere.
- **`ReadWritePaths`** is locked to `~/.claude/channels/telegram` — that's
  where `access.json`, `.env`, `bot.pid`, `inbox/`, `approved/` live.

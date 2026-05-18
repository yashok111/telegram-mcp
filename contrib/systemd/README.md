# systemd --user unit

By default the daemon auto-spawns from the first Claude Code session that
needs it and idles out 30 minutes after the last shim disconnects. This unit
keeps the daemon alive permanently — useful when you want the bot to accept
DMs (and replies via `/telegram:access`) across CC sessions and reboots.

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

- **One daemon per host** — there is exactly one bot-token poller per host.
  When this unit is running, `daemon.sock` is already present, so every shim
  dials it immediately instead of spawning its own daemon. If the unit is
  stopped, the next shim falls back to spawning a daemon itself. Registering
  the binary as a CC MCP server is still required for shims to attach.
- **`ExecStart`** assumes `~/projects/telegram-mcp/bin/telegram-mcp`. Adjust
  the path if you installed elsewhere.
- **`ReadWritePaths`** is locked to `~/.claude/channels/telegram` — that's
  where `access.json`, `.env`, `bot.pid`, `inbox/`, `approved/` live.

# telegram-mcp

Local Go port of the Claude Code Telegram channel plugin. Single binary, no node/bun, dies with parent via `PR_SET_PDEATHSIG`.

## State

Compatible with the TS plugin: reads `~/.claude/channels/telegram/{access.json,.env,approved/,inbox/}` unchanged.

## Build

```
make build
```

## Register

```
claude mcp add telegram -s user -- $(pwd)/bin/telegram-mcp
```

Then disable the marketplace plugin so only one poller is alive:

```
claude plugin disable telegram
```

## Layout

- `cmd/server/main.go` — entry, lifecycle, PID file, PDEATHSIG, env load
- `internal/access/` — access.json schema + load/save + pairing codes
- `internal/bot/` — telego long-polling + gate + inbound handlers
- `internal/mcp/` — stdio MCP server, tool registry, permission flow
- `internal/chunk/` — message splitter

## Status

Skeleton. Most handlers have `TODO` markers — fill iteratively:

1. `bot.SendMessage` / `SendPhoto` / `SendDocument` / `EditMessage` / `React` / `DownloadFile`
2. `mcp.handleReply` — wire chunk.Split + bot send loop
3. `mcp` notifications — `notifications/claude/channel{,/permission}` (experimental, mcp-go needs custom handler)
4. `bot.onPhoto` / `onDocument` — file_id meta + deferred download
5. `bot.onPermissionCallback` — inline-keyboard outcomes
6. `bot.isMentioned` — entity scan + extra patterns
7. Permission-reply regex intercept in `onText`

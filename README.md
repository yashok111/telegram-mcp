# telegram-mcp

Local Go port of the Claude Code Telegram channel plugin. Single binary, no
node/bun, dies with parent via `PR_SET_PDEATHSIG`. Drop-in compatible with the
TS plugin's `~/.claude/channels/telegram/` state directory — existing
`access.json` pairing carries over.

## Why

The original `external_plugins/telegram` plugin ships as a bun-runtime
`server.ts`. Its grandchild process could survive past its parent and busy-loop
at 100% CPU under certain restart conditions. Go port removes the runtime,
binds death to the parent at the kernel level, and adds a comm-checked stale
PID claim so we never SIGTERM unrelated processes.

## Build

```bash
make build         # → bin/telegram-mcp
make check         # lint + race-enabled test + build
```

Requires Go 1.26 and (for `make lint`) golangci-lint v2.12+ built with Go 1.26:

```bash
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
```

## Register with Claude Code

```bash
claude mcp add telegram -s user -- $(pwd)/bin/telegram-mcp
```

If the marketplace bun version is enabled, disable it first or you'll have two
pollers fighting over the same Telegram token (409 Conflict storm):

```bash
claude plugin disable telegram
```

Restart the Claude Code session after registering.

## Persistent daemon (optional)

The daemon auto-spawns on the first Claude Code session and idles out after the
last shim disconnects. If you want the bot to keep accepting DMs across
sessions (and reboots), install the systemd `--user` unit from
`contrib/systemd/README.md`. The unit runs `telegram-mcp daemon` directly.

## Config

State lives in `~/.claude/channels/telegram/` (or `TELEGRAM_STATE_DIR`):

- `.env` — `TELEGRAM_BOT_TOKEN=...` (chmod 0600, owner-only)
- `access.json` — allowlist, pairing state, group policy, UX prefs
- `bot.pid` — current poller PID
- `inbox/` — downloaded attachments + photos
- `approved/` — pairing-confirmation drop-zone from `/telegram:access` skill

Managed by the `/telegram:access` skill — do not edit `access.json` by hand
during a live session; the bot reads it on every gate decision.

`TELEGRAM_ACCESS_MODE=static` snapshots `access.json` at boot and refuses to
re-read or write it. Useful for read-only filesystems or when you want pairing
locked at deploy time.

## Skills

`bash scripts/install-skills.sh` installs 37 curated skills into
`.agents/skills/` (gitignored, lockfile committed). 18 of them are Go-specific
(samber, JetBrains, netresearch); the rest cover dev workflow (obra,
mattpocock) and MCP authoring (anthropics).

## Tooling

```bash
make lint          # golangci-lint v2 — 49 enabled linters, 0 expected findings
make lint-fix      # auto-fix (modernize, gofumpt, intrange, etc.)
make test          # go test -race ./...
make check         # lint + test + build
```

Install the pre-commit hook to gate every commit on the same checks:

```bash
bash scripts/install-hooks.sh
```

## Status

- 182 tests passing under `-race`
- ~84% LOC coverage (full report: `make test` then `go tool cover -html=...`)
- `go vet` clean, `golangci-lint` clean
- goleak-verified in every package

## Security

- Token lives only in `~/.claude/channels/telegram/.env` (chmod 0600). Never
  logged, never sent over the MCP channel — the `reply` tool refuses to send
  files from inside the state dir.
- Permission-reply intercept (`yes xxxxx` / `no xxxxx`) is gate-authenticated
  before reaching Claude Code — non-allowlisted senders are dropped first.
- Group commands are silently dropped, never echoed back, so the bot's
  presence in unapproved chats is not confirmed.

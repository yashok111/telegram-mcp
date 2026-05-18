# CLAUDE.md

Claude Code repo instructions for **telegram-mcp** — local Go MCP server bridging Telegram <-> Claude Code. Replaces the bun-runtime `external_plugins/telegram` plugin. Single binary, PR_SET_PDEATHSIG, drop-in compatible with the TS plugin's `~/.claude/channels/telegram/` state.

## Stack

Go **1.26** · `github.com/mark3labs/mcp-go` v0.54 (stdio MCP server) · `github.com/mymmrac/telego` v1.9 (Telegram bot, long-polling) · `log/slog` JSON to stderr · `go.uber.org/goleak` in every test pkg. No DB, no cache — a single daemon owns the bot token; each Claude Code session attaches via a stdio shim.

## Commands

```bash
make build              # → bin/telegram-mcp (trimpath + ldflags -s -w)
make test               # go test -race ./...
make lint               # golangci-lint v2 (built from source w/ Go 1.26)
make lint-fix
make check              # lint + test + build (CI gate)

bash scripts/install-skills.sh   # → .agents/skills/ (37 skills, lockfile)
bash scripts/install-hooks.sh    # → .git/hooks/pre-commit
```

`golangci-lint` must be a v2 build with Go 1.26 — prebuilt v2.6 uses Go 1.25 and refuses our go.mod. `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest`.

## Layout

```
cmd/server/main.go              entry, lifecycle, env, PDEATHSIG, PID claim
internal/access/                access.json schema + atomic save + corrupt recovery
internal/bot/                   telego long-poller, gate, handlers, outbound API
  bot.go                          ~900 LOC: Poll, handleCommand/Message/Callback, gate, send*
  markdown.go                     EscapeMarkdownV2 + EscapeMarkdownV2Code helpers
internal/mcp/                   stdio MCP server, tool registry, notification surface
internal/chunk/                 4096-cap message splitter (length or newline boundaries)
contrib/systemd/                user-mode unit for standalone (non-MCP) deployment
.agents/skills/                 37 curated skills (gitignored, lockfile in repo)
scripts/                        install-skills.sh, install-hooks.sh, pre-commit
```

**Path discipline:**
- `internal/bot` is the only place that talks to telego. MCP layer calls into the `BotAPI` interface — keep it that way so tests can swap a fake.
- `mcp` imports `bot` for `bot.SendOpts` + `bot.PermissionDetails`. `bot` MUST NOT import `mcp` — it uses its own `Notifier` interface to call back.
- `cmd/server` only wires; no business logic.

## Lifecycle

Three subprocesses, two long-lived:

1. **Shim (per Claude Code session)**: the binary launched by Claude Code with no args. Speaks stdio MCP to Claude Code; speaks IPC to the daemon. PR_SET_PDEATHSIG ties it to the parent CC session so it dies with Claude Code.
2. **Daemon (one per host)**: owns the bot token, runs the long-poller, holds the access gate. Spawned by the first shim if not already running (or by systemd via `contrib/systemd/telegram-mcp.service`). Survives shim disconnects; idles out 30 minutes after the last shim leaves.
3. **Self (`telegram-mcp self`)**: read-only context renderer for the SessionStart hook + statusline; does not touch the bot or daemon.

Ctx-driven shutdown everywhere. `Poll` exits within ~2s of `ctx.Done()` via `StopWithContext`. `approvalLoop` is a 5s ticker that respects `ctx.Done()`.

## Daemon

Single daemon per host; every Claude Code session attaches to it via shim.

**Subcommands:** `telegram-mcp daemon` (run daemon foreground), `telegram-mcp shim` (run shim explicitly), `telegram-mcp` (no args; auto-detects — defaults to shim, auto-spawns daemon on first run).

**Routing:** inbound messages go to the shim that last replied to that chat. Fresh chats fall back to the most-recently-connected shim. Permission replies route by `request_id`.

**Daemon owns the bot token.** Shims never see it. Daemon enforces the access.json gate authoritatively.

**Idle exit:** daemon dies 30 minutes after the last shim disconnects. Override with `TELEGRAM_DAEMON_IDLE_EXIT=<seconds>`; `=0` disables.

**Files:**
- `~/.claude/channels/telegram/daemon.sock` (0600) — IPC unix socket
- `~/.claude/channels/telegram/daemon.pid` — daemon's PID
- `~/.claude/channels/telegram/daemon.log` — daemon stderr when shim-spawned (systemd captures it via journal otherwise)

**Systemd alternative:** install `contrib/systemd/telegram-mcp.service` to keep the daemon alive across reboots and outside any Claude Code session.

## CC self-context (SessionStart hook)

The agent should know its own shim alias from turn 1 so `@s2 do X` mentions work without needing inbound message metadata. Correlation key is **CC's pid** — `os.Getppid()` from the shim's perspective — because Claude Code does not expose its session id through MCP `initialize` or via env to plugin processes (confirmed empirically against CC 2.1.143).

1. **Shim side** — on `Wire()` success, the shim writes a per-session snapshot to:

   ```
   ~/.claude/channels/telegram/sessions/<cc_pid>.json
   ```

   File is mode 0600, atomic (tmp+rename), removed when `Run()` exits. Schema:
   `{alias, shim_id, shim_id_prefix, cc_pid, shim_pid, cc_session_id?, workdir, label?, started_at, mode}`. `cc_session_id` is preserved opportunistically from env (CC sets it for Bash and hooks, not for MCP servers); never load-bearing.

2. **CC side** — `telegram-mcp self` reads that file by walking the PPID chain (up to 8 hops) for the first ancestor whose `/proc/<pid>/comm` starts with `claude`. Override the walk by exporting `CC_PID=<pid>`. Wire it as a SessionStart hook in `~/.claude/settings.json`:

   ```json
   {
     "hooks": {
       "SessionStart": [
         { "hooks": [ { "type": "command", "command": "/abs/path/to/bin/telegram-mcp self --hook" } ] }
       ]
     }
   }
   ```

   Or use the bundled wrapper: `contrib/hooks/session-start.sh`. `--hook` emits CC's
   `{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"..."}}`
   shape. Without `--hook`, plain text is printed (useful for `telegram-mcp self`
   at the shell).

**Pre-Wire race**: if `telegram-mcp self` fires before the shim has written its session file (or the file is unreadable), `self` prints a fallback message and exits 0. Hooks must never abort a CC session.

**Statusline** — `telegram-mcp self --statusline` prints a compact `tg:@sN` tag (or empty
string if there's no session file). Compose into CC's `statusLine.command` so the user
sees their addressable alias at a glance:

```json
{
  "statusLine": {
    "type": "command",
    "command": "/abs/path/to/bin/telegram-mcp self --statusline"
  }
}
```

If you already have a custom statusline command, wrap it so the `tg:` tag is appended
when present and silently dropped otherwise.

## Testing

`go.uber.org/goleak` in every package's `TestMain`. Ignored upstream leaks (documented inline): `fasthttp.HostClient.connsCleaner` / `Client.mCleaner` / `TCPDialer.tcpAddrsClean`, `telego.Bot.doLongPolling` (sleeps in backoff after ctx cancel).

**Mocking strategy:**
- `internal/mcp` uses a hand-rolled `fakeBot` matching `BotAPI`.
- `internal/bot/bot_api_test.go` runs a real httptest server impersonating `api.telegram.org/bot<TOKEN>/<method>`. `telego.WithAPIServer(URL)` points the client at it. File-CDN downloads route through `fileClient`, which tests swap to a `redirectTransport`.
- Tests use `t.TempDir()` + `t.Setenv()` exclusively — no `os.Setenv` survives across tests.

**Coverage:** ~84% LOC across the project (chunk 100%, access 92%, bot 90%, mcp 85%, main 42%). The 16% gap is `main.run()` and `Bot.Poll()` — entry points that need subprocess execution or live Telegram. Not worth the test scaffolding.

## Rules

### Code

- Comments default to **none**. Only write when WHY is non-obvious. Don't explain WHAT — names and types do that.
- Errors: wrap with `fmt.Errorf("...: %w", err)`, lowercase, no trailing punctuation. Use `errors.Is`/`errors.As`, never bare `==`. Low-cardinality messages — variable data goes into `slog.Error("msg", "key", value)`, NOT into the message string.
- Logs: `log/slog` JSON to stderr. Claude Code picks it up. `slog.Info` / `Warn` / `Error` only — no `fmt.Fprintf(os.Stderr, ...)`.
- Modern Go: `slices.Contains`, `strings.Cut`, `strings.Lines`, `maps.Copy`, `range len(x)`. The `modernize` linter is enabled.
- Pointer vs value receivers: `Store`, `Bot`, `Server` all carry mutexes — pointer receivers throughout. Value-type receivers on `State`, `Pending`, `GroupPolicy` (no mutex).
- HTTP: NEVER `http.DefaultClient` for outbound. Use the package-level `fileClient` with timeout. Always pass `ctx`.

### Lint config (`.golangci.yml`)

49 enabled linters, 0 expected findings. Disabled (with rationale inline): `paralleltest` (httptest mocks share state), `dupl` (table tests look duplicated), `goconst` (short repeated strings not worth factoring), `wrapcheck`/`err113`/`mnd`/`iface` (too noisy as defaults). funlen 200/120, gocyclo 20 — the gate switch and handler dispatchers are intentionally wide.

### Tests

- TDD when feasible. Failing test first, minimal pass, refactor.
- Table-driven for any function with >2 input cases. Each case gets a `name` string and a `t.Run(tt.name, ...)`.
- `require` for failure-stop assertions (length checks before indexing). `assert` for invariant accumulation.
- No `t.Parallel()` — our tests share httptest servers and env vars.
- New code without a test gets pushback unless it's wiring (cmd/server entry).

### What NOT to do

- Don't import `mcp` from `bot`. Use the `Notifier` interface.
- Don't add a third package. The current four are the bottom of the carving.
- Don't reintroduce `fmt.Fprintf(os.Stderr, ...)`. slog only.
- Don't commit `.env`, `bin/`, `bot.pid`, `*.log`, anything under `.claude/channels/telegram/`.
- Don't bypass the gate. Every outbound `assertAllowedChat` / inbound `gate()` call exists because the TS predecessor had vulnerabilities here.
- Don't silently swallow errors. Either return wrapped, or `slog.Error` + explicit reason for the swallow (`//nolint:nilerr` with a comment).

## Gotchas

- **fasthttp/telego goroutine leak** is a known upstream limitation; `goleak.IgnoreAnyFunction` masks it in `TestMain`. Don't add more ignores without a strong reason.
- **`real` shadows the builtin** — use `resolved` / `realPath` for `filepath.EvalSymlinks` results. revive's `redefines-builtin-id` catches this.
- **`golangci-lint --fix` rewrites `fmt.Errorf("plain string")` → `errors.New(...)` but doesn't add the import**. Re-run build after `make lint-fix` and add `errors` if needed.
- **Telego `MessageID` is `int`**, but `Chat.ID` / `User.ID` are `int64`. Don't crosswire — strconv.Atoi vs strconv.ParseInt.
- **`bot.pid` claim is comm-checked** — only PIDs whose `/proc/<pid>/comm` is `telegram-mcp` or `bun` get SIGTERMed. Anything else is left alone. Same logic prevents PID recycling from making us murder an unrelated user process.

## Skills

Source — `.agents/skills/` (37 skills, lockfile `skills-lock.json`). Re-run `bash scripts/install-skills.sh` after fresh clone. Skill tool **does not** see local skills by name — open via `Read .agents/skills/<slug>/SKILL.md`.

**Must invoke** (project invariants):

- `mcp-builder` — any change to MCP tool surface or notification handlers
- `test-driven-development` / `tdd` — every code task
- `systematic-debugging` / `diagnose` — any bug/test failure before fix
- `golang-error-handling` — anywhere you create/wrap/log errors
- `golang-concurrency` — anywhere you spawn a goroutine or share state

**Companion pairs (NOT overlaps):** `test-driven-development` + `tdd` · `systematic-debugging` + `diagnose` · `requesting-code-review` + `receiving-code-review` + `grill-me`.

Match by task essence not keywords. Multiple skills may match — invoke all. Invoke before work, not after.

## Out of scope

- Webhooks. Long-polling only — runs behind any NAT, no public ingress.
- Multi-user / multi-tenant. Single-poller, single bot token by design.
- Database. State is a JSON file. Pairing is small enough to keep in RAM.
- Metrics / tracing. `slog` is the observability surface. pprof is one Go import away if we ever need it.

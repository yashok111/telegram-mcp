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
cmd/server/
  main.go                       entry, mode dispatch (daemon|shim|self), env, PDEATHSIG, dotenv
  self.go                       SessionStart hook + statusline renderer (PPID-walk to find CC pid)
internal/access/                access.json schema + atomic save + corrupt recovery
  access.go                       State, DMPolicy/Pending/GroupPolicy, atomic Save
  rules.go                        PermissionRule + Match (specificity-scored) + PruneRules + glob `**` matcher
internal/bot/                   telego long-poller, gate, handlers, outbound API
  bot.go                          ~1100 LOC: Poll, handleCommand/Message/Callback, gate, send*
  markdown.go                     EscapeMarkdownV2 + EscapeMarkdownV2Code helpers
  sessions.go                     status dashboard + /sessions switcher (mention/pin/evict)
  rules_cmd.go                    /rules list|clear|revoke + addRuleAndResolve callback helper
internal/chunk/                 4096-cap message splitter (length or newline boundaries)
internal/mcp/                   stdio MCP server, tool registry, notification surface
  mcp.go                          tools: reply, react, edit_message, download_attachment, telegram_peers
  peers.go                        telegram_peers tool (lists connected shims via PeerProvider)
internal/ipc/                   shim<->daemon JSON-RPC over unix socket (line-framed)
  proto.go, codec.go              wire format (Request/Response/Notify, base64 binary payloads)
  client.go, server.go            Dial/Listen, OnConnect/OnDisconnect, Call/Notify
internal/daemon/                bot-owner process: holds telego, routes, fans out
  daemon.go                       Run lifecycle, PID claim, evict-old-daemon, idle exit
  routing.go                      Router: replyRing (reply_to → shim), chat affinity, mention/pin/LRU
  handlers.go                     IPC method handlers (send*, react, edit, download, peers, broadcast_permission)
  notifier.go                     bot.Notifier impl that fans messages out through Router
  idle.go                         30-min idle timer (override via TELEGRAM_DAEMON_IDLE_EXIT)
  rules_cleanup.go                1-min ticker: PruneRules + Save when anything expired
  aliases.go, mentions.go         @s1/@s2 alias allocation + mention parsing
  log_redirect.go                 dup stderr to daemon.log when shim-spawned and stderr isn't a tty
internal/shim/                  per-CC-session process: speaks MCP up, IPC down
  shim.go                         Run lifecycle, hello handshake, reconnectLoop (backoff)
  spawn.go                        EnsureDaemon: dial existing or fork-detached daemon
  botadapter.go                   bot.BotAPI impl that forwards calls over IPC to the daemon
  notifier.go                     receives daemon push notifs, forwards to mcp.Server
  sessionfile.go                  writes ~/.claude/channels/telegram/sessions/<cc_pid>.json
contrib/systemd/                user-mode unit for keeping the daemon alive across reboots
contrib/hooks/                  session-start.sh wrapper for `telegram-mcp self --hook`
docs/superpowers/plans/         dated PRDs for shipped features (read-only history, not authoritative)
.agents/skills/                 37 curated skills (gitignored, lockfile in repo)
scripts/                        install-skills.sh, install-hooks.sh, pre-commit
```

**Path discipline:**
- `internal/bot` is the only place that talks to telego. The daemon attaches a `Notifier` and is the sole caller of `bot.SendMessage`/`SendPhoto`/`EditMessage`/`React`/`DownloadFile`.
- `internal/mcp` knows nothing about telego: it depends on `bot.BotAPI` (for tool semantics + `bot.SendOpts`/`bot.PermissionDetails` types) and is fed by `shim.BotAdapter` (forwards over IPC) — never wired to `bot` directly.
- `bot` MUST NOT import `mcp`, `daemon`, `shim`, or `ipc`. It calls back via its own `Notifier` interface.
- `daemon` ↔ `shim` communicate ONLY through `internal/ipc`. Neither imports the other.
- `cmd/server` only wires; no business logic. `runDaemon`, `runShim`, `runSelf` live there.

## Lifecycle

Two roles, daemon+shim. The legacy embedded mode (single-process, in-CC poller) was removed in #16 — there is no longer a code path where the shim runs the bot directly. Don't reintroduce it; the routing model assumes a separate daemon.

1. **Shim (per Claude Code session)**: the binary launched by Claude Code with no args. Speaks stdio MCP to Claude Code; speaks IPC to the daemon. PR_SET_PDEATHSIG ties it to the parent CC session so it dies with Claude Code. On daemon disconnect, the shim reconnects with capped exponential backoff and re-issues `hello` — it does NOT exit to force a CC re-spawn.
2. **Daemon (one per host)**: owns the bot token, runs the long-poller, holds the access gate, routes inbound traffic to the right shim. Spawned fork-detached by the first shim if not already running (or by systemd via `contrib/systemd/telegram-mcp.service`). Survives shim disconnects; idles out 30 minutes after the last shim leaves.
3. **Self (`telegram-mcp self`)**: read-only context renderer for the SessionStart hook + statusline; does not touch the bot or daemon.

`cmd/server/main.go:selectMode` decides which role to run: explicit `daemon`/`shim`/`self` subcommand wins, otherwise `TELEGRAM_DAEMON=1` env → daemon, else shim. Auto-detect after dotenv load (PR #4).

Ctx-driven shutdown everywhere. `Poll` exits within ~2s of `ctx.Done()` via `StopWithContext`. `approvalLoop` is a 5s ticker that respects `ctx.Done()`. The shim's `reconnectLoop` and daemon's idle timer both gate on ctx too — no orphan goroutines.

## Daemon

Single daemon per host; every Claude Code session attaches to it via shim.

**Subcommands:** `telegram-mcp daemon` (run daemon foreground), `telegram-mcp shim` (run shim explicitly), `telegram-mcp` (no args; auto-detects — defaults to shim, auto-spawns daemon on first run).

**Routing (priority order, see `daemon/routing.go`):**
1. **Reply-based** (PR #15) — if the inbound Telegram message has `reply_to_message_id`, look it up in the per-chat `replyRing` (LRU of recent outbound `(shimID, messageID)` pairs). If we sent that message, route to that shim. This is the primary mechanism for multi-shim disambiguation; the user simply replies in Telegram to the message they want answered.
2. **Mention-based** — tokens of the form `@<word>` (grammar `[A-Za-z0-9_-]+`, case-insensitive) route to one or more shims. Resolution order inside the mention step:
   - `@all` → broadcast to every connected shim.
   - exact alias match (`@s1`, `@s2`, …) → that shim.
   - `Shim.Label` match (case-insensitive) → every shim with that label. Example: `/label main-bot` makes the shim addressable as `@main-bot`.
   - Alias **wins** over a same-named label.
   - Multiple shims sharing one label → fan-out (broadcast to all of them) plus a `slog.Warn`.
   - Labels containing characters outside `[A-Za-z0-9_-]` are not addressable by `@mention`; they still show in `/sessions` and `telegram_peers`. A separate `/use <prefix>` DM command pins a chat to a specific shim by shim_id prefix.
3. **Chat affinity** — chat→shim pin set by previous outbound, with TTL.
4. **LRU fallback** — most-recently-connected shim if no other rule matches.
5. **Permission replies** route by `request_id` regardless of chat (registered in `permRegistry` at broadcast time).

**EnsureDaemon split-brain fix (PR #12):** `shim.EnsureDaemon` dials `daemon.sock` first; only if dial fails does it `fork+exec` a new daemon. The daemon refuses to start if `daemon.pid` is held by a live `telegram-mcp` process (`daemon.go:claimPID`), and on stale PIDs it evicts the old daemon via SIGTERM with a bounded wait. Prevents two daemons fighting over the bot token after systemd restart.

**Auto-reconnect (PRs #13, #14):** if the daemon dies or rotates, `shim.reconnectLoop` reattaches with capped backoff and replays `hello`. `botadapter` queues calls during reconnect (bounded; surfaces `daemon unavailable` after the cap). MCP serve is NOT cancelled — the CC session stays alive.

**Daemon owns the bot token.** Shims never see it. Daemon enforces the access.json gate authoritatively — every IPC handler calls `Handlers.gate(chatID)` before forwarding to the bot. Shim-side checks are convenience only.

**Idle exit:** daemon dies 30 minutes after the last shim disconnects. Override with `TELEGRAM_DAEMON_IDLE_EXIT=<seconds>`; `=0` disables.

**Rules cleanup:** background goroutine pulls `access.State`, calls `access.PruneRules`, and saves once a minute. Belt to the suspenders of the in-process `Match()` filter that already skips expired rules on each `permission_request` — keeps `/rules list` honest and disk size bounded over long runs.

**Files:**
- `~/.claude/channels/telegram/daemon.sock` (0600) — IPC unix socket
- `~/.claude/channels/telegram/daemon.pid` — daemon's PID (comm-checked before any SIGTERM)
- `~/.claude/channels/telegram/daemon.log` — daemon stderr when shim-spawned (systemd captures it via journal otherwise)
- `~/.claude/channels/telegram/sessions/<cc_pid>.json` — per-shim session snapshot for `self`

**Systemd alternative:** install `contrib/systemd/telegram-mcp.service` to keep the daemon alive across reboots and outside any Claude Code session.

**Background tasks (`/bg`):** DM `/bg <prompt> [--in <dir>]` spawns a one-shot `claude --print --output-format=stream-json --verbose` in the resolved workdir. Daemon edits a single progress message every `EditThrottle` (default 5s) and sends the final result chunked + cost summary on completion. `/bg list` and `/bg cancel <id>` manage in-flight tasks. Cancellation is SIGTERM → 5s wait → SIGKILL. `BgRunner.Stop()` is called from `runDaemon`'s defer chain so a daemon shutdown cancels every in-flight task instead of orphaning them until their 30-minute timeout. Implemented in `internal/daemon/bg.go` (`BgRunner` satisfies `bot.BgRunner`); per-call dispatch lives in `internal/bot/bg.go:handleBgCommand`. Env vars: `TELEGRAM_BG_MAX_PARALLEL` (default 3), `TELEGRAM_BG_TIMEOUT` (default 30m), `TELEGRAM_BG_DEFAULT_WORKDIR` (else `$HOME`), `TELEGRAM_BG_RATE_PER_HOUR` (per-user, default 10), `TELEGRAM_BG_CLAUDE_BIN` (default `"claude"`). Integration test in `internal/daemon/bg_integration_test.go` (build tag `integration`).

**`/bg` threat model:** `--in <dir>` is passed unvalidated to `cmd.Dir`, and the spawned `claude` inherits the daemon's UID. The defense is the allowlist: only DMs from senders in `access.json.AllowFrom` reach `handleBgCommand` (the same gate that protects every other command). The bot does not run as root, the daemon does not run as root, and per the single-bot-token / single-user design (see "Out of scope"), the trusted set is "operators of this daemon instance." Stderr tail (≤2 KB) is forwarded to Telegram on failure; if `claude` is configured with secrets that emit on stderr (e.g., proxy auth failure dumps), operators should be aware that those bytes land in the chat.

## MCP tool surface

Registered in `internal/mcp/mcp.go:registerTools` via `s.srv.AddTool`:

- `reply` — send text or files to a chat; auto-chunks at 4096; honors `reply_to` for quote-replies
- `react` — set/clear emoji reaction on a message
- `edit_message` — edit a previously-sent message (caption for media, text for plain)
- `download_attachment` — fetch a file_id from Telegram CDN into `~/.claude/channels/telegram/inbox/`
- `telegram_peers` (PR #9) — list other shims connected to this daemon (alias, shim_id, workdir, idle_seconds). Used by `@s2 do X` flows where one shim wants to know what's online.

Any change to this surface MUST go through the `mcp-builder` skill — see `Skills` section.

## Permission auto-approve

CC sends `notifications/claude/channel/permission_request` for every Bash/Edit/Read/etc. tool call that needs human approval. By default the shim's `mcp.Server` fans the prompt out to allowlisted DMs via the daemon's `bot.BroadcastPermissionRequest`. Three escape hatches short-circuit the round-trip:

1. **Inline buttons on the prompt** (extended in this PR): in addition to the legacy `✅ Allow` / `❌ Deny` / `ℹ See more`, every prompt now offers `⏳ Allow <Tool> 1h`, `♾ Always allow <Tool>`, and `🚫 Always deny <Tool>`. Tapping one writes a `PermissionRule` into `access.json` and resolves the current request in the same click.
2. **`access.Match`** (`internal/access/rules.go`) — called inside `mcp.Server.handlePermissionRequest` BEFORE the broadcast. Specificity scoring: exact tool (+2), `path_pattern` present (+1 plus `len(pattern)/10`), recency tiebreak. Path is sniffed out of the `input_preview` blob via the `pathFieldRE` regex (`file_path` / `path` / `notebook_path` / `pattern`, quoted or bare). A match → `ResolvePermission("allow"|"deny")` and no IPC traffic.
3. **`/rules` command** — list active rules (TTL countdown), `/rules clear`, `/rules revoke <id>`. DM-only via the existing `handleCommand` gate.

Rules with `expires_at > 0` are pruned by the daemon's 1-minute ticker; the per-request `Match` already skips them so a stale rule never grants access between ticks.

The MCP surface declares `claude/channel/permission` in its experimental capabilities (`mcp.go:New`) — only valid because gate()/allowlist authenticates the replier. Don't re-enable this declaration in any context where senders are unauthenticated.

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
- `internal/ipc` is exercised via real `net.UnixConn` pairs in `client_server_test.go` — no mock socket.
- `internal/shim` integration: `shim_test.go` spins a real `ipc.Server` in-process and verifies hello/reconnect/rewire against it. The shim's IPC client is the real one — only the upstream bot is faked via the daemon's test handlers.
- `internal/daemon/integration_test.go` runs the full triangle: real `ipc.Server`, two real `Shim`s, a fake `botSurface`, and the real `Router`. Exercises reply/mention/affinity routing end-to-end.
- Tests use `t.TempDir()` + `t.Setenv()` exclusively — no `os.Setenv` survives across tests.

**Coverage (414 tests, ~84% project LOC):** chunk 100% · access 91% · bot 89% · daemon 87% · mcp 85% · ipc 81% · shim 72% · cmd/server 45%. The cmd/server gap is `main.run()` wiring (subprocess execution to cover) and `Bot.Poll()` (live Telegram). Not worth the scaffolding. Re-check with `go test -count=1 -cover ./...` before claiming a coverage change.

## Rules

### Code

- Comments default to **none**. Only write when WHY is non-obvious. Don't explain WHAT — names and types do that.
- Errors: wrap with `fmt.Errorf("...: %w", err)`, lowercase, no trailing punctuation. Use `errors.Is`/`errors.As`, never bare `==`. Low-cardinality messages — variable data goes into `slog.Error("msg", "key", value)`, NOT into the message string.
- Logs: `log/slog` JSON to stderr. Claude Code picks it up. `slog.Info` / `Warn` / `Error` only — no `fmt.Fprintf(os.Stderr, ...)`.
- Modern Go: `slices.Contains`, `strings.Cut`, `strings.Lines`, `maps.Copy`, `range len(x)`. The `modernize` linter is enabled.
- Pointer vs value receivers: anything with a mutex or a long-lived resource (`Store`, `Bot`, `mcp.Server`, `ipc.Server`/`Client`, `daemon.Daemon`/`Router`, `shim.Shim`/`BotAdapter`) gets pointer receivers throughout. Value-type receivers on plain data (`State`, `Pending`, `GroupPolicy`, `ipc.Request`/`Response`).
- HTTP: NEVER `http.DefaultClient` for outbound. Use the package-level `fileClient` with timeout. Always pass `ctx`.

### Lint config (`.golangci.yml`)

50 enabled linters, 0 expected findings. Disabled (with rationale inline): `paralleltest` (httptest mocks share state), `dupl` (table tests look duplicated), `goconst` (short repeated strings not worth factoring), `wrapcheck`/`err113`/`mnd`/`iface`/`varnamelen`/`exhaustruct` (too noisy as defaults). funlen 200/120, gocyclo 20 — the gate switch and handler dispatchers are intentionally wide.

### Tests

- TDD when feasible. Failing test first, minimal pass, refactor.
- Table-driven for any function with >2 input cases. Each case gets a `name` string and a `t.Run(tt.name, ...)`.
- `require` for failure-stop assertions (length checks before indexing). `assert` for invariant accumulation.
- No `t.Parallel()` — our tests share httptest servers and env vars.
- New code without a test gets pushback unless it's wiring (cmd/server entry).

### What NOT to do

- Don't import `mcp` from `bot`, or `daemon`/`shim` from each other. The dependency arrows are: `cmd/server` → {`daemon`, `shim`, `mcp`, `bot`, `access`, `ipc`}; `daemon` → {`bot`, `access`, `ipc`}; `shim` → {`mcp`, `bot` (for types), `ipc`}; `mcp` → {`bot` (for types only)}; `bot` → {`access`, `chunk`}. Don't introduce a new edge.
- Don't add an 8th internal package. The current seven are the bottom of the carving — if something doesn't fit, it usually belongs in `daemon` or `bot`.
- Don't reintroduce embedded mode (bot poller inside the shim/CC process). Removed in #16; routing assumes a separate daemon and adding it back means undoing the IPC layer.
- Don't reintroduce `fmt.Fprintf(os.Stderr, ...)`. slog only.
- Don't commit `.env`, `bin/`, `bot.pid`, `*.log`, anything under `.claude/channels/telegram/`.
- Don't bypass the gate. Every outbound `assertAllowedChat` / inbound `gate()` call exists because the TS predecessor had vulnerabilities here. The daemon-side `Handlers.gate` is the load-bearing one — shim-side checks are convenience only.
- Don't silently swallow errors. Either return wrapped, or `slog.Error` + explicit reason for the swallow (`//nolint:nilerr` with a comment).
- Don't make the shim exit on daemon disconnect to force a CC re-spawn — #13 replaced that with reconnect-with-backoff. Exiting kills the CC session's MCP server, which is user-hostile.

## Gotchas

- **fasthttp/telego goroutine leak** is a known upstream limitation; `goleak.IgnoreAnyFunction` masks it in `TestMain`. Don't add more ignores without a strong reason.
- **`real` shadows the builtin** — use `resolved` / `realPath` for `filepath.EvalSymlinks` results. revive's `redefines-builtin-id` catches this.
- **`golangci-lint --fix` rewrites `fmt.Errorf("plain string")` → `errors.New(...)` but doesn't add the import**. Re-run build after `make lint-fix` and add `errors` if needed.
- **Telego `MessageID` is `int`**, but `Chat.ID` / `User.ID` are `int64`. Don't crosswire — strconv.Atoi vs strconv.ParseInt. The IPC wire format uses string `chat_id` and int `message_id` for the same reason.
- **`daemon.pid` and `bot.pid` claims are comm-checked** — only PIDs whose `/proc/<pid>/comm` is `telegram-mcp` (or `bun`, for legacy TS-plugin handoff) get SIGTERMed. Anything else is left alone. Prevents PID recycling from making us murder an unrelated user process.
- **Routing key is `reply_to_message_id`, not chat** — if a user types a fresh message in a chat that has multiple connected shims pinned/affinitied, it goes to the LRU/mention/pinned shim. To direct a reply at a specific shim's prior message, the user must use Telegram's "reply to" UI. Tests that assert "next inbound goes to shim X" must set `reply_to_message_id` explicitly.
- **IPC reconnect drops in-flight calls** — `botadapter` waits briefly for the reconnect to land, but a long disconnection surfaces `daemon unavailable` to the MCP tool caller. The daemon-side response was lost; the user will see the tool error and can retry.
- **Session file race** — `telegram-mcp self --hook` can fire before `shim.Wire()` writes `sessions/<cc_pid>.json`. It must print a fallback and exit 0; never abort the CC session. The session file is removed on shim exit, so its absence after that is correct, not an error.
- **PR_SET_PDEATHSIG only on shim** — `cmd/server/main.go:bindParentDeath` skips it in daemon mode (#3 / commit 50c8773). The daemon's parent is systemd or the original shim's grandparent shell, and dying with either is wrong.

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

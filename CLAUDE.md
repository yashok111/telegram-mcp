# CLAUDE.md

Claude Code repo instructions for **telegram-mcp** — local Go MCP server bridging Telegram <-> Claude Code. Replaces the bun-runtime `external_plugins/telegram` plugin. Single binary, PR_SET_PDEATHSIG, drop-in compatible with the TS plugin's `~/.claude/channels/telegram/` state.

> Operator-facing reference (full env-var defaults, setup, feature walkthroughs) lives in **README.md**. This file holds architecture invariants, gotchas, and "don't do X" rules for editing the code — it deliberately keeps only env-var *names* and points to README for values.

> Project **decision history** — ADRs + upstream (Claude Code changelog) reviews — lives in **Notion**: `HQ ▸ Projects ▸ telegram-mcp ▸ {ADRs, Upstream Reviews}`. After assessing a CC update, log a review entry there; record an architecture call as an ADR. Data-source IDs + the logging convention are in agent memory `notion-decision-log` (kept out of this committed file).

## Stack

Go **1.26** · `github.com/mark3labs/mcp-go` v0.54 (stdio MCP server) · `github.com/mymmrac/telego` v1.9 (Telegram bot, long-polling) · `log/slog` JSON to stderr · `go.uber.org/goleak` in every test pkg. No DB, no cache — a single daemon owns the bot token; each Claude Code session attaches via a stdio shim.

## Commands

```bash
make build              # → bin/telegram-mcp (trimpath + ldflags -s -w)
make test               # go test -race ./...
make lint               # golangci-lint v2 (built from source w/ Go 1.26)
make lint-fix
make check              # lint + test + build (CI gate)
make deploy             # check + `systemctl --user restart` + post-deploy healthcheck
make health             # post-deploy runtime checks against the LIVE daemon (exit-coded)

bash scripts/install-skills.sh   # → .agents/skills/ (37 skills, lockfile)
bash scripts/install-hooks.sh    # → .git/hooks/pre-commit
bash scripts/healthcheck.sh      # runtime health check; --full prepends `make check`
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
  idle.go                         7-day idle timer (override via TELEGRAM_DAEMON_IDLE_EXIT)
  rules_cleanup.go                1-min ticker: PruneRules + Save when anything expired
  aliases.go, mentions.go         @s1/@s2 alias allocation (sticky per workdir/label) + mention parsing
  orphan_sweep.go                 closes forum topics whose owner left past TELEGRAM_TOPIC_ORPHAN_AFTER
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
2. **Daemon (one per host)**: owns the bot token, runs the long-poller, holds the access gate, routes inbound traffic to the right shim. Spawned fork-detached by the first shim if not already running (or by systemd via `contrib/systemd/telegram-mcp.service`). Survives shim disconnects; idles out 7 days after the last shim leaves.
3. **Self (`telegram-mcp self`)**: read-only context renderer for the SessionStart hook + statusline; does not touch the bot or daemon.

`cmd/server/main.go:selectMode` decides which role to run: explicit `daemon`/`shim`/`self` subcommand wins, otherwise `TELEGRAM_DAEMON=1` env → daemon, else shim. Auto-detect after dotenv load.

Ctx-driven shutdown everywhere. `Poll` exits within ~2s of `ctx.Done()` via `StopWithContext`. `approvalLoop` is a 5s ticker that respects `ctx.Done()`. The shim's `reconnectLoop` and daemon's idle timer both gate on ctx too — no orphan goroutines.

## Daemon

Single daemon per host; every Claude Code session attaches to it via shim.

**Subcommands:** `telegram-mcp daemon` (run daemon foreground), `telegram-mcp shim` (run shim explicitly), `telegram-mcp` (no args; auto-detects — defaults to shim, auto-spawns daemon on first run).

**Routing (priority order, see `daemon/routing.go`):**
1. **Reply-based** — if the inbound has `reply_to_message_id`, look it up in the per-chat `replyRing` (LRU of recent outbound `(shimID, messageID)` pairs). If we sent that message, route to that shim. Primary mechanism for multi-shim disambiguation: the user replies in Telegram to the message they want answered.
2. **Mention-based** — `@<word>` tokens (grammar `[A-Za-z0-9_-]+`, case-insensitive). Resolution order:
   - `@all` → broadcast to every connected shim.
   - exact alias match (`@s1`, `@s2`, …) → that shim.
   - `Shim.Label` match (case-insensitive) → every shim with that label. `/label main-bot` makes the shim addressable as `@main-bot`.
   - Alias **wins** over a same-named label.
   - Aliases are **sticky**: an `@sN` is bound to a project's reuse-key (`workdir:`/`label:`) and persisted in `access.json → alias_by_key`, so a session keeps the same alias across reconnects and daemon restarts; an offline project's number is reserved. Wired via `Router.SetStickyAliasStore` at boot; nil store (tests) falls back to legacy lowest-free allocation.
   - Multiple shims sharing one label → fan-out (broadcast) plus a `slog.Warn`.
   - Labels with chars outside `[A-Za-z0-9_-]` aren't `@mention`-addressable; they still show in `/sessions` and `telegram_peers`. `/use <prefix>` DM pins a chat to a shim by shim_id prefix.
3. **Chat affinity** — chat→shim pin set by previous outbound, with TTL.
4. **LRU fallback** — most-recently-connected shim if no other rule matches.
5. **Permission replies** route by `request_id` regardless of chat (registered in `permRegistry` at broadcast time).

**EnsureDaemon split-brain fix:** `shim.EnsureDaemon` dials `daemon.sock` first; only if dial fails does it `fork+exec` a new daemon. The daemon refuses to start if `daemon.pid` is held by a live `telegram-mcp` process (`daemon.go:claimPID`), and on stale PIDs evicts the old daemon via SIGTERM with a bounded wait. Prevents two daemons fighting over the bot token after systemd restart.

**Auto-reconnect:** if the daemon dies or rotates, `shim.reconnectLoop` reattaches with capped backoff and replays `hello`. `botadapter` queues calls during reconnect (bounded; surfaces `daemon unavailable` after the cap). MCP serve is NOT cancelled — the CC session stays alive.

**Daemon owns the bot token.** Shims never see it. Daemon enforces the access.json gate authoritatively — every IPC handler calls `Handlers.gate(chatID)` before forwarding to the bot. Shim-side checks are convenience only.

**Idle exit:** daemon dies 7 days after the last shim disconnects. Override with `TELEGRAM_DAEMON_IDLE_EXIT=<seconds>`; `=0` (or any non-positive) disables. Unset / unparseable falls back to the 7-day default (`cmd/server.resolveIdleTimeout`, logs one `slog.Warn` on unparseable).

**Rules cleanup:** background goroutine pulls `access.State`, calls `access.PruneRules`, saves once a minute. Belt-and-suspenders to the in-process `Match()` filter (which already skips expired rules per request) — keeps `/rules list` honest and disk bounded.

**Typing tracker (`internal/daemon/typing.go`):** keeps the chat-action bubble alive + rotates a reaction emoji on the inbound while the agent thinks; `Done()` swaps a ✓-style emoji on the shim's first outbound. Env (`cmd/server.loadTypingConfig`): `TELEGRAM_TYPING_REFRESH` (off-switch), `TELEGRAM_TYPING_TTL`, `TELEGRAM_TYPING_ROTATION_EMOJIS` (comma-list, empty disables rotation), `TELEGRAM_TYPING_DONE_EMOJI` (empty/`off` suppresses swap). **Caveat:** Telegram's `setMessageReaction` accepts only a curated emoji whitelist — anything off it (`⏳`,`💭`,`✅`…) returns `Bad Request: REACTION_INVALID`, surfaced as a `slog.Warn` (not a crash). Pick from their allowlist or check `daemon.log` after the first inbound.

**Topic headers (`internal/daemon/header.go`):** in forum mode every topic gets a pinned header message the daemon maintains — sent + pinned on topic creation (`HeaderManager.Ensure`, from the hello handler after `BindTopic`), edited in place as the owning shim's state changes. Render is plain text (NOT MarkdownV2 — workdir/label are `/._-`-heavy, escaping buys nothing and perturbs the dedup hash):

```
🟢 @s3 — telegram-mcp
workdir: /home/yakov/projects/telegram-mcp
label: (none)
status: idle
last activity: 12s ago
uptime: 2h 15m
shim: d35ae17c
```

State icons: 🟢 idle · 🟡 busy · 🔵 awaiting permission · ⚪ disconnected · 🔴 closed. **Busy is daemon-INFERRED, not shim-reported** — CC pushes only `permission_request` to MCP servers (no tool_start/end), so there's no IPC/MCP change. State rides existing hooks: `DeliverInbound`→🟡, `HandleBroadcastPermission`→🔵 (the ONLY state with a tool name, from `permDetails`), `ResolvePermission`→🟡, shim outbound `HandleSendMessage`/`HandleSendFile`→🟢 (**not** `HandleEditMessage` — edits are interim, would flicker), `OnDisconnect`→⚪ (must run before `Router.Drop` so `TopicForShim` still resolves), `CloseTopic`→🔴. **Concurrency:** `headerState`/`setShimHeaderState` resolve shim→topic via `Router.TopicForShim` behind `r.mu`; `flush` pulls identity (`Router.HeaderIdentity`) OUTSIDE `m.mu` to avoid a lock-order inversion with `SetLabel`→`headerHook`→`Refresh`. Persisted in `TopicMeta.{HeaderMessageID,HeaderPinned,HeaderRenderHash}` (restart re-binds the existing pin, doesn't resend); FNV-64a content hash skips no-op edits (and Telegram's "not modified" 400). Recovery: edit "message to edit not found" → recreate + re-pin; pin 400 (no `can_pin_messages`) → graceful degrade (`HeaderPinned=false`, retried on a later flush). Env (`cmd/server.buildHeaderManager`): `TELEGRAM_TOPIC_HEADER` (flag, default on), `TELEGRAM_TOPIC_HEADER_REFRESH`, `TELEGRAM_TOPIC_HEADER_TICK`. nil when forum off / flag off; every hook nil-safe. Accepted limits: busy carries no tool name; a shim that finishes without sending stays 🟡 till next inbound/disconnect; closed-topic edits rely on the bot's topic-admin rights (failure logs a warn, no crash).

**Topic naming + lifecycle:**
- **Title = project identity** (`label` or workdir basename), NOT the alias — `buildTopicName` (`forum.go`). The alias reshuffles across restarts, so embedding it in the persistent title made the list lie about "who is who"; the live `@sN` rides the pinned header instead. Legacy `@sN — x` titles self-migrate to project-only on next reuse (`resyncName`). Degenerate workdir (`/`, empty) falls back to the raw path or literal `session` — never the alias, so the title never churns.
- **Orphan sweep** (`internal/daemon/orphan_sweep.go`) — a topic whose owner disconnected (`LockedBy==""`, `ReleasedAt>0`) longer than `TELEGRAM_TOPIC_ORPHAN_AFTER` (default 12h, `=0` disables) is closed via `TopicCloser`, then deleted by `TopicSweep` after `TELEGRAM_TOPIC_PURGE_AFTER` (default 12h). Race-safe: an atomic `claim` re-checks `LockedBy` and strips the reuse key in one `store.Mutate`, so a shim reattaching in the same instant either wins the lock (claim bails) or gets a fresh topic (reuse key gone) — a live shim is never closed out. `ReleasedAt` is stamped in `Forum.ReleaseLock`, cleared on re-lock. `TOPIC_ID_INVALID` is classified permanent (`bot/apierr.go`) so both sweeps drop a gone topic instead of retrying forever.
- **Disconnected/closed headers freeze** — `HeaderManager.markActiveDirty` skips ⚪ and 🔴 topics, so the 60s uptime tick doesn't churn an `editMessageText` per interval on topics with no live owner.

**Files:**
- `~/.claude/channels/telegram/daemon.sock` (0600) — IPC unix socket
- `~/.claude/channels/telegram/daemon.pid` — daemon's PID (comm-checked before any SIGTERM)
- `~/.claude/channels/telegram/daemon.log` — daemon stderr when shim-spawned (systemd captures it via journal otherwise)
- `~/.claude/channels/telegram/sessions/<cc_pid>.json` — per-shim session snapshot for `self`

**Systemd alternative:** install `contrib/systemd/telegram-mcp.service` to keep the daemon alive across reboots and outside any Claude Code session.

**Deploy:** the canonical way to ship a change to the live daemon is `make deploy` — it runs `make check` (so a broken build never reaches the daemon), `systemctl --user restart $(SERVICE)`, then `scripts/healthcheck.sh` to verify the restart. The systemd unit's `ExecStart` points at the repo's `bin/telegram-mcp`, so `make build` alone (which `check` does) is enough to update the binary the unit launches. `make health` runs just the post-deploy checks against a running daemon (binary/unit/process/pid-file/socket/log-errors/shims/access.json; exit-coded). The shim + admin-agent reconnect on their own after the restart; in-flight `/spawn` children are cancelled by `spawnRunner.Stop()` on shutdown.

**Background tasks (`/bg`):** DM `/bg <prompt> [--in <dir>]` runs a one-shot `claude --print --output-format=stream-json --verbose` in the resolved workdir; the daemon edits one progress message every `EditThrottle` then sends the chunked result + cost on completion. `/bg list`, `/bg cancel <id>` (SIGTERM→5s→SIGKILL). `BgRunner.Stop()` runs in `runDaemon`'s defer chain so shutdown cancels in-flight tasks. Code `internal/daemon/bg.go` (`BgRunner` satisfies `bot.BgRunner`); dispatch `internal/bot/bg.go:handleBgCommand`. Env: `TELEGRAM_BG_{MAX_PARALLEL,TIMEOUT,DEFAULT_WORKDIR,RATE_PER_HOUR,CLAUDE_BIN}` — defaults + CLAUDE_BIN auto-resolution in README / `cmd/server.resolveClaudeBin`. Integration test `bg_integration_test.go` (build tag `integration`).

**`/bg` + `/spawn` threat model:** `--in <dir>` is passed unvalidated to `cmd.Dir` and the spawned `claude` inherits the daemon's UID. The defense is the allowlist: only DMs from `access.json.AllowFrom` senders reach the handler (same gate as every command). Daemon runs non-root; per the single-bot-token / single-user design (see "Out of scope") the trusted set is "operators of this daemon instance." Stderr tail (≤2 KB) is forwarded to Telegram on failure — if `claude` is configured with secrets that emit on stderr, those bytes land in the chat.

**Outbound source-alias prefix:** the daemon injects a `@sN: ` prefix on every shim-originated message at the IPC handler boundary so the user sees which session replied without consulting `/sessions`. `HandleSendMessage`/`HandleEditMessage` prepend text; `HandleSendFile` sets `SendOpts.Caption=@sN` (no colon — captions can't lead with whitespace); `HandleBroadcastPermission` takes the prefix as a new first arg. Lookup `Router.AliasForShim`; an empty alias (pre-hello, ghost connection, opt-out) → no prefix. Daemon-direct outbound (`/bg`,`/spawn`,`/sessions`,`/status`,`/rules`, pairing) stays unprefixed; `react` unaffected. **Load-bearing:** the shim reserves `sourcePrefixReserve=16` bytes per chunk in `mcp.resolveChunkOpts` so the daemon's per-chunk prepend never overflows 4096 (and every chunk in a split carries the marker) — keep this if you touch chunk sizing. Aliases are `s`+digits → MarkdownV2-safe, no escape. Opt-out `TELEGRAM_PREFIX_ALIAS=0|false|no|off` (daemon-wide; the shim's 16-byte reserve stays regardless, decoupling it from daemon runtime state). Helpers `internal/daemon/prefix.go`.

**Per-chat effort (`/effort`):** DM `/effort <low|medium|high|xhigh|max|ultra>|show|clear` sets the model + `MAX_THINKING_TOKENS` for *future* `/spawn` + `/bg` from that chat. Map (`internal/bot/effort.go:effortConfigs`): low→haiku-4-5+0, medium→sonnet-4-6+8000, high→opus-4-8+16000, xhigh→opus-4-8+32000, max→opus-4-8+64000, ultra→fable-5+64000. Persisted as `State.EffortByChat[chatID]=levelName` (level string, not resolved model, so future remaps survive). `handleSpawnCommand`/`handleBgCommand` call `ResolveEffort` → fill `SpawnRequest.{Model,ThinkingTokens}` (zero = daemon defaults); the daemon translates to `--model=` argv + `MAX_THINKING_TOKENS=` env (`filterEnv` strips an inherited dup; `--model` appended via `slices.Clone` so concurrent `/spawn` don't race the shared base slice). Unknown stored level → `ResolveEffort ok=false` → defaults. `/status` renders it via `renderEffortLine`. Existing shims keep their boot-time model till respawn. Gated by `access.json.AllowFrom`.

**Daemon-spawned CC clients (`/spawn`):** DM `/spawn [--in <dir>]` forks a fresh `claude --dangerously-load-development-channels plugin:telegram@<channel>` via `pty.New()`+`pty.CommandContext` (go-pty) in the resolved workdir. The CC loads the telegram plugin → plugin starts its own shim → shim does `hello` → `Router.Register` allocates a fresh `@sN`. From there it's a **regular shim** (addressable via `@alias`/`reply_to`/`/sessions`/`/use`/`/label`; no special routing). The pty exists only because real `claude` refuses to start without a TTY; its output drains to `io.Discard` (real traffic flows through the shim's MCP/IPC). **Auto-confirm gotcha (#33):** `--dangerously-load-development-channels` renders a blocking consent splash — `execSpawnCommander.Start` writes `\r` into the pty master 6× @500ms so the default "1. I am using this for local development" option is selected, else the plugin never loads and no shim appears. Spawn↔shim linkage via `TELEGRAM_SPAWN_ID` env (daemon stamps before `cmd.Start()` → shim echoes in `hello` → `Shim.SpawnID`); `/spawn list` cross-refs `Router.Snapshot()`. `/spawn cancel <id>` = SIGTERM→5s→SIGKILL→close pty. **Idle sweep** (`SpawnRunner.Run`, 1-min tick): cancels spawns whose shim idled past idle-timeout, plus orphans whose shim never registered (covers mid-bootstrap crashes). `SpawnRunner` owns only subprocess lifecycle. Code `internal/daemon/spawn.go`, dispatch `internal/bot/spawn.go:handleSpawnCommand`. Env: `TELEGRAM_SPAWN_{MAX_PARALLEL,HARD_TIMEOUT,IDLE_TIMEOUT,DEFAULT_WORKDIR,RATE_PER_HOUR,CLAUDE_BIN,CLAUDE_ARGS}` — CLAUDE_BIN/ARGS auto-resolved (`cmd/server.resolveClaudeBin` + `resolveSpawnPluginSpec`, logged once at startup; details in README). New IPC field `hello.spawn_id` (string, optional — empty for user-launched shims).

## MCP tool surface

Registered in `internal/mcp/mcp.go:registerTools` via `s.srv.AddTool`:

- `reply` — send text or files to a chat; auto-chunks at 4096; honors `reply_to` for quote-replies
- `react` — set/clear emoji reaction on a message
- `edit_message` — edit a previously-sent message (caption for media, text for plain)
- `download_attachment` — fetch a file_id from Telegram CDN into `~/.claude/channels/telegram/inbox/`
- `telegram_peers` — list other shims connected to this daemon (alias, shim_id, workdir, idle_seconds). Used by `@s2 do X` flows where one shim wants to know what's online.

Any change to this surface MUST go through the `mcp-builder` skill — see `Skills` section.

## Permission auto-approve

CC sends `notifications/claude/channel/permission_request` for every Bash/Edit/Read/etc. tool call needing approval. By default the shim's `mcp.Server` fans the prompt out to allowlisted DMs via the daemon's `bot.BroadcastPermissionRequest`. Three escape hatches short-circuit the round-trip:

1. **Inline buttons** — `✅ Allow` / `❌ Deny` / `ℹ See more`, plus `⏳ Allow <Tool> 1h` / `♾ Always allow <Tool>` / `🚫 Always deny <Tool>`. Tapping a rule-button writes a `PermissionRule` into `access.json` and resolves the current request in one click.
2. **`access.Match`** (`internal/access/rules.go`) — called inside `mcp.Server.handlePermissionRequest` BEFORE the broadcast. Specificity scoring: exact tool (+2), `path_pattern` present (+1 plus `len(pattern)/10`), recency tiebreak. Path is sniffed out of the `input_preview` blob via `pathFieldRE` (`file_path`/`path`/`notebook_path`/`pattern`, quoted or bare). A match → `ResolvePermission("allow"|"deny")`, no IPC traffic.
3. **`/rules` command** — list active rules (TTL countdown), `/rules clear`, `/rules revoke <id>`. DM-only via the `handleCommand` gate.

Rules with `expires_at > 0` are pruned by the 1-minute ticker; the per-request `Match` already skips them so a stale rule never grants access between ticks.

The MCP surface declares `claude/channel/permission` in its experimental capabilities (`mcp.go:New`) — only valid because gate()/allowlist authenticates the replier. Don't re-enable this declaration where senders are unauthenticated.

## CC self-context (SessionStart hook)

The agent must know its shim alias from turn 1 so `@s2 do X` mentions work without inbound metadata. The shim writes a snapshot keyed by CC's pid (`os.Getppid()`), tagged with `cc_session_id`. Correlation is **version-gated**: CC ≤2.1.153 didn't expose its session id to MCP subprocesses → `self` walks the PPID chain to the `<cc_pid>.json` filename; CC 2.1.154+ sets `CLAUDECODE=1` + `CLAUDE_CODE_SESSION_ID` in MCP/hook env (confirmed live 2026-05-28) → `self` prefers an exact `cc_session_id` match (robust against PID recycling / odd process trees), PPID walk as fallback.

1. **Shim side** — on `Wire()` success writes `~/.claude/channels/telegram/sessions/<cc_pid>.json` (mode 0600, atomic tmp+rename, removed when `Run()` exits). Schema: `{alias, shim_id, shim_id_prefix, cc_pid, shim_pid, cc_session_id?, workdir, label?, started_at, mode}`. `cc_session_id` comes from `CLAUDE_CODE_SESSION_ID` (empty + non-load-bearing on CC ≤2.1.153, which didn't set it for MCP subprocesses).

2. **CC side** — `telegram-mcp self` (`cmd/server/self.go`): with a trusted session id (`CLAUDECODE=1` gates trusting `CLAUDE_CODE_SESSION_ID`) it scans `sessions/*.json` for a matching `cc_session_id`, else walks the PPID chain (≤8 hops) for the first ancestor whose `/proc/<pid>/comm` starts with `claude` and reads `<cc_pid>.json`. Override with `CC_PID=<pid>`. Wire it as a SessionStart hook in `~/.claude/settings.json` (or use `contrib/hooks/session-start.sh`):

   ```json
   {
     "hooks": {
       "SessionStart": [
         { "hooks": [ { "type": "command", "command": "/abs/path/to/bin/telegram-mcp self --hook" } ] }
       ]
     }
   }
   ```

   `--hook` emits CC's `{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"..."}}` shape; without it, plain text is printed.

**Pre-Wire race**: `telegram-mcp self --hook` can fire before the shim writes `sessions/<cc_pid>.json` (or the file is unreadable) → it prints a fallback and exits 0. Hooks must never abort a CC session. The file is removed on shim exit, so its absence after that is correct, not an error.

**Statusline** — `telegram-mcp self --statusline` prints a compact `tg:@sN` tag (or empty if no session file). Compose into CC's `statusLine.command`; if you already have a custom statusline, wrap it so the `tg:` tag is appended when present and silently dropped otherwise.

   ```json
   { "statusLine": { "type": "command", "command": "/abs/path/to/bin/telegram-mcp self --statusline" } }
   ```

## Testing

`go.uber.org/goleak` in every package's `TestMain`. Ignored upstream leaks (documented inline): `fasthttp.HostClient.connsCleaner` / `Client.mCleaner` / `TCPDialer.tcpAddrsClean`, `telego.Bot.doLongPolling` (sleeps in backoff after ctx cancel).

**Mocking strategy:**
- `internal/mcp` uses a hand-rolled `fakeBot` matching `BotAPI`.
- `internal/bot/bot_api_test.go` runs a real httptest server impersonating `api.telegram.org/bot<TOKEN>/<method>`. `telego.WithAPIServer(URL)` points the client at it. File-CDN downloads route through `fileClient`, which tests swap to a `redirectTransport`.
- `internal/ipc` is exercised via real `net.UnixConn` pairs in `client_server_test.go` — no mock socket.
- `internal/shim` integration: `shim_test.go` spins a real `ipc.Server` in-process and verifies hello/reconnect/rewire against it. The shim's IPC client is the real one — only the upstream bot is faked via the daemon's test handlers.
- `internal/daemon/integration_test.go` runs the full triangle: real `ipc.Server`, two real `Shim`s, a fake `botSurface`, and the real `Router`. Exercises reply/mention/affinity routing end-to-end.
- Tests use `t.TempDir()` + `t.Setenv()` exclusively — no `os.Setenv` survives across tests.

**Coverage (816 tests, ~84% project LOC):** chunk 100% · access 91% · bot 89% · daemon 87% · mcp 85% · ipc 81% · shim 72% · cmd/server 45%. The cmd/server gap is `main.run()` wiring and `Bot.Poll()` (live Telegram) — not worth the scaffolding. Re-check with `go test -count=1 -cover ./...` before claiming a coverage change.

## Rules

### Code

- Comments default to **none**. Only write when WHY is non-obvious. Don't explain WHAT — names and types do that.
- Errors: wrap with `fmt.Errorf("...: %w", err)`, lowercase, no trailing punctuation. Use `errors.Is`/`errors.As`, never bare `==`. Low-cardinality messages — variable data goes into `slog.Error("msg", "key", value)`, NOT into the message string.
- Logs: `log/slog` JSON to stderr. Claude Code picks it up. `slog.Info` / `Warn` / `Error` only — no `fmt.Fprintf(os.Stderr, ...)`.
- Modern Go: `slices.Contains`, `strings.Cut`, `strings.Lines`, `maps.Copy`, `range len(x)`. The `modernize` linter is enabled.
- Pointer vs value receivers: anything with a mutex or long-lived resource (`Store`, `Bot`, `mcp.Server`, `ipc.Server`/`Client`, `daemon.Daemon`/`Router`, `shim.Shim`/`BotAdapter`) gets pointer receivers throughout. Value receivers on plain data (`State`, `Pending`, `GroupPolicy`, `ipc.Request`/`Response`).
- HTTP: NEVER `http.DefaultClient` for outbound. Use the package-level `fileClient` with timeout. Always pass `ctx`.

### Lint config (`.golangci.yml`)

50 enabled linters, 0 expected findings. Disabled (rationale inline): `paralleltest` (httptest mocks share state), `dupl` (table tests look duplicated), `goconst` (short repeated strings not worth factoring), `wrapcheck`/`err113`/`mnd`/`iface`/`varnamelen`/`exhaustruct` (too noisy). funlen 200/120, gocyclo 20 — the gate switch and handler dispatchers are intentionally wide.

### Tests

- TDD when feasible. Failing test first, minimal pass, refactor.
- Table-driven for any function with >2 input cases. Each case gets a `name` string and a `t.Run(tt.name, ...)`.
- `require` for failure-stop assertions (length checks before indexing). `assert` for invariant accumulation.
- No `t.Parallel()` — our tests share httptest servers and env vars.
- New code without a test gets pushback unless it's wiring (cmd/server entry).

### What NOT to do

- Don't import `mcp` from `bot`, or `daemon`/`shim` from each other. Direct internal imports are: `cmd/server` → {`daemon`, `shim`, `mcp`, `bot`, `admin`, `access`, `ipc`}; `daemon` → {`bot`, `access`, `chunk`, `ipc`}; `shim` → {`mcp`, `bot` (for types), `access`, `ipc`}; `mcp` → {`bot` (for types), `access`, `chunk`}; `bot` → {`access`}; `admin` → {`access`, `chunk`, `ipc`}. `access` and `chunk` are leaf utilities — they import no other internal package, and any layer above may import them. The role packages (`daemon`, `shim`, `mcp`, `bot`) must not gain new edges between each other; the existing edges above are the full set.
- The admin-agent lives in its own package `internal/admin` (8th package). It's a process-role peer of `shim`: speaks IPC to the daemon and never imports `daemon`/`bot`/`mcp`/`shim` (it mirrors `daemon.Event` rather than importing it; a wire-compat test guards the mirror). Only `cmd/server` imports it. Don't add a **9th** internal package — anything new should fit one of the eight, usually `daemon`, `bot`, or `admin`.
- Don't reintroduce embedded mode (bot poller inside the shim/CC process). Removed in #16; routing assumes a separate daemon and adding it back means undoing the IPC layer.
- Don't reintroduce `fmt.Fprintf(os.Stderr, ...)`. slog only.
- Don't commit `.env`, `bin/`, `bot.pid`, `*.log`, anything under `.claude/channels/telegram/`.
- Don't bypass the gate. Every outbound `assertAllowedChat` / inbound `gate()` call exists because the TS predecessor had vulnerabilities here. The daemon-side `Handlers.gate` is the load-bearing one — shim-side checks are convenience only.
- Don't silently swallow errors. Either return wrapped, or `slog.Error` + explicit reason for the swallow (`//nolint:nilerr` with a comment).
- Don't make the shim exit on daemon disconnect to force a CC re-spawn — #13 replaced that with reconnect-with-backoff. Exiting kills the CC session's MCP server, which is user-hostile.
- Don't commit on local `main` (or `master`). Every PR lands as a squash merge, so origin gets one commit while a local main with the original N commits diverges (ahead N, behind 1) and `git pull` refuses to fast-forward. Before the first `git commit`, run `git fetch origin main && git checkout -b feat/<short-name> origin/main`; after merge, sync with `git checkout main && git pull` (fast-forward works only because local main was untouched).

## Gotchas

- **fasthttp/telego goroutine leak** is a known upstream limitation; `goleak.IgnoreAnyFunction` masks it in `TestMain`. Don't add more ignores without a strong reason.
- **`real` shadows the builtin** — use `resolved` / `realPath` for `filepath.EvalSymlinks` results. revive's `redefines-builtin-id` catches this.
- **`golangci-lint --fix` rewrites `fmt.Errorf("plain string")` → `errors.New(...)` but doesn't add the import**. Re-run build after `make lint-fix` and add `errors` if needed.
- **Telego `MessageID` is `int`**, but `Chat.ID` / `User.ID` are `int64`. Don't crosswire — strconv.Atoi vs strconv.ParseInt. The IPC wire format uses string `chat_id` and int `message_id` for the same reason.
- **`daemon.pid` and `bot.pid` claims are comm-checked** — only PIDs whose `/proc/<pid>/comm` is `telegram-mcp` (or `bun`, for legacy TS-plugin handoff) get SIGTERMed. Anything else is left alone. Prevents PID recycling from making us murder an unrelated user process.
- **Routing key is `reply_to_message_id`, not chat** — if a user types a fresh message in a chat with multiple connected shims, it goes to the LRU/mention/pinned shim. To direct a reply at a specific shim's prior message, the user must use Telegram's "reply to" UI. Tests asserting "next inbound goes to shim X" must set `reply_to_message_id` explicitly.
- **IPC reconnect drops in-flight calls** — `botadapter` waits briefly for the reconnect to land, but a long disconnection surfaces `daemon unavailable` to the MCP tool caller. The daemon-side response was lost; the user sees the tool error and can retry.
- **Session file race** — `telegram-mcp self --hook` can fire before `shim.Wire()` writes `sessions/<cc_pid>.json`. It must print a fallback and exit 0; never abort the CC session. The file is removed on shim exit, so its absence after that is correct.
- **PR_SET_PDEATHSIG only on shim** — `cmd/server/main.go:bindParentDeath` skips it in daemon mode. The daemon's parent is systemd or the original shim's grandparent shell, and dying with either is wrong.
- **Headless `systemd --user` needs `loginctl enable-linger`** — when telegram-mcp runs as a user unit on a server you only reach over ssh, systemd-logind tears down the user manager (and every `--user` service) seconds after the last login session closes. Daemon shutdown runs `defer spawnRunner.Stop()` which cancels every live `/spawn` — symptom in `daemon.log` is `"ipc server stopping"` then `"shim disconnected"` for the spawn's shim, with no preceding `"idle timer started"` or `/spawn cancel`. Cross-check `journalctl -S ... | grep "session closed"` and `loginctl show-user $USER | grep Linger`. Fix: `loginctl enable-linger <user>` (see `contrib/systemd/README.md`).

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

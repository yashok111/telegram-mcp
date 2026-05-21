// telegram-mcp — local MCP bridge between Telegram and Claude Code.
//
// State lives in ~/.claude/channels/telegram (compat with the original TS
// plugin). Lifecycle: shim is bound to its parent CC session via
// PR_SET_PDEATHSIG; daemon outlives any single shim and idles out 7 days
// after the last shim disconnects.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/yakov/telegram-mcp/internal/access"
	"github.com/yakov/telegram-mcp/internal/bot"
	daemonpkg "github.com/yakov/telegram-mcp/internal/daemon"
	"github.com/yakov/telegram-mcp/internal/ipc"
	"github.com/yakov/telegram-mcp/internal/mcp"
	shimpkg "github.com/yakov/telegram-mcp/internal/shim"
)

type mode int

const (
	modeDaemon mode = iota
	modeShim
	modeSelf
)

func selectMode(argv []string) mode {
	if len(argv) >= 2 {
		switch argv[1] {
		case "daemon":
			return modeDaemon
		case "shim":
			return modeShim
		case "self":
			return modeSelf
		}
	}

	return modeShim
}

func main() {
	setupSlog()

	stateDir, err := bootstrapStateDir()
	if err != nil {
		slog.Error("bootstrap", "err", err)
		os.Exit(1)
	}

	// Load .env so TELEGRAM_BOT_TOKEN (commonly set only inside
	// ~/.claude/channels/telegram/.env) is visible to loadConfig.
	if err := loadDotEnv(filepath.Join(stateDir, ".env")); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn(".env load failed", "err", err)
	}

	selected := selectMode(os.Args)

	// `self` is a read-only context-rendering subcommand. It must not bind
	// PR_SET_PDEATHSIG or otherwise mutate process-global state.
	if selected == modeSelf {
		os.Exit(runSelf(stateDir, os.Args[2:], os.Stdout))
	}

	// PR_SET_PDEATHSIG binds our lifetime to the spawning Claude Code session
	// in shim mode. Daemon mode must outlive any single shim, so it opts out —
	// its lifetime is governed by IdleTimeout and systemd / signal handling.
	var runErr error

	switch selected {
	case modeDaemon:
		runErr = runDaemon(stateDir)
	case modeShim:
		bindParentDeath()

		runErr = runShim(stateDir)
	case modeSelf:
		// Handled above; included so the switch is exhaustive for linters.
	}

	if runErr != nil {
		slog.Error("fatal", "err", runErr)
		os.Exit(1)
	}
}

func runDaemon(stateDir string) error {
	var logger *daemonpkg.Logger

	if daemonpkg.ShouldRedirect() {
		maxBytes := resolveLogMaxBytes()

		l, err := daemonpkg.OpenLog(filepath.Join(stateDir, "daemon.log"), maxBytes)
		if err != nil {
			slog.Warn("stderr redirect failed", "err", err)
		} else {
			logger = l
			defer logger.Close()

			setupSlog()
		}
	}

	token, err := loadConfig(stateDir)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := access.NewStore(stateDir, os.Getenv("TELEGRAM_ACCESS_MODE") == "static")

	router := daemonpkg.NewRouter()

	var typing *daemonpkg.TypingTracker
	if daemonpkg.TypingEnabled() {
		typing = daemonpkg.NewTypingTracker(nil, loadTypingConfig())
	}

	notifier := daemonpkg.NewNotifier(router, store, typing)

	tgBot, err := bot.NewWithRouter(token, store, notifier, &routerAdapter{r: router})
	if err != nil {
		return fmt.Errorf("telegram init: %w", err)
	}

	if typing != nil {
		typing.AttachBot(tgBot)
	}

	bgRunner := daemonpkg.NewBgRunnerWithDeps(loadBgConfig(), tgBot, daemonpkg.NewExecCommander())
	tgBot.SetBgRunner(bgRunner)

	defer bgRunner.Stop()

	spawnRunner := daemonpkg.NewSpawnRunnerWithDeps(loadSpawnConfig(), tgBot, daemonpkg.NewExecSpawnCommander())
	tgBot.SetSpawnRunner(spawnRunner)

	// IdleLookup walks the live Router snapshot to find the shim paired with a
	// given spawn_id. Used by SpawnRunner.Run to enforce TELEGRAM_SPAWN_IDLE_TIMEOUT.
	spawnRunner.SetIdleLookup(func(spawnID string) (time.Duration, bool) {
		now := time.Now()

		for _, s := range router.Snapshot() {
			if s.SpawnID == spawnID {
				return s.IdleFor(now), true
			}
		}

		return 0, false
	})

	go spawnRunner.Run(ctx)

	defer spawnRunner.Stop()

	idleTimeout := resolveIdleTimeout()

	inboxTTL := resolveDurationEnv("TELEGRAM_INBOX_TTL", 7*24*time.Hour)
	corruptTTL := resolveDurationEnv("TELEGRAM_CORRUPT_TTL", 7*24*time.Hour)
	sessionsTTL := resolveDurationEnv("TELEGRAM_SESSIONS_TTL", time.Hour)

	d := &daemonpkg.Daemon{
		StateDir:    stateDir,
		SocketPath:  filepath.Join(stateDir, "daemon.sock"),
		PidPath:     filepath.Join(stateDir, "daemon.pid"),
		Store:       store,
		Bot:         tgBot,
		Router:      router,
		Typing:      typing,
		IdleTimeout: idleTimeout,
		InboxTTL:    inboxTTL,
		CorruptTTL:  corruptTTL,
		SessionsTTL: sessionsTTL,
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	defer signal.Stop(sigs)

	go func() {
		select {
		case <-sigs:
			cancel()
		case <-ctx.Done():
		}
	}()

	var wg sync.WaitGroup

	wg.Add(2)
	go func() {
		defer wg.Done()
		defer cancel()

		if err := tgBot.Poll(ctx); err != nil {
			slog.Error("poll exited", "err", err)
		}
	}()
	go func() {
		defer wg.Done()
		defer cancel()

		if err := d.Run(ctx); err != nil {
			slog.Error("daemon exited", "err", err)
		}
	}()

	if logger != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			logger.Run(ctx, daemonpkg.DefaultLogRotateCheck)
		}()
	}

	shutDone := make(chan struct{})

	go func() {
		wg.Wait()
		tgBot.Stop()
		close(shutDone)
	}()

	<-ctx.Done()

	select {
	case <-shutDone:
		return nil
	case <-time.After(7 * time.Second):
		slog.Error("daemon shutdown exceeded 7s deadline, forcing exit")
		signal.Stop(sigs)
		os.Exit(1) //nolint:gocritic // signal.Stop is called explicitly above; defer cleanup is the normal path.
	}

	return nil
}

func runShim(stateDir string) error {
	socketPath := filepath.Join(stateDir, "daemon.sock")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := shimpkg.EnsureDaemon(ctx, shimpkg.EnsureOpts{
		SocketPath: socketPath,
		StateDir:   stateDir,
	}); err != nil {
		return fmt.Errorf("ensure daemon: %w", err)
	}

	client, err := ipc.Dial(socketPath)
	if err != nil {
		return fmt.Errorf("dial daemon: %w", err)
	}
	defer func() { _ = client.Close() }()

	store := access.NewStore(stateDir, os.Getenv("TELEGRAM_ACCESS_MODE") == "static")

	mcpSrv, err := mcp.New(store)
	if err != nil {
		return fmt.Errorf("mcp init: %w", err)
	}

	sh := &shimpkg.Shim{
		Client:     client,
		MCP:        mcpSrv,
		Store:      store,
		StateDir:   stateDir,
		SocketPath: socketPath,
		HelloLabel: os.Getenv("CLAUDE_SESSION_LABEL"),
		DialIPC: func(p string) (shimpkg.IPCClient, error) {
			return ipc.Dial(p)
		},
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	defer signal.Stop(sigs)

	go func() {
		select {
		case <-sigs:
			cancel()
		case <-ctx.Done():
		}
	}()

	return sh.Run(ctx)
}

// setupSlog routes all slog calls to stderr as JSON. Kept as its own function
// so tests can assert the handler shape without dragging in os.Stderr fixtures.
func setupSlog() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))
}

// bindParentDeath wires PR_SET_PDEATHSIG so the kernel sends SIGTERM to us
// the moment our parent goes away. Logged-and-swallowed if the syscall fails
// (containers with seccomp profiles may block it).
func bindParentDeath() {
	if err := unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(unix.SIGTERM), 0, 0, 0); err != nil {
		slog.Warn("PR_SET_PDEATHSIG failed", "err", err)
	}
}

// defaultDaemonIdleTimeout is the daemon's idle-exit window when
// TELEGRAM_DAEMON_IDLE_EXIT is unset.
const defaultDaemonIdleTimeout = 7 * 24 * time.Hour

// resolveIdleTimeout returns the daemon's idle-exit timeout from
// TELEGRAM_DAEMON_IDLE_EXIT (seconds). Unset or unparseable → default
// (7 days). "0" or a negative value disables idle-exit (Daemon.Run treats
// any non-positive duration as "no timer").
func resolveIdleTimeout() time.Duration {
	raw, ok := os.LookupEnv("TELEGRAM_DAEMON_IDLE_EXIT")
	if !ok {
		return defaultDaemonIdleTimeout
	}

	secs, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		slog.Warn("invalid TELEGRAM_DAEMON_IDLE_EXIT, using default", "value", raw, "default", defaultDaemonIdleTimeout)

		return defaultDaemonIdleTimeout
	}

	// time.Duration is int64 nanoseconds; cap to avoid overflow on huge
	// values (e.g. user pastes max int64). The cap (~292 years) is well
	// beyond any reasonable idle window.
	const maxSecs = int64(math.MaxInt64) / int64(time.Second)
	if secs > maxSecs {
		slog.Warn("TELEGRAM_DAEMON_IDLE_EXIT too large, capping", "value", raw, "max_secs", maxSecs)

		secs = maxSecs
	}

	return time.Duration(secs) * time.Second
}

// resolveDurationEnv parses a duration from env (e.g. "24h"). Unset, empty,
// or unparseable falls back to def. Negative parses through unchanged so
// callers can disable a sweep with a negative value if they need to.
func resolveDurationEnv(name string, def time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return def
	}

	d, err := time.ParseDuration(v)
	if err != nil {
		slog.Warn("invalid duration env, using default", "name", name, "value", v, "default", def)
		return def
	}

	return d
}

// resolveLogMaxBytes parses TELEGRAM_LOG_MAX_BYTES into a rotation threshold.
// `=0` (or any non-positive) disables rotation. Unset or unparseable falls back
// to daemonpkg.DefaultLogMaxBytes. Negative-but-parseable values disable too
// (mirrors the daemon-idle-exit convention).
func resolveLogMaxBytes() int64 {
	raw, ok := os.LookupEnv("TELEGRAM_LOG_MAX_BYTES")
	if !ok {
		return daemonpkg.DefaultLogMaxBytes
	}

	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		slog.Warn("invalid TELEGRAM_LOG_MAX_BYTES, using default", "value", raw, "default", daemonpkg.DefaultLogMaxBytes)

		return daemonpkg.DefaultLogMaxBytes
	}

	if n <= 0 {
		return 0
	}

	return n
}

// resolveStateDir returns the channel state directory: TELEGRAM_STATE_DIR if
// set, otherwise ~/.claude/channels/telegram.
func resolveStateDir() string {
	if s := os.Getenv("TELEGRAM_STATE_DIR"); s != "" {
		return s
	}

	home, _ := os.UserHomeDir()

	return filepath.Join(home, ".claude", "channels", "telegram")
}

// bootstrapStateDir resolves the state directory, creates it with 0700, and
// returns the path. Wraps both pieces so tests can drive errors independently.
func bootstrapStateDir() (string, error) {
	dir := resolveStateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir state: %w", err)
	}

	return dir, nil
}

// loadConfig returns the resolved TELEGRAM_BOT_TOKEN. The .env file is read
// once at startup in main(); we only consult the process environment here.
func loadConfig(stateDir string) (string, error) {
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		return "", fmt.Errorf("TELEGRAM_BOT_TOKEN required (set in %s/.env)", stateDir)
	}

	return token, nil
}

// loadDotEnv mirrors the TS plugin: KEY=VALUE lines, real env wins.
func loadDotEnv(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	if err := os.Chmod(path, 0o600); err != nil {
		slog.Warn(".env chmod 0600 failed", "path", path, "err", err)
	}

	for line := range strings.Lines(string(raw)) {
		k, v, ok := strings.Cut(strings.TrimRight(line, "\n"), "=")
		if !ok || k == "" || os.Getenv(k) != "" {
			continue
		}

		_ = os.Setenv(k, v)
	}

	return nil
}

// loadTypingConfig folds the typing-tracker env vars into a TypingConfig.
// Zero-valued fields keep NewTypingTracker's defaults (refresh / TTL /
// rotation cadence / built-in emojis). Empty (non-nil) RotationEmojis or
// DoneEmojiDisabled=true express "explicitly off" so operators can turn the
// feature off without rebuilding.
func loadTypingConfig() daemonpkg.TypingConfig {
	cfg := daemonpkg.TypingConfig{TTL: daemonpkg.TypingTTLFromEnv()}

	if emojis := daemonpkg.TypingRotationEmojisFromEnv(); emojis != nil {
		cfg.RotationEmojis = emojis
	}

	if emoji, configured := daemonpkg.TypingDoneEmojiFromEnv(); configured {
		cfg.DoneEmoji = emoji
		cfg.DoneEmojiDisabled = emoji == ""
	}

	return cfg
}

// loadBgConfig folds env vars into a BgConfig, leaving zero-valued fields so
// daemonpkg.NewBgRunner applies its defaults (3 parallel / 30m timeout /
// 10 starts per hour / 5s edit throttle / "claude" binary).
func loadBgConfig() daemonpkg.BgConfig {
	cfg := daemonpkg.BgConfig{}

	if v, err := strconv.Atoi(os.Getenv("TELEGRAM_BG_MAX_PARALLEL")); err == nil && v > 0 {
		cfg.MaxParallel = v
	}

	if v := os.Getenv("TELEGRAM_BG_TIMEOUT"); v != "" {
		if parsed, err := time.ParseDuration(v); err == nil && parsed > 0 {
			cfg.Timeout = parsed
		} else {
			slog.Warn("invalid TELEGRAM_BG_TIMEOUT, using default", "value", v)
		}
	}

	if v := os.Getenv("TELEGRAM_BG_DEFAULT_WORKDIR"); v != "" {
		cfg.DefaultWorkdir = v
	}

	if v, err := strconv.Atoi(os.Getenv("TELEGRAM_BG_RATE_PER_HOUR")); err == nil && v > 0 {
		cfg.RatePerHourPerUser = v
	}

	if v := os.Getenv("TELEGRAM_BG_CLAUDE_BIN"); v != "" {
		cfg.ClaudeBin = v
	} else {
		cfg.ClaudeBin = resolveClaudeBin()
		slog.Info("bg claude binary resolved", "bin", cfg.ClaudeBin)
	}

	return cfg
}

// loadSpawnConfig folds env vars into a SpawnConfig. Zero fields keep
// daemonpkg.NewSpawnRunner's defaults (3 parallel · 24h hard · 5 starts per
// hour · "claude" binary · telegram plugin args). IdleTimeout defaults to 4h
// here (not in NewSpawnRunner) so `TELEGRAM_SPAWN_IDLE_TIMEOUT=0` truly
// disables the sweep instead of silently re-defaulting.
func loadSpawnConfig() daemonpkg.SpawnConfig {
	cfg := daemonpkg.SpawnConfig{IdleTimeout: 4 * time.Hour}

	if v, err := strconv.Atoi(os.Getenv("TELEGRAM_SPAWN_MAX_PARALLEL")); err == nil && v > 0 {
		cfg.MaxParallel = v
	}

	if v := os.Getenv("TELEGRAM_SPAWN_HARD_TIMEOUT"); v != "" {
		if parsed, err := time.ParseDuration(v); err == nil && parsed > 0 {
			cfg.HardTimeout = parsed
		} else {
			slog.Warn("invalid TELEGRAM_SPAWN_HARD_TIMEOUT, using default", "value", v)
		}
	}

	if v := os.Getenv("TELEGRAM_SPAWN_IDLE_TIMEOUT"); v != "" {
		if parsed, err := time.ParseDuration(v); err == nil && parsed >= 0 {
			cfg.IdleTimeout = parsed
		} else {
			slog.Warn("invalid TELEGRAM_SPAWN_IDLE_TIMEOUT, using default", "value", v)
		}
	}

	if v := os.Getenv("TELEGRAM_SPAWN_DEFAULT_WORKDIR"); v != "" {
		cfg.DefaultWorkdir = v
	}

	if v, err := strconv.Atoi(os.Getenv("TELEGRAM_SPAWN_RATE_PER_HOUR")); err == nil && v > 0 {
		cfg.RatePerHourPerUser = v
	}

	if v := os.Getenv("TELEGRAM_SPAWN_CLAUDE_BIN"); v != "" {
		cfg.ClaudeBin = v
	} else {
		cfg.ClaudeBin = resolveClaudeBin()
		slog.Info("spawn claude binary resolved", "bin", cfg.ClaudeBin)
	}

	if v := os.Getenv("TELEGRAM_SPAWN_CLAUDE_ARGS"); v != "" {
		cfg.ClaudeArgs = strings.Fields(v)
	} else if spec := resolveSpawnPluginSpec(); spec != "" {
		cfg.ClaudeArgs = []string{"--dangerously-load-development-channels", spec}
		slog.Info("spawn claude args resolved", "args", cfg.ClaudeArgs)
	}

	return cfg
}

// marketplaceManifestMaxBytes caps how much of a marketplace.json we'll read
// into memory. Real manifests are a few KB; anything larger is either corrupt
// or a hostile attempt to OOM the daemon at startup.
const marketplaceManifestMaxBytes = 1 << 20 // 1 MiB

// readBoundedFile reads at most maxBytes from path. Returns an error if the
// file is unreadable. If the file exceeds maxBytes, the result is truncated
// and a warning is logged so an operator debugging a stuck startup can see
// the manifest was rejected (the caller's JSON parse will then fail).
func readBoundedFile(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	// Read maxBytes+1 so we can tell "exactly maxBytes" apart from "exceeds".
	b, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}

	if int64(len(b)) > maxBytes {
		slog.Warn("manifest exceeds size cap, truncating", "path", path, "max_bytes", maxBytes)

		return b[:maxBytes], nil
	}

	return b, nil
}

// resolveSpawnPluginSpec scans `~/.claude/plugins/marketplaces/*/.claude-plugin/marketplace.json`
// for a marketplace that publishes the `telegram` plugin AND has a corresponding
// installed-plugin dir at `~/.claude/plugins/data/telegram-<channel>`. Returns
// `plugin:telegram@<channel>`. Empty result means no installed marketplace
// matched — caller falls back to the daemon-side default.
//
// Multiple installed marketplaces (e.g. a local dev marketplace alongside the
// official one) are resolved by `data/telegram-<channel>` mtime, which tracks
// actual plugin usage — marketplace.json mtime is unreliable because CC
// refreshes marketplace metadata in the background. Ties broken by channel
// name for determinism. Operators can pin via TELEGRAM_SPAWN_CLAUDE_ARGS.
func resolveSpawnPluginSpec() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	matches, _ := filepath.Glob(filepath.Join(home, ".claude", "plugins", "marketplaces", "*", ".claude-plugin", "marketplace.json"))

	type candidate struct {
		channel string
		usedAt  time.Time
	}

	var cands []candidate

	for _, p := range matches {
		b, err := readBoundedFile(p, marketplaceManifestMaxBytes)
		if err != nil {
			continue
		}

		var m struct {
			Name    string `json:"name"`
			Plugins []struct {
				Name string `json:"name"`
			} `json:"plugins"`
		}

		if err := json.Unmarshal(b, &m); err != nil || m.Name == "" {
			continue
		}

		// Reject manifest names that could break out of the data dir via
		// path separators or ".." segments. Real channel names are short
		// identifiers (e.g. "local-yakov", "stable").
		if strings.ContainsAny(m.Name, `/\`) || strings.Contains(m.Name, "..") {
			slog.Warn("marketplace name rejected (path-unsafe)", "path", p, "name", m.Name)

			continue
		}

		if !slices.ContainsFunc(m.Plugins, func(pl struct {
			Name string `json:"name"`
		},
		) bool {
			return pl.Name == "telegram"
		}) {
			continue
		}

		dataDir := filepath.Join(home, ".claude", "plugins", "data", "telegram-"+m.Name)

		// Lstat (not Stat) so a crafted marketplace.json with m.Name like
		// "../../etc" can't follow a symlink to probe arbitrary paths under
		// the daemon's UID. We require the data dir to be a real directory.
		info, err := os.Lstat(dataDir)
		if err != nil || !info.IsDir() {
			continue
		}

		cands = append(cands, candidate{channel: m.Name, usedAt: info.ModTime()})
	}

	if len(cands) == 0 {
		return ""
	}

	sort.Slice(cands, func(i, j int) bool {
		if !cands[i].usedAt.Equal(cands[j].usedAt) {
			return cands[i].usedAt.After(cands[j].usedAt)
		}

		return cands[i].channel < cands[j].channel
	})

	return "plugin:telegram@" + cands[0].channel
}

// resolveClaudeBin finds the `claude` executable when neither
// TELEGRAM_SPAWN_CLAUDE_BIN nor TELEGRAM_BG_CLAUDE_BIN pins an absolute path.
// PATH lookup catches the common case (shell-launched daemon with nvm/brew
// sourced); the nvm-glob fallback catches systemd-launched daemons whose PATH
// is sanitized and never contains ~/.nvm/versions/node/<v>/bin.
//
// Returns "claude" as a last resort so the eventual exec error is the same
// "executable file not found in $PATH" the operator sees today, just from
// the spawn site rather than from a bad env var.
func resolveClaudeBin() string {
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "claude"
	}

	matches, _ := filepath.Glob(filepath.Join(home, ".nvm", "versions", "node", "*", "bin", "claude"))
	if len(matches) == 0 {
		return "claude"
	}

	// Newest install wins. Lexicographic on "v9.x" vs "v10.x" would invert.
	sort.Slice(matches, func(i, j int) bool {
		si, errI := os.Stat(matches[i])
		sj, errJ := os.Stat(matches[j])

		if errI != nil || errJ != nil {
			return matches[i] > matches[j]
		}

		return si.ModTime().After(sj.ModTime())
	})

	return matches[0]
}

// routerAdapter adapts daemon.Router to bot.RouterView, converting
// daemon.ShimInfo into bot.ShimInfo at the boundary so internal/bot doesn't
// import internal/daemon.
type routerAdapter struct{ r *daemonpkg.Router }

func (a *routerAdapter) Snapshot() []bot.ShimInfo {
	in := a.r.Snapshot()
	out := make([]bot.ShimInfo, len(in))

	for i, s := range in {
		out[i] = adaptShimInfo(s)
	}

	return out
}

func (a *routerAdapter) Pin(chatID, prefix string, ttl time.Duration) (bot.ShimInfo, error) {
	sh, err := a.r.ResolveShimByPrefix(prefix)
	if err != nil {
		return bot.ShimInfo{}, fmt.Errorf("resolve shim prefix: %w", err)
	}

	if err := a.r.Pin(chatID, sh.ID, ttl); err != nil {
		return bot.ShimInfo{}, fmt.Errorf("pin shim: %w", err)
	}

	return lookupShimInfo(a.r, sh.ID), nil
}

func (a *routerAdapter) Evict(prefix string) (bot.ShimInfo, error) {
	sh, err := a.r.ResolveShimByPrefix(prefix)
	if err != nil {
		return bot.ShimInfo{}, fmt.Errorf("resolve shim prefix: %w", err)
	}

	info := lookupShimInfo(a.r, sh.ID)
	a.r.Drop(sh.ID)

	return info, nil
}

func (a *routerAdapter) SetLabel(prefix, label string) (bot.ShimInfo, error) {
	sh, err := a.r.ResolveShimByPrefix(prefix)
	if err != nil {
		return bot.ShimInfo{}, fmt.Errorf("resolve shim prefix: %w", err)
	}

	info, err := a.r.SetLabel(sh.ID, label)
	if err != nil {
		return bot.ShimInfo{}, fmt.Errorf("set label: %w", err)
	}

	return adaptShimInfo(info), nil
}

func lookupShimInfo(r *daemonpkg.Router, id string) bot.ShimInfo {
	for _, s := range r.Snapshot() {
		if s.ID == id {
			return adaptShimInfo(s)
		}
	}

	return bot.ShimInfo{}
}

func adaptShimInfo(s daemonpkg.ShimInfo) bot.ShimInfo {
	return bot.ShimInfo{
		ID:           s.ID,
		IDPrefix:     s.IDPrefix(),
		Alias:        s.Alias,
		Label:        s.Label,
		Workdir:      s.Workdir,
		CCSessionID:  s.CCSessionID,
		SpawnID:      s.SpawnID,
		ConnectedAt:  s.ConnectedAt,
		LastOutbound: s.LastOutbound,
		PinnedChats:  s.PinnedChats,
	}
}

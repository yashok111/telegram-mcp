// telegram-mcp — local MCP bridge between Telegram and Claude Code.
//
// State lives in ~/.claude/channels/telegram (compat with the original TS
// plugin). Lifecycle: shim is bound to its parent CC session via
// PR_SET_PDEATHSIG; daemon outlives any single shim and idles out 30 minutes
// after the last shim disconnects.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
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
	if daemonpkg.ShouldRedirect() {
		restore, err := daemonpkg.RedirectStderrTo(filepath.Join(stateDir, "daemon.log"))
		if err != nil {
			slog.Warn("stderr redirect failed", "err", err)
		} else {
			defer restore()

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
	notifier := daemonpkg.NewNotifier(router)

	tgBot, err := bot.NewWithRouter(token, store, notifier, &routerAdapter{r: router})
	if err != nil {
		return fmt.Errorf("telegram init: %w", err)
	}

	idleSecs, _ := strconv.Atoi(os.Getenv("TELEGRAM_DAEMON_IDLE_EXIT"))
	if idleSecs == 0 {
		idleSecs = 1800
	}

	inboxTTL := 7 * 24 * time.Hour
	if v := os.Getenv("TELEGRAM_INBOX_TTL"); v != "" {
		if parsed, err := time.ParseDuration(v); err == nil {
			inboxTTL = parsed
		} else {
			slog.Warn("invalid TELEGRAM_INBOX_TTL, using default", "value", v, "default", inboxTTL)
		}
	}

	d := &daemonpkg.Daemon{
		StateDir:    stateDir,
		SocketPath:  filepath.Join(stateDir, "daemon.sock"),
		PidPath:     filepath.Join(stateDir, "daemon.pid"),
		Store:       store,
		Bot:         tgBot,
		Router:      router,
		IdleTimeout: time.Duration(idleSecs) * time.Second,
		InboxTTL:    inboxTTL,
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
			if err := shimpkg.EnsureDaemon(ctx, shimpkg.EnsureOpts{
				SocketPath: p,
				StateDir:   stateDir,
			}); err != nil {
				return nil, fmt.Errorf("ensure daemon on reconnect: %w", err)
			}

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

// loadConfig pulls TELEGRAM_BOT_TOKEN from .env first (if real env hasn't set
// it) and returns the resolved token. Errors if neither source provides one.
func loadConfig(stateDir string) (string, error) {
	if err := loadDotEnv(filepath.Join(stateDir, ".env")); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn(".env load failed", "err", err)
	}

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

	_ = os.Chmod(path, 0o600)

	for line := range strings.Lines(string(raw)) {
		k, v, ok := strings.Cut(strings.TrimRight(line, "\n"), "=")
		if !ok || k == "" || os.Getenv(k) != "" {
			continue
		}

		_ = os.Setenv(k, v)
	}

	return nil
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
		ConnectedAt:  s.ConnectedAt,
		LastOutbound: s.LastOutbound,
		PinnedChats:  s.PinnedChats,
	}
}

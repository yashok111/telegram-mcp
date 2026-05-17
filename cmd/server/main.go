// telegram-mcp — local MCP bridge between Telegram and Claude Code.
//
// State lives in ~/.claude/channels/telegram (compat with the original TS
// plugin). Lifecycle: bound to parent via PR_SET_PDEATHSIG so we die with
// Claude Code even if the supervisor leaks us.
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
	modeEmbedded mode = iota
	modeDaemon
	modeShim
)

func selectMode(argv []string) mode {
	if len(argv) >= 2 {
		switch argv[1] {
		case "daemon":
			return modeDaemon
		case "shim":
			return modeShim
		}
	}

	if v := os.Getenv("TELEGRAM_DAEMON"); v != "" && v != "0" {
		return modeShim
	}

	return modeEmbedded
}

func main() {
	setupSlog()
	bindParentDeath()

	stateDir, err := bootstrapStateDir()
	if err != nil {
		slog.Error("bootstrap", "err", err)
		os.Exit(1)
	}

	var runErr error

	switch selectMode(os.Args) {
	case modeDaemon:
		runErr = runDaemon(stateDir)
	case modeShim:
		runErr = runShim(stateDir)
	case modeEmbedded:
		runErr = runEmbedded(stateDir)
	}

	if runErr != nil {
		slog.Error("fatal", "err", runErr)
		os.Exit(1)
	}
}

func runEmbedded(stateDir string) error {
	token, err := loadConfig(stateDir)
	if err != nil {
		return err
	}

	if err := claimPID(filepath.Join(stateDir, "bot.pid")); err != nil {
		return fmt.Errorf("claim pid: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := access.NewStore(stateDir, os.Getenv("TELEGRAM_ACCESS_MODE") == "static")

	mcpSrv, err := mcp.New(store)
	if err != nil {
		return fmt.Errorf("mcp init: %w", err)
	}

	tgBot, err := bot.New(token, store, mcpSrv)
	if err != nil {
		return fmt.Errorf("telegram init: %w", err)
	}

	mcpSrv.AttachBot(tgBot)

	var wg sync.WaitGroup

	wg.Add(2)
	go func() {
		defer wg.Done()
		defer cancel()

		if err := mcpSrv.ServeStdio(ctx); err != nil {
			slog.Error("mcp loop exited", "err", err)
		}
	}()
	go func() {
		defer wg.Done()
		defer cancel()

		if err := tgBot.Poll(ctx); err != nil {
			slog.Error("poll loop exited", "err", err)
		}
	}()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	select {
	case <-ctx.Done():
	case sig := <-sigs:
		slog.Info("shutting down", "signal", sig.String())
		cancel()
	}

	tgBot.Stop()
	wg.Wait()

	return nil
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

	tgBot, err := bot.New(token, store, notifier)
	if err != nil {
		return fmt.Errorf("telegram init: %w", err)
	}

	idleSecs, _ := strconv.Atoi(os.Getenv("TELEGRAM_DAEMON_IDLE_EXIT"))
	if idleSecs == 0 {
		idleSecs = 1800
	}

	d := &daemonpkg.Daemon{
		StateDir:    stateDir,
		SocketPath:  filepath.Join(stateDir, "daemon.sock"),
		PidPath:     filepath.Join(stateDir, "daemon.pid"),
		Store:       store,
		Bot:         tgBot,
		Router:      router,
		IdleTimeout: time.Duration(idleSecs) * time.Second,
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	go func() {
		<-sigs
		cancel()
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

	wg.Wait()
	tgBot.Stop()

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
	}

	sigs := make(chan os.Signal, 1)

	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() { <-sigs; cancel() }()

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

// claimPID writes our PID to bot.pid. If a previous live process owns it AND
// /proc says it's one of our binaries (bun or telegram-mcp), send SIGTERM and
// replace — Telegram allows exactly one getUpdates consumer per token.
// Refuses to signal a PID with an unrelated comm so PID recycling can't hijack
// us into killing a random process.
func claimPID(path string) error {
	if raw, err := os.ReadFile(path); err == nil {
		if old, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && old > 1 && old != os.Getpid() {
			if syscall.Kill(old, 0) == nil && isOurPoller(old) {
				slog.Info("replacing stale poller", "pid", old)
				_ = syscall.Kill(old, syscall.SIGTERM)
			}
		}
	}

	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o600)
}

func isOurPoller(pid int) bool {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return false
	}

	comm := strings.TrimSpace(string(raw))

	return comm == "telegram-mcp" || comm == "bun"
}

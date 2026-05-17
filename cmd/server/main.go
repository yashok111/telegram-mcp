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

	"golang.org/x/sys/unix"

	"github.com/yakov/telegram-mcp/internal/access"
	"github.com/yakov/telegram-mcp/internal/bot"
	"github.com/yakov/telegram-mcp/internal/mcp"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	// Stderr → Claude Code logs. JSON keeps it parseable, low-cardinality
	// messages keep aggregators happy.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	// Die-with-parent: kernel sends SIGTERM the moment our parent goes away.
	// Fixes the orphan-watchdog race the TS version had.
	if err := unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(unix.SIGTERM), 0, 0, 0); err != nil {
		slog.Warn("PR_SET_PDEATHSIG failed", "err", err)
	}

	stateDir := os.Getenv("TELEGRAM_STATE_DIR")
	if stateDir == "" {
		home, _ := os.UserHomeDir()
		stateDir = filepath.Join(home, ".claude", "channels", "telegram")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("mkdir state: %w", err)
	}

	if err := loadDotEnv(filepath.Join(stateDir, ".env")); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn(".env load failed", "err", err)
	}

	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		return fmt.Errorf("TELEGRAM_BOT_TOKEN required (set in %s/.env)", stateDir)
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

	// Signal shutdown: SIGTERM from PDEATHSIG, SIGINT for manual stop.
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

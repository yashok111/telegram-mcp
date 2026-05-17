// telegram-mcp — local MCP bridge between Telegram and Claude Code.
//
// State lives in ~/.claude/channels/telegram (compat with the original TS
// plugin). Lifecycle: bound to parent via PR_SET_PDEATHSIG so we die with
// Claude Code even if the supervisor leaks us.
package main

import (
	"context"
	"fmt"
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
		fmt.Fprintf(os.Stderr, "telegram-mcp: fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Die-with-parent: kernel sends SIGTERM the moment our parent goes away.
	// Fixes the orphan-watchdog race the TS version had.
	if err := unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(unix.SIGTERM), 0, 0, 0); err != nil {
		fmt.Fprintf(os.Stderr, "telegram-mcp: PR_SET_PDEATHSIG failed: %v\n", err)
	}

	stateDir := os.Getenv("TELEGRAM_STATE_DIR")
	if stateDir == "" {
		home, _ := os.UserHomeDir()
		stateDir = filepath.Join(home, ".claude", "channels", "telegram")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("mkdir state: %w", err)
	}

	if err := loadDotEnv(filepath.Join(stateDir, ".env")); err != nil {
		fmt.Fprintf(os.Stderr, "telegram-mcp: .env load: %v\n", err)
	}

	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		return fmt.Errorf("TELEGRAM_BOT_TOKEN required (set in %s/.env)", stateDir)
	}

	if err := claimPID(filepath.Join(stateDir, "bot.pid")); err != nil {
		return err
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
			fmt.Fprintf(os.Stderr, "telegram-mcp: mcp loop: %v\n", err)
		}
	}()
	go func() {
		defer wg.Done()
		defer cancel()
		if err := tgBot.Poll(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "telegram-mcp: poll loop: %v\n", err)
		}
	}()

	// Signal shutdown: SIGTERM from PDEATHSIG, SIGINT for manual stop.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	select {
	case <-ctx.Done():
	case sig := <-sigs:
		fmt.Fprintf(os.Stderr, "telegram-mcp: %s received, shutting down\n", sig)
		cancel()
	}

	tgBot.Stop()
	wg.Wait()
	return nil
}

// loadDotEnv mirrors the TS plugin: KEY=VALUE lines, real env wins.
func loadDotEnv(path string) error {
	f, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	_ = os.Chmod(path, 0o600)
	for _, line := range splitLines(string(f)) {
		k, v, ok := splitKV(line)
		if !ok || os.Getenv(k) != "" {
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
				fmt.Fprintf(os.Stderr, "telegram-mcp: replacing stale poller pid=%d\n", old)
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

func splitLines(s string) []string {
	var out []string
	start := 0
	for i, c := range s {
		if c == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func splitKV(line string) (string, string, bool) {
	for i := 0; i < len(line); i++ {
		if line[i] == '=' {
			k := line[:i]
			v := line[i+1:]
			if k == "" {
				return "", "", false
			}
			return k, v, true
		}
	}
	return "", "", false
}

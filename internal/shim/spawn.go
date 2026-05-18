package shim

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type EnsureOpts struct {
	SocketPath  string
	BinaryPath  string // defaults to /proc/self/exe
	StateDir    string // for log redirect path; passed in env to daemon
	WaitTimeout time.Duration
	NoSpawn     bool // for tests
}

var readComm = func(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(b))
}

// EnsureDaemon checks for a daemon by dialing the socket. If absent but a live
// daemon process is already recorded in daemon.pid, waits longer for its socket
// to appear instead of spawning a duplicate. Otherwise spawns a fresh daemon
// subprocess (detached via setsid) and polls until the socket appears or ctx done.
//
// The spawn path is serialized cross-process via flock on daemon.spawn.lock:
// without it, two shims racing past canDial both fork-exec their own daemon,
// then the loser's claimPID SIGKILLs the live winner — see PR 6ggF7hGF.
func EnsureDaemon(ctx context.Context, opts EnsureOpts) error {
	if opts.WaitTimeout == 0 {
		opts.WaitTimeout = 5 * time.Second
	}

	if canDial(opts.SocketPath) {
		slog.Info("daemon already listening", "socket", opts.SocketPath)
		return nil
	}

	if pid, err := readDaemonPID(opts.StateDir); err == nil && pid > 1 && processAlive(pid) && isOurDaemon(pid) {
		slog.Info("daemon process alive but socket not yet reachable — waiting", "pid", pid, "socket", opts.SocketPath)
		return waitForSocket(ctx, opts.SocketPath, opts.WaitTimeout*2)
	}

	if opts.NoSpawn {
		return errors.New("daemon socket missing and spawn disabled")
	}

	release, err := acquireSpawnLock(filepath.Dir(opts.SocketPath))
	if err != nil {
		return err
	}
	defer release()

	// Re-check under lock: a sibling shim may have finished spawning while we
	// blocked on Flock.
	if canDial(opts.SocketPath) {
		slog.Info("daemon appeared while waiting for spawn lock", "socket", opts.SocketPath)
		return nil
	}

	if pid, err := readDaemonPID(opts.StateDir); err == nil && pid > 1 && processAlive(pid) && isOurDaemon(pid) {
		slog.Info("daemon pid alive after lock — waiting for socket", "pid", pid)
		return waitForSocket(ctx, opts.SocketPath, opts.WaitTimeout*2)
	}

	slog.Info("daemon not reachable — spawning", "socket", opts.SocketPath)

	bin := opts.BinaryPath
	if bin == "" {
		var err error

		bin, err = os.Executable()
		if err != nil {
			return fmt.Errorf("locate self: %w", err)
		}
	}

	// We deliberately use exec.Command (not CommandContext): the daemon must
	// outlive the shim's ctx — CommandContext would kill it on shim exit.
	cmd := exec.Command(bin, "daemon") //nolint:noctx // see comment above; daemon detached.
	cmd.Stdin = nil

	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open /dev/null: %w", err)
	}
	defer func() { _ = devnull.Close() }()

	cmd.Stdout = devnull
	cmd.Stderr = devnull
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn daemon: %w", err)
	}

	slog.Info("daemon spawned", "bin", bin, "pid", cmd.Process.Pid)

	// Wait() instead of Release(): Release severs Go's handle to the child but
	// does NOT call wait4(2). If the daemon exits while the shim is still
	// alive, the kernel keeps the daemon as a zombie under the shim's PID.
	// claimPID in a fresh daemon uses syscall.Kill(pid, 0), which returns
	// success for zombies, so it thinks the old daemon is alive and bails out
	// with 409-Conflict thrash. The goroutine exits as soon as the child does.
	go func() { _, _ = cmd.Process.Wait() }()

	return waitForSocket(ctx, opts.SocketPath, opts.WaitTimeout)
}

// acquireSpawnLock blocks until it holds an exclusive flock(2) on
// <dir>/daemon.spawn.lock. The release closure drops the lock and closes the
// fd. Cross-process: any other shim hitting EnsureDaemon's spawn path on the
// same state dir blocks here, eliminating the canDial-vs-cmd.Start race where
// two shims both spawn a daemon and the loser SIGKILLs the live winner.
func acquireSpawnLock(dir string) (func(), error) {
	path := filepath.Join(dir, "daemon.spawn.lock")

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // dir is internal CC channel state.
	if err != nil {
		return nil, fmt.Errorf("open spawn lock: %w", err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()

		return nil, fmt.Errorf("acquire spawn lock: %w", err)
	}

	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

func waitForSocket(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if canDial(path) {
			slog.Info("daemon socket reachable", "socket", path, "wait_ms", time.Since(deadline.Add(-timeout)).Milliseconds())
			return nil
		}

		time.Sleep(50 * time.Millisecond)
	}

	return fmt.Errorf("daemon failed to bind %s within %s", filepath.Base(path), timeout)
}

func readDaemonPID(stateDir string) (int, error) {
	if stateDir == "" {
		return 0, errors.New("empty state dir")
	}

	b, err := os.ReadFile(filepath.Join(stateDir, "daemon.pid")) //nolint:gosec // stateDir is internal CC channel state.
	if err != nil {
		return 0, fmt.Errorf("read daemon pid: %w", err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, fmt.Errorf("parse daemon pid: %w", err)
	}

	return pid, nil
}

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func isOurDaemon(pid int) bool {
	return readComm(pid) == "telegram-mcp"
}

func canDial(path string) bool {
	c, err := net.DialTimeout("unix", path, 200*time.Millisecond)
	if err != nil {
		return false
	}

	_ = c.Close()

	return true
}

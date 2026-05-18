package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/access"
)

func TestDaemonShutsDownOnCancel(t *testing.T) {
	dir := t.TempDir()
	d := &Daemon{
		StateDir:   dir,
		SocketPath: filepath.Join(dir, "daemon.sock"),
		PidPath:    filepath.Join(dir, "daemon.pid"),
		Store:      access.NewStore(dir, false),
		Bot:        &fakeBot{},
		Router:     NewRouter(),
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- d.Run(ctx) }()

	require.Eventually(t, func() bool {
		_, err := readPID(filepath.Join(dir, "daemon.pid"))
		return err == nil
	}, time.Second, 10*time.Millisecond)

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("Daemon.Run did not return after cancel")
	}
}

func TestIdleExitFiresAfterTimeout(t *testing.T) {
	r := NewRouter()
	fired := make(chan struct{}, 1)

	idle := NewIdleExit(r, 50*time.Millisecond, func() {
		select {
		case fired <- struct{}{}:
		default:
		}
	})

	ctx := t.Context()

	go idle.Run(ctx)

	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatal("idle exit did not fire when no shims connected")
	}
}

func TestIdleExitDoesNotFireWhileConnected(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "x", Notify: func(string, any) error { return nil }})

	fired := make(chan struct{}, 1)
	idle := NewIdleExit(r, 50*time.Millisecond, func() {
		select {
		case fired <- struct{}{}:
		default:
		}
	})

	ctx := t.Context()

	go idle.Run(ctx)

	select {
	case <-fired:
		t.Fatal("idle exit fired with connected shim")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestIdleExitDisabledByZero(t *testing.T) {
	r := NewRouter()
	fired := make(chan struct{}, 1)

	idle := NewIdleExit(r, 0, func() {
		select {
		case fired <- struct{}{}:
		default:
		}
	})

	ctx := t.Context()

	go idle.Run(ctx)

	select {
	case <-fired:
		t.Fatal("idle exit fired with timeout=0 (disabled)")
	case <-time.After(150 * time.Millisecond):
	}
}

func swapIsOurDaemonFn(t *testing.T, fn func(int) bool) {
	t.Helper()

	prev := isOurDaemonFn
	isOurDaemonFn = fn

	t.Cleanup(func() { isOurDaemonFn = prev })
}

func swapWaitTimeouts(t *testing.T, term, kill time.Duration) {
	t.Helper()

	prevTerm := termWaitTimeout
	prevKill := killWaitTimeout
	termWaitTimeout = term
	killWaitTimeout = kill

	t.Cleanup(func() {
		termWaitTimeout = prevTerm
		killWaitTimeout = prevKill
	})
}

func TestClaimPIDWaitsForOldDaemonExit(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "daemon.pid")

	cmd := exec.Command("sleep", "10") //nolint:noctx // CommandContext spawns watchCtx goroutine that races goleak; cleanup reaps explicitly.
	require.NoError(t, cmd.Start())

	oldPID := cmd.Process.Pid

	// Reap concurrently so the subprocess doesn't linger as a zombie —
	// kill(pid, 0) returns nil for zombies until reaped, which would make
	// waitForExit spin until its deadline.
	reaped := make(chan struct{})

	go func() {
		_, _ = cmd.Process.Wait()

		close(reaped)
	}()

	t.Cleanup(func() {
		_ = cmd.Process.Kill()

		<-reaped
	})

	require.NoError(t, os.WriteFile(pidPath, []byte(strconv.Itoa(oldPID)), 0o600))

	swapIsOurDaemonFn(t, func(int) bool { return true })
	swapWaitTimeouts(t, 2*time.Second, 500*time.Millisecond)

	d := &Daemon{PidPath: pidPath}

	done := make(chan error, 1)

	go func() {
		done <- d.claimPID()
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("claimPID did not return after old daemon exit")
	}

	<-reaped
	require.Error(t, syscall.Kill(oldPID, 0), "old process should be gone")

	raw, err := os.ReadFile(pidPath)
	require.NoError(t, err)

	got, err := strconv.Atoi(string(raw))
	require.NoError(t, err)
	require.Equal(t, os.Getpid(), got)
}

func TestClaimPIDEscalatesToSIGKILLOnHang(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "daemon.pid")

	// Trap SIGTERM in child; only SIGKILL will reap it.
	cmd := exec.Command("sh", "-c", "trap '' TERM; sleep 60") //nolint:noctx // see TestClaimPIDWaitsForOldDaemonExit.
	require.NoError(t, cmd.Start())

	oldPID := cmd.Process.Pid

	reaped := make(chan struct{})

	go func() {
		_, _ = cmd.Process.Wait()

		close(reaped)
	}()

	t.Cleanup(func() {
		_ = cmd.Process.Kill()

		<-reaped
	})

	require.NoError(t, os.WriteFile(pidPath, []byte(strconv.Itoa(oldPID)), 0o600))

	swapIsOurDaemonFn(t, func(int) bool { return true })
	swapWaitTimeouts(t, 200*time.Millisecond, 2*time.Second)

	d := &Daemon{PidPath: pidPath}

	done := make(chan error, 1)
	go func() { done <- d.claimPID() }()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("claimPID did not return after SIGKILL escalation")
	}

	<-reaped
	require.Error(t, syscall.Kill(oldPID, 0), "old process should be killed by SIGKILL")
}

// TestClaimPIDTreatsZombieAsStale verifies that a daemon.pid pointing at a
// zombie process is treated as stale (overwritten) rather than alive.
// syscall.Kill(zombiePID, 0) returns nil until the zombie is reaped, so without
// an explicit State:Z guard, claimPID would SIGTERM/SIGKILL the zombie and
// then spin waitForExit until killWaitTimeout — the failure mode from the
// 2026-05-18 incident.
func TestClaimPIDTreatsZombieAsStale(t *testing.T) {
	if _, err := os.Stat("/bin/true"); err != nil {
		t.Skipf("/bin/true unavailable: %v", err)
	}

	dir := t.TempDir()
	pidPath := filepath.Join(dir, "daemon.pid")

	cmd := exec.Command("/bin/true") //nolint:noctx // intentionally not reaped — we want the zombie state.
	require.NoError(t, cmd.Start())

	zombiePID := cmd.Process.Pid

	t.Cleanup(func() { _, _ = cmd.Process.Wait() })

	require.Eventually(t, func() bool {
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", zombiePID))
		if err != nil {
			return false
		}

		return strings.Contains(string(raw), "State:\tZ")
	}, 2*time.Second, 10*time.Millisecond, "child never reached zombie state")

	require.NoError(t, os.WriteFile(pidPath, []byte(strconv.Itoa(zombiePID)), 0o600))

	swapIsOurDaemonFn(t, func(int) bool { return true })
	// Short timeouts: without the zombie guard, evictOldDaemon would
	// SIGTERM-then-SIGKILL the zombie and waitForExit would spin for
	// term+kill = 400ms before returning an error. With the guard, claimPID
	// returns within a few milliseconds (file ops only).
	swapWaitTimeouts(t, 200*time.Millisecond, 200*time.Millisecond)

	d := &Daemon{PidPath: pidPath}

	start := time.Now()
	err := d.claimPID()
	elapsed := time.Since(start)

	require.NoError(t, err)
	require.Less(t, elapsed, 100*time.Millisecond,
		"claimPID escalated SIGTERM/SIGKILL on a zombie instead of treating it as stale")

	raw, err := os.ReadFile(pidPath)
	require.NoError(t, err)

	got, err := strconv.Atoi(string(raw))
	require.NoError(t, err)
	require.Equal(t, os.Getpid(), got, "pid file should be overwritten with our pid")
}

func TestClaimPIDLeavesForeignProcessAlone(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "daemon.pid")

	// Spawn a benign sub-process so we have a real, alive PID that is NOT us.
	cmd := exec.Command("sleep", "30") //nolint:noctx // see TestClaimPIDWaitsForOldDaemonExit.
	require.NoError(t, cmd.Start())

	foreignPID := cmd.Process.Pid

	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	require.NoError(t, os.WriteFile(pidPath, []byte(strconv.Itoa(foreignPID)), 0o600))

	swapIsOurDaemonFn(t, func(int) bool { return false })
	swapWaitTimeouts(t, 200*time.Millisecond, 200*time.Millisecond)

	d := &Daemon{PidPath: pidPath}

	require.NoError(t, d.claimPID())

	require.NoError(t, syscall.Kill(foreignPID, 0), "foreign process must remain alive")

	raw, err := os.ReadFile(pidPath)
	require.NoError(t, err)

	got, err := strconv.Atoi(string(raw))
	require.NoError(t, err)
	require.Equal(t, os.Getpid(), got)
}

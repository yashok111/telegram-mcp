package daemon

import (
	"context"
	"fmt"
	"net"
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

func TestIdleExitResetsOnReconnect(t *testing.T) {
	r := NewRouter()

	fired := make(chan struct{}, 1)
	idle := NewIdleExit(r, 200*time.Millisecond, func() {
		select {
		case fired <- struct{}{}:
		default:
		}
	})

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	go idle.Run(ctx)

	// Start with 0 shims; let the timer arm on the first tick (timeout/4 = 50ms).
	require.Eventually(t, func() bool {
		return r.ConnectedCount() == 0
	}, 100*time.Millisecond, 10*time.Millisecond)

	time.Sleep(100 * time.Millisecond)

	// Connect a shim so the next tick sees count>0 and resets idleSince.
	r.Register(&Shim{ID: "x", Notify: func(string, any) error { return nil }})

	select {
	case <-fired:
		t.Fatal("idle exit fired after a shim reconnected before the timeout elapsed")
	case <-time.After(400 * time.Millisecond):
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

func swapSocketPeerPIDFn(t *testing.T, fn func(string, time.Duration) (int, bool)) {
	t.Helper()

	prev := socketPeerPIDFn
	socketPeerPIDFn = fn

	t.Cleanup(func() { socketPeerPIDFn = prev })
}

func TestSocketPeerPID_returnsListenerPID(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "test.sock")

	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)

	t.Cleanup(func() { _ = ln.Close() })

	accepted := make(chan struct{})

	go func() {
		c, aerr := ln.Accept()
		if aerr == nil {
			_ = c.Close()
		}

		close(accepted)
	}()

	pid, ok := socketPeerPID(sockPath, time.Second)
	require.True(t, ok, "socketPeerPID must return ok=true against a live listener")
	require.Equal(t, os.Getpid(), pid, "peer of an in-process listener is ourselves")

	<-accepted
}

func TestSocketPeerPID_returnsFalseOnMissingSocket(t *testing.T) {
	dir := t.TempDir()
	pid, ok := socketPeerPID(filepath.Join(dir, "nope.sock"), 100*time.Millisecond)
	require.False(t, ok)
	require.Zero(t, pid)
}

// TestClaimPIDEvictsSocketPeerWhenPidfileStale exercises the bug-2 scenario:
// a daemon is alive holding the socket, but daemon.pid points at a different
// (dead) process. claimPID must still detect the live socket holder via
// SO_PEERCRED and SIGTERM it before claiming.
func TestClaimPIDEvictsSocketPeerWhenPidfileStale(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "daemon.pid")
	sockPath := filepath.Join(dir, "daemon.sock")

	cmd := exec.Command("sleep", "30") //nolint:noctx // see TestClaimPIDWaitsForOldDaemonExit.
	require.NoError(t, cmd.Start())

	peerPID := cmd.Process.Pid

	reaped := make(chan struct{})

	go func() {
		_, _ = cmd.Process.Wait()

		close(reaped)
	}()

	t.Cleanup(func() {
		_ = cmd.Process.Kill()

		<-reaped
	})

	// daemon.pid holds a stale (already-dead) PID. Without the socket-peer
	// probe, claimPID would log "stale, overwriting" and miss the live daemon
	// on the socket entirely.
	require.NoError(t, os.WriteFile(pidPath, []byte("1"), 0o600))

	// Stub the socket probe so the test doesn't need a real unix-socket
	// listener inside the foreign subprocess.
	swapSocketPeerPIDFn(t, func(p string, _ time.Duration) (int, bool) {
		require.Equal(t, sockPath, p)
		return peerPID, true
	})
	swapIsOurDaemonFn(t, func(int) bool { return true })
	swapWaitTimeouts(t, 2*time.Second, 500*time.Millisecond)

	d := &Daemon{PidPath: pidPath, SocketPath: sockPath}

	done := make(chan error, 1)
	go func() { done <- d.claimPID() }()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("claimPID did not return after socket-peer eviction")
	}

	<-reaped
	require.Error(t, syscall.Kill(peerPID, 0), "socket-peer process should be terminated")

	raw, err := os.ReadFile(pidPath)
	require.NoError(t, err)

	got, err := strconv.Atoi(string(raw))
	require.NoError(t, err)
	require.Equal(t, os.Getpid(), got)
}

// TestClaimPIDLeavesForeignSocketPeerAlone — peer holds the socket but is
// not telegram-mcp (e.g. some unrelated user process owns daemon.sock). We
// must NOT SIGTERM it.
func TestClaimPIDLeavesForeignSocketPeerAlone(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "daemon.pid")
	sockPath := filepath.Join(dir, "daemon.sock")

	cmd := exec.Command("sleep", "30") //nolint:noctx // see TestClaimPIDWaitsForOldDaemonExit.
	require.NoError(t, cmd.Start())

	peerPID := cmd.Process.Pid

	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	swapSocketPeerPIDFn(t, func(string, time.Duration) (int, bool) { return peerPID, true })
	swapIsOurDaemonFn(t, func(int) bool { return false })
	swapWaitTimeouts(t, 200*time.Millisecond, 200*time.Millisecond)

	d := &Daemon{PidPath: pidPath, SocketPath: sockPath}

	require.NoError(t, d.claimPID())
	require.NoError(t, syscall.Kill(peerPID, 0), "foreign socket peer must remain alive")
}

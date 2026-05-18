package shim

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnsureDaemonConnectsToExistingSocket(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "daemon.sock")

	l, err := net.Listen("unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })

	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}

			_ = c.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	err = EnsureDaemon(ctx, EnsureOpts{SocketPath: sock, NoSpawn: true})
	assert.NoError(t, err)
}

func TestEnsureDaemonFailsWhenNoSocketAndNoSpawn(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "daemon.sock")

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	err := EnsureDaemon(ctx, EnsureOpts{SocketPath: sock, NoSpawn: true})
	assert.Error(t, err)
}

func TestEnsureDaemonWaitsWhenDaemonPidAlive(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "daemon.sock")
	pidPath := filepath.Join(dir, "daemon.pid")

	require.NoError(t, os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o600))

	prev := readComm
	readComm = func(_ int) string { return "telegram-mcp" }

	t.Cleanup(func() { readComm = prev })

	go func() {
		time.Sleep(100 * time.Millisecond)

		l, err := net.Listen("unix", sock)
		if err != nil {
			return
		}

		t.Cleanup(func() { _ = l.Close() })

		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}

				_ = c.Close()
			}
		}()
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	err := EnsureDaemon(ctx, EnsureOpts{
		SocketPath:  sock,
		StateDir:    dir,
		NoSpawn:     true,
		WaitTimeout: 500 * time.Millisecond,
	})
	assert.NoError(t, err)
}

func TestEnsureDaemonWaitTimesOutWhenSocketNeverAppears(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "daemon.sock")
	pidPath := filepath.Join(dir, "daemon.pid")

	require.NoError(t, os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o600))

	prev := readComm
	readComm = func(_ int) string { return "telegram-mcp" }

	t.Cleanup(func() { readComm = prev })

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	err := EnsureDaemon(ctx, EnsureOpts{
		SocketPath:  sock,
		StateDir:    dir,
		NoSpawn:     true,
		WaitTimeout: 100 * time.Millisecond,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "within")
}

func TestEnsureDaemonSpawnsWhenNoLiveDaemon(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "daemon.sock")

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	err := EnsureDaemon(ctx, EnsureOpts{
		SocketPath: sock,
		StateDir:   dir,
		NoSpawn:    true,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spawn disabled")
}

func TestEnsureDaemonSpawnsWhenPidStale(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "daemon.sock")
	pidPath := filepath.Join(dir, "daemon.pid")

	require.NoError(t, os.WriteFile(pidPath, []byte("1"), 0o600))

	prev := readComm
	readComm = func(_ int) string { return "init" }

	t.Cleanup(func() { readComm = prev })

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	err := EnsureDaemon(ctx, EnsureOpts{
		SocketPath: sock,
		StateDir:   dir,
		NoSpawn:    true,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spawn disabled")
}

func TestAcquireSpawnLockSerializesConcurrentCallers(t *testing.T) {
	dir := t.TempDir()

	release1, err := acquireSpawnLock(dir)
	require.NoError(t, err)
	require.NotNil(t, release1)

	got := make(chan struct{})

	go func() {
		defer close(got)

		release2, err := acquireSpawnLock(dir)
		if err == nil && release2 != nil {
			release2()
		}
	}()

	select {
	case <-got:
		t.Fatal("second acquire returned while first lock still held")
	case <-time.After(75 * time.Millisecond):
	}

	release1()

	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("second acquire never returned after first release")
	}
}

// TestEnsureDaemonReapsExitedChild verifies the spawn goroutine calls Wait()
// rather than Release(): a daemon binary that exits before the socket appears
// must not leave a zombie under the shim's PID. Regression test for the
// 2026-05-18 incident where claimPID's syscall.Kill(pid, 0) was fooled by
// zombies and refused to evict a "live" predecessor.
func TestEnsureDaemonReapsExitedChild(t *testing.T) {
	if _, err := os.Stat("/bin/true"); err != nil {
		t.Skipf("/bin/true unavailable: %v", err)
	}

	dir := t.TempDir()
	sock := filepath.Join(dir, "daemon.sock")

	zombiesBefore := countZombieChildren(os.Getpid())

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	err := EnsureDaemon(ctx, EnsureOpts{
		SocketPath:  sock,
		BinaryPath:  "/bin/true",
		WaitTimeout: 300 * time.Millisecond,
	})
	require.Error(t, err)

	require.Eventually(t, func() bool {
		return countZombieChildren(os.Getpid()) <= zombiesBefore
	}, 3*time.Second, 25*time.Millisecond,
		"spawned daemon left a zombie under shim pid — reaper goroutine did not wait4")
}

// countZombieChildren returns the number of /proc entries whose PPid matches
// the given pid AND whose State line starts with Z (zombie). Linux-only;
// callers must gate on /bin/true (or equivalent) being present.
func countZombieChildren(ppid int) int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		// non-Linux or /proc unmounted: caller skipped on /bin/true probe.
		return 0
	}

	target := fmt.Sprintf("PPid:\t%d", ppid)

	count := 0

	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}

		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "status"))
		if err != nil {
			// race: process exited between readdir and readfile.
			continue
		}

		s := string(b)
		if !strings.Contains(s, target) {
			continue
		}

		if strings.Contains(s, "State:\tZ") {
			count++
		}
	}

	return count
}

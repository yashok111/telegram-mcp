package shim

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
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

package shim

import (
	"context"
	"net"
	"path/filepath"
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

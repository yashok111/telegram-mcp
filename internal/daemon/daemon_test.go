package daemon

import (
	"context"
	"path/filepath"
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

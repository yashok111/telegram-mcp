package daemon

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/access"
	"github.com/yakov/telegram-mcp/internal/ipc"
)

// captureSlog redirects the default logger into a buffer for the duration of
// the test. The crash-vs-clean classification in Daemon.OnDisconnect is only
// observable through the log now that the anomaly EventBus is gone.
func captureSlog(t *testing.T) *syncBuffer {
	t.Helper()

	buf := &syncBuffer{}
	prev := slog.Default()

	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	return buf
}

// runBareDaemon boots a Daemon with no bot (the disconnect path never touches
// it) and returns its socket plus a stop func that joins Run so goleak stays
// clean.
func runBareDaemon(t *testing.T) (sock string, router *Router, stop func()) {
	t.Helper()

	dir := t.TempDir()
	store := access.NewStore(dir, false)
	require.NoError(t, store.Save(access.State{
		DMPolicy:  access.PolicyAllowlist,
		AllowFrom: []string{"123"},
		Groups:    map[string]access.GroupPolicy{},
		Pending:   map[string]access.Pending{},
	}))

	router = NewRouter()
	sock = filepath.Join(dir, "daemon.sock")

	d := &Daemon{
		StateDir:   dir,
		SocketPath: sock,
		PidPath:    filepath.Join(dir, "daemon.pid"),
		Store:      store,
		Router:     router,
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() {
		_ = d.Run(ctx)

		close(done)
	}()

	require.Eventually(t, func() bool {
		c, err := ipc.Dial(sock)
		if err != nil {
			return false
		}

		_ = c.Close()

		return true
	}, 2*time.Second, 20*time.Millisecond)

	stop = func() {
		cancel()

		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()

		select {
		case <-done:
		case <-timer.C:
			t.Error("daemon did not exit cleanly")
		}
	}

	return sock, router, stop
}

func TestDaemon_UngracefulDisconnectWarns(t *testing.T) {
	buf := captureSlog(t)

	sock, router, stop := runBareDaemon(t)
	defer stop()

	c, shimID := connectShim(t, sock)
	_ = c.Close() // no goodbye → likely crash

	require.Eventually(t, func() bool {
		return router.ConnectedCount() == 0
	}, 2*time.Second, 20*time.Millisecond, "daemon must process the disconnect")

	out := buf.String()
	assert.Contains(t, out, "shim disconnected without goodbye")
	assert.Contains(t, out, shimID)
	assert.Contains(t, out, `"graceful":false`, "the info line must still carry the classification")
}

func TestDaemon_GracefulDisconnectDoesNotWarn(t *testing.T) {
	buf := captureSlog(t)

	sock, router, stop := runBareDaemon(t)
	defer stop()

	c, _ := connectShim(t, sock)
	require.NoError(t, c.Notify(ipc.MethodGoodbye, map[string]any{}))
	_ = c.Close()

	require.Eventually(t, func() bool {
		return router.ConnectedCount() == 0
	}, 2*time.Second, 20*time.Millisecond, "daemon must process the disconnect")

	out := buf.String()
	assert.NotContains(t, out, "shim disconnected without goodbye", "a goodbye must never look like a crash")
	assert.Contains(t, out, `"graceful":true`, "the info line must record the clean exit")
}

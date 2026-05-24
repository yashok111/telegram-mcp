package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/access"
	"github.com/yakov/telegram-mcp/internal/ipc"
)

// runDaemonWithBus boots a minimal Daemon (nil bot — the disconnect path never
// touches it) wired with a real EventBus persisting to <dir>/admin/events.jsonl.
// Returns the socket path, the EventLog to assert against, and a stop func that
// cancels and joins Run so goleak stays clean.
func runDaemonWithBus(t *testing.T) (sock string, log *EventLog, router *Router, stop func()) {
	t.Helper()

	dir := t.TempDir()
	store := access.NewStore(dir, false)
	require.NoError(t, store.Save(access.State{
		DMPolicy:  access.PolicyAllowlist,
		AllowFrom: []string{"123"},
		Groups:    map[string]access.GroupPolicy{},
		Pending:   map[string]access.Pending{},
	}))

	log = NewEventLog(dir)
	bus := NewEventBus(log, func(string, any) bool { return false }) // no admin connected: persist only
	router = NewRouter()

	sock = filepath.Join(dir, "daemon.sock")
	d := &Daemon{
		StateDir:   dir,
		SocketPath: sock,
		PidPath:    filepath.Join(dir, "daemon.pid"),
		Store:      store,
		Router:     router,
		EventBus:   bus,
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

	return sock, log, router, stop
}

func hasEvent(log *EventLog, typ, subject string) bool {
	evs, _ := log.Recent(0)
	for _, e := range evs {
		if e.Type == typ && e.Subject == subject {
			return true
		}
	}

	return false
}

func TestDaemon_UngracefulDisconnectEmitsShimDisconnected(t *testing.T) {
	sock, log, _, stop := runDaemonWithBus(t)
	defer stop()

	c, shimID := connectShim(t, sock)
	_ = c.Close() // no goodbye → unexpected disconnect

	require.Eventually(t, func() bool {
		return hasEvent(log, "shim_disconnected", shimID)
	}, 3*time.Second, 50*time.Millisecond, "ungraceful disconnect must emit shim_disconnected")
}

func TestDaemon_GracefulDisconnectNoEvent(t *testing.T) {
	sock, log, router, stop := runDaemonWithBus(t)
	defer stop()

	c, shimID := connectShim(t, sock)
	require.NoError(t, c.Notify(ipc.MethodGoodbye, map[string]any{}))
	_ = c.Close()

	// Wait until the daemon has fully processed the disconnect (Drop ran, so the
	// emit decision in the same OnDisconnect callback has also already run) — not
	// a wall-clock guess.
	require.Eventually(t, func() bool {
		return router.ConnectedCount() == 0
	}, 2*time.Second, 20*time.Millisecond, "daemon must process the disconnect")

	// The decision is made; a goodbye must never produce shim_disconnected. The
	// short window also covers the async EventBus drain.
	require.Never(t, func() bool {
		return hasEvent(log, "shim_disconnected", shimID)
	}, 200*time.Millisecond, 25*time.Millisecond, "graceful goodbye must not emit shim_disconnected")
}

package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/access"
)

func TestAcquirePollerLock_SecondAcquireBlocked(t *testing.T) {
	dir := t.TempDir()

	release1, err := AcquirePollerLock(dir)
	require.NoError(t, err)

	defer release1()

	_, err = AcquirePollerLock(dir)
	require.ErrorIs(t, err, ErrPollerHeld)
}

func TestAcquirePollerLock_ReleaseAllowsReacquire(t *testing.T) {
	dir := t.TempDir()

	release, err := AcquirePollerLock(dir)
	require.NoError(t, err)

	release()

	release2, err := AcquirePollerLock(dir)
	require.NoError(t, err)
	release2()
}

func TestAcquirePollerLock_DistinctDirsIndependent(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()

	releaseA, err := AcquirePollerLock(dirA)
	require.NoError(t, err)

	defer releaseA()

	releaseB, err := AcquirePollerLock(dirB)
	require.NoError(t, err, "lock on dir B must be independent of dir A")
	releaseB()
}

// TestDaemonRunRefusesWhenPollerLockHeld is the integration check for the
// bug-of-record: a stale daemon's poller still holds the flock, so a second
// Daemon.Run must surface ErrPollerHeld instead of starting a second long-poll
// loop (which would trigger 409 Conflict storms from Telegram).
func TestDaemonRunRefusesWhenPollerLockHeld(t *testing.T) {
	dir := t.TempDir()

	release, err := AcquirePollerLock(dir)
	require.NoError(t, err)

	defer release()

	d := &Daemon{
		StateDir:   dir,
		SocketPath: filepath.Join(dir, "daemon.sock"),
		PidPath:    filepath.Join(dir, "daemon.pid"),
		Store:      access.NewStore(dir, false),
		Bot:        &fakeBot{},
		Router:     NewRouter(),
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)

	go func() { done <- d.Run(ctx) }()

	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrPollerHeld)
	case <-time.After(3 * time.Second):
		t.Fatal("Daemon.Run did not return when poller lock was held")
	}
}

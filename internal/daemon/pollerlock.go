package daemon

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
)

const pollerLockName = "daemon.poller.lock"

// ErrPollerHeld is returned by AcquirePollerLock when another daemon process
// already holds the flock. Caller MUST refuse to start polling — surfaces the
// case where evictOldDaemon missed a stale daemon and would otherwise
// dual-poll the bot token, producing 409 Conflict storms from Telegram.
var ErrPollerHeld = errors.New("another daemon holds the poller lock")

// AcquirePollerLock takes a non-blocking exclusive flock(2) on
// <stateDir>/daemon.poller.lock and returns a release closure. Held for the
// entire daemon process lifetime as a kernel-enforced backstop against the
// race where evictOldDaemon misses a still-polling sibling — the second
// daemon hits ErrPollerHeld instead of starting a second long-poll loop.
//
// flock auto-releases when the holder's process exits (kernel closes the fd
// table), so a hung old daemon does NOT permanently deadlock new starts —
// claimPID's SIGTERM→SIGKILL ladder kills the holder, the kernel releases the
// lock, and the next daemon start acquires.
func AcquirePollerLock(stateDir string) (func(), error) {
	path := filepath.Join(stateDir, pollerLockName)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // dir is internal CC channel state.
	if err != nil {
		return nil, fmt.Errorf("open poller lock: %w", err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()

		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrPollerHeld
		}

		return nil, fmt.Errorf("acquire poller lock: %w", err)
	}

	slog.Info("poller lock acquired", "path", path)

	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

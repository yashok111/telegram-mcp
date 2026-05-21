package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// DefaultLogMaxBytes caps daemon.log size before rotation.
	DefaultLogMaxBytes = 10 * 1024 * 1024
	// DefaultLogRotateCheck is how often Run checks the file size.
	DefaultLogRotateCheck = 10 * time.Minute
)

// Logger owns the stderr-attached log file and supports size-based rotation.
// Rotation renames path → path+".1" (replacing any prior) and reopens path
// as a fresh file. Stderr is re-dup2'd onto the new fd so existing handles
// keep working without re-init by callers.
type Logger struct {
	path     string
	maxBytes int64
	origFd   int

	mu sync.Mutex
	f  *os.File
}

// ShouldRedirect reports whether stderr is a non-tty (e.g., a closed fd or
// pipe in a shim-spawned daemon).
func ShouldRedirect() bool {
	_, err := unix.IoctlGetTermios(int(os.Stderr.Fd()), unix.TCGETS)
	return err != nil
}

// OpenLog opens path (append, 0600), dup2's it onto fd 2, and returns a
// Logger. maxBytes ≤ 0 disables rotation in Run/MaybeRotate.
func OpenLog(path string, maxBytes int64) (*Logger, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	origFd, err := unix.Dup(int(os.Stderr.Fd()))
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("dup stderr: %w", err)
	}

	if err := unix.Dup2(int(f.Fd()), int(os.Stderr.Fd())); err != nil {
		_ = f.Close()
		_ = unix.Close(origFd)

		return nil, fmt.Errorf("dup2: %w", err)
	}

	return &Logger{path: path, maxBytes: maxBytes, origFd: origFd, f: f}, nil
}

// Close restores the original stderr and closes the log file. Safe to call
// multiple times; subsequent calls are no-ops.
func (l *Logger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.f == nil {
		return
	}

	_ = unix.Dup2(l.origFd, int(os.Stderr.Fd()))
	_ = unix.Close(l.origFd)
	l.origFd = -1
	_ = l.f.Close()
	l.f = nil
}

// Rotate renames the current log to path+".1" (replacing prior), opens a
// fresh file at path, and re-attaches stderr to the new fd. On OpenFile
// failure after a successful rename, it best-effort restores the original
// name so stderr keeps a recoverable path.
func (l *Logger) Rotate() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.rotateLocked()
}

func (l *Logger) rotateLocked() error {
	if l.f == nil {
		return nil
	}

	rotated := l.path + ".1"
	_ = os.Remove(rotated)

	renamed := false

	if err := os.Rename(l.path, rotated); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("rename: %w", err)
		}
	} else {
		renamed = true
	}

	newF, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		if renamed {
			if rerr := os.Rename(rotated, l.path); rerr != nil {
				slog.Error("daemon.log rotate failed to restore prior name; stderr now attached to rotated file",
					"open_err", err, "restore_err", rerr, "rotated", rotated)
			}
		}

		return fmt.Errorf("open: %w", err)
	}

	if err := unix.Dup2(int(newF.Fd()), int(os.Stderr.Fd())); err != nil {
		_ = newF.Close()
		// Old file at l.path still exists; old fd still valid. Best-effort
		// restore the rotated name back so disk state matches in-memory.
		if renamed {
			_ = os.Rename(rotated, l.path)
		}

		return fmt.Errorf("dup2: %w", err)
	}

	_ = l.f.Close()
	l.f = newF

	return nil
}

// MaybeRotate rotates only when current size ≥ maxBytes. Returns true if it
// rotated. No-op when maxBytes ≤ 0. Holds the mutex across stat-and-rotate
// so a concurrent Close cannot invalidate the fd mid-check.
func (l *Logger) MaybeRotate() (bool, error) {
	if l.maxBytes <= 0 {
		return false, nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.f == nil {
		return false, nil
	}

	fi, err := l.f.Stat()
	if err != nil {
		return false, fmt.Errorf("stat: %w", err)
	}

	if fi.Size() < l.maxBytes {
		return false, nil
	}

	if err := l.rotateLocked(); err != nil {
		return false, err
	}

	return true, nil
}

// Run drives periodic size checks; ticks until ctx is done. No-op when
// maxBytes ≤ 0. interval ≤ 0 falls back to DefaultLogRotateCheck.
func (l *Logger) Run(ctx context.Context, interval time.Duration) {
	if l.maxBytes <= 0 {
		return
	}

	if interval <= 0 {
		interval = DefaultLogRotateCheck
	}

	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			rotated, err := l.MaybeRotate()
			if err != nil {
				slog.Warn("daemon.log rotate failed", "err", err)
			} else if rotated {
				slog.Info("daemon.log rotated", "path", l.path, "max_bytes", l.maxBytes)
			}
		}
	}
}

// RedirectStderrTo opens path with rotation disabled and returns a close
// function. Retained for legacy callers and tests; production wiring should
// use OpenLog and start Logger.Run for periodic rotation.
func RedirectStderrTo(path string) (func(), error) {
	l, err := OpenLog(path, 0)
	if err != nil {
		return nil, err
	}

	return l.Close, nil
}

package daemon

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// ShouldRedirect reports whether stderr is a non-tty (e.g., a closed fd or
// pipe in a shim-spawned daemon).
func ShouldRedirect() bool {
	_, err := unix.IoctlGetTermios(int(os.Stderr.Fd()), unix.TCGETS)
	return err != nil
}

// RedirectStderrTo opens path (append, 0600), dup2's it onto fd 2, and returns
// a restore function that swaps the original back.
func RedirectStderrTo(path string) (restore func(), err error) {
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

	return func() {
		_ = unix.Dup2(origFd, int(os.Stderr.Fd()))
		_ = unix.Close(origFd)
		_ = f.Close()
	}, nil
}

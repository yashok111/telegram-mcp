package shim

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

type EnsureOpts struct {
	SocketPath  string
	BinaryPath  string // defaults to /proc/self/exe
	StateDir    string // for log redirect path; passed in env to daemon
	WaitTimeout time.Duration
	NoSpawn     bool // for tests
}

// EnsureDaemon checks for a daemon by dialing the socket. If absent, spawns
// the daemon subprocess (detached via setsid + Setpgid) and polls until the
// socket appears or ctx is done.
func EnsureDaemon(ctx context.Context, opts EnsureOpts) error {
	if opts.WaitTimeout == 0 {
		opts.WaitTimeout = 5 * time.Second
	}

	if canDial(opts.SocketPath) {
		return nil
	}

	if opts.NoSpawn {
		return errors.New("daemon socket missing and spawn disabled")
	}

	bin := opts.BinaryPath
	if bin == "" {
		var err error

		bin, err = os.Executable()
		if err != nil {
			return fmt.Errorf("locate self: %w", err)
		}
	}

	cmd := exec.Command(bin, "daemon")
	cmd.Stdin = nil

	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open /dev/null: %w", err)
	}
	defer func() { _ = devnull.Close() }()

	cmd.Stdout = devnull
	cmd.Stderr = devnull
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn daemon: %w", err)
	}

	go func() { _ = cmd.Process.Release() }()

	deadline := time.Now().Add(opts.WaitTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if canDial(opts.SocketPath) {
			return nil
		}

		time.Sleep(50 * time.Millisecond)
	}

	return fmt.Errorf("daemon failed to bind %s within %s", filepath.Base(opts.SocketPath), opts.WaitTimeout)
}

func canDial(path string) bool {
	c, err := net.DialTimeout("unix", path, 200*time.Millisecond)
	if err != nil {
		return false
	}

	_ = c.Close()
	return true
}

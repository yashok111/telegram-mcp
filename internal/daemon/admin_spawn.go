package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// AdminTokenEnv carries a per-daemon-boot secret to the forked admin-agent.
// HandleHello rejects role="admin" unless the hello carries this exact value,
// preventing a rogue shim from claiming the AdminAlias and intercepting DM
// fallback traffic. The token never touches disk and rotates on every daemon
// restart.
const AdminTokenEnv = "TELEGRAM_ADMIN_TOKEN"

// generateAdminToken returns a 32-hex-char random secret. crypto/rand failure
// returns "" so the daemon refuses to forward a degraded token rather than
// silently using a predictable one.
func generateAdminToken() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		slog.Error("admin token rand failed", "err", err)
		return ""
	}

	return hex.EncodeToString(buf)
}

// AdminEnabled gates whether the daemon forks the admin-agent at boot. Until
// the agent's tool surface lands (PR-2 ships only the scaffold), operators
// can leave it off without losing daemon functionality.
const adminEnableEnv = "TELEGRAM_ADMIN_ENABLE"

// AdminBackoffMax caps the exponential restart delay for the agent.
const (
	adminBackoffStart = 500 * time.Millisecond
	adminBackoffMax   = 30 * time.Second
)

// AdminCommander abstracts the subprocess launcher so tests can verify args
// + env without forking real telegram-mcp processes.
type AdminCommander interface {
	// Start spawns the admin-agent. Returns a handle whose Wait blocks until
	// the process exits and Pid/Stop allow supervisors to terminate it.
	Start(ctx context.Context, binary string, args, env []string) (AdminProcess, error)
}

// AdminProcess is the subset of os/exec.Cmd the spawner needs after Start.
type AdminProcess interface {
	Wait() error
	Pid() int
	// Stop signals the process and waits up to timeout for exit; SIGKILL if
	// SIGTERM is ignored.
	Stop(timeout time.Duration) error
}

// AdminSpawner supervises a single admin-agent subprocess. Fork-execs the
// daemon binary in `admin-agent` mode, restarts on crash with capped
// exponential backoff, and stops on context cancel.
type AdminSpawner struct {
	Binary   string
	Args     []string
	Env      []string
	Cmder    AdminCommander
	Enabled  bool
	Token    string
	startMin time.Duration
	startMax time.Duration

	mu     sync.Mutex
	proc   AdminProcess
	cancel context.CancelFunc
	done   chan struct{}
}

// NewAdminSpawner builds a spawner with default backoff. Set Enabled via env
// (TELEGRAM_ADMIN_ENABLE=1) — when disabled, Run returns immediately so the
// daemon boots without forking.
//
// The Token is generated per daemon boot and passed to the forked admin-agent
// via TELEGRAM_ADMIN_TOKEN. HandleHello rejects role="admin" without it.
// Callers wire the matching value into Handlers via SetAdminToken before
// server.Listen so user shims can never impersonate the admin-agent.
//
// done is pre-allocated so Stop blocks on it deterministically even when
// called before Run's goroutine has executed past its lock section.
func NewAdminSpawner(binary string, cmder AdminCommander) *AdminSpawner {
	return &AdminSpawner{
		Binary:   binary,
		Args:     []string{"admin-agent"},
		Cmder:    cmder,
		Enabled:  adminEnabledFromEnv(),
		Token:    generateAdminToken(),
		startMin: adminBackoffStart,
		startMax: adminBackoffMax,
		done:     make(chan struct{}),
	}
}

func adminEnabledFromEnv() bool {
	v := os.Getenv(adminEnableEnv)
	switch v {
	case "1", "true", "yes", "on":
		return true
	}

	return false
}

// Run drives the supervise loop until ctx is done. Returns nil on clean
// shutdown. Safe to call once per spawner; subsequent Run invocations are no-ops.
func (s *AdminSpawner) Run(ctx context.Context) {
	if !s.Enabled {
		slog.Info("admin-agent disabled (TELEGRAM_ADMIN_ENABLE not set)")
		return
	}

	if s.Cmder == nil {
		slog.Error("admin spawner missing AdminCommander; refusing to start")
		return
	}

	s.mu.Lock()

	if s.cancel != nil {
		s.mu.Unlock()
		return
	}

	rctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		done := s.done
		s.cancel = nil
		s.mu.Unlock()

		if done != nil {
			close(done)
		}
	}()

	backoff := s.startMin

	for {
		if rctx.Err() != nil {
			return
		}

		env := s.Env
		if s.Token != "" {
			env = append([]string{AdminTokenEnv + "=" + s.Token}, s.Env...)
		}

		proc, err := s.Cmder.Start(rctx, s.Binary, s.Args, env)
		if err != nil {
			slog.Error("admin-agent spawn failed", "err", err, "backoff", backoff)

			if !waitWithBackoff(rctx, backoff) {
				return
			}

			backoff = nextAdminBackoff(backoff, s.startMax)

			continue
		}

		s.mu.Lock()
		s.proc = proc
		s.mu.Unlock()

		slog.Info("admin-agent started", "pid", proc.Pid())

		waitErr := proc.Wait()

		s.mu.Lock()
		s.proc = nil
		s.mu.Unlock()

		if rctx.Err() != nil {
			return
		}

		slog.Warn("admin-agent exited; restarting", "err", waitErr, "backoff", backoff)

		if !waitWithBackoff(rctx, backoff) {
			return
		}

		backoff = nextAdminBackoff(backoff, s.startMax)
	}
}

// Stop cancels Run and signals any running child. Safe to call from any
// goroutine. Blocks until Run's defer fires; a no-op when Run hasn't been
// called.
func (s *AdminSpawner) Stop() {
	s.mu.Lock()
	cancel := s.cancel
	proc := s.proc
	done := s.done
	s.mu.Unlock()

	if cancel == nil && proc == nil {
		return
	}

	if cancel != nil {
		cancel()
	}

	if proc != nil {
		if err := proc.Stop(5 * time.Second); err != nil {
			slog.Warn("admin-agent stop failed", "err", err)
		}
	}

	if done != nil {
		<-done
	}
}

// nextAdminBackoff doubles cur up to ceil. Caller is responsible for sleeping.
func nextAdminBackoff(cur, ceil time.Duration) time.Duration {
	next := cur * 2
	if next > ceil {
		return ceil
	}

	return next
}

// waitWithBackoff blocks for d or until ctx is cancelled. Returns false on
// cancel so the caller can break the supervise loop.
func waitWithBackoff(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// execAdminCommander is the production AdminCommander. It exec's the daemon
// binary with `admin-agent` as the first arg and stamps no environment
// overrides (the agent inherits the daemon's env, which already has the
// correct TELEGRAM_STATE_DIR / .env settings).
type execAdminCommander struct{}

// NewExecAdminCommander returns the production-mode commander.
func NewExecAdminCommander() AdminCommander { return execAdminCommander{} }

func (execAdminCommander) Start(_ context.Context, binary string, args, env []string) (AdminProcess, error) {
	if binary == "" {
		return nil, errors.New("admin commander: empty binary path")
	}

	// gosec: binary path is /proc/self/exe (or argv[0]) resolved by the
	// daemon's resolveAdminBin — never user-supplied, never PATH-derived.
	//
	// Plain exec.Command (not CommandContext) so this struct's Wait+Stop
	// own the single os.Process.Wait call. CommandContext spins its own
	// internal goroutine that races with our Stop on context cancel.
	//nolint:gosec,noctx // trusted: same binary as daemon; ctx handled by our Stop+Wait owner
	cmd := exec.Command(binary, args...)
	if len(env) > 0 {
		// The admin-agent owns no bot connection — it speaks IPC to the daemon
		// like any shim. Strip TELEGRAM_BOT_TOKEN so neither the agent nor the
		// claude it forks ever sees the token. Strip any inherited
		// TELEGRAM_ADMIN_TOKEN too so only the freshly-generated per-boot value
		// prepended via env survives (glibc getenv returns the first match —
		// a stale leading copy would otherwise win and break admin auth).
		cmd.Env = append(filterEnv(os.Environ(), "TELEGRAM_BOT_TOKEN=", AdminTokenEnv+"="), env...)
	}

	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	// Inherit daemon stderr so admin-agent slog lands in daemon.log (and
	// daemon's shim_log fan-out picks up any shim_id-tagged lines from the
	// admin's logged inbounds in PR-3+).
	cmd.Stderr = os.Stderr
	cmd.Stdout = nil
	cmd.Stdin = nil

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}

	p := &execAdminProcess{cmd: cmd, waitDone: make(chan struct{})}

	// Single Wait goroutine. Run blocks on p.Wait(), Stop's signal triggers
	// the same Wait to return; both read p.waitErr after p.waitDone is
	// closed. Two callers of *exec.Cmd.Wait would race the underlying
	// syscall.Wait4.
	go func() {
		p.waitErr = cmd.Wait()
		close(p.waitDone)
	}()

	return p, nil
}

type execAdminProcess struct {
	cmd      *exec.Cmd
	waitDone chan struct{}
	waitErr  error
}

func (p *execAdminProcess) Wait() error {
	<-p.waitDone
	return p.waitErr
}

func (p *execAdminProcess) Pid() int {
	if p.cmd.Process == nil {
		return 0
	}

	return p.cmd.Process.Pid
}

func (p *execAdminProcess) Stop(timeout time.Duration) error {
	if p.cmd.Process == nil {
		return nil
	}

	select {
	case <-p.waitDone:
		return nil
	default:
	}

	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("sigterm: %w", err)
	}

	t := time.NewTimer(timeout)
	defer t.Stop()

	select {
	case <-p.waitDone:
		return nil
	case <-t.C:
		_ = p.cmd.Process.Kill()
		<-p.waitDone

		return errors.New("admin-agent ignored SIGTERM, killed")
	}
}

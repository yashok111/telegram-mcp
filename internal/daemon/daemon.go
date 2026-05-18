package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/yakov/telegram-mcp/internal/access"
	"github.com/yakov/telegram-mcp/internal/ipc"
)

// Daemon is composed by main() and run via Run(ctx). It does not own
// process-wide concerns (slog setup, PR_SET_PDEATHSIG) — those live in main.
type Daemon struct {
	StateDir   string
	SocketPath string
	PidPath    string

	Store  *access.Store
	Bot    botSurface
	Router *Router

	IdleTimeout time.Duration // 0 disables
	InboxTTL    time.Duration // 0 disables inbox sweep

	//nolint:containedctx // dctx is an internal cancel signal scoped to Run(); IdleExit needs it.
	dctx    context.Context
	dcancel context.CancelFunc
}

func (d *Daemon) Run(ctx context.Context) error {
	if err := d.claimPID(); err != nil {
		return fmt.Errorf("claim daemon.pid: %w", err)
	}

	defer func() { _ = os.Remove(d.PidPath) }()

	server := ipc.NewServer(d.SocketPath)

	handlers := NewHandlers(d.Store, d.Bot, d.Router)
	handlers.Register(server)

	server.OnDisconnect(func(c *ipc.Conn) {
		v, ok := c.Meta.Load(metaShimID)
		if !ok {
			return
		}

		id, _ := v.(string)
		if id == "" {
			return
		}

		d.Router.Drop(id)
		slog.Info("shim disconnected", "shim_id", id)
	})

	server.Handle(ipc.MethodHello, func(hctx context.Context, c *ipc.Conn, params json.RawMessage) (any, *ipc.Error) {
		res, rpcErr := handlers.HandleHello(hctx, c, params)
		if rpcErr != nil {
			return nil, rpcErr
		}

		m, _ := res.(map[string]any)
		id, _ := m["shim_id"].(string)

		label, _ := c.Meta.Load(metaLabel)
		labelStr, _ := label.(string)
		wd, _ := c.Meta.Load(metaWorkdir)
		wdStr, _ := wd.(string)
		cc, _ := c.Meta.Load(metaCCSessionID)
		ccStr, _ := cc.(string)

		shim := &Shim{
			ID:          id,
			Label:       labelStr,
			Workdir:     wdStr,
			CCSessionID: ccStr,
			Notify:      c.Notify,
		}
		d.Router.Register(shim)
		m["alias"] = shim.Alias

		slog.Info("shim connected", "shim_id", id, "alias", shim.Alias,
			"label", labelStr, "workdir", wdStr, "cc_session_id", ccStr)

		return m, nil
	})

	d.dctx, d.dcancel = context.WithCancel(ctx)
	defer d.dcancel()

	var idleWG sync.WaitGroup

	if d.IdleTimeout > 0 {
		idleExit := NewIdleExit(d.Router, d.IdleTimeout, func() {
			slog.Info("idle timeout — exiting", "timeout", d.IdleTimeout)
			d.dcancel()
		})

		idleWG.Go(func() {
			idleExit.Run(d.dctx)
		})
	}

	cleanup := NewRulesCleanup(d.Store, time.Minute)
	idleWG.Go(func() {
		cleanup.Run(d.dctx)
	})

	if ic := NewInboxCleanup(d.Store, d.InboxTTL, time.Hour); ic != nil {
		idleWG.Go(func() {
			ic.Run(d.dctx)
		})
	}

	listenErr := server.Listen(d.dctx)
	d.dcancel()
	idleWG.Wait()

	if listenErr != nil && !strings.Contains(listenErr.Error(), "closed") {
		return fmt.Errorf("ipc listen: %w", listenErr)
	}

	return nil
}

// Test seams: swappable so tests can mock the comm guard and shorten waits.
var (
	isOurDaemonFn   = isOurDaemon
	termWaitTimeout = 5 * time.Second
	killWaitTimeout = 2 * time.Second
)

// claimPID writes daemon.pid; if a previous daemon owns it, signal SIGTERM
// (with /proc/<pid>/comm guard), wait for exit, escalate to SIGKILL if needed,
// then replace. Defensive wait avoids split-brain when systemd restarts the
// daemon faster than the old one shuts down.
func (d *Daemon) claimPID() error {
	if err := d.evictOldDaemon(); err != nil {
		return err
	}

	if err := os.WriteFile(d.PidPath, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return err
	}

	slog.Info("daemon.pid claimed", "pid", os.Getpid(), "path", d.PidPath)

	return nil
}

func (d *Daemon) evictOldDaemon() error {
	raw, err := os.ReadFile(d.PidPath)
	if err != nil {
		return nil //nolint:nilerr // missing daemon.pid means no prior daemon to evict.
	}

	old, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || old <= 1 || old == os.Getpid() {
		return nil //nolint:nilerr // garbage / self / pid<=1 means no prior daemon to evict.
	}

	alive := processIsLive(old)
	ours := isOurDaemonFn(old)

	switch {
	case alive && ours:
		return terminateOldDaemon(old)
	case alive && !ours:
		slog.Warn("daemon.pid points at foreign process — leaving it alone", "pid", old)
	default:
		slog.Info("daemon.pid stale, overwriting", "pid", old)
	}

	return nil
}

func terminateOldDaemon(pid int) error {
	slog.Info("replacing stale daemon", "pid", pid)
	_ = syscall.Kill(pid, syscall.SIGTERM)

	if err := waitForExit(pid, termWaitTimeout); err == nil {
		return nil
	}

	slog.Warn("old daemon did not exit on SIGTERM — escalating to SIGKILL", "pid", pid)
	_ = syscall.Kill(pid, syscall.SIGKILL)

	if err := waitForExit(pid, killWaitTimeout); err != nil {
		return fmt.Errorf("old daemon pid=%d still alive after SIGKILL: %w", pid, err)
	}

	return nil
}

func waitForExit(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processIsLive(pid) {
			return nil
		}

		time.Sleep(50 * time.Millisecond)
	}

	return fmt.Errorf("process %d still alive after %s", pid, timeout)
}

// processIsLive reports whether pid refers to a running process. A zombie
// passes syscall.Kill(pid, 0) (the kernel still has its entry), so without the
// /proc/<pid>/status check this returns true for any unreaped child. Zombies
// cannot be terminated by any signal, so treating them as alive triggers a
// futile SIGTERM/SIGKILL escalation and a "still alive after SIGKILL" error.
func processIsLive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}

	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		// Non-Linux or /proc unmounted: fall back to Kill(0) semantics.
		return true
	}

	return !strings.Contains(string(raw), "State:\tZ")
}

func isOurDaemon(pid int) bool {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return false
	}

	return strings.TrimSpace(string(raw)) == "telegram-mcp"
}

func readPID(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}

	return strconv.Atoi(strings.TrimSpace(string(raw)))
}

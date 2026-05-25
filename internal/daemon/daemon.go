package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
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

	Store      *access.Store
	Bot        botSurface
	Router     *Router
	Typing     *TypingTracker // nil disables typing-refresh goroutine
	Forum      *Forum         // nil disables forum-topic allocation; required only when ForumChatID is configured
	Header     *HeaderManager // nil disables pinned topic headers (forum mode only)
	TopicSweep *TopicSweep    // nil disables periodic closed-topic deletion
	ShimLogs   *ShimLogs      // nil disables per-shim log files
	ShimsSweep *ShimsSweep    // nil disables shims/*.log retention sweep

	// EventBus persists anomaly events and pushes them to the admin-agent.
	// nil disables event observability entirely.
	EventBus *EventBus
	// Sitrep fires the daily owner-digest trigger to the admin-agent on an
	// interval. nil disables.
	Sitrep *SitrepTicker

	// SpawnRunner / BgRunner feed the admin.snapshot IPC method. nil omits
	// that section of the snapshot. Set by cmd/server after the runners exist.
	SpawnRunner spawnLister
	BgRunner    bgLister

	// AdminToken authenticates a hello carrying role="admin". Empty
	// disables the admin-agent path entirely (no shim can claim AdminAlias).
	AdminToken string

	// AdminMutator backs the admin.mutate IPC method (Tier-2 auto-apply /
	// Tier-3 owner-confirm). nil disables admin mutations entirely.
	AdminMutator *AdminMutator

	IdleTimeout time.Duration // 0 disables
	InboxTTL    time.Duration // 0 disables inbox sweep
	CorruptTTL  time.Duration // 0 disables access.json.corrupt-* sweep
	SessionsTTL time.Duration // 0 disables sessions/<cc_pid>.json orphan sweep

	//nolint:containedctx // dctx is an internal cancel signal scoped to Run(); IdleExit needs it.
	dctx    context.Context
	dcancel context.CancelFunc
}

func (d *Daemon) Run(ctx context.Context) error {
	if err := d.claimPID(); err != nil {
		return fmt.Errorf("claim daemon.pid: %w", err)
	}

	defer func() { _ = os.Remove(d.PidPath) }()
	defer func() {
		if d.ShimLogs != nil {
			d.ShimLogs.CloseAll()
		}
	}()

	release, err := AcquirePollerLock(filepath.Dir(d.PidPath))
	if err != nil {
		return fmt.Errorf("poller lock: %w", err)
	}

	defer release()

	server := ipc.NewServer(d.SocketPath)

	handlers := NewHandlers(d.Store, d.Bot, d.Router, d.Typing)
	handlers.SetShimLogs(d.ShimLogs)
	handlers.SetAdminToken(d.AdminToken)
	handlers.SetRunners(d.SpawnRunner, d.BgRunner)
	handlers.SetMutator(d.AdminMutator)
	handlers.SetHeader(d.Header)

	if d.EventBus != nil {
		handlers.SetEventSink(d.EventBus)
	}

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

		if d.Forum != nil {
			d.Forum.ReleaseLock(id)
		}

		d.headerDisconnect(id)

		d.Router.Drop(id)

		if d.ShimLogs != nil {
			d.ShimLogs.Close(id)
		}

		// A shim that exits cleanly sends goodbye first (HandleNotify stamps
		// metaGoodbye). Absence of the flag means a likely crash — surface it as
		// an anomaly. The admin-agent itself is excluded: its supervisor restarts
		// it on every exit, so its disconnects are expected churn, not anomalies.
		_, graceful := c.Meta.Load(metaGoodbye)
		role, _ := c.Meta.Load(metaRole)
		roleStr, _ := role.(string)

		if d.EventBus != nil && !graceful && roleStr != "admin" {
			d.EventBus.Emit(Event{
				Type:     "shim_disconnected",
				Severity: "warning",
				Subject:  id,
				Detail:   "shim disconnected without goodbye (possible crash)",
			})
		}

		slog.Info("shim disconnected", "shim_id", id, "graceful", graceful)
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
		spawn, _ := c.Meta.Load(metaSpawnID)
		spawnStr, _ := spawn.(string)
		role, _ := c.Meta.Load(metaRole)
		roleStr, _ := role.(string)

		shim := &Shim{
			ID:          id,
			Label:       labelStr,
			Workdir:     wdStr,
			CCSessionID: ccStr,
			SpawnID:     spawnStr,
			Role:        roleStr,
			Notify:      c.Notify,
		}
		d.Router.Register(shim)
		m["alias"] = shim.Alias

		// Forum topic allocation runs after Register so resolveReuseKey
		// sees the freshly-allocated alias on the Shim (topic name uses
		// it). Failure falls through to DM-mode rather than aborting the
		// hello — the shim still works, just without topic routing.
		if d.Forum != nil && d.Forum.Enabled() {
			tid, err := d.Forum.AllocateOrReuse(hctx, shim)
			if err != nil {
				slog.Warn("forum topic allocation failed — falling back to DM-mode",
					"shim_id", id, "err", err)
			} else if tid > 0 {
				// BindTopic updates the Shim and Router.topicOwners under
				// the same lock so the next inbound's topic-arm sees the
				// owner immediately.
				d.Router.BindTopic(id, tid)
				m["topic_id"] = tid
				d.Header.Ensure(hctx, tid) // nil-safe; no-op when headers disabled
			}
		}

		slog.Info("shim connected", "shim_id", id, "alias", shim.Alias,
			"label", labelStr, "workdir", wdStr, "cc_session_id", ccStr, "spawn_id", spawnStr,
			"topic_id", shim.TopicID)

		return m, nil
	})

	d.dctx, d.dcancel = context.WithCancel(ctx)
	defer d.dcancel()

	var idleWG sync.WaitGroup

	d.startBackgroundWorkers(&idleWG)

	listenErr := server.Listen(d.dctx)
	d.dcancel()
	idleWG.Wait()

	if listenErr != nil && !strings.Contains(listenErr.Error(), "closed") {
		return fmt.Errorf("ipc listen: %w", listenErr)
	}

	return nil
}

// startBackgroundWorkers launches every long-running goroutine that Daemon.Run
// owns onto wg. Each worker reads ctx via d.dctx and shuts down when Listen
// returns and the parent cancels. Pulled out of Run so the wiring fits under
// the package gocyclo threshold.
func (d *Daemon) startBackgroundWorkers(wg *sync.WaitGroup) {
	if d.IdleTimeout > 0 {
		idleExit := NewIdleExit(d.Router, d.IdleTimeout, func() {
			slog.Info("idle timeout — exiting", "timeout", d.IdleTimeout)
			d.dcancel()
		})

		wg.Go(func() { idleExit.Run(d.dctx) })
	}

	cleanup := NewRulesCleanup(d.Store, time.Minute)

	wg.Go(func() { cleanup.Run(d.dctx) })

	if d.TopicSweep != nil {
		wg.Go(func() { d.TopicSweep.Run(d.dctx) })
	}

	if d.Header != nil {
		wg.Go(func() { d.Header.Run(d.dctx) })
	}

	if ic := NewInboxCleanup(d.Store, d.InboxTTL, time.Hour); ic != nil {
		wg.Go(func() { ic.Run(d.dctx) })
	}

	if cs := NewCorruptSweep(d.Store, d.CorruptTTL, time.Hour); cs != nil {
		wg.Go(func() { cs.Run(d.dctx) })
	}

	if ss := NewSessionsSweep(d.Store, d.SessionsTTL, time.Hour); ss != nil {
		wg.Go(func() { ss.Run(d.dctx) })
	}

	if d.ShimsSweep != nil {
		wg.Go(func() { d.ShimsSweep.Run(d.dctx) })
	}

	if d.Typing != nil {
		wg.Go(func() { d.Typing.Run(d.dctx) })
	}

	if d.EventBus != nil {
		wg.Go(func() { d.EventBus.Run(d.dctx) })
	}

	if d.Sitrep != nil {
		wg.Go(func() { d.Sitrep.Run(d.dctx) })
	}

	if d.AdminMutator != nil {
		wg.Go(func() { d.AdminMutator.Run(d.dctx) })
	}
}

// headerDisconnect flips the disconnecting shim's topic header to ⚪ before the
// shim is dropped from the Router — TopicForShim needs it still registered.
// No-op when headers are disabled (Header nil → Disconnected guards it).
func (d *Daemon) headerDisconnect(shimID string) {
	if d.Header == nil {
		return
	}

	if tid, ok := d.Router.TopicForShim(shimID); ok {
		d.Header.Disconnected(tid)
	}
}

// Test seams: swappable so tests can mock the comm guard and shorten waits.
var (
	isOurDaemonFn          = isOurDaemon
	socketPeerPIDFn        = socketPeerPID
	socketPeerProbeTimeout = 500 * time.Millisecond
	termWaitTimeout        = 5 * time.Second
	killWaitTimeout        = 2 * time.Second
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
	// Probe the socket first. The pidfile is unreliable as a "who owns the bot
	// token" source — a daemon spawned outside the normal lifecycle (e.g. shim
	// auto-spawn racing systemd) can hold the socket without ever writing
	// daemon.pid. SO_PEERCRED on a fresh connect tells us who is actually
	// listening right now.
	if err := d.evictSocketPeer(); err != nil {
		return err
	}

	raw, err := os.ReadFile(d.PidPath)
	if err != nil {
		slog.Debug("evictOldDaemon: no daemon.pid to consult", "err", err, "path", d.PidPath)

		return nil
	}

	old, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || old <= 1 || old == os.Getpid() {
		slog.Debug("evictOldDaemon: daemon.pid unusable", "raw", strings.TrimSpace(string(raw)), "parse_err", err, "self_pid", os.Getpid())

		return nil
	}

	alive := processIsLive(old)
	ours := isOurDaemonFn(old)

	switch {
	case alive && ours:
		return terminateOldDaemon(old)
	case alive && !ours:
		slog.Warn("daemon.pid points at foreign process — leaving it alone", "pid", old)
	default:
		slog.Info("daemon.pid stale, overwriting", "pid", old, "alive", alive, "ours", ours)
	}

	return nil
}

func (d *Daemon) evictSocketPeer() error {
	if d.SocketPath == "" {
		return nil
	}

	peer, ok := socketPeerPIDFn(d.SocketPath, socketPeerProbeTimeout)
	if !ok || peer <= 1 || peer == os.Getpid() {
		return nil
	}

	if !isOurDaemonFn(peer) {
		slog.Warn("daemon.sock peer is foreign process — leaving it alone", "pid", peer)

		return nil
	}

	slog.Info("evicting daemon detected via socket peer", "pid", peer, "socket", d.SocketPath)

	return terminateOldDaemon(peer)
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

// socketPeerPID dials socketPath and reports the listener's PID via
// SO_PEERCRED. ok=false means nothing is listening, dial timed out, or the
// kernel did not return credentials. Linux-only — falls back to ok=false
// silently on other platforms (GetsockoptUcred unavailable).
func socketPeerPID(socketPath string, timeout time.Duration) (int, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var dialer net.Dialer

	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return 0, false
	}
	defer func() { _ = conn.Close() }()

	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, false
	}

	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, false
	}

	var (
		pid     int
		credErr error
	)

	if ctlErr := raw.Control(func(fd uintptr) {
		cred, e := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
		if e != nil {
			credErr = e
			return
		}

		pid = int(cred.Pid)
	}); ctlErr != nil || credErr != nil {
		// Connect succeeded but SO_PEERCRED didn't — platform unsupported,
		// kernel denied, or a transient syscall error. Surface at debug so
		// developers running with --log-level=debug can tell this apart from
		// the much more common "nothing was listening" case.
		slog.Debug("socketPeerPID: SO_PEERCRED unavailable", "ctl_err", ctlErr, "cred_err", credErr, "socket", socketPath)

		return 0, false
	}

	if pid <= 1 {
		return 0, false
	}

	return pid, true
}

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

		d.Router.Register(&Shim{ID: id, Notify: c.Notify})
		slog.Info("shim connected", "shim_id", id)

		return res, nil
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

	listenErr := server.Listen(d.dctx)
	d.dcancel()
	idleWG.Wait()

	if listenErr != nil && !strings.Contains(listenErr.Error(), "closed") {
		return fmt.Errorf("ipc listen: %w", listenErr)
	}

	return nil
}

// claimPID writes daemon.pid; if a previous daemon owns it, signal SIGTERM
// (with /proc/<pid>/comm guard) and replace.
func (d *Daemon) claimPID() error {
	if raw, err := os.ReadFile(d.PidPath); err == nil {
		if old, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && old > 1 && old != os.Getpid() {
			if syscall.Kill(old, 0) == nil && isOurDaemon(old) {
				slog.Info("replacing stale daemon", "pid", old)
				_ = syscall.Kill(old, syscall.SIGTERM)
			}
		}
	}

	return os.WriteFile(d.PidPath, []byte(strconv.Itoa(os.Getpid())), 0o600)
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

package shim

import (
	"context"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/yakov/telegram-mcp/internal/access"
	"github.com/yakov/telegram-mcp/internal/ipc"
	mcpkg "github.com/yakov/telegram-mcp/internal/mcp"
)

const reconnectMaxBackoff = 5 * time.Second

// Shim composes the IPC client, the BotAdapter, and the mcp.Server.
// Run drives ServeStdio until ctx is done, transparently reconnecting to the
// daemon when the IPC client drops mid-session so the MCP tool surface in
// Claude Code survives daemon restarts.
type Shim struct {
	Client IPCClient
	MCP    *mcpkg.Server
	Store  *access.Store

	StateDir   string
	SocketPath string

	HelloPID   int
	HelloLabel string

	// CCPID overrides os.Getppid() for tests. Production callers leave it zero.
	CCPID int

	// DialIPC opens a new IPC client during reconnect. Defaults to ipc.Dial;
	// tests inject fakes.
	DialIPC func(socketPath string) (IPCClient, error)

	WireContext func() context.Context // injected for tests; defaults to context.Background

	// ServeStdio is injected for tests so Run can be exercised without blocking
	// on os.Stdin. Production callers leave it nil; Run falls back to MCP.ServeStdio.
	ServeStdio func(context.Context) error

	idMu  sync.RWMutex
	id    string
	alias string
	ccPID int

	clientMu sync.RWMutex

	adapter *BotAdapter
}

func (s *Shim) ShimID() (string, bool) {
	s.idMu.RLock()
	defer s.idMu.RUnlock()

	return s.id, s.id != ""
}

func (s *Shim) ShimAlias() (string, bool) {
	s.idMu.RLock()
	defer s.idMu.RUnlock()

	return s.alias, s.alias != ""
}

func (s *Shim) currentClient() IPCClient {
	s.clientMu.RLock()
	defer s.clientMu.RUnlock()

	return s.Client
}

func (s *Shim) setClient(c IPCClient) {
	s.clientMu.Lock()
	s.Client = c
	s.clientMu.Unlock()
}

func (s *Shim) Wire() error {
	adapter := NewBotAdapter(s.Client, func(reqID string) (string, string) {
		d, ok := s.MCP.LookupPermission(reqID)
		if !ok {
			return "", ""
		}

		return d.Description, d.InputPreview
	})
	s.adapter = adapter
	s.MCP.AttachBot(adapter)
	s.MCP.AttachPeerProvider(adapter)

	if s.StateDir != "" {
		AttachNotifierDebug(filepath.Join(s.StateDir, "shim-debug.log"))
	}

	AttachNotifier(s.Client, s.MCP)

	if s.HelloPID == 0 {
		s.HelloPID = os.Getpid()
	}

	ccPID := s.CCPID
	if ccPID == 0 {
		ccPID = os.Getppid()
	}

	return s.hello(context.Background(), s.Client, ccPID)
}

func (s *Shim) hello(ctx context.Context, c IPCClient, ccPID int) error {
	wctx := ctx
	if wctx == nil {
		wctx = context.Background()
	}

	if s.WireContext != nil {
		wctx = s.WireContext()
	}

	var hello struct {
		ShimID        string `json:"shim_id"`
		DaemonVersion string `json:"daemon_version"`
		Alias         string `json:"alias"`
	}

	wd, err := os.Getwd()
	if err != nil {
		slog.Warn("os.Getwd failed; sending empty workdir in Hello", "err", err)
	}

	if err := c.Call(wctx, ipc.MethodHello, map[string]any{
		"shim_pid":      s.HelloPID,
		"label":         s.HelloLabel,
		"workdir":       wd,
		"cc_session_id": os.Getenv("CLAUDE_CODE_SESSION_ID"),
	}, &hello); err != nil {
		return err
	}

	s.idMu.Lock()
	s.id = hello.ShimID
	s.alias = hello.Alias
	s.ccPID = ccPID
	s.idMu.Unlock()

	if ccPID > 0 && s.StateDir != "" {
		info := SessionInfo{
			Alias:        hello.Alias,
			ShimID:       hello.ShimID,
			ShimIDPrefix: shimIDPrefix(hello.ShimID),
			CCPID:        ccPID,
			ShimPID:      s.HelloPID,
			CCSessionID:  os.Getenv("CLAUDE_CODE_SESSION_ID"),
			Workdir:      wd,
			Label:        s.HelloLabel,
			Mode:         "shim",
		}
		if path, err := writeSessionFile(s.StateDir, info); err != nil {
			slog.Warn("session file write failed", "err", err)
		} else {
			slog.Info("session file written", "path", path, "cc_pid", ccPID)
		}
	}

	slog.Info("shim wired", "shim_id", hello.ShimID, "daemon_version", hello.DaemonVersion, "alias", hello.Alias, "shim_pid", s.HelloPID, "cc_pid", ccPID, "label", s.HelloLabel)

	return nil
}

func (s *Shim) Run(ctx context.Context) error {
	if s.DialIPC == nil {
		s.DialIPC = func(p string) (IPCClient, error) { return ipc.Dial(p) }
	}

	if err := s.Wire(); err != nil {
		return err
	}

	defer func() {
		s.idMu.RLock()
		cc := s.ccPID
		s.idMu.RUnlock()

		if cc > 0 && s.StateDir != "" {
			if err := removeSessionFile(s.StateDir, cc); err != nil {
				slog.Warn("session file remove failed", "err", err)
			}
		}

		_ = s.currentClient().Notify(ipc.MethodGoodbye, map[string]any{})
	}()

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()

	serve := s.ServeStdio
	if serve == nil {
		serve = s.MCP.ServeStdio
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- serve(sctx) }()

	reconnDone := make(chan struct{})

	go func() {
		defer close(reconnDone)

		s.reconnectLoop(sctx)
	}()

	var finalErr error
	select {
	case finalErr = <-serveErr:
	case <-ctx.Done():
		finalErr = ctx.Err()
	}

	cancel()
	<-reconnDone

	if ctxErr := ctx.Err(); ctxErr != nil && finalErr == nil {
		finalErr = ctxErr
	}

	return finalErr
}

// reconnectLoop watches the current IPC client; when it drops, it re-dials
// the daemon and re-wires the BotAdapter so the MCP server keeps serving.
func (s *Shim) reconnectLoop(ctx context.Context) {
	for {
		cur := s.currentClient()
		if cur == nil {
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-cur.Done():
		}

		if ctx.Err() != nil {
			return
		}

		slog.Warn("daemon ipc dropped, reconnecting")

		if !s.reconnectWithBackoff(ctx) {
			return
		}

		slog.Info("daemon reconnected", "socket", s.SocketPath)
	}
}

func (s *Shim) reconnectWithBackoff(ctx context.Context) bool {
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return false
		}

		newClient, dialErr := s.DialIPC(s.SocketPath)
		if dialErr == nil {
			if err := s.rewire(ctx, newClient); err != nil {
				slog.Warn("rewire after reconnect failed", "err", err)

				_ = newClient.Close()
			} else {
				return true
			}
		} else {
			slog.Warn("reconnect dial failed", "attempt", attempt, "err", dialErr)
		}

		if !waitBackoff(ctx, attempt) {
			return false
		}
	}
}

func (s *Shim) rewire(ctx context.Context, newClient IPCClient) error {
	s.idMu.RLock()
	ccPID := s.ccPID
	s.idMu.RUnlock()

	if err := s.hello(ctx, newClient, ccPID); err != nil {
		return err
	}

	AttachNotifier(newClient, s.MCP)
	s.adapter.SwapClient(newClient)
	s.setClient(newClient)

	return nil
}

func backoffDelay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}

	d := time.Duration(math.Pow(2, float64(attempt))) * 100 * time.Millisecond
	if d <= 0 || d > reconnectMaxBackoff {
		return reconnectMaxBackoff
	}

	return d
}

func waitBackoff(ctx context.Context, attempt int) bool {
	t := time.NewTimer(backoffDelay(attempt))
	defer t.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

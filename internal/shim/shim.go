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

	// ServeStdio is injected for tests so Run can be exercised without blocking
	// on os.Stdin. Production callers leave it nil; Run falls back to MCP.ServeStdio.
	ServeStdio func(context.Context) error

	idMu      sync.RWMutex
	id        string
	alias     string
	ccPID     int
	startedAt time.Time

	clientMu sync.RWMutex

	adapter *BotAdapter
	worker  *notifierWorker

	// runCancel cancels the per-Run sub-context. Set by Run() before any
	// IPC notification handlers can fire, so RequestShutdown is safe to
	// call from off-thread (notifier worker, signal handler, tests).
	runMu     sync.Mutex
	runCancel context.CancelFunc
}

// Stop drains the notifier worker. Called from Run's defer in production;
// tests that exercise Wire directly (without Run) should call it explicitly
// or via t.Cleanup so goleak sees a clean goroutine set.
func (s *Shim) Stop() {
	s.worker.Stop()
}

// RequestShutdown cancels the active Run() so the MCP serve loop unwinds
// and the process exits cleanly. Used by the daemon's NotifyShutdown
// handler (fired during /topic close for non-spawned shims). No-op if
// Run hasn't started yet or has already returned.
func (s *Shim) RequestShutdown() {
	s.runMu.Lock()
	cancel := s.runCancel
	s.runMu.Unlock()

	if cancel != nil {
		slog.Info("shim shutdown requested by daemon")
		cancel()
	}
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

// UpdateLabel mutates the in-memory label and rewrites the sessionfile so
// `telegram-mcp self` and the statusline pick up the new label without a
// CC restart. Safe to call from any goroutine; called by the daemon push
// handler registered in AttachLabelHandler.
func (s *Shim) UpdateLabel(label string) {
	s.idMu.Lock()
	s.HelloLabel = label
	id := s.id
	alias := s.alias
	ccPID := s.ccPID
	startedAt := s.startedAt
	s.idMu.Unlock()

	if id == "" || ccPID <= 0 || s.StateDir == "" {
		slog.Warn("UpdateLabel skipped sessionfile rewrite", "shim_id", id, "cc_pid", ccPID, "state_dir", s.StateDir)
		return
	}

	wd, err := os.Getwd()
	if err != nil {
		slog.Warn("os.Getwd in UpdateLabel", "err", err)
	}

	info := SessionInfo{
		Alias:        alias,
		ShimID:       id,
		ShimIDPrefix: shimIDPrefix(id),
		CCPID:        ccPID,
		ShimPID:      s.HelloPID,
		CCSessionID:  os.Getenv("CLAUDE_CODE_SESSION_ID"),
		Workdir:      wd,
		Label:        label,
		StartedAt:    startedAt,
		Mode:         "shim",
	}

	if path, err := writeSessionFile(s.StateDir, info); err != nil {
		slog.Warn("sessionfile rewrite on label change failed", "err", err)
	} else {
		slog.Info("sessionfile rewritten on label change", "path", path, "label", label)
	}
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

func (s *Shim) Wire(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

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

	if s.worker == nil {
		s.worker = newNotifierWorker()
	}

	AttachNotifier(s.Client, s.MCP, s.worker)
	AttachLabelHandler(s.Client, s, s.worker)
	AttachShutdownHandler(s.Client, s, s.worker)

	if s.HelloPID == 0 {
		s.HelloPID = os.Getpid()
	}

	ccPID := s.CCPID
	if ccPID == 0 {
		ccPID = os.Getppid()
	}

	return s.hello(ctx, s.Client, ccPID)
}

func (s *Shim) hello(ctx context.Context, c IPCClient, ccPID int) error {
	if ctx == nil {
		ctx = context.Background()
	}

	var hello struct {
		ShimID        string `json:"shim_id"`
		DaemonVersion string `json:"daemon_version"`
		Alias         string `json:"alias"`
		TopicID       int    `json:"topic_id"`
	}

	wd, err := os.Getwd()
	if err != nil {
		slog.Warn("os.Getwd failed; sending empty workdir in Hello", "err", err)
	}

	if err := c.Call(ctx, ipc.MethodHello, map[string]any{
		"shim_pid":      s.HelloPID,
		"label":         s.HelloLabel,
		"workdir":       wd,
		"cc_session_id": os.Getenv("CLAUDE_CODE_SESSION_ID"),
		"spawn_id":      os.Getenv("TELEGRAM_SPAWN_ID"),
	}, &hello); err != nil {
		return err
	}

	s.idMu.Lock()
	s.id = hello.ShimID
	s.alias = hello.Alias
	s.ccPID = ccPID

	if s.startedAt.IsZero() {
		s.startedAt = time.Now().UTC()
	}

	startedAt := s.startedAt
	s.idMu.Unlock()

	// Forward the topic assignment to the BotAdapter so every outbound
	// SendMessage/SendFile injects message_thread_id by default. Zero
	// clears any prior binding — daemon reassigns on each hello, so the
	// shim must reflect that even across reconnect.
	if s.adapter != nil {
		s.adapter.SetTopicID(hello.TopicID)
	}

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
			StartedAt:    startedAt,
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

	if err := s.Wire(ctx); err != nil {
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

		// Drain any in-flight notifier work before letting Run return so
		// tests (goleak) and production teardown see a clean goroutine set.
		s.Stop()
	}()

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()

	s.runMu.Lock()
	s.runCancel = cancel
	s.runMu.Unlock()

	defer func() {
		s.runMu.Lock()
		s.runCancel = nil
		s.runMu.Unlock()
	}()

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

	AttachNotifier(newClient, s.MCP, s.worker)
	AttachLabelHandler(newClient, s, s.worker)
	AttachShutdownHandler(newClient, s, s.worker)
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

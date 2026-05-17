package shim

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/yakov/telegram-mcp/internal/access"
	"github.com/yakov/telegram-mcp/internal/ipc"
	mcpkg "github.com/yakov/telegram-mcp/internal/mcp"
)

// Shim composes the IPC client, the BotAdapter, and the mcp.Server.
// Run drives ServeStdio until ctx is done.
type Shim struct {
	Client IPCClient
	MCP    *mcpkg.Server
	Store  *access.Store

	StateDir   string
	SocketPath string

	HelloPID   int
	HelloLabel string

	WireContext func() context.Context // injected for tests; defaults to context.Background

	idMu sync.RWMutex
	id   string
}

func (s *Shim) ShimID() (string, bool) {
	s.idMu.RLock()
	defer s.idMu.RUnlock()

	return s.id, s.id != ""
}

func (s *Shim) Wire() error {
	adapter := &BotAdapter{
		Client: s.Client,
		PermDetails: func(reqID string) (string, string) {
			d, ok := s.MCP.LookupPermission(reqID)
			if !ok {
				return "", ""
			}

			return d.Description, d.InputPreview
		},
	}
	s.MCP.AttachBot(adapter)

	if s.StateDir != "" {
		AttachNotifierDebug(filepath.Join(s.StateDir, "shim-debug.log"))
	}

	AttachNotifier(s.Client, s.MCP)

	if s.HelloPID == 0 {
		s.HelloPID = os.Getpid()
	}

	wctx := context.Background()
	if s.WireContext != nil {
		wctx = s.WireContext()
	}

	var hello struct {
		ShimID        string `json:"shim_id"`
		DaemonVersion string `json:"daemon_version"`
	}

	if err := s.Client.Call(wctx, ipc.MethodHello, map[string]any{
		"shim_pid": s.HelloPID,
		"label":    s.HelloLabel,
	}, &hello); err != nil {
		return err
	}

	s.idMu.Lock()
	s.id = hello.ShimID
	s.idMu.Unlock()

	slog.Info("shim wired", "shim_id", hello.ShimID, "daemon_version", hello.DaemonVersion, "shim_pid", s.HelloPID, "label", s.HelloLabel)

	return nil
}

func (s *Shim) Run(ctx context.Context) error {
	if err := s.Wire(); err != nil {
		return err
	}

	defer func() { _ = s.Client.Notify(ipc.MethodGoodbye, map[string]any{}) }()

	go func() {
		select {
		case <-ctx.Done():
		case <-s.Client.Done():
		}
	}()

	return s.MCP.ServeStdio(ctx)
}

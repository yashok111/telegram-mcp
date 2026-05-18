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

	idMu  sync.RWMutex
	id    string
	alias string
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
		Alias         string `json:"alias"`
	}

	wd, err := os.Getwd()
	if err != nil {
		slog.Warn("os.Getwd failed; sending empty workdir in Hello", "err", err)
	}

	if err := s.Client.Call(wctx, ipc.MethodHello, map[string]any{
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
	s.idMu.Unlock()

	if cc := os.Getenv("CLAUDE_CODE_SESSION_ID"); cc != "" && s.StateDir != "" {
		info := SessionInfo{
			Alias:        hello.Alias,
			ShimID:       hello.ShimID,
			ShimIDPrefix: shimIDPrefix(hello.ShimID),
			CCSessionID:  cc,
			Workdir:      wd,
			Label:        s.HelloLabel,
			Mode:         "shim",
		}
		if path, err := writeSessionFile(s.StateDir, info); err != nil {
			slog.Warn("session file write failed", "err", err)
		} else {
			slog.Info("session file written", "path", path)
		}
	}

	slog.Info("shim wired", "shim_id", hello.ShimID, "daemon_version", hello.DaemonVersion, "alias", hello.Alias, "shim_pid", s.HelloPID, "label", s.HelloLabel)

	return nil
}

func (s *Shim) Run(ctx context.Context) error {
	if err := s.Wire(); err != nil {
		return err
	}

	defer func() {
		if cc := os.Getenv("CLAUDE_CODE_SESSION_ID"); cc != "" && s.StateDir != "" {
			if err := removeSessionFile(s.StateDir, cc); err != nil {
				slog.Warn("session file remove failed", "err", err)
			}
		}
		_ = s.Client.Notify(ipc.MethodGoodbye, map[string]any{})
	}()

	go func() {
		select {
		case <-ctx.Done():
		case <-s.Client.Done():
		}
	}()

	return s.MCP.ServeStdio(ctx)
}

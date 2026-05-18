package shim

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/yakov/telegram-mcp/internal/ipc"
)

// MCPSink is the slice of mcp.Server the notifier writes into.
type MCPSink interface {
	DeliverInbound(content string, meta map[string]string)
	ResolvePermission(requestID, behavior string)
}

// StatsSink is an optional diagnostic interface — when MCPSink also implements
// it, the notifier logs MCP session state to shim-debug.log so we can verify
// notifications arrive only after the stdio handshake completed.
type StatsSink interface {
	SessionStats() (int32, int32)
}

// debugLogPath, if non-empty, receives one JSON line per inbound the shim
// pulls off the IPC wire. Used to verify daemon→shim delivery independently
// of MCP session lifecycle. Empty by default — set via AttachNotifierDebug.
var debugLogPath string

// AttachNotifierDebug wires AttachNotifier with a per-shim diagnostic log
// file at path. Empty path disables. Path is process-global; the last call
// wins (we expect one shim per process).
func AttachNotifierDebug(path string) { debugLogPath = path }

func AttachNotifier(c IPCClient, sink MCPSink) {
	c.OnNotify(ipc.NotifyInbound, func(_ context.Context, params json.RawMessage) {
		var p struct {
			Content string            `json:"content"`
			Meta    map[string]string `json:"meta"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			slog.Warn("inbound unmarshal", "err", err)
			return
		}

		slog.Info("shim received inbound", "chat_id", p.Meta["chat_id"], "user", p.Meta["user"], "content_len", len(p.Content))

		preFields := map[string]any{
			"chat_id":     p.Meta["chat_id"],
			"user":        p.Meta["user"],
			"content_len": len(p.Content),
			"pid":         os.Getpid(),
		}

		if stats, ok := sink.(StatsSink); ok {
			r, i := stats.SessionStats()
			preFields["mcp_sessions_registered"] = r
			preFields["mcp_sessions_inited"] = i
		}

		writeDebug("inbound", preFields)

		sink.DeliverInbound(p.Content, p.Meta)

		postFields := map[string]any{
			"chat_id": p.Meta["chat_id"],
			"pid":     os.Getpid(),
		}

		if stats, ok := sink.(StatsSink); ok {
			r, i := stats.SessionStats()
			postFields["mcp_sessions_registered"] = r
			postFields["mcp_sessions_inited"] = i
		}

		writeDebug("inbound_delivered_to_mcp", postFields)
	})

	c.OnNotify(ipc.NotifyPermissionResolved, func(_ context.Context, params json.RawMessage) {
		var p struct {
			RequestID string `json:"request_id"`
			Behavior  string `json:"behavior"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			slog.Warn("perm resolved unmarshal", "err", err)
			return
		}

		writeDebug("permission_resolved", map[string]any{
			"request_id": p.RequestID,
			"behavior":   p.Behavior,
			"pid":        os.Getpid(),
		})

		sink.ResolvePermission(p.RequestID, p.Behavior)
	})
}

// LabelUpdater receives runtime label changes pushed by the daemon. The shim
// implements this to rewrite its sessionfile so `telegram-mcp self` and the
// statusline pick up the new label without a CC restart.
type LabelUpdater interface {
	UpdateLabel(label string)
}

// AttachLabelHandler registers the daemon→shim label-change notification handler.
// nil updater disables the handler (test-only path).
func AttachLabelHandler(c IPCClient, updater LabelUpdater) {
	if updater == nil {
		return
	}

	c.OnNotify(ipc.NotifyLabelChanged, func(_ context.Context, params json.RawMessage) {
		var p struct {
			Label string `json:"label"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			slog.Warn("label notify unmarshal", "err", err)
			return
		}

		slog.Info("shim received label change", "label", p.Label)
		updater.UpdateLabel(p.Label)
	})
}

func writeDebug(event string, fields map[string]any) {
	if debugLogPath == "" {
		return
	}

	fields["event"] = event
	fields["ts"] = time.Now().Format(time.RFC3339Nano)

	line, err := json.Marshal(fields)
	if err != nil {
		return
	}

	f, err := os.OpenFile(debugLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // debugLogPath is set internally from StateDir, not user input.
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()

	_, _ = fmt.Fprintln(f, string(line))
}

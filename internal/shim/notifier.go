package shim

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/yakov/telegram-mcp/internal/ipc"
)

// MCPSink is the slice of mcp.Server the notifier writes into.
type MCPSink interface {
	DeliverInbound(content string, meta map[string]string)
	ResolvePermission(requestID, behavior string)
}

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

		sink.DeliverInbound(p.Content, p.Meta)
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

		sink.ResolvePermission(p.RequestID, p.Behavior)
	})
}

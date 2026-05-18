package daemon

import (
	"log/slog"
	"strconv"

	"github.com/yakov/telegram-mcp/internal/bot"
	"github.com/yakov/telegram-mcp/internal/ipc"
)

// Notifier implements bot.Notifier by routing daemon-side bot callbacks to
// the right shim over IPC. The bot package doesn't import daemon — it sees
// only bot.Notifier.
type Notifier struct {
	router *Router
}

func NewNotifier(r *Router) *Notifier { return &Notifier{router: r} }

// DeliverInbound fans an inbound Telegram message out to every target shim
// resolved by the Router. RouteInboundMulti returns a snapshot of *Shim
// pointers and the Router's mu is released on its return — so the per-target
// Notify calls below run concurrently across DeliverInbound invocations for
// different chats, never serialized on r.mu.
func (n *Notifier) DeliverInbound(content string, meta map[string]string) {
	chatID := meta["chat_id"]
	replyToMsgID, _ := strconv.Atoi(meta["reply_to_message_id"])

	targets := n.router.RouteInboundMulti(chatID, content, replyToMsgID)
	if len(targets) == 0 {
		slog.Warn("inbound dropped: no shim connected", "chat_id", chatID, "user", meta["user"])
		return
	}

	params := map[string]any{
		"content": content,
		"meta":    meta,
	}

	slog.Info("DeliverInbound dispatch",
		"chat_id", chatID,
		"fanout", len(targets),
		"targets", shimIDs(targets),
		"content_len", len(content),
	)

	for _, t := range targets {
		if err := t.Notify(ipc.NotifyInbound, params); err != nil {
			slog.Error("inbound notify failed", "shim_id", t.ID, "chat_id", chatID, "err", err)
		}
	}
}

func shimIDs(targets []*Shim) []string {
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		out = append(out, t.ID)
	}

	return out
}

func (n *Notifier) LookupPermission(requestID string) (bot.PermissionDetails, bool) {
	d, ok := n.router.LookupPermissionDetails(requestID)
	if !ok {
		return bot.PermissionDetails{}, false
	}

	return bot.PermissionDetails{
		ToolName:     d.ToolName,
		Description:  d.Description,
		InputPreview: d.InputPreview,
	}, true
}

func (n *Notifier) ResolvePermission(requestID, behavior string) {
	target, ok := n.router.RoutePermission(requestID)
	n.router.ResolvePermission(requestID)

	if !ok {
		slog.Warn("permission resolution dropped: shim gone", "request_id", requestID, "behavior", behavior)
		return
	}

	if err := target.Notify(ipc.NotifyPermissionResolved, map[string]any{
		"request_id": requestID,
		"behavior":   behavior,
	}); err != nil {
		slog.Error("permission notify failed", "shim_id", target.ID, "request_id", requestID, "err", err)
	}
}

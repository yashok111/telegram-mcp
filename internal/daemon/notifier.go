package daemon

import (
	"log/slog"
	"strconv"

	"github.com/yakov/telegram-mcp/internal/access"
	"github.com/yakov/telegram-mcp/internal/bot"
	"github.com/yakov/telegram-mcp/internal/ipc"
)

// Notifier implements bot.Notifier by routing daemon-side bot callbacks to
// the right shim over IPC. The bot package doesn't import daemon — it sees
// only bot.Notifier.
type Notifier struct {
	router *Router
	store  *access.Store
	typing *TypingTracker
}

// NewNotifier wires the router used to fan out inbound messages plus, optionally,
// a TypingTracker (nil disables typing-refresh) and the access.Store used to
// decide whether the rotating-reaction half of the indicator should fire (only
// when access.State.AckReaction is non-empty). Passing nil for store is allowed
// and silently disables reaction rotation while typing-refresh still runs.
func NewNotifier(r *Router, store *access.Store, typing *TypingTracker) *Notifier {
	return &Notifier{router: r, store: store, typing: typing}
}

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

	// Mark chat for typing-refresh BEFORE notifying shims. If the shim is fast
	// enough to send an outbound (and the IPC handler calls Clear) before this
	// goroutine reaches Mark, the order would invert and Mark would re-add a
	// just-cleared chat — leaving the typing indicator armed for one full TTL.
	msgID, _ := strconv.Atoi(meta["message_id"])
	n.typing.Mark(chatID, msgID, n.shouldRotateReaction())

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

// shouldRotateReaction reports whether the user opted into ack reactions.
// Returns false when the store is unwired (tests) or AckReaction is unset,
// which keeps reaction rotation aligned with bot.handleMessage's initial
// reaction placement.
func (n *Notifier) shouldRotateReaction() bool {
	if n.store == nil {
		return false
	}

	return n.store.Load().AckReaction != ""
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

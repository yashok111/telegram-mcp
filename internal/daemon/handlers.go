package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/yakov/telegram-mcp/internal/access"
	"github.com/yakov/telegram-mcp/internal/bot"
	"github.com/yakov/telegram-mcp/internal/ipc"
)

const (
	metaShimID      = "shim_id"
	metaLabel       = "label"
	metaWorkdir     = "workdir"
	metaCCSessionID = "cc_session_id"
	metaSpawnID     = "spawn_id"
)

// DaemonVersion is wired in via -ldflags at build time; default suffices for dev.
var DaemonVersion = "dev"

// botSurface mirrors the methods the daemon needs from *bot.Bot. Lets tests use a fake.
type botSurface interface {
	SendMessage(ctx context.Context, chatID, text string, opts bot.SendOpts) (int, error)
	SendFile(ctx context.Context, chatID, path string, opts bot.SendOpts) (int, error)
	EditMessage(ctx context.Context, chatID string, msgID int, text, parseMode string) (int, error)
	React(ctx context.Context, chatID string, msgID int, emoji string) error
	SendChatAction(ctx context.Context, chatID, action string) error
	DownloadFile(ctx context.Context, fileID string) (string, error)
	BroadcastPermissionRequest(ctx context.Context, prefix, requestID, toolName string)
}

type Handlers struct {
	store  *access.Store
	bot    botSurface
	router *Router
	typing *TypingTracker
}

func NewHandlers(store *access.Store, b botSurface, r *Router, typing *TypingTracker) *Handlers {
	return &Handlers{store: store, bot: b, router: r, typing: typing}
}

func (h *Handlers) shimID(c *ipc.Conn) string {
	v, ok := c.Meta.Load(metaShimID)
	if !ok {
		return ""
	}

	s, _ := v.(string)

	return s
}

// textPrefixFor resolves the `@sN: ` source-alias prefix for the shim that
// owns conn c. Returns "" when prefix injection is disabled via env or the
// shim isn't registered (anonymous / pre-hello). Used to mark every outbound
// text/edit so a Telegram user can see which session is replying without
// running /sessions.
func (h *Handlers) textPrefixFor(c *ipc.Conn) string {
	if !prefixEnabled() {
		return ""
	}

	return formatTextPrefix(h.router.AliasForShim(h.shimID(c)))
}

// captionFor is the file-upload counterpart of textPrefixFor: returns the
// shorter `@sN` marker for use as a Telegram photo/document caption.
func (h *Handlers) captionFor(c *ipc.Conn) string {
	if !prefixEnabled() {
		return ""
	}

	return formatCaption(h.router.AliasForShim(h.shimID(c)))
}

func (h *Handlers) gate(chatID string) *ipc.Error {
	st := h.store.Load()
	if access.Allowed(st, chatID) {
		return nil
	}

	slog.Warn("gate denied: chat not allowlisted", "chat_id", chatID)

	data, _ := json.Marshal(map[string]string{"chat_id": chatID})

	return &ipc.Error{Code: ipc.CodeNotAllowlisted, Message: "chat not allowlisted", Data: data}
}

func (h *Handlers) HandleHello(_ context.Context, c *ipc.Conn, params json.RawMessage) (any, *ipc.Error) {
	var p struct {
		ShimPID     int    `json:"shim_pid"`
		Label       string `json:"label"`
		Workdir     string `json:"workdir"`
		CCSessionID string `json:"cc_session_id"`
		SpawnID     string `json:"spawn_id"`
	}

	if err := json.Unmarshal(params, &p); err != nil {
		slog.Warn("hello params unmarshal failed", "err", err)
	}

	buf := make([]byte, 6)
	_, _ = rand.Read(buf)

	id := hex.EncodeToString(buf)
	c.Meta.Store(metaShimID, id)
	c.Meta.Store(metaLabel, p.Label)
	c.Meta.Store(metaWorkdir, p.Workdir)
	c.Meta.Store(metaCCSessionID, p.CCSessionID)
	c.Meta.Store(metaSpawnID, p.SpawnID)

	slog.Info("hello received",
		"shim_id", id, "shim_pid", p.ShimPID, "label", p.Label,
		"workdir", p.Workdir, "cc_session_id", p.CCSessionID,
		"spawn_id", p.SpawnID, "daemon_version", DaemonVersion)

	return map[string]any{"shim_id": id, "daemon_version": DaemonVersion}, nil
}

func (h *Handlers) HandleSendMessage(ctx context.Context, c *ipc.Conn, params json.RawMessage) (any, *ipc.Error) {
	var p struct {
		ChatID    string `json:"chat_id"`
		Text      string `json:"text"`
		ReplyTo   int    `json:"reply_to"`
		ParseMode string `json:"parse_mode"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &ipc.Error{Code: ipc.CodeInvalidParams, Message: err.Error()}
	}

	if rpcErr := h.gate(p.ChatID); rpcErr != nil {
		return nil, rpcErr
	}

	text := h.textPrefixFor(c) + p.Text

	id, err := h.bot.SendMessage(ctx, p.ChatID, text, bot.SendOpts{ReplyTo: p.ReplyTo, ParseMode: p.ParseMode})
	if err != nil {
		slog.Error("bot.SendMessage failed", "shim_id", h.shimID(c), "chat_id", p.ChatID, "text_len", len(text), "err", err)
		return nil, &ipc.Error{Code: ipc.CodeBotError, Message: err.Error()}
	}

	slog.Info("bot.SendMessage ok", "shim_id", h.shimID(c), "chat_id", p.ChatID, "message_id", id, "text_len", len(text), "reply_to", p.ReplyTo, "parse_mode", p.ParseMode)

	h.router.RecordOutbound(h.shimID(c), p.ChatID, id)
	h.typing.Clear(p.ChatID)

	return map[string]any{"message_id": id}, nil
}

func (h *Handlers) HandleSendFile(ctx context.Context, c *ipc.Conn, params json.RawMessage) (any, *ipc.Error) {
	var p struct {
		ChatID  string `json:"chat_id"`
		Path    string `json:"path"`
		ReplyTo int    `json:"reply_to"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &ipc.Error{Code: ipc.CodeInvalidParams, Message: err.Error()}
	}

	if rpcErr := h.gate(p.ChatID); rpcErr != nil {
		return nil, rpcErr
	}

	id, err := h.bot.SendFile(ctx, p.ChatID, p.Path, bot.SendOpts{ReplyTo: p.ReplyTo, Caption: h.captionFor(c)})
	if err != nil {
		slog.Error("bot.SendFile failed", "shim_id", h.shimID(c), "chat_id", p.ChatID, "path", p.Path, "err", err)
		return nil, &ipc.Error{Code: ipc.CodeBotError, Message: err.Error()}
	}

	slog.Info("bot.SendFile ok", "shim_id", h.shimID(c), "chat_id", p.ChatID, "message_id", id, "path", p.Path, "reply_to", p.ReplyTo)

	h.router.RecordOutbound(h.shimID(c), p.ChatID, id)
	h.typing.Clear(p.ChatID)

	return map[string]any{"message_id": id}, nil
}

func (h *Handlers) HandleEditMessage(ctx context.Context, c *ipc.Conn, params json.RawMessage) (any, *ipc.Error) {
	var p struct {
		ChatID    string `json:"chat_id"`
		MessageID int    `json:"message_id"`
		Text      string `json:"text"`
		ParseMode string `json:"parse_mode"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &ipc.Error{Code: ipc.CodeInvalidParams, Message: err.Error()}
	}

	if rpcErr := h.gate(p.ChatID); rpcErr != nil {
		return nil, rpcErr
	}

	text := h.textPrefixFor(c) + p.Text

	id, err := h.bot.EditMessage(ctx, p.ChatID, p.MessageID, text, p.ParseMode)
	if err != nil {
		slog.Error("bot.EditMessage failed", "chat_id", p.ChatID, "message_id", p.MessageID, "err", err)
		return nil, &ipc.Error{Code: ipc.CodeBotError, Message: err.Error()}
	}

	slog.Info("bot.EditMessage ok", "chat_id", p.ChatID, "message_id", id, "text_len", len(text), "parse_mode", p.ParseMode)

	h.typing.Clear(p.ChatID)

	return map[string]any{"message_id": id}, nil
}

func (h *Handlers) HandleReact(ctx context.Context, _ *ipc.Conn, params json.RawMessage) (any, *ipc.Error) {
	var p struct {
		ChatID    string `json:"chat_id"`
		MessageID int    `json:"message_id"`
		Emoji     string `json:"emoji"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &ipc.Error{Code: ipc.CodeInvalidParams, Message: err.Error()}
	}

	if rpcErr := h.gate(p.ChatID); rpcErr != nil {
		return nil, rpcErr
	}

	if err := h.bot.React(ctx, p.ChatID, p.MessageID, p.Emoji); err != nil {
		slog.Error("bot.React failed", "chat_id", p.ChatID, "message_id", p.MessageID, "emoji", p.Emoji, "err", err)
		return nil, &ipc.Error{Code: ipc.CodeBotError, Message: err.Error()}
	}

	slog.Info("bot.React ok", "chat_id", p.ChatID, "message_id", p.MessageID, "emoji", p.Emoji)

	return map[string]any{}, nil
}

func (h *Handlers) HandleDownloadFile(ctx context.Context, _ *ipc.Conn, params json.RawMessage) (any, *ipc.Error) {
	var p struct {
		FileID string `json:"file_id"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &ipc.Error{Code: ipc.CodeInvalidParams, Message: err.Error()}
	}

	path, err := h.bot.DownloadFile(ctx, p.FileID)
	if err != nil {
		slog.Error("bot.DownloadFile failed", "file_id", p.FileID, "err", err)
		return nil, &ipc.Error{Code: ipc.CodeBotError, Message: err.Error()}
	}

	slog.Info("bot.DownloadFile ok", "file_id", p.FileID, "path", path)

	return map[string]any{"path": path}, nil
}

func (h *Handlers) HandleBroadcastPermission(ctx context.Context, c *ipc.Conn, params json.RawMessage) (any, *ipc.Error) {
	var p struct {
		RequestID    string `json:"request_id"`
		ToolName     string `json:"tool_name"`
		Description  string `json:"description"`
		InputPreview string `json:"input_preview"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &ipc.Error{Code: ipc.CodeInvalidParams, Message: err.Error()}
	}

	shimID := h.shimID(c)
	if err := h.router.RegisterPermission(p.RequestID, shimID, PermDetails{
		ToolName: p.ToolName, Description: p.Description, InputPreview: p.InputPreview,
	}); err != nil {
		slog.Warn("permission register collision", "request_id", p.RequestID, "shim_id", shimID, "tool", p.ToolName, "err", err)
		data, _ := json.Marshal(map[string]string{"request_id": p.RequestID})

		return nil, &ipc.Error{Code: ipc.CodeRequestIDCollision, Message: err.Error(), Data: data}
	}

	slog.Info("permission registered", "request_id", p.RequestID, "shim_id", shimID, "tool", p.ToolName, "desc_len", len(p.Description), "preview_len", len(p.InputPreview))

	h.bot.BroadcastPermissionRequest(ctx, h.textPrefixFor(c), p.RequestID, p.ToolName)

	return map[string]any{}, nil
}

func (h *Handlers) HandlePeers(_ context.Context, c *ipc.Conn, _ json.RawMessage) (any, *ipc.Error) {
	callerID := h.shimID(c)
	snap := h.router.Snapshot()
	now := time.Now()

	peers := make([]PeerInfo, len(snap))
	for i, s := range snap {
		peers[i] = PeerInfo{
			Alias:        s.Alias,
			ShimIDPrefix: s.IDPrefix(),
			Workdir:      s.Workdir,
			Label:        s.Label,
			IdleSeconds:  int(s.IdleFor(now).Round(time.Second).Seconds()),
			Self:         s.ID == callerID,
		}
	}

	slog.Info("daemon.peers served", "caller_shim_id", callerID, "peer_count", len(peers))

	return map[string]any{"peers": peers}, nil
}

func (h *Handlers) Register(s *ipc.Server) {
	s.Handle(ipc.MethodHello, h.HandleHello)
	s.Handle(ipc.MethodBotSendMessage, h.HandleSendMessage)
	s.Handle(ipc.MethodBotSendFile, h.HandleSendFile)
	s.Handle(ipc.MethodBotEditMessage, h.HandleEditMessage)
	s.Handle(ipc.MethodBotReact, h.HandleReact)
	s.Handle(ipc.MethodBotDownloadFile, h.HandleDownloadFile)
	s.Handle(ipc.MethodBotBroadcastPermissionRequest, h.HandleBroadcastPermission)
	s.Handle(ipc.MethodDaemonPeers, h.HandlePeers)

	s.HandleNotify(ipc.MethodGoodbye, func(_ context.Context, c *ipc.Conn, _ json.RawMessage) {
		slog.Info("goodbye received", "shim_id", h.shimID(c))
	})
}

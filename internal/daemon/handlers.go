package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"

	"github.com/yakov/telegram-mcp/internal/access"
	"github.com/yakov/telegram-mcp/internal/bot"
	"github.com/yakov/telegram-mcp/internal/ipc"
)

const (
	metaShimID = "shim_id"
	metaLabel  = "label"
)

// DaemonVersion is wired in via -ldflags at build time; default suffices for dev.
var DaemonVersion = "dev"

// botSurface mirrors the methods the daemon needs from *bot.Bot. Lets tests use a fake.
type botSurface interface {
	SendMessage(ctx context.Context, chatID, text string, opts bot.SendOpts) (int, error)
	SendFile(ctx context.Context, chatID, path string, opts bot.SendOpts) (int, error)
	EditMessage(ctx context.Context, chatID string, msgID int, text, parseMode string) (int, error)
	React(ctx context.Context, chatID string, msgID int, emoji string) error
	DownloadFile(ctx context.Context, fileID string) (string, error)
	BroadcastPermissionRequest(ctx context.Context, requestID, toolName string)
}

type Handlers struct {
	store  *access.Store
	bot    botSurface
	router *Router
}

func NewHandlers(store *access.Store, b botSurface, r *Router) *Handlers {
	return &Handlers{store: store, bot: b, router: r}
}

func (h *Handlers) shimID(c *ipc.Conn) string {
	v, ok := c.Meta.Load(metaShimID)
	if !ok {
		return ""
	}

	s, _ := v.(string)

	return s
}

func (h *Handlers) gate(chatID string) *ipc.Error {
	st := h.store.Load()
	if access.Allowed(st, chatID) {
		return nil
	}

	data, _ := json.Marshal(map[string]string{"chat_id": chatID})

	return &ipc.Error{Code: ipc.CodeNotAllowlisted, Message: "chat not allowlisted", Data: data}
}

func (h *Handlers) HandleHello(_ context.Context, c *ipc.Conn, params json.RawMessage) (any, *ipc.Error) {
	var p struct {
		ShimPID int    `json:"shim_pid"`
		Label   string `json:"label"`
	}

	_ = json.Unmarshal(params, &p)

	buf := make([]byte, 6)
	_, _ = rand.Read(buf)

	id := hex.EncodeToString(buf)
	c.Meta.Store(metaShimID, id)
	c.Meta.Store(metaLabel, p.Label)

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

	id, err := h.bot.SendMessage(ctx, p.ChatID, p.Text, bot.SendOpts{ReplyTo: p.ReplyTo, ParseMode: p.ParseMode})
	if err != nil {
		return nil, &ipc.Error{Code: ipc.CodeBotError, Message: err.Error()}
	}

	h.router.RecordOutbound(h.shimID(c), p.ChatID)

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

	id, err := h.bot.SendFile(ctx, p.ChatID, p.Path, bot.SendOpts{ReplyTo: p.ReplyTo})
	if err != nil {
		return nil, &ipc.Error{Code: ipc.CodeBotError, Message: err.Error()}
	}

	h.router.RecordOutbound(h.shimID(c), p.ChatID)

	return map[string]any{"message_id": id}, nil
}

func (h *Handlers) HandleEditMessage(ctx context.Context, _ *ipc.Conn, params json.RawMessage) (any, *ipc.Error) {
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

	id, err := h.bot.EditMessage(ctx, p.ChatID, p.MessageID, p.Text, p.ParseMode)
	if err != nil {
		return nil, &ipc.Error{Code: ipc.CodeBotError, Message: err.Error()}
	}

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
		return nil, &ipc.Error{Code: ipc.CodeBotError, Message: err.Error()}
	}

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
		return nil, &ipc.Error{Code: ipc.CodeBotError, Message: err.Error()}
	}

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
		data, _ := json.Marshal(map[string]string{"request_id": p.RequestID})
		return nil, &ipc.Error{Code: ipc.CodeRequestIDCollision, Message: err.Error(), Data: data}
	}

	h.bot.BroadcastPermissionRequest(ctx, p.RequestID, p.ToolName)

	return map[string]any{}, nil
}

func (h *Handlers) Register(s *ipc.Server) {
	s.Handle(ipc.MethodHello, h.HandleHello)
	s.Handle(ipc.MethodBotSendMessage, h.HandleSendMessage)
	s.Handle(ipc.MethodBotSendFile, h.HandleSendFile)
	s.Handle(ipc.MethodBotEditMessage, h.HandleEditMessage)
	s.Handle(ipc.MethodBotReact, h.HandleReact)
	s.Handle(ipc.MethodBotDownloadFile, h.HandleDownloadFile)
	s.Handle(ipc.MethodBotBroadcastPermissionRequest, h.HandleBroadcastPermission)

	s.HandleNotify(ipc.MethodGoodbye, func(context.Context, *ipc.Conn, json.RawMessage) {
		// graceful disconnect is signaled by Conn close; nothing to do.
	})
}

// Package shim hosts the per-CC-session process: stdio MCP server in front,
// IPC client to the daemon behind. The Telegram bot token never enters this
// process — it lives only in the daemon.
package shim

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"

	"github.com/yakov/telegram-mcp/internal/bot"
	"github.com/yakov/telegram-mcp/internal/ipc"
	mcpkg "github.com/yakov/telegram-mcp/internal/mcp"
)

var _ mcpkg.PeerProvider = (*BotAdapter)(nil)

// Sentinel errors map from JSON-RPC error codes returned by the daemon, so
// the MCP tool layer can produce consistent error messages.
var (
	ErrNotAllowlisted     = errors.New("chat not allowlisted")
	ErrAttachmentTooLarge = errors.New("attachment too large")
	ErrNotSendable        = errors.New("path not sendable")
	ErrRequestIDCollision = errors.New("permission request_id already in use")
	ErrDaemonUnreachable  = errors.New("daemon unreachable, retrying")
)

// IPCClient is the subset of ipc.Client the adapter needs. Lets tests inject
// a fake without spinning up a unix socket.
type IPCClient interface {
	Call(ctx context.Context, method string, params, result any) error
	Notify(method string, params any) error
	OnNotify(method string, h ipc.NotifyHandler)
	Close() error
	Done() <-chan struct{}
}

// PermDetailsProvider lets the adapter populate description/input_preview
// when forwarding broadcastPermissionRequest. Returns ("", "") if unknown —
// the daemon will still send the keyboard, just without details for "See more".
type PermDetailsProvider func(requestID string) (description, inputPreview string)

// BotAdapter implements mcp.BotAPI by forwarding to the daemon over IPC.
type BotAdapter struct {
	PermDetails PermDetailsProvider

	mu     sync.RWMutex
	client IPCClient
}

func NewBotAdapter(c IPCClient, perm PermDetailsProvider) *BotAdapter {
	return &BotAdapter{client: c, PermDetails: perm}
}

func (a *BotAdapter) Client() IPCClient {
	a.mu.RLock()
	defer a.mu.RUnlock()

	return a.client
}

func (a *BotAdapter) SwapClient(c IPCClient) {
	a.mu.Lock()
	a.client = c
	a.mu.Unlock()
}

func (a *BotAdapter) SendMessage(ctx context.Context, chatID, text string, opts bot.SendOpts) (int, error) {
	var res struct {
		MessageID int `json:"message_id"`
	}

	c := a.Client()
	err := c.Call(ctx, ipc.MethodBotSendMessage, map[string]any{
		"chat_id": chatID, "text": text, "reply_to": opts.ReplyTo, "parse_mode": opts.ParseMode,
	}, &res)
	if err != nil {
		return 0, mapErr(err)
	}

	return res.MessageID, nil
}

func (a *BotAdapter) SendFile(ctx context.Context, chatID, path string, opts bot.SendOpts) (int, error) {
	var res struct {
		MessageID int `json:"message_id"`
	}

	c := a.Client()
	err := c.Call(ctx, ipc.MethodBotSendFile, map[string]any{
		"chat_id": chatID, "path": path, "reply_to": opts.ReplyTo,
	}, &res)
	if err != nil {
		return 0, mapErr(err)
	}

	return res.MessageID, nil
}

func (a *BotAdapter) EditMessage(ctx context.Context, chatID string, msgID int, text, parseMode string) (int, error) {
	var res struct {
		MessageID int `json:"message_id"`
	}

	c := a.Client()
	err := c.Call(ctx, ipc.MethodBotEditMessage, map[string]any{
		"chat_id": chatID, "message_id": msgID, "text": text, "parse_mode": parseMode,
	}, &res)
	if err != nil {
		return 0, mapErr(err)
	}

	return res.MessageID, nil
}

func (a *BotAdapter) React(ctx context.Context, chatID string, msgID int, emoji string) error {
	c := a.Client()

	return mapErr(c.Call(ctx, ipc.MethodBotReact, map[string]any{
		"chat_id": chatID, "message_id": msgID, "emoji": emoji,
	}, nil))
}

func (a *BotAdapter) DownloadFile(ctx context.Context, fileID string) (string, error) {
	var res struct {
		Path string `json:"path"`
	}

	c := a.Client()
	err := c.Call(ctx, ipc.MethodBotDownloadFile, map[string]any{"file_id": fileID}, &res)
	if err != nil {
		return "", mapErr(err)
	}

	return res.Path, nil
}

func (a *BotAdapter) Peers(ctx context.Context) ([]mcpkg.Peer, error) {
	var res struct {
		Peers []mcpkg.Peer `json:"peers"`
	}

	c := a.Client()
	if err := c.Call(ctx, ipc.MethodDaemonPeers, struct{}{}, &res); err != nil {
		return nil, mapErr(err)
	}

	return res.Peers, nil
}

func (a *BotAdapter) BroadcastPermissionRequest(ctx context.Context, requestID, toolName string) {
	desc, preview := "", ""
	if a.PermDetails != nil {
		desc, preview = a.PermDetails(requestID)
	}

	c := a.Client()
	_ = c.Call(ctx, ipc.MethodBotBroadcastPermissionRequest, map[string]any{
		"request_id": requestID, "tool_name": toolName,
		"description": desc, "input_preview": preview,
	}, nil)
}

func mapErr(err error) error {
	if err == nil {
		return nil
	}

	if errors.Is(err, net.ErrClosed) || strings.Contains(err.Error(), "connection closed") {
		return ErrDaemonUnreachable
	}

	var rpcErr *ipc.Error
	if !errors.As(err, &rpcErr) {
		return err
	}

	switch rpcErr.Code {
	case ipc.CodeNotAllowlisted:
		return ErrNotAllowlisted
	case ipc.CodeAttachmentTooLarge:
		return ErrAttachmentTooLarge
	case ipc.CodeNotSendable:
		return ErrNotSendable
	case ipc.CodeRequestIDCollision:
		return ErrRequestIDCollision
	default:
		return errors.New(rpcErr.Message)
	}
}

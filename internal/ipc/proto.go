// Package ipc implements JSON-RPC 2.0 over Content-Length framed unix sockets.
// Used by the daemon ↔ shim relay so multiple Claude Code sessions can share
// a single Telegram bot poller.
package ipc

import "encoding/json"

const JSONRPCVersion = "2.0"

// Request is a JSON-RPC 2.0 call that expects a Response.
type Request struct {
	Jsonrpc string          `json:"jsonrpc"`
	ID      uint64          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response correlates with a Request by ID. Exactly one of Result or Error is set.
type Response struct {
	Jsonrpc string          `json:"jsonrpc"`
	ID      uint64          `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Notification is a fire-and-forget message; no Response will be sent.
type Notification struct {
	Jsonrpc string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Error is the JSON-RPC error object.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// Method names exchanged between shim and daemon.
const (
	MethodHello                         = "hello"
	MethodGoodbye                       = "goodbye"
	MethodBotSendMessage                = "bot.sendMessage"
	MethodBotSendFile                   = "bot.sendFile"
	MethodBotEditMessage                = "bot.editMessage"
	MethodBotReact                      = "bot.react"
	MethodBotDownloadFile               = "bot.downloadFile"
	MethodBotBroadcastPermissionRequest = "bot.broadcastPermissionRequest"
	MethodDaemonPeers                   = "daemon.peers"
	NotifyInbound                       = "notifications/inbound"
	NotifyPermissionResolved            = "notifications/permission/resolved"
	NotifyLabelChanged                  = "notifications/label/changed"
)

// Custom error codes (JSON-RPC reserves -32000..-32099 for application errors).
const (
	CodeNotAllowlisted     = -32001
	CodeBotError           = -32002
	CodeNotSendable        = -32003
	CodeAttachmentTooLarge = -32004
	CodeRequestIDCollision = -32005
	CodeInternal           = -32603 // JSON-RPC reserved
	CodeMethodNotFound     = -32601
	CodeInvalidParams      = -32602
)

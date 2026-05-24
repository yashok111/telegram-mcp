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
	// MethodAdminSnapshot returns live in-memory daemon state (connected
	// shims, spawns, bg tasks) to the admin-tools MCP server. Token-gated
	// per-call; the caller never does hello, so it is not a routable shim.
	MethodAdminSnapshot = "admin.snapshot"
	// MethodAdminMutate requests a daemon-side state mutation (label/pin/
	// evict/pairing/allow/rule/effort/cancel/broadcast). Token-gated like
	// admin.snapshot. The daemon classifies the tool's tier authoritatively:
	// Tier-2 (low-risk) applies immediately + reports; Tier-3 (high-risk)
	// registers a pending mutation and renders an owner ✅/❌ confirm.
	MethodAdminMutate        = "admin.mutate"
	NotifyInbound            = "notifications/inbound"
	NotifyPermissionResolved = "notifications/permission/resolved"
	NotifyLabelChanged       = "notifications/label/changed"
	// NotifyShutdown asks the shim to exit gracefully — daemon fires it
	// when /topic close kills a non-spawned shim. Shim cancels its run
	// context, drains the notifier worker, and exits with status 0.
	NotifyShutdown = "notifications/shutdown"
	// NotifyAdminEvent is a daemon→admin-agent push for an anomaly event.
	NotifyAdminEvent = "notifications/admin/event"
	// NotifyAdminSitrep is a daemon→admin-agent push for a daily sitrep trigger.
	NotifyAdminSitrep = "notifications/admin/sitrep"
)

// Custom error codes (JSON-RPC reserves -32000..-32099 for application errors).
const (
	CodeNotAllowlisted     = -32001
	CodeBotError           = -32002
	CodeNotSendable        = -32003
	CodeAttachmentTooLarge = -32004
	CodeRequestIDCollision = -32005
	CodeUnauthorized       = -32006
	// CodeMutateRejected is returned by admin.mutate when a mutation is
	// refused before (or instead of) applying: denied tool, unknown tool,
	// rate limit, invalid args, blocked target, or no owner to confirm to.
	CodeMutateRejected = -32007
	CodeInternal       = -32603 // JSON-RPC reserved
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
)

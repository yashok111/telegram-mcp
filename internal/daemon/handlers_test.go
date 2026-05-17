package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/access"
	"github.com/yakov/telegram-mcp/internal/bot"
	"github.com/yakov/telegram-mcp/internal/ipc"
)

type fakeBot struct {
	sentMessage struct {
		chatID, text string
		opts         bot.SendOpts
		retID        int
		retErr       error
	}
	sentFile struct {
		chatID, path string
		opts         bot.SendOpts
		retID        int
		retErr       error
	}
	editedMessage struct {
		chatID    string
		messageID int
		text      string
		parseMode string
		retID     int
		retErr    error
	}
	reactedErr     error
	downloadResult string
	downloadErr    error

	broadcastSeen []string // request_ids
}

func (b *fakeBot) SendMessage(_ context.Context, chatID, text string, opts bot.SendOpts) (int, error) {
	b.sentMessage.chatID = chatID
	b.sentMessage.text = text
	b.sentMessage.opts = opts

	return b.sentMessage.retID, b.sentMessage.retErr
}

func (b *fakeBot) SendFile(_ context.Context, chatID, path string, opts bot.SendOpts) (int, error) {
	b.sentFile.chatID = chatID
	b.sentFile.path = path
	b.sentFile.opts = opts

	return b.sentFile.retID, b.sentFile.retErr
}

func (b *fakeBot) EditMessage(_ context.Context, chatID string, msgID int, text, pm string) (int, error) {
	b.editedMessage.chatID = chatID
	b.editedMessage.messageID = msgID
	b.editedMessage.text = text
	b.editedMessage.parseMode = pm

	return b.editedMessage.retID, b.editedMessage.retErr
}

func (b *fakeBot) React(_ context.Context, _ string, _ int, _ string) error {
	return b.reactedErr
}

func (b *fakeBot) DownloadFile(_ context.Context, _ string) (string, error) {
	return b.downloadResult, b.downloadErr
}

func (b *fakeBot) BroadcastPermissionRequest(_ context.Context, reqID, _ string) {
	b.broadcastSeen = append(b.broadcastSeen, reqID)
}

func newHandlersFixture(t *testing.T) (*Handlers, *fakeBot, *Router, *access.Store) {
	t.Helper()

	dir := t.TempDir()
	store := access.NewStore(dir, false)

	st := access.State{
		DMPolicy:  access.PolicyAllowlist,
		AllowFrom: []string{"123"},
		Groups:    map[string]access.GroupPolicy{"-100200": {}},
		Pending:   map[string]access.Pending{},
	}
	require.NoError(t, store.Save(st))

	fb := &fakeBot{}
	r := NewRouter()
	r.Register(&Shim{ID: "shim-a", Notify: func(string, any) error { return nil }})

	h := NewHandlers(store, fb, r)

	return h, fb, r, store
}

func raw(t *testing.T, v any) json.RawMessage {
	t.Helper()

	b, err := json.Marshal(v)
	require.NoError(t, err)

	return b
}

func conn(id string) *ipc.Conn {
	c := &ipc.Conn{}
	c.Meta.Store(metaShimID, id)

	return c
}

func TestHandleSendMessageAllowed(t *testing.T) {
	h, fb, r, _ := newHandlersFixture(t)

	fb.sentMessage.retID = 42

	res, rpcErr := h.HandleSendMessage(context.Background(), conn("shim-a"), raw(t, map[string]any{
		"chat_id": "123", "text": "hello", "reply_to": 0,
	}))
	require.Nil(t, rpcErr)
	assert.Equal(t, "hello", fb.sentMessage.text)
	assert.Equal(t, map[string]any{"message_id": 42}, res)

	_, ok := r.RouteInbound("123")
	assert.True(t, ok, "RecordOutbound records on success")
}

func TestHandleSendMessageRecordsOwnershipOnSuccess(t *testing.T) {
	h, fb, r, _ := newHandlersFixture(t)
	fb.sentMessage.retID = 1

	_, rpcErr := h.HandleSendMessage(context.Background(), conn("shim-a"), raw(t, map[string]any{
		"chat_id": "123", "text": "h",
	}))
	require.Nil(t, rpcErr)

	owner, ok := r.RouteInbound("123")
	require.True(t, ok)
	assert.Equal(t, "shim-a", owner.ID)
}

func TestHandleSendMessageBlockedByGate(t *testing.T) {
	h, _, _, _ := newHandlersFixture(t)

	_, rpcErr := h.HandleSendMessage(context.Background(), conn("shim-a"), raw(t, map[string]any{
		"chat_id": "999", "text": "x",
	}))
	require.NotNil(t, rpcErr)
	assert.Equal(t, ipc.CodeNotAllowlisted, rpcErr.Code)
}

func TestHandleSendMessageBotErrorIsRPCError(t *testing.T) {
	h, fb, _, _ := newHandlersFixture(t)
	fb.sentMessage.retErr = errors.New("HTTP 400")

	_, rpcErr := h.HandleSendMessage(context.Background(), conn("shim-a"), raw(t, map[string]any{
		"chat_id": "123", "text": "x",
	}))
	require.NotNil(t, rpcErr)
	assert.Equal(t, ipc.CodeBotError, rpcErr.Code)
}

func TestHandleSendFileGatedAndRecorded(t *testing.T) {
	h, fb, r, _ := newHandlersFixture(t)
	fb.sentFile.retID = 7

	res, rpcErr := h.HandleSendFile(context.Background(), conn("shim-a"), raw(t, map[string]any{
		"chat_id": "123", "path": "/tmp/x.png",
	}))
	require.Nil(t, rpcErr)
	assert.Equal(t, map[string]any{"message_id": 7}, res)

	owner, ok := r.RouteInbound("123")
	require.True(t, ok)
	assert.Equal(t, "shim-a", owner.ID)
}

func TestHandleEditMessageGated(t *testing.T) {
	h, fb, _, _ := newHandlersFixture(t)
	fb.editedMessage.retID = 9

	_, rpcErr := h.HandleEditMessage(context.Background(), conn("shim-a"), raw(t, map[string]any{
		"chat_id": "123", "message_id": 5, "text": "edited",
	}))
	require.Nil(t, rpcErr)
	assert.Equal(t, 5, fb.editedMessage.messageID)
}

func TestHandleReactGated(t *testing.T) {
	h, _, _, _ := newHandlersFixture(t)

	_, rpcErr := h.HandleReact(context.Background(), conn("shim-a"), raw(t, map[string]any{
		"chat_id": "999", "message_id": 1, "emoji": "👍",
	}))
	require.NotNil(t, rpcErr)
	assert.Equal(t, ipc.CodeNotAllowlisted, rpcErr.Code)
}

func TestHandleDownloadFileReturnsPath(t *testing.T) {
	h, fb, _, _ := newHandlersFixture(t)
	fb.downloadResult = "/inbox/photo.jpg"

	res, rpcErr := h.HandleDownloadFile(context.Background(), conn("shim-a"), raw(t, map[string]any{
		"file_id": "ABC",
	}))
	require.Nil(t, rpcErr)
	assert.Equal(t, map[string]any{"path": "/inbox/photo.jpg"}, res)
}

func TestHandleBroadcastPermissionRegistersAndForwards(t *testing.T) {
	h, fb, r, _ := newHandlersFixture(t)

	_, rpcErr := h.HandleBroadcastPermission(context.Background(), conn("shim-a"), raw(t, map[string]any{
		"request_id":    "ababc",
		"tool_name":     "Bash",
		"description":   "run a command",
		"input_preview": "ls -la",
	}))
	require.Nil(t, rpcErr)
	assert.Equal(t, []string{"ababc"}, fb.broadcastSeen)

	d, ok := r.LookupPermissionDetails("ababc")
	require.True(t, ok)
	assert.Equal(t, "Bash", d.ToolName)
	assert.Equal(t, "ls -la", d.InputPreview)
}

func TestHandleBroadcastPermissionCollisionRejected(t *testing.T) {
	h, fb, _, _ := newHandlersFixture(t)

	r := h.router
	r.Register(&Shim{ID: "shim-b", Notify: func(string, any) error { return nil }})

	require.NoError(t, r.RegisterPermission("dupe", "shim-b", PermDetails{}))

	_, rpcErr := h.HandleBroadcastPermission(context.Background(), conn("shim-a"), raw(t, map[string]any{
		"request_id": "dupe", "tool_name": "Bash",
	}))
	require.NotNil(t, rpcErr)
	assert.Equal(t, ipc.CodeRequestIDCollision, rpcErr.Code)
	assert.Empty(t, fb.broadcastSeen, "must not fan to DMs when request_id already in use")
}

func TestHandleHelloAssignsShimIDAndAlias(t *testing.T) {
	h, _, _, _ := newHandlersFixture(t)
	c := &ipc.Conn{}

	res, rpcErr := h.HandleHello(context.Background(), c, raw(t, map[string]any{
		"shim_pid": 4242,
		"label":    "session-A",
	}))
	require.Nil(t, rpcErr)

	m, ok := res.(map[string]any)
	require.True(t, ok)
	assert.NotEmpty(t, m["shim_id"])
	assert.NotEmpty(t, m["daemon_version"])
	assert.Empty(t, m["alias"], "HandleHello alone does not register; alias set by daemon Router.Register")
}

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	sentCalls []struct{ chatID, text string } // every SendMessage in order (sentMessage keeps only the last)

	reactedErr     error
	downloadResult string
	downloadErr    error

	broadcastSeen    []string               // request_ids
	broadcastPrefix  []string               // prefix per broadcast
	broadcastTargets []bot.PermissionTarget // resolved target per call

	chatActions []struct{ chatID, action string }

	mutationConfirms []struct {
		target             bot.PermissionTarget
		pendingID, summary string
	}
	confirmRetID  int
	confirmRetErr error
}

func (b *fakeBot) SendMessage(_ context.Context, chatID, text string, opts bot.SendOpts) (int, error) {
	b.sentMessage.chatID = chatID
	b.sentMessage.text = text
	b.sentMessage.opts = opts
	b.sentCalls = append(b.sentCalls, struct{ chatID, text string }{chatID, text})

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

func (b *fakeBot) SendChatAction(_ context.Context, chatID, action string) error {
	b.chatActions = append(b.chatActions, struct{ chatID, action string }{chatID, action})
	return nil
}

func (b *fakeBot) DownloadFile(_ context.Context, _ string) (string, error) {
	return b.downloadResult, b.downloadErr
}

func (b *fakeBot) SendPermissionPrompt(_ context.Context, target bot.PermissionTarget, prefix, reqID, _ string) {
	b.broadcastSeen = append(b.broadcastSeen, reqID)
	b.broadcastPrefix = append(b.broadcastPrefix, prefix)
	b.broadcastTargets = append(b.broadcastTargets, target)
}

func (b *fakeBot) BroadcastMutationConfirm(_ context.Context, target bot.PermissionTarget, pendingID, summary string) (int, error) {
	b.mutationConfirms = append(b.mutationConfirms, struct {
		target             bot.PermissionTarget
		pendingID, summary string
	}{target, pendingID, summary})

	return b.confirmRetID, b.confirmRetErr
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

	h := NewHandlers(store, fb, r, nil)

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
	assert.Equal(t, "@s1: hello", fb.sentMessage.text, "shim-a → alias s1 prefix prepended")
	assert.Equal(t, map[string]any{"message_id": 42}, res)

	_, ok := r.RouteInbound("123")
	assert.True(t, ok, "RecordOutbound records on success")
}

func TestHandleSendMessage_prefixDisabledByEnv(t *testing.T) {
	t.Setenv("TELEGRAM_PREFIX_ALIAS", "0")

	h, fb, _, _ := newHandlersFixture(t)
	fb.sentMessage.retID = 1

	_, rpcErr := h.HandleSendMessage(context.Background(), conn("shim-a"), raw(t, map[string]any{
		"chat_id": "123", "text": "hello",
	}))
	require.Nil(t, rpcErr)
	assert.Equal(t, "hello", fb.sentMessage.text, "env opt-out drops prefix")
}

func TestHandleSendMessage_prefixSkippedForUnknownShim(t *testing.T) {
	h, fb, _, _ := newHandlersFixture(t)
	fb.sentMessage.retID = 1

	_, rpcErr := h.HandleSendMessage(context.Background(), conn("ghost"), raw(t, map[string]any{
		"chat_id": "123", "text": "hello",
	}))
	require.Nil(t, rpcErr)
	assert.Equal(t, "hello", fb.sentMessage.text, "unknown shim → no alias → no prefix")
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

func TestHandleSendMessage_propagatesMessageThreadID(t *testing.T) {
	h, fb, _, _ := newHandlersFixture(t)
	fb.sentMessage.retID = 1

	_, rpcErr := h.HandleSendMessage(context.Background(), conn("shim-a"), raw(t, map[string]any{
		"chat_id": "123", "text": "hi", "message_thread_id": 42,
	}))
	require.Nil(t, rpcErr)
	assert.Equal(t, 42, fb.sentMessage.opts.MessageThreadID)
}

func TestHandleSendFile_propagatesMessageThreadID(t *testing.T) {
	h, fb, _, _ := newHandlersFixture(t)
	fb.sentFile.retID = 1

	_, rpcErr := h.HandleSendFile(context.Background(), conn("shim-a"), raw(t, map[string]any{
		"chat_id": "123", "path": "/tmp/x.png", "message_thread_id": 17,
	}))
	require.Nil(t, rpcErr)
	assert.Equal(t, 17, fb.sentFile.opts.MessageThreadID)
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
	assert.Equal(t, "@s1", fb.sentFile.opts.Caption, "shim-a → alias s1 caption")

	owner, ok := r.RouteInbound("123")
	require.True(t, ok)
	assert.Equal(t, "shim-a", owner.ID)
}

func TestHandleEditMessage_ownedSucceeds(t *testing.T) {
	h, fb, r, _ := newHandlersFixture(t)
	fb.editedMessage.retID = 9

	// Edit requires prior ownership of the (chat, message_id) pair.
	r.RecordOutbound("shim-a", "123", 5)

	_, rpcErr := h.HandleEditMessage(context.Background(), conn("shim-a"), raw(t, map[string]any{
		"chat_id": "123", "message_id": 5, "text": "edited",
	}))
	require.Nil(t, rpcErr)
	assert.Equal(t, 5, fb.editedMessage.messageID)
	assert.Equal(t, "@s1: edited", fb.editedMessage.text, "edit also gets prefix")
}

func TestHandleEditMessage_blockedByGate(t *testing.T) {
	h, fb, r, _ := newHandlersFixture(t)
	// Even pre-existing ownership must not bypass the chat allowlist gate.
	r.RecordOutbound("shim-a", "999", 5)

	_, rpcErr := h.HandleEditMessage(context.Background(), conn("shim-a"), raw(t, map[string]any{
		"chat_id": "999", "message_id": 5, "text": "x",
	}))
	require.NotNil(t, rpcErr)
	assert.Equal(t, ipc.CodeNotAllowlisted, rpcErr.Code)
	assert.Zero(t, fb.editedMessage.messageID, "gate-blocked edit must not reach bot")
}

func TestHandleEditMessage_deniesCrossShimEdit(t *testing.T) {
	h, fb, r, _ := newHandlersFixture(t)
	r.Register(&Shim{ID: "shim-b", Notify: func(string, any) error { return nil }})
	r.RecordOutbound("shim-b", "123", 5)

	_, rpcErr := h.HandleEditMessage(context.Background(), conn("shim-a"), raw(t, map[string]any{
		"chat_id": "123", "message_id": 5, "text": "tampered",
	}))
	require.NotNil(t, rpcErr)
	assert.Equal(t, ipc.CodeNotAllowlisted, rpcErr.Code)
	assert.Zero(t, fb.editedMessage.messageID, "denied edit must not reach bot")
}

func TestHandleEditMessage_deniesUnknownMessage(t *testing.T) {
	h, fb, _, _ := newHandlersFixture(t)

	_, rpcErr := h.HandleEditMessage(context.Background(), conn("shim-a"), raw(t, map[string]any{
		"chat_id": "123", "message_id": 999, "text": "edited",
	}))
	require.NotNil(t, rpcErr)
	assert.Equal(t, ipc.CodeNotAllowlisted, rpcErr.Code)
	assert.Zero(t, fb.editedMessage.messageID, "edit of an unknown message_id must not reach bot")
}

func TestHandleReactGated(t *testing.T) {
	h, _, _, _ := newHandlersFixture(t)

	_, rpcErr := h.HandleReact(context.Background(), conn("shim-a"), raw(t, map[string]any{
		"chat_id": "999", "message_id": 1, "emoji": "👍",
	}))
	require.NotNil(t, rpcErr)
	assert.Equal(t, ipc.CodeNotAllowlisted, rpcErr.Code)
}

func TestHandleReactBotErrorIsRPCError(t *testing.T) {
	h, fb, _, _ := newHandlersFixture(t)
	fb.reactedErr = errors.New("HTTP 400")

	_, rpcErr := h.HandleReact(context.Background(), conn("shim-a"), raw(t, map[string]any{
		"chat_id": "123", "message_id": 7, "emoji": "👍",
	}))
	require.NotNil(t, rpcErr)
	assert.Equal(t, ipc.CodeBotError, rpcErr.Code)
}

func TestHandleReactInvalidParams(t *testing.T) {
	h, _, _, _ := newHandlersFixture(t)

	_, rpcErr := h.HandleReact(context.Background(), conn("shim-a"), json.RawMessage("not json"))
	require.NotNil(t, rpcErr)
	assert.Equal(t, ipc.CodeInvalidParams, rpcErr.Code)
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
	assert.Equal(t, []string{"@s1: "}, fb.broadcastPrefix, "broadcast carries shim alias prefix")

	d, ok := r.LookupPermissionDetails("ababc")
	require.True(t, ok)
	assert.Equal(t, "Bash", d.ToolName)
	assert.Equal(t, "ls -la", d.InputPreview)
}

func TestHandleBroadcastPermission_pickTargetAllowFromHead(t *testing.T) {
	h, fb, _, _ := newHandlersFixture(t)

	_, rpcErr := h.HandleBroadcastPermission(context.Background(), conn("shim-a"), raw(t, map[string]any{
		"request_id": "abcde", "tool_name": "Bash",
	}))
	require.Nil(t, rpcErr)
	require.Len(t, fb.broadcastTargets, 1)
	assert.EqualValues(t, 123, fb.broadcastTargets[0].ChatID, "target picks parsed AllowFrom[0]")
	assert.Zero(t, fb.broadcastTargets[0].ThreadID, "no forum routing in this wave")
}

func TestPickPermissionTarget_forumModePrefersTopic(t *testing.T) {
	dir := t.TempDir()
	store := access.NewStore(dir, false)
	require.NoError(t, store.Save(access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"123"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
		ForumChatID: -100777,
	}))

	fb := &fakeBot{}
	r := NewRouter()
	r.Register(&Shim{ID: "shim-a", Notify: func(string, any) error { return nil }})
	r.BindTopic("shim-a", 42)

	h := NewHandlers(store, fb, r, nil)
	_, rpcErr := h.HandleBroadcastPermission(context.Background(), conn("shim-a"), raw(t, map[string]any{
		"request_id": "abcde", "tool_name": "Bash",
	}))
	require.Nil(t, rpcErr)
	require.Len(t, fb.broadcastTargets, 1)

	tgt := fb.broadcastTargets[0]
	assert.EqualValues(t, -100777, tgt.ChatID, "forum chat picked over AllowFrom")
	assert.Equal(t, 42, tgt.ThreadID)
}

func TestPickPermissionTarget_forumModeFallsBackToDM_whenShimHasNoTopic(t *testing.T) {
	dir := t.TempDir()
	store := access.NewStore(dir, false)
	require.NoError(t, store.Save(access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"123"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
		ForumChatID: -100777,
	}))

	fb := &fakeBot{}
	r := NewRouter()
	// No BindTopic call — shim is in forum mode but never got a topic.
	r.Register(&Shim{ID: "shim-a", Notify: func(string, any) error { return nil }})

	h := NewHandlers(store, fb, r, nil)
	_, rpcErr := h.HandleBroadcastPermission(context.Background(), conn("shim-a"), raw(t, map[string]any{
		"request_id": "abcde", "tool_name": "Bash",
	}))
	require.Nil(t, rpcErr)
	require.Len(t, fb.broadcastTargets, 1)
	assert.EqualValues(t, 123, fb.broadcastTargets[0].ChatID, "no topic → fall back to AllowFrom DM")
	assert.Zero(t, fb.broadcastTargets[0].ThreadID)
}

func TestHandleBroadcastPermission_emptyAllowlistYieldsZeroTarget(t *testing.T) {
	dir := t.TempDir()
	store := access.NewStore(dir, false)
	require.NoError(t, store.Save(access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{},
		Groups: map[string]access.GroupPolicy{"-100200": {}}, Pending: map[string]access.Pending{},
	}))

	fb := &fakeBot{}
	r := NewRouter()
	r.Register(&Shim{ID: "shim-x", Notify: func(string, any) error { return nil }})

	h := NewHandlers(store, fb, r, nil)
	_, rpcErr := h.HandleBroadcastPermission(context.Background(), conn("shim-x"), raw(t, map[string]any{
		"request_id": "xyzab", "tool_name": "Bash",
	}))
	require.Nil(t, rpcErr, "registration succeeds even with empty allowlist")
	require.Len(t, fb.broadcastTargets, 1)
	assert.Zero(t, fb.broadcastTargets[0].ChatID, "empty allowlist → zero target; bot side no-ops")
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

func TestHandlePeersReturnsSnapshotWithSelf(t *testing.T) {
	h, _, r, _ := newHandlersFixture(t)

	r.Register(&Shim{
		ID:          "shim-b-1234abcd",
		Notify:      func(string, any) error { return nil },
		ConnectedAt: time.Now().Add(-1 * time.Hour),
	})

	res, rpcErr := h.HandlePeers(context.Background(), conn("shim-a"), nil)
	require.Nil(t, rpcErr)

	m, ok := res.(map[string]any)
	require.True(t, ok)

	peers, ok := m["peers"].([]PeerInfo)
	require.True(t, ok)
	require.Len(t, peers, 2)

	byID := map[string]PeerInfo{}
	for _, p := range peers {
		byID[p.ShimIDPrefix] = p
	}

	pa, hasA := byID["shim-a"]
	require.True(t, hasA, "shim-a (<=8 chars) uses full id as prefix")
	assert.True(t, pa.Self, "shim-a is the caller — self must be true")

	pb, hasB := byID["shim-b-1"]
	require.True(t, hasB, "shim-b-1234abcd prefix is first 8 chars 'shim-b-1'")
	assert.False(t, pb.Self)
}

func TestHandlePeersUnknownCallerNoSelf(t *testing.T) {
	h, _, _, _ := newHandlersFixture(t)

	res, rpcErr := h.HandlePeers(context.Background(), conn("ghost"), nil)
	require.Nil(t, rpcErr)

	m, ok := res.(map[string]any)
	require.True(t, ok)

	peers, ok := m["peers"].([]PeerInfo)
	require.True(t, ok)

	for _, p := range peers {
		assert.False(t, p.Self, "no peer should be self when caller is unknown")
	}
}

func TestHandlePeersIdleSecondsFromLastOutbound(t *testing.T) {
	h, _, r, _ := newHandlersFixture(t)

	r.RecordOutbound("shim-a", "999", 0)

	res, rpcErr := h.HandlePeers(context.Background(), conn("shim-a"), nil)
	require.Nil(t, rpcErr)

	m, _ := res.(map[string]any)
	peers, _ := m["peers"].([]PeerInfo)
	require.Len(t, peers, 1)
	assert.LessOrEqual(t, peers[0].IdleSeconds, 1, "just recorded outbound — idle ~0s")
}

func TestHandleHelloRoleAdminRejectedWithoutToken(t *testing.T) {
	h, _, r, _ := newHandlersFixture(t)
	h.SetAdminToken("secret-xyz")

	c := &ipc.Conn{}

	res, rpcErr := h.HandleHello(context.Background(), c, raw(t, map[string]any{
		"shim_pid": 4242,
		"role":     "admin",
	}))
	require.Nil(t, rpcErr)

	m, _ := res.(map[string]any)
	id, _ := m["shim_id"].(string)
	require.NotEmpty(t, id)

	storedRole, _ := c.Meta.Load(metaRole)
	assert.Empty(t, storedRole, "rogue admin claim must downgrade to user role")

	r.Register(&Shim{ID: id, Notify: func(string, any) error { return nil }})

	_, ok := r.ResolveAlias(AdminAlias)
	assert.False(t, ok, "rogue shim must not bind AdminAlias")
}

func TestHandleHelloRoleAdminRejectedWithWrongToken(t *testing.T) {
	h, _, _, _ := newHandlersFixture(t)
	h.SetAdminToken("secret-xyz")

	c := &ipc.Conn{}

	_, rpcErr := h.HandleHello(context.Background(), c, raw(t, map[string]any{
		"shim_pid":    4242,
		"role":        "admin",
		"admin_token": "wrong",
	}))
	require.Nil(t, rpcErr)

	storedRole, _ := c.Meta.Load(metaRole)
	assert.Empty(t, storedRole, "wrong token must downgrade to user role")
}

func TestHandleHelloRoleAdminAcceptedWithCorrectToken(t *testing.T) {
	h, _, _, _ := newHandlersFixture(t)
	h.SetAdminToken("secret-xyz")

	c := &ipc.Conn{}

	_, rpcErr := h.HandleHello(context.Background(), c, raw(t, map[string]any{
		"shim_pid":    4242,
		"role":        "admin",
		"admin_token": "secret-xyz",
	}))
	require.Nil(t, rpcErr)

	storedRole, _ := c.Meta.Load(metaRole)
	assert.Equal(t, "admin", storedRole, "correct token must keep role=admin")
}

func TestHandleHelloRoleAdminDisabledWhenTokenUnset(t *testing.T) {
	h, _, _, _ := newHandlersFixture(t)

	c := &ipc.Conn{}

	_, rpcErr := h.HandleHello(context.Background(), c, raw(t, map[string]any{
		"shim_pid":    4242,
		"role":        "admin",
		"admin_token": "any-value",
	}))
	require.Nil(t, rpcErr)

	storedRole, _ := c.Meta.Load(metaRole)
	assert.Empty(t, storedRole, "no token configured means no role=admin can be claimed")
}

func TestHandleHelloOpensShimLogBeforeReturn(t *testing.T) {
	h, _, _, _ := newHandlersFixture(t)

	logsDir := t.TempDir()
	sink, err := NewShimLogs(logsDir, 0)
	require.NoError(t, err)
	t.Cleanup(sink.CloseAll)
	h.SetShimLogs(sink)

	c := &ipc.Conn{}

	res, rpcErr := h.HandleHello(context.Background(), c, raw(t, map[string]any{
		"shim_pid": 7777,
		"label":    "early-log",
	}))
	require.Nil(t, rpcErr)

	m, ok := res.(map[string]any)
	require.True(t, ok)

	id, _ := m["shim_id"].(string)
	require.NotEmpty(t, id)

	require.True(t, sink.IsOpen(id), "shim log file must open before HandleHello returns")

	sink.Write(id, []byte(`{"probe":1}`+"\n"))

	raw, err := os.ReadFile(filepath.Join(logsDir, id+".log"))
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"probe":1`, "open handle must be writable")
}

func TestGateDeniedEmitsUnauthorizedDMEvent(t *testing.T) {
	h, _, _, _ := newHandlersFixture(t)
	sink := &recordingSink{}
	h.SetEventSink(sink)

	rpcErr := h.gate("999")
	require.NotNil(t, rpcErr)
	assert.Equal(t, ipc.CodeNotAllowlisted, rpcErr.Code)
	assert.Equal(t, 1, sink.typeCount("unauthorized_dm"))
}

func TestGateAllowedDoesNotEmitEvent(t *testing.T) {
	h, _, _, _ := newHandlersFixture(t)
	sink := &recordingSink{}
	h.SetEventSink(sink)

	rpcErr := h.gate("123")
	assert.Nil(t, rpcErr)
	assert.Equal(t, 0, sink.typeCount("unauthorized_dm"))
}

// TestGoodbyeNotifyStampsMetaFlag — skipped at unit level.
// ipc.Server.notifyHandlers is unexported; Register takes *ipc.Server (not an
// interface), so the registered closure cannot be extracted or intercepted in
// this package's tests without either a live unix socket or an export_test.go
// shim that the task scope doesn't allow. Covered by Wave 3 integration test
// (admin_ipc_test.go) which drives a full daemon round-trip over the socket.

func TestHandleHelloRecordsWorkdirAndSession(t *testing.T) {
	h, _, _, _ := newHandlersFixture(t)
	c := &ipc.Conn{}

	res, rpcErr := h.HandleHello(context.Background(), c, raw(t, map[string]any{
		"shim_pid":      123,
		"label":         "demo",
		"workdir":       "/home/u/code",
		"cc_session_id": "sess-xyz",
	}))
	require.Nil(t, rpcErr)

	m, ok := res.(map[string]any)
	require.True(t, ok)
	assert.NotEmpty(t, m["shim_id"])

	wd, _ := c.Meta.Load(metaWorkdir)
	assert.Equal(t, "/home/u/code", wd)

	sid, _ := c.Meta.Load(metaCCSessionID)
	assert.Equal(t, "sess-xyz", sid)
}

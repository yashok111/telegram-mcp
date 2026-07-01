package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/access"
	"github.com/yakov/telegram-mcp/internal/ipc"
)

func TestHandleAsk_registersAndForwards(t *testing.T) {
	h, fb, r, _ := newHandlersFixture(t)

	_, rpcErr := h.HandleAsk(context.Background(), conn("shim-a"), raw(t, map[string]any{
		"question_id": "abcde",
		"question":    "Deploy now?",
		"options":     []any{"Yes", "No"},
		"chat_id":     "123",
	}))
	require.Nil(t, rpcErr)

	require.Len(t, fb.askSeen, 1)
	assert.Equal(t, "abcde", fb.askSeen[0].qid)
	assert.Equal(t, "Deploy now?", fb.askSeen[0].question)
	assert.Equal(t, []string{"Yes", "No"}, fb.askSeen[0].options)
	assert.Equal(t, "@s1: ", fb.askSeen[0].prefix, "ask carries the shim alias prefix")

	// qid is registered to the asking shim.
	got, ok := r.RouteAndResolveAsk("abcde")
	require.True(t, ok)
	assert.Equal(t, "shim-a", got.ID)
}

func TestHandleAsk_collisionReturnsError(t *testing.T) {
	h, _, r, _ := newHandlersFixture(t)
	require.NoError(t, r.RegisterAsk("abcde", "shim-a"))

	_, rpcErr := h.HandleAsk(context.Background(), conn("shim-a"), raw(t, map[string]any{
		"question_id": "abcde", "question": "Q?", "options": []any{"a", "b"}, "chat_id": "123",
	}))
	require.NotNil(t, rpcErr)
	assert.Equal(t, ipc.CodeRequestIDCollision, rpcErr.Code)
}

func TestHandleAsk_setsHeaderAwaiting(t *testing.T) {
	h, _, r, store := newHandlersFixture(t)
	r.BindTopic("shim-a", 119)

	m := NewHeaderManager(store, &fakeHeaderBot{}, r, testForumChat, time.Minute, time.Minute)
	h.SetHeader(m)

	_, rpcErr := h.HandleAsk(context.Background(), conn("shim-a"), raw(t, map[string]any{
		"question_id": "abcde", "question": "Q?", "options": []any{"a", "b"}, "chat_id": "123",
	}))
	require.Nil(t, rpcErr)

	m.mu.Lock()
	e := m.entries[119]
	m.mu.Unlock()

	require.NotNil(t, e)
	assert.Equal(t, HeaderPermission, e.state, "ask flips header to 🔵 awaiting input")
}

func TestHandleAskCancel_forgetsAndFlipsHeader(t *testing.T) {
	h, _, r, store := newHandlersFixture(t)
	r.BindTopic("shim-a", 119)
	require.NoError(t, r.RegisterAsk("abcde", "shim-a"))

	m := NewHeaderManager(store, &fakeHeaderBot{}, r, testForumChat, time.Minute, time.Minute)
	m.SetState(119, HeaderPermission, "ask")
	h.SetHeader(m)

	_, rpcErr := h.HandleAskCancel(context.Background(), conn("shim-a"), raw(t, map[string]any{
		"question_id": "abcde",
	}))
	require.Nil(t, rpcErr)

	// qid forgotten — a late tap now finds no owner.
	_, ok := r.RouteAndResolveAsk("abcde")
	assert.False(t, ok)

	m.mu.Lock()
	e := m.entries[119]
	m.mu.Unlock()
	require.NotNil(t, e)
	assert.Equal(t, HeaderBusy, e.state, "cancel flips 🔵 awaiting back to 🟡 busy")
}

func TestHandleAskCancel_unknownQidNoHeaderChange(t *testing.T) {
	h, _, _, _ := newHandlersFixture(t)

	_, rpcErr := h.HandleAskCancel(context.Background(), conn("shim-a"), raw(t, map[string]any{
		"question_id": "zzzzz",
	}))
	require.Nil(t, rpcErr, "cancelling an unknown qid is a no-op, not an error")
}

func TestHandleAsk_forumModeTargetsTopic(t *testing.T) {
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
	_, rpcErr := h.HandleAsk(context.Background(), conn("shim-a"), raw(t, map[string]any{
		"question_id": "abcde", "question": "Q?", "options": []any{"a", "b"}, "chat_id": "123",
	}))
	require.Nil(t, rpcErr)
	require.Len(t, fb.askSeen, 1)
	assert.EqualValues(t, -100777, fb.askSeen[0].target.ChatID)
	assert.Equal(t, 42, fb.askSeen[0].target.ThreadID)
}

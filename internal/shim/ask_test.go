package shim

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/bot"
	"github.com/yakov/telegram-mcp/internal/ipc"
)

func TestBotAdapterBroadcastAsk_forwards(t *testing.T) {
	fc := &fakeClient{returnResult: json.RawMessage(`{}`)}
	a := NewBotAdapter(fc, nil)

	err := a.BroadcastAsk(context.Background(), "abcde", "Deploy?", []string{"Yes", "No"}, "123")
	require.NoError(t, err)
	assert.Equal(t, ipc.MethodBotAsk, fc.calledMethod)
	assert.Contains(t, string(fc.calledParams), `"question_id":"abcde"`)
	assert.Contains(t, string(fc.calledParams), `"question":"Deploy?"`)
	assert.Contains(t, string(fc.calledParams), `"chat_id":"123"`)
	assert.Contains(t, string(fc.calledParams), `"Yes"`)
}

func TestBotAdapterBroadcastAsk_collisionMapsSentinel(t *testing.T) {
	fc := &fakeClient{returnErr: &ipc.Error{Code: ipc.CodeRequestIDCollision, Message: "dupe"}}
	a := NewBotAdapter(fc, nil)

	err := a.BroadcastAsk(context.Background(), "abcde", "Q?", []string{"a", "b"}, "123")
	require.Error(t, err)
	assert.ErrorIs(t, err, bot.ErrAskIDInUse, "collision must map to the bot sentinel for mcp regenerate")
}

func TestBotAdapterBroadcastAsk_otherErrorPassesThrough(t *testing.T) {
	fc := &fakeClient{returnErr: &ipc.Error{Code: ipc.CodeInternal, Message: "boom"}}
	a := NewBotAdapter(fc, nil)

	err := a.BroadcastAsk(context.Background(), "abcde", "Q?", []string{"a", "b"}, "123")
	require.Error(t, err)
	assert.NotErrorIs(t, err, bot.ErrAskIDInUse)
}

func TestBotAdapterCancelAsk_forwards(t *testing.T) {
	fc := &fakeClient{returnResult: json.RawMessage(`{}`)}
	a := NewBotAdapter(fc, nil)

	require.NoError(t, a.CancelAsk(context.Background(), "abcde"))
	assert.Equal(t, ipc.MethodBotAskCancel, fc.calledMethod)
	assert.Contains(t, string(fc.calledParams), `"question_id":"abcde"`)
}

func TestAttachAskHandler_registersAndDispatches(t *testing.T) {
	c := newSpyClient()
	mcp := &fakeMCP{}

	w := newNotifierWorker()
	t.Cleanup(w.Stop)
	AttachAskHandler(c, mcp, w)

	h, ok := c.handlers[ipc.NotifyAskAnswered]
	require.True(t, ok, "ask-answered handler must be registered")

	h(context.Background(), json.RawMessage(`{"question_id":"abcde","index":2,"user":"@yak"}`))
	flush(w)

	require.Len(t, mcp.asks, 1)
	assert.Equal(t, "abcde", mcp.asks[0].qid)
	assert.Equal(t, 2, mcp.asks[0].idx)
	assert.Equal(t, "@yak", mcp.asks[0].user)
}

func TestAttachAskHandler_badJSONNoDispatch(t *testing.T) {
	c := newSpyClient()
	mcp := &fakeMCP{}

	w := newNotifierWorker()
	t.Cleanup(w.Stop)
	AttachAskHandler(c, mcp, w)

	c.handlers[ipc.NotifyAskAnswered](context.Background(), json.RawMessage(`{bad`))
	flush(w)

	assert.Empty(t, mcp.asks)
}

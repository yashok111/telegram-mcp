package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/access"
	"github.com/yakov/telegram-mcp/internal/ipc"
)

func TestNotifierResolveAskRoutesAndRemoves(t *testing.T) {
	r := NewRouter()

	var sink []capturedNotify

	r.Register(newCapturingShim("a", &sink))
	require.NoError(t, r.RegisterAsk("qidaa", "a"))

	n := NewNotifier(r, nil, nil)
	delivered := n.ResolveAsk("qidaa", 1, "@yak")

	assert.True(t, delivered, "answer to a live qid reports delivered")
	require.Len(t, sink, 1)
	assert.Equal(t, ipc.NotifyAskAnswered, sink[0].method)

	_, ok := r.RouteAndResolveAsk("qidaa")
	assert.False(t, ok, "ask must be removed after resolution")
}

func TestNotifierResolveAskUnknownReportsNotDelivered(t *testing.T) {
	r := NewRouter()
	n := NewNotifier(r, nil, nil)

	delivered := n.ResolveAsk("nope", 0, "@x") // must not panic
	assert.False(t, delivered, "unknown qid → not delivered so the bot shows expired")
}

func TestNotifierResolveAsk_setsHeaderBusy(t *testing.T) {
	r := NewRouter()

	var sink []capturedNotify

	r.Register(newCapturingShim("a", &sink))
	r.BindTopic("a", 119)
	require.NoError(t, r.RegisterAsk("qidaa", "a"))

	store := access.NewStore(t.TempDir(), false)
	m := NewHeaderManager(store, &fakeHeaderBot{}, r, testForumChat, time.Minute, time.Minute)
	m.SetState(119, HeaderPermission, "ask")

	n := NewNotifier(r, store, nil)
	n.SetHeader(m)

	n.ResolveAsk("qidaa", 0, "@yak")

	m.mu.Lock()
	e := m.entries[119]
	m.mu.Unlock()

	require.NotNil(t, e)
	assert.Equal(t, HeaderBusy, e.state, "answered ask flips 🔵 back to 🟡 busy")
}

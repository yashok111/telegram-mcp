package daemon

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/bot"
)

type capturedNotify struct {
	method string
	params any
}

func newCapturingShim(id string, sink *[]capturedNotify) *Shim {
	return &Shim{
		ID: id,
		Notify: func(m string, p any) error {
			*sink = append(*sink, capturedNotify{m, p})
			return nil
		},
	}
}

func TestNotifierDeliverInboundUsesChatOwner(t *testing.T) {
	r := NewRouter()

	var aSink, bSink []capturedNotify

	r.Register(newCapturingShim("a", &aSink))
	r.Register(newCapturingShim("b", &bSink))
	r.RecordOutbound("a", "chat-1", 0)

	n := NewNotifier(r)
	n.DeliverInbound("hi", map[string]string{"chat_id": "chat-1", "user": "alice"})

	require.Len(t, aSink, 1)
	assert.Equal(t, "notifications/inbound", aSink[0].method)
	assert.Empty(t, bSink, "non-owner shim must not receive")
}

func TestNotifierDeliverInboundFallsBackToLRU(t *testing.T) {
	r := NewRouter()

	var aSink, bSink []capturedNotify

	r.Register(newCapturingShim("a", &aSink))
	r.Register(newCapturingShim("b", &bSink)) // b is most recent

	n := NewNotifier(r)
	n.DeliverInbound("hi", map[string]string{"chat_id": "unknown"})

	assert.Empty(t, aSink)
	assert.Len(t, bSink, 1)
}

func TestNotifierDeliverInboundDropsWhenNoShim(_ *testing.T) {
	r := NewRouter()
	n := NewNotifier(r)

	// Must not panic.
	n.DeliverInbound("hi", map[string]string{"chat_id": "x"})
}

func TestNotifierLookupPermission(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "a", Notify: func(string, any) error { return nil }})
	require.NoError(t, r.RegisterPermission("xyzab", "a", PermDetails{
		ToolName: "Bash", Description: "run", InputPreview: "ls -la",
	}))

	n := NewNotifier(r)
	d, ok := n.LookupPermission("xyzab")
	require.True(t, ok)
	assert.Equal(t, bot.PermissionDetails{ToolName: "Bash", Description: "run", InputPreview: "ls -la"}, d)
}

func TestNotifierResolvePermissionRoutesAndRemoves(t *testing.T) {
	r := NewRouter()

	var sink []capturedNotify

	r.Register(newCapturingShim("a", &sink))
	require.NoError(t, r.RegisterPermission("xyzab", "a", PermDetails{}))

	n := NewNotifier(r)
	n.ResolvePermission("xyzab", "allow")

	require.Len(t, sink, 1)
	assert.Equal(t, "notifications/permission/resolved", sink[0].method)

	_, ok := r.RoutePermission("xyzab")
	assert.False(t, ok, "permission must be removed after resolution")
}

func TestNotifierResolveUnknownIsNoop(_ *testing.T) {
	r := NewRouter()
	n := NewNotifier(r)
	n.ResolvePermission("nope", "deny") // must not panic
}

func TestDeliverInboundDispatchesToEveryTargetOnBroadcast(t *testing.T) {
	r := NewRouter()
	n := NewNotifier(r)

	var (
		mu        sync.Mutex
		delivered = map[string]int{}
	)

	recordTo := func(id string) func(string, any) error {
		return func(_ string, _ any) error {
			mu.Lock()
			delivered[id]++
			mu.Unlock()

			return nil
		}
	}

	r.Register(&Shim{ID: "a", Notify: recordTo("a")})
	r.Register(&Shim{ID: "b", Notify: recordTo("b")})
	r.Register(&Shim{ID: "c", Notify: recordTo("c")})

	n.DeliverInbound("@all hello", map[string]string{"chat_id": "chat-1"})

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, 1, delivered["a"])
	assert.Equal(t, 1, delivered["b"])
	assert.Equal(t, 1, delivered["c"])
}

func TestDeliverInboundRoutesByReplyOverOwner(t *testing.T) {
	r := NewRouter()
	n := NewNotifier(r)

	var aSink, bSink []capturedNotify

	r.Register(newCapturingShim("a", &aSink))
	r.Register(newCapturingShim("b", &bSink))

	r.RecordOutbound("a", "chat-1", 42) // a sent msg 42
	r.RecordOutbound("b", "chat-1", 99) // b owns chat-1 (last-writer-wins)

	n.DeliverInbound("hi", map[string]string{
		"chat_id":             "chat-1",
		"reply_to_message_id": "42",
	})

	require.Len(t, aSink, 1, "reply must route to a (original sender)")
	assert.Empty(t, bSink, "owner b must not receive when reply targets a")
}

func TestDeliverInboundReplyMissFallsThroughToOwner(t *testing.T) {
	r := NewRouter()
	n := NewNotifier(r)

	var aSink, bSink []capturedNotify

	r.Register(newCapturingShim("a", &aSink))
	r.Register(newCapturingShim("b", &bSink))

	r.RecordOutbound("a", "chat-1", 0) // a owns chat-1; no reply index

	n.DeliverInbound("hi", map[string]string{
		"chat_id":             "chat-1",
		"reply_to_message_id": "999",
	})

	require.Len(t, aSink, 1, "unknown reply_to falls through to owner")
	assert.Empty(t, bSink)
}

func TestDeliverInboundMalformedReplyHeaderIsIgnored(t *testing.T) {
	r := NewRouter()
	n := NewNotifier(r)

	var aSink []capturedNotify

	r.Register(newCapturingShim("a", &aSink))
	r.RecordOutbound("a", "chat-1", 0)

	n.DeliverInbound("hi", map[string]string{
		"chat_id":             "chat-1",
		"reply_to_message_id": "not-a-number",
	})

	require.Len(t, aSink, 1, "garbage reply_to_message_id is silently ignored")
}

// TestDeliverInboundParallelDispatchDoesNotSerializeOnRouterLock proves that
// the Router's mu is released before per-target Notify runs: two concurrent
// DeliverInbound calls for distinct chats both reach the blocked Notify stage
// before either returns. If a future refactor mistakenly holds r.mu across
// the fan-out loop, the second call would block on the lock and this test
// would time out.
func TestDeliverInboundParallelDispatchDoesNotSerializeOnRouterLock(t *testing.T) {
	r := NewRouter()
	n := NewNotifier(r)

	startedA := make(chan struct{}, 1)
	startedB := make(chan struct{}, 1)
	release := make(chan struct{})

	blockingNotify := func(started chan<- struct{}) func(string, any) error {
		return func(_ string, _ any) error {
			started <- struct{}{}
			<-release

			return nil
		}
	}

	r.Register(&Shim{ID: "a", Notify: blockingNotify(startedA)})
	r.Register(&Shim{ID: "b", Notify: blockingNotify(startedB)})

	// Two separate chats, each pinned to a different shim — RouteInboundMulti
	// resolves each call to exactly one target.
	r.RecordOutbound("a", "chat-a", 0)
	r.RecordOutbound("b", "chat-b", 0)

	go n.DeliverInbound("hi", map[string]string{"chat_id": "chat-a"})
	go n.DeliverInbound("hi", map[string]string{"chat_id": "chat-b"})

	awaitBoth := func() {
		deadline := time.After(2 * time.Second)
		got := 0

		for got < 2 {
			select {
			case <-startedA:
				got++
			case <-startedB:
				got++
			case <-deadline:
				t.Fatalf("only %d of 2 concurrent dispatches reached Notify — router lock held across fan-out?", got)
			}
		}
	}

	awaitBoth()
	close(release)
}

func TestDeliverInboundDispatchesToMentionTargetOnly(t *testing.T) {
	r := NewRouter()
	n := NewNotifier(r)

	var (
		mu        sync.Mutex
		delivered = map[string]int{}
	)

	recordTo := func(id string) func(string, any) error {
		return func(_ string, _ any) error {
			mu.Lock()
			delivered[id]++
			mu.Unlock()

			return nil
		}
	}

	r.Register(&Shim{ID: "a", Notify: recordTo("a")}) // s1
	r.Register(&Shim{ID: "b", Notify: recordTo("b")}) // s2

	n.DeliverInbound("@s2 ping", map[string]string{"chat_id": "chat-1"})

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, 0, delivered["a"])
	assert.Equal(t, 1, delivered["b"])
}

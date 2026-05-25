package daemon

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/access"
	"github.com/yakov/telegram-mcp/internal/bot"
)

// fakeSpawner records SpawnRequests and signals each call on fired so tests can
// wait for the off-goroutine Spawn deterministically.
type fakeSpawner struct {
	mu    sync.Mutex
	reqs  []bot.SpawnRequest
	fired chan struct{}
}

func newFakeSpawner() *fakeSpawner { return &fakeSpawner{fired: make(chan struct{}, 16)} }

func (f *fakeSpawner) Spawn(_ context.Context, req bot.SpawnRequest) (string, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()

	f.fired <- struct{}{}

	return "spawn-test", nil
}

func (f *fakeSpawner) calls() []bot.SpawnRequest {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.reqs)
}

// waitFired blocks until n Spawn calls land or the test times out.
func (f *fakeSpawner) waitFired(t *testing.T, n int) {
	t.Helper()

	for range n {
		select {
		case <-f.fired:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for spawn (got %d of %d)", len(f.calls()), n)
		}
	}
}

func forumTestStore(t *testing.T, forumID int64, topics map[string]access.TopicMeta, effort map[string]string) *access.Store {
	t.Helper()

	st := access.NewStore(t.TempDir(), false)
	require.NoError(t, st.Mutate(func(s *access.State) bool {
		s.ForumChatID = forumID
		s.TopicsByThread = topics
		s.EffortByChat = effort

		return true
	}))

	return st
}

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

	n := NewNotifier(r, nil, nil)
	n.DeliverInbound("hi", map[string]string{"chat_id": "chat-1", "user": "alice"})

	require.Len(t, aSink, 1)
	assert.Equal(t, "notifications/inbound", aSink[0].method)
	assert.Empty(t, bSink, "non-owner shim must not receive")
}

func TestNotifierDeliverInboundFallsBackToLRA(t *testing.T) {
	r := NewRouter()

	var aSink, bSink []capturedNotify

	r.Register(newCapturingShim("a", &aSink))
	r.Register(newCapturingShim("b", &bSink))

	n := NewNotifier(r, nil, nil)
	n.DeliverInbound("hi", map[string]string{"chat_id": "unknown"})

	assert.Len(t, aSink, 1, "no pin/owner, both at zero — LRA lex tie-break picks a")
	assert.Empty(t, bSink)
}

func TestNotifierDeliverInboundDropsWhenNoShim(_ *testing.T) {
	r := NewRouter()
	n := NewNotifier(r, nil, nil)

	// Must not panic.
	n.DeliverInbound("hi", map[string]string{"chat_id": "x"})
}

func TestNotifierLookupPermission(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "a", Notify: func(string, any) error { return nil }})
	require.NoError(t, r.RegisterPermission("xyzab", "a", PermDetails{
		ToolName: "Bash", Description: "run", InputPreview: "ls -la",
	}))

	n := NewNotifier(r, nil, nil)
	d, ok := n.LookupPermission("xyzab")
	require.True(t, ok)
	assert.Equal(t, bot.PermissionDetails{ToolName: "Bash", Description: "run", InputPreview: "ls -la"}, d)
}

func TestNotifierResolvePermissionRoutesAndRemoves(t *testing.T) {
	r := NewRouter()

	var sink []capturedNotify

	r.Register(newCapturingShim("a", &sink))
	require.NoError(t, r.RegisterPermission("xyzab", "a", PermDetails{}))

	n := NewNotifier(r, nil, nil)
	n.ResolvePermission("xyzab", "allow")

	require.Len(t, sink, 1)
	assert.Equal(t, "notifications/permission/resolved", sink[0].method)

	_, ok := r.RoutePermission("xyzab")
	assert.False(t, ok, "permission must be removed after resolution")
}

func TestNotifierResolveUnknownIsNoop(_ *testing.T) {
	r := NewRouter()
	n := NewNotifier(r, nil, nil)
	n.ResolvePermission("nope", "deny") // must not panic
}

func TestDeliverInboundDispatchesToEveryTargetOnBroadcast(t *testing.T) {
	r := NewRouter()
	n := NewNotifier(r, nil, nil)

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
	n := NewNotifier(r, nil, nil)

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
	n := NewNotifier(r, nil, nil)

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

func TestDeliverInboundForwardsReplyQuoteMetaUntouched(t *testing.T) {
	r := NewRouter()
	n := NewNotifier(r, nil, nil)

	var aSink []capturedNotify

	r.Register(newCapturingShim("a", &aSink))
	r.RecordOutbound("a", "chat-1", 0)

	in := map[string]string{
		"chat_id":             "chat-1",
		"reply_to_message_id": "42",
		"reply_to_text":       "original message body",
		"reply_to_from":       "@alice",
		"reply_to_quote":      "highlighted slice",
	}
	n.DeliverInbound("hi", in)

	require.Len(t, aSink, 1)
	p, ok := aSink[0].params.(map[string]any)
	require.True(t, ok, "params is map[string]any")
	meta, ok := p["meta"].(map[string]string)
	require.True(t, ok, "meta survives as map[string]string")
	assert.Equal(t, "42", meta["reply_to_message_id"])
	assert.Equal(t, "original message body", meta["reply_to_text"])
	assert.Equal(t, "@alice", meta["reply_to_from"])
	assert.Equal(t, "highlighted slice", meta["reply_to_quote"])
}

func TestDeliverInboundMalformedReplyHeaderIsIgnored(t *testing.T) {
	r := NewRouter()
	n := NewNotifier(r, nil, nil)

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
	n := NewNotifier(r, nil, nil)

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
	n := NewNotifier(r, nil, nil)

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

func TestNotifier_autoSpawn_firesOnEmptyForumTopic(t *testing.T) {
	r := NewRouter()
	r.SetForumChatID(forumChatIDInt())

	store := forumTestStore(t, forumChatIDInt(),
		map[string]access.TopicMeta{"7": {ThreadID: 7, Workdir: "/home/yakov/projects/telegram-mcp"}}, nil)

	sp := newFakeSpawner()
	n := NewNotifier(r, store, nil)
	n.SetAutoSpawn(sp, time.Minute)

	// No shim owns topic 7 → RouteInboundMulti returns empty → auto-spawn.
	n.DeliverInbound("how do I run tests?", map[string]string{
		"chat_id": forumChat, "message_thread_id": "7", "user_id": "42", "user": "yakov",
	})

	sp.waitFired(t, 1)

	calls := sp.calls()
	require.Len(t, calls, 1)
	assert.Equal(t, 7, calls[0].ThreadID, "spawn pinned to the topic")
	assert.Equal(t, forumChat, calls[0].ChatID)
	assert.Equal(t, "42", calls[0].UserID)
	assert.Equal(t, "/home/yakov/projects/telegram-mcp", calls[0].Workdir, "workdir resolved from topic meta")
}

func TestNotifier_autoSpawn_dedupWithinCooldown(t *testing.T) {
	r := NewRouter()
	r.SetForumChatID(forumChatIDInt())

	store := forumTestStore(t, forumChatIDInt(), nil, nil)

	sp := newFakeSpawner()
	n := NewNotifier(r, store, nil)
	n.SetAutoSpawn(sp, time.Hour) // long cooldown

	meta := map[string]string{"chat_id": forumChat, "message_thread_id": "7", "user_id": "42"}
	n.DeliverInbound("first", meta)
	n.DeliverInbound("second", meta) // suppressed: dedup decision is synchronous

	sp.waitFired(t, 1)
	assert.Len(t, sp.calls(), 1, "second message within cooldown must not spawn again")
}

func TestNotifier_autoSpawn_appliesEffort(t *testing.T) {
	r := NewRouter()
	r.SetForumChatID(forumChatIDInt())

	store := forumTestStore(t, forumChatIDInt(), nil, map[string]string{forumChat: "high"})

	sp := newFakeSpawner()
	n := NewNotifier(r, store, nil)
	n.SetAutoSpawn(sp, time.Minute)

	n.DeliverInbound("q", map[string]string{"chat_id": forumChat, "message_thread_id": "7", "user_id": "42"})

	sp.waitFired(t, 1)

	cfg, ok := bot.ResolveEffort("high")
	require.True(t, ok)
	assert.Equal(t, cfg.Model, sp.calls()[0].Model, "auto-spawn honors per-chat /effort")
	assert.Equal(t, cfg.ThinkingTokens, sp.calls()[0].ThinkingTokens)
}

func TestNotifier_autoSpawn_skipsGeneralTopic(t *testing.T) {
	r := NewRouter()
	r.SetForumChatID(forumChatIDInt())

	store := forumTestStore(t, forumChatIDInt(), nil, nil)

	sp := newFakeSpawner()
	n := NewNotifier(r, store, nil)
	n.SetAutoSpawn(sp, time.Minute)

	// threadID 0 = General; not a topic → no auto-spawn (and no shims → drop).
	n.DeliverInbound("hi", map[string]string{"chat_id": forumChat, "user_id": "42"})

	assert.Empty(t, sp.calls(), "General topic must not auto-spawn")
}

func TestNotifier_autoSpawn_skipsNonForumChat(t *testing.T) {
	r := NewRouter()
	r.SetForumChatID(forumChatIDInt())

	store := forumTestStore(t, forumChatIDInt(), nil, nil)

	sp := newFakeSpawner()
	n := NewNotifier(r, store, nil)
	n.SetAutoSpawn(sp, time.Minute)

	// A different chat with a thread id (e.g. linked-discussion group) is not
	// the configured forum → no auto-spawn.
	n.DeliverInbound("hi", map[string]string{"chat_id": "-9999", "message_thread_id": "7", "user_id": "42"})

	assert.Empty(t, sp.calls(), "non-forum chat must not auto-spawn")
}

func TestNotifier_autoSpawn_skipsWhenForumOff(t *testing.T) {
	r := NewRouter() // forum off (no SetForumChatID)

	store := forumTestStore(t, 0, nil, nil) // ForumChatID 0

	sp := newFakeSpawner()
	n := NewNotifier(r, store, nil)
	n.SetAutoSpawn(sp, time.Minute)

	n.DeliverInbound("hi", map[string]string{"chat_id": forumChat, "message_thread_id": "7", "user_id": "42"})

	assert.Empty(t, sp.calls(), "forum mode off → no auto-spawn")
}

func TestNotifier_autoSpawn_skipsWhenOwnerExists(t *testing.T) {
	r := NewRouter()
	r.SetForumChatID(forumChatIDInt())

	var aSink []capturedNotify

	r.Register(newCapturingShim("a", &aSink))
	r.BindTopic("a", 7)

	store := forumTestStore(t, forumChatIDInt(), nil, nil)

	sp := newFakeSpawner()
	n := NewNotifier(r, store, nil)
	n.SetAutoSpawn(sp, time.Minute)

	n.DeliverInbound("hi", map[string]string{"chat_id": forumChat, "message_thread_id": "7", "user_id": "42"})

	assert.Empty(t, sp.calls(), "topic already owned → route to owner, no spawn")
	assert.Len(t, aSink, 1, "owner shim receives the inbound")
}

func TestNotifier_autoSpawn_disabledWhenNoSpawner(t *testing.T) {
	r := NewRouter()
	r.SetForumChatID(forumChatIDInt())

	store := forumTestStore(t, forumChatIDInt(), nil, nil)

	n := NewNotifier(r, store, nil) // SetAutoSpawn never called

	// Must not panic; just drops.
	n.DeliverInbound("hi", map[string]string{"chat_id": forumChat, "message_thread_id": "7", "user_id": "42"})
}

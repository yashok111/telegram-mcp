package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/access"
	"github.com/yakov/telegram-mcp/internal/bot"
	"github.com/yakov/telegram-mcp/internal/ipc"
)

type tgFake struct {
	mu        sync.Mutex
	sends     []map[string]any
	nextMsgID int
}

func (t *tgFake) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if len(parts) < 2 {
			http.NotFound(w, r)
			return
		}

		method := parts[1]

		t.mu.Lock()
		defer t.mu.Unlock()

		switch method {
		case "getMe":
			_, _ = w.Write([]byte(`{"ok":true,"result":{"id":1,"is_bot":true,"username":"botty"}}`))
		case "sendMessage":
			body := map[string]any{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			body["method"] = "sendMessage"
			t.sends = append(t.sends, body)

			id := t.nextMsgID
			t.nextMsgID++

			_, _ = fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d,"date":1,"chat":{"id":1,"type":"private"}}}`, id)
		case "getUpdates":
			time.Sleep(50 * time.Millisecond)

			_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		}
	})
}

type integrationFixture struct {
	daemon  *Daemon
	sock    string
	tg      *tgFake
	bot     *bot.Bot
	router  *Router
	cleanup func()
}

func newIntegration(t *testing.T) *integrationFixture {
	t.Helper()

	dir := t.TempDir()
	store := access.NewStore(dir, false)
	require.NoError(t, store.Save(access.State{
		DMPolicy:  access.PolicyAllowlist,
		AllowFrom: []string{"123"},
		Groups:    map[string]access.GroupPolicy{},
		Pending:   map[string]access.Pending{},
	}))

	tg := &tgFake{nextMsgID: 42}
	ts := httptest.NewServer(tg.handler())

	router := NewRouter()
	notifier := NewNotifier(router)

	tgBot, err := bot.NewFromAPIWithRouter("1234567890:AAH00000000000000000000000000000000", store, notifier, ts.URL, nil)
	require.NoError(t, err)

	sock := filepath.Join(dir, "daemon.sock")
	d := &Daemon{
		StateDir:   dir,
		SocketPath: sock,
		PidPath:    filepath.Join(dir, "daemon.pid"),
		Store:      store,
		Bot:        tgBot,
		Router:     router,
	}

	ctx, cancel := context.WithCancel(t.Context())

	daemonDone := make(chan struct{})
	pollDone := make(chan struct{})

	go func() {
		_ = d.Run(ctx)

		close(daemonDone)
	}()
	go func() {
		_ = tgBot.Poll(ctx)

		close(pollDone)
	}()

	require.Eventually(t, func() bool {
		c, err := ipc.Dial(sock)
		if err != nil {
			return false
		}

		_ = c.Close()

		return true
	}, 2*time.Second, 20*time.Millisecond)

	cleanup := func() {
		cancel()
		ts.Close()

		daemonTimer := time.NewTimer(3 * time.Second)
		defer daemonTimer.Stop()

		select {
		case <-daemonDone:
		case <-daemonTimer.C:
			t.Error("daemon did not exit cleanly")
		}

		pollTimer := time.NewTimer(3 * time.Second)
		defer pollTimer.Stop()

		select {
		case <-pollDone:
		case <-pollTimer.C:
			t.Error("poll did not exit cleanly")
		}
	}

	return &integrationFixture{daemon: d, sock: sock, tg: tg, bot: tgBot, router: router, cleanup: cleanup}
}

type helloResp struct {
	ShimID        string `json:"shim_id"`
	DaemonVersion string `json:"daemon_version"`
	Alias         string `json:"alias"`
}

func connectShim(t *testing.T, sock string) (*ipc.Client, string) {
	t.Helper()

	c, err := ipc.Dial(sock)
	require.NoError(t, err)

	var hello helloResp
	require.NoError(t, c.Call(t.Context(), ipc.MethodHello, map[string]any{}, &hello))
	require.NotEmpty(t, hello.Alias, "daemon must return alias in hello response")

	return c, hello.ShimID
}

func TestIntegrationPerChatAffinity(t *testing.T) {
	f := newIntegration(t)
	defer f.cleanup()

	cA, idA := connectShim(t, f.sock)
	defer cA.Close()

	cB, idB := connectShim(t, f.sock)
	defer cB.Close()

	require.NotEqual(t, idA, idB)
	require.NotEmpty(t, idA)
	require.NotEmpty(t, idB)

	var sendRes struct {
		MessageID int `json:"message_id"`
	}
	require.NoError(t, cA.Call(t.Context(), ipc.MethodBotSendMessage, map[string]any{
		"chat_id": "123", "text": "hi",
	}, &sendRes))
	assert.Equal(t, 42, sendRes.MessageID)

	require.Eventually(t, func() bool {
		f.tg.mu.Lock()
		defer f.tg.mu.Unlock()

		return len(f.tg.sends) == 1
	}, time.Second, 20*time.Millisecond)

	owner, ok := f.router.RouteInbound("123")
	require.True(t, ok)
	assert.Equal(t, idA, owner.ID, "chat 123 must route to shim A which last replied")
}

func TestIntegrationHelloReturnsAlias(t *testing.T) {
	f := newIntegration(t)
	defer f.cleanup()

	cA, idA := connectShim(t, f.sock)
	defer cA.Close()

	cB, idB := connectShim(t, f.sock)
	defer cB.Close()

	require.NotEqual(t, idA, idB)

	shim, ok := f.router.ResolveAlias("s1")
	require.True(t, ok)
	assert.NotEmpty(t, shim.Alias)

	shim2, ok := f.router.ResolveAlias("s2")
	require.True(t, ok)
	assert.NotEqual(t, shim.ID, shim2.ID)
}

func TestIntegrationMentionDispatchHitsTarget(t *testing.T) {
	f := newIntegration(t)
	defer f.cleanup()

	cA, idA := connectShim(t, f.sock)
	defer cA.Close()

	cB, idB := connectShim(t, f.sock)
	defer cB.Close()

	var (
		mu   sync.Mutex
		gotA []string
		gotB []string
	)

	cA.OnNotify(ipc.NotifyInbound, func(_ context.Context, params json.RawMessage) {
		var p struct {
			Content string `json:"content"`
		}

		_ = json.Unmarshal(params, &p)

		mu.Lock()

		gotA = append(gotA, p.Content)
		mu.Unlock()
	})
	cB.OnNotify(ipc.NotifyInbound, func(_ context.Context, params json.RawMessage) {
		var p struct {
			Content string `json:"content"`
		}

		_ = json.Unmarshal(params, &p)

		mu.Lock()

		gotB = append(gotB, p.Content)
		mu.Unlock()
	})

	target, ok := f.router.ResolveAlias("s2")
	require.True(t, ok)

	n := NewNotifier(f.router)
	n.DeliverInbound("@s2 hello", map[string]string{"chat_id": "123", "user": "tester"})

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()

		if target.ID == idA {
			return len(gotA) == 1 && len(gotB) == 0
		}

		return len(gotB) == 1 && len(gotA) == 0
	}, time.Second, 20*time.Millisecond, "only @s2 target should receive")

	_ = idB
}

func TestIntegrationAtAllBroadcasts(t *testing.T) {
	f := newIntegration(t)
	defer f.cleanup()

	cA, _ := connectShim(t, f.sock)
	defer cA.Close()

	cB, _ := connectShim(t, f.sock)
	defer cB.Close()

	var (
		mu   sync.Mutex
		gotA int
		gotB int
	)

	cA.OnNotify(ipc.NotifyInbound, func(_ context.Context, _ json.RawMessage) {
		mu.Lock()
		gotA++
		mu.Unlock()
	})
	cB.OnNotify(ipc.NotifyInbound, func(_ context.Context, _ json.RawMessage) {
		mu.Lock()
		gotB++
		mu.Unlock()
	})

	n := NewNotifier(f.router)
	n.DeliverInbound("@all status", map[string]string{"chat_id": "123", "user": "tester"})

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()

		return gotA == 1 && gotB == 1
	}, time.Second, 20*time.Millisecond)
}

func TestIntegrationMentionDoesNotChangeOwnership(t *testing.T) {
	f := newIntegration(t)
	defer f.cleanup()

	cA, idA := connectShim(t, f.sock)
	defer cA.Close()

	cB, _ := connectShim(t, f.sock)
	defer cB.Close()

	var sendRes struct {
		MessageID int `json:"message_id"`
	}
	require.NoError(t, cA.Call(t.Context(), ipc.MethodBotSendMessage, map[string]any{
		"chat_id": "123", "text": "hi",
	}, &sendRes))

	require.Eventually(t, func() bool {
		owner, ok := f.router.RouteInbound("123")
		return ok && owner.ID == idA
	}, time.Second, 20*time.Millisecond, "shim A must own chat 123 after its first outbound")

	n := NewNotifier(f.router)
	n.DeliverInbound("@s2 ping", map[string]string{"chat_id": "123", "user": "tester"})

	owner, ok := f.router.RouteInbound("123")
	require.True(t, ok)
	assert.Equal(t, idA, owner.ID, "mention must not rewrite chatOwners")
}

func TestIntegrationPeersListsAllShimsWithSelf(t *testing.T) {
	f := newIntegration(t)
	defer f.cleanup()

	cA, idA := connectShim(t, f.sock)
	defer cA.Close()

	cB, idB := connectShim(t, f.sock)
	defer cB.Close()

	require.NotEqual(t, idA, idB)

	var res struct {
		Peers []PeerInfo `json:"peers"`
	}
	require.NoError(t, cA.Call(t.Context(), ipc.MethodDaemonPeers, struct{}{}, &res))
	require.Len(t, res.Peers, 2)

	byAlias := map[string]PeerInfo{}
	for _, p := range res.Peers {
		byAlias[p.Alias] = p
	}

	require.Contains(t, byAlias, "s1")
	require.Contains(t, byAlias, "s2")

	var selfCount int

	for _, p := range res.Peers {
		if p.Self {
			selfCount++

			assert.True(t, strings.HasPrefix(idA, p.ShimIDPrefix), "self peer's id_prefix must prefix the calling shim's id")
		}
	}

	assert.Equal(t, 1, selfCount, "exactly one peer marked self when called from A")

	require.NoError(t, cB.Call(t.Context(), ipc.MethodDaemonPeers, struct{}{}, &res))

	var selfB int

	for _, p := range res.Peers {
		if p.Self {
			selfB++

			assert.True(t, strings.HasPrefix(idB, p.ShimIDPrefix), "self peer's id_prefix must prefix idB")
		}
	}

	assert.Equal(t, 1, selfB, "exactly one peer marked self when called from B")
}

func TestIntegrationReplyRoutesToOriginalSender(t *testing.T) {
	f := newIntegration(t)
	defer f.cleanup()

	cA, idA := connectShim(t, f.sock)
	defer cA.Close()

	cB, idB := connectShim(t, f.sock)
	defer cB.Close()

	var (
		mu         sync.Mutex
		gotA, gotB []string
	)

	cA.OnNotify(ipc.NotifyInbound, func(_ context.Context, params json.RawMessage) {
		var p struct {
			Content string `json:"content"`
		}

		_ = json.Unmarshal(params, &p)

		mu.Lock()

		gotA = append(gotA, p.Content)
		mu.Unlock()
	})
	cB.OnNotify(ipc.NotifyInbound, func(_ context.Context, params json.RawMessage) {
		var p struct {
			Content string `json:"content"`
		}

		_ = json.Unmarshal(params, &p)

		mu.Lock()

		gotB = append(gotB, p.Content)
		mu.Unlock()
	})

	var sendA struct {
		MessageID int `json:"message_id"`
	}
	require.NoError(t, cA.Call(t.Context(), ipc.MethodBotSendMessage, map[string]any{
		"chat_id": "123", "text": "from A",
	}, &sendA))
	require.Equal(t, 42, sendA.MessageID)

	var sendB struct {
		MessageID int `json:"message_id"`
	}
	require.NoError(t, cB.Call(t.Context(), ipc.MethodBotSendMessage, map[string]any{
		"chat_id": "123", "text": "from B",
	}, &sendB))
	require.Equal(t, 43, sendB.MessageID)

	require.Eventually(t, func() bool {
		owner, ok := f.router.RouteInbound("123")
		return ok && owner.ID == idB
	}, time.Second, 20*time.Millisecond, "B owns chat 123 after its second outbound (last-writer-wins)")

	n := NewNotifier(f.router)
	n.DeliverInbound("reply to A", map[string]string{
		"chat_id":             "123",
		"user":                "tester",
		"reply_to_message_id": "42",
	})

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()

		return len(gotA) == 1 && len(gotB) == 0
	}, time.Second, 20*time.Millisecond, "reply to A's msg 42 must route to A, not owner B")

	_ = idA
}

func TestIntegrationReplyToUnknownMessageFallsThroughToOwner(t *testing.T) {
	f := newIntegration(t)
	defer f.cleanup()

	cA, _ := connectShim(t, f.sock)
	defer cA.Close()

	cB, idB := connectShim(t, f.sock)
	defer cB.Close()

	var (
		mu         sync.Mutex
		gotA, gotB int
	)

	cA.OnNotify(ipc.NotifyInbound, func(_ context.Context, _ json.RawMessage) {
		mu.Lock()
		gotA++
		mu.Unlock()
	})
	cB.OnNotify(ipc.NotifyInbound, func(_ context.Context, _ json.RawMessage) {
		mu.Lock()
		gotB++
		mu.Unlock()
	})

	var send struct {
		MessageID int `json:"message_id"`
	}
	require.NoError(t, cB.Call(t.Context(), ipc.MethodBotSendMessage, map[string]any{
		"chat_id": "123", "text": "hi",
	}, &send))

	require.Eventually(t, func() bool {
		owner, ok := f.router.RouteInbound("123")
		return ok && owner.ID == idB
	}, time.Second, 20*time.Millisecond, "B owns chat 123")

	n := NewNotifier(f.router)
	n.DeliverInbound("reply text", map[string]string{
		"chat_id":             "123",
		"reply_to_message_id": "9999",
	})

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()

		return gotA == 0 && gotB == 1
	}, time.Second, 20*time.Millisecond, "unknown reply_to falls back to owner B")
}

func TestIntegrationReplyAfterSenderDroppedFallsThrough(t *testing.T) {
	f := newIntegration(t)
	defer f.cleanup()

	cA, _ := connectShim(t, f.sock)

	cB, _ := connectShim(t, f.sock)
	defer cB.Close()

	var (
		mu         sync.Mutex
		gotA, gotB int
	)

	cA.OnNotify(ipc.NotifyInbound, func(_ context.Context, _ json.RawMessage) {
		mu.Lock()
		gotA++
		mu.Unlock()
	})
	cB.OnNotify(ipc.NotifyInbound, func(_ context.Context, _ json.RawMessage) {
		mu.Lock()
		gotB++
		mu.Unlock()
	})

	var sendA struct {
		MessageID int `json:"message_id"`
	}
	require.NoError(t, cA.Call(t.Context(), ipc.MethodBotSendMessage, map[string]any{
		"chat_id": "123", "text": "from A",
	}, &sendA))

	_ = cA.Close()

	require.Eventually(t, func() bool {
		return f.router.ConnectedCount() == 1
	}, time.Second, 20*time.Millisecond, "A drop must propagate to router")

	n := NewNotifier(f.router)
	n.DeliverInbound("reply to dropped A", map[string]string{
		"chat_id":             "123",
		"reply_to_message_id": "42",
	})

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()

		return gotA == 0 && gotB == 1
	}, time.Second, 20*time.Millisecond, "reply to A (dropped) falls through to surviving shim B")
}

func TestIntegrationGateBlocksUnknownChat(t *testing.T) {
	f := newIntegration(t)
	defer f.cleanup()

	c, _ := connectShim(t, f.sock)
	defer c.Close()

	err := c.Call(t.Context(), ipc.MethodBotSendMessage, map[string]any{
		"chat_id": "999", "text": "blocked",
	}, nil)
	require.Error(t, err)

	var rpcErr *ipc.Error
	require.ErrorAs(t, err, &rpcErr)
	assert.Equal(t, ipc.CodeNotAllowlisted, rpcErr.Code)
}

package daemon

import (
	"context"
	"encoding/json"
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
	mu    sync.Mutex
	sends []map[string]any
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
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":42,"date":1,"chat":{"id":1,"type":"private"}}}`))
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

	tg := &tgFake{}
	ts := httptest.NewServer(tg.handler())

	router := NewRouter()
	notifier := NewNotifier(router)

	tgBot, err := bot.NewFromAPI("1234567890:AAH00000000000000000000000000000000", store, notifier, ts.URL)
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
}

func connectShim(t *testing.T, sock string) (*ipc.Client, string) {
	t.Helper()

	c, err := ipc.Dial(sock)
	require.NoError(t, err)

	var hello helloResp
	require.NoError(t, c.Call(t.Context(), ipc.MethodHello, map[string]any{}, &hello))

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

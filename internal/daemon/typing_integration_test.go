package daemon

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/access"
	"github.com/yakov/telegram-mcp/internal/bot"
	"github.com/yakov/telegram-mcp/internal/ipc"
)

// newTypingIntegration builds the same triangle as newIntegration but with a
// real TypingTracker wired to the daemon. RefreshInterval is dialled down to
// 30ms so a test can observe several ticks without blowing past the goleak
// shutdown budget.
func newTypingIntegration(t *testing.T, st access.State) *integrationFixture {
	t.Helper()

	dir := t.TempDir()
	store := access.NewStore(dir, false)
	require.NoError(t, store.Save(st))

	tg := &tgFake{nextMsgID: 42}
	ts := httptest.NewServer(tg.handler())

	router := NewRouter()

	typing := NewTypingTracker(nil, TypingConfig{
		RefreshInterval:  30 * time.Millisecond,
		TTL:              500 * time.Millisecond,
		RotationInterval: 30 * time.Millisecond,
	})

	notifier := NewNotifier(router, store, typing)

	tgBot, err := bot.NewFromAPIWithRouter("1234567890:AAH00000000000000000000000000000000", store, notifier, ts.URL, nil)
	require.NoError(t, err)

	typing.AttachBot(tgBot)

	sock := filepath.Join(dir, "daemon.sock")
	d := &Daemon{
		StateDir:   dir,
		SocketPath: sock,
		PidPath:    filepath.Join(dir, "daemon.pid"),
		Store:      store,
		Bot:        tgBot,
		Router:     router,
		Typing:     typing,
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

func TestIntegrationTypingFiresOnInboundAndClearsOnOutbound(t *testing.T) {
	f := newTypingIntegration(t, access.State{
		DMPolicy:  access.PolicyAllowlist,
		AllowFrom: []string{"123"},
		Groups:    map[string]access.GroupPolicy{},
		Pending:   map[string]access.Pending{},
	})
	defer f.cleanup()

	c, _ := connectShim(t, f.sock)
	defer c.Close()

	n := NewNotifier(f.router, nil, f.daemon.Typing)
	n.DeliverInbound("ping", map[string]string{
		"chat_id":    "123",
		"user":       "tester",
		"message_id": "9",
	})

	require.Eventually(t, func() bool {
		return f.tg.chatActionCount() >= 2
	}, time.Second, 20*time.Millisecond, "ticker must refresh typing while chat is pending")

	var sendRes struct {
		MessageID int `json:"message_id"`
	}
	require.NoError(t, c.Call(t.Context(), ipc.MethodBotSendMessage, map[string]any{
		"chat_id": "123", "text": "ok",
	}, &sendRes))

	require.Eventually(t, func() bool {
		return len(f.daemon.Typing.Pending()) == 0
	}, time.Second, 20*time.Millisecond, "outbound must clear the chat from the pending set")

	// Once Pending is empty, the ticker must stop adding to chatActions. Allow
	// up to one in-flight tick that captured chat 123 before Clear ran — it
	// will still fire its HTTP call after Clear returns. Past that, the count
	// must stay flat for a window of several refresh intervals.
	beforeWindow := f.tg.chatActionCount()
	time.Sleep(300 * time.Millisecond)
	afterWindow := f.tg.chatActionCount()

	assert.LessOrEqual(t, afterWindow-beforeWindow, 1,
		"after Pending cleared, at most one in-flight tick may land; got %d new actions in 300ms (refresh=30ms)",
		afterWindow-beforeWindow)
}

func TestIntegrationTypingTTLEvictsStaleChat(t *testing.T) {
	f := newTypingIntegration(t, access.State{
		DMPolicy:  access.PolicyAllowlist,
		AllowFrom: []string{"123"},
		Groups:    map[string]access.GroupPolicy{},
		Pending:   map[string]access.Pending{},
	})
	defer f.cleanup()

	c, _ := connectShim(t, f.sock)
	defer c.Close()

	n := NewNotifier(f.router, nil, f.daemon.Typing)
	n.DeliverInbound("ping", map[string]string{
		"chat_id":    "123",
		"user":       "tester",
		"message_id": "11",
	})

	require.Eventually(t, func() bool {
		return len(f.daemon.Typing.Pending()) == 0
	}, 2*time.Second, 20*time.Millisecond, "TTL (500ms) must expire the chat without an outbound clear")
}

func TestIntegrationTypingRotatesReactionWhenAckConfigured(t *testing.T) {
	f := newTypingIntegration(t, access.State{
		DMPolicy:    access.PolicyAllowlist,
		AllowFrom:   []string{"123"},
		Groups:      map[string]access.GroupPolicy{},
		Pending:     map[string]access.Pending{},
		AckReaction: "👀",
	})
	defer f.cleanup()

	c, _ := connectShim(t, f.sock)
	defer c.Close()

	n := NewNotifier(f.router, f.daemon.Store, f.daemon.Typing)
	n.DeliverInbound("ping", map[string]string{
		"chat_id":    "123",
		"user":       "tester",
		"message_id": "17",
	})

	require.Eventually(t, func() bool {
		return f.tg.reactionCount() >= 2
	}, time.Second, 20*time.Millisecond, "rotation must fire React on the inbound msg when AckReaction is set")
}

func TestIntegrationTypingDoneStampsCheckOnOutbound(t *testing.T) {
	f := newTypingIntegration(t, access.State{
		DMPolicy:    access.PolicyAllowlist,
		AllowFrom:   []string{"123"},
		Groups:      map[string]access.GroupPolicy{},
		Pending:     map[string]access.Pending{},
		AckReaction: "👀",
	})
	defer f.cleanup()

	c, _ := connectShim(t, f.sock)
	defer c.Close()

	n := NewNotifier(f.router, f.daemon.Store, f.daemon.Typing)
	n.DeliverInbound("ping", map[string]string{
		"chat_id":    "123",
		"user":       "tester",
		"message_id": "21",
	})

	require.Eventually(t, func() bool {
		return len(f.daemon.Typing.Pending()) == 1
	}, time.Second, 20*time.Millisecond, "chat must be marked pending before outbound")

	var sendRes struct {
		MessageID int `json:"message_id"`
	}
	require.NoError(t, c.Call(t.Context(), ipc.MethodBotSendMessage, map[string]any{
		"chat_id": "123", "text": "done",
	}, &sendRes))

	require.Eventually(t, func() bool {
		return slices.Contains(f.tg.reactionEmojis(), defaultDoneEmoji)
	}, time.Second, 20*time.Millisecond, "Done must swap a %s onto the inbound after outbound lands", defaultDoneEmoji)
}

func TestIntegrationTypingSkipsRotationWhenAckUnset(t *testing.T) {
	f := newTypingIntegration(t, access.State{
		DMPolicy:  access.PolicyAllowlist,
		AllowFrom: []string{"123"},
		Groups:    map[string]access.GroupPolicy{},
		Pending:   map[string]access.Pending{},
	})
	defer f.cleanup()

	c, _ := connectShim(t, f.sock)
	defer c.Close()

	n := NewNotifier(f.router, f.daemon.Store, f.daemon.Typing)
	n.DeliverInbound("ping", map[string]string{
		"chat_id":    "123",
		"user":       "tester",
		"message_id": "19",
	})

	require.Eventually(t, func() bool {
		return f.tg.chatActionCount() >= 2
	}, time.Second, 20*time.Millisecond, "typing must still fire even when reactions are off")

	assert.Zero(t, f.tg.reactionCount(), "rotation must stay quiet when access.State.AckReaction is empty")
}

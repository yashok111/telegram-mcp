package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/ipc"
)

// TestIntegrationAskRoundTrip drives the full ask triangle over real IPC: a
// shim asks (as BotAdapter.BroadcastAsk would), the real bot renders the inline
// keyboard to the fake Telegram server, a simulated tap resolves via a Notifier
// sharing the daemon's Router, and the answer is pushed back to the same shim.
func TestIntegrationAskRoundTrip(t *testing.T) {
	f := newIntegration(t)
	defer f.cleanup()

	c, _ := connectShim(t, f.sock)
	defer func() { _ = c.Close() }()

	answered := make(chan map[string]any, 1)

	c.OnNotify(ipc.NotifyAskAnswered, func(_ context.Context, params json.RawMessage) {
		var m map[string]any

		_ = json.Unmarshal(params, &m)
		answered <- m
	})

	require.NoError(t, c.Call(t.Context(), ipc.MethodBotAsk, map[string]any{
		"question_id": "abcde",
		"question":    "Deploy?",
		"options":     []string{"Yes", "No"},
		"chat_id":     "123",
	}, nil))

	// The real bot rendered a per-option inline keyboard to Telegram.
	require.Eventually(t, func() bool {
		return f.tg.sendBodyContains("ask:abcde:0") && f.tg.sendBodyContains("ask:abcde:1")
	}, 2*time.Second, 20*time.Millisecond, "ask keyboard never rendered")

	// Simulate the operator tapping option 1 — same call handleCallback makes.
	n := NewNotifier(f.router, f.daemon.Store, nil)
	n.ResolveAsk("abcde", 1, "@yak")

	select {
	case m := <-answered:
		assert.Equal(t, "abcde", m["question_id"])
		assert.EqualValues(t, 1, m["index"])
		assert.Equal(t, "@yak", m["user"])
	case <-time.After(2 * time.Second):
		t.Fatal("ask answer never routed back to the shim")
	}

	// The qid is consumed — a second tap finds nothing (first-tap-wins).
	_, ok := f.router.RouteAndResolveAsk("abcde")
	assert.False(t, ok)
}

package mcp

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/bot"
)

func TestNewQID_shape(t *testing.T) {
	re := regexp.MustCompile(`^[a-km-z]{5}$`)

	for range 200 {
		qid, err := newQID()
		require.NoError(t, err)
		assert.True(t, re.MatchString(qid), "qid %q outside [a-km-z]{5}", qid)
	}
}

func TestValidateAsk(t *testing.T) {
	srv, _, _ := newServerWithAllowlist(t, "123")

	cases := []struct {
		name     string
		question string
		options  []string
		wantErr  string // substring; "" = valid
	}{
		{"valid", "Pick", []string{"a", "b"}, ""},
		{"empty question", "", []string{"a", "b"}, "must not be empty"},
		{"long question", strings.Repeat("x", askDefaultMaxQuestionLen+1), []string{"a", "b"}, "too long"},
		{"one option", "Pick", []string{"a"}, "at least"},
		{"too many options", "Pick", make([]string, askDefaultMaxOptions+1), "too many options"},
		{"empty option", "Pick", []string{"a", ""}, "is empty"},
		{"long label", "Pick", []string{"a", strings.Repeat("y", askDefaultMaxLabel+1)}, "too long"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// fill non-empty options for the "too many" case so only the count trips
			opts := c.options
			if c.name == "too many options" {
				for i := range opts {
					opts[i] = "o"
				}
			}

			got := srv.validateAsk(c.question, opts)
			if c.wantErr == "" {
				assert.Empty(t, got)
				return
			}

			assert.Contains(t, got, c.wantErr)
		})
	}
}

func TestHandleAsk_gateDenied(t *testing.T) {
	srv, _, _ := newServerWithAllowlist(t, "123")
	res, err := srv.handleAsk(t.Context(), callTool("ask", map[string]any{
		"chat_id":  "999",
		"question": "Q?",
		"options":  []any{"a", "b"},
	}))
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Contains(t, contentText(res), "not allowlisted")
}

func TestHandleAsk_badOptions(t *testing.T) {
	srv, _, _ := newServerWithAllowlist(t, "123")
	res, err := srv.handleAsk(t.Context(), callTool("ask", map[string]any{
		"chat_id":  "123",
		"question": "Q?",
		"options":  []any{"only-one"},
	}))
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Contains(t, contentText(res), "at least")
}

func TestHandleAsk_resolveReturnsChoice(t *testing.T) {
	srv, fb, _ := newServerWithAllowlist(t, "123")

	// Simulate the operator tapping option 1 the moment the prompt is delivered.

	fb.askFn = func(qid, _ string, _ []string, _ string) error {
		go srv.ResolveAsk(qid, 1, "@yak")
		return nil
	}

	res, err := srv.handleAsk(t.Context(), callTool("ask", map[string]any{
		"chat_id":  "123",
		"question": "Deploy?",
		"options":  []any{"Yes", "No"},
	}))
	require.NoError(t, err)
	require.False(t, res.IsError, contentText(res))
	assert.Contains(t, contentText(res), "chose: No (index 1, by @yak)")

	require.Len(t, fb.askCalls, 1)
	assert.Equal(t, "Deploy?", fb.askCalls[0].question)
}

func TestHandleAsk_timeout(t *testing.T) {
	srv, fb, _ := newServerWithAllowlist(t, "123")
	srv.SetAskConfig(20*time.Millisecond, 0, 0)

	res, err := srv.handleAsk(t.Context(), callTool("ask", map[string]any{
		"chat_id":  "123",
		"question": "Q?",
		"options":  []any{"a", "b"},
	}))
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Contains(t, contentText(res), "no answer within")

	// pending entry cleaned up on exit.
	srv.askMu.Lock()
	n := len(srv.pendingAsk)
	srv.askMu.Unlock()
	assert.Zero(t, n)

	// timeout tells the daemon to forget the qid (no leak / stale button).
	require.Len(t, fb.askCalls, 1)
	assert.Equal(t, []string{fb.askCalls[0].qid}, fb.askCancels)
}

func TestHandleAsk_ctxCancel(t *testing.T) {
	srv, fb, _ := newServerWithAllowlist(t, "123")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	res, err := srv.handleAsk(ctx, callTool("ask", map[string]any{
		"chat_id":  "123",
		"question": "Q?",
		"options":  []any{"a", "b"},
	}))
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Contains(t, contentText(res), "cancelled")

	// ctx-cancel also forgets the qid daemon-side (runs on a fresh ctx, so the
	// cancelled caller ctx doesn't abort the cleanup).
	require.Len(t, fb.askCalls, 1)
	assert.Equal(t, []string{fb.askCalls[0].qid}, fb.askCancels)
}

func TestHandleAsk_concurrentCap(t *testing.T) {
	srv, _, _ := newServerWithAllowlist(t, "123")

	// Saturate pendingAsk to the cap directly.
	srv.askMu.Lock()
	for i := range srv.askMaxConc {
		srv.pendingAsk["qid0"+string(rune('a'+i))] = make(chan askAnswer, 1)
	}
	srv.askMu.Unlock()

	res, err := srv.handleAsk(t.Context(), callTool("ask", map[string]any{
		"chat_id":  "123",
		"question": "Q?",
		"options":  []any{"a", "b"},
	}))
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Contains(t, contentText(res), "too many pending questions")
}

func TestCancelAllAsks_unblocksWaiter(t *testing.T) {
	srv, fb, _ := newServerWithAllowlist(t, "123")

	fb.askFn = func(_, _ string, _ []string, _ string) error {
		go srv.CancelAllAsks("daemon unavailable")
		return nil
	}

	res, err := srv.handleAsk(t.Context(), callTool("ask", map[string]any{
		"chat_id":  "123",
		"question": "Q?",
		"options":  []any{"a", "b"},
	}))
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Contains(t, contentText(res), "daemon unavailable")
}

func TestResolveAsk_noPending_noop(t *testing.T) {
	srv, _, _ := newServerWithAllowlist(t, "123")
	// Must not panic and must not register anything.
	srv.ResolveAsk("zzzzz", 0, "@x")

	srv.askMu.Lock()
	n := len(srv.pendingAsk)
	srv.askMu.Unlock()
	assert.Zero(t, n)
}

func TestHandleAsk_broadcastError(t *testing.T) {
	srv, fb, _ := newServerWithAllowlist(t, "123")
	fb.askFn = func(_, _ string, _ []string, _ string) error {
		return assertedErr("ipc down")
	}

	res, err := srv.handleAsk(t.Context(), callTool("ask", map[string]any{
		"chat_id":  "123",
		"question": "Q?",
		"options":  []any{"a", "b"},
	}))
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Contains(t, contentText(res), "could not deliver")

	srv.askMu.Lock()
	n := len(srv.pendingAsk)
	srv.askMu.Unlock()
	assert.Zero(t, n, "failed broadcast must unregister the qid")

	// A non-collision broadcast failure fires a best-effort daemon-side cancel
	// in case the daemon registered the qid before the shim gave up.
	require.Len(t, fb.askCancels, 1)
}

func TestHandleAsk_collisionRegenerates(t *testing.T) {
	srv, fb, _ := newServerWithAllowlist(t, "123")

	var calls int

	fb.askFn = func(qid, _ string, _ []string, _ string) error {
		calls++
		if calls == 1 {
			return bot.ErrAskIDInUse // first qid collides at daemon
		}

		go srv.ResolveAsk(qid, 0, "@yak")

		return nil
	}

	res, err := srv.handleAsk(t.Context(), callTool("ask", map[string]any{
		"chat_id":  "123",
		"question": "Q?",
		"options":  []any{"a", "b"},
	}))
	require.NoError(t, err)
	require.False(t, res.IsError, contentText(res))
	assert.Contains(t, contentText(res), "chose: a")
	assert.GreaterOrEqual(t, calls, 2, "collision should have forced a regenerate")
}

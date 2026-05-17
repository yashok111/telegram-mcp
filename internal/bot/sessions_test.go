package bot

import (
	"strings"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/access"
)

type fakeRouterView struct {
	snap      []ShimInfo
	pinErr    error
	pinned    ShimInfo
	evictErr  error
	evicted   ShimInfo
	lastPin   pinCall
	lastEvict string
}

type pinCall struct {
	chatID, prefix string
	ttl            time.Duration
}

func (f *fakeRouterView) Snapshot() []ShimInfo { return f.snap }

func (f *fakeRouterView) Pin(chat, pref string, ttl time.Duration) (ShimInfo, error) {
	f.lastPin = pinCall{chatID: chat, prefix: pref, ttl: ttl}
	return f.pinned, f.pinErr
}

func (f *fakeRouterView) Evict(pref string) (ShimInfo, error) {
	f.lastEvict = pref
	return f.evicted, f.evictErr
}

func TestBotNoRouterView(t *testing.T) {
	b := &Bot{}
	// Should not panic when calling helpers with nil router view.
	out := b.renderShims(time.Now())
	assert.NotEmpty(t, out, "renderShims returned empty string; expected friendly fallback")
}

func TestRenderShimsEmpty(t *testing.T) {
	b := &Bot{router: &fakeRouterView{}}
	out := b.renderShims(time.Now())
	assert.Contains(t, out, "No active CC sessions")
}

func TestRenderShimsWithSessions(t *testing.T) {
	now := time.Now()
	fv := &fakeRouterView{snap: []ShimInfo{{
		ID:           "abcdef012345",
		IDPrefix:     "abcdef01",
		Alias:        "s1",
		Label:        "main",
		Workdir:      "/code",
		ConnectedAt:  now.Add(-time.Hour),
		LastOutbound: now.Add(-5 * time.Second),
	}}}
	b := &Bot{router: fv}
	out := b.renderShims(now)
	assert.Contains(t, out, "abcdef01")
	assert.Contains(t, out, "main")
	assert.Contains(t, out, "/code")
	assert.Contains(t, out, "busy")
}

func TestRenderShimsIdleState(t *testing.T) {
	now := time.Now()
	fv := &fakeRouterView{snap: []ShimInfo{{
		ID:           "ffff00000000",
		IDPrefix:     "ffff0000",
		Alias:        "s2",
		Label:        "",
		Workdir:      "",
		ConnectedAt:  now.Add(-2 * time.Hour),
		LastOutbound: now.Add(-10 * time.Minute),
	}}}
	b := &Bot{router: fv}
	out := b.renderShims(now)
	assert.Contains(t, out, "idle")
	assert.Contains(t, out, "(no label)")
	assert.Contains(t, out, "?")
}

func TestRenderShimsPinNote(t *testing.T) {
	now := time.Now()
	fv := &fakeRouterView{snap: []ShimInfo{{
		ID:          "aaaa11112222",
		IDPrefix:    "aaaa1111",
		Alias:       "s3",
		Label:       "pinned",
		Workdir:     "/x",
		ConnectedAt: now.Add(-time.Minute),
		PinnedChats: []string{"42"},
	}}}
	b := &Bot{router: fv}
	out := b.renderShims(now)
	assert.Contains(t, out, "📌")
}

func TestHandleUseSuccess(t *testing.T) {
	fv := &fakeRouterView{pinned: ShimInfo{IDPrefix: "abcdef01", Alias: "s1", Label: "main"}}
	b := &Bot{router: fv}

	reply, ok := b.handleUseCommand("123", "/use abcdef")
	require.True(t, ok)
	assert.Contains(t, reply, "Pinned")
	assert.Equal(t, "123", fv.lastPin.chatID)
	assert.Equal(t, "abcdef", fv.lastPin.prefix)
	assert.Equal(t, PinTTL, fv.lastPin.ttl)
}

func TestHandleUseSuccessEmptyLabel(t *testing.T) {
	fv := &fakeRouterView{pinned: ShimInfo{IDPrefix: "abcdef01", Alias: "s1"}}
	b := &Bot{router: fv}

	reply, ok := b.handleUseCommand("123", "/use abcdef")
	require.True(t, ok)
	assert.Contains(t, reply, "(no label)")
}

func TestHandleUseStripsBotMention(t *testing.T) {
	fv := &fakeRouterView{pinned: ShimInfo{IDPrefix: "abcdef01", Alias: "s1", Label: "main"}}
	b := &Bot{router: fv}

	_, ok := b.handleUseCommand("123", "/use@my_bot abcdef")
	require.True(t, ok)
	assert.Equal(t, "abcdef", fv.lastPin.prefix)
}

func TestHandleUseMissingPrefix(t *testing.T) {
	b := &Bot{router: &fakeRouterView{}}
	reply, ok := b.handleUseCommand("123", "/use")
	require.True(t, ok)
	assert.Contains(t, reply, "Usage")
}

func TestHandleUseNoRouter(t *testing.T) {
	b := &Bot{}
	reply, ok := b.handleUseCommand("123", "/use abc")
	require.True(t, ok)
	assert.Contains(t, reply, "daemon mode")
}

func TestHandleUsePinError(t *testing.T) {
	fv := &fakeRouterView{pinErr: assertErr("ambiguous")}
	b := &Bot{router: fv}
	reply, ok := b.handleUseCommand("123", "/use ab")
	require.True(t, ok)
	assert.Contains(t, reply, "Pin failed")
	assert.Contains(t, reply, "ambiguous")
}

type stringErr string

func (s stringErr) Error() string { return string(s) }

func assertErr(s string) error { return stringErr(s) }

// renderShims output stays compact enough to never need Telegram's split.
func TestRenderShimsStaysUnderChunkLimit(t *testing.T) {
	now := time.Now()

	infos := make([]ShimInfo, 0, 10)
	for i := range 10 {
		infos = append(infos, ShimInfo{
			ID:           strings.Repeat("a", 12),
			IDPrefix:     "aaaaaaaa",
			Alias:        "s" + string(rune('0'+i)),
			Label:        "label-" + string(rune('0'+i)),
			Workdir:      "/workdir/" + string(rune('0'+i)),
			ConnectedAt:  now.Add(-time.Hour),
			LastOutbound: now.Add(-time.Minute),
		})
	}

	b := &Bot{router: &fakeRouterView{snap: infos}}
	out := b.renderShims(now)
	assert.Less(t, len(out), 4000)
}

// ===== handleCommand dispatcher integration tests =====

func TestHandleCommand_statusPairedWithShims(t *testing.T) {
	b, api, _ := newTestBot(t, allowlistState("1"))
	b.router = &fakeRouterView{
		snap: []ShimInfo{{
			IDPrefix:    "deadbeef",
			Alias:       "s1",
			Label:       "demo",
			Workdir:     "/home/u",
			ConnectedAt: time.Now().Add(-time.Hour),
		}},
	}
	require.NoError(t, b.handleCommand(t.Context(), dmMsg(1, "/status")))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)

	text := calls[0].params["text"].(string)
	assert.Contains(t, text, "Paired as")
	assert.Contains(t, text, "deadbeef")
	assert.Contains(t, text, "demo")
}

func TestHandleCommand_sessions_listsShims(t *testing.T) {
	b, api, _ := newTestBot(t, allowlistState("1"))
	b.router = &fakeRouterView{
		snap: []ShimInfo{{IDPrefix: "abcd0000", Alias: "s1", Label: "main"}},
	}
	require.NoError(t, b.handleCommand(t.Context(), dmMsg(1, "/sessions")))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)

	text := calls[0].params["text"].(string)
	assert.Contains(t, text, "Tap a session")
	assert.Contains(t, payloadString(calls[0].params), "sess:use:abcd0000")
}

func TestHandleCommand_sessions_emptyList(t *testing.T) {
	b, api, _ := newTestBot(t, allowlistState("1"))
	b.router = &fakeRouterView{}
	require.NoError(t, b.handleCommand(t.Context(), dmMsg(1, "/sessions")))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].params["text"].(string), "No active CC sessions")
}

func TestHandleCommand_sessions_noRouter(t *testing.T) {
	b, api, _ := newTestBot(t, allowlistState("1"))
	require.NoError(t, b.handleCommand(t.Context(), dmMsg(1, "/sessions")))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].params["text"].(string), "daemon mode")
}

func TestHandleCommand_use_pinsShim(t *testing.T) {
	fv := &fakeRouterView{pinned: ShimInfo{IDPrefix: "abcdef01", Alias: "s1", Label: "main"}}

	b, api, _ := newTestBot(t, allowlistState("1"))
	b.router = fv
	require.NoError(t, b.handleCommand(t.Context(), dmMsg(1, "/use abcdef")))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].params["text"].(string), "Pinned")
	assert.Equal(t, "1", fv.lastPin.chatID)
	assert.Equal(t, "abcdef", fv.lastPin.prefix)
}

func allowlistState(userID string) access.State {
	return access.State{
		DMPolicy:  access.PolicyAllowlist,
		AllowFrom: []string{userID},
		Groups:    map[string]access.GroupPolicy{},
		Pending:   map[string]access.Pending{},
	}
}

func dmMsg(userID int64, text string) telego.Message {
	return telego.Message{
		Chat: telego.Chat{ID: userID, Type: "private"},
		From: &telego.User{ID: userID},
		Text: text,
	}
}

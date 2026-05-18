package bot

import (
	"errors"
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
	assert.Contains(t, reply, "no router wired")
}

func TestHandleUsePinError(t *testing.T) {
	fv := &fakeRouterView{pinErr: errors.New("ambiguous")}
	b := &Bot{router: fv}
	reply, ok := b.handleUseCommand("123", "/use ab")
	require.True(t, ok)
	assert.Contains(t, reply, "Pin failed")
	assert.Contains(t, reply, "ambiguous")
}

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
	assert.Contains(t, calls[0].params["text"], "Paired as")
	assert.Contains(t, calls[0].params["text"], "deadbeef")
	assert.Contains(t, calls[0].params["text"], "demo")
}

func TestHandleCommand_sessions_listsShims(t *testing.T) {
	b, api, _ := newTestBot(t, allowlistState("1"))
	b.router = &fakeRouterView{
		snap: []ShimInfo{{IDPrefix: "abcd0000", Alias: "s1", Label: "main"}},
	}
	require.NoError(t, b.handleCommand(t.Context(), dmMsg(1, "/sessions")))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].params["text"], "Tap a session")
	assert.Contains(t, payloadString(calls[0].params), "sess:use:abcd0000")
}

func TestHandleCommand_sessions_emptyList(t *testing.T) {
	b, api, _ := newTestBot(t, allowlistState("1"))
	b.router = &fakeRouterView{}
	require.NoError(t, b.handleCommand(t.Context(), dmMsg(1, "/sessions")))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].params["text"], "No active CC sessions")
}

func TestHandleCommand_sessions_noRouter(t *testing.T) {
	b, api, _ := newTestBot(t, allowlistState("1"))
	require.NoError(t, b.handleCommand(t.Context(), dmMsg(1, "/sessions")))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].params["text"], "no router wired")
}

func TestHandleCommand_use_pinsShim(t *testing.T) {
	fv := &fakeRouterView{pinned: ShimInfo{IDPrefix: "abcdef01", Alias: "s1", Label: "main"}}

	b, api, _ := newTestBot(t, allowlistState("1"))
	b.router = fv
	require.NoError(t, b.handleCommand(t.Context(), dmMsg(1, "/use abcdef")))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].params["text"], "Pinned")
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

// ===== /idle command =====

func TestPickIdleEmpty(t *testing.T) {
	b := &Bot{router: &fakeRouterView{}}
	_, ok := b.pickIdle(time.Now())
	assert.False(t, ok)
}

func TestPickIdleNoRouter(t *testing.T) {
	b := &Bot{}
	_, ok := b.pickIdle(time.Now())
	assert.False(t, ok)
}

func TestPickIdleSelectsLongest(t *testing.T) {
	now := time.Now()
	fv := &fakeRouterView{snap: []ShimInfo{
		{ID: "a", IDPrefix: "a", Alias: "s1", LastOutbound: now.Add(-time.Second)},
		{ID: "b", IDPrefix: "b", Alias: "s2", LastOutbound: now.Add(-time.Hour)},
		{ID: "c", IDPrefix: "c", Alias: "s3", LastOutbound: now.Add(-time.Minute)},
	}}
	b := &Bot{router: fv}

	pick, ok := b.pickIdle(now)
	require.True(t, ok)
	assert.Equal(t, "b", pick.ID)
}

func TestPickIdleFallsBackToConnectedAt(t *testing.T) {
	now := time.Now()
	fv := &fakeRouterView{snap: []ShimInfo{
		{ID: "a", IDPrefix: "a", ConnectedAt: now.Add(-5 * time.Minute), LastOutbound: now.Add(-time.Second)},
		{ID: "b", IDPrefix: "b", ConnectedAt: now.Add(-time.Hour)}, // never sent → uses ConnectedAt
	}}
	b := &Bot{router: fv}

	pick, ok := b.pickIdle(now)
	require.True(t, ok)
	assert.Equal(t, "b", pick.ID)
}

func TestHandleCommand_idle_listsMostIdle(t *testing.T) {
	now := time.Now()
	fv := &fakeRouterView{snap: []ShimInfo{
		{IDPrefix: "abcd1111", Alias: "s1", Label: "main", Workdir: "/w1", LastOutbound: now.Add(-time.Hour)},
	}}

	b, api, _ := newTestBot(t, allowlistState("1"))
	b.router = fv
	require.NoError(t, b.handleCommand(t.Context(), dmMsg(1, "/idle")))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].params["text"], "Most idle")
	assert.Contains(t, calls[0].params["text"], "abcd1111")
	assert.Contains(t, calls[0].params["text"], "main")

	payload := payloadString(calls[0].params)
	assert.Contains(t, payload, "sess:use:abcd1111")
	assert.Contains(t, payload, "sess:kill:abcd1111")
}

func TestHandleCommand_idle_emptyList(t *testing.T) {
	b, api, _ := newTestBot(t, allowlistState("1"))
	b.router = &fakeRouterView{}
	require.NoError(t, b.handleCommand(t.Context(), dmMsg(1, "/idle")))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].params["text"], "No active CC sessions")
}

func TestHandleCommand_idle_noRouter(t *testing.T) {
	b, api, _ := newTestBot(t, allowlistState("1"))
	require.NoError(t, b.handleCommand(t.Context(), dmMsg(1, "/idle")))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].params["text"], "no router wired")
}

// ===== sess: callback =====

func TestCallbackSessPrefixRegex(t *testing.T) {
	cases := map[string]bool{
		"sess:use:abcdef01":  true,
		"sess:kill:abcdef01": true,
		"sess:use:":          false,
		"perm:allow:abcde":   false,
		"sess:bogus:abcdef":  false,
		"sess:use:ABCDEF":    false, // uppercase rejected
	}
	for input, want := range cases {
		t.Run(input, func(t *testing.T) {
			got := sessCallbackRE.MatchString(input)
			assert.Equal(t, want, got)
		})
	}
}

func TestHandleCallback_sessUseAllowed_pinsAndAcks(t *testing.T) {
	fv := &fakeRouterView{pinned: ShimInfo{IDPrefix: "abcdef01", Alias: "s1", Label: "main"}}
	b, api, _ := newTestBot(t, allowlistState("1"))
	b.router = fv

	q := telego.CallbackQuery{
		ID:      "cb1",
		From:    telego.User{ID: 1},
		Data:    "sess:use:abcdef01",
		Message: &telego.Message{MessageID: 99, Chat: telego.Chat{ID: 1}, Text: "before"},
	}
	require.NoError(t, b.handleCallback(t.Context(), q))

	assert.Equal(t, "1", fv.lastPin.chatID)
	assert.Equal(t, "abcdef01", fv.lastPin.prefix)

	acks := api.recordedCalls("answerCallbackQuery")
	require.Len(t, acks, 1)
	assert.Contains(t, payloadString(acks[0].params), "Pinned")

	edits := api.recordedCalls("editMessageText")
	require.Len(t, edits, 1)
	assert.Contains(t, edits[0].params["text"], "Pinned")
}

func TestHandleCallback_sessKillAllowed_evictsAndAcks(t *testing.T) {
	fv := &fakeRouterView{evicted: ShimInfo{IDPrefix: "abcdef01", Alias: "s1", Label: "main"}}
	b, api, _ := newTestBot(t, allowlistState("1"))
	b.router = fv

	q := telego.CallbackQuery{
		ID:      "cb2",
		From:    telego.User{ID: 1},
		Data:    "sess:kill:abcdef01",
		Message: &telego.Message{MessageID: 99, Chat: telego.Chat{ID: 1}, Text: "before"},
	}
	require.NoError(t, b.handleCallback(t.Context(), q))

	assert.Equal(t, "abcdef01", fv.lastEvict)

	acks := api.recordedCalls("answerCallbackQuery")
	require.Len(t, acks, 1)
	assert.Contains(t, payloadString(acks[0].params), "Evicted")
}

func TestHandleCallback_sessUseNotAllowed_denies(t *testing.T) {
	fv := &fakeRouterView{}
	b, api, _ := newTestBot(t, allowlistState("1"))
	b.router = fv

	q := telego.CallbackQuery{
		ID:   "cb3",
		From: telego.User{ID: 999}, // not allowlisted
		Data: "sess:use:abcdef01",
	}
	require.NoError(t, b.handleCallback(t.Context(), q))

	assert.Empty(t, fv.lastPin.prefix, "Pin should not have been called for unauthorized user")

	acks := api.recordedCalls("answerCallbackQuery")
	require.Len(t, acks, 1)
	assert.Contains(t, payloadString(acks[0].params), "Not authorized")
}

func TestHandleCallback_sessNoRouter_says_unavailable(t *testing.T) {
	b, api, _ := newTestBot(t, allowlistState("1"))
	q := telego.CallbackQuery{
		ID:   "cb4",
		From: telego.User{ID: 1},
		Data: "sess:use:abcdef01",
	}
	require.NoError(t, b.handleCallback(t.Context(), q))

	acks := api.recordedCalls("answerCallbackQuery")
	require.Len(t, acks, 1)
	assert.Contains(t, payloadString(acks[0].params), "Session switcher unavailable")
}

func TestHandleCallback_sessPinError_acksMessage(t *testing.T) {
	fv := &fakeRouterView{pinErr: errors.New("ambiguous")}
	b, api, _ := newTestBot(t, allowlistState("1"))
	b.router = fv

	q := telego.CallbackQuery{
		ID:      "cb5",
		From:    telego.User{ID: 1},
		Data:    "sess:use:abcdef01",
		Message: &telego.Message{MessageID: 99, Chat: telego.Chat{ID: 1}, Text: "before"},
	}
	require.NoError(t, b.handleCallback(t.Context(), q))

	acks := api.recordedCalls("answerCallbackQuery")
	require.Len(t, acks, 1)
	assert.Contains(t, payloadString(acks[0].params), "Pin failed")
}

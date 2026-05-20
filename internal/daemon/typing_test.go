package daemon

import (
	"context"
	"errors"
	"os"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeTypingBot records every call. Methods are safe for concurrent use because
// the tracker fires them from outside its own mutex; reusing a fakeTypingBot
// across goroutines must not race.
type fakeTypingBot struct {
	mu       sync.Mutex
	actions  []typingCall
	reacts   []reactCall
	actionEr error
	reactEr  error
}

type typingCall struct {
	chatID string
	action string
}

type reactCall struct {
	chatID string
	msgID  int
	emoji  string
}

func (f *fakeTypingBot) SendChatAction(_ context.Context, chatID, action string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.actions = append(f.actions, typingCall{chatID, action})

	return f.actionEr
}

func (f *fakeTypingBot) React(_ context.Context, chatID string, msgID int, emoji string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.reacts = append(f.reacts, reactCall{chatID, msgID, emoji})

	return f.reactEr
}

func (f *fakeTypingBot) snapshotActions() []typingCall {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.actions)
}

func (f *fakeTypingBot) snapshotReacts() []reactCall {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.reacts)
}

func TestTypingTracker_DefaultsApplied(t *testing.T) {
	tr := NewTypingTracker(&fakeTypingBot{}, TypingConfig{})

	assert.Equal(t, defaultTypingRefresh, tr.cfg.RefreshInterval)
	assert.Equal(t, defaultTypingTTL, tr.cfg.TTL)
	assert.Equal(t, defaultRotateInterval, tr.cfg.RotationInterval)
	assert.Equal(t, defaultRotationEmojis, tr.cfg.RotationEmojis)
}

func TestTypingTracker_MarkClearPending(t *testing.T) {
	tr := NewTypingTracker(&fakeTypingBot{}, TypingConfig{})

	tr.Mark("123", 7, true)
	tr.Mark("456", 0, false)
	assert.ElementsMatch(t, []string{"123", "456"}, tr.Pending())

	tr.Clear("123")
	assert.ElementsMatch(t, []string{"456"}, tr.Pending())

	// Clearing an unknown chat is a no-op.
	tr.Clear("999")
	assert.ElementsMatch(t, []string{"456"}, tr.Pending())
}

func TestTypingTracker_TickFiresTypingForEveryPendingChat(t *testing.T) {
	bot := &fakeTypingBot{}
	tr := NewTypingTracker(bot, TypingConfig{})

	tr.Mark("aaa", 1, false)
	tr.Mark("bbb", 2, false)

	tr.tickOnce(t.Context(), time.Now())

	actions := bot.snapshotActions()
	require.Len(t, actions, 2)

	chats := []string{actions[0].chatID, actions[1].chatID}
	assert.ElementsMatch(t, []string{"aaa", "bbb"}, chats)
	assert.Equal(t, "typing", actions[0].action)
	assert.Equal(t, "typing", actions[1].action)
	assert.Empty(t, bot.snapshotReacts(), "rotateReaction=false must not call React")
}

func TestTypingTracker_TickExpiresStaleEntries(t *testing.T) {
	bot := &fakeTypingBot{}
	tr := NewTypingTracker(bot, TypingConfig{TTL: 50 * time.Millisecond})

	tr.Mark("aaa", 0, false)
	assert.Equal(t, []string{"aaa"}, tr.Pending())

	tr.tickOnce(t.Context(), time.Now().Add(time.Second))

	assert.Empty(t, tr.Pending(), "TTL elapsed — entry must be evicted")
	assert.Empty(t, bot.snapshotActions(), "expired entry must not fire SendChatAction")
}

func TestTypingTracker_TickRotatesReactionsOnSchedule(t *testing.T) {
	bot := &fakeTypingBot{}
	tr := NewTypingTracker(bot, TypingConfig{
		RefreshInterval:  100 * time.Millisecond,
		RotationInterval: 200 * time.Millisecond,
		RotationEmojis:   []string{"A", "B", "C"},
	})

	tr.Mark("chat", 42, true)

	start := time.Now()
	tr.tickOnce(t.Context(), start)                                  // tick 1: rotates (lastRotate was zero)
	tr.tickOnce(t.Context(), start.Add(150*time.Millisecond))        // tick 2: no rotate yet
	tr.tickOnce(t.Context(), start.Add(250*time.Millisecond))        // tick 3: rotates (>=200ms since last)
	tr.tickOnce(t.Context(), start.Add(500*time.Millisecond))        // tick 4: rotates

	actions := bot.snapshotActions()
	assert.Len(t, actions, 4, "every tick fires typing regardless of rotation cadence")

	reacts := bot.snapshotReacts()
	require.Len(t, reacts, 3, "rotation should fire on ticks 1, 3, 4")
	assert.Equal(t, "A", reacts[0].emoji)
	assert.Equal(t, "B", reacts[1].emoji)
	assert.Equal(t, "C", reacts[2].emoji)

	for _, r := range reacts {
		assert.Equal(t, "chat", r.chatID)
		assert.Equal(t, 42, r.msgID)
	}
}

func TestTypingTracker_TickSkipsRotationWhenDisabled(t *testing.T) {
	bot := &fakeTypingBot{}
	tr := NewTypingTracker(bot, TypingConfig{})

	tr.Mark("chat", 42, false)
	tr.tickOnce(t.Context(), time.Now())

	assert.Len(t, bot.snapshotActions(), 1)
	assert.Empty(t, bot.snapshotReacts(), "rotateReaction=false suppresses React")
}

func TestTypingTracker_RotationDisabledForZeroMsgID(t *testing.T) {
	bot := &fakeTypingBot{}
	tr := NewTypingTracker(bot, TypingConfig{})

	tr.Mark("chat", 0, true)
	tr.tickOnce(t.Context(), time.Now())

	assert.Len(t, bot.snapshotActions(), 1)
	assert.Empty(t, bot.snapshotReacts(), "msgID=0 must suppress reaction even when caller asks for rotation")
}

func TestTypingTracker_EmptyEmojiSetDisablesRotation(t *testing.T) {
	bot := &fakeTypingBot{}
	tr := NewTypingTracker(bot, TypingConfig{RotationEmojis: []string{}})

	tr.Mark("chat", 42, true)
	tr.tickOnce(t.Context(), time.Now())

	assert.Empty(t, bot.snapshotReacts(), "empty (non-nil) RotationEmojis disables rotation")
}

func TestTypingTracker_RotationCyclesThroughEmojis(t *testing.T) {
	bot := &fakeTypingBot{}
	tr := NewTypingTracker(bot, TypingConfig{
		RotationInterval: 10 * time.Millisecond,
		RotationEmojis:   []string{"X", "Y"},
	})

	tr.Mark("chat", 1, true)

	start := time.Now()
	for i := range 5 {
		tr.tickOnce(t.Context(), start.Add(time.Duration(i)*100*time.Millisecond))
	}

	reacts := bot.snapshotReacts()
	require.Len(t, reacts, 5)
	assert.Equal(t, []string{"X", "Y", "X", "Y", "X"}, []string{
		reacts[0].emoji, reacts[1].emoji, reacts[2].emoji, reacts[3].emoji, reacts[4].emoji,
	}, "rotationIdx wraps with modulo over the emoji slice")
}

func TestTypingTracker_TickSurvivesBotErrors(t *testing.T) {
	bot := &fakeTypingBot{actionEr: errors.New("api down"), reactEr: errors.New("react down")}
	tr := NewTypingTracker(bot, TypingConfig{})

	tr.Mark("chat", 1, true)
	require.NotPanics(t, func() {
		tr.tickOnce(t.Context(), time.Now())
	})

	assert.Equal(t, []string{"chat"}, tr.Pending(), "transient API errors must not drop the entry")
}

func TestTypingTracker_RunCancelExits(t *testing.T) {
	bot := &fakeTypingBot{}
	tr := NewTypingTracker(bot, TypingConfig{RefreshInterval: 5 * time.Millisecond, TTL: time.Hour})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() {
		tr.Run(ctx)
		close(done)
	}()

	tr.Mark("aaa", 0, false)

	require.Eventually(t, func() bool {
		return len(bot.snapshotActions()) >= 2
	}, time.Second, 5*time.Millisecond, "Run must drive ticks while ctx is alive")

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not exit within 1s of cancel")
	}
}

func TestTypingTracker_ConcurrentMarkClearIsSafe(t *testing.T) {
	bot := &fakeTypingBot{}
	tr := NewTypingTracker(bot, TypingConfig{})

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			chat := "c" + strconv.Itoa(i%5)
			tr.Mark(chat, i, true)
			tr.Clear(chat)
		})
	}
	wg.Wait()
	tr.tickOnce(t.Context(), time.Now())
}

func TestTypingTracker_NilSafe(t *testing.T) {
	var tr *TypingTracker

	require.NotPanics(t, func() {
		tr.Mark("c", 1, true)
		tr.Clear("c")
		tr.Done(t.Context(), "c")
		assert.Nil(t, tr.Pending())
	}, "nil tracker calls are no-ops so daemon wiring can skip the field when disabled")
}

func TestTypingTracker_DoneReactsAndClears(t *testing.T) {
	bot := &fakeTypingBot{}
	tr := NewTypingTracker(bot, TypingConfig{})

	tr.Mark("chat", 99, true)
	tr.Done(t.Context(), "chat")

	assert.Empty(t, tr.Pending(), "Done removes the entry like Clear does")

	reacts := bot.snapshotReacts()
	require.Len(t, reacts, 1)
	assert.Equal(t, reactCall{chatID: "chat", msgID: 99, emoji: defaultDoneEmoji}, reacts[0])
}

func TestTypingTracker_DoneSkipsWhenRotationDisabled(t *testing.T) {
	bot := &fakeTypingBot{}
	tr := NewTypingTracker(bot, TypingConfig{})

	tr.Mark("chat", 99, false)
	tr.Done(t.Context(), "chat")

	assert.Empty(t, tr.Pending(), "Done still clears even when rotation was off")
	assert.Empty(t, bot.snapshotReacts(), "Done must not place a reaction when the user opted out of rotation")
}

func TestTypingTracker_DoneNoopForUnknownChat(t *testing.T) {
	bot := &fakeTypingBot{}
	tr := NewTypingTracker(bot, TypingConfig{})

	tr.Done(t.Context(), "never-marked")

	assert.Empty(t, bot.snapshotReacts())
}

func TestTypingTracker_DoneRespectsDisabledFlag(t *testing.T) {
	bot := &fakeTypingBot{}
	tr := NewTypingTracker(bot, TypingConfig{DoneEmojiDisabled: true})

	assert.Empty(t, tr.cfg.DoneEmoji, "DoneEmojiDisabled forces DoneEmoji to empty in the constructor")

	tr.Mark("chat", 99, true)
	tr.Done(t.Context(), "chat")

	assert.Empty(t, tr.Pending())
	assert.Empty(t, bot.snapshotReacts(), "DoneEmojiDisabled suppresses the swap and Done degenerates to Clear")
}

func TestTypingTracker_DoneEmojiDisabledOverridesExplicitEmoji(t *testing.T) {
	bot := &fakeTypingBot{}
	tr := NewTypingTracker(bot, TypingConfig{DoneEmoji: "🔥", DoneEmojiDisabled: true})

	assert.Empty(t, tr.cfg.DoneEmoji, "Disabled wins over any non-empty DoneEmoji")

	tr.Mark("chat", 99, true)
	tr.Done(t.Context(), "chat")

	assert.Empty(t, bot.snapshotReacts())
}

func TestTypingTracker_DoneIdempotent(t *testing.T) {
	bot := &fakeTypingBot{}
	tr := NewTypingTracker(bot, TypingConfig{})

	tr.Mark("chat", 99, true)
	tr.Done(t.Context(), "chat")
	tr.Done(t.Context(), "chat")
	tr.Done(t.Context(), "chat")

	assert.Len(t, bot.snapshotReacts(), 1, "second Done after entry is gone must be a no-op — covers edit-after-send")
}

func TestTypingEnabled_EnvOptOut(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"", true},
		{"1", true},
		{"yes", true},
		{"0", false},
		{"false", false},
		{"FALSE", false},
		{"No", false},
		{"off", false},
	}

	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			t.Setenv("TELEGRAM_TYPING_REFRESH", tc.val)
			assert.Equal(t, tc.want, TypingEnabled())
		})
	}
}

func TestTypingTTLFromEnv(t *testing.T) {
	cases := []struct {
		val  string
		want time.Duration
	}{
		{"", 0},
		{"30", 30 * time.Second},
		{"120", 120 * time.Second},
		{"abc", 0},
		{"0", 0},
		{"-5", 0},
	}

	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			t.Setenv("TELEGRAM_TYPING_TTL", tc.val)
			assert.Equal(t, tc.want, TypingTTLFromEnv())
		})
	}
}

func TestTypingRotationEmojisFromEnv(t *testing.T) {
	cases := []struct {
		name string
		set  bool
		val  string
		want []string
	}{
		{name: "unset returns nil so caller uses default", set: false, want: nil},
		{name: "empty value disables rotation", set: true, val: "", want: []string{}},
		{name: "whitespace-only value disables rotation", set: true, val: "   ", want: []string{}},
		{name: "comma-only value disables rotation", set: true, val: ",,,", want: []string{}},
		{name: "single emoji", set: true, val: "🔥", want: []string{"🔥"}},
		{name: "comma list", set: true, val: "🔥,⚡,💯", want: []string{"🔥", "⚡", "💯"}},
		{name: "whitespace around entries is trimmed", set: true, val: " 🔥 , ⚡ , 💯 ", want: []string{"🔥", "⚡", "💯"}},
		{name: "empty fields between commas are dropped", set: true, val: "🔥,,⚡,,,💯,", want: []string{"🔥", "⚡", "💯"}},
		{name: "preserves multi-codepoint emoji (writing hand)", set: true, val: "✍", want: []string{"✍"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("TELEGRAM_TYPING_ROTATION_EMOJIS", tc.val)
			} else {
				// t.Setenv only sets — explicitly clear so an outer-process value
				// doesn't bleed into the "unset" case.
				_ = os.Unsetenv("TELEGRAM_TYPING_ROTATION_EMOJIS")
			}

			got := TypingRotationEmojisFromEnv()
			assert.Equal(t, tc.want, got)

			if tc.set && len(tc.want) == 0 {
				assert.NotNil(t, got, "set-but-disabled must return non-nil empty slice to distinguish from default")
			}
		})
	}
}

func TestTypingDoneEmojiFromEnv(t *testing.T) {
	cases := []struct {
		name     string
		set      bool
		val      string
		want     string
		wantConf bool
	}{
		{name: "unset returns not-configured", set: false, want: "", wantConf: false},
		{name: "empty disables", set: true, val: "", want: "", wantConf: true},
		{name: "whitespace disables", set: true, val: "   ", want: "", wantConf: true},
		{name: "off disables", set: true, val: "off", want: "", wantConf: true},
		{name: "OFF case-insensitive", set: true, val: "OFF", want: "", wantConf: true},
		{name: "Off mixed case", set: true, val: "Off", want: "", wantConf: true},
		{name: "explicit emoji passes through", set: true, val: "🔥", want: "🔥", wantConf: true},
		{name: "whitespace around emoji is trimmed", set: true, val: "  🔥  ", want: "🔥", wantConf: true},
		{name: "garbage is forwarded as-is (Telegram rejects later)", set: true, val: "nope", want: "nope", wantConf: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("TELEGRAM_TYPING_DONE_EMOJI", tc.val)
			} else {
				_ = os.Unsetenv("TELEGRAM_TYPING_DONE_EMOJI")
			}

			emoji, configured := TypingDoneEmojiFromEnv()
			assert.Equal(t, tc.want, emoji)
			assert.Equal(t, tc.wantConf, configured)
		})
	}
}

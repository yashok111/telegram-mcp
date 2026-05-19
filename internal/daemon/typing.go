package daemon

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Default knobs for TypingTracker. Telegram's typing bubble decays after ~5s,
// so refresh slightly faster; the reaction rotation is throttled to avoid
// hammering the SetMessageReaction endpoint.
const (
	defaultTypingRefresh  = 4 * time.Second
	defaultTypingTTL      = 60 * time.Second
	defaultRotateInterval = 8 * time.Second
)

// rotationEmojis is the cycle daemon walks through for "still thinking"
// reactions on the user's original inbound message. Only emojis from
// Telegram's curated bot-reaction allowlist work — calls with any other
// emoji surface as `Bad Request: REACTION_INVALID` and the indicator gets
// stuck on the previous frame. Keep this list inside that allowlist.
//
// The first emoji is already placed by bot.handleMessage when
// access.State.AckReaction is set; the daemon rotates onwards from index 1.
var defaultRotationEmojis = []string{"👀", "🤔", "✍"}

// defaultDoneEmoji is what Done() swaps onto the inbound message after the
// shim's first outbound lands — a visible "agent finished" marker. Must be
// inside Telegram's bot-reaction allowlist for the same reason as
// defaultRotationEmojis above; ✅ is in the allowlist.
const defaultDoneEmoji = "✅"

// typingBot is the slice of *bot.Bot the tracker needs. Defined here so
// the typing package surface is testable with a tiny fake.
type typingBot interface {
	SendChatAction(ctx context.Context, chatID, action string) error
	React(ctx context.Context, chatID string, messageID int, emoji string) error
}

type TypingConfig struct {
	// RefreshInterval drives the ticker cadence — every tick fires a
	// SendChatAction(typing) for each tracked chat. Zero falls back to
	// defaultTypingRefresh.
	RefreshInterval time.Duration

	// TTL caps how long a chat stays in the pending set without an outbound
	// Clear. Zero falls back to defaultTypingTTL.
	TTL time.Duration

	// RotationInterval is the minimum spacing between reaction swaps on the
	// same inbound message. Zero falls back to defaultRotateInterval.
	RotationInterval time.Duration

	// RotationEmojis is the cycle the daemon walks through. Nil falls back
	// to defaultRotationEmojis. Empty (non-nil) slice disables rotation.
	RotationEmojis []string

	// DoneEmoji is what Done() places on the inbound message when the shim
	// sends its first outbound. Empty string disables the swap (Done then
	// degenerates into Clear). Zero value falls back to defaultDoneEmoji.
	DoneEmoji string
}

// TypingTracker keeps a per-chat pending set and, while ctx is alive, sends a
// SendChatAction("typing") to every chat in the set on each tick. Optionally
// rotates a reaction emoji on the inbound message to give long-running
// agent work a visible heartbeat in the Telegram UI.
type TypingTracker struct {
	cfg TypingConfig

	botMu sync.RWMutex
	bot   typingBot

	mu      sync.Mutex
	pending map[string]*typingEntry
}

type typingEntry struct {
	msgID         int
	startedAt     time.Time
	lastTyping    time.Time
	lastRotate    time.Time
	rotationIdx   int
	rotateEnabled bool
}

func NewTypingTracker(b typingBot, cfg TypingConfig) *TypingTracker {
	if cfg.RefreshInterval <= 0 {
		cfg.RefreshInterval = defaultTypingRefresh
	}

	if cfg.TTL <= 0 {
		cfg.TTL = defaultTypingTTL
	}

	if cfg.RotationInterval <= 0 {
		cfg.RotationInterval = defaultRotateInterval
	}

	if cfg.RotationEmojis == nil {
		cfg.RotationEmojis = defaultRotationEmojis
	}

	if cfg.DoneEmoji == "" {
		cfg.DoneEmoji = defaultDoneEmoji
	}

	return &TypingTracker{
		cfg:     cfg,
		bot:     b,
		pending: map[string]*typingEntry{},
	}
}

// AttachBot wires the outbound surface after construction. Used by main()
// where the tracker must be built before the *bot.Bot exists so the Notifier
// can hold a reference to the tracker before bot.NewWithRouter sees the
// Notifier. Callers must invoke AttachBot before Run kicks off the ticker;
// once Run has started, swapping the bot mid-flight races with tick I/O.
func (t *TypingTracker) AttachBot(b typingBot) {
	if t == nil {
		return
	}

	t.botMu.Lock()
	t.bot = b
	t.botMu.Unlock()
}

func (t *TypingTracker) currentBot() typingBot {
	t.botMu.RLock()
	defer t.botMu.RUnlock()

	return t.bot
}

// Mark adds chatID to the pending set so the next tick fires a typing action.
// msgID is the inbound message that should carry the rotating reaction; pass
// 0 (or rotateReaction=false) to suppress rotation. A second Mark for the
// same chat resets startedAt — equivalent to "the user just pinged again".
func (t *TypingTracker) Mark(chatID string, msgID int, rotateReaction bool) {
	if t == nil {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	t.pending[chatID] = &typingEntry{
		msgID:         msgID,
		startedAt:     now,
		rotateEnabled: rotateReaction && msgID > 0 && len(t.cfg.RotationEmojis) > 0,
	}
}

// Clear removes chatID from the pending set. Safe to call for unknown chats —
// they're a no-op. Called by daemon handlers after an outbound reply lands.
func (t *TypingTracker) Clear(chatID string) {
	if t == nil {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	delete(t.pending, chatID)
}

// Done is Clear plus a final "agent answered" reaction on the original
// inbound message. Called instead of Clear when a shim's first outbound
// for the pending chat lands, so the rotating 👀 / 🤔 / ✍ visibly settles
// on a ✅ instead of freezing on whichever frame the ticker last set.
//
// The reaction fires only when rotation was armed for this chat (Mark was
// called with rotateReaction=true and a non-zero msgID) and DoneEmoji is
// non-empty — otherwise this degenerates into a Clear so users who opted
// out of reactions via "/reaction off" don't suddenly get one.
func (t *TypingTracker) Done(ctx context.Context, chatID string) {
	if t == nil {
		return
	}

	t.mu.Lock()
	entry, ok := t.pending[chatID]

	var msgID int

	var rotateEnabled bool

	if ok {
		msgID = entry.msgID
		rotateEnabled = entry.rotateEnabled
		delete(t.pending, chatID)
	}

	emoji := t.cfg.DoneEmoji
	t.mu.Unlock()

	if !ok || !rotateEnabled || msgID == 0 || emoji == "" {
		return
	}

	b := t.currentBot()
	if b == nil {
		return
	}

	if err := b.React(ctx, chatID, msgID, emoji); err != nil {
		slog.Warn("typing tracker Done React failed", "chat_id", chatID, "msg_id", msgID, "emoji", emoji, "err", err)
	}
}

// Pending returns the chat IDs currently tracked. Exposed for tests + daemon
// diagnostics; not on the hot path.
func (t *TypingTracker) Pending() []string {
	if t == nil {
		return nil
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	out := make([]string, 0, len(t.pending))
	for c := range t.pending {
		out = append(out, c)
	}

	return out
}

// Run blocks until ctx is done, firing a tick every RefreshInterval. The
// initial tick happens after one interval, matching the time.Ticker contract.
func (t *TypingTracker) Run(ctx context.Context) {
	ticker := time.NewTicker(t.cfg.RefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			t.tickOnce(ctx, now)
		}
	}
}

// tickOnce snapshots the pending set under the mutex, then fires SendChatAction
// and React calls outside the lock so a slow Telegram response can't stall
// concurrent Mark/Clear callers. Expired entries (older than TTL) are
// dropped on the same pass.
func (t *TypingTracker) tickOnce(ctx context.Context, now time.Time) {
	type job struct {
		chatID string
		msgID  int
		emoji  string
	}

	var jobs []job

	t.mu.Lock()

	for chat, e := range t.pending {
		if now.Sub(e.startedAt) > t.cfg.TTL {
			delete(t.pending, chat)
			slog.Info("typing tracker TTL expired", "chat_id", chat, "age", now.Sub(e.startedAt))

			continue
		}

		j := job{chatID: chat, msgID: e.msgID}
		e.lastTyping = now

		if e.rotateEnabled && now.Sub(e.lastRotate) >= t.cfg.RotationInterval {
			j.emoji = t.cfg.RotationEmojis[e.rotationIdx%len(t.cfg.RotationEmojis)]
			e.rotationIdx++
			e.lastRotate = now
		}

		jobs = append(jobs, j)
	}

	t.mu.Unlock()

	b := t.currentBot()
	if b == nil {
		return
	}

	for _, j := range jobs {
		if err := b.SendChatAction(ctx, j.chatID, "typing"); err != nil {
			slog.Warn("typing tracker SendChatAction failed", "chat_id", j.chatID, "err", err)
		}

		if j.emoji != "" && j.msgID > 0 {
			if err := b.React(ctx, j.chatID, j.msgID, j.emoji); err != nil {
				slog.Warn("typing tracker React failed", "chat_id", j.chatID, "msg_id", j.msgID, "emoji", j.emoji, "err", err)
			}
		}
	}
}

// TypingEnabled reports whether the daemon should run the typing-refresh
// goroutine. Disabled by env TELEGRAM_TYPING_REFRESH in {"0","false","no","off"}
// (case-insensitive). Default: enabled.
func TypingEnabled() bool {
	v := strings.TrimSpace(os.Getenv("TELEGRAM_TYPING_REFRESH"))
	if v == "" {
		return true
	}

	switch strings.ToLower(v) {
	case "0", "false", "no", "off":
		return false
	}

	return true
}

// TypingTTLFromEnv reads TELEGRAM_TYPING_TTL as an integer second count.
// Returns 0 (caller falls back to defaultTypingTTL) on parse failure or
// non-positive values.
func TypingTTLFromEnv() time.Duration {
	v := strings.TrimSpace(os.Getenv("TELEGRAM_TYPING_TTL"))
	if v == "" {
		return 0
	}

	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		slog.Warn("invalid TELEGRAM_TYPING_TTL — falling back to default", "value", v, "err", err)
		return 0
	}

	return time.Duration(n) * time.Second
}

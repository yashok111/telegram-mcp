package bot

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

// ShimInfo mirrors daemon.ShimInfo for bot-side rendering. Kept duplicated to
// avoid a bot→daemon import. Conversion happens at the wiring boundary in main.
type ShimInfo struct {
	ID           string
	IDPrefix     string
	Alias        string
	Label        string
	Workdir      string
	CCSessionID  string
	ConnectedAt  time.Time
	LastOutbound time.Time
	PinnedChats  []string
}

// RouterView is the slice of the daemon Router the bot needs for /status,
// /sessions, /use, /idle commands. Embedded mode passes nil; commands then
// no-op with a friendly message.
type RouterView interface {
	Snapshot() []ShimInfo
	Pin(chatID, shimIDPrefix string, ttl time.Duration) (ShimInfo, error)
	Evict(shimIDPrefix string) (ShimInfo, error)
}

// PinTTL is how long /use and /sessions pins last. Matches the daemon's
// default IdleTimeout so a pin doesn't outlive the daemon that holds it.
const PinTTL = 30 * time.Minute

func (b *Bot) renderShims(now time.Time) string {
	if b.router == nil {
		return "Session switcher is only available in daemon mode (TELEGRAM_DAEMON=1). Running in embedded mode."
	}

	shims := b.router.Snapshot()
	if len(shims) == 0 {
		return "No active CC sessions connected to this daemon."
	}

	var sb strings.Builder

	fmt.Fprintf(&sb, "🔌 %d active session(s):\n\n", len(shims))

	for _, s := range shims {
		label := s.Label
		if label == "" {
			label = "(no label)"
		}

		wd := s.Workdir
		if wd == "" {
			wd = "?"
		}

		idle := s.IdleFor(now).Truncate(time.Second)

		state := "idle"
		if !s.LastOutbound.IsZero() && idle < 30*time.Second {
			state = "busy"
		}

		pinNote := ""
		if len(s.PinnedChats) > 0 {
			pinNote = " 📌"
		}

		fmt.Fprintf(&sb, "• %s [%s] %s%s\n  %s\n  %s ago • %s\n",
			s.IDPrefix, s.Alias, label, pinNote, wd, idle, state)
	}

	return sb.String()
}

// IdleFor mirrors daemon.ShimInfo.IdleFor for bot-side rendering.
func (s ShimInfo) IdleFor(now time.Time) time.Duration {
	t := s.LastOutbound
	if t.IsZero() {
		t = s.ConnectedAt
	}

	return now.Sub(t)
}

// handleUseCommand parses "/use <prefix>" and pins. Returns (reply, true) on
// any handled outcome so the caller can forward reply verbatim via SendMessage.
func (b *Bot) handleUseCommand(chatID, text string) (string, bool) {
	if b.router == nil {
		return "Session switcher is only available in daemon mode.", true
	}

	rest := strings.TrimSpace(text)
	// Strip leading "/use" and any "@bot_name" suffix that Telegram appends.
	if idx := strings.IndexAny(rest, " \t"); idx >= 0 {
		rest = strings.TrimSpace(rest[idx+1:])
	} else {
		rest = ""
	}

	parts := strings.Fields(rest)
	if len(parts) == 0 {
		return "Usage: /use <shim_id_prefix>\nFind prefixes via /status or /sessions.", true
	}

	prefix := parts[0]

	info, err := b.router.Pin(chatID, prefix, PinTTL)
	if err != nil {
		return fmt.Sprintf("Pin failed: %s.", err.Error()), true
	}

	label := info.Label
	if label == "" {
		label = "(no label)"
	}

	return fmt.Sprintf("📌 Pinned %s [%s] %s for %s. Your messages route here until pin expires or another session takes over.",
		info.IDPrefix, info.Alias, label, PinTTL), true
}

// sendSessions renders the inline-keyboard picker for /sessions.
func (b *Bot) sendSessions(ctx context.Context, msg telego.Message) {
	if b.router == nil {
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), "Session switcher is only available in daemon mode."))
		return
	}

	shims := b.router.Snapshot()
	if len(shims) == 0 {
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), "No active CC sessions connected."))
		return
	}

	rows := make([][]telego.InlineKeyboardButton, 0, len(shims))

	for _, s := range shims {
		label := s.Label
		if label == "" {
			label = "(no label)"
		}

		btn := tu.InlineKeyboardButton(fmt.Sprintf("%s [%s] %s", s.IDPrefix, s.Alias, label)).
			WithCallbackData("sess:use:" + s.IDPrefix)
		rows = append(rows, tu.InlineKeyboardRow(btn))
	}

	keyboard := tu.InlineKeyboard(rows...)
	_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), "Tap a session to pin it as your reply target:").WithReplyMarkup(keyboard))
}

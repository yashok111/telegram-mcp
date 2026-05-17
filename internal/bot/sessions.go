package bot

import (
	"fmt"
	"strings"
	"time"
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

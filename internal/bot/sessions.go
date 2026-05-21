package bot

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

// sessCallbackRE matches the session-control callback button payloads emitted
// by /sessions and /idle. Lowercase-hex is what daemon.ShimInfo.IDPrefix
// returns, so any other casing is rejected as a forged/garbled payload.
var sessCallbackRE = regexp.MustCompile(`^sess:(use|kill):([a-f0-9]{1,12})$`)

// labelCallbackRE matches "sess:label:<hex-prefix>" — the label text itself is
// looked up out-of-band in Bot.pendingLabel because Telegram's 64-byte
// callback_data limit doesn't comfortably fit utf-8 label strings.
var labelCallbackRE = regexp.MustCompile(`^sess:label:([a-f0-9]{1,12})$`)

const pendingLabelTTL = 60 * time.Second

type pendingLabel struct {
	text      string
	expiresAt time.Time
}

// ShimInfo mirrors daemon.ShimInfo for bot-side rendering. Kept duplicated to
// avoid a bot→daemon import. Conversion happens at the wiring boundary in main.
type ShimInfo struct {
	ID           string
	IDPrefix     string
	Alias        string
	Label        string
	Workdir      string
	CCSessionID  string
	SpawnID      string
	TopicID      int
	ConnectedAt  time.Time
	LastOutbound time.Time
	PinnedChats  []string
}

// RouterView is the slice of the daemon Router the bot needs for /status,
// /sessions, /use, /idle commands. Always non-nil in production (daemon owns
// the Router); tests may pass nil to exercise bots that never invoke the
// session-switcher commands.
type RouterView interface {
	Snapshot() []ShimInfo
	Pin(chatID, shimIDPrefix string, ttl time.Duration) (ShimInfo, error)
	Evict(shimIDPrefix string) (ShimInfo, error)
	SetLabel(shimIDPrefix, label string) (ShimInfo, error)
}

// PinTTL is how long /use and /sessions pins last. Matches the daemon's
// default IdleTimeout so a pin doesn't outlive the daemon that holds it.
const PinTTL = 30 * time.Minute

func (b *Bot) renderShims(now time.Time) string {
	if b.router == nil {
		return "Session switcher is unavailable: no router wired\\."
	}

	shims := b.router.Snapshot()
	if len(shims) == 0 {
		return "No active CC sessions connected to this daemon\\."
	}

	var sb strings.Builder

	fmt.Fprintf(&sb, "🔌 %d active session\\(s\\):\n\n", len(shims))

	for _, s := range shims {
		label := "\\(no label\\)"
		if s.Label != "" {
			label = EscapeMarkdownV2(s.Label)
		}

		wd := "?"
		if s.Workdir != "" {
			wd = EscapeMarkdownV2(s.Workdir)
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

		fmt.Fprintf(&sb, "• %s \\[%s\\] %s%s\n  %s\n  %s ago • %s\n",
			MdCode(s.IDPrefix), MdCode(s.Alias), label, pinNote, wd, idle, state)
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
		return "Session switcher is unavailable: no router wired\\.", true
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
		return "Usage: /use \\<shim\\_id\\_prefix\\>\nFind prefixes via /status or /sessions\\.", true
	}

	prefix := parts[0]

	info, err := b.router.Pin(chatID, prefix, PinTTL)
	if err != nil {
		return "Pin failed: " + EscapeMarkdownV2(err.Error()) + "\\.", true
	}

	label := "\\(no label\\)"
	if info.Label != "" {
		label = EscapeMarkdownV2(info.Label)
	}

	// Returned string is MarkdownV2 — caller in bot.go sends with ParseMode "MarkdownV2".
	return "📌 Pinned " + MdCode(info.IDPrefix) + " \\[" + MdCode(info.Alias) + "\\] " + label +
		" for " + EscapeMarkdownV2(PinTTL.String()) + "\\. Your messages route here until pin expires or another session takes over\\.", true
}

// sendSessions renders the inline-keyboard picker for /sessions.
func (b *Bot) sendSessions(ctx context.Context, msg telego.Message) {
	if b.router == nil {
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), "Session switcher is unavailable: no router wired\\.").WithParseMode("MarkdownV2"))
		return
	}

	shims := b.router.Snapshot()
	if len(shims) == 0 {
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), "No active CC sessions connected\\.").WithParseMode("MarkdownV2"))
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

// pickIdle returns the shim whose IdleFor(now) is largest. Returns ok=false
// when no shims are connected or the router isn't wired (test-only path).
func (b *Bot) pickIdle(now time.Time) (ShimInfo, bool) {
	if b.router == nil {
		return ShimInfo{}, false
	}

	shims := b.router.Snapshot()
	if len(shims) == 0 {
		return ShimInfo{}, false
	}

	pick := shims[0]
	for _, s := range shims[1:] {
		if s.IdleFor(now) > pick.IdleFor(now) {
			pick = s
		}
	}

	return pick, true
}

func (b *Bot) sendIdle(ctx context.Context, msg telego.Message) {
	pick, ok := b.pickIdle(time.Now())
	if !ok {
		text := "No active CC sessions connected\\."
		if b.router == nil {
			text = "Session switcher is unavailable: no router wired\\."
		}

		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), text).WithParseMode("MarkdownV2"))

		return
	}

	idle := pick.IdleFor(time.Now()).Truncate(time.Second)

	label := "\\(no label\\)"
	if pick.Label != "" {
		label = EscapeMarkdownV2(pick.Label)
	}

	wd := "?"
	if pick.Workdir != "" {
		wd = EscapeMarkdownV2(pick.Workdir)
	}

	text := "Most idle: " + MdCode(pick.IDPrefix) + " \\[" + MdCode(pick.Alias) + "\\] " + label +
		"\n" + wd + "\nIdle for " + idle.String() + "\\."
	keyboard := tu.InlineKeyboard(tu.InlineKeyboardRow(
		tu.InlineKeyboardButton("📌 Use this").WithCallbackData("sess:use:"+pick.IDPrefix),
		tu.InlineKeyboardButton("❌ Evict").WithCallbackData("sess:kill:"+pick.IDPrefix),
	))
	_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), text).WithReplyMarkup(keyboard).WithParseMode("MarkdownV2"))
}

// handleSessCallback executes the use/kill action carried by a sess: callback.
// Caller is responsible for the allowlist check before reaching here — same
// rule as the existing perm: callback path.
func (b *Bot) handleSessCallback(ctx context.Context, q telego.CallbackQuery, action, prefix string) error {
	if b.router == nil {
		_ = b.api.AnswerCallbackQuery(ctx, &telego.AnswerCallbackQueryParams{
			CallbackQueryID: q.ID, Text: "Session switcher unavailable.",
		})

		return nil
	}

	switch action {
	case "use":
		chatID := strconv.FormatInt(q.From.ID, 10)
		if msg, ok := q.Message.(*telego.Message); ok && msg != nil {
			chatID = strconv.FormatInt(msg.Chat.ID, 10)
		}

		info, err := b.router.Pin(chatID, prefix, PinTTL)
		if err != nil {
			// nilerr: err is surfaced to the user via the callback ack toast; no
			// further handler-side error propagation is appropriate.
			_ = b.api.AnswerCallbackQuery(ctx, &telego.AnswerCallbackQueryParams{
				CallbackQueryID: q.ID, Text: "Pin failed: " + err.Error(),
			})

			return nil //nolint:nilerr // error already surfaced to user via callback ack
		}

		label := info.Label
		if label == "" {
			label = "(no label)"
		}

		_ = b.api.AnswerCallbackQuery(ctx, &telego.AnswerCallbackQueryParams{
			CallbackQueryID: q.ID, Text: "📌 Pinned " + info.Alias,
		})

		if msg, ok := q.Message.(*telego.Message); ok && msg != nil && msg.Text != "" {
			_, _ = b.api.EditMessageText(ctx, tu.EditMessageText(tu.ID(msg.Chat.ID), msg.MessageID,
				EscapeMarkdownV2(msg.Text)+"\n\n📌 Pinned "+MdCode(info.IDPrefix)+" \\["+MdCode(info.Alias)+"\\] "+EscapeMarkdownV2(label)).
				WithParseMode("MarkdownV2"))
		}
	case "kill":
		info, err := b.router.Evict(prefix)
		if err != nil {
			// nilerr: err is surfaced to the user via the callback ack toast; no
			// further handler-side error propagation is appropriate.
			_ = b.api.AnswerCallbackQuery(ctx, &telego.AnswerCallbackQueryParams{
				CallbackQueryID: q.ID, Text: "Evict failed: " + err.Error(),
			})

			return nil //nolint:nilerr // error already surfaced to user via callback ack
		}

		_ = b.api.AnswerCallbackQuery(ctx, &telego.AnswerCallbackQueryParams{
			CallbackQueryID: q.ID, Text: "❌ Evicted " + info.Alias,
		})

		if msg, ok := q.Message.(*telego.Message); ok && msg != nil && msg.Text != "" {
			_, _ = b.api.EditMessageText(ctx, tu.EditMessageText(tu.ID(msg.Chat.ID), msg.MessageID,
				EscapeMarkdownV2(msg.Text)+"\n\n❌ Evicted "+MdCode(info.IDPrefix)+" \\["+MdCode(info.Alias)+"\\]").
				WithParseMode("MarkdownV2"))
		}
	}

	return nil
}

// handleLabelCommand parses "/label <text>" and either dispatches the label
// change to the pinned/sole shim, or stashes the label and emits an inline
// picker when ambiguous. Empty text clears the label.
func (b *Bot) handleLabelCommand(ctx context.Context, msg telego.Message) {
	if b.router == nil {
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), "Session switcher is unavailable: no router wired\\.").WithParseMode("MarkdownV2"))
		return
	}

	rest := msg.Text
	if idx := strings.IndexAny(rest, " \t"); idx >= 0 {
		rest = strings.TrimSpace(rest[idx+1:])
	} else {
		rest = ""
	}

	label := rest

	shims := b.router.Snapshot()
	if len(shims) == 0 {
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), "No active CC sessions connected\\.").WithParseMode("MarkdownV2"))
		return
	}

	chatID := strconv.FormatInt(msg.Chat.ID, 10)

	if target, ok := b.pinnedShim(chatID, shims); ok {
		b.applyLabel(ctx, msg, target.IDPrefix, label)
		return
	}

	if len(shims) == 1 {
		b.applyLabel(ctx, msg, shims[0].IDPrefix, label)
		return
	}

	b.stashPendingLabel(chatID, label)
	b.sendLabelPicker(ctx, msg, label, shims)
}

func (b *Bot) pinnedShim(chatID string, shims []ShimInfo) (ShimInfo, bool) {
	for _, s := range shims {
		if slices.Contains(s.PinnedChats, chatID) {
			return s, true
		}
	}

	return ShimInfo{}, false
}

func (b *Bot) applyLabel(ctx context.Context, msg telego.Message, prefix, label string) {
	info, err := b.router.SetLabel(prefix, label)
	if err != nil {
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), "Set label failed: "+err.Error()))
		return
	}

	display := "\\(no label\\)"
	if label != "" {
		display = EscapeMarkdownV2(label)
	}

	_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID),
		"✅ "+MdCode(info.IDPrefix)+" \\["+MdCode(info.Alias)+"\\] → "+display).WithParseMode("MarkdownV2"))
}

func (b *Bot) stashPendingLabel(chatID, label string) {
	b.pendingLabelMu.Lock()
	defer b.pendingLabelMu.Unlock()

	if b.pendingLabel == nil {
		b.pendingLabel = map[string]pendingLabel{}
	}

	now := time.Now()
	for cid, pl := range b.pendingLabel {
		if now.After(pl.expiresAt) {
			delete(b.pendingLabel, cid)
		}
	}

	b.pendingLabel[chatID] = pendingLabel{text: label, expiresAt: now.Add(pendingLabelTTL)}
}

func (b *Bot) takePendingLabel(chatID string) (string, bool) {
	b.pendingLabelMu.Lock()
	defer b.pendingLabelMu.Unlock()

	pl, ok := b.pendingLabel[chatID]
	if !ok {
		return "", false
	}

	delete(b.pendingLabel, chatID)

	if time.Now().After(pl.expiresAt) {
		return "", false
	}

	return pl.text, true
}

func (b *Bot) sendLabelPicker(ctx context.Context, msg telego.Message, label string, shims []ShimInfo) {
	escapedDisplay := "\\(no label\\)"
	if label != "" {
		escapedDisplay = EscapeMarkdownV2(label)
	}

	rows := make([][]telego.InlineKeyboardButton, 0, len(shims))

	for _, s := range shims {
		shimLabel := s.Label
		if shimLabel == "" {
			shimLabel = "(no label)"
		}

		btn := tu.InlineKeyboardButton(fmt.Sprintf("%s [%s] %s", s.IDPrefix, s.Alias, shimLabel)).
			WithCallbackData("sess:label:" + s.IDPrefix)
		rows = append(rows, tu.InlineKeyboardRow(btn))
	}

	keyboard := tu.InlineKeyboard(rows...)
	_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID),
		"Which session should get label \\\""+escapedDisplay+"\\\"? \\(expires in "+EscapeMarkdownV2(pendingLabelTTL.String())+"\\)").
		WithReplyMarkup(keyboard).WithParseMode("MarkdownV2"))
}

// handleLabelCallback resolves a "sess:label:<prefix>" callback by looking up
// the stashed pending label for the chat and applying it. Caller must enforce
// the allowlist before reaching here, mirroring handleSessCallback.
func (b *Bot) handleLabelCallback(ctx context.Context, q telego.CallbackQuery, prefix string) error {
	chatID := strconv.FormatInt(q.From.ID, 10)
	if msg, ok := q.Message.(*telego.Message); ok && msg != nil {
		chatID = strconv.FormatInt(msg.Chat.ID, 10)
	}

	label, ok := b.takePendingLabel(chatID)
	if !ok {
		_ = b.api.AnswerCallbackQuery(ctx, &telego.AnswerCallbackQueryParams{
			CallbackQueryID: q.ID, Text: "Label expired — send /label <text> again.",
		})

		return nil
	}

	if b.router == nil {
		_ = b.api.AnswerCallbackQuery(ctx, &telego.AnswerCallbackQueryParams{
			CallbackQueryID: q.ID, Text: "Session switcher unavailable.",
		})

		return nil
	}

	info, err := b.router.SetLabel(prefix, label)
	if err != nil {
		_ = b.api.AnswerCallbackQuery(ctx, &telego.AnswerCallbackQueryParams{
			CallbackQueryID: q.ID, Text: "Set label failed: " + err.Error(),
		})

		return nil //nolint:nilerr // error already surfaced to user via callback ack
	}

	display := label
	if display == "" {
		display = "(no label)"
	}

	_ = b.api.AnswerCallbackQuery(ctx, &telego.AnswerCallbackQueryParams{
		CallbackQueryID: q.ID, Text: "✅ " + info.Alias + " → " + display,
	})

	if msg, ok := q.Message.(*telego.Message); ok && msg != nil && msg.Text != "" {
		_, _ = b.api.EditMessageText(ctx, tu.EditMessageText(tu.ID(msg.Chat.ID), msg.MessageID,
			EscapeMarkdownV2(msg.Text)+"\n\n✅ "+MdCode(info.IDPrefix)+" \\["+MdCode(info.Alias)+"\\] → "+EscapeMarkdownV2(display)).
			WithParseMode("MarkdownV2"))
	}

	return nil
}

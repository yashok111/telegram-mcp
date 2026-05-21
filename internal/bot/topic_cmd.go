package bot

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/yakov/telegram-mcp/internal/access"
)

// maxTopicNameRunes mirrors Telegram Bot API's editForumTopic 128-char
// limit. Pre-flight rejection gives a friendlier error than letting the
// API call fail with NAME_TOO_LONG (which surfaces as raw text via
// sendPlain).
const maxTopicNameRunes = 128

// TopicCloser is the high-level operation the bot delegates to when the user
// issues `/topic close` inside a forum topic. Implemented by the daemon (it
// owns the Router, SpawnRunner, and bot ref needed to kill a shim, close
// the Telegram topic, and update access.State.ClosedTopics).
type TopicCloser interface {
	CloseTopic(ctx context.Context, threadID int) error
}

// SetTopicCloser wires a TopicCloser. Nil disables `/topic close` (handler
// replies with an explanatory error). Production wires it in main.go.
func (b *Bot) SetTopicCloser(c TopicCloser) { b.topicCloser = c }

// handleTopicCommand is the entry for `/topic …` commands. It runs before
// the DM-only gate in handleCommand so it can reach commands sent inside a
// forum supergroup topic.
//
// Subcommands handled here:
//   - `/topic close` — kill the shim that owns this topic, close the topic,
//     schedule it for purge.
//
// All other subcommands (info / rename / list) are deferred to Wave 6.
func (b *Bot) handleTopicCommand(ctx context.Context, msg telego.Message) {
	st := b.store.Load()

	if !b.topicCommandGate(ctx, &msg, st) {
		return
	}

	args := topicArgs(msg.Text)
	if len(args) == 0 {
		b.handleTopicInfo(ctx, &msg)
		return
	}

	switch strings.ToLower(args[0]) {
	case "close":
		b.handleTopicClose(ctx, &msg)
	case "rename":
		b.handleTopicRename(ctx, &msg, strings.TrimSpace(strings.Join(args[1:], " ")))
	case "info":
		b.handleTopicInfo(ctx, &msg)
	default:
		b.replyTopicHelp(ctx, msg.Chat.ID, msg.MessageThreadID)
	}
}

// topicCommandGate ensures the message is in the configured forum chat,
// inside a real topic thread, and from an allowlisted user. Replies with a
// short error message and returns false when any predicate fails.
func (b *Bot) topicCommandGate(ctx context.Context, msg *telego.Message, st access.State) bool {
	if st.ForumChatID == 0 {
		b.sendPlain(ctx, msg.Chat.ID, msg.MessageThreadID, "Forum routing is disabled. Set TELEGRAM_FORUM_CHAT_ID first.")
		return false
	}

	if msg.Chat.ID != st.ForumChatID {
		b.sendPlain(ctx, msg.Chat.ID, msg.MessageThreadID, "This command only runs inside the configured forum supergroup.")
		return false
	}

	if msg.MessageThreadID == 0 {
		b.sendPlain(ctx, msg.Chat.ID, 0, "Use this command inside a topic, not the General thread.")
		return false
	}

	if msg.From == nil {
		return false
	}

	senderID := strconv.FormatInt(msg.From.ID, 10)
	for _, allow := range st.AllowFrom {
		if allow == senderID {
			return true
		}
	}

	slog.Warn("topic command denied: sender not allowlisted",
		"user_id", msg.From.ID, "chat_id", msg.Chat.ID, "thread_id", msg.MessageThreadID)
	b.sendPlain(ctx, msg.Chat.ID, msg.MessageThreadID, "Not authorized.")

	return false
}

func (b *Bot) handleTopicClose(ctx context.Context, msg *telego.Message) {
	if b.topicCloser == nil {
		b.sendPlain(ctx, msg.Chat.ID, msg.MessageThreadID, "/topic close is not wired in this build.")
		return
	}

	if err := b.topicCloser.CloseTopic(ctx, msg.MessageThreadID); err != nil {
		slog.Error("topic close failed", "thread_id", msg.MessageThreadID, "err", err)
		// Topic close may have already happened on Telegram's side but
		// the bookkeeping failed — fall back to the chat itself (not the
		// thread, which might be closed/deleted) for the error reply.
		b.sendPlain(ctx, msg.Chat.ID, 0, "Topic close failed: "+err.Error())

		return
	}

	slog.Info("topic closed via /topic close", "thread_id", msg.MessageThreadID, "chat_id", msg.Chat.ID, "user_id", msg.From.ID)
}

func (b *Bot) handleTopicInfo(ctx context.Context, msg *telego.Message) {
	if b.router == nil {
		b.sendPlain(ctx, msg.Chat.ID, msg.MessageThreadID, "Topic info unavailable: router not wired.")
		return
	}

	var owner *ShimInfo

	for _, s := range b.router.Snapshot() {
		if s.TopicID == msg.MessageThreadID {
			owner = &s
			break
		}
	}

	if owner == nil {
		b.sendPlain(ctx, msg.Chat.ID, msg.MessageThreadID, "Topic has no attached shim (orphan or pre-Wave-3 topic).")
		return
	}

	label := owner.Label
	if label == "" {
		label = "(no label)"
	}

	workdir := owner.Workdir
	if workdir == "" {
		workdir = "(unknown)"
	}

	text := fmt.Sprintf("Topic %d\nalias: @%s\nlabel: %s\nworkdir: %s\nconnected: %s",
		msg.MessageThreadID, owner.Alias, label, workdir, owner.ConnectedAt.UTC().Format(time.RFC3339))
	b.sendPlain(ctx, msg.Chat.ID, msg.MessageThreadID, text)
}

func (b *Bot) handleTopicRename(ctx context.Context, msg *telego.Message, newName string) {
	if newName == "" {
		b.sendPlain(ctx, msg.Chat.ID, msg.MessageThreadID, "Usage: /topic rename <new name>")
		return
	}

	if utf8.RuneCountInString(newName) > maxTopicNameRunes {
		b.sendPlain(ctx, msg.Chat.ID, msg.MessageThreadID,
			fmt.Sprintf("Topic name too long: %d chars (max %d).",
				utf8.RuneCountInString(newName), maxTopicNameRunes))
		return
	}

	if err := b.EditForumTopic(ctx, msg.Chat.ID, msg.MessageThreadID, newName); err != nil {
		slog.Error("topic rename failed", "thread_id", msg.MessageThreadID, "name", newName, "err", err)
		b.sendPlain(ctx, msg.Chat.ID, msg.MessageThreadID, "Rename failed: "+err.Error())

		return
	}

	b.sendPlain(ctx, msg.Chat.ID, msg.MessageThreadID, "Renamed.")
}

func (b *Bot) replyTopicHelp(ctx context.Context, chatID int64, threadID int) {
	b.sendPlain(ctx, chatID, threadID,
		"Usage:\n"+
			"  /topic — show this topic's info\n"+
			"  /topic close — close + schedule purge\n"+
			"  /topic rename <new name>\n"+
			"  /topics list — DM-only: list all topics")
}

// handleTopicsListCommand renders every known topic from access.State.
// DM-only — leaks internal topology, not for the supergroup. Format is
// monospaced text suitable for Markdown rendering.
func (b *Bot) handleTopicsListCommand(ctx context.Context, msg telego.Message) {
	if msg.Chat.Type != "private" {
		// Silent in groups so a /topics command doesn't leak diagnostics.
		return
	}

	if msg.From == nil {
		return
	}

	st := b.store.Load()
	senderID := strconv.FormatInt(msg.From.ID, 10)

	allowed := false
	for _, a := range st.AllowFrom {
		if a == senderID {
			allowed = true
			break
		}
	}

	if !allowed {
		return
	}

	if st.ForumChatID == 0 {
		b.sendPlain(ctx, msg.Chat.ID, 0, "Forum routing disabled.")
		return
	}

	if len(st.TopicsByThread) == 0 {
		b.sendPlain(ctx, msg.Chat.ID, 0, "No topics yet.")
		return
	}

	type row struct {
		threadID int
		label    string
		workdir  string
		locked   string
	}

	rows := make([]row, 0, len(st.TopicsByThread))
	for tidStr, m := range st.TopicsByThread {
		tid, err := strconv.Atoi(tidStr)
		if err != nil {
			continue
		}

		rows = append(rows, row{threadID: tid, label: m.Label, workdir: m.Workdir, locked: m.LockedBy})
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].threadID < rows[j].threadID })

	var sb strings.Builder

	fmt.Fprintf(&sb, "Topics (%d total, forum_chat=%d):\n\n", len(rows), st.ForumChatID)

	for _, r := range rows {
		label := r.label
		if label == "" {
			label = "-"
		}

		workdir := r.workdir
		if workdir == "" {
			workdir = "-"
		}

		state := "free"
		if r.locked != "" {
			state = "locked by " + r.locked
		}

		fmt.Fprintf(&sb, "%d · %s · %s · %s\n", r.threadID, label, workdir, state)
	}

	if len(st.ClosedTopics) > 0 {
		fmt.Fprintf(&sb, "\nClosed (pending purge): %d\n", len(st.ClosedTopics))
	}

	b.sendPlain(ctx, msg.Chat.ID, 0, sb.String())
}

// sendPlain is a tiny wrapper around b.api.SendMessage for short status
// replies. When threadID > 0 the reply lands in the same forum topic as
// the command (good UX); threadID = 0 keeps it in General — used only
// when the topic itself is no longer addressable (close failed, command
// issued outside any thread).
func (b *Bot) sendPlain(ctx context.Context, chatID int64, threadID int, text string) {
	p := tu.Message(tu.ID(chatID), text)
	if threadID > 0 {
		p = p.WithMessageThreadID(threadID)
	}

	if _, err := b.api.SendMessage(ctx, p); err != nil {
		slog.Warn("topic_cmd sendPlain failed", "chat_id", chatID, "thread_id", threadID, "err", err)
	}
}

// topicArgs splits "/topic close foo bar" → ["close", "foo", "bar"]. The
// leading token (the bare /topic) is dropped; trailing whitespace is
// trimmed. Returns nil for "/topic" alone.
func topicArgs(text string) []string {
	text = strings.TrimSpace(text)
	parts := strings.Fields(text)
	if len(parts) <= 1 {
		return nil
	}

	return parts[1:]
}

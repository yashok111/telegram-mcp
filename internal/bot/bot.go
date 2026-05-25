// Package bot wraps the Telegram long-poller, the gate decisions (pairing,
// allowlist, group mention), and the outbound Telegram API surface that the
// MCP layer calls into. Inbound messages that pass the gate are forwarded
// to Claude Code via the Notifier interface.
package bot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/yakov/telegram-mcp/internal/access"
)

// fileClient is shared across photo + attachment downloads. Timeout caps a
// stuck Telegram CDN connection so it can't wedge the poller indefinitely.
var fileClient = &http.Client{Timeout: 60 * time.Second}

// Notifier is the slice of the MCP server the bot calls into. Defined as an
// interface so bot tests can drop in a fake without pulling the MCP package.
type Notifier interface {
	DeliverInbound(content string, meta map[string]string)
	LookupPermission(requestID string) (PermissionDetails, bool)
	ResolvePermission(requestID, behavior string)
	// ResolveMutation applies (approve=true) or discards (approve=false) a
	// pending admin Tier-3 mutation identified by pendingID, on the owner's
	// gate-authenticated ✅/❌ tap. Returns applied=true only when a mutation
	// actually executed, plus a human detail for the owner. bot must not import
	// daemon — this is the seam, mirroring ResolvePermission.
	ResolveMutation(ctx context.Context, pendingID string, approve bool) (applied bool, detail string)
}

// PermissionDetails mirrors what mcp.Server stores for "See more" expansion.
type PermissionDetails struct {
	ToolName     string
	Description  string
	InputPreview string
}

type SendOpts struct {
	ReplyTo   int
	ParseMode string
	// Caption is rendered under photo/document uploads when non-empty.
	// Ignored by SendMessage and EditMessage.
	Caption string
	// MessageThreadID, when non-zero, scopes the outbound to a supergroup
	// forum topic (Telegram's message_thread_id). Telego omits the field
	// when zero, so DM/plain-supergroup callers leave it unset.
	MessageThreadID int
}

// Permission-reply spec from the TS plugin: 5 lowercase letters a-z minus 'l'.
// Case-insensitive for phone autocorrect. Strict — no bare yes/no, no chatter.
var permissionReplyRE = regexp.MustCompile(`(?i)^\s*(y|yes|n|no)\s+([a-km-z]{5})\s*$`)

const pairingCodeTTLMs = int64(time.Hour / time.Millisecond)

// mentionCacheCap bounds compiledMentionPattern's regex cache; typical usage
// is well under 10 patterns, the cap exists to defeat pathological growth.
const mentionCacheCap = 256

// Photo extensions go as sendPhoto (inline preview + compression); anything
// else goes as sendDocument (raw bytes).
var photoExts = map[string]struct{}{
	".jpg":  {},
	".jpeg": {},
	".png":  {},
	".gif":  {},
	".webp": {},
}

type Bot struct {
	api         *telego.Bot
	token       string
	username    string
	store       *access.Store
	notifier    Notifier
	router      RouterView
	bgRunner    BgRunner
	spawnRunner SpawnRunner
	topicCloser TopicCloser

	pollHandler *th.BotHandler
	stopOnce    sync.Once

	inboxOnce sync.Once
	inboxErr  error

	mentionMu sync.Mutex
	// Lives for daemon lifetime; patterns rarely change and access.json edits
	// typically follow a process restart. nil entries negative-cache invalid
	// regexes so a broken user pattern stops trying to compile. Bounded by
	// mentionCacheCap with FIFO eviction so a misbehaving caller rotating
	// patterns can't leak memory.
	mentionCache map[string]*regexp.Regexp
	mentionOrder []string

	pendingLabelMu sync.Mutex
	pendingLabel   map[string]pendingLabel
}

// SetBgRunner wires the background-task spawner. Must be called before Poll;
// nil-safe so tests and embeddings that don't use /bg can skip the call.
func (b *Bot) SetBgRunner(r BgRunner) { b.bgRunner = r }

// NewWithRouter is the production constructor: rv carries the active Router
// owned by the daemon process. Tests that pass nil for rv must avoid the
// session-switcher commands (renderShims / sendSessions / sendIdle).
func NewWithRouter(token string, store *access.Store, notifier Notifier, rv RouterView) (*Bot, error) {
	api, err := telego.NewBot(token, telego.WithDiscardLogger())
	if err != nil {
		return nil, err
	}

	return &Bot{api: api, token: token, store: store, notifier: notifier, router: rv}, nil
}

// NewFromAPIWithRouter is the test constructor: points telego at a custom API
// server URL (httptest) and accepts either a stubbed RouterView or nil for
// tests that don't exercise session-switcher commands.
func NewFromAPIWithRouter(token string, store *access.Store, notifier Notifier, apiURL string, rv RouterView) (*Bot, error) {
	opts := []telego.BotOption{telego.WithDiscardLogger()}
	if apiURL != "" {
		opts = append(opts, telego.WithAPIServer(apiURL))
	}

	api, err := telego.NewBot(token, opts...)
	if err != nil {
		return nil, err
	}

	return &Bot{api: api, token: token, store: store, notifier: notifier, router: rv}, nil
}

// Poll runs the long-poller until ctx is cancelled or an unrecoverable error
// occurs. Returns when the handler stops.
func (b *Bot) Poll(ctx context.Context) error {
	me, err := b.api.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("getMe: %w", err)
	}

	b.username = me.Username
	slog.Info("polling started", "username", b.username)

	updates, err := b.api.UpdatesViaLongPolling(ctx, nil)
	if err != nil {
		return fmt.Errorf("updates via long-polling: %w", err)
	}

	bh, err := th.NewBotHandler(b.api, updates)
	if err != nil {
		return fmt.Errorf("new bot handler: %w", err)
	}

	b.pollHandler = bh
	b.registerHandlers(bh)

	// Background: deliver any approval confirmations dropped by /telegram:access.
	go b.approvalLoop(ctx)

	// Fire-and-forget — best-effort, command list isn't load-bearing. 5s cap
	// keeps the goroutine from sitting forever against a wedged CDN.
	go func() {
		cmdCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		if err := b.api.SetMyCommands(cmdCtx, &telego.SetMyCommandsParams{
			Commands: []telego.BotCommand{
				{Command: "start", Description: "Welcome and setup guide"},
				{Command: "help", Description: "What this bot can do"},
				{Command: "status", Description: "Pairing + active sessions"},
				{Command: "sessions", Description: "Pick which CC session to talk to"},
				{Command: "use", Description: "/use <prefix> — pin a session"},
				{Command: "idle", Description: "Show the most idle session"},
				{Command: "rules", Description: "Manage auto-approve permission rules"},
				{Command: "label", Description: "/label <text> — set session label (empty clears)"},
				{Command: "reaction", Description: "/reaction <emoji>|off — set ack emoji on inbound (no args shows current)"},
				{Command: "bg", Description: "/bg <prompt> [--in <dir>] — fire-and-forget Claude run"},
				{Command: "spawn", Description: "/spawn [--in <dir>] — fork a Claude Code session owned by this daemon"},
				{Command: "effort", Description: "/effort <low|medium|high|xhigh|max>|show|clear — set per-chat model + thinking budget"},
				{Command: "topics", Description: "List forum topics with shim attachment + lock state (DM only)"},
			},
			Scope: &telego.BotCommandScopeAllPrivateChats{Type: "all_private_chats"},
		}); err != nil {
			slog.Warn("SetMyCommands failed", "err", err)
		}
	}()

	// bh.Start blocks; ctx-driven shutdown goes through StopWithContext.
	done := make(chan error, 1)
	go func() { done <- bh.Start() }()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		_ = bh.StopWithContext(shutCtx)

		select {
		case <-done:
		case <-shutCtx.Done():
			slog.Warn("bot handler did not stop within deadline, abandoning")
		}

		return nil
	case err := <-done:
		if err != nil {
			return fmt.Errorf("bot handler exited: %w", err)
		}

		return nil
	}
}

func (b *Bot) Stop() {
	b.stopOnce.Do(func() {
		if b.pollHandler != nil {
			_ = b.pollHandler.Stop()
		}
	})
}

func (b *Bot) registerHandlers(bh *th.BotHandler) {
	// Commands first — they need to win over the generic text handler.
	bh.HandleMessage(b.onCommand, th.AnyCommand())
	bh.HandleMessage(b.onMessage, messageContentPredicate())
	bh.HandleCallbackQuery(b.onCallback)
}

// messageContentPredicate matches only messages carrying user content, so
// service updates (forum topic created/edited/closed, chat joins) and
// content-less messages never reach onMessage. Telegram emits such updates for
// the daemon's own CreateForumTopic/EditForumTopic calls and the bot polls them
// back; left unfiltered they were delivered to Claude Code as empty inbound and
// (post-#71) could trigger an auto-spawn into a forum topic. The three
// predicates are OR-ed into one argument because HandleMessage AND-s multiple
// predicates. AnyMessageWithMedia covers all content types beyond text/caption
// (location, contact, poll, dice, …), so none are dropped.
func messageContentPredicate() th.Predicate {
	return th.Or(
		th.AnyMessageWithText(),
		th.AnyMessageWithCaption(),
		th.AnyMessageWithMedia(),
	)
}

// onCommand is the th.Context-flavoured entry that delegates to handleCommand;
// the split exists so tests can drive handleCommand with a plain context.Context.
func (b *Bot) onCommand(ctx *th.Context, msg telego.Message) error {
	return b.handleCommand(ctx, msg)
}

// handleCommand routes /start, /help, /status. DM-only — group commands would
// leak pairing codes and confirm bot presence in unapproved chats.
// routeForumCommand dispatches commands that are valid inside a forum
// supergroup topic. These run before the DM gate (their own gate is
// topicCommandGate). Returns true when it consumed the message.
func (b *Bot) routeForumCommand(ctx context.Context, msg telego.Message, cmd string) bool {
	switch {
	case cmd == "topic":
		b.handleTopicCommand(ctx, msg)
		return true
	case cmd == "spawn" && msg.Chat.Type == "supergroup":
		// /spawn from inside a forum topic (only supergroups have topics)
		// pins the spawn to that topic; the DM path in handleCommand still
		// handles /spawn in a private chat. Plain groups fall through and
		// drop silently rather than leaking a presence-confirming reply.
		b.handleSpawnInTopic(ctx, msg)
		return true
	default:
		return false
	}
}

func (b *Bot) handleCommand(ctx context.Context, msg telego.Message) error {
	cmd := commandName(msg.Text)

	// Forum-supergroup commands run inside a topic, not DM, so they branch
	// before the DM gate below.
	if b.routeForumCommand(ctx, msg, cmd) {
		return nil
	}

	if msg.Chat.Type != "private" || msg.From == nil {
		return nil
	}

	senderID := strconv.FormatInt(msg.From.ID, 10)

	st := b.store.Load()

	if st.DMPolicy == access.PolicyDisabled {
		return nil
	}

	if st.DMPolicy == access.PolicyAllowlist && !slices.Contains(st.AllowFrom, senderID) {
		return nil
	}

	slog.Info("inbound command", "cmd", cmd, "user_id", msg.From.ID, "chat_id", msg.Chat.ID, "user", userLabel(msg.From), "dm_policy", st.DMPolicy)

	switch cmd {
	case "start":
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID),
			"This bot bridges Telegram to a Claude Code session.\n\n"+
				"To pair:\n"+
				"1. DM me anything — you'll get a 6-char code\n"+
				"2. In Claude Code: /telegram:access pair <code>\n\n"+
				"After that, DMs here reach that session."))
	case "help":
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID),
			"Messages you send here route to a paired Claude Code session. "+
				"Text and photos are forwarded; replies and reactions come back.\n\n"+
				"/start — pairing instructions\n"+
				"/status — pairing + active sessions\n"+
				"/sessions — pick which CC session to talk to\n"+
				"/use <prefix> — pin a specific session\n"+
				"/idle — show the most idle session\n"+
				"/rules — list/clear/revoke auto-approve permission rules\n"+
				"/label <text> — set session label (empty clears)\n"+
				"/reaction <emoji>|off — set ack emoji on inbound (no args shows current)\n"+
				"/bg <prompt> [--in <dir>] — fire-and-forget Claude run; /bg list, /bg cancel <id>\n"+
				"/spawn [--in <dir>] — fork a daemon-owned Claude Code client; /spawn list, /spawn cancel <id>\n"+
				"/effort <low|medium|high|xhigh|max>|show|clear — per-chat model + thinking budget for new /spawn and /bg\n"+
				"/topics — list forum topics (DM only); /topic inside a topic for info/close/rename"))
	case "status":
		b.sendStatus(ctx, msg, st, senderID)
	case "sessions":
		b.sendSessions(ctx, msg)
	case "use":
		reply, _ := b.handleUseCommand(strconv.FormatInt(msg.Chat.ID, 10), msg.Text)
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), reply).WithParseMode("MarkdownV2"))
	case "idle":
		b.sendIdle(ctx, msg)
	case "rules":
		b.handleRulesCommand(ctx, msg, st)
	case "label":
		b.handleLabelCommand(ctx, msg)
	case "reaction":
		b.handleReactionCommand(ctx, msg, st)
	case "bg":
		b.handleBgCommand(ctx, msg, b.bgRunner)
	case "spawn":
		b.handleSpawnCommand(ctx, msg, b.spawnRunner)
	case "effort":
		b.handleEffortCommand(ctx, msg)
	case "topics":
		b.handleTopicsListCommand(ctx, msg)
	}

	return nil
}

func (b *Bot) sendStatus(ctx context.Context, msg telego.Message, st access.State, senderID string) {
	if slices.Contains(st.AllowFrom, senderID) {
		label := senderID
		if msg.From.Username != "" {
			label = "@" + msg.From.Username
		}

		effortLine := renderEffortLine(st, strconv.FormatInt(msg.Chat.ID, 10))
		text := fmt.Sprintf("Paired as %s\\.\n%s\n%s", EscapeMarkdownV2(label), effortLine, b.renderShims(time.Now()))
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), text).WithParseMode("MarkdownV2"))

		return
	}

	if code, _, ok := findPendingFor(st.Pending, senderID); ok {
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID),
			"Pending pairing — run in Claude Code:\n\n/telegram:access pair "+MdCode(code)).WithParseMode("MarkdownV2"))

		return
	}

	_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID),
		"Not paired. Send me a message to get a pairing code."))
}

func (b *Bot) onMessage(ctx *th.Context, msg telego.Message) error {
	return b.handleMessage(ctx, msg)
}

// handleMessage is the generic inbound path: gate → permission-reply intercept →
// attachment classification → DeliverInbound to Claude.
func (b *Bot) handleMessage(ctx context.Context, msg telego.Message) error {
	if msg.Text != "" && strings.HasPrefix(msg.Text, "/") {
		// Already routed by onCommand. Avoid double-handling.
		return nil
	}

	decision := b.gate(&msg)

	switch decision.action {
	case actionDrop:
		slog.Info("inbound dropped", "chat_id", msg.Chat.ID, "user_id", msg.From.ID, "kind", classifyMessageKind(&msg))
		return nil
	case actionPair:
		lead := "Pairing required"
		if decision.isResend {
			lead = "Still pending"
		}

		slog.Info("inbound pairing", "chat_id", msg.Chat.ID, "user_id", msg.From.ID, "is_resend", decision.isResend)

		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID),
			fmt.Sprintf("%s — run in Claude Code:\n\n/telegram:access pair %s", lead, MdCode(decision.code))).WithParseMode("MarkdownV2"))

		return nil
	case actionDeliver:
		// Fall through to the delivery path below.
	}

	st := decision.state
	text := messageContent(msg)
	chatID := strconv.FormatInt(msg.Chat.ID, 10)
	msgID := msg.MessageID

	// Permission-reply intercept: "yes xxxxx" / "no xxxxx" → resolve, no chat relay.
	if m := permissionReplyRE.FindStringSubmatch(text); m != nil {
		behavior := "deny"
		emoji := "❌"

		if strings.HasPrefix(strings.ToLower(m[1]), "y") {
			behavior = "allow"
			emoji = "✅"
		}

		slog.Info("inbound permission-reply", "request_id", strings.ToLower(m[2]), "behavior", behavior, "chat_id", chatID, "user_id", msg.From.ID)

		b.notifier.ResolvePermission(strings.ToLower(m[2]), behavior)
		_ = b.setReaction(ctx, chatID, msgID, emoji)

		return nil
	}

	// Typing indicator — best-effort, expires after ~5s on Telegram's side.
	// The daemon's TypingTracker refreshes this while the agent is still working.
	_ = b.SendChatAction(ctx, chatID, "typing")

	if st.AckReaction != "" {
		_ = b.setReaction(ctx, chatID, msgID, st.AckReaction)
	}

	meta := map[string]string{
		"chat_id":    chatID,
		"message_id": strconv.Itoa(msgID),
		"user":       userLabel(msg.From),
		"user_id":    strconv.FormatInt(msg.From.ID, 10),
		"ts":         time.Unix(msg.Date, 0).UTC().Format(time.RFC3339),
	}

	if msg.MessageThreadID != 0 {
		meta["message_thread_id"] = strconv.Itoa(msg.MessageThreadID)
	}

	maps.Copy(meta, replyToMeta(&msg))

	switch {
	case msg.Photo != nil:
		// Deferred download — only spend API calls + disk on gate-approved senders.
		if path, err := b.downloadPhoto(ctx, msg.Photo); err == nil && path != "" {
			meta["image_path"] = path
		} else if err != nil {
			slog.Error("photo download failed", "err", err)
		}
	default:
		maps.Copy(meta, attachmentMeta(&msg))
	}

	slog.Info("inbound delivered",
		"chat_id", chatID,
		"message_id", msgID,
		"user", userLabel(msg.From),
		"kind", classifyMessageKind(&msg),
		"text_len", len(text),
	)

	b.notifier.DeliverInbound(text, meta)

	return nil
}

// attachmentMeta extracts attachment_kind / file_id / size / mime / name for
// non-photo media. Returns nil if the message carries no attachment.
// classifyMessageKind names the inbound payload shape for telemetry/diagnostics.
func classifyMessageKind(msg *telego.Message) string {
	switch {
	case msg.Photo != nil:
		return "photo"
	case msg.Voice != nil:
		return "voice"
	case msg.VideoNote != nil:
		return "video_note"
	case msg.Video != nil:
		return "video"
	case msg.Audio != nil:
		return "audio"
	case msg.Document != nil:
		return "document"
	case msg.Sticker != nil:
		return "sticker"
	case msg.Animation != nil:
		return "animation"
	case msg.Text != "":
		return "text"
	default:
		return "other"
	}
}

// replyToMeta extracts cited-message context when the inbound is a quote-reply.
// Returns an empty (never nil) map when there is no reply, so callers can
// unconditionally maps.Copy. Quote is only honored alongside a real
// ReplyToMessage — Telegram never sets Quote without one, but the guard keeps
// the rendered <channel> tag coherent if that ever changes.
func replyToMeta(msg *telego.Message) map[string]string {
	out := map[string]string{}
	if msg.ReplyToMessage == nil || msg.ReplyToMessage.MessageID == 0 {
		return out
	}

	r := msg.ReplyToMessage
	out["reply_to_message_id"] = strconv.Itoa(r.MessageID)

	switch {
	case r.Text != "":
		out["reply_to_text"] = r.Text
	case r.Caption != "":
		out["reply_to_text"] = r.Caption
	}

	if from := senderLabel(r.From, r.SenderChat); from != "" {
		out["reply_to_from"] = from
	}

	if msg.Quote != nil && msg.Quote.Text != "" {
		out["reply_to_quote"] = msg.Quote.Text
	}

	return out
}

func attachmentMeta(msg *telego.Message) map[string]string {
	put := func(kind, fileID string, size int64, mime, name string) map[string]string {
		m := map[string]string{
			"attachment_kind":    kind,
			"attachment_file_id": fileID,
		}
		if size > 0 {
			m["attachment_size"] = strconv.FormatInt(size, 10)
		}

		if mime != "" {
			m["attachment_mime"] = mime
		}

		if n := safeName(name); n != "" {
			m["attachment_name"] = n
		}

		return m
	}

	switch {
	case msg.Document != nil:
		d := msg.Document
		return put("document", d.FileID, d.FileSize, d.MimeType, d.FileName)
	case msg.Voice != nil:
		v := msg.Voice
		return put("voice", v.FileID, v.FileSize, v.MimeType, "")
	case msg.Audio != nil:
		a := msg.Audio
		return put("audio", a.FileID, a.FileSize, a.MimeType, a.FileName)
	case msg.Video != nil:
		v := msg.Video
		return put("video", v.FileID, v.FileSize, v.MimeType, v.FileName)
	case msg.VideoNote != nil:
		v := msg.VideoNote
		return put("video_note", v.FileID, int64(v.FileSize), "", "")
	case msg.Sticker != nil:
		s := msg.Sticker
		return put("sticker", s.FileID, int64(s.FileSize), "", "")
	}

	return nil
}

func messageContent(msg telego.Message) string {
	if msg.Text != "" {
		return msg.Text
	}

	if msg.Caption != "" {
		return msg.Caption
	}

	switch {
	case msg.Photo != nil:
		return "(photo)"
	case msg.Document != nil:
		name := safeName(msg.Document.FileName)
		if name == "" {
			name = "file"
		}

		return fmt.Sprintf("(document: %s)", name)
	case msg.Voice != nil:
		return "(voice message)"
	case msg.Audio != nil:
		return "(audio)"
	case msg.Video != nil:
		return "(video)"
	case msg.VideoNote != nil:
		return "(video note)"
	case msg.Sticker != nil:
		if msg.Sticker.Emoji != "" {
			return fmt.Sprintf("(sticker %s)", msg.Sticker.Emoji)
		}

		return "(sticker)"
	}

	return ""
}

func (b *Bot) downloadPhoto(ctx context.Context, sizes []telego.PhotoSize) (string, error) {
	if len(sizes) == 0 {
		return "", nil
	}

	best := sizes[len(sizes)-1]

	return b.fetchFile(ctx, best.FileID, best.FileUniqueID)
}

// ensureInboxDir runs os.MkdirAll once per Bot lifetime. The directory rarely
// changes and the syscall isn't free on every photo/attachment download.
func (b *Bot) ensureInboxDir() error {
	b.inboxOnce.Do(func() {
		b.inboxErr = os.MkdirAll(b.store.InboxDir(), 0o700)
	})

	return b.inboxErr
}

// fetchFile resolves a file_id via getFile then downloads the bytes into
// inbox/. Returns the on-disk path. uniqueHint becomes part of the filename
// so multiple downloads of the same file don't collide.
func (b *Bot) fetchFile(ctx context.Context, fileID, uniqueHint string) (string, error) {
	file, err := b.api.GetFile(ctx, &telego.GetFileParams{FileID: fileID})
	if err != nil {
		return "", err
	}

	if file.FilePath == "" {
		return "", errors.New("telegram returned empty file_path — file may have expired")
	}

	url := fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", b.token, file.FilePath)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}

	res, err := fileClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch: %w", err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusOK {
		// Drain so the underlying connection can be reused from the pool.
		_, _ = io.Copy(io.Discard, res.Body)
		return "", fmt.Errorf("download failed: HTTP %d", res.StatusCode)
	}

	ext := safeExt(file.FilePath)
	if ext == "" {
		ext = "bin"
	}

	unique := sanitizeID(uniqueHint)
	if unique == "" {
		unique = "dl"
	}

	if err := b.ensureInboxDir(); err != nil {
		return "", err
	}

	path := filepath.Join(b.store.InboxDir(), fmt.Sprintf("%d-%s.%s", time.Now().UnixMilli(), unique, ext))

	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	if _, err := io.Copy(f, res.Body); err != nil {
		_ = os.Remove(path)
		return "", err
	}

	return path, nil
}

// onCallback handles permission inline-button clicks ("perm:allow:xxxxx",
// "perm:deny:xxxxx", "perm:more:xxxxx"). Allowlist check mirrors the
// text-reply path so unpaired group members can't decide permissions.
var callbackRE = regexp.MustCompile(`^perm:(allow|deny|more|atool1h|atoolall|dtoolall):([a-km-z]{5})$`)

// amCallbackRE matches admin-mutation confirm taps: "am:<pending_id>:confirm"
// / "am:<pending_id>:cancel". pending_id is hex from PendingStore; the {1,64}
// bound caps length (Telegram callback_data is ≤64 bytes anyway) so oversized
// data never reaches the store.
var amCallbackRE = regexp.MustCompile(`^am:([0-9a-f]{1,64}):(confirm|cancel)$`)

func (b *Bot) onCallback(ctx *th.Context, q telego.CallbackQuery) error {
	return b.handleCallback(ctx, q)
}

// authorizeCallback enforces the allowlist on a callback query. Logs+answers
// "Not authorized." and returns false on denial so the caller can early-return.
func (b *Bot) authorizeCallback(ctx context.Context, q *telego.CallbackQuery, st access.State) bool {
	senderID := strconv.FormatInt(q.From.ID, 10)
	if slices.Contains(st.AllowFrom, senderID) {
		return true
	}

	slog.Warn("callback denied: sender not allowlisted", "user_id", q.From.ID, "data", q.Data)
	_ = b.api.AnswerCallbackQuery(ctx, &telego.AnswerCallbackQueryParams{
		CallbackQueryID: q.ID, Text: "Not authorized.",
	})

	return false
}

// isOwnerID reports whether userID is the owner — the first allowlist entry
// that parses as an int64, matching the single-user assumption the daemon's
// ownerTarget / pickPermissionTarget use. Admin-mutation confirms are
// owner-only even though other allowlisted users may answer permission prompts.
func isOwnerID(allowFrom []string, userID int64) bool {
	for _, raw := range allowFrom {
		// First POSITIVE id is the owner (matches the daemon's
		// firstParseableChatID): a group id is negative and never the owner.
		if id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil && id > 0 {
			return id == userID
		}
	}

	return false
}

func (b *Bot) handleCallback(ctx context.Context, q telego.CallbackQuery) error {
	st := b.store.Load()

	if lm := labelCallbackRE.FindStringSubmatch(q.Data); lm != nil {
		if !b.authorizeCallback(ctx, &q, st) {
			return nil
		}

		return b.handleLabelCallback(ctx, q, lm[1])
	}

	if sm := sessCallbackRE.FindStringSubmatch(q.Data); sm != nil {
		if !b.authorizeCallback(ctx, &q, st) {
			return nil
		}

		return b.handleSessCallback(ctx, q, sm[1], sm[2])
	}

	if am := amCallbackRE.FindStringSubmatch(q.Data); am != nil {
		if !b.authorizeCallback(ctx, &q, st) {
			return nil
		}

		// Admin mutations are owner-only: unlike permission prompts (any
		// allowlisted user may answer), a config change confirmed to the owner's
		// DM must not be approvable by a second allowlisted user.
		if !isOwnerID(st.AllowFrom, q.From.ID) {
			slog.Warn("admin confirm denied: tapper is not the owner", "user_id", q.From.ID)
			_ = b.api.AnswerCallbackQuery(ctx, &telego.AnswerCallbackQueryParams{
				CallbackQueryID: q.ID, Text: "Owner only.",
			})

			return nil
		}

		return b.handleMutationCallback(ctx, q, am[1], am[2] == "confirm")
	}

	m := callbackRE.FindStringSubmatch(q.Data)
	if m == nil {
		slog.Info("callback unknown pattern", "data", q.Data, "user_id", q.From.ID)
		_ = b.api.AnswerCallbackQuery(ctx, &telego.AnswerCallbackQueryParams{CallbackQueryID: q.ID})

		return nil
	}

	if !b.authorizeCallback(ctx, &q, st) {
		return nil
	}

	behavior, requestID := m[1], m[2]

	slog.Info("callback received", "behavior", behavior, "request_id", requestID, "user_id", q.From.ID)

	if behavior == "more" {
		return b.handleMoreCallback(ctx, q, requestID)
	}

	switch behavior {
	case "atool1h":
		b.addRuleAndResolve(ctx, &q, requestID, access.RuleApprove, time.Hour)
		return nil
	case "atoolall":
		b.addRuleAndResolve(ctx, &q, requestID, access.RuleApprove, 0)
		return nil
	case "dtoolall":
		b.addRuleAndResolve(ctx, &q, requestID, access.RuleDeny, 0)
		return nil
	}

	b.notifier.ResolvePermission(requestID, behavior)

	label := "✅ Allowed"
	if behavior == "deny" {
		label = "❌ Denied"
	}

	_ = b.api.AnswerCallbackQuery(ctx, &telego.AnswerCallbackQueryParams{
		CallbackQueryID: q.ID,
		Text:            label,
	})
	// Replace the buttons with the outcome so the same prompt can't be answered twice.
	if msg, ok := q.Message.(*telego.Message); ok && msg != nil && msg.Text != "" {
		_, _ = b.api.EditMessageText(ctx, tu.EditMessageText(tu.ID(msg.Chat.ID), msg.MessageID,
			msg.Text+"\n\n"+label))
	}

	return nil
}

// handleMoreCallback expands a permission prompt with the full tool details
// when the user taps "ℹ See more", then restores the Allow/Deny buttons so the
// request can still be answered. Authorization already ran in handleCallback.
func (b *Bot) handleMoreCallback(ctx context.Context, q telego.CallbackQuery, requestID string) error {
	details, ok := b.notifier.LookupPermission(requestID)
	if !ok {
		_ = b.api.AnswerCallbackQuery(ctx, &telego.AnswerCallbackQueryParams{
			CallbackQueryID: q.ID,
			Text:            "Details no longer available.",
		})

		return nil
	}

	expanded := fmt.Sprintf("🔐 Permission: %s\n\ntool_name: %s\ndescription: %s\ninput_preview:\n%s",
		details.ToolName, details.ToolName, details.Description, details.InputPreview)

	keyboard := tu.InlineKeyboard(tu.InlineKeyboardRow(
		tu.InlineKeyboardButton("✅ Allow").WithCallbackData("perm:allow:"+requestID),
		tu.InlineKeyboardButton("❌ Deny").WithCallbackData("perm:deny:"+requestID),
	))
	if msg, ok := q.Message.(*telego.Message); ok && msg != nil {
		_, _ = b.api.EditMessageText(ctx, tu.EditMessageText(tu.ID(msg.Chat.ID), msg.MessageID, expanded).
			WithReplyMarkup(keyboard))
	}

	_ = b.api.AnswerCallbackQuery(ctx, &telego.AnswerCallbackQueryParams{CallbackQueryID: q.ID})

	return nil
}

// handleMutationCallback resolves an admin Tier-3 confirm tap. The tapper was
// already gate-authenticated in handleCallback. It calls ResolveMutation (which
// applies or discards daemon-side), answers the toast, and rewrites the prompt
// with the outcome — dropping the buttons so the same mutation can't resolve
// twice (the daemon's PendingStore.Take also enforces at-most-once).
func (b *Bot) handleMutationCallback(ctx context.Context, q telego.CallbackQuery, pendingID string, approve bool) error {
	applied, detail := b.notifier.ResolveMutation(ctx, pendingID, approve)

	slog.Info("admin mutation callback", "pending_id", pendingID, "approve", approve, "applied", applied, "user_id", q.From.ID)

	toast, outcome := mutationOutcomeLabels(approve, applied, detail)

	_ = b.api.AnswerCallbackQuery(ctx, &telego.AnswerCallbackQueryParams{
		CallbackQueryID: q.ID,
		Text:            toast,
	})

	if msg, ok := q.Message.(*telego.Message); ok && msg != nil && msg.Text != "" {
		_, _ = b.api.EditMessageText(ctx, tu.EditMessageText(tu.ID(msg.Chat.ID), msg.MessageID, msg.Text+"\n\n"+outcome))
	}

	return nil
}

// mutationOutcomeLabels renders the short callback toast and the longer message
// suffix for a resolved admin mutation.
func mutationOutcomeLabels(approve, applied bool, detail string) (toast, outcome string) {
	switch {
	case !approve:
		return "🚫 Cancelled", "🚫 " + detail
	case applied:
		return "✅ Applied", "✅ Applied: " + detail
	default:
		return "⚠️ Not applied", "⚠️ " + detail
	}
}

func (b *Bot) addRuleAndResolve(ctx context.Context, q *telego.CallbackQuery, requestID string, action access.RuleAction, ttl time.Duration) {
	details, ok := b.notifier.LookupPermission(requestID)
	if !ok || strings.TrimSpace(details.ToolName) == "" {
		slog.Warn("addRule refused: missing tool name", "request_id", requestID, "lookup_ok", ok)

		_ = b.api.AnswerCallbackQuery(ctx, &telego.AnswerCallbackQueryParams{
			CallbackQueryID: q.ID,
			Text:            "⚠️ Can't create rule: tool name missing",
			ShowAlert:       true,
		})

		return
	}

	toolName := details.ToolName

	rule := access.PermissionRule{Tool: toolName, Action: action}
	if ttl > 0 {
		rule.ExpiresAt = time.Now().Add(ttl).UnixMilli()
	}

	if err := b.store.Mutate(func(st *access.State) bool {
		access.AddRule(st, rule)
		return true
	}); err != nil {
		slog.Error("addRule save failed", "request_id", requestID, "tool", toolName, "err", err)

		_ = b.api.AnswerCallbackQuery(ctx, &telego.AnswerCallbackQueryParams{
			CallbackQueryID: q.ID,
			Text:            "⚠️ Rule save failed: " + err.Error(),
			ShowAlert:       true,
		})

		return
	}

	behavior := "allow"
	if action == access.RuleDeny {
		behavior = "deny"
	}

	b.notifier.ResolvePermission(requestID, behavior)

	label := fmt.Sprintf("✅ Allowed %s (rule added)", toolName)
	if action == access.RuleDeny {
		label = fmt.Sprintf("❌ Denied %s (rule added)", toolName)
	}

	_ = b.api.AnswerCallbackQuery(ctx, &telego.AnswerCallbackQueryParams{
		CallbackQueryID: q.ID, Text: label,
	})

	if msg, ok := q.Message.(*telego.Message); ok && msg != nil && msg.Text != "" {
		_, _ = b.api.EditMessageText(ctx, tu.EditMessageText(tu.ID(msg.Chat.ID), msg.MessageID,
			msg.Text+"\n\n"+label))
	}
}

type gateAction int

const (
	actionDrop gateAction = iota
	actionDeliver
	actionPair
)

type gateResult struct {
	action   gateAction
	code     string
	isResend bool
	state    access.State
}

func (b *Bot) gate(msg *telego.Message) gateResult {
	st := b.store.Load()

	if st.DMPolicy == access.PolicyDisabled || msg.From == nil {
		return gateResult{action: actionDrop}
	}

	senderID := strconv.FormatInt(msg.From.ID, 10)

	switch msg.Chat.Type {
	case "private":
		if slices.Contains(st.AllowFrom, senderID) {
			return gateResult{action: actionDeliver, state: st}
		}

		if st.DMPolicy == access.PolicyAllowlist {
			return gateResult{action: actionDrop}
		}

		if code, p, ok := findPendingFor(st.Pending, senderID); ok {
			if p.Replies >= 2 {
				return gateResult{action: actionDrop}
			}

			p.Replies++
			st.Pending[code] = p
			_ = b.store.Save(st)

			return gateResult{action: actionPair, code: code, isResend: true}
		}

		if len(st.Pending) >= 3 {
			return gateResult{action: actionDrop}
		}

		code := access.NewPairingCode()
		now := time.Now().UnixMilli()
		st.Pending[code] = access.Pending{
			SenderID:  senderID,
			ChatID:    strconv.FormatInt(msg.Chat.ID, 10),
			CreatedAt: now,
			ExpiresAt: now + pairingCodeTTLMs,
			Replies:   1,
		}
		_ = b.store.Save(st)

		return gateResult{action: actionPair, code: code, isResend: false}

	case "group", "supergroup":
		groupID := strconv.FormatInt(msg.Chat.ID, 10)

		policy, ok := st.Groups[groupID]
		if !ok {
			return gateResult{action: actionDrop}
		}

		if len(policy.AllowFrom) > 0 && !slices.Contains(policy.AllowFrom, senderID) {
			return gateResult{action: actionDrop}
		}

		if policy.RequireMention && !b.isMentioned(msg, st.MentionPatterns) {
			return gateResult{action: actionDrop}
		}

		return gateResult{action: actionDeliver, state: st}
	}

	return gateResult{action: actionDrop}
}

func (b *Bot) isMentioned(msg *telego.Message, extraPatterns []string) bool {
	text := msg.Text
	if text == "" {
		text = msg.Caption
	}

	entities := msg.Entities
	if len(entities) == 0 {
		entities = msg.CaptionEntities
	}

	for _, e := range entities {
		switch e.Type {
		case "mention":
			if e.Offset+e.Length <= len(text) {
				mentioned := text[e.Offset : e.Offset+e.Length]
				if strings.EqualFold(mentioned, "@"+b.username) {
					return true
				}
			}
		case "text_mention":
			if e.User != nil && e.User.IsBot && e.User.Username == b.username {
				return true
			}
		}
	}
	// Replying to one of our messages counts as an implicit mention.
	if msg.ReplyToMessage != nil && msg.ReplyToMessage.From != nil && msg.ReplyToMessage.From.Username == b.username {
		return true
	}

	for _, pat := range extraPatterns {
		// Invalid user-supplied patterns are silently skipped — mirrors TS behavior.
		re := b.compiledMentionPattern(pat)
		if re == nil {
			continue
		}

		if re.MatchString(text) {
			return true
		}
	}

	return false
}

func (b *Bot) compiledMentionPattern(pat string) *regexp.Regexp {
	b.mentionMu.Lock()
	defer b.mentionMu.Unlock()

	if b.mentionCache == nil {
		b.mentionCache = map[string]*regexp.Regexp{}
	}

	if re, ok := b.mentionCache[pat]; ok {
		return re
	}

	re, err := regexp.Compile("(?i)" + pat)
	if err != nil {
		re = nil
	}

	if len(b.mentionCache) >= mentionCacheCap && len(b.mentionOrder) > 0 {
		evict := b.mentionOrder[0]
		b.mentionOrder = b.mentionOrder[1:]

		delete(b.mentionCache, evict)
	}

	b.mentionCache[pat] = re
	b.mentionOrder = append(b.mentionOrder, pat)

	return re
}

func (b *Bot) approvalLoop(ctx context.Context) {
	// 30s is plenty for a manual pairing flow — the user runs
	// /telegram:access on their terminal, walks to their phone, and looks
	// at the bot. Sub-second latency just spins the daemon.
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.checkApprovals(ctx)
			b.purgeExpiredPendingLabels()
		}
	}
}

// purgeExpiredPendingLabels drops timed-out label-picker stashes. Without
// this, chats that opened a label picker and walked away leave entries in
// pendingLabel until the next /label invocation cleans them out.
func (b *Bot) purgeExpiredPendingLabels() {
	b.pendingLabelMu.Lock()
	defer b.pendingLabelMu.Unlock()

	now := time.Now()
	for cid, pl := range b.pendingLabel {
		if now.After(pl.expiresAt) {
			delete(b.pendingLabel, cid)
		}
	}
}

func (b *Bot) checkApprovals(ctx context.Context) {
	entries, err := os.ReadDir(b.store.ApprovedDir())
	if err != nil {
		return
	}

	for _, e := range entries {
		senderID := e.Name()

		id, err := strconv.ParseInt(senderID, 10, 64)
		if err != nil {
			_ = os.Remove(filepath.Join(b.store.ApprovedDir(), senderID))
			continue
		}

		if _, err := b.api.SendMessage(ctx, tu.Message(tu.ID(id), "Paired! Say hi to Claude.")); err != nil {
			slog.Error("send approval confirm failed", "sender_id", senderID, "err", err)
		}

		_ = os.Remove(filepath.Join(b.store.ApprovedDir(), senderID))
	}
}

// SendMessage / SendPhoto / SendDocument / EditMessage / React — the outbound
// surface the MCP layer calls into.

func (b *Bot) SendMessage(ctx context.Context, chatID, text string, opts SendOpts) (int, error) {
	id, err := parseChatID(chatID)
	if err != nil {
		slog.Warn("SendMessage bad chat_id", "chat_id", chatID, "err", err)
		return 0, err
	}

	p := tu.Message(tu.ID(id), text)
	if opts.ReplyTo > 0 {
		p = p.WithReplyParameters(&telego.ReplyParameters{MessageID: opts.ReplyTo})
	}

	if opts.ParseMode != "" {
		p = p.WithParseMode(opts.ParseMode)
	}

	if opts.MessageThreadID > 0 {
		p = p.WithMessageThreadID(opts.MessageThreadID)
	}

	m, err := b.api.SendMessage(ctx, p)
	if err != nil {
		slog.Error("Telegram sendMessage failed", "chat_id", id, "text_len", len(text), "reply_to", opts.ReplyTo, "parse_mode", opts.ParseMode, "err", err)
		return 0, err
	}

	slog.Info("Telegram sendMessage ok", "chat_id", id, "message_id", m.MessageID, "text_len", len(text), "reply_to", opts.ReplyTo)

	return m.MessageID, nil
}

func (b *Bot) SendFile(ctx context.Context, chatID, path string, opts SendOpts) (int, error) {
	id, err := parseChatID(chatID)
	if err != nil {
		return 0, err
	}

	f, err := os.Open(path)
	if err != nil {
		slog.Warn("SendFile open failed", "path", path, "err", err)
		return 0, err
	}
	defer func() { _ = f.Close() }()

	ext := strings.ToLower(filepath.Ext(path))
	if _, isPhoto := photoExts[ext]; isPhoto {
		p := tu.Photo(tu.ID(id), tu.File(f))
		if opts.ReplyTo > 0 {
			p = p.WithReplyParameters(&telego.ReplyParameters{MessageID: opts.ReplyTo})
		}

		if opts.Caption != "" {
			p = p.WithCaption(opts.Caption)
		}

		if opts.MessageThreadID > 0 {
			p = p.WithMessageThreadID(opts.MessageThreadID)
		}

		m, err := b.api.SendPhoto(ctx, p)
		if err != nil {
			slog.Error("Telegram sendPhoto failed", "chat_id", id, "path", path, "err", err)
			return 0, err
		}

		slog.Info("Telegram sendPhoto ok", "chat_id", id, "message_id", m.MessageID, "path", path)

		return m.MessageID, nil
	}

	p := tu.Document(tu.ID(id), tu.File(f))
	if opts.ReplyTo > 0 {
		p = p.WithReplyParameters(&telego.ReplyParameters{MessageID: opts.ReplyTo})
	}

	if opts.Caption != "" {
		p = p.WithCaption(opts.Caption)
	}

	if opts.MessageThreadID > 0 {
		p = p.WithMessageThreadID(opts.MessageThreadID)
	}

	m, err := b.api.SendDocument(ctx, p)
	if err != nil {
		slog.Error("Telegram sendDocument failed", "chat_id", id, "path", path, "err", err)
		return 0, err
	}

	slog.Info("Telegram sendDocument ok", "chat_id", id, "message_id", m.MessageID, "path", path)

	return m.MessageID, nil
}

func (b *Bot) EditMessage(ctx context.Context, chatID string, messageID int, text, parseMode string) (int, error) {
	id, err := parseChatID(chatID)
	if err != nil {
		return 0, err
	}

	p := tu.EditMessageText(tu.ID(id), messageID, text)
	if parseMode != "" {
		p = p.WithParseMode(parseMode)
	}

	m, err := b.api.EditMessageText(ctx, p)
	if err != nil {
		slog.Error("Telegram editMessageText failed", "chat_id", id, "message_id", messageID, "err", err)
		return 0, err
	}

	if m == nil {
		slog.Info("Telegram editMessageText ok (no message)", "chat_id", id, "message_id", messageID)
		return messageID, nil
	}

	slog.Info("Telegram editMessageText ok", "chat_id", id, "message_id", m.MessageID)

	return m.MessageID, nil
}

func (b *Bot) React(ctx context.Context, chatID string, messageID int, emoji string) error {
	return b.setReaction(ctx, chatID, messageID, emoji)
}

// SendChatAction posts a transient activity indicator (e.g. "typing") to the
// chat. Telegram displays it for roughly 5s, so the daemon's TypingTracker
// refreshes it on a 4s cadence while a shim is still composing a response.
func (b *Bot) SendChatAction(ctx context.Context, chatID, action string) error {
	id, err := parseChatID(chatID)
	if err != nil {
		return fmt.Errorf("parse chat id: %w", err)
	}

	return b.api.SendChatAction(ctx, &telego.SendChatActionParams{
		ChatID: tu.ID(id),
		Action: action,
	})
}

func (b *Bot) setReaction(ctx context.Context, chatID string, messageID int, emoji string) error {
	id, err := parseChatID(chatID)
	if err != nil {
		return fmt.Errorf("parse chat id: %w", err)
	}

	return b.api.SetMessageReaction(ctx, &telego.SetMessageReactionParams{
		ChatID:    tu.ID(id),
		MessageID: messageID,
		Reaction:  []telego.ReactionType{&telego.ReactionTypeEmoji{Type: "emoji", Emoji: emoji}},
	})
}

// DownloadFile resolves a Telegram file_id, downloads it to inbox/, and returns
// the on-disk path. Called by the MCP download_attachment tool.
func (b *Bot) DownloadFile(ctx context.Context, fileID string) (string, error) {
	return b.fetchFile(ctx, fileID, fileID)
}

// CreateForumTopic creates a new forum topic in supergroup chatID and returns
// the new thread_id. Requires can_manage_topics on the bot in that chat. A
// zero iconColor lets Telegram pick the default; non-zero must be one of the
// six preset hex colors documented in the Bot API.
func (b *Bot) CreateForumTopic(ctx context.Context, chatID int64, name string, iconColor int) (int, error) {
	p := &telego.CreateForumTopicParams{
		ChatID: tu.ID(chatID),
		Name:   name,
	}
	if iconColor != 0 {
		p.IconColor = iconColor
	}

	topic, err := b.api.CreateForumTopic(ctx, p)
	if err != nil {
		slog.Error("Telegram createForumTopic failed", "chat_id", chatID, "name", name, "err", err)
		return 0, fmt.Errorf("create forum topic: %w", err)
	}

	slog.Info("Telegram createForumTopic ok", "chat_id", chatID, "name", name, "thread_id", topic.MessageThreadID)

	return topic.MessageThreadID, nil
}

// EditForumTopic renames an existing topic. Icon left unchanged.
func (b *Bot) EditForumTopic(ctx context.Context, chatID int64, threadID int, name string) error {
	if err := b.api.EditForumTopic(ctx, &telego.EditForumTopicParams{
		ChatID:          tu.ID(chatID),
		MessageThreadID: threadID,
		Name:            name,
	}); err != nil {
		if isTopicNotModified(err) {
			slog.Debug("Telegram editForumTopic: title already current (idempotent)",
				"chat_id", chatID, "thread_id", threadID, "name", name)

			return nil
		}

		slog.Error("Telegram editForumTopic failed", "chat_id", chatID, "thread_id", threadID, "name", name, "err", err)

		return fmt.Errorf("edit forum topic: %w", err)
	}

	slog.Info("Telegram editForumTopic ok", "chat_id", chatID, "thread_id", threadID, "name", name)

	return nil
}

// isTopicNotModified reports whether err is Telegram's 400 TOPIC_NOT_MODIFIED,
// which editForumTopic returns when the requested title already equals the
// current one. There is no telego sentinel for this code, so we match on the
// API description carried in the error string — that text is Telegram's
// stable contract for the condition.
func isTopicNotModified(err error) bool {
	return err != nil && strings.Contains(err.Error(), "TOPIC_NOT_MODIFIED")
}

// CloseForumTopic makes a topic read-only without deleting history.
func (b *Bot) CloseForumTopic(ctx context.Context, chatID int64, threadID int) error {
	if err := b.api.CloseForumTopic(ctx, &telego.CloseForumTopicParams{
		ChatID:          tu.ID(chatID),
		MessageThreadID: threadID,
	}); err != nil {
		slog.Error("Telegram closeForumTopic failed", "chat_id", chatID, "thread_id", threadID, "err", err)
		return fmt.Errorf("close forum topic: %w", err)
	}

	slog.Info("Telegram closeForumTopic ok", "chat_id", chatID, "thread_id", threadID)

	return nil
}

// DeleteForumTopic permanently removes a topic and all its messages.
func (b *Bot) DeleteForumTopic(ctx context.Context, chatID int64, threadID int) error {
	if err := b.api.DeleteForumTopic(ctx, &telego.DeleteForumTopicParams{
		ChatID:          tu.ID(chatID),
		MessageThreadID: threadID,
	}); err != nil {
		slog.Error("Telegram deleteForumTopic failed", "chat_id", chatID, "thread_id", threadID, "err", err)
		return fmt.Errorf("delete forum topic: %w", err)
	}

	slog.Info("Telegram deleteForumTopic ok", "chat_id", chatID, "thread_id", threadID)

	return nil
}

// PermissionTarget identifies the single chat (and optional supergroup forum
// topic) that should receive a permission prompt. The daemon's permission
// handler picks this via pickPermissionTarget; ThreadID=0 means a plain DM
// or non-forum chat. ChatID=0 disables sending (treated as no-op).
type PermissionTarget struct {
	ChatID   int64
	ThreadID int
}

// SendPermissionPrompt posts a single inline-keyboard permission prompt to
// target. Called by the daemon after picking the recipient (single-user DM
// in non-forum mode; topic-scoped supergroup message once forum routing is
// wired). The prefix is prepended verbatim (e.g. "@s1: ") so the recipient
// can tell which session asked; pass "" to disable.
//
// A zero ChatID is logged and skipped, not an error — callers in degraded
// states (empty allowlist, no forum target available) can call this
// unconditionally without guarding.
func (b *Bot) SendPermissionPrompt(ctx context.Context, target PermissionTarget, prefix, requestID, toolName string) {
	if target.ChatID == 0 {
		slog.Warn("SendPermissionPrompt skipped: zero target chat", "request_id", requestID, "tool", toolName)
		return
	}

	text := prefix + "🔐 Permission: " + toolName
	keyboard := tu.InlineKeyboard(
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton("✅ Allow").WithCallbackData("perm:allow:"+requestID),
			tu.InlineKeyboardButton("❌ Deny").WithCallbackData("perm:deny:"+requestID),
			tu.InlineKeyboardButton("ℹ See more").WithCallbackData("perm:more:"+requestID),
		),
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton("⏳ Allow "+toolName+" 1h").WithCallbackData("perm:atool1h:"+requestID),
		),
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton("♾ Always allow "+toolName).WithCallbackData("perm:atoolall:"+requestID),
			tu.InlineKeyboardButton("🚫 Always deny "+toolName).WithCallbackData("perm:dtoolall:"+requestID),
		),
	)

	p := tu.Message(tu.ID(target.ChatID), text).WithReplyMarkup(keyboard)
	if target.ThreadID > 0 {
		p = p.WithMessageThreadID(target.ThreadID)
	}

	if _, err := b.api.SendMessage(ctx, p); err != nil {
		slog.Error("permission_request send failed", "chat_id", target.ChatID, "thread_id", target.ThreadID, "err", err)
	}
}

// BroadcastMutationConfirm posts an admin-mutation confirmation prompt with
// ✅ Confirm / ❌ Cancel inline buttons to target, returning the sent
// message_id so the daemon can edit it on resolve/expiry. summary is the full
// human-readable description of the pending mutation (tool + resolved args) so
// the owner is never blind-tapping a pending_id. Rendered as plain text (no
// parse mode) so a user-controlled summary fragment can't inject markdown.
//
// Callback data is "am:<pendingID>:confirm" / "am:<pendingID>:cancel"; the
// owner tap is gate-authenticated in handleCallback before it resolves.
func (b *Bot) BroadcastMutationConfirm(ctx context.Context, target PermissionTarget, pendingID, summary string) (int, error) {
	if target.ChatID == 0 {
		return 0, errors.New("no target chat for mutation confirm")
	}

	text := "🛠 Admin action awaiting your approval:\n\n" + summary

	keyboard := tu.InlineKeyboard(tu.InlineKeyboardRow(
		tu.InlineKeyboardButton("✅ Confirm").WithCallbackData("am:"+pendingID+":confirm"),
		tu.InlineKeyboardButton("❌ Cancel").WithCallbackData("am:"+pendingID+":cancel"),
	))

	p := tu.Message(tu.ID(target.ChatID), text).WithReplyMarkup(keyboard)
	if target.ThreadID > 0 {
		p = p.WithMessageThreadID(target.ThreadID)
	}

	m, err := b.api.SendMessage(ctx, p)
	if err != nil {
		slog.Error("mutation confirm send failed", "chat_id", target.ChatID, "thread_id", target.ThreadID, "err", err)
		return 0, fmt.Errorf("send mutation confirm: %w", err)
	}

	slog.Info("mutation confirm sent", "chat_id", target.ChatID, "message_id", m.MessageID, "pending_id", pendingID)

	return m.MessageID, nil
}

// groupAnonymousBotID is Telegram's well-known stand-in user that authors all
// anonymous-admin group messages. For those messages SenderChat carries the
// real identity (the group), so senderLabel routes through to it instead of
// surfacing the literal "@GroupAnonymousBot" tag.
const groupAnonymousBotID int64 = 1087968824

// senderLabel resolves a message's display source. Falls back to SenderChat
// when From is nil, empty, or is the GroupAnonymousBot stand-in — covers
// channel posts (no From, SenderChat names the channel) and anonymous group
// admins (From is GroupAnonymousBot, SenderChat is the group). Without this
// fallback reply_to_from would silently disappear or be uselessly tagged.
func senderLabel(u *telego.User, c *telego.Chat) string {
	if u != nil && u.ID != groupAnonymousBotID {
		if label := userLabel(u); label != "" {
			return label
		}
	}

	if c == nil {
		return ""
	}

	if c.Username != "" {
		return "@" + c.Username
	}

	if c.Title != "" {
		return c.Title
	}

	return strconv.FormatInt(c.ID, 10)
}

func userLabel(u *telego.User) string {
	if u == nil {
		return ""
	}

	if u.Username != "" {
		return "@" + u.Username
	}

	return strconv.FormatInt(u.ID, 10)
}

func parseChatID(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}

// findPendingFor returns the live pairing code for senderID, skipping any
// entry whose ExpiresAt has elapsed. The ExpiresAt filter is load-bearing —
// PruneExpired runs on the daemon's RulesCleanup ticker now (no longer inline
// in the per-message hot path), so callers will see stale entries on disk
// until the next tick.
func findPendingFor(pending map[string]access.Pending, senderID string) (string, access.Pending, bool) {
	now := time.Now().UnixMilli()
	for code, p := range pending {
		if p.SenderID == senderID && now < p.ExpiresAt {
			return code, p, true
		}
	}

	return "", access.Pending{}, false
}

func commandName(text string) string {
	if !strings.HasPrefix(text, "/") {
		return ""
	}

	rest := text[1:]

	end := strings.IndexAny(rest, " @")
	if end < 0 {
		return rest
	}

	return rest[:end]
}

// safeName sanitizes uploader-controlled filenames/titles. They land inside a
// <channel> notification — delimiter chars could let a sender break out of the
// tag or forge a second meta entry.
func safeName(s string) string {
	if s == "" {
		return ""
	}

	rep := strings.NewReplacer("<", "_", ">", "_", "[", "_", "]", "_", "\r", "_", "\n", "_", ";", "_")

	return rep.Replace(s)
}

func safeExt(filePath string) string {
	ext := strings.TrimPrefix(filepath.Ext(filePath), ".")

	out := make([]byte, 0, len(ext))
	for i := range len(ext) {
		c := ext[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			out = append(out, c)
		}
	}

	return string(out)
}

func sanitizeID(s string) string {
	out := make([]byte, 0, len(s))
	for i := range len(s) {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' {
			out = append(out, c)
		}
	}

	return string(out)
}

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
}

// Permission-reply spec from the TS plugin: 5 lowercase letters a-z minus 'l'.
// Case-insensitive for phone autocorrect. Strict — no bare yes/no, no chatter.
var permissionReplyRE = regexp.MustCompile(`(?i)^\s*(y|yes|n|no)\s+([a-km-z]{5})\s*$`)

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
	api      *telego.Bot
	token    string
	username string
	store    *access.Store
	notifier Notifier
	router   RouterView

	pollHandler *th.BotHandler
	stopOnce    sync.Once

	mentionMu sync.Mutex
	// Lives for daemon lifetime; patterns rarely change and access.json edits
	// typically follow a process restart. nil entries negative-cache invalid
	// regexes so a broken user pattern stops trying to compile.
	mentionCache map[string]*regexp.Regexp
}

// NewWithRouter is the production constructor: rv carries the active Router
// owned by the daemon process. Tests that pass nil for rv must avoid the
// session-switcher commands (renderShims / sendSessions / sendIdle).
func NewWithRouter(token string, store *access.Store, notifier Notifier, rv RouterView) (*Bot, error) {
	api, err := telego.NewBot(token, telego.WithDefaultDebugLogger())
	if err != nil {
		return nil, err
	}

	return &Bot{api: api, token: token, store: store, notifier: notifier, router: rv}, nil
}

// NewFromAPIWithRouter is the test constructor: points telego at a custom API
// server URL (httptest) and accepts either a stubbed RouterView or nil for
// tests that don't exercise session-switcher commands.
func NewFromAPIWithRouter(token string, store *access.Store, notifier Notifier, apiURL string, rv RouterView) (*Bot, error) {
	opts := []telego.BotOption{telego.WithDefaultDebugLogger()}
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
		return err
	}

	bh, err := th.NewBotHandler(b.api, updates)
	if err != nil {
		return err
	}

	b.pollHandler = bh
	b.registerHandlers(bh)

	// Background: deliver any approval confirmations dropped by /telegram:access.
	go b.approvalLoop(ctx)

	// Fire-and-forget — best-effort, command list isn't load-bearing.
	go func() {
		_ = b.api.SetMyCommands(ctx, &telego.SetMyCommandsParams{
			Commands: []telego.BotCommand{
				{Command: "start", Description: "Welcome and setup guide"},
				{Command: "help", Description: "What this bot can do"},
				{Command: "status", Description: "Pairing + active sessions"},
				{Command: "sessions", Description: "Pick which CC session to talk to"},
				{Command: "use", Description: "/use <prefix> — pin a session"},
				{Command: "idle", Description: "Show the most idle session"},
				{Command: "rules", Description: "Manage auto-approve permission rules"},
			},
			Scope: &telego.BotCommandScopeAllPrivateChats{Type: "all_private_chats"},
		})
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
		return err
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
	bh.HandleMessage(b.onMessage, th.AnyMessage())
	bh.HandleCallbackQuery(b.onCallback)
}

// onCommand is the th.Context-flavoured entry that delegates to handleCommand;
// the split exists so tests can drive handleCommand with a plain context.Context.
func (b *Bot) onCommand(ctx *th.Context, msg telego.Message) error {
	return b.handleCommand(ctx, msg)
}

// handleCommand routes /start, /help, /status. DM-only — group commands would
// leak pairing codes and confirm bot presence in unapproved chats.
func (b *Bot) handleCommand(ctx context.Context, msg telego.Message) error {
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

	cmd := commandName(msg.Text)

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
				"/rules — list/clear/revoke auto-approve permission rules"))
	case "status":
		b.sendStatus(ctx, msg, st, senderID)
	case "sessions":
		b.sendSessions(ctx, msg)
	case "use":
		reply, _ := b.handleUseCommand(strconv.FormatInt(msg.Chat.ID, 10), msg.Text)
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), reply))
	case "idle":
		b.sendIdle(ctx, msg)
	case "rules":
		b.handleRulesCommand(ctx, msg, st)
	}

	return nil
}

func (b *Bot) sendStatus(ctx context.Context, msg telego.Message, st access.State, senderID string) {
	if slices.Contains(st.AllowFrom, senderID) {
		label := senderID
		if msg.From.Username != "" {
			label = "@" + msg.From.Username
		}

		text := fmt.Sprintf("Paired as %s.\n\n%s", label, b.renderShims(time.Now()))
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), text))

		return
	}

	if code, _, ok := findPendingFor(st.Pending, senderID); ok {
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID),
			"Pending pairing — run in Claude Code:\n\n/telegram:access pair "+code))

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
			fmt.Sprintf("%s — run in Claude Code:\n\n/telegram:access pair %s", lead, decision.code)))

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
	_ = b.api.SendChatAction(ctx, &telego.SendChatActionParams{
		ChatID: tu.ID(msg.Chat.ID),
		Action: "typing",
	})

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

	if msg.ReplyToMessage != nil && msg.ReplyToMessage.MessageID != 0 {
		meta["reply_to_message_id"] = strconv.Itoa(msg.ReplyToMessage.MessageID)
	}

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

	if err := os.MkdirAll(b.store.InboxDir(), 0o700); err != nil {
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

func (b *Bot) onCallback(ctx *th.Context, q telego.CallbackQuery) error {
	return b.handleCallback(ctx, q)
}

func (b *Bot) handleCallback(ctx context.Context, q telego.CallbackQuery) error {
	st := b.store.Load()
	senderID := strconv.FormatInt(q.From.ID, 10)

	if sm := sessCallbackRE.FindStringSubmatch(q.Data); sm != nil {
		if !slices.Contains(st.AllowFrom, senderID) {
			slog.Warn("sess callback denied: sender not allowlisted", "user_id", q.From.ID, "data", q.Data)
			_ = b.api.AnswerCallbackQuery(ctx, &telego.AnswerCallbackQueryParams{
				CallbackQueryID: q.ID, Text: "Not authorized.",
			})

			return nil
		}

		return b.handleSessCallback(ctx, q, sm[1], sm[2])
	}

	m := callbackRE.FindStringSubmatch(q.Data)
	if m == nil {
		slog.Info("callback unknown pattern", "data", q.Data, "user_id", q.From.ID)
		_ = b.api.AnswerCallbackQuery(ctx, &telego.AnswerCallbackQueryParams{CallbackQueryID: q.ID})

		return nil
	}

	if !slices.Contains(st.AllowFrom, senderID) {
		slog.Warn("callback denied: sender not allowlisted", "user_id", q.From.ID, "data", q.Data)
		_ = b.api.AnswerCallbackQuery(ctx, &telego.AnswerCallbackQueryParams{
			CallbackQueryID: q.ID,
			Text:            "Not authorized.",
		})

		return nil
	}

	behavior, requestID := m[1], m[2]

	slog.Info("callback received", "behavior", behavior, "request_id", requestID, "user_id", q.From.ID)

	if behavior == "more" {
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

func (b *Bot) addRuleAndResolve(ctx context.Context, q *telego.CallbackQuery, requestID string, action access.RuleAction, ttl time.Duration) {
	details, ok := b.notifier.LookupPermission(requestID)
	toolName := "*"
	if ok && details.ToolName != "" {
		toolName = details.ToolName
	}

	rule := access.PermissionRule{Tool: toolName, Action: action}
	if ttl > 0 {
		rule.ExpiresAt = time.Now().Add(ttl).UnixMilli()
	}

	if err := b.store.Mutate(func(st *access.State) bool {
		access.AddRule(st, rule)
		return true
	}); err != nil {
		slog.Error("addRule save failed", "request_id", requestID, "tool", toolName, "err", err)
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
			ExpiresAt: now + 60*60*1000,
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
		b.mentionCache[pat] = nil
		return nil
	}

	b.mentionCache[pat] = re

	return re
}

func (b *Bot) approvalLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.checkApprovals(ctx)
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

func (b *Bot) setReaction(ctx context.Context, chatID string, messageID int, emoji string) error {
	id, err := parseChatID(chatID)
	if err != nil {
		return err
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

// BroadcastPermissionRequest fans an inline-keyboard prompt to every
// allowlisted DM. Called by mcp.Server.SendPermissionRequest.
func (b *Bot) BroadcastPermissionRequest(ctx context.Context, requestID, toolName string) {
	st := b.store.Load()
	text := "🔐 Permission: " + toolName
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

	for _, dm := range st.AllowFrom {
		id, err := parseChatID(dm)
		if err != nil {
			continue
		}

		if _, err := b.api.SendMessage(ctx, tu.Message(tu.ID(id), text).WithReplyMarkup(keyboard)); err != nil {
			slog.Error("permission_request send failed", "chat_id", dm, "err", err)
		}
	}
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

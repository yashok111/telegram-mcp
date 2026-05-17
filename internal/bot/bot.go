// Package bot wraps the Telegram long-poller, the gate decisions (pairing,
// allowlist, group mention), and the outbound Telegram API surface that the
// MCP layer calls into. Inbound messages that pass the gate are forwarded
// to Claude Code via the Notifier interface.
package bot

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/yakov/telegram-mcp/internal/access"
)

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

	pollHandler *th.BotHandler
	stopOnce    sync.Once
}

func New(token string, store *access.Store, notifier Notifier) (*Bot, error) {
	api, err := telego.NewBot(token, telego.WithDefaultDebugLogger())
	if err != nil {
		return nil, err
	}
	return &Bot{api: api, token: token, store: store, notifier: notifier}, nil
}

// Poll runs the long-poller until ctx is cancelled or an unrecoverable error
// occurs. Returns when the handler stops.
func (b *Bot) Poll(ctx context.Context) error {
	me, err := b.api.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("getMe: %w", err)
	}
	b.username = me.Username
	fmt.Fprintf(os.Stderr, "telegram-mcp: polling as @%s\n", b.username)

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
				{Command: "status", Description: "Check your pairing status"},
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
		<-done
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

// onCommand routes /start, /help, /status. DM-only — group commands would
// leak pairing codes and confirm bot presence in unapproved chats.
func (b *Bot) onCommand(ctx *th.Context, msg telego.Message) error {
	if msg.Chat.Type != "private" || msg.From == nil {
		return nil
	}
	senderID := strconv.FormatInt(msg.From.ID, 10)
	st := b.store.Load()
	if access.PruneExpired(&st) {
		_ = b.store.Save(st)
	}
	if st.DMPolicy == access.PolicyDisabled {
		return nil
	}
	if st.DMPolicy == access.PolicyAllowlist && !containsString(st.AllowFrom, senderID) {
		return nil
	}

	cmd := commandName(msg.Text)
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
				"/status — check your pairing state"))
	case "status":
		b.sendStatus(ctx, msg, st, senderID)
	}
	return nil
}

func (b *Bot) sendStatus(ctx context.Context, msg telego.Message, st access.State, senderID string) {
	if containsString(st.AllowFrom, senderID) {
		label := senderID
		if msg.From.Username != "" {
			label = "@" + msg.From.Username
		}
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), fmt.Sprintf("Paired as %s.", label)))
		return
	}
	for code, p := range st.Pending {
		if p.SenderID == senderID {
			_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID),
				fmt.Sprintf("Pending pairing — run in Claude Code:\n\n/telegram:access pair %s", code)))
			return
		}
	}
	_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID),
		"Not paired. Send me a message to get a pairing code."))
}

// onMessage is the generic inbound path: gate → permission-reply intercept →
// attachment classification → DeliverInbound to Claude.
func (b *Bot) onMessage(ctx *th.Context, msg telego.Message) error {
	if msg.Text != "" && strings.HasPrefix(msg.Text, "/") {
		// Already routed by onCommand. Avoid double-handling.
		return nil
	}

	decision := b.gate(&msg)
	switch decision.action {
	case actionDrop:
		return nil
	case actionPair:
		lead := "Pairing required"
		if decision.isResend {
			lead = "Still pending"
		}
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID),
			fmt.Sprintf("%s — run in Claude Code:\n\n/telegram:access pair %s", lead, decision.code)))
		return nil
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

	switch {
	case msg.Photo != nil:
		// Deferred download — only spend API calls + disk on gate-approved senders.
		if path, err := b.downloadPhoto(ctx, msg.Photo); err == nil && path != "" {
			meta["image_path"] = path
		} else if err != nil {
			fmt.Fprintf(os.Stderr, "telegram-mcp: photo download failed: %v\n", err)
		}
	case msg.Document != nil:
		meta["attachment_kind"] = "document"
		meta["attachment_file_id"] = msg.Document.FileID
		if msg.Document.FileSize > 0 {
			meta["attachment_size"] = strconv.FormatInt(msg.Document.FileSize, 10)
		}
		if msg.Document.MimeType != "" {
			meta["attachment_mime"] = msg.Document.MimeType
		}
		if n := safeName(msg.Document.FileName); n != "" {
			meta["attachment_name"] = n
		}
	case msg.Voice != nil:
		meta["attachment_kind"] = "voice"
		meta["attachment_file_id"] = msg.Voice.FileID
		if msg.Voice.FileSize > 0 {
			meta["attachment_size"] = strconv.FormatInt(msg.Voice.FileSize, 10)
		}
		if msg.Voice.MimeType != "" {
			meta["attachment_mime"] = msg.Voice.MimeType
		}
	case msg.Audio != nil:
		meta["attachment_kind"] = "audio"
		meta["attachment_file_id"] = msg.Audio.FileID
		if msg.Audio.FileSize > 0 {
			meta["attachment_size"] = strconv.FormatInt(msg.Audio.FileSize, 10)
		}
		if msg.Audio.MimeType != "" {
			meta["attachment_mime"] = msg.Audio.MimeType
		}
		if n := safeName(msg.Audio.FileName); n != "" {
			meta["attachment_name"] = n
		}
	case msg.Video != nil:
		meta["attachment_kind"] = "video"
		meta["attachment_file_id"] = msg.Video.FileID
		if msg.Video.FileSize > 0 {
			meta["attachment_size"] = strconv.FormatInt(msg.Video.FileSize, 10)
		}
		if msg.Video.MimeType != "" {
			meta["attachment_mime"] = msg.Video.MimeType
		}
		if n := safeName(msg.Video.FileName); n != "" {
			meta["attachment_name"] = n
		}
	case msg.VideoNote != nil:
		meta["attachment_kind"] = "video_note"
		meta["attachment_file_id"] = msg.VideoNote.FileID
		if msg.VideoNote.FileSize > 0 {
			meta["attachment_size"] = strconv.Itoa(msg.VideoNote.FileSize)
		}
	case msg.Sticker != nil:
		meta["attachment_kind"] = "sticker"
		meta["attachment_file_id"] = msg.Sticker.FileID
		if msg.Sticker.FileSize > 0 {
			meta["attachment_size"] = strconv.Itoa(msg.Sticker.FileSize)
		}
	}

	b.notifier.DeliverInbound(text, meta)
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
		return "", fmt.Errorf("telegram returned empty file_path — file may have expired")
	}

	url := fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", b.token, file.FilePath)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
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
	defer f.Close()
	if _, err := io.Copy(f, res.Body); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

// onCallback handles permission inline-button clicks ("perm:allow:xxxxx",
// "perm:deny:xxxxx", "perm:more:xxxxx"). Allowlist check mirrors the
// text-reply path so unpaired group members can't decide permissions.
var callbackRE = regexp.MustCompile(`^perm:(allow|deny|more):([a-km-z]{5})$`)

func (b *Bot) onCallback(ctx *th.Context, q telego.CallbackQuery) error {
	m := callbackRE.FindStringSubmatch(q.Data)
	if m == nil {
		_ = b.api.AnswerCallbackQuery(ctx, &telego.AnswerCallbackQueryParams{CallbackQueryID: q.ID})
		return nil
	}
	st := b.store.Load()
	senderID := strconv.FormatInt(q.From.ID, 10)
	if !containsString(st.AllowFrom, senderID) {
		_ = b.api.AnswerCallbackQuery(ctx, &telego.AnswerCallbackQueryParams{
			CallbackQueryID: q.ID,
			Text:            "Not authorized.",
		})
		return nil
	}
	behavior, requestID := m[1], m[2]

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
	if access.PruneExpired(&st) {
		_ = b.store.Save(st)
	}
	if st.DMPolicy == access.PolicyDisabled || msg.From == nil {
		return gateResult{action: actionDrop}
	}
	senderID := strconv.FormatInt(msg.From.ID, 10)

	switch msg.Chat.Type {
	case "private":
		if containsString(st.AllowFrom, senderID) {
			return gateResult{action: actionDeliver, state: st}
		}
		if st.DMPolicy == access.PolicyAllowlist {
			return gateResult{action: actionDrop}
		}
		for code, p := range st.Pending {
			if p.SenderID == senderID {
				if p.Replies >= 2 {
					return gateResult{action: actionDrop}
				}
				p.Replies++
				st.Pending[code] = p
				_ = b.store.Save(st)
				return gateResult{action: actionPair, code: code, isResend: true}
			}
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
		if len(policy.AllowFrom) > 0 && !containsString(policy.AllowFrom, senderID) {
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
	entities := msg.Entities
	text := msg.Text
	if len(entities) == 0 {
		entities = msg.CaptionEntities
		text = msg.Caption
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
		re, err := regexp.Compile("(?i)" + pat)
		if err != nil {
			continue
		}
		if re.MatchString(text) {
			return true
		}
	}
	return false
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
		_, err = b.api.SendMessage(ctx, tu.Message(tu.ID(id), "Paired! Say hi to Claude."))
		if err != nil {
			fmt.Fprintf(os.Stderr, "telegram-mcp: failed to send approval confirm: %v\n", err)
		}
		_ = os.Remove(filepath.Join(b.store.ApprovedDir(), senderID))
	}
}

// SendMessage / SendPhoto / SendDocument / EditMessage / React — the outbound
// surface the MCP layer calls into.

func (b *Bot) SendMessage(ctx context.Context, chatID, text string, opts SendOpts) (int, error) {
	id, err := parseChatID(chatID)
	if err != nil {
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
		return 0, err
	}
	return m.MessageID, nil
}

func (b *Bot) SendFile(ctx context.Context, chatID, path string, opts SendOpts) (int, error) {
	id, err := parseChatID(chatID)
	if err != nil {
		return 0, err
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	ext := strings.ToLower(filepath.Ext(path))
	if _, isPhoto := photoExts[ext]; isPhoto {
		p := tu.Photo(tu.ID(id), tu.File(f))
		if opts.ReplyTo > 0 {
			p = p.WithReplyParameters(&telego.ReplyParameters{MessageID: opts.ReplyTo})
		}
		m, err := b.api.SendPhoto(ctx, p)
		if err != nil {
			return 0, err
		}
		return m.MessageID, nil
	}
	p := tu.Document(tu.ID(id), tu.File(f))
	if opts.ReplyTo > 0 {
		p = p.WithReplyParameters(&telego.ReplyParameters{MessageID: opts.ReplyTo})
	}
	m, err := b.api.SendDocument(ctx, p)
	if err != nil {
		return 0, err
	}
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
		return 0, err
	}
	if m == nil {
		return messageID, nil
	}
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
	text := fmt.Sprintf("🔐 Permission: %s", toolName)
	keyboard := tu.InlineKeyboard(tu.InlineKeyboardRow(
		tu.InlineKeyboardButton("See more").WithCallbackData("perm:more:"+requestID),
		tu.InlineKeyboardButton("✅ Allow").WithCallbackData("perm:allow:"+requestID),
		tu.InlineKeyboardButton("❌ Deny").WithCallbackData("perm:deny:"+requestID),
	))
	for _, dm := range st.AllowFrom {
		id, err := parseChatID(dm)
		if err != nil {
			continue
		}
		_, err = b.api.SendMessage(ctx, tu.Message(tu.ID(id), text).WithReplyMarkup(keyboard))
		if err != nil {
			fmt.Fprintf(os.Stderr, "telegram-mcp: permission_request send to %s failed: %v\n", dm, err)
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

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func parseChatID(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
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
	for i := 0; i < len(ext); i++ {
		c := ext[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			out = append(out, c)
		}
	}
	return string(out)
}

func sanitizeID(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' {
			out = append(out, c)
		}
	}
	return string(out)
}

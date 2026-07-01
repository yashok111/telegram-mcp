package bot

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strconv"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

// ErrAskIDInUse signals that a question id collided with one already in flight
// at the daemon. It lives in bot (not shim) so the mcp layer — which imports
// bot but not shim/ipc — can errors.Is against it and regenerate the qid. The
// shim's BotAdapter.BroadcastAsk maps the IPC collision code to this sentinel.
var ErrAskIDInUse = errors.New("ask question_id already in use")

// askCallbackRE matches an `ask` answer tap: "ask:<qid>:<idx>". qid is the
// short question id (same [a-km-z]{5} alphabet as permission, `l` excluded to
// dodge 1/l ambiguity); idx is the 0-based option index (0..99, one or two
// digits — the option count is capped well under 100). Option text lives
// server-side, never in callback_data, so the payload stays under Telegram's
// 64-byte limit.
var askCallbackRE = regexp.MustCompile(`^ask:([a-km-z]{5}):(\d{1,2})$`)

// SendAskPrompt posts a multiple-choice question with one inline-keyboard
// button per option to target. Mirrors SendPermissionPrompt: prefix is
// prepended verbatim (e.g. "@s1: "), the body is rendered as PLAIN TEXT (no
// parse mode) so arbitrary agent-authored question text never needs MarkdownV2
// escaping, and a zero ChatID is skipped with a warn rather than erroring.
//
// Callback data is "ask:<qid>:<idx>"; the tap is gate-authenticated in
// handleCallback before it resolves. One button per row keeps long option
// labels readable.
func (b *Bot) SendAskPrompt(ctx context.Context, target PermissionTarget, prefix, qid, question string, options []string) {
	if target.ChatID == 0 {
		slog.Warn("SendAskPrompt skipped: zero target chat", "qid", qid)
		return
	}

	// Callback data carries the option index, and askCallbackRE bounds it to
	// two digits — an index ≥ maxRenderableOptions would render a button whose
	// tap never matches and so can never resolve. The mcp layer already caps
	// options well below this (askDefaultMaxOptions=10); this is defence in
	// depth so a bad caller degrades to a truncated-but-usable keyboard, not a
	// silently dead button.
	const maxRenderableOptions = 100
	if len(options) > maxRenderableOptions {
		slog.Warn("ask options truncated to renderable limit", "qid", qid, "got", len(options), "limit", maxRenderableOptions)
		options = options[:maxRenderableOptions]
	}

	rows := make([][]telego.InlineKeyboardButton, 0, len(options))
	for i, opt := range options {
		rows = append(rows, tu.InlineKeyboardRow(
			tu.InlineKeyboardButton(opt).WithCallbackData("ask:"+qid+":"+strconv.Itoa(i)),
		))
	}

	text := prefix + "❓ " + question

	p := tu.Message(tu.ID(target.ChatID), text).WithReplyMarkup(tu.InlineKeyboard(rows...))
	if target.ThreadID > 0 {
		p = p.WithMessageThreadID(target.ThreadID)
	}

	if _, err := b.api.SendMessage(ctx, p); err != nil {
		slog.Error("ask prompt send failed", "chat_id", target.ChatID, "thread_id", target.ThreadID, "qid", qid, "err", err)
	}
}

// handleAskCallback resolves a tapped `ask` answer. Authorization already ran
// in handleCallback. The daemon-side registry (RouteAsk+ResolveAsk) is the
// authority for "first tap wins"; the EditMessageText freeze here is UI-only
// and best-effort (a raced second tap resolves to a no-op daemon-side).
func (b *Bot) handleAskCallback(ctx context.Context, q telego.CallbackQuery, qid, idxStr string) error {
	idx, err := strconv.Atoi(idxStr)
	if err != nil {
		// Malformed callback data (askCallbackRE already bounds it to digits, so
		// this is defensive): ack the query and drop it — nothing to route.
		slog.Warn("ask callback bad index", "qid", qid, "idx", idxStr)

		_ = b.api.AnswerCallbackQuery(ctx, &telego.AnswerCallbackQueryParams{CallbackQueryID: q.ID})

		return nil //nolint:nilerr // swallow the parse failure; the query is acked above
	}

	user := userLabel(&q.From)

	slog.Info("ask callback received", "qid", qid, "idx", idx, "user_id", q.From.ID)

	delivered := b.notifier.ResolveAsk(qid, idx, user)

	if !delivered {
		// The question already resolved (another tap) or the asking tool timed
		// out and cancelled it — DON'T show a false "chose X" confirmation.
		_ = b.api.AnswerCallbackQuery(ctx, &telego.AnswerCallbackQueryParams{
			CallbackQueryID: q.ID,
			Text:            "⌛ This question is no longer active.",
		})

		if msg, ok := q.Message.(*telego.Message); ok && msg != nil && msg.Text != "" {
			_, _ = b.api.EditMessageText(ctx, tu.EditMessageText(tu.ID(msg.Chat.ID), msg.MessageID,
				msg.Text+"\n\n⌛ expired"))
		}

		return nil
	}

	label := askOptionLabel(q.Message, idx)

	_ = b.api.AnswerCallbackQuery(ctx, &telego.AnswerCallbackQueryParams{
		CallbackQueryID: q.ID,
		Text:            "✅ Answered",
	})

	// Replace the buttons with the outcome so the prompt can't be answered twice.
	if msg, ok := q.Message.(*telego.Message); ok && msg != nil && msg.Text != "" {
		chosen := "option " + strconv.Itoa(idx)
		if label != "" {
			chosen = label
		}

		_, _ = b.api.EditMessageText(ctx, tu.EditMessageText(tu.ID(msg.Chat.ID), msg.MessageID,
			msg.Text+"\n\n✅ "+user+" chose: "+chosen))
	}

	return nil
}

// askOptionLabel recovers the tapped button's label from the message's inline
// keyboard so the frozen prompt shows the choice text, not a bare index.
// Returns "" when the markup is unavailable or the index is out of range.
func askOptionLabel(m telego.MaybeInaccessibleMessage, idx int) string {
	msg, ok := m.(*telego.Message)
	if !ok || msg == nil || msg.ReplyMarkup == nil {
		return ""
	}

	var flat []telego.InlineKeyboardButton
	for _, row := range msg.ReplyMarkup.InlineKeyboard {
		flat = append(flat, row...)
	}

	if idx < 0 || idx >= len(flat) {
		return ""
	}

	return flat[idx].Text
}

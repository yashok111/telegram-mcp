package bot

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/yakov/telegram-mcp/internal/access"
)

// maxReactionRunes caps the AckReaction string length. A single Telegram
// reaction emoji is one or two code points; 8 runes leaves headroom for ZWJ
// sequences without letting a user paste arbitrary text into access.json.
const maxReactionRunes = 8

// handleReactionCommand drives the `/reaction` DM command:
//
//	/reaction          → show current AckReaction
//	/reaction off      → clear AckReaction
//	/reaction <emoji>  → set AckReaction to the trimmed argument
//
// The argument is stored verbatim (no emoji validation): an invalid value
// surfaces later when Telegram rejects the SetMessageReaction call. The
// store mutation goes through Mutate so it doesn't race the daemon's
// background RulesCleanup ticker.
func (b *Bot) handleReactionCommand(ctx context.Context, msg telego.Message, st access.State) {
	chatID := tu.ID(msg.Chat.ID)

	rest := commandArg(msg.Text)

	if rest == "" {
		_, _ = b.api.SendMessage(ctx, tu.Message(chatID, renderReactionStatus(st.AckReaction)))
		return
	}

	if strings.EqualFold(rest, "off") {
		var mutated bool

		if err := b.store.Mutate(func(s *access.State) bool {
			if s.AckReaction == "" {
				return false
			}

			s.AckReaction = ""
			mutated = true

			return true
		}); err != nil {
			_, _ = b.api.SendMessage(ctx, tu.Message(chatID, "Failed to save: "+err.Error()))
			return
		}

		reply := "Ack reaction cleared."
		if !mutated {
			reply = "Ack reaction was already off."
		}

		_, _ = b.api.SendMessage(ctx, tu.Message(chatID, reply))

		return
	}

	if utf8.RuneCountInString(rest) > maxReactionRunes {
		_, _ = b.api.SendMessage(ctx, tu.Message(chatID, "Reaction too long — pass a single emoji."))
		return
	}

	var mutated bool

	if err := b.store.Mutate(func(s *access.State) bool {
		if s.AckReaction == rest {
			return false
		}

		s.AckReaction = rest
		mutated = true

		return true
	}); err != nil {
		_, _ = b.api.SendMessage(ctx, tu.Message(chatID, "Failed to save: "+err.Error()))
		return
	}

	reply := "Ack reaction set to " + rest
	if !mutated {
		reply = "Ack reaction already " + rest
	}

	_, _ = b.api.SendMessage(ctx, tu.Message(chatID, reply))
}

// commandArg returns the substring after the first whitespace in text — the
// shared "everything after the command word" idiom used by /label and friends.
// Returns "" when text has no argument.
func commandArg(text string) string {
	idx := strings.IndexAny(text, " \t")
	if idx < 0 {
		return ""
	}

	return strings.TrimSpace(text[idx+1:])
}

func renderReactionStatus(current string) string {
	if current == "" {
		return "Ack reaction: off. Set with `/reaction <emoji>` (e.g. `/reaction 👀`)."
	}

	return "Ack reaction: " + current + ". Clear with `/reaction off`."
}

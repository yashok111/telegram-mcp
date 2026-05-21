package bot

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/yakov/telegram-mcp/internal/access"
)

func (b *Bot) handleRulesCommand(ctx context.Context, msg telego.Message, st access.State) {
	parts := strings.Fields(strings.TrimSpace(msg.Text))

	sub := ""
	if len(parts) > 1 {
		sub = strings.ToLower(parts[1])
	}

	chatID := tu.ID(msg.Chat.ID)

	switch sub {
	case "", "list":
		_, _ = b.api.SendMessage(ctx, tu.Message(chatID, renderRules(st.Rules)).WithParseMode("MarkdownV2"))
	case "clear":
		var n int

		err := b.store.Mutate(func(st *access.State) bool {
			n = access.ClearRules(st)
			return n > 0
		})
		if err != nil {
			_, _ = b.api.SendMessage(ctx, tu.Message(chatID, "Failed to save: "+err.Error()))
			return
		}

		_, _ = b.api.SendMessage(ctx, tu.Message(chatID, fmt.Sprintf("Cleared %d rule(s).", n)))
	case "revoke":
		if len(parts) < 3 {
			_, _ = b.api.SendMessage(ctx, tu.Message(chatID, "Usage: /rules revoke <id>"))
			return
		}

		id := parts[2]

		var found bool

		err := b.store.Mutate(func(st *access.State) bool {
			found = access.RevokeRule(st, id)
			return found
		})
		if !found {
			_, _ = b.api.SendMessage(ctx, tu.Message(chatID, "No rule with id "+MdCode(id)).WithParseMode("MarkdownV2"))
			return
		}

		if err != nil {
			_, _ = b.api.SendMessage(ctx, tu.Message(chatID, "Failed to save: "+err.Error()))
			return
		}

		_, _ = b.api.SendMessage(ctx, tu.Message(chatID, "Revoked rule "+MdCode(id)).WithParseMode("MarkdownV2"))
	default:
		_, _ = b.api.SendMessage(ctx, tu.Message(chatID, "Usage: /rules [list|clear|revoke <id>]"))
	}
}

func renderRules(rules []access.PermissionRule) string {
	now := time.Now().UnixMilli()

	active := make([]access.PermissionRule, 0, len(rules))
	for _, r := range rules {
		if r.ExpiresAt == 0 || r.ExpiresAt > now {
			active = append(active, r)
		}
	}

	if len(active) == 0 {
		return "No permission rules\\. Tap a button on a permission prompt to add one\\."
	}

	var sb strings.Builder

	sb.WriteString("Active permission rules:\n")

	for _, r := range active {
		exp := "never"

		if r.ExpiresAt > 0 {
			left := time.Duration(r.ExpiresAt-now) * time.Millisecond
			exp = "expires in " + left.Round(time.Minute).String()
		}

		var path string
		if r.PathPattern == "" {
			path = "\\(any path\\)"
		} else {
			path = EscapeMarkdownV2(r.PathPattern)
		}

		fmt.Fprintf(&sb, "• %s — %s %s \\[%s\\] — %s\n",
			MdCode(r.ID), r.Action, EscapeMarkdownV2(r.Tool), path, EscapeMarkdownV2(exp))
	}

	return sb.String()
}

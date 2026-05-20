package bot

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/yakov/telegram-mcp/internal/access"
)

// EffortLevel is a user-facing label that maps to (model, thinking-budget).
// Stored as a string in access.State.EffortByChat to survive process restarts.
type EffortLevel string

const (
	EffortLow    EffortLevel = "low"
	EffortMedium EffortLevel = "medium"
	EffortHigh   EffortLevel = "high"
	EffortXHigh  EffortLevel = "xhigh"
	EffortMax    EffortLevel = "max"
)

// EffortConfig is the concrete spawn-time payload derived from an EffortLevel.
type EffortConfig struct {
	Model          string
	ThinkingTokens int
}

var effortConfigs = map[EffortLevel]EffortConfig{
	EffortLow:    {Model: "claude-haiku-4-5", ThinkingTokens: 0},
	EffortMedium: {Model: "claude-sonnet-4-6", ThinkingTokens: 8000},
	EffortHigh:   {Model: "claude-opus-4-7", ThinkingTokens: 16000},
	EffortXHigh:  {Model: "claude-opus-4-7", ThinkingTokens: 32000},
	EffortMax:    {Model: "claude-opus-4-7", ThinkingTokens: 64000},
}

// AllEfforts returns the levels in deterministic, increasing-effort order so
// /help and /status reads can render them consistently.
func AllEfforts() []EffortLevel {
	return []EffortLevel{EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax}
}

// ResolveEffort looks up the spawn config for a level. ok=false when name is
// unrecognized (callers fall back to daemon defaults).
func ResolveEffort(name string) (EffortConfig, bool) {
	c, ok := effortConfigs[EffortLevel(strings.ToLower(strings.TrimSpace(name)))]
	return c, ok
}

// EffortSubCmd discriminates the /effort sub-actions.
type EffortSubCmd int

const (
	EffortSubShow  EffortSubCmd = iota // /effort, /effort show
	EffortSubSet                       // /effort <level>
	EffortSubClear                     // /effort clear|off|reset
	EffortSubHelp                      // /effort help
)

// EffortArgs is the parsed form of the user's /effort text.
type EffortArgs struct {
	Sub   EffortSubCmd
	Level EffortLevel
}

var ErrEffortUnknownLevel = errors.New("unknown effort level")

// parseEffortArgs handles "" / "show" / "help" / "low|medium|high|xhigh|max" /
// "clear|off|reset". Extra tokens are rejected.
func parseEffortArgs(text string) (EffortArgs, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return EffortArgs{Sub: EffortSubShow}, nil
	}

	fields := strings.Fields(text)
	if len(fields) > 1 {
		return EffortArgs{}, ErrEffortUnknownLevel
	}

	first := strings.ToLower(fields[0])
	switch first {
	case "show":
		return EffortArgs{Sub: EffortSubShow}, nil
	case "help":
		return EffortArgs{Sub: EffortSubHelp}, nil
	case "clear", "off", "reset":
		return EffortArgs{Sub: EffortSubClear}, nil
	}

	if _, ok := effortConfigs[EffortLevel(first)]; ok {
		return EffortArgs{Sub: EffortSubSet, Level: EffortLevel(first)}, nil
	}

	return EffortArgs{}, ErrEffortUnknownLevel
}

// handleEffortCommand processes /effort show|<level>|clear|help in DMs.
// The persisted level is read by /spawn and /bg dispatchers to set model +
// MAX_THINKING_TOKENS on freshly-launched Claude clients; running shims keep
// their effort until respawn.
func (b *Bot) handleEffortCommand(ctx context.Context, msg telego.Message) {
	rest := stripBotCmd(msg.Text)
	chatID := strconv.FormatInt(msg.Chat.ID, 10)

	args, err := parseEffortArgs(rest)
	if err != nil {
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID),
			"Invalid /effort syntax: "+err.Error()+"\n\n"+formatEffortHelpReply()))

		return
	}

	switch args.Sub {
	case EffortSubHelp:
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), formatEffortHelpReply()))
	case EffortSubShow:
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), b.formatEffortStatus(chatID)).WithParseMode("MarkdownV2"))
	case EffortSubClear:
		_ = b.store.Mutate(func(st *access.State) bool {
			if st.EffortByChat == nil {
				return false
			}

			if _, ok := st.EffortByChat[chatID]; !ok {
				return false
			}

			delete(st.EffortByChat, chatID)

			return true
		})
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID),
			"🧹 Cleared\\. Future /spawn and /bg use daemon defaults\\.").WithParseMode("MarkdownV2"))
	case EffortSubSet:
		level := string(args.Level)
		_ = b.store.Mutate(func(st *access.State) bool {
			if st.EffortByChat == nil {
				st.EffortByChat = map[string]string{}
			}

			st.EffortByChat[chatID] = level

			return true
		})
		cfg, _ := ResolveEffort(level)
		text := fmt.Sprintf("✅ Effort set to %s · model %s · thinking %d\nApplies to new /spawn and /bg\\. Existing sessions keep their settings until respawn\\.",
			MdCode(level), MdCode(cfg.Model), cfg.ThinkingTokens)
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), text).WithParseMode("MarkdownV2"))
	}
}

func (b *Bot) formatEffortStatus(chatID string) string {
	st := b.store.Load()

	level, ok := st.EffortByChat[chatID]
	if !ok {
		return "No effort override set for this chat\\. Using daemon defaults\\.\n\n" + formatEffortHelpReply()
	}

	cfg, _ := ResolveEffort(level)

	return fmt.Sprintf("Effort: %s · model %s · thinking %d", MdCode(level), MdCode(cfg.Model), cfg.ThinkingTokens)
}

// renderEffortLine produces the /status one-liner. Output is MarkdownV2-safe.
func renderEffortLine(st access.State, chatID string) string {
	level, ok := st.EffortByChat[chatID]
	if !ok {
		return "Effort: daemon default \\(set with /effort <level>\\)\\."
	}

	cfg, found := ResolveEffort(level)
	if !found {
		return "Effort: " + MdCode(level) + " \\(unknown level — clear with /effort clear\\)\\."
	}

	return fmt.Sprintf("Effort: %s · model %s · thinking %d", MdCode(level), MdCode(cfg.Model), cfg.ThinkingTokens)
}

func formatEffortHelpReply() string {
	lines := []string{
		"Usage:",
		"  /effort                  — show current setting",
		"  /effort <level>          — set level (low | medium | high | xhigh | max)",
		"  /effort clear            — drop override; fall back to daemon defaults",
		"",
		"Levels:",
		"  low    — claude-haiku-4-5  · thinking 0",
		"  medium — claude-sonnet-4-6 · thinking 8000",
		"  high   — claude-opus-4-7   · thinking 16000",
		"  xhigh  — claude-opus-4-7   · thinking 32000",
		"  max    — claude-opus-4-7   · thinking 64000",
		"",
		"Applies to new /spawn and /bg sessions. Existing shims keep their settings until respawn.",
	}

	return strings.Join(lines, "\n")
}

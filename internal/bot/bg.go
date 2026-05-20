package bot

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

type BgSubCmd int

const (
	BgSubStart BgSubCmd = iota
	BgSubList
	BgSubCancel
	BgSubHelp
)

type BgArgs struct {
	Sub     BgSubCmd
	Prompt  string
	Workdir string
	TaskID  string
}

var (
	ErrBgEmptyPrompt         = errors.New("prompt is empty")
	ErrBgFlagInRequiresValue = errors.New("--in requires a directory")
	ErrBgCancelNeedsID       = errors.New("cancel requires a task id")
)

type BgSpawnRequest struct {
	Prompt  string
	Workdir string
	ChatID  string
	UserID  string
}

type BgTaskInfo struct {
	ID         string
	StartedAt  time.Time
	Workdir    string
	PromptHead string
	UserID     string
	Status     string
}

type BgRunner interface {
	Spawn(ctx context.Context, req BgSpawnRequest) (string, error)
	List() []BgTaskInfo
	Cancel(id string) error
}

func parseBgArgs(text string) (BgArgs, error) {
	text = strings.TrimSpace(text)
	if text == "" || strings.EqualFold(text, "help") {
		return BgArgs{Sub: BgSubHelp}, nil
	}

	first, rest, _ := strings.Cut(text, " ")
	switch strings.ToLower(first) {
	case "list":
		return BgArgs{Sub: BgSubList}, nil
	case "cancel":
		tid := strings.TrimSpace(rest)
		if tid == "" {
			return BgArgs{}, ErrBgCancelNeedsID
		}

		return BgArgs{Sub: BgSubCancel, TaskID: tid}, nil
	}

	fields := strings.Fields(text)

	var (
		prompt  []string
		workdir string
	)

	for i := 0; i < len(fields); i++ {
		if fields[i] == "--in" {
			if i+1 >= len(fields) {
				return BgArgs{}, ErrBgFlagInRequiresValue
			}

			workdir = fields[i+1]
			i++

			continue
		}

		prompt = append(prompt, fields[i])
	}

	if len(prompt) == 0 {
		return BgArgs{}, ErrBgEmptyPrompt
	}

	return BgArgs{Sub: BgSubStart, Prompt: strings.Join(prompt, " "), Workdir: workdir}, nil
}

// handleBgCommand parses the /bg subcommand and dispatches to runner. The
// runner is passed in (rather than read off the Bot) so Wave 3 can wire it
// without forcing a Bot-struct change in this isolated handler task.
func (b *Bot) handleBgCommand(ctx context.Context, msg telego.Message, runner BgRunner) {
	if runner == nil {
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), "Background tasks are not configured."))
		return
	}

	rest := stripBotCmd(msg.Text)

	args, err := parseBgArgs(rest)
	if err != nil {
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID),
			"Invalid /bg syntax: "+err.Error()+"\n\n"+formatBgHelpReply()))

		return
	}

	switch args.Sub {
	case BgSubHelp:
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), formatBgHelpReply()))
	case BgSubList:
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), formatBgListReply(runner.List())).WithParseMode("MarkdownV2"))
	case BgSubCancel:
		if cerr := runner.Cancel(args.TaskID); cerr != nil {
			_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), "Cancel failed: "+cerr.Error()))
		} else {
			_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), "🛑 Cancelling task "+MdCode(args.TaskID)).WithParseMode("MarkdownV2"))
		}
	case BgSubStart:
		var userID string
		if msg.From != nil {
			userID = strconv.FormatInt(msg.From.ID, 10)
		}

		id, serr := runner.Spawn(ctx, BgSpawnRequest{
			Prompt:  args.Prompt,
			Workdir: args.Workdir,
			ChatID:  strconv.FormatInt(msg.Chat.ID, 10),
			UserID:  userID,
		})
		if serr != nil {
			_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), "Start failed: "+serr.Error()))
			return
		}

		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID),
			"📋 Started task "+MdCode(id)+"\\. Use "+MdCode("/bg cancel "+id)+" to stop\\.").WithParseMode("MarkdownV2"))
	}
}

func formatBgHelpReply() string {
	return strings.Join([]string{
		"Usage:",
		"  /bg <prompt> [--in <dir>]  — start a one-shot Claude run",
		"  /bg list                   — list running tasks",
		"  /bg cancel <id>            — cancel a task",
		"  /bg help                   — this message",
	}, "\n")
}

func formatBgListReply(tasks []BgTaskInfo) string {
	if len(tasks) == 0 {
		return "No /bg tasks running\\."
	}

	var b strings.Builder

	now := time.Now()
	for _, t := range tasks {
		fmt.Fprintf(&b, "%s · %s · %s ago · %s\n", MdCode(t.ID), t.Status, now.Sub(t.StartedAt).Round(time.Second), EscapeMarkdownV2(t.PromptHead))
	}

	return strings.TrimRight(b.String(), "\n")
}

// stripBotCmd removes the leading "/word" (or "/word@bot") and returns the
// trimmed rest. Empty string when text contains only the command.
func stripBotCmd(text string) string {
	if !strings.HasPrefix(text, "/") {
		return text
	}

	_, rest, _ := strings.Cut(text, " ")

	return strings.TrimSpace(rest)
}

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

// SpawnRequest is the input to SpawnRunner.Spawn. There is no prompt field —
// /spawn bootstraps a fresh CC client; the user then drives it via @mention
// in Telegram once the spawn's shim has hello-handshaked with the daemon.
type SpawnRequest struct {
	Workdir        string
	ChatID         string
	UserID         string
	Model          string
	ThinkingTokens int
	// ThreadID, when non-zero, is the forum topic the /spawn command was
	// issued from. The daemon pins the spawn to this thread so the spawned
	// session's shim adopts that exact topic on hello instead of allocating
	// one by workdir/label. Zero for DM-launched spawns.
	ThreadID int
}

// SpawnTaskInfo is the runner's view of one live spawn. PID + StartedAt let
// `/spawn list` cross-reference against Router.Snapshot() (which exposes the
// matching shim's SpawnID, see internal/daemon/info.go:ShimInfo).
type SpawnTaskInfo struct {
	ID        string
	Pid       int
	StartedAt time.Time
	Workdir   string
	UserID    string
	ChatID    string
	Status    string
}

// SpawnRunner is the bot-facing slice of the daemon's spawn manager. The
// daemon owns the actual subprocess; the bot only needs Spawn/List/Cancel.
// Routing is handled by the standard Router (@mention / reply / affinity) —
// there is no bot-side hook for /spawn inbounds.
type SpawnRunner interface {
	Spawn(ctx context.Context, req SpawnRequest) (string, error)
	List() []SpawnTaskInfo
	Cancel(id string) error
}

type SpawnSubCmd int

const (
	SpawnSubStart SpawnSubCmd = iota
	SpawnSubList
	SpawnSubCancel
	SpawnSubHelp
)

type SpawnArgs struct {
	Sub     SpawnSubCmd
	Workdir string
	TaskID  string
}

var (
	ErrSpawnArgsFlagInRequiresValue = errors.New("--in requires a directory")
	ErrSpawnArgsCancelNeedsID       = errors.New("cancel requires a spawn id")
	ErrSpawnArgsUnknownArg          = errors.New("unknown argument")
)

// SetSpawnRunner wires the interactive-session spawner. Must be called before
// Poll; nil-safe so tests and embeddings that don't use /spawn can skip it.
func (b *Bot) SetSpawnRunner(r SpawnRunner) { b.spawnRunner = r }

func parseSpawnArgs(text string) (SpawnArgs, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return SpawnArgs{Sub: SpawnSubStart}, nil
	}

	first, rest, _ := strings.Cut(text, " ")
	switch strings.ToLower(first) {
	case "help":
		return SpawnArgs{Sub: SpawnSubHelp}, nil
	case "list":
		return SpawnArgs{Sub: SpawnSubList}, nil
	case "cancel":
		tid := strings.TrimSpace(rest)
		if tid == "" {
			return SpawnArgs{}, ErrSpawnArgsCancelNeedsID
		}

		return SpawnArgs{Sub: SpawnSubCancel, TaskID: tid}, nil
	}

	// /spawn [--in <dir>] — only the --in flag is accepted; anything else is
	// rejected so users don't pass a prompt expecting one-shot semantics.
	fields := strings.Fields(text)

	var workdir string

	for i := 0; i < len(fields); i++ {
		switch fields[i] {
		case "--in":
			if i+1 >= len(fields) {
				return SpawnArgs{}, ErrSpawnArgsFlagInRequiresValue
			}

			workdir = fields[i+1]
			i++
		default:
			return SpawnArgs{}, fmt.Errorf("%w: %q", ErrSpawnArgsUnknownArg, fields[i])
		}
	}

	return SpawnArgs{Sub: SpawnSubStart, Workdir: workdir}, nil
}

// handleSpawnCommand parses the /spawn subcommand and dispatches to runner.
// runner is passed in so handler stays testable without a Bot-struct mutation.
func (b *Bot) handleSpawnCommand(ctx context.Context, msg telego.Message, runner SpawnRunner) {
	threadID := msg.MessageThreadID

	// send keeps every reply in the originating forum topic when the command
	// was issued inside one (threadID > 0); DM callers leave it unset.
	send := func(text, parseMode string) {
		p := tu.Message(tu.ID(msg.Chat.ID), text)
		if threadID > 0 {
			p = p.WithMessageThreadID(threadID)
		}

		if parseMode != "" {
			p = p.WithParseMode(parseMode)
		}

		_, _ = b.api.SendMessage(ctx, p)
	}

	if runner == nil {
		send("Spawn sessions are not configured.", "")
		return
	}

	args, err := parseSpawnArgs(stripBotCmd(msg.Text))
	if err != nil {
		send("Invalid /spawn syntax: "+err.Error()+"\n\n"+formatSpawnHelpReply(), "")
		return
	}

	chatID := strconv.FormatInt(msg.Chat.ID, 10)

	switch args.Sub {
	case SpawnSubHelp:
		send(formatSpawnHelpReply(), "")
	case SpawnSubList:
		send(b.renderSpawnList(runner.List()), "MarkdownV2")
	case SpawnSubCancel:
		if cerr := runner.Cancel(args.TaskID); cerr != nil {
			send("Cancel failed: "+cerr.Error(), "")
		} else {
			send("🛑 Cancelling spawn "+MdCode(args.TaskID), "MarkdownV2")
		}
	case SpawnSubStart:
		var userID string
		if msg.From != nil {
			userID = strconv.FormatInt(msg.From.ID, 10)
		}

		var (
			model    string
			thinking int
		)

		if b.store != nil {
			st := b.store.Load()
			if level, ok := st.EffortByChat[chatID]; ok {
				if cfg, found := ResolveEffort(level); found {
					model = cfg.Model
					thinking = cfg.ThinkingTokens
				}
			}
		}

		_, serr := runner.Spawn(ctx, SpawnRequest{
			Workdir:        args.Workdir,
			ChatID:         chatID,
			UserID:         userID,
			Model:          model,
			ThinkingTokens: thinking,
			ThreadID:       threadID,
		})
		if serr != nil {
			send("Spawn failed: "+serr.Error(), "")
			return
		}
		// The runner posts a "Spawn <id> started" message itself.
	}
}

// handleSpawnInTopic handles /spawn issued inside a forum supergroup topic.
// It reuses topicCommandGate (forum-chat + real-topic + allowlist checks)
// rather than the DM gate, then delegates to handleSpawnCommand which reads
// msg.MessageThreadID to pin the spawn to this topic.
func (b *Bot) handleSpawnInTopic(ctx context.Context, msg telego.Message) {
	if !b.topicCommandGate(ctx, &msg, b.store.Load()) {
		return
	}

	b.handleSpawnCommand(ctx, msg, b.spawnRunner)
}

func formatSpawnHelpReply() string {
	return strings.Join([]string{
		"Usage:",
		"  /spawn [--in <dir>]   — fork a fresh Claude Code client owned by this daemon",
		"  /spawn list           — list daemon-spawned sessions (resolves to @alias if registered)",
		"  /spawn cancel <id>    — SIGTERM the spawn (shim disconnects via PR_SET_PDEATHSIG)",
		"  /spawn help           — this message",
		"",
		"The spawned CC connects back as a fresh shim. Talk to it via @<alias> or /sessions like any other session.",
	}, "\n")
}

// renderSpawnList walks the runner's task table and cross-references against
// the Router snapshot (via b.router) so each row can show the matched alias.
// Falls back to "(no shim)" when the spawn is still booting or has crashed
// without hello-handshaking.
func (b *Bot) renderSpawnList(tasks []SpawnTaskInfo) string {
	if len(tasks) == 0 {
		return "No /spawn sessions running\\."
	}

	aliasByID := map[string]string{}

	if b.router != nil {
		for _, s := range b.router.Snapshot() {
			if s.SpawnID != "" {
				aliasByID[s.SpawnID] = s.Alias
			}
		}
	}

	var sb strings.Builder

	now := time.Now()

	for _, t := range tasks {
		alias := aliasByID[t.ID]

		var aliasCell string
		if alias == "" {
			aliasCell = "\\(no shim\\)"
		} else {
			aliasCell = MdCode(alias)
		}

		fmt.Fprintf(&sb, "%s · %s · %s · pid\\=%s · %s ago · %s\n",
			MdCode(t.ID), t.Status, aliasCell, MdCode(strconv.Itoa(t.Pid)),
			now.Sub(t.StartedAt).Round(time.Second), EscapeMarkdownV2(t.Workdir))
	}

	return strings.TrimRight(sb.String(), "\n")
}

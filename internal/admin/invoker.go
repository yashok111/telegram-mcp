package admin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	defaultInvokeTimeout = 5 * time.Minute

	// maxStreamLine caps a single stream-json line. claude's terminal result
	// event embeds the full answer, so this must be generous — but bounded so a
	// runaway subprocess can't exhaust memory the way the old history reader did.
	maxStreamLine = 4 << 20
)

// Result is the outcome of one claude --print invocation.
type Result struct {
	Text     string
	CostUSD  float64
	NumTurns int
}

// ExecFunc runs the claude subprocess and returns its stdout. Injected so tests
// drive Invoke without spawning a real process; production uses defaultExec.
type ExecFunc func(ctx context.Context, dir, bin string, args, env []string) (stdout []byte, err error)

// Invoker answers an admin DM by spawning
// `claude --print --output-format=stream-json --verbose`, folding prior
// conversation into the prompt. Non-streaming: the full stdout is collected
// then parsed for the terminal result event (final text + cost). Same trust
// model as /bg and /spawn — the resolved bin runs with the daemon's UID and the
// allowlist is the gate.
type Invoker struct {
	ClaudeBin string
	Workdir   string
	Model     string
	Timeout   time.Duration

	// SelfBin wires the admin-tools MCP server: when set, Invoke passes
	// --mcp-config pointing at `SelfBin admin-tools` so the spawned claude gets
	// the read-only admin tools, scoped by --allowedTools and
	// --strict-mcp-config. Empty SelfBin → no tools (plain Q&A). The admin
	// token is NOT passed here — admin-tools inherits it from the env.
	SelfBin string

	Exec ExecFunc

	// Directives, when set, returns the operator's standing directives. They are
	// prepended to every prompt as persistent delegation context (claude --print
	// is stateless per call). nil = no directives.
	Directives func() string
}

// Invoke runs claude for a human-initiated DM with the FULL tool set (read +
// Tier-2 auto-apply + Tier-3 confirm). ctx bounds the call; Timeout (default
// 5m) caps it independently.
func (iv *Invoker) Invoke(ctx context.Context, prompt string, history []Message) (Result, error) {
	return iv.invokeWith(ctx, prompt, history, adminToolNames)
}

// InvokeObserve runs claude for an AUTONOMOUS observer reaction (event/sitrep)
// with the restricted tool set (read + Tier-3 propose, NO Tier-2 auto-apply),
// so observed/injected content can never drive an unconfirmed mutation.
func (iv *Invoker) InvokeObserve(ctx context.Context, prompt string, history []Message) (Result, error) {
	return iv.invokeWith(ctx, prompt, history, adminObserveToolNames)
}

func (iv *Invoker) invokeWith(ctx context.Context, prompt string, history []Message, tools []string) (Result, error) {
	exe := iv.Exec
	if exe == nil {
		exe = defaultExec
	}

	bin := iv.ClaudeBin
	if bin == "" {
		bin = "claude"
	}

	timeout := iv.Timeout
	if timeout <= 0 {
		timeout = defaultInvokeTimeout
	}

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{"--print", "--output-format=stream-json", "--verbose"}
	if iv.Model != "" {
		args = append(args, "--model="+iv.Model)
	}

	args = append(args, iv.toolArgsFor(tools)...)

	var directives string
	if iv.Directives != nil {
		directives = iv.Directives()
	}

	// "--" ends option parsing so the prompt positional isn't swallowed by the
	// variadic --allowedTools (claude declares it as <tools...>, greedy). Without
	// it claude exits "Input must be provided ... when using --print". Verified
	// against the real CLI.
	args = append(args, "--", buildPrompt(directives, history, prompt))

	stdout, execErr := exe(cctx, iv.Workdir, bin, args, nil)

	// Parse even on a non-zero exit: claude sometimes emits a complete result
	// event then exits non-zero. A parsed result beats discarding it. Only when
	// the output yields no usable result do we surface the exec error.
	res, parseErr := parseInvocation(stdout)

	switch {
	case parseErr == nil:
		if execErr != nil {
			slog.Warn("claude exited non-zero but emitted a result; using it", "err", execErr)
		}

		return res, nil
	case execErr != nil:
		return Result{}, fmt.Errorf("claude invoke: %w", execErr)
	default:
		return Result{}, parseErr
	}
}

// defaultExec runs claude and captures stdout. On a non-zero exit it returns
// whatever stdout was produced alongside an error carrying a bounded stderr tail
// — the caller decides whether to surface it to the operator.
func defaultExec(ctx context.Context, dir, bin string, args, env []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // bin is operator-resolved (resolveClaudeBin), same trust model as /bg and /spawn

	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if tail := boundedTail(strings.TrimSpace(stderr.String()), 2048); tail != "" {
			return stdout.Bytes(), fmt.Errorf("%w: %s", err, tail)
		}

		return stdout.Bytes(), err
	}

	return stdout.Bytes(), nil
}

func boundedTail(s string, n int) string {
	if len(s) <= n {
		return s
	}

	// Cut on a rune boundary: this tail is surfaced to the operator over
	// Telegram, which rejects invalid UTF-8. Advance past any continuation
	// bytes left at the cut point.
	cut := len(s) - n
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}

	return s[cut:]
}

// streamLine is the subset of a stream-json record the invoker cares about.
type streamLine struct {
	Type      string  `json:"type"`
	Result    string  `json:"result"`
	TotalCost float64 `json:"total_cost_usd"`
	NumTurns  int     `json:"num_turns"`
	Message   *struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

// parseInvocation reads the collected stdout line-by-line (it is JSONL), pulls
// the assistant text and the terminal result event. A blank result.result falls
// back to the accumulated assistant text. A stream with no result event is an
// error (claude crashed or was killed mid-turn).
func parseInvocation(stdout []byte) (Result, error) {
	var (
		res       Result
		sawResult bool
		assistant strings.Builder
	)

	sc := bufio.NewScanner(bytes.NewReader(stdout))
	sc.Buffer(make([]byte, 0, 64*1024), maxStreamLine)

	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}

		var sl streamLine
		if err := json.Unmarshal(line, &sl); err != nil {
			slog.Warn("invoker skipping malformed stream line",
				"err", err, "snippet", string(line[:min(len(line), 80)]))

			continue
		}

		switch sl.Type {
		case "assistant":
			if sl.Message != nil {
				for _, c := range sl.Message.Content {
					if c.Type == "text" {
						assistant.WriteString(c.Text)
					}
				}
			}
		case "result":
			res.CostUSD = sl.TotalCost
			res.NumTurns = sl.NumTurns
			res.Text = sl.Result
			sawResult = true
		}
	}

	if err := sc.Err(); err != nil {
		return res, fmt.Errorf("scan stream: %w", err)
	}

	if !sawResult {
		return res, errors.New("stream ended without result event")
	}

	if res.Text == "" {
		res.Text = strings.TrimSpace(assistant.String())
	}

	return res, nil
}

// toolArgs builds the --mcp-config / --allowedTools flags that give the spawned
// claude the read-only admin tools. Empty when SelfBin is unset (plain Q&A).
// --strict-mcp-config keeps the spawned claude from inheriting the operator's
// other MCP servers; --allowedTools scopes it to exactly the admin read tools so
// nothing else (Bash, Edit, …) runs unprompted in --print mode.
// toolArgsFor builds the --mcp-config / --allowedTools flags scoped to the given
// tool names. The caller picks the set: adminToolNames for human DMs,
// adminObserveToolNames for autonomous observer reactions.
func (iv *Invoker) toolArgsFor(tools []string) []string {
	if iv.SelfBin == "" {
		return nil
	}

	// No "env" block: admin-tools inherits TELEGRAM_ADMIN_TOKEN through the
	// spawn chain (daemon→agent→claude→admin-tools). Putting it here would
	// embed the token in claude's argv, world-readable via /proc/<pid>/cmdline.
	cfg, err := json.Marshal(map[string]any{
		"mcpServers": map[string]any{
			mcpServerName: map[string]any{
				"command": iv.SelfBin,
				"args":    []string{"admin-tools"},
			},
		},
	})
	if err != nil {
		slog.Warn("admin invoker mcp-config marshal failed; running without tools", "err", err)
		return nil
	}

	allowed := make([]string, len(tools))
	for i, n := range tools {
		allowed[i] = "mcp__" + mcpServerName + "__" + n
	}

	return []string{
		"--strict-mcp-config",
		"--mcp-config", string(cfg),
		"--allowedTools", strings.Join(allowed, " "),
	}
}

// buildPrompt folds directives and prior conversation into a single prompt
// string. claude --print is stateless per invocation, so the full retained
// history is replayed as context ahead of the current message.
func buildPrompt(directives string, history []Message, prompt string) string {
	var b strings.Builder

	if d := strings.TrimSpace(directives); d != "" {
		b.WriteString("Standing directives from the operator (persistent guidance):\n")
		b.WriteString(d)
		b.WriteString("\n\n")
	}

	if len(history) > 0 {
		b.WriteString("Prior conversation (most recent last):\n")

		for _, m := range history {
			fmt.Fprintf(&b, "%s: %s\n", m.Role, m.Content)
		}

		b.WriteString("\nCurrent message:\n")
	}

	b.WriteString(prompt)

	return b.String()
}

package admin

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// streamFixture is a canonical claude --print --output-format=stream-json
// stdout: system-init, one assistant text event, then the terminal result
// event carrying the final text + cost + turn count.
const streamFixture = `{"type":"system","subtype":"init","session_id":"sess-1","tools":["Bash"]}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Hello "}]},"session_id":"sess-1"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"there"}]},"session_id":"sess-1"}
{"type":"result","subtype":"success","is_error":false,"num_turns":2,"result":"Hello there","total_cost_usd":0.0123,"session_id":"sess-1"}
`

func fixedExec(stdout string, err error) ExecFunc {
	return func(_ context.Context, _, _ string, _, _ []string) ([]byte, error) {
		return []byte(stdout), err
	}
}

func TestInvokeParsesResultEvent(t *testing.T) {
	iv := &Invoker{Exec: fixedExec(streamFixture, nil)}

	res, err := iv.Invoke(context.Background(), "hi", nil)
	require.NoError(t, err)
	assert.Equal(t, "Hello there", res.Text)
	assert.InDelta(t, 0.0123, res.CostUSD, 1e-9)
	assert.Equal(t, 2, res.NumTurns)
}

func TestInvokeFallsBackToAssistantTextWhenResultBlank(t *testing.T) {
	stream := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"partial answer"}]}}
{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":0.001}
`
	iv := &Invoker{Exec: fixedExec(stream, nil)}

	res, err := iv.Invoke(context.Background(), "hi", nil)
	require.NoError(t, err)
	assert.Equal(t, "partial answer", res.Text)
	assert.Equal(t, 1, res.NumTurns)
}

func TestInvokeErrorsWhenNoResultEvent(t *testing.T) {
	stream := `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"x"}]}}
`
	iv := &Invoker{Exec: fixedExec(stream, nil)}

	_, err := iv.Invoke(context.Background(), "hi", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "result")
}

func TestInvokeSkipsMalformedStreamLines(t *testing.T) {
	stream := "{garbage not json}\n" + streamFixture
	iv := &Invoker{Exec: fixedExec(stream, nil)}

	res, err := iv.Invoke(context.Background(), "hi", nil)
	require.NoError(t, err)
	assert.Equal(t, "Hello there", res.Text)
}

func TestInvokeSurfacesExecError(t *testing.T) {
	iv := &Invoker{Exec: fixedExec("", errors.New("boom"))}

	_, err := iv.Invoke(context.Background(), "hi", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
}

func TestInvokePassesPromptAndModelToExec(t *testing.T) {
	var gotArgs []string

	iv := &Invoker{
		Model: "claude-opus-4-7",
		Exec: func(_ context.Context, _, bin string, args, _ []string) ([]byte, error) {
			gotArgs = args

			assert.Equal(t, "claude", bin) // empty ClaudeBin defaults to "claude"

			return []byte(streamFixture), nil
		},
	}

	_, err := iv.Invoke(context.Background(), "the question", nil)
	require.NoError(t, err)

	assert.Contains(t, gotArgs, "--print")
	assert.Contains(t, gotArgs, "--output-format=stream-json")
	assert.Contains(t, gotArgs, "--model=claude-opus-4-7")
	require.NotEmpty(t, gotArgs)
	assert.Contains(t, gotArgs[len(gotArgs)-1], "the question", "prompt is the final positional arg")
}

func TestBuildPromptFoldsHistory(t *testing.T) {
	history := []Message{
		{Role: "user", Content: "first q"},
		{Role: "assistant", Content: "first a"},
	}

	got := buildPrompt("", history, "second q")

	assert.Contains(t, got, "first q")
	assert.Contains(t, got, "first a")
	assert.Contains(t, got, "second q")
	assert.Less(t, strings.Index(got, "first q"), strings.Index(got, "second q"), "history precedes current message")
}

func TestBuildPromptNoHistoryReturnsPromptVerbatim(t *testing.T) {
	assert.Equal(t, "just this", buildPrompt("", nil, "just this"))
}

func TestBoundedTailRuneSafe(t *testing.T) {
	// "aé" = a, 0xC3, 0xA9. Keeping the last 1 byte would isolate the trailing
	// continuation byte (0xA9); the cut must advance to a rune boundary so the
	// tail (surfaced over Telegram) is valid UTF-8.
	out := boundedTail("aé", 1)
	assert.True(t, utf8.ValidString(out), "tail must be valid UTF-8, got %q", out)

	// Shorter than the budget: returned verbatim.
	assert.Equal(t, "abc", boundedTail("abc", 10))
}

func TestInvokerToolArgsEmptyWithoutSelfBin(t *testing.T) {
	iv := &Invoker{}
	assert.Empty(t, iv.toolArgsFor(adminToolNames))
}

func TestInvokerToolArgsWiresAdminTools(t *testing.T) {
	iv := &Invoker{SelfBin: "/usr/bin/telegram-mcp"}

	joined := strings.Join(iv.toolArgsFor(adminToolNames), " ")
	assert.Contains(t, joined, "--strict-mcp-config")
	assert.Contains(t, joined, "--mcp-config")
	assert.Contains(t, joined, "--allowedTools")
	assert.Contains(t, joined, `"command":"/usr/bin/telegram-mcp"`)
	assert.Contains(t, joined, `"admin-tools"`)
	assert.Contains(t, joined, "mcp__admin__list_shims")
	assert.Contains(t, joined, "mcp__admin__read_daemon_log")
	// Token must NOT appear in argv (leaks via /proc/<pid>/cmdline); admin-tools
	// inherits it from the env instead.
	assert.NotContains(t, joined, `"env"`)
}

// TestInvokeOwnerPathHasFullPermissions verifies the owner-DM path (Invoke) runs
// with normal, unrestricted permissions so the operator can drive todoist,
// subagents (Task), Bash, and any other configured tool — not only the admin
// tools. Headless --print can't surface an approval prompt, so this requires
// --permission-mode bypassPermissions; the admin tools stay wired via
// --mcp-config; and neither --strict-mcp-config (which would hide the operator's
// own MCP servers like todoist) nor --allowedTools (which would scope everything
// else out) is present.
func TestInvokeOwnerPathHasFullPermissions(t *testing.T) {
	var got []string

	iv := &Invoker{
		SelfBin: "/bin/tg",
		Exec: func(_ context.Context, _, _ string, args, _ []string) ([]byte, error) {
			got = args
			return []byte(streamFixture), nil
		},
	}

	_, err := iv.Invoke(context.Background(), "q", nil)
	require.NoError(t, err)

	joined := strings.Join(got, " ")
	assert.Contains(t, joined, "--permission-mode")
	assert.Contains(t, joined, "bypassPermissions")
	assert.Contains(t, joined, "--mcp-config", "admin tools stay available on the owner path")
	assert.NotContains(t, joined, "--strict-mcp-config", "operator MCP servers (todoist, …) must load")
	assert.NotContains(t, joined, "--allowedTools", "owner path must not scope tools out")
}

// TestInvokeObserveStaysSandboxed is the security regression guard for the
// autonomous observer paths (event reactions, sitreps, non-owner DMs): they keep
// the restricted tool set — read + owner-confirmed Tier-3 (propose only) — and
// must NEVER gain Tier-2 auto-apply or full bypass permissions, or injected
// content in an observed event/log/message could drive an unconfirmed mutation
// or arbitrary tool use with no human in the loop.
func TestInvokeObserveStaysSandboxed(t *testing.T) {
	var got []string

	iv := &Invoker{
		SelfBin: "/bin/tg",
		Exec: func(_ context.Context, _, _ string, args, _ []string) ([]byte, error) {
			got = args
			return []byte(streamFixture), nil
		},
	}

	_, err := iv.InvokeObserve(context.Background(), "q", nil)
	require.NoError(t, err)

	joined := strings.Join(got, " ")
	assert.Contains(t, joined, "--strict-mcp-config")
	assert.Contains(t, joined, "--allowedTools")
	assert.NotContains(t, joined, "bypassPermissions", "observer path must not run with full permissions")

	// Read + Tier-3 (owner-confirmed) present; Tier-2 auto-apply absent.
	assert.Contains(t, joined, "mcp__admin__list_shims")
	assert.Contains(t, joined, "mcp__admin__add_allow")

	for _, t2 := range adminTier2ToolNames {
		assert.NotContains(t, joined, "mcp__admin__"+t2, "observer path must NOT expose Tier-2 %q", t2)
	}
}

func TestInvokePassesToolArgsToExec(t *testing.T) {
	var gotArgs []string

	iv := &Invoker{
		SelfBin: "/bin/tg",
		Exec: func(_ context.Context, _, _ string, args, _ []string) ([]byte, error) {
			gotArgs = args

			return []byte(streamFixture), nil
		},
	}

	_, err := iv.Invoke(context.Background(), "the question", nil)
	require.NoError(t, err)

	assert.Contains(t, strings.Join(gotArgs, " "), "--mcp-config")
	assert.Equal(t, "the question", gotArgs[len(gotArgs)-1], "prompt stays the final positional arg")
}

// TestInvokeShieldsPromptFromVariadicAllowedTools guards the live-found bug:
// claude's --allowedTools is variadic (<tools...>), so a prompt placed right
// after it is swallowed and claude exits "Input must be provided". A "--"
// separator after the tool flags forces the prompt to be parsed as a positional.
// The observer path is exercised here because it is the path that still passes
// --allowedTools (the owner path dropped it for full permissions); the trailing
// "--" is appended on every path regardless.
func TestInvokeShieldsPromptFromVariadicAllowedTools(t *testing.T) {
	var gotArgs []string

	iv := &Invoker{
		SelfBin: "/bin/tg", // ensures --allowedTools (the variadic flag) is present
		Exec: func(_ context.Context, _, _ string, args, _ []string) ([]byte, error) {
			gotArgs = args
			return []byte(streamFixture), nil
		},
	}

	_, err := iv.InvokeObserve(context.Background(), "the question", nil)
	require.NoError(t, err)

	require.GreaterOrEqual(t, len(gotArgs), 2)
	assert.Equal(t, "the question", gotArgs[len(gotArgs)-1], "prompt is the final positional")
	assert.Equal(t, "--", gotArgs[len(gotArgs)-2], "'--' must immediately precede the prompt")

	allowedIdx, dashIdx := -1, -1

	for i, a := range gotArgs {
		switch a {
		case "--allowedTools":
			allowedIdx = i
		case "--":
			dashIdx = i
		}
	}

	require.NotEqual(t, -1, allowedIdx, "--allowedTools present")
	require.NotEqual(t, -1, dashIdx, "-- present")
	assert.Less(t, allowedIdx, dashIdx, "-- must come after --allowedTools to shield the prompt")
}

func TestBuildPromptFoldsDirectives(t *testing.T) {
	got := buildPrompt("be terse", nil, "hello")
	assert.Contains(t, got, "Standing directives")
	assert.Contains(t, got, "be terse")
	assert.Contains(t, got, "hello")
	assert.Less(t, strings.Index(got, "be terse"), strings.Index(got, "hello"), "directives precede prompt")

	assert.Equal(t, "hello", buildPrompt("", nil, "hello"))
}

func TestInvokeCallsDirectives(t *testing.T) {
	var capturedPrompt string

	iv := &Invoker{
		Directives: func() string { return "RULE-X" },
		Exec: func(_ context.Context, _, _ string, args, _ []string) ([]byte, error) {
			capturedPrompt = args[len(args)-1]

			return []byte(streamFixture), nil
		},
	}

	_, err := iv.Invoke(context.Background(), "do something", nil)
	require.NoError(t, err)
	assert.Contains(t, capturedPrompt, "RULE-X")
}

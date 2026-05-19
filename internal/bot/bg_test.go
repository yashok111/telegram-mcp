package bot

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/access"
)

// ===== handleBgCommand fixtures =====

type fakeBgRunner struct {
	spawnID     string
	spawnErr    error
	spawnCalls  []BgSpawnRequest
	listResult  []BgTaskInfo
	cancelErr   error
	cancelCalls []string
}

func (f *fakeBgRunner) Spawn(_ context.Context, req BgSpawnRequest) (string, error) {
	f.spawnCalls = append(f.spawnCalls, req)
	return f.spawnID, f.spawnErr
}

func (f *fakeBgRunner) List() []BgTaskInfo { return f.listResult }

func (f *fakeBgRunner) Cancel(id string) error {
	f.cancelCalls = append(f.cancelCalls, id)
	return f.cancelErr
}

// bgTestBot wires the real *Bot to the package-local mockAPI (defined in
// bot_api_test.go). Allowlist is irrelevant — handleBgCommand bypasses the
// gate because the caller (handleCommand) already ran it.
func bgTestBot(t *testing.T) (*Bot, *mockAPI) {
	t.Helper()
	b, api, _ := newTestBot(t, access.State{
		DMPolicy:  access.PolicyAllowlist,
		AllowFrom: []string{"99"},
		Groups:    map[string]access.GroupPolicy{},
		Pending:   map[string]access.Pending{},
	})

	return b, api
}

func bgMsg(text string) telego.Message {
	return telego.Message{
		Chat: telego.Chat{ID: 1, Type: "private"},
		From: &telego.User{ID: 99},
		Text: text,
	}
}

func sentMessageTexts(api *mockAPI) []string {
	calls := api.recordedCalls("sendMessage")
	out := make([]string, 0, len(calls))

	for _, c := range calls {
		if t, ok := c.params["text"].(string); ok {
			out = append(out, t)
		}
	}

	return out
}

func TestParseBgArgs_EmptyIsHelp(t *testing.T) {
	a, err := parseBgArgs("")
	require.NoError(t, err)
	assert.Equal(t, BgSubHelp, a.Sub)
}

func TestParseBgArgs_HelpKeyword(t *testing.T) {
	a, err := parseBgArgs("help")
	require.NoError(t, err)
	assert.Equal(t, BgSubHelp, a.Sub)
}

func TestParseBgArgs_List(t *testing.T) {
	a, err := parseBgArgs("list")
	require.NoError(t, err)
	assert.Equal(t, BgSubList, a.Sub)
}

func TestParseBgArgs_Cancel(t *testing.T) {
	a, err := parseBgArgs("cancel a3f2c1")
	require.NoError(t, err)
	assert.Equal(t, BgSubCancel, a.Sub)
	assert.Equal(t, "a3f2c1", a.TaskID)
}

func TestParseBgArgs_CancelNeedsID(t *testing.T) {
	_, err := parseBgArgs("cancel")
	assert.ErrorIs(t, err, ErrBgCancelNeedsID)
}

func TestParseBgArgs_StartNoFlag(t *testing.T) {
	a, err := parseBgArgs("refactor auth module")
	require.NoError(t, err)
	assert.Equal(t, BgSubStart, a.Sub)
	assert.Equal(t, "refactor auth module", a.Prompt)
	assert.Empty(t, a.Workdir)
}

func TestParseBgArgs_StartWithInTrailing(t *testing.T) {
	a, err := parseBgArgs("refactor auth --in /repo")
	require.NoError(t, err)
	assert.Equal(t, BgSubStart, a.Sub)
	assert.Equal(t, "refactor auth", a.Prompt)
	assert.Equal(t, "/repo", a.Workdir)
}

func TestParseBgArgs_StartWithInLeading(t *testing.T) {
	a, err := parseBgArgs("--in /repo refactor auth")
	require.NoError(t, err)
	assert.Equal(t, BgSubStart, a.Sub)
	assert.Equal(t, "refactor auth", a.Prompt)
	assert.Equal(t, "/repo", a.Workdir)
}

func TestParseBgArgs_InMissingValue(t *testing.T) {
	_, err := parseBgArgs("refactor --in")
	assert.ErrorIs(t, err, ErrBgFlagInRequiresValue)
}

func TestParseBgArgs_OnlyFlag(t *testing.T) {
	_, err := parseBgArgs("--in /repo")
	assert.ErrorIs(t, err, ErrBgEmptyPrompt)
}

func TestParseBgArgs_SubKeywordCaseInsensitive(t *testing.T) {
	for _, sub := range []string{"LIST", "List", "list"} {
		a, err := parseBgArgs(sub)
		require.NoError(t, err)
		assert.Equal(t, BgSubList, a.Sub)
	}
}

// ===== handleBgCommand =====

func TestHandleBgCommand_Help(t *testing.T) {
	b, api := bgTestBot(t)
	runner := &fakeBgRunner{}

	b.handleBgCommand(t.Context(), bgMsg("/bg help"), runner)

	texts := sentMessageTexts(api)
	require.Len(t, texts, 1)
	assert.Contains(t, texts[0], "Usage:")
	assert.Contains(t, texts[0], "/bg <prompt>")
	assert.Contains(t, texts[0], "/bg list")
	assert.Contains(t, texts[0], "/bg cancel")
}

func TestHandleBgCommand_BareSlashBgRendersHelp(t *testing.T) {
	b, api := bgTestBot(t)
	runner := &fakeBgRunner{}

	b.handleBgCommand(t.Context(), bgMsg("/bg"), runner)

	texts := sentMessageTexts(api)
	require.Len(t, texts, 1)
	assert.Contains(t, texts[0], "Usage:")
}

func TestHandleBgCommand_ListEmpty(t *testing.T) {
	b, api := bgTestBot(t)
	runner := &fakeBgRunner{}

	b.handleBgCommand(t.Context(), bgMsg("/bg list"), runner)

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Equal(t, "No /bg tasks running\\.", calls[0].params["text"])
	assert.Equal(t, "MarkdownV2", calls[0].params["parse_mode"])
}

func TestHandleBgCommand_ListRendersTasks(t *testing.T) {
	b, api := bgTestBot(t)
	runner := &fakeBgRunner{listResult: []BgTaskInfo{
		{ID: "a1", Status: "running", StartedAt: time.Now().Add(-30 * time.Second), PromptHead: "first"},
		{ID: "b2", Status: "running", StartedAt: time.Now().Add(-10 * time.Second), PromptHead: "second"},
	}}

	b.handleBgCommand(t.Context(), bgMsg("/bg list"), runner)

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	text, _ := calls[0].params["text"].(string)
	assert.Contains(t, text, "`a1` · running")
	assert.Contains(t, text, "`b2` · running")
	assert.Contains(t, text, "first")
	assert.Contains(t, text, "second")
	assert.Equal(t, "MarkdownV2", calls[0].params["parse_mode"])
}

func TestHandleBgCommand_CancelOK(t *testing.T) {
	b, api := bgTestBot(t)
	runner := &fakeBgRunner{}

	b.handleBgCommand(t.Context(), bgMsg("/bg cancel a1"), runner)

	assert.Equal(t, []string{"a1"}, runner.cancelCalls)

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	text, _ := calls[0].params["text"].(string)
	assert.Contains(t, text, "🛑 Cancelling task `a1`")
	assert.Equal(t, "MarkdownV2", calls[0].params["parse_mode"])
}

func TestHandleBgCommand_CancelUnknown(t *testing.T) {
	b, api := bgTestBot(t)
	runner := &fakeBgRunner{cancelErr: errors.New("task not found")}

	b.handleBgCommand(t.Context(), bgMsg("/bg cancel zzz"), runner)

	assert.Equal(t, []string{"zzz"}, runner.cancelCalls)

	texts := sentMessageTexts(api)
	require.Len(t, texts, 1)
	assert.Contains(t, texts[0], "Cancel failed: task not found")
}

func TestHandleBgCommand_StartOK(t *testing.T) {
	b, api := bgTestBot(t)
	runner := &fakeBgRunner{spawnID: "abc123"}

	b.handleBgCommand(t.Context(), bgMsg("/bg refactor auth --in /repo"), runner)

	require.Len(t, runner.spawnCalls, 1)
	assert.Equal(t, "refactor auth", runner.spawnCalls[0].Prompt)
	assert.Equal(t, "/repo", runner.spawnCalls[0].Workdir)
	assert.Equal(t, "1", runner.spawnCalls[0].ChatID)
	assert.Equal(t, "99", runner.spawnCalls[0].UserID)

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	text, _ := calls[0].params["text"].(string)
	assert.Contains(t, text, "📋 Started task `abc123`")
	assert.Contains(t, text, "`/bg cancel abc123`")
	assert.Equal(t, "MarkdownV2", calls[0].params["parse_mode"])
}

func TestHandleBgCommand_StartRateLimited(t *testing.T) {
	b, api := bgTestBot(t)
	runner := &fakeBgRunner{spawnErr: errors.New("rate limited: try again later")}

	b.handleBgCommand(t.Context(), bgMsg("/bg do stuff"), runner)

	require.Len(t, runner.spawnCalls, 1)

	texts := sentMessageTexts(api)
	require.Len(t, texts, 1)
	assert.Contains(t, texts[0], "Start failed: rate limited")
}

func TestHandleBgCommand_InvalidSyntax(t *testing.T) {
	b, api := bgTestBot(t)
	runner := &fakeBgRunner{}

	b.handleBgCommand(t.Context(), bgMsg("/bg refactor --in"), runner)

	texts := sentMessageTexts(api)
	require.Len(t, texts, 1)
	assert.Contains(t, texts[0], "Invalid /bg syntax: --in requires")
	assert.Contains(t, texts[0], "Usage:")
	assert.Empty(t, runner.spawnCalls)
}

func TestHandleBgCommand_NoRunnerConfigured(t *testing.T) {
	b, api := bgTestBot(t)

	b.handleBgCommand(t.Context(), bgMsg("/bg help"), nil)

	texts := sentMessageTexts(api)
	require.Len(t, texts, 1)
	assert.Contains(t, texts[0], "not configured")
}

func TestStripBotCmd(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"plain command", "/bg", ""},
		{"command with args", "/bg list", "list"},
		{"command with bot suffix and args", "/bg@some_bot refactor auth", "refactor auth"},
		{"non-slash", "hello", "hello"},
		{"trailing whitespace", "/bg  list  ", "list"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, stripBotCmd(tc.in))
		})
	}
}

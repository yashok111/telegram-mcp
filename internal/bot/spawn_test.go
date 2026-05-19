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

type fakeSpawnRunner struct {
	spawnID     string
	spawnErr    error
	spawnCalls  []SpawnRequest
	listResult  []SpawnTaskInfo
	cancelErr   error
	cancelCalls []string
}

func (f *fakeSpawnRunner) Spawn(_ context.Context, req SpawnRequest) (string, error) {
	f.spawnCalls = append(f.spawnCalls, req)
	return f.spawnID, f.spawnErr
}

func (f *fakeSpawnRunner) List() []SpawnTaskInfo { return f.listResult }

func (f *fakeSpawnRunner) Cancel(id string) error {
	f.cancelCalls = append(f.cancelCalls, id)
	return f.cancelErr
}

func spawnTestBot(t *testing.T) (*Bot, *mockAPI) {
	t.Helper()

	b, api, _ := newTestBot(t, access.State{
		DMPolicy:  access.PolicyAllowlist,
		AllowFrom: []string{"99"},
		Groups:    map[string]access.GroupPolicy{},
		Pending:   map[string]access.Pending{},
	})

	return b, api
}

func spawnMsg(text string) telego.Message {
	return telego.Message{
		Chat: telego.Chat{ID: 1, Type: "private"},
		From: &telego.User{ID: 99},
		Text: text,
	}
}

func TestParseSpawnArgs_EmptyIsStart(t *testing.T) {
	a, err := parseSpawnArgs("")
	require.NoError(t, err)
	assert.Equal(t, SpawnSubStart, a.Sub)
	assert.Empty(t, a.Workdir)
}

func TestParseSpawnArgs_HelpKeyword(t *testing.T) {
	a, err := parseSpawnArgs("help")
	require.NoError(t, err)
	assert.Equal(t, SpawnSubHelp, a.Sub)
}

func TestParseSpawnArgs_List(t *testing.T) {
	a, err := parseSpawnArgs("list")
	require.NoError(t, err)
	assert.Equal(t, SpawnSubList, a.Sub)
}

func TestParseSpawnArgs_Cancel(t *testing.T) {
	a, err := parseSpawnArgs("cancel a3f2c1")
	require.NoError(t, err)
	assert.Equal(t, SpawnSubCancel, a.Sub)
	assert.Equal(t, "a3f2c1", a.TaskID)
}

func TestParseSpawnArgs_CancelNeedsID(t *testing.T) {
	_, err := parseSpawnArgs("cancel")
	assert.ErrorIs(t, err, ErrSpawnArgsCancelNeedsID)
}

func TestParseSpawnArgs_StartWithIn(t *testing.T) {
	a, err := parseSpawnArgs("--in /repo")
	require.NoError(t, err)
	assert.Equal(t, SpawnSubStart, a.Sub)
	assert.Equal(t, "/repo", a.Workdir)
}

func TestParseSpawnArgs_InMissingValue(t *testing.T) {
	_, err := parseSpawnArgs("--in")
	assert.ErrorIs(t, err, ErrSpawnArgsFlagInRequiresValue)
}

func TestParseSpawnArgs_UnknownArg(t *testing.T) {
	_, err := parseSpawnArgs("some prompt text")
	assert.ErrorIs(t, err, ErrSpawnArgsUnknownArg)
}

func TestHandleSpawnCommand_Help(t *testing.T) {
	b, api := spawnTestBot(t)
	runner := &fakeSpawnRunner{}

	b.handleSpawnCommand(t.Context(), spawnMsg("/spawn help"), runner)

	texts := sentMessageTexts(api)
	require.Len(t, texts, 1)
	assert.Contains(t, texts[0], "Usage:")
	assert.Contains(t, texts[0], "/spawn [--in <dir>]")
}

func TestHandleSpawnCommand_StartOK(t *testing.T) {
	b, api := spawnTestBot(t)
	runner := &fakeSpawnRunner{spawnID: "ff00aa"}

	b.handleSpawnCommand(t.Context(), spawnMsg("/spawn --in /repo"), runner)

	require.Len(t, runner.spawnCalls, 1)
	assert.Equal(t, "/repo", runner.spawnCalls[0].Workdir)
	assert.Equal(t, "1", runner.spawnCalls[0].ChatID)
	assert.Equal(t, "99", runner.spawnCalls[0].UserID)

	// Runner posts its own confirmation; handler stays quiet on success.
	assert.Empty(t, sentMessageTexts(api))
}

func TestHandleSpawnCommand_BareSpawnStarts(t *testing.T) {
	b, api := spawnTestBot(t)
	runner := &fakeSpawnRunner{spawnID: "abc"}

	b.handleSpawnCommand(t.Context(), spawnMsg("/spawn"), runner)

	require.Len(t, runner.spawnCalls, 1)
	assert.Empty(t, runner.spawnCalls[0].Workdir)
	assert.Empty(t, sentMessageTexts(api))
}

func TestHandleSpawnCommand_StartFailureSurfaces(t *testing.T) {
	b, api := spawnTestBot(t)
	runner := &fakeSpawnRunner{spawnErr: errors.New("too many concurrent /spawn sessions")}

	b.handleSpawnCommand(t.Context(), spawnMsg("/spawn"), runner)

	texts := sentMessageTexts(api)
	require.Len(t, texts, 1)
	assert.Contains(t, texts[0], "Spawn failed: too many concurrent")
}

func TestHandleSpawnCommand_CancelOK(t *testing.T) {
	b, api := spawnTestBot(t)
	runner := &fakeSpawnRunner{}

	b.handleSpawnCommand(t.Context(), spawnMsg("/spawn cancel zz9"), runner)

	assert.Equal(t, []string{"zz9"}, runner.cancelCalls)

	texts := sentMessageTexts(api)
	require.Len(t, texts, 1)
	assert.Contains(t, texts[0], "🛑 Cancelling spawn zz9")
}

func TestHandleSpawnCommand_CancelUnknown(t *testing.T) {
	b, api := spawnTestBot(t)
	runner := &fakeSpawnRunner{cancelErr: errors.New("spawn not found")}

	b.handleSpawnCommand(t.Context(), spawnMsg("/spawn cancel zz9"), runner)

	texts := sentMessageTexts(api)
	require.Len(t, texts, 1)
	assert.Contains(t, texts[0], "Cancel failed: spawn not found")
}

func TestHandleSpawnCommand_ListEmpty(t *testing.T) {
	b, api := spawnTestBot(t)
	runner := &fakeSpawnRunner{}

	b.handleSpawnCommand(t.Context(), spawnMsg("/spawn list"), runner)

	texts := sentMessageTexts(api)
	require.Len(t, texts, 1)
	assert.Equal(t, "No /spawn sessions running.", texts[0])
}

func TestHandleSpawnCommand_ListRendersMatchedAlias(t *testing.T) {
	b, api := spawnTestBot(t)
	b.router = &fakeRouterView{snap: []ShimInfo{
		{Alias: "@s2", SpawnID: "ff00aa"},
		{Alias: "@s3", SpawnID: ""},
	}}
	runner := &fakeSpawnRunner{listResult: []SpawnTaskInfo{
		{ID: "ff00aa", Status: "running", Pid: 4242, StartedAt: time.Now().Add(-30 * time.Second), Workdir: "/repo"},
		{ID: "bbbbbb", Status: "starting", Pid: 4343, StartedAt: time.Now().Add(-5 * time.Second), Workdir: "/other"},
	}}

	b.handleSpawnCommand(t.Context(), spawnMsg("/spawn list"), runner)

	texts := sentMessageTexts(api)
	require.Len(t, texts, 1)
	assert.Contains(t, texts[0], "ff00aa · running · @s2 · pid=4242")
	assert.Contains(t, texts[0], "bbbbbb · starting · (no shim) · pid=4343")
}

func TestHandleSpawnCommand_NoRunnerConfigured(t *testing.T) {
	b, api := spawnTestBot(t)

	b.handleSpawnCommand(t.Context(), spawnMsg("/spawn"), nil)

	texts := sentMessageTexts(api)
	require.Len(t, texts, 1)
	assert.Contains(t, texts[0], "not configured")
}

func TestHandleSpawnCommand_InvalidSyntax(t *testing.T) {
	b, api := spawnTestBot(t)
	runner := &fakeSpawnRunner{}

	b.handleSpawnCommand(t.Context(), spawnMsg("/spawn build me a thing"), runner)

	texts := sentMessageTexts(api)
	require.Len(t, texts, 1)
	assert.Contains(t, texts[0], "Invalid /spawn syntax:")
	assert.Contains(t, texts[0], "Usage:")
	assert.Empty(t, runner.spawnCalls)
}

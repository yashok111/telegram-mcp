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
	assert.Contains(t, texts[0], "🛑 Cancelling spawn `zz9`")
	assert.Equal(t, "MarkdownV2", api.recordedCalls("sendMessage")[0].params["parse_mode"])
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
	assert.Equal(t, "No /spawn sessions running\\.", texts[0])
	assert.Equal(t, "MarkdownV2", api.recordedCalls("sendMessage")[0].params["parse_mode"])
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
	assert.Contains(t, texts[0], "`ff00aa` · running · `@s2` · pid\\=`4242`")
	assert.Contains(t, texts[0], "`bbbbbb` · starting · \\(no shim\\) · pid\\=`4343`")
	assert.Equal(t, "MarkdownV2", api.recordedCalls("sendMessage")[0].params["parse_mode"])
}

func TestHandleSpawnCommand_NoRunnerConfigured(t *testing.T) {
	b, api := spawnTestBot(t)

	b.handleSpawnCommand(t.Context(), spawnMsg("/spawn"), nil)

	texts := sentMessageTexts(api)
	require.Len(t, texts, 1)
	assert.Contains(t, texts[0], "not configured")
}

func TestHandleSpawnCommand_appliesEffortFromState(t *testing.T) {
	b, _, _ := newTestBot(t, access.State{
		DMPolicy:     access.PolicyAllowlist,
		AllowFrom:    []string{"99"},
		Groups:       map[string]access.GroupPolicy{},
		Pending:      map[string]access.Pending{},
		EffortByChat: map[string]string{"1": "high"},
	})
	runner := &fakeSpawnRunner{spawnID: "ok"}

	b.handleSpawnCommand(t.Context(), spawnMsg("/spawn"), runner)

	require.Len(t, runner.spawnCalls, 1)
	assert.Equal(t, "claude-opus-4-8", runner.spawnCalls[0].Model)
	assert.Equal(t, 16000, runner.spawnCalls[0].ThinkingTokens)
}

func TestHandleSpawnCommand_noEffortFallsBackToZeroValues(t *testing.T) {
	b, _ := spawnTestBot(t)
	runner := &fakeSpawnRunner{spawnID: "ok"}

	b.handleSpawnCommand(t.Context(), spawnMsg("/spawn"), runner)

	require.Len(t, runner.spawnCalls, 1)
	assert.Empty(t, runner.spawnCalls[0].Model)
	assert.Zero(t, runner.spawnCalls[0].ThinkingTokens)
}

func TestHandleSpawnCommand_unknownLevelInStateIgnored(t *testing.T) {
	b, _, _ := newTestBot(t, access.State{
		DMPolicy:     access.PolicyAllowlist,
		AllowFrom:    []string{"99"},
		Groups:       map[string]access.GroupPolicy{},
		Pending:      map[string]access.Pending{},
		EffortByChat: map[string]string{"1": "garbage"},
	})
	runner := &fakeSpawnRunner{spawnID: "ok"}

	b.handleSpawnCommand(t.Context(), spawnMsg("/spawn"), runner)

	require.Len(t, runner.spawnCalls, 1)
	assert.Empty(t, runner.spawnCalls[0].Model)
	assert.Zero(t, runner.spawnCalls[0].ThinkingTokens)
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

// forumSpawnBot wires a bot in a forum-enabled state with a fake spawn runner,
// modelling /spawn issued from inside a supergroup topic.
func forumSpawnBot(t *testing.T) (*Bot, *mockAPI, *fakeSpawnRunner) {
	t.Helper()

	b, api, _ := newTestBot(t, access.State{
		DMPolicy:    access.PolicyAllowlist,
		AllowFrom:   []string{"99"},
		Groups:      map[string]access.GroupPolicy{},
		Pending:     map[string]access.Pending{},
		ForumChatID: -100777,
	})

	fr := &fakeSpawnRunner{spawnID: "abc123"}
	b.SetSpawnRunner(fr)

	return b, api, fr
}

func forumSpawnMsg(threadID int, userID int64) telego.Message {
	return telego.Message{
		Chat:            telego.Chat{ID: -100777, Type: "supergroup"},
		From:            &telego.User{ID: userID},
		MessageThreadID: threadID,
		Text:            "/spawn",
	}
}

func TestHandleSpawn_DM_threadIDZero(t *testing.T) {
	b, _ := spawnTestBot(t)
	fr := &fakeSpawnRunner{spawnID: "x"}
	b.SetSpawnRunner(fr)

	require.NoError(t, b.handleCommand(t.Context(), spawnMsg("/spawn")))

	require.Len(t, fr.spawnCalls, 1)
	assert.Zero(t, fr.spawnCalls[0].ThreadID, "DM spawn carries no forum thread")
}

func TestHandleSpawn_forumTopic_passesThreadID(t *testing.T) {
	b, _, fr := forumSpawnBot(t)

	require.NoError(t, b.handleCommand(t.Context(), forumSpawnMsg(9, 99)))

	require.Len(t, fr.spawnCalls, 1, "spawn invoked from inside a forum topic")
	assert.Equal(t, 9, fr.spawnCalls[0].ThreadID, "thread of the originating topic")
	assert.Equal(t, "-100777", fr.spawnCalls[0].ChatID, "forum chat id")
}

func TestHandleSpawn_forumGeneralThread_rejected(t *testing.T) {
	b, api, fr := forumSpawnBot(t)

	require.NoError(t, b.handleCommand(t.Context(), forumSpawnMsg(0, 99)))

	assert.Empty(t, fr.spawnCalls, "no spawn from the General thread")

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].params["text"], "inside a topic")
}

func TestHandleSpawn_forumNotAllowlisted_rejected(t *testing.T) {
	b, api, fr := forumSpawnBot(t)

	require.NoError(t, b.handleCommand(t.Context(), forumSpawnMsg(9, 12345)))

	assert.Empty(t, fr.spawnCalls, "non-allowlisted sender cannot spawn")

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].params["text"], "Not authorized")
}

func TestHandleSpawn_forumWrongChat_rejected(t *testing.T) {
	b, api, fr := forumSpawnBot(t)

	msg := forumSpawnMsg(9, 99)
	msg.Chat.ID = -100999 // not the configured forum chat

	require.NoError(t, b.handleCommand(t.Context(), msg))

	assert.Empty(t, fr.spawnCalls)

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].params["text"], "only runs inside the configured forum supergroup")
}

func TestHandleSpawn_plainGroup_silentlyDropped(t *testing.T) {
	b, api, fr := forumSpawnBot(t)

	// A plain (non-supergroup) group is not a forum; /spawn there must not be
	// routed to the forum path at all — silent drop, no presence-confirming
	// reply, no spawn.
	msg := forumSpawnMsg(0, 99)
	msg.Chat.Type = "group"

	require.NoError(t, b.handleCommand(t.Context(), msg))

	assert.Empty(t, fr.spawnCalls, "no spawn from a plain group")
	assert.Empty(t, api.recordedCalls("sendMessage"), "no reply leaks into a plain group")
}

func TestHandleSpawn_forumTopic_errorReplyThreaded(t *testing.T) {
	b, api, fr := forumSpawnBot(t)
	fr.spawnErr = errors.New("boom")

	require.NoError(t, b.handleCommand(t.Context(), forumSpawnMsg(9, 99)))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].params["text"], "Spawn failed")
	assert.EqualValues(t, 9, calls[0].params["message_thread_id"], "error reply lands in the topic")
}

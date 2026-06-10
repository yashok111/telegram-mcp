package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/bot"
)

func TestBgRunner_EmptyList(t *testing.T) {
	r := NewBgRunner(DefaultBgConfig())
	assert.Empty(t, r.List())
}

func TestBgRunner_CancelUnknown(t *testing.T) {
	r := NewBgRunner(DefaultBgConfig())
	assert.ErrorIs(t, r.Cancel("nope"), ErrTaskNotFound)
}

func TestBgRunner_ReserveSlotEnforcesMaxParallel(t *testing.T) {
	r := NewBgRunner(BgConfig{MaxParallel: 2, RatePerHourPerUser: 99})
	id1, err := r.reserveSlot("u1")
	require.NoError(t, err)
	id2, err := r.reserveSlot("u1")
	require.NoError(t, err)
	_, err = r.reserveSlot("u2")
	require.ErrorIs(t, err, ErrTooManyBgTasks)
	assert.NotEqual(t, id1, id2)
}

func TestBgRunner_ReserveSlotRateLimitsPerUser(t *testing.T) {
	r := NewBgRunner(BgConfig{MaxParallel: 99, RatePerHourPerUser: 2})
	_, err := r.reserveSlot("u1")
	require.NoError(t, err)
	r.releaseSlot(mustReserve(t, r, "u1"), BgStatusDone)
	_, err = r.reserveSlot("u1")
	require.ErrorIs(t, err, ErrRateLimited)
	_, err = r.reserveSlot("u2")
	require.NoError(t, err)
}

func TestBgRunner_ReleaseSlotFreesParallelSlot(t *testing.T) {
	r := NewBgRunner(BgConfig{MaxParallel: 1, RatePerHourPerUser: 99})
	id, err := r.reserveSlot("u1")
	require.NoError(t, err)
	_, err = r.reserveSlot("u1")
	require.ErrorIs(t, err, ErrTooManyBgTasks)
	r.releaseSlot(id, BgStatusDone)
	_, err = r.reserveSlot("u1")
	require.NoError(t, err)
}

func TestBgRunner_PerUserMapDropsStaleKeys(t *testing.T) {
	r := NewBgRunner(BgConfig{MaxParallel: 99, RatePerHourPerUser: 99})

	// Seed 5 users with stale timestamps (>1h ago).
	stale := time.Now().Add(-2 * time.Hour)

	r.mu.Lock()
	for i := range 5 {
		uid := fmt.Sprintf("u_stale_%d", i)
		r.perUser[uid] = []time.Time{stale}
	}
	r.mu.Unlock()

	// One fresh user's reserveSlot triggers gc.
	_, err := r.reserveSlot("u_fresh")
	require.NoError(t, err)

	r.mu.Lock()
	for i := range 5 {
		uid := fmt.Sprintf("u_stale_%d", i)
		_, present := r.perUser[uid]
		assert.False(t, present, "%s should have been GC'd", uid)
	}

	_, presentFresh := r.perUser["u_fresh"]
	assert.True(t, presentFresh)
	r.mu.Unlock()
}

func TestBgRunner_TaskIDsAreUnique(t *testing.T) {
	r := NewBgRunner(BgConfig{MaxParallel: 1000, RatePerHourPerUser: 1000})
	seen := map[string]bool{}

	for range 100 {
		id, err := r.reserveSlot("u1")
		require.NoError(t, err)
		assert.False(t, seen[id], "duplicate id %s", id)
		seen[id] = true
	}
}

func TestBgRunner_DefaultsAppliedForZeroValues(t *testing.T) {
	r := NewBgRunner(BgConfig{})
	assert.Equal(t, 3, r.cfg.MaxParallel)
	assert.Equal(t, "claude", r.cfg.ClaudeBin)
	assert.Positive(t, int64(r.cfg.Timeout))
	assert.Positive(t, int64(r.cfg.EditThrottle))
	assert.Equal(t, 10, r.cfg.RatePerHourPerUser)
}

func TestBgRunner_StopCancelsAllTasks(t *testing.T) {
	r := NewBgRunner(BgConfig{MaxParallel: 99, RatePerHourPerUser: 99})

	canceled := make(map[string]bool)

	for range 3 {
		id, err := r.reserveSlot("u1")
		require.NoError(t, err)

		r.mu.Lock()
		taskID := id
		r.tasks[taskID].cancel = func() { canceled[taskID] = true }
		r.mu.Unlock()
	}

	r.Stop()

	assert.Len(t, canceled, 3, "every in-flight task should have been cancelled")
}

func TestBgRunner_ListReflectsReserve(t *testing.T) {
	r := NewBgRunner(BgConfig{MaxParallel: 5, RatePerHourPerUser: 99})
	id, err := r.reserveSlot("u1")
	require.NoError(t, err)

	infos := r.List()
	require.Len(t, infos, 1)
	assert.Equal(t, id, infos[0].ID)
	assert.Equal(t, "u1", infos[0].UserID)
	assert.Equal(t, string(BgStatusRunning), infos[0].Status)
}

func mustReserve(t *testing.T, r *BgRunner, u string) string {
	t.Helper()

	id, err := r.reserveSlot(u)
	require.NoError(t, err)

	return id
}

// lockedBot wraps fakeBot with a mutex so the test goroutine can read state
// while runTask's goroutine writes via the botSurface methods. The fakeBot in
// handlers_test.go is single-threaded by construction; tests here run async.
type lockedBot struct {
	mu sync.Mutex
	fb *fakeBot
}

func newLockedBot() *lockedBot { return &lockedBot{fb: &fakeBot{}} }

func (b *lockedBot) SendMessage(ctx context.Context, chatID, text string, opts bot.SendOpts) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.fb.SendMessage(ctx, chatID, text, opts)
}

func (b *lockedBot) SendFile(ctx context.Context, chatID, path string, opts bot.SendOpts) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.fb.SendFile(ctx, chatID, path, opts)
}

func (b *lockedBot) EditMessage(ctx context.Context, chatID string, msgID int, text, pm string) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.fb.EditMessage(ctx, chatID, msgID, text, pm)
}

func (b *lockedBot) React(ctx context.Context, chatID string, msgID int, emoji string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.fb.React(ctx, chatID, msgID, emoji)
}

func (b *lockedBot) DownloadFile(ctx context.Context, id string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.fb.DownloadFile(ctx, id)
}

func (b *lockedBot) SendPermissionPrompt(ctx context.Context, target bot.PermissionTarget, prefix, reqID, tool string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.fb.SendPermissionPrompt(ctx, target, prefix, reqID, tool)
}

func (b *lockedBot) BroadcastMutationConfirm(ctx context.Context, target bot.PermissionTarget, pendingID, summary string) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.fb.BroadcastMutationConfirm(ctx, target, pendingID, summary)
}

func (b *lockedBot) SendChatAction(ctx context.Context, chatID, action string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.fb.SendChatAction(ctx, chatID, action)
}

func (b *lockedBot) setSendRet(id int, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.fb.sentMessage.retID = id
	b.fb.sentMessage.retErr = err
}

func (b *lockedBot) lastEditedText() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.fb.editedMessage.text
}

func (b *lockedBot) lastEditedParseMode() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.fb.editedMessage.parseMode
}

func (b *lockedBot) lastSentText() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.fb.sentMessage.text
}

type fakeProcess struct {
	stdout io.ReadCloser
	stderr io.ReadCloser
	waitCh chan error
	pid    int
	signal func(os.Signal) error
}

func (p *fakeProcess) Stdout() io.ReadCloser    { return p.stdout }
func (p *fakeProcess) Stderr() io.ReadCloser    { return p.stderr }
func (p *fakeProcess) Pid() int                 { return p.pid }
func (p *fakeProcess) Signal(s os.Signal) error { return p.signal(s) }
func (p *fakeProcess) Wait() error              { return <-p.waitCh }

type fakeCommander struct {
	startFn func(ctx context.Context, workdir, bin string, args, env []string) (Process, error)
}

func (f *fakeCommander) Start(ctx context.Context, workdir, bin string, args, env []string) (Process, error) {
	return f.startFn(ctx, workdir, bin, args, env)
}

func TestBgRunner_SpawnSendFailureReleasesSlot(t *testing.T) {
	fb := newLockedBot()
	fb.setSendRet(0, errors.New("nope"))

	r := NewBgRunnerWithDeps(
		BgConfig{MaxParallel: 1, RatePerHourPerUser: 99},
		fb,
		&fakeCommander{startFn: nil},
	)

	_, err := r.Spawn(context.Background(), bot.BgSpawnRequest{Prompt: "x", ChatID: "1", UserID: "u"})
	require.Error(t, err)
	assert.Empty(t, r.List(), "failed initial send must release the slot")
}

func TestBgRunner_SpawnRejectsEmptyPrompt(t *testing.T) {
	r := NewBgRunnerWithDeps(DefaultBgConfig(), newLockedBot(), &fakeCommander{})
	_, err := r.Spawn(context.Background(), bot.BgSpawnRequest{Prompt: "   "})
	assert.ErrorIs(t, err, ErrEmptyPrompt)
}

func TestBgRunner_SpawnHappyPath(t *testing.T) {
	pr, pw := io.Pipe()
	stderrR, stderrW := io.Pipe()
	waitCh := make(chan error, 1)

	cmder := &fakeCommander{startFn: func(_ context.Context, _, _ string, _, _ []string) (Process, error) {
		return &fakeProcess{
			stdout: pr,
			stderr: stderrR,
			waitCh: waitCh,
			signal: func(os.Signal) error { return nil },
		}, nil
	}}
	fb := newLockedBot()
	fb.setSendRet(100, nil)

	r := NewBgRunnerWithDeps(BgConfig{
		MaxParallel:        1,
		RatePerHourPerUser: 99,
		EditThrottle:       50 * time.Millisecond,
		Timeout:            5 * time.Second,
	}, fb, cmder)

	id, err := r.Spawn(context.Background(), bot.BgSpawnRequest{Prompt: "say hi", ChatID: "1", UserID: "u"})
	require.NoError(t, err)
	require.NotEmpty(t, id)

	go func() {
		defer pw.Close()
		defer stderrW.Close()

		_, _ = pw.Write([]byte(`{"type":"system","subtype":"init"}` + "\n"))
		_, _ = pw.Write([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]}}` + "\n"))
		_, _ = pw.Write([]byte(`{"type":"result","subtype":"success","is_error":false,"duration_ms":1,"num_turns":1,"result":"hi!","total_cost_usd":0.01}` + "\n"))
	}()

	waitCh <- nil

	require.Eventually(t, func() bool { return len(r.List()) == 0 }, 2*time.Second, 10*time.Millisecond)

	assert.Contains(t, fb.lastEditedText(), "✅ Task "+bot.MdCode(id))
	assert.Contains(t, fb.lastEditedText(), "done")
	assert.Equal(t, "MarkdownV2", fb.lastEditedParseMode())
	assert.Contains(t, fb.lastSentText(), "hi!")
}

func TestBgRunner_CancelSendsSIGTERMAndMarks(t *testing.T) {
	pr, pw := io.Pipe()
	stderrR, stderrW := io.Pipe()
	waitCh := make(chan error, 1)
	sigSeen := make(chan os.Signal, 4)

	cmder := &fakeCommander{startFn: func(_ context.Context, _, _ string, _, _ []string) (Process, error) {
		return &fakeProcess{
			stdout: pr,
			stderr: stderrR,
			waitCh: waitCh,
			signal: func(s os.Signal) error { sigSeen <- s; return nil },
		}, nil
	}}
	fb := newLockedBot()
	fb.setSendRet(7, nil)

	r := NewBgRunnerWithDeps(BgConfig{
		MaxParallel:        1,
		RatePerHourPerUser: 99,
		EditThrottle:       50 * time.Millisecond,
		Timeout:            10 * time.Second,
	}, fb, cmder)

	id, err := r.Spawn(context.Background(), bot.BgSpawnRequest{Prompt: "long task", ChatID: "1", UserID: "u"})
	require.NoError(t, err)

	require.NoError(t, r.Cancel(id))

	go func() {
		select {
		case <-sigSeen:
			_ = pw.Close()
			_ = stderrW.Close()

			waitCh <- errors.New("signal: terminated")
		case <-time.After(2 * time.Second):
		}
	}()

	require.Eventually(t, func() bool { return len(r.List()) == 0 }, 3*time.Second, 20*time.Millisecond)
	assert.Contains(t, fb.lastEditedText(), "🛑 Task "+bot.MdCode(id))
	assert.Contains(t, fb.lastEditedText(), "cancelled")
	assert.Equal(t, "MarkdownV2", fb.lastEditedParseMode())
}

func TestBgRunner_StartFailureMarksFailed(t *testing.T) {
	cmder := &fakeCommander{startFn: func(_ context.Context, _, _ string, _, _ []string) (Process, error) {
		return nil, errors.New("not found in $PATH")
	}}
	fb := newLockedBot()
	fb.setSendRet(11, nil)

	r := NewBgRunnerWithDeps(BgConfig{
		MaxParallel:        1,
		RatePerHourPerUser: 99,
		EditThrottle:       50 * time.Millisecond,
		Timeout:            time.Second,
	}, fb, cmder)

	id, err := r.Spawn(context.Background(), bot.BgSpawnRequest{Prompt: "x", ChatID: "1", UserID: "u"})
	require.NoError(t, err)

	require.Eventually(t, func() bool { return len(r.List()) == 0 }, 2*time.Second, 20*time.Millisecond)
	assert.Contains(t, fb.lastEditedText(), "❌ Task "+bot.MdCode(id))
	assert.Equal(t, "MarkdownV2", fb.lastEditedParseMode())
}

func TestBgRunner_WorkdirFallbackHome(t *testing.T) {
	t.Setenv("HOME", "/tmp/home-x")

	var (
		mu      sync.Mutex
		seenDir string
	)

	cmder := &fakeCommander{startFn: func(_ context.Context, dir, _ string, _, _ []string) (Process, error) {
		mu.Lock()
		seenDir = dir
		mu.Unlock()

		return nil, errors.New("stop here")
	}}
	fb := newLockedBot()
	fb.setSendRet(1, nil)

	r := NewBgRunnerWithDeps(BgConfig{
		MaxParallel:        1,
		RatePerHourPerUser: 99,
		EditThrottle:       50 * time.Millisecond,
		Timeout:            time.Second,
	}, fb, cmder)

	_, err := r.Spawn(context.Background(), bot.BgSpawnRequest{Prompt: "x", ChatID: "1", UserID: "u"})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()

		return seenDir != ""
	}, time.Second, 10*time.Millisecond)

	mu.Lock()
	got := seenDir
	mu.Unlock()
	assert.Equal(t, "/tmp/home-x", got)

	require.Eventually(t, func() bool { return len(r.List()) == 0 }, time.Second, 10*time.Millisecond)
}

func TestBgRunner_SpawnRespectsRateLimit(t *testing.T) {
	cmder := &fakeCommander{startFn: func(_ context.Context, _, _ string, _, _ []string) (Process, error) {
		return nil, errors.New("stop")
	}}
	fb := newLockedBot()
	fb.setSendRet(1, nil)

	r := NewBgRunnerWithDeps(BgConfig{
		MaxParallel:        99,
		RatePerHourPerUser: 1,
		EditThrottle:       50 * time.Millisecond,
		Timeout:            time.Second,
	}, fb, cmder)

	_, _ = r.Spawn(context.Background(), bot.BgSpawnRequest{Prompt: "x", ChatID: "1", UserID: "u"})

	_, err := r.Spawn(context.Background(), bot.BgSpawnRequest{Prompt: "y", ChatID: "1", UserID: "u"})
	require.ErrorIs(t, err, ErrRateLimited)

	require.Eventually(t, func() bool { return len(r.List()) == 0 }, time.Second, 10*time.Millisecond)
}

type recordedBgCall struct {
	args []string
	env  []string
}

type recordingBgCommander struct {
	mu     sync.Mutex
	calls  []recordedBgCall
	stdout func() io.ReadCloser
	stderr func() io.ReadCloser
	waitCh chan error
}

func (r *recordingBgCommander) Start(_ context.Context, _, _ string, args, env []string) (Process, error) {
	r.mu.Lock()
	r.calls = append(r.calls, recordedBgCall{args: slices.Clone(args), env: slices.Clone(env)})
	r.mu.Unlock()

	return &fakeProcess{
		stdout: r.stdout(),
		stderr: r.stderr(),
		waitCh: r.waitCh,
		signal: func(os.Signal) error { return nil },
	}, nil
}

func (r *recordingBgCommander) lastCall() recordedBgCall {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.calls) == 0 {
		return recordedBgCall{}
	}

	return r.calls[len(r.calls)-1]
}

// newRecordingBgRunner wires a recordingBgCommander that emits a clean result
// event so runTask completes deterministically; returns the runner and the
// commander so tests can read the captured args/env after Spawn finishes.
func newRecordingBgRunner(t *testing.T) (*BgRunner, *recordingBgCommander, *lockedBot) {
	t.Helper()

	pr, pw := io.Pipe()
	stderrR, stderrW := io.Pipe()
	waitCh := make(chan error, 1)

	cmder := &recordingBgCommander{
		stdout: func() io.ReadCloser { return pr },
		stderr: func() io.ReadCloser { return stderrR },
		waitCh: waitCh,
	}

	fb := newLockedBot()
	fb.setSendRet(100, nil)

	r := NewBgRunnerWithDeps(BgConfig{
		MaxParallel:        1,
		RatePerHourPerUser: 99,
		EditThrottle:       50 * time.Millisecond,
		Timeout:            5 * time.Second,
	}, fb, cmder)

	go func() {
		defer pw.Close()
		defer stderrW.Close()

		_, _ = pw.Write([]byte(`{"type":"result","subtype":"success","is_error":false,"duration_ms":1,"num_turns":1,"result":"ok","total_cost_usd":0.01}` + "\n"))
	}()

	waitCh <- nil

	return r, cmder, fb
}

func TestBgRunner_RunTaskAppendsModelArg(t *testing.T) {
	r, cmder, _ := newRecordingBgRunner(t)

	id, err := r.Spawn(context.Background(), bot.BgSpawnRequest{
		Prompt: "say hi",
		ChatID: "1",
		UserID: "u",
		Model:  "claude-haiku-4-5",
	})
	require.NoError(t, err)
	require.NotEmpty(t, id)

	require.Eventually(t, func() bool { return len(r.List()) == 0 }, 2*time.Second, 10*time.Millisecond)

	call := cmder.lastCall()
	require.NotEmpty(t, call.args)
	assert.Contains(t, call.args, "--model=claude-haiku-4-5")
	assert.Equal(t, "say hi", call.args[len(call.args)-1], "prompt must be the last positional arg")
}

func TestBgRunner_RunTaskSetsThinkingTokensEnv(t *testing.T) {
	t.Setenv("BG_TEST_SENTINEL", "present")

	r, cmder, _ := newRecordingBgRunner(t)

	id, err := r.Spawn(context.Background(), bot.BgSpawnRequest{
		Prompt:         "say hi",
		ChatID:         "1",
		UserID:         "u",
		ThinkingTokens: 8000,
	})
	require.NoError(t, err)
	require.NotEmpty(t, id)

	require.Eventually(t, func() bool { return len(r.List()) == 0 }, 2*time.Second, 10*time.Millisecond)

	call := cmder.lastCall()
	require.NotEmpty(t, call.env)
	assert.Contains(t, call.env, "MAX_THINKING_TOKENS=8000")
	assert.Contains(t, call.env, "BG_TEST_SENTINEL=present", "must inherit parent env, not replace it")
}

func TestBgRunner_RunTaskZeroThinkingStripsCCEnvButInherits(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sid-from-parent-cc")
	t.Setenv("BG_ZERO_SENTINEL", "keep-me")

	r, cmder, _ := newRecordingBgRunner(t)

	id, err := r.Spawn(context.Background(), bot.BgSpawnRequest{
		Prompt: "say hi",
		ChatID: "1",
		UserID: "u",
	})
	require.NoError(t, err)
	require.NotEmpty(t, id)

	require.Eventually(t, func() bool { return len(r.List()) == 0 }, 2*time.Second, 10*time.Millisecond)

	call := cmder.lastCall()
	for _, a := range call.args {
		assert.NotContains(t, a, "--model=", "no --model arg when Model is empty")
	}

	// Even with no MAX_THINKING_TOKENS, the child must not inherit the daemon's
	// foreign CC session identity — but must still inherit unrelated parent env.
	require.NotEmpty(t, call.env, "env is now always set so CC vars can be stripped")

	for _, e := range call.env {
		assert.False(t, strings.HasPrefix(e, "CLAUDECODE="), "must strip inherited CLAUDECODE")
		assert.False(t, strings.HasPrefix(e, "CLAUDE_CODE_SESSION_ID="), "must strip inherited CLAUDE_CODE_SESSION_ID")
		assert.False(t, strings.HasPrefix(e, "MAX_THINKING_TOKENS="), "no MAX_THINKING_TOKENS when zero")
	}

	assert.Contains(t, call.env, "BG_ZERO_SENTINEL=keep-me", "must inherit unrelated parent env")
}

func TestBgRunner_StartFailureEmitsBgFailed(t *testing.T) {
	cmder := &fakeCommander{startFn: func(_ context.Context, _, _ string, _, _ []string) (Process, error) {
		return nil, errors.New("exec not found")
	}}
	fb := newLockedBot()
	fb.setSendRet(42, nil)

	r := NewBgRunnerWithDeps(BgConfig{
		MaxParallel:        1,
		RatePerHourPerUser: 99,
		EditThrottle:       50 * time.Millisecond,
		Timeout:            time.Second,
	}, fb, cmder)

	sink := &recordingSink{}
	r.SetEventSink(sink)

	_, err := r.Spawn(context.Background(), bot.BgSpawnRequest{Prompt: "x", ChatID: "1", UserID: "u"})
	require.NoError(t, err)

	require.Eventually(t, func() bool { return sink.typeCount("bg_failed") == 1 }, 2*time.Second, 20*time.Millisecond)
}

func TestBgRunner_StreamFailureEmitsBgFailed(t *testing.T) {
	pr, pw := io.Pipe()
	stderrR, stderrW := io.Pipe()
	waitCh := make(chan error, 1)

	cmder := &fakeCommander{startFn: func(_ context.Context, _, _ string, _, _ []string) (Process, error) {
		return &fakeProcess{
			stdout: pr,
			stderr: stderrR,
			waitCh: waitCh,
			signal: func(os.Signal) error { return nil },
		}, nil
	}}
	fb := newLockedBot()
	fb.setSendRet(55, nil)

	r := NewBgRunnerWithDeps(BgConfig{
		MaxParallel:        1,
		RatePerHourPerUser: 99,
		EditThrottle:       50 * time.Millisecond,
		Timeout:            5 * time.Second,
	}, fb, cmder)

	sink := &recordingSink{}
	r.SetEventSink(sink)

	_, err := r.Spawn(context.Background(), bot.BgSpawnRequest{Prompt: "x", ChatID: "1", UserID: "u"})
	require.NoError(t, err)

	go func() {
		defer pw.Close()
		defer stderrW.Close()
	}()

	waitCh <- nil

	require.Eventually(t, func() bool { return sink.typeCount("bg_failed") == 1 }, 2*time.Second, 20*time.Millisecond)
}

func TestBgRunner_CancelDoesNotEmitBgFailed(t *testing.T) {
	pr, pw := io.Pipe()
	stderrR, stderrW := io.Pipe()
	waitCh := make(chan error, 1)

	cmder := &fakeCommander{startFn: func(_ context.Context, _, _ string, _, _ []string) (Process, error) {
		return &fakeProcess{
			stdout: pr,
			stderr: stderrR,
			waitCh: waitCh,
			signal: func(os.Signal) error { return nil },
		}, nil
	}}
	fb := newLockedBot()
	fb.setSendRet(77, nil)

	r := NewBgRunnerWithDeps(BgConfig{
		MaxParallel:        1,
		RatePerHourPerUser: 99,
		EditThrottle:       50 * time.Millisecond,
		Timeout:            5 * time.Second,
	}, fb, cmder)

	sink := &recordingSink{}
	r.SetEventSink(sink)

	id, err := r.Spawn(context.Background(), bot.BgSpawnRequest{Prompt: "x", ChatID: "1", UserID: "u"})
	require.NoError(t, err)

	// Cancel, then make the stream error out as a consequence of the kill. This
	// reliably drives the doneCh branch with ctx already cancelled — the exact
	// path that had no guard and emitted a false bg_failed before the fix.
	require.NoError(t, r.Cancel(id))

	go func() {
		_ = pw.CloseWithError(errors.New("killed"))
		_ = stderrW.Close()
	}()

	waitCh <- errors.New("signal: killed")

	// Whichever select branch wins, a cancelled task must terminate WITHOUT
	// emitting bg_failed (the false-anomaly the ctx.Err() guard prevents).
	require.Eventually(t, func() bool {
		for _, ti := range r.List() {
			if ti.ID == id {
				return false
			}
		}

		return true
	}, 2*time.Second, 20*time.Millisecond, "cancelled task must reach a terminal state")

	assert.Equal(t, 0, sink.typeCount("bg_failed"), "cancelled task must not emit bg_failed")
}

func TestBgRunner_RunTaskDedupesThinkingTokensEnv(t *testing.T) {
	t.Setenv("MAX_THINKING_TOKENS", "5000")

	r, cmder, _ := newRecordingBgRunner(t)

	id, err := r.Spawn(context.Background(), bot.BgSpawnRequest{
		Prompt:         "say hi",
		ChatID:         "1",
		UserID:         "u",
		ThinkingTokens: 16000,
	})
	require.NoError(t, err)
	require.NotEmpty(t, id)

	require.Eventually(t, func() bool { return len(r.List()) == 0 }, 2*time.Second, 10*time.Millisecond)

	call := cmder.lastCall()
	matches := 0

	for _, e := range call.env {
		if len(e) >= 20 && e[:20] == "MAX_THINKING_TOKENS=" {
			matches++
		}
	}

	assert.Equal(t, 1, matches, "exactly one MAX_THINKING_TOKENS entry expected; parent's value must be filtered before append")
	assert.Contains(t, call.env, "MAX_THINKING_TOKENS=16000", "the request-supplied value must win")
}

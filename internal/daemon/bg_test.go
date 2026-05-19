package daemon

import (
	"context"
	"errors"
	"io"
	"os"
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

func (b *lockedBot) BroadcastPermissionRequest(ctx context.Context, prefix, reqID, tool string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fb.BroadcastPermissionRequest(ctx, prefix, reqID, tool)
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
	startFn func(ctx context.Context, workdir, bin string, args []string) (Process, error)
}

func (f *fakeCommander) Start(ctx context.Context, workdir, bin string, args []string) (Process, error) {
	return f.startFn(ctx, workdir, bin, args)
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

	cmder := &fakeCommander{startFn: func(_ context.Context, _, _ string, _ []string) (Process, error) {
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

	assert.Contains(t, fb.lastEditedText(), "✅ Task "+id+" done")
	assert.Contains(t, fb.lastSentText(), "hi!")
}

func TestBgRunner_CancelSendsSIGTERMAndMarks(t *testing.T) {
	pr, pw := io.Pipe()
	stderrR, stderrW := io.Pipe()
	waitCh := make(chan error, 1)
	sigSeen := make(chan os.Signal, 4)

	cmder := &fakeCommander{startFn: func(_ context.Context, _, _ string, _ []string) (Process, error) {
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
	assert.Contains(t, fb.lastEditedText(), "🛑 Task "+id+" cancelled")
}

func TestBgRunner_StartFailureMarksFailed(t *testing.T) {
	cmder := &fakeCommander{startFn: func(_ context.Context, _, _ string, _ []string) (Process, error) {
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
	assert.Contains(t, fb.lastEditedText(), "❌ Task "+id)
}

func TestBgRunner_WorkdirFallbackHome(t *testing.T) {
	t.Setenv("HOME", "/tmp/home-x")

	var (
		mu      sync.Mutex
		seenDir string
	)

	cmder := &fakeCommander{startFn: func(_ context.Context, dir, _ string, _ []string) (Process, error) {
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
	cmder := &fakeCommander{startFn: func(_ context.Context, _, _ string, _ []string) (Process, error) {
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

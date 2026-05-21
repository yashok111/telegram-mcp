package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/bot"
)

// recordingBot is a minimal botSurface that retains every Send/Edit call so
// /spawn tests can assert on full traces.
type recordingBot struct {
	mu        sync.Mutex
	sentTexts []string
	editTexts []string
	sendErr   error
	sendID    int
}

func newRecordingBot(sendID int) *recordingBot { return &recordingBot{sendID: sendID} }

func (b *recordingBot) SendMessage(_ context.Context, _, text string, _ bot.SendOpts) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.sendErr != nil {
		return 0, b.sendErr
	}

	b.sentTexts = append(b.sentTexts, text)

	return b.sendID, nil
}

func (b *recordingBot) SendFile(_ context.Context, _, _ string, _ bot.SendOpts) (int, error) {
	return 0, nil
}

func (b *recordingBot) EditMessage(_ context.Context, _ string, _ int, text, _ string) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.editTexts = append(b.editTexts, text)

	return 0, nil
}

func (b *recordingBot) React(_ context.Context, _ string, _ int, _ string) error     { return nil }
func (b *recordingBot) SendChatAction(_ context.Context, _, _ string) error          { return nil }
func (b *recordingBot) DownloadFile(_ context.Context, _ string) (string, error)     { return "", nil }
func (b *recordingBot) BroadcastPermissionRequest(_ context.Context, _, _, _ string) {}

func (b *recordingBot) sent() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make([]string, len(b.sentTexts))
	copy(out, b.sentTexts)

	return out
}

// fakeSpawnProcess is a no-pipe stand-in for execSpawnProcess. Signal handler
// pushes the signal to sigSeen, then closes waitCh so Wait returns.
type fakeSpawnProcess struct {
	pid     int
	waitCh  chan error
	closeCh chan struct{}
	signal  func(os.Signal) error
	closeFn func() error
}

func (p *fakeSpawnProcess) Pid() int                 { return p.pid }
func (p *fakeSpawnProcess) Signal(s os.Signal) error { return p.signal(s) }
func (p *fakeSpawnProcess) Wait() error              { return <-p.waitCh }
func (p *fakeSpawnProcess) Close() error             { return p.closeFn() }

type spawnCmdCall struct {
	workdir, bin string
	args, env    []string
}

type fakeSpawnCommander struct {
	mu      sync.Mutex
	startFn func(ctx context.Context, workdir, bin string, args, env []string) (SpawnProcess, error)
	calls   []spawnCmdCall
}

func (f *fakeSpawnCommander) Start(ctx context.Context, workdir, bin string, args, env []string) (SpawnProcess, error) {
	f.mu.Lock()
	f.calls = append(f.calls, spawnCmdCall{workdir: workdir, bin: bin, args: args, env: env})
	f.mu.Unlock()

	return f.startFn(ctx, workdir, bin, args, env)
}

func (f *fakeSpawnCommander) lastCall() spawnCmdCall {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.calls) == 0 {
		return spawnCmdCall{}
	}

	return f.calls[len(f.calls)-1]
}

// newFakeSpawnProcess builds a process whose Signal handler triggers waitCh
// release on first SIGTERM, simulating a well-behaved child.
func newFakeSpawnProcess(pid int) *fakeSpawnProcess {
	waitCh := make(chan error, 1)

	var once sync.Once

	closeFn := func() error { return nil }

	p := &fakeSpawnProcess{
		pid:     pid,
		waitCh:  waitCh,
		closeCh: make(chan struct{}),
		closeFn: closeFn,
	}
	p.signal = func(_ os.Signal) error {
		once.Do(func() { waitCh <- nil })

		return nil
	}

	return p
}

func TestSpawnRunner_DefaultsAppliedForZeroValues(t *testing.T) {
	r := NewSpawnRunner(SpawnConfig{})
	assert.Equal(t, 3, r.cfg.MaxParallel)
	assert.Equal(t, "claude", r.cfg.ClaudeBin)
	assert.Positive(t, int64(r.cfg.HardTimeout))
	assert.Equal(t, 5, r.cfg.RatePerHourPerUser)
	assert.NotEmpty(t, r.cfg.ClaudeArgs, "default args must load the telegram plugin")
}

func TestSpawnRunner_EmptyList(t *testing.T) {
	r := NewSpawnRunner(DefaultSpawnConfig())
	assert.Empty(t, r.List())
}

func TestSpawnRunner_CancelUnknown(t *testing.T) {
	r := NewSpawnRunner(DefaultSpawnConfig())
	assert.ErrorIs(t, r.Cancel("nope"), ErrSpawnNotFound)
}

func TestSpawnRunner_ReserveSlotEnforcesMaxParallel(t *testing.T) {
	r := NewSpawnRunner(SpawnConfig{MaxParallel: 2, RatePerHourPerUser: 99})
	id1, err := r.reserveSlot("u1")
	require.NoError(t, err)
	id2, err := r.reserveSlot("u1")
	require.NoError(t, err)
	_, err = r.reserveSlot("u2")
	require.ErrorIs(t, err, ErrTooManySpawnTasks)
	assert.NotEqual(t, id1, id2)
}

func TestSpawnRunner_ReserveSlotRateLimitsPerUser(t *testing.T) {
	r := NewSpawnRunner(SpawnConfig{MaxParallel: 99, RatePerHourPerUser: 2})

	_, err := r.reserveSlot("u1")
	require.NoError(t, err)

	id2, err := r.reserveSlot("u1")
	require.NoError(t, err)
	r.releaseSlot(id2, SpawnStatusDone)

	_, err = r.reserveSlot("u1")
	require.ErrorIs(t, err, ErrSpawnRateLimited)
	_, err = r.reserveSlot("u2")
	require.NoError(t, err)
}

func TestSpawnRunner_PerUserMapDropsStaleKeys(t *testing.T) {
	r := NewSpawnRunner(SpawnConfig{MaxParallel: 99, RatePerHourPerUser: 99})

	stale := time.Now().Add(-2 * time.Hour)
	r.mu.Lock()
	for i := range 5 {
		r.perUser[fmt.Sprintf("u_stale_%d", i)] = []time.Time{stale}
	}
	r.mu.Unlock()

	_, err := r.reserveSlot("u_fresh")
	require.NoError(t, err)

	r.mu.Lock()
	for i := range 5 {
		_, present := r.perUser[fmt.Sprintf("u_stale_%d", i)]
		assert.False(t, present, "stale user %d should have been GC'd", i)
	}
	r.mu.Unlock()
}

func TestSpawnRunner_SpawnPassesWorkdirArgsAndEnvWithSpawnID(t *testing.T) {
	proc := newFakeSpawnProcess(4242)
	cmder := &fakeSpawnCommander{startFn: func(_ context.Context, _, _ string, _, _ []string) (SpawnProcess, error) {
		return proc, nil
	}}
	fb := newRecordingBot(100)

	r := NewSpawnRunnerWithDeps(SpawnConfig{
		MaxParallel:        1,
		RatePerHourPerUser: 99,
		HardTimeout:        time.Minute,
		ClaudeBin:          "/usr/local/bin/claude",
		ClaudeArgs:         []string{"--dangerously-load-development-channels", "plugin:telegram@local-yakov"},
	}, fb, cmder)

	id, err := r.Spawn(context.Background(), bot.SpawnRequest{
		Workdir: "/tmp/wd", ChatID: "1", UserID: "u",
	})
	require.NoError(t, err)
	require.NotEmpty(t, id)

	call := cmder.lastCall()
	assert.Equal(t, "/tmp/wd", call.workdir)
	assert.Equal(t, "/usr/local/bin/claude", call.bin)
	assert.Equal(t, []string{"--dangerously-load-development-channels", "plugin:telegram@local-yakov"}, call.args)
	assert.True(t, slices.Contains(call.env, "TELEGRAM_SPAWN_ID="+id), "env must carry the spawn_id so the spawned shim can stamp it in hello")

	require.NoError(t, r.Cancel(id))
	require.Eventually(t, func() bool { return len(r.List()) == 0 }, 3*time.Second, 20*time.Millisecond)
}

func TestSpawnRunner_SpawnPostsStartConfirmation(t *testing.T) {
	proc := newFakeSpawnProcess(123)
	cmder := &fakeSpawnCommander{startFn: func(_ context.Context, _, _ string, _, _ []string) (SpawnProcess, error) {
		return proc, nil
	}}
	fb := newRecordingBot(100)

	r := NewSpawnRunnerWithDeps(SpawnConfig{
		MaxParallel:        1,
		RatePerHourPerUser: 99,
		HardTimeout:        time.Minute,
	}, fb, cmder)

	id, err := r.Spawn(context.Background(), bot.SpawnRequest{ChatID: "1", UserID: "u", Workdir: "/x"})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		for _, s := range fb.sent() {
			if contains(s, "🚀 Spawn "+id+" started") {
				return true
			}
		}

		return false
	}, time.Second, 20*time.Millisecond)

	require.NoError(t, r.Cancel(id))
	require.Eventually(t, func() bool { return len(r.List()) == 0 }, 3*time.Second, 20*time.Millisecond)
}

func TestSpawnRunner_SpawnStartFailureReleasesSlot(t *testing.T) {
	cmder := &fakeSpawnCommander{startFn: func(_ context.Context, _, _ string, _, _ []string) (SpawnProcess, error) {
		return nil, errors.New("not found in $PATH")
	}}
	fb := newRecordingBot(100)

	r := NewSpawnRunnerWithDeps(SpawnConfig{
		MaxParallel:        1,
		RatePerHourPerUser: 99,
		HardTimeout:        time.Minute,
	}, fb, cmder)

	_, err := r.Spawn(context.Background(), bot.SpawnRequest{ChatID: "1", UserID: "u"})
	require.Error(t, err)
	assert.Empty(t, r.List(), "start failure must release the slot")

	var sawFailure bool

	for _, s := range fb.sent() {
		if contains(s, "❌ Spawn") {
			sawFailure = true
		}
	}

	assert.True(t, sawFailure, "start failure must post an error message to chat")
}

func TestSpawnRunner_CancelSendsSIGTERMAndReapsSlot(t *testing.T) {
	sigSeen := make(chan os.Signal, 2)
	proc := newFakeSpawnProcess(9000)
	signalFn := proc.signal
	proc.signal = func(s os.Signal) error {
		sigSeen <- s

		return signalFn(s)
	}

	cmder := &fakeSpawnCommander{startFn: func(_ context.Context, _, _ string, _, _ []string) (SpawnProcess, error) {
		return proc, nil
	}}

	r := NewSpawnRunnerWithDeps(SpawnConfig{
		MaxParallel:        1,
		RatePerHourPerUser: 99,
		HardTimeout:        time.Minute,
	}, newRecordingBot(100), cmder)

	id, err := r.Spawn(context.Background(), bot.SpawnRequest{ChatID: "1", UserID: "u"})
	require.NoError(t, err)

	require.NoError(t, r.Cancel(id))

	select {
	case sig := <-sigSeen:
		assert.Equal(t, os.Signal(syscall.SIGTERM), sig)
	case <-time.After(2 * time.Second):
		t.Fatal("expected SIGTERM after Cancel")
	}

	require.Eventually(t, func() bool { return len(r.List()) == 0 }, 3*time.Second, 20*time.Millisecond)
}

func TestSpawnRunner_NaturalExitMarksDone(t *testing.T) {
	proc := newFakeSpawnProcess(5555)
	cmder := &fakeSpawnCommander{startFn: func(_ context.Context, _, _ string, _, _ []string) (SpawnProcess, error) {
		return proc, nil
	}}

	r := NewSpawnRunnerWithDeps(SpawnConfig{
		MaxParallel:        1,
		RatePerHourPerUser: 99,
		HardTimeout:        time.Minute,
	}, newRecordingBot(100), cmder)

	_, err := r.Spawn(context.Background(), bot.SpawnRequest{ChatID: "1", UserID: "u"})
	require.NoError(t, err)

	// Process exits on its own (no Cancel call).
	proc.waitCh <- nil

	require.Eventually(t, func() bool { return len(r.List()) == 0 }, 3*time.Second, 20*time.Millisecond)
}

func TestSpawnRunner_StopCancelsAll(t *testing.T) {
	r := NewSpawnRunner(SpawnConfig{MaxParallel: 99, RatePerHourPerUser: 99})

	var (
		mu       sync.Mutex
		canceled = map[string]bool{}
	)

	for range 3 {
		id, err := r.reserveSlot("u1")
		require.NoError(t, err)

		taskID := id

		r.mu.Lock()
		r.tasks[taskID].cancel = func() {
			mu.Lock()
			canceled[taskID] = true
			mu.Unlock()
		}
		r.mu.Unlock()
	}

	r.Stop()

	mu.Lock()
	defer mu.Unlock()

	assert.Len(t, canceled, 3)
}

func TestSpawnRunner_WorkdirFallbackHome(t *testing.T) {
	t.Setenv("HOME", "/tmp/home-spawn")

	cmder := &fakeSpawnCommander{startFn: func(_ context.Context, _, _ string, _, _ []string) (SpawnProcess, error) {
		return nil, errors.New("stop here")
	}}

	r := NewSpawnRunnerWithDeps(SpawnConfig{
		MaxParallel:        1,
		RatePerHourPerUser: 99,
		HardTimeout:        time.Minute,
	}, newRecordingBot(1), cmder)

	_, _ = r.Spawn(context.Background(), bot.SpawnRequest{ChatID: "1", UserID: "u"})

	assert.Equal(t, "/tmp/home-spawn", cmder.lastCall().workdir)
}

func TestSpawnRunner_WorkdirFallbackDefault(t *testing.T) {
	cmder := &fakeSpawnCommander{startFn: func(_ context.Context, _, _ string, _, _ []string) (SpawnProcess, error) {
		return nil, errors.New("stop here")
	}}

	r := NewSpawnRunnerWithDeps(SpawnConfig{
		MaxParallel:        1,
		RatePerHourPerUser: 99,
		HardTimeout:        time.Minute,
		DefaultWorkdir:     "/srv/code",
	}, newRecordingBot(1), cmder)

	_, _ = r.Spawn(context.Background(), bot.SpawnRequest{ChatID: "1", UserID: "u"})

	assert.Equal(t, "/srv/code", cmder.lastCall().workdir)
}

func TestSpawnRunner_ListReflectsActiveSpawns(t *testing.T) {
	proc := newFakeSpawnProcess(11)
	cmder := &fakeSpawnCommander{startFn: func(_ context.Context, _, _ string, _, _ []string) (SpawnProcess, error) {
		return proc, nil
	}}

	r := NewSpawnRunnerWithDeps(SpawnConfig{
		MaxParallel:        1,
		RatePerHourPerUser: 99,
		HardTimeout:        time.Minute,
	}, newRecordingBot(1), cmder)

	id, err := r.Spawn(context.Background(), bot.SpawnRequest{ChatID: "C1", UserID: "u", Workdir: "/x"})
	require.NoError(t, err)

	infos := r.List()
	require.Len(t, infos, 1)
	assert.Equal(t, id, infos[0].ID)
	assert.Equal(t, 11, infos[0].Pid)
	assert.Equal(t, "/x", infos[0].Workdir)
	assert.Equal(t, "C1", infos[0].ChatID)
	assert.Equal(t, string(SpawnStatusRunning), infos[0].Status)

	require.NoError(t, r.Cancel(id))
	require.Eventually(t, func() bool { return len(r.List()) == 0 }, 3*time.Second, 20*time.Millisecond)
}

func TestSpawnRunner_SweepIdle_CancelsWhenPairedShimIdleExceeds(t *testing.T) {
	r := NewSpawnRunner(SpawnConfig{
		MaxParallel: 5, RatePerHourPerUser: 99, HardTimeout: time.Hour, IdleTimeout: time.Hour,
	})

	id, err := r.reserveSlot("u")
	require.NoError(t, err)

	cancelled := make(chan struct{}, 1)

	r.mu.Lock()
	r.tasks[id].cancel = func() { cancelled <- struct{}{} }
	r.mu.Unlock()

	r.SetIdleLookup(func(string) (time.Duration, bool) { return 2 * time.Hour, true })

	r.sweepIdle(time.Now())

	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("idle-exceeded spawn must be cancelled")
	}
}

func TestSpawnRunner_SweepIdle_PairedShimUnderThresholdSurvives(t *testing.T) {
	r := NewSpawnRunner(SpawnConfig{
		MaxParallel: 5, RatePerHourPerUser: 99, HardTimeout: time.Hour, IdleTimeout: time.Hour,
	})

	id, err := r.reserveSlot("u")
	require.NoError(t, err)

	cancelled := make(chan struct{}, 1)

	r.mu.Lock()
	r.tasks[id].cancel = func() { cancelled <- struct{}{} }
	r.mu.Unlock()

	r.SetIdleLookup(func(string) (time.Duration, bool) { return 10 * time.Minute, true })

	r.sweepIdle(time.Now())

	select {
	case <-cancelled:
		t.Fatal("non-idle paired spawn must not be cancelled")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSpawnRunner_SweepIdle_OrphanCancelledAfterGrace(t *testing.T) {
	r := NewSpawnRunner(SpawnConfig{
		MaxParallel: 5, RatePerHourPerUser: 99, HardTimeout: time.Hour, IdleTimeout: time.Hour,
	})

	id, err := r.reserveSlot("u")
	require.NoError(t, err)

	cancelled := make(chan struct{}, 1)

	r.mu.Lock()
	r.tasks[id].info.StartedAt = time.Now().Add(-2 * time.Hour)
	r.tasks[id].cancel = func() { cancelled <- struct{}{} }
	r.mu.Unlock()

	r.SetIdleLookup(func(string) (time.Duration, bool) { return 0, false })

	r.sweepIdle(time.Now())

	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("orphan spawn past IdleTimeout must be cancelled")
	}
}

func TestSpawnRunner_SweepIdle_OrphanInsideGraceSurvives(t *testing.T) {
	r := NewSpawnRunner(SpawnConfig{
		MaxParallel: 5, RatePerHourPerUser: 99, HardTimeout: time.Hour, IdleTimeout: time.Hour,
	})

	id, err := r.reserveSlot("u")
	require.NoError(t, err)

	cancelled := make(chan struct{}, 1)

	r.mu.Lock()
	r.tasks[id].info.StartedAt = time.Now()
	r.tasks[id].cancel = func() { cancelled <- struct{}{} }
	r.mu.Unlock()

	r.SetIdleLookup(func(string) (time.Duration, bool) { return 0, false })

	r.sweepIdle(time.Now())

	select {
	case <-cancelled:
		t.Fatal("freshly-started spawn without shim yet must survive")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSpawnRunner_Run_ZeroIdleTimeoutReturnsImmediately(t *testing.T) {
	r := NewSpawnRunner(SpawnConfig{
		MaxParallel: 1, RatePerHourPerUser: 99, HardTimeout: time.Hour, IdleTimeout: 0,
	})

	ctx := t.Context()

	done := make(chan struct{})

	go func() {
		r.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run must return immediately when IdleTimeout <= 0")
	}
}

func TestSpawnRunner_SpawnAppendsModelArg(t *testing.T) {
	proc := newFakeSpawnProcess(4243)
	cmder := &fakeSpawnCommander{startFn: func(_ context.Context, _, _ string, _, _ []string) (SpawnProcess, error) {
		return proc, nil
	}}
	fb := newRecordingBot(100)

	r := NewSpawnRunnerWithDeps(SpawnConfig{
		MaxParallel:        1,
		RatePerHourPerUser: 99,
		HardTimeout:        time.Minute,
		ClaudeBin:          "/usr/local/bin/claude",
		ClaudeArgs:         []string{"--dangerously-load-development-channels", "plugin:telegram@local-yakov"},
	}, fb, cmder)

	id, err := r.Spawn(context.Background(), bot.SpawnRequest{
		Workdir: "/tmp/wd", ChatID: "1", UserID: "u", Model: "claude-opus-4-7",
	})
	require.NoError(t, err)
	require.NotEmpty(t, id)

	call := cmder.lastCall()
	assert.True(t, slices.Contains(call.args, "--model=claude-opus-4-7"), "args must include --model flag")
	assert.True(t, slices.Contains(call.args, "--dangerously-load-development-channels"), "default args must survive")
	assert.True(t, slices.Contains(call.args, "plugin:telegram@local-yakov"), "default args must survive")

	require.NoError(t, r.Cancel(id))
	require.Eventually(t, func() bool { return len(r.List()) == 0 }, 3*time.Second, 20*time.Millisecond)
}

func TestSpawnRunner_SpawnAppendsThinkingTokensEnv(t *testing.T) {
	proc := newFakeSpawnProcess(4244)
	cmder := &fakeSpawnCommander{startFn: func(_ context.Context, _, _ string, _, _ []string) (SpawnProcess, error) {
		return proc, nil
	}}
	fb := newRecordingBot(100)

	r := NewSpawnRunnerWithDeps(SpawnConfig{
		MaxParallel:        1,
		RatePerHourPerUser: 99,
		HardTimeout:        time.Minute,
		ClaudeBin:          "/usr/local/bin/claude",
		ClaudeArgs:         []string{"--dangerously-load-development-channels", "plugin:telegram@local-yakov"},
	}, fb, cmder)

	id, err := r.Spawn(context.Background(), bot.SpawnRequest{
		Workdir: "/tmp/wd", ChatID: "1", UserID: "u", ThinkingTokens: 16000,
	})
	require.NoError(t, err)
	require.NotEmpty(t, id)

	call := cmder.lastCall()
	assert.True(t, slices.Contains(call.env, "MAX_THINKING_TOKENS=16000"), "env must include MAX_THINKING_TOKENS")

	require.NoError(t, r.Cancel(id))
	require.Eventually(t, func() bool { return len(r.List()) == 0 }, 3*time.Second, 20*time.Millisecond)
}

func TestSpawnRunner_SpawnZeroValuesAreNoOp(t *testing.T) {
	proc := newFakeSpawnProcess(4245)
	cmder := &fakeSpawnCommander{startFn: func(_ context.Context, _, _ string, _, _ []string) (SpawnProcess, error) {
		return proc, nil
	}}
	fb := newRecordingBot(100)

	r := NewSpawnRunnerWithDeps(SpawnConfig{
		MaxParallel:        1,
		RatePerHourPerUser: 99,
		HardTimeout:        time.Minute,
		ClaudeBin:          "/usr/local/bin/claude",
		ClaudeArgs:         []string{"--dangerously-load-development-channels", "plugin:telegram@local-yakov"},
	}, fb, cmder)

	id, err := r.Spawn(context.Background(), bot.SpawnRequest{
		Workdir: "/tmp/wd", ChatID: "1", UserID: "u",
	})
	require.NoError(t, err)
	require.NotEmpty(t, id)

	call := cmder.lastCall()
	hasModel := slices.ContainsFunc(call.args, func(a string) bool {
		return len(a) >= 8 && a[:8] == "--model="
	})
	assert.False(t, hasModel, "no --model arg when Model is empty")

	hasThinking := slices.ContainsFunc(call.env, func(e string) bool {
		return len(e) >= 20 && e[:20] == "MAX_THINKING_TOKENS="
	})
	assert.False(t, hasThinking, "no MAX_THINKING_TOKENS env when ThinkingTokens is zero")

	require.NoError(t, r.Cancel(id))
	require.Eventually(t, func() bool { return len(r.List()) == 0 }, 3*time.Second, 20*time.Millisecond)
}

func TestSpawnRunner_SpawnDoesNotMutateConfigClaudeArgs(t *testing.T) {
	cmder := &fakeSpawnCommander{startFn: func(_ context.Context, _, _ string, _, _ []string) (SpawnProcess, error) {
		return newFakeSpawnProcess(4246), nil
	}}
	fb := newRecordingBot(100)

	baseArgs := []string{"--dangerously-load-development-channels", "plugin:telegram@local-yakov"}

	r := NewSpawnRunnerWithDeps(SpawnConfig{
		MaxParallel:        2,
		RatePerHourPerUser: 99,
		HardTimeout:        time.Minute,
		ClaudeBin:          "/usr/local/bin/claude",
		ClaudeArgs:         baseArgs,
	}, fb, cmder)

	id1, err := r.Spawn(context.Background(), bot.SpawnRequest{
		Workdir: "/tmp/wd", ChatID: "1", UserID: "u", Model: "claude-opus-4-7",
	})
	require.NoError(t, err)

	id2, err := r.Spawn(context.Background(), bot.SpawnRequest{
		Workdir: "/tmp/wd", ChatID: "1", UserID: "u", Model: "claude-sonnet-4",
	})
	require.NoError(t, err)

	assert.Equal(t, []string{"--dangerously-load-development-channels", "plugin:telegram@local-yakov"}, r.cfg.ClaudeArgs,
		"cfg.ClaudeArgs must remain unchanged across Spawn calls")
	assert.Len(t, r.cfg.ClaudeArgs, 2, "cfg.ClaudeArgs length must not grow")

	require.NoError(t, r.Cancel(id1))
	require.NoError(t, r.Cancel(id2))
	require.Eventually(t, func() bool { return len(r.List()) == 0 }, 3*time.Second, 20*time.Millisecond)
}

func TestSpawnRunner_SpawnDedupesThinkingTokensEnv(t *testing.T) {
	t.Setenv("MAX_THINKING_TOKENS", "5000")

	cmder := &fakeSpawnCommander{startFn: func(_ context.Context, _, _ string, _, _ []string) (SpawnProcess, error) {
		return newFakeSpawnProcess(4247), nil
	}}
	fb := newRecordingBot(100)

	r := NewSpawnRunnerWithDeps(SpawnConfig{
		MaxParallel:        1,
		RatePerHourPerUser: 99,
		HardTimeout:        time.Minute,
		ClaudeBin:          "/usr/local/bin/claude",
		ClaudeArgs:         []string{"--dangerously-load-development-channels", "plugin:telegram@local-yakov"},
	}, fb, cmder)

	id, err := r.Spawn(context.Background(), bot.SpawnRequest{
		Workdir: "/tmp/wd", ChatID: "1", UserID: "u", ThinkingTokens: 16000,
	})
	require.NoError(t, err)

	call := cmder.lastCall()
	matches := 0

	for _, e := range call.env {
		if len(e) >= 20 && e[:20] == "MAX_THINKING_TOKENS=" {
			matches++
		}
	}

	assert.Equal(t, 1, matches, "exactly one MAX_THINKING_TOKENS entry expected; parent's value must be filtered before append")
	assert.Contains(t, call.env, "MAX_THINKING_TOKENS=16000", "the request-supplied value must win")

	require.NoError(t, r.Cancel(id))
	require.Eventually(t, func() bool { return len(r.List()) == 0 }, 3*time.Second, 20*time.Millisecond)
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0) }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}

	return -1
}

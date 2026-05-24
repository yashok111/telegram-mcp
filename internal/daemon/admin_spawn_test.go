package daemon

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeAdminProcess struct {
	pid        int
	waitErrCh  chan error
	stopped    atomic.Bool
	stopErr    error
	stopCalled atomic.Int32
}

func newFakeAdminProcess(pid int) *fakeAdminProcess {
	return &fakeAdminProcess{pid: pid, waitErrCh: make(chan error, 1)}
}

func (p *fakeAdminProcess) Wait() error { return <-p.waitErrCh }
func (p *fakeAdminProcess) Pid() int    { return p.pid }
func (p *fakeAdminProcess) Stop(_ time.Duration) error {
	p.stopCalled.Add(1)

	if p.stopped.Swap(true) {
		return p.stopErr
	}

	p.waitErrCh <- errors.New("sigterm")

	return p.stopErr
}

type fakeAdminCommander struct {
	mu      sync.Mutex
	starts  int
	wantErr error
	procs   []*fakeAdminProcess
	gotEnv  [][]string
	started chan *fakeAdminProcess
}

func newFakeAdminCommander() *fakeAdminCommander {
	return &fakeAdminCommander{started: make(chan *fakeAdminProcess, 8)}
}

func (c *fakeAdminCommander) Start(_ context.Context, _ string, _, env []string) (AdminProcess, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.starts++
	c.gotEnv = append(c.gotEnv, env)

	if c.wantErr != nil {
		return nil, c.wantErr
	}

	p := newFakeAdminProcess(1000 + c.starts)
	c.procs = append(c.procs, p)

	select {
	case c.started <- p:
	default:
	}

	return p, nil
}

func TestAdminSpawnerSkippedWhenDisabled(t *testing.T) {
	cmder := newFakeAdminCommander()
	sp := NewAdminSpawner("/bin/telegram-mcp", cmder)
	sp.Enabled = false

	ctx := t.Context()

	done := make(chan struct{})

	go func() { sp.Run(ctx); close(done) }()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return immediately when disabled")
	}

	assert.Equal(t, 0, cmder.starts)
}

func TestAdminSpawnerStartsOnRun(t *testing.T) {
	cmder := newFakeAdminCommander()
	sp := NewAdminSpawner("/bin/telegram-mcp", cmder)
	sp.Enabled = true

	ctx := context.Background()

	done := make(chan struct{})

	go func() { sp.Run(ctx); close(done) }()

	select {
	case <-cmder.started:
	case <-time.After(time.Second):
		t.Fatal("Start not called within 1s")
	}

	sp.Stop()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit within 2s of Stop")
	}
}

func TestAdminSpawnerRestartsOnCrash(t *testing.T) {
	cmder := newFakeAdminCommander()
	sp := NewAdminSpawner("/bin/telegram-mcp", cmder)
	sp.Enabled = true
	sp.startMin = 5 * time.Millisecond
	sp.startMax = 5 * time.Millisecond

	ctx := context.Background()

	done := make(chan struct{})

	go func() { sp.Run(ctx); close(done) }()

	first := waitForProc(t, cmder.started)
	first.waitErrCh <- errors.New("crash")

	second := waitForProc(t, cmder.started)
	require.NotNil(t, second)
	assert.NotEqual(t, first.Pid(), second.Pid())

	sp.Stop()
	<-done
}

func TestAdminSpawnerBackoffOnStartFailure(t *testing.T) {
	cmder := newFakeAdminCommander()
	cmder.wantErr = errors.New("boom")

	sp := NewAdminSpawner("/bin/telegram-mcp", cmder)
	sp.Enabled = true
	sp.startMin = 5 * time.Millisecond
	sp.startMax = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})

	go func() { sp.Run(ctx); close(done) }()

	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	cmder.mu.Lock()
	starts := cmder.starts
	cmder.mu.Unlock()

	assert.GreaterOrEqual(t, starts, 2, "spawner should retry through backoff")
}

func TestAdminSpawnerStopSendsSigterm(t *testing.T) {
	cmder := newFakeAdminCommander()
	sp := NewAdminSpawner("/bin/telegram-mcp", cmder)
	sp.Enabled = true

	ctx := context.Background()

	done := make(chan struct{})

	go func() { sp.Run(ctx); close(done) }()

	p := waitForProc(t, cmder.started)

	sp.Stop()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit within 2s of Stop")
	}

	assert.GreaterOrEqual(t, int(p.stopCalled.Load()), 1, "Stop should signal the running child")
}

func TestNextAdminBackoffCaps(t *testing.T) {
	ceil := 100 * time.Millisecond
	assert.Equal(t, 20*time.Millisecond, nextAdminBackoff(10*time.Millisecond, ceil))
	assert.Equal(t, ceil, nextAdminBackoff(80*time.Millisecond, ceil))
	assert.Equal(t, ceil, nextAdminBackoff(ceil, ceil))
}

func waitForProc(t *testing.T, ch chan *fakeAdminProcess) *fakeAdminProcess {
	t.Helper()

	select {
	case p := <-ch:
		return p
	case <-time.After(2 * time.Second):
		t.Fatal("no process started within 2s")
		return nil
	}
}

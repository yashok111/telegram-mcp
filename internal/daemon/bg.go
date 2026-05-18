package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/yakov/telegram-mcp/internal/bot"
	"github.com/yakov/telegram-mcp/internal/chunk"
)

type BgTaskStatus string

const (
	BgStatusRunning   BgTaskStatus = "running"
	BgStatusDone      BgTaskStatus = "done"
	BgStatusFailed    BgTaskStatus = "failed"
	BgStatusCancelled BgTaskStatus = "cancelled"
)

type BgConfig struct {
	MaxParallel        int
	Timeout            time.Duration
	DefaultWorkdir     string
	RatePerHourPerUser int
	EditThrottle       time.Duration
	ClaudeBin          string
}

func DefaultBgConfig() BgConfig {
	return BgConfig{
		MaxParallel:        3,
		Timeout:            30 * time.Minute,
		RatePerHourPerUser: 10,
		EditThrottle:       5 * time.Second,
		ClaudeBin:          "claude",
	}
}

type bgTask struct {
	info   bot.BgTaskInfo
	cancel func()
}

type BgRunner struct {
	cfg BgConfig
	bot botSurface
	cmd Commander

	mu      sync.Mutex
	tasks   map[string]*bgTask
	perUser map[string][]time.Time
}

var (
	ErrTaskNotFound   = errors.New("task not found")
	ErrTooManyBgTasks = errors.New("too many concurrent /bg tasks")
	ErrRateLimited    = errors.New("rate limited: try again later")
	ErrEmptyPrompt    = errors.New("empty prompt")
)

func NewBgRunner(cfg BgConfig) *BgRunner {
	if cfg.MaxParallel <= 0 {
		cfg.MaxParallel = 3
	}

	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Minute
	}

	if cfg.RatePerHourPerUser <= 0 {
		cfg.RatePerHourPerUser = 10
	}

	if cfg.EditThrottle <= 0 {
		cfg.EditThrottle = 5 * time.Second
	}

	if cfg.ClaudeBin == "" {
		cfg.ClaudeBin = "claude"
	}

	return &BgRunner{
		cfg:     cfg,
		tasks:   map[string]*bgTask{},
		perUser: map[string][]time.Time{},
	}
}

func NewBgRunnerWithDeps(cfg BgConfig, b botSurface, cmder Commander) *BgRunner {
	r := NewBgRunner(cfg)
	r.bot = b
	r.cmd = cmder

	return r
}

func (r *BgRunner) List() []bot.BgTaskInfo {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]bot.BgTaskInfo, 0, len(r.tasks))
	for _, t := range r.tasks {
		out = append(out, t.info)
	}

	return out
}

// Stop cancels every in-flight task by invoking its CancelFunc. Used by the
// daemon shutdown path so running /bg subprocesses don't outlive the daemon.
// Best-effort: cancellation is fire-and-forget; callers that need a join
// should wait on List() draining via require.Eventually-style polling.
func (r *BgRunner) Stop() {
	r.mu.Lock()

	cancels := make([]func(), 0, len(r.tasks))
	for _, t := range r.tasks {
		cancels = append(cancels, t.cancel)
	}

	r.mu.Unlock()

	for _, c := range cancels {
		c()
	}
}

func (r *BgRunner) Cancel(id string) error {
	r.mu.Lock()
	t, ok := r.tasks[id]

	var cancel func()
	if ok {
		cancel = t.cancel
	}

	r.mu.Unlock()

	if !ok {
		return ErrTaskNotFound
	}

	cancel()

	return nil
}

func (r *BgRunner) reserveSlot(userID string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.tasks) >= r.cfg.MaxParallel {
		return "", ErrTooManyBgTasks
	}

	now := time.Now()
	cutoff := now.Add(-time.Hour)
	stamps := r.perUser[userID]

	keep := stamps[:0]
	for _, t := range stamps {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}

	if len(keep) >= r.cfg.RatePerHourPerUser {
		r.perUser[userID] = keep
		return "", ErrRateLimited
	}

	r.perUser[userID] = append(keep, now)

	buf := make([]byte, 3)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}

	id := hex.EncodeToString(buf)
	r.tasks[id] = &bgTask{
		info: bot.BgTaskInfo{
			ID:        id,
			StartedAt: now,
			UserID:    userID,
			Status:    string(BgStatusRunning),
		},
		cancel: func() {},
	}

	return id, nil
}

func (r *BgRunner) releaseSlot(id string, finalStatus BgTaskStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if t, ok := r.tasks[id]; ok {
		t.info.Status = string(finalStatus)

		delete(r.tasks, id)
	}
}

// Commander forks a child process whose lifetime tracks ctx. Mocked in tests.
type Commander interface {
	Start(ctx context.Context, workdir, bin string, args []string) (Process, error)
}

type Process interface {
	Stdout() io.ReadCloser
	Stderr() io.ReadCloser
	Pid() int
	Signal(sig os.Signal) error
	Wait() error
}

type execCommander struct{}

func NewExecCommander() Commander { return execCommander{} }

func (execCommander) Start(ctx context.Context, workdir, bin string, args []string) (Process, error) {
	// bin is operator-configured via TELEGRAM_BG_CLAUDE_BIN (defaults to "claude").
	// args[0] is the user prompt, passed as a single argv element — not shell-eval'd.
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // intentional subprocess; bin operator-trusted, prompt is argv not shell.
	cmd.Dir = workdir

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()

		return nil, fmt.Errorf("start: %w", err)
	}

	return &execProcess{cmd: cmd, stdout: stdout, stderr: stderr}, nil
}

type execProcess struct {
	cmd            *exec.Cmd
	stdout, stderr io.ReadCloser
}

func (p *execProcess) Stdout() io.ReadCloser { return p.stdout }
func (p *execProcess) Stderr() io.ReadCloser { return p.stderr }
func (p *execProcess) Pid() int              { return p.cmd.Process.Pid }

func (p *execProcess) Signal(sig os.Signal) error {
	if p.cmd.Process == nil {
		return errors.New("process not started")
	}

	return p.cmd.Process.Signal(sig)
}

func (p *execProcess) Wait() error { return p.cmd.Wait() }

func (r *BgRunner) Spawn(ctx context.Context, req bot.BgSpawnRequest) (string, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return "", ErrEmptyPrompt
	}

	workdir := req.Workdir
	if workdir == "" {
		workdir = r.cfg.DefaultWorkdir
	}

	if workdir == "" {
		workdir, _ = os.UserHomeDir()
	}

	id, err := r.reserveSlot(req.UserID)
	if err != nil {
		return "", err
	}

	initial := fmt.Sprintf("🚀 Task %s started\nworkdir: %s\nprompt: %s", id, workdir, truncate(req.Prompt, 200))

	msgID, sendErr := r.bot.SendMessage(ctx, req.ChatID, initial, bot.SendOpts{})
	if sendErr != nil {
		r.releaseSlot(id, BgStatusFailed)
		return "", fmt.Errorf("send initial: %w", sendErr)
	}

	taskCtx, cancel := context.WithTimeout(context.Background(), r.cfg.Timeout)

	r.mu.Lock()
	t := r.tasks[id]
	t.info.Workdir = workdir
	t.info.PromptHead = truncate(req.Prompt, 60)
	t.cancel = cancel
	r.mu.Unlock()

	go r.runTask(taskCtx, cancel, id, req, workdir, msgID)

	return id, nil
}

func (r *BgRunner) runTask(ctx context.Context, cancel context.CancelFunc, id string, req bot.BgSpawnRequest, workdir string, progressMsgID int) {
	defer cancel()

	args := []string{"--print", "--output-format=stream-json", "--verbose", req.Prompt}

	proc, err := r.cmd.Start(ctx, workdir, r.cfg.ClaudeBin, args)
	if err != nil {
		r.editFinal(ctx, req.ChatID, progressMsgID, id, fmt.Sprintf("❌ Task %s failed to start: %v", id, err), 0)
		r.releaseSlot(id, BgStatusFailed)

		return
	}

	stderrTail := &stderrRing{capacity: 2048}
	stderrDone := make(chan struct{})

	go func() {
		defer close(stderrDone)

		buf := make([]byte, 4096)
		for {
			n, rerr := proc.Stderr().Read(buf)
			if n > 0 {
				stderrTail.add(buf[:n])
			}

			if rerr != nil {
				return
			}
		}
	}()

	state := &bgRunState{startedAt: time.Now()}
	doneCh := make(chan error, 1)

	go func() { doneCh <- r.consumeStream(proc.Stdout(), state) }()

	tick := time.NewTicker(r.cfg.EditThrottle)
	defer tick.Stop()

	var result *StreamEvent

	for result == nil {
		select {
		case <-ctx.Done():
			_ = proc.Signal(syscall.SIGTERM)

			select {
			case <-doneCh:
			case <-time.After(5 * time.Second):
				_ = proc.Signal(syscall.SIGKILL)
				<-doneCh
			}

			_ = proc.Wait()
			<-stderrDone

			text := fmt.Sprintf("🛑 Task %s cancelled · ran %s", id, time.Since(state.startedAt).Round(time.Second))
			r.editFinal(ctx, req.ChatID, progressMsgID, id, text, 0)
			r.releaseSlot(id, BgStatusCancelled)

			return
		case derr := <-doneCh:
			if derr != nil {
				_ = proc.Wait()
				<-stderrDone
				r.finalizeFailure(ctx, req.ChatID, progressMsgID, id, derr, stderrTail.String())

				return
			}

			result = state.last()
			if result == nil {
				_ = proc.Wait()
				<-stderrDone
				r.finalizeFailure(ctx, req.ChatID, progressMsgID, id, errors.New("stream ended without result"), stderrTail.String())

				return
			}
		case <-tick.C:
			text := state.progressText(id)
			if text != "" {
				if _, eerr := r.bot.EditMessage(ctx, req.ChatID, progressMsgID, text, ""); eerr != nil {
					slog.Warn("bg progress edit failed", "task_id", id, "err", eerr)
				}
			}
		}
	}

	_ = proc.Wait()
	<-stderrDone

	dur := time.Since(state.startedAt).Round(time.Second)
	final := fmt.Sprintf("✅ Task %s done · %s · $%.4f · %d turns", id, dur, result.CostUSD, result.NumTurns)
	r.editFinal(ctx, req.ChatID, progressMsgID, id, final, result.NumTurns)

	for _, c := range chunk.Split(result.ResultText, 4096, chunk.Length) {
		if _, serr := r.bot.SendMessage(ctx, req.ChatID, c, bot.SendOpts{ReplyTo: progressMsgID}); serr != nil {
			slog.Warn("bg result chunk send failed", "task_id", id, "err", serr)
		}
	}

	r.releaseSlot(id, BgStatusDone)
}

type bgRunState struct {
	mu        sync.Mutex
	startedAt time.Time
	numTurns  int
	numTools  int
	lastText  string
	lastEvent *StreamEvent
}

func (s *bgRunState) record(ev StreamEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch ev.Kind {
	case StreamEventAssistantText:
		s.lastText = ev.Text
		s.numTurns++
	case StreamEventToolUse:
		s.numTools++
	case StreamEventResult:
		cp := ev
		s.lastEvent = &cp
	case StreamEventInit, StreamEventOther:
	}
}

func (s *bgRunState) progressText(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	head := truncate(s.lastText, 200)
	if head == "" {
		head = "(no output yet)"
	}

	return fmt.Sprintf("🔄 Task %s · turns=%d tools=%d\n%s", id, s.numTurns, s.numTools, head)
}

func (s *bgRunState) last() *StreamEvent {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.lastEvent
}

func (r *BgRunner) consumeStream(stdout io.ReadCloser, state *bgRunState) error {
	defer func() { _ = stdout.Close() }()

	sr := NewStreamReader(stdout)
	for {
		ev, err := sr.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}

			return fmt.Errorf("stream: %w", err)
		}

		state.record(ev)
	}
}

func (r *BgRunner) editFinal(ctx context.Context, chatID string, msgID int, taskID, text string, _ int) {
	if _, err := r.bot.EditMessage(ctx, chatID, msgID, text, ""); err != nil {
		slog.Warn("bg final edit failed", "task_id", taskID, "err", err)
	}
}

func (r *BgRunner) finalizeFailure(ctx context.Context, chatID string, msgID int, id string, err error, stderrTail string) {
	r.editFinal(ctx, chatID, msgID, id, fmt.Sprintf("❌ Task %s failed: %v", id, err), 0)

	if tail := strings.TrimSpace(stderrTail); tail != "" {
		if _, serr := r.bot.SendMessage(ctx, chatID, "stderr:\n"+truncate(tail, 1800), bot.SendOpts{ReplyTo: msgID}); serr != nil {
			slog.Warn("bg stderr tail send failed", "task_id", id, "err", serr)
		}
	}

	r.releaseSlot(id, BgStatusFailed)
}

// stderrRing is a bounded byte buffer that drops the oldest data on overflow.
type stderrRing struct {
	mu       sync.Mutex
	buf      []byte
	capacity int
}

func (s *stderrRing) add(p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.buf = append(s.buf, p...)
	if len(s.buf) > s.capacity {
		s.buf = s.buf[len(s.buf)-s.capacity:]
	}
}

func (s *stderrRing) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return string(s.buf)
}

func truncate(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}

	count := 0
	for i := range s {
		if count == n {
			return s[:i] + "…"
		}

		count++
	}

	return s
}

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
	"slices"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/aymanbagabas/go-pty"

	"github.com/yakov/telegram-mcp/internal/bot"
)

// SpawnStatus is the lifecycle state stored on a spawnTask.info. starting →
// running once the pty Start succeeds; terminal states are done/failed/cancelled.
type SpawnStatus string

const (
	SpawnStatusStarting  SpawnStatus = "starting"
	SpawnStatusRunning   SpawnStatus = "running"
	SpawnStatusDone      SpawnStatus = "done"
	SpawnStatusFailed    SpawnStatus = "failed"
	SpawnStatusCancelled SpawnStatus = "cancelled"
)

type SpawnConfig struct {
	MaxParallel        int
	HardTimeout        time.Duration
	DefaultWorkdir     string
	RatePerHourPerUser int

	// IdleTimeout cancels a spawn whose paired shim has gone idle for longer
	// than this, freeing the parallel slot. =0 disables the sweep.
	// Spawns whose shim never connects within IdleTimeout of StartedAt are
	// treated as orphans and cancelled by the same sweep.
	IdleTimeout time.Duration

	// ClaudeBin is the executable spawned for each /spawn invocation; defaults
	// to "claude" but operators can point it at a wrapper script.
	ClaudeBin string

	// ClaudeArgs are passed verbatim after ClaudeBin. The default loads the
	// telegram plugin so the spawned CC client connects back to this daemon
	// as a fresh shim.
	ClaudeArgs []string
}

func DefaultSpawnConfig() SpawnConfig {
	return SpawnConfig{
		MaxParallel:        3,
		HardTimeout:        24 * time.Hour,
		IdleTimeout:        4 * time.Hour,
		RatePerHourPerUser: 5,
		ClaudeBin:          "claude",
		ClaudeArgs:         []string{"--dangerously-load-development-channels", "plugin:telegram@local-yakov"},
	}
}

// SpawnProcess is the pty-attached subprocess handle. The daemon does not read
// or write the pty — the spawned CC's TUI rendering goes into /dev/null via
// the pty master. All real traffic flows through the embedded shim's MCP/IPC.
type SpawnProcess interface {
	Pid() int
	Signal(sig os.Signal) error
	Wait() error
	Close() error
}

// SpawnCommander forks a child whose lifetime tracks ctx. Mocked in tests.
type SpawnCommander interface {
	Start(ctx context.Context, workdir, bin string, args, env []string) (SpawnProcess, error)
}

type execSpawnCommander struct{}

func NewExecSpawnCommander() SpawnCommander { return execSpawnCommander{} }

func (execSpawnCommander) Start(ctx context.Context, workdir, bin string, args, env []string) (SpawnProcess, error) {
	// bin is operator-configured (TELEGRAM_SPAWN_CLAUDE_BIN); args static
	// (operator can override via TELEGRAM_SPAWN_CLAUDE_ARGS). User input
	// never reaches argv — /spawn carries only a workdir, no prompt.
	p, err := pty.New()
	if err != nil {
		return nil, fmt.Errorf("pty new: %w", err)
	}

	cmd := p.CommandContext(ctx, bin, args...)
	cmd.Dir = workdir
	cmd.Env = env

	if err := cmd.Start(); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("pty start: %w", err)
	}

	// Drain pty output to /dev/null so the kernel buffer never fills — would
	// otherwise block claude's TUI writes and stall the session. The Read
	// loop exits on Close (master fd closed) via io.EOF.
	go func() { _, _ = io.Copy(io.Discard, p) }()

	// Press Enter into the pty a handful of times to dismiss claude's
	// `--dangerously-load-development-channels` consent prompt (default option
	// "1. I am using this for local development" is preselected). Without
	// this the spawned CC sits at the consent screen forever, the telegram
	// plugin never loads, and no shim ever connects back to the daemon.
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()

		for range 6 {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := p.Write([]byte("\r")); err != nil {
					return
				}
			}
		}
	}()

	return &execSpawnProcess{cmd: cmd, pty: p}, nil
}

type execSpawnProcess struct {
	cmd *pty.Cmd
	pty pty.Pty
}

func (p *execSpawnProcess) Pid() int {
	if p.cmd.Process == nil {
		return 0
	}

	return p.cmd.Process.Pid
}

func (p *execSpawnProcess) Signal(sig os.Signal) error {
	if p.cmd.Process == nil {
		return errors.New("process not started")
	}

	return p.cmd.Process.Signal(sig)
}

func (p *execSpawnProcess) Wait() error { return p.cmd.Wait() }

func (p *execSpawnProcess) Close() error {
	if p.pty == nil {
		return nil
	}

	return p.pty.Close()
}

type spawnTask struct {
	info   bot.SpawnTaskInfo
	cancel func()
	pid    int
	// topicThreadID is the forum topic the /spawn was issued from (0 for
	// DM-launched spawns). TopicForSpawn exposes it so Forum.AllocateOrReuse
	// seats the spawned shim in that exact topic on hello.
	topicThreadID int
}

// IdleLookup reports the idle duration for the shim currently paired with
// spawnID. ok=false means no shim is registered with that spawn_id, in which
// case the sweeper falls back to time-since-StartedAt to detect orphans.
type IdleLookup func(spawnID string) (idle time.Duration, ok bool)

// SpawnRunner bootstraps Claude Code clients owned by the daemon. Each /spawn
// invocation forks `claude` with the telegram plugin pre-loaded, so the
// spawned CC connects back as a fresh shim via the standard IPC handshake —
// the daemon then routes via @sN mentions / reply-rings / chat affinity like
// any user-launched session. SpawnRunner ONLY owns the subprocess lifecycle
// (PID tracking, MaxParallel, Cancel via SIGTERM, hard timeout).
type SpawnRunner struct {
	cfg SpawnConfig
	bot botSurface
	cmd SpawnCommander

	mu         sync.Mutex
	tasks      map[string]*spawnTask
	perUser    map[string][]time.Time
	idleLookup IdleLookup
	sink       EventSink
}

var (
	ErrSpawnNotFound     = errors.New("spawn not found")
	ErrTooManySpawnTasks = errors.New("too many concurrent /spawn sessions")
	ErrSpawnRateLimited  = errors.New("rate limited: try again later")
)

func NewSpawnRunner(cfg SpawnConfig) *SpawnRunner {
	if cfg.MaxParallel <= 0 {
		cfg.MaxParallel = 3
	}

	if cfg.HardTimeout <= 0 {
		cfg.HardTimeout = 24 * time.Hour
	}

	if cfg.RatePerHourPerUser <= 0 {
		cfg.RatePerHourPerUser = 5
	}

	if cfg.ClaudeBin == "" {
		cfg.ClaudeBin = "claude"
	}

	if len(cfg.ClaudeArgs) == 0 {
		cfg.ClaudeArgs = []string{"--dangerously-load-development-channels", "plugin:telegram@local-yakov"}
	}

	return &SpawnRunner{
		cfg:     cfg,
		tasks:   map[string]*spawnTask{},
		perUser: map[string][]time.Time{},
	}
}

func NewSpawnRunnerWithDeps(cfg SpawnConfig, b botSurface, cmder SpawnCommander) *SpawnRunner {
	r := NewSpawnRunner(cfg)
	r.bot = b
	r.cmd = cmder

	return r
}

// List returns a snapshot of every live spawn. Status is the runner's notion
// (starting/running). Matching to a Router-tracked shim alias happens at the
// /spawn list rendering layer, which can cross-reference SpawnID.
func (r *SpawnRunner) List() []bot.SpawnTaskInfo {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]bot.SpawnTaskInfo, 0, len(r.tasks))
	for _, t := range r.tasks {
		out = append(out, t.info)
	}

	return out
}

// SetIdleLookup wires the per-spawn idle-time lookup used by Run's sweep.
// Pass nil to disable the lookup (orphan detection via StartedAt still applies).
func (r *SpawnRunner) SetIdleLookup(fn IdleLookup) {
	r.mu.Lock()
	r.idleLookup = fn
	r.mu.Unlock()
}

// SetEventSink wires the anomaly-event sink (nil disables emission). Safe to
// call concurrently with the sweep/spawn goroutines.
func (r *SpawnRunner) SetEventSink(s EventSink) {
	r.mu.Lock()
	r.sink = s
	r.mu.Unlock()
}

// emit sends e if a sink is wired. Reads r.sink under the lock (the sweep and
// runSpawn goroutines call this); EventSink.Emit is itself non-blocking.
func (r *SpawnRunner) emit(e Event) {
	r.mu.Lock()
	sink := r.sink
	r.mu.Unlock()

	if sink != nil {
		sink.Emit(e)
	}
}

// Run is the idle-timeout sweeper. Every minute it cancels any spawn whose
// paired shim has been idle past IdleTimeout, or whose shim never connected
// within IdleTimeout of StartedAt (orphan). Returns immediately when
// IdleTimeout <= 0. Caller is responsible for ctx.Done() — typically run as
// `go r.Run(ctx)` from the daemon's lifecycle wiring.
func (r *SpawnRunner) Run(ctx context.Context) {
	if r.cfg.IdleTimeout <= 0 {
		return
	}

	t := time.NewTicker(time.Minute)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.sweepIdle(time.Now())
		}
	}
}

// sweepIdle inspects every live task and cancels those whose paired shim is
// idle past IdleTimeout, or whose StartedAt is older than IdleTimeout with no
// shim ever registered. Lock is held only while collecting victims; the
// cancel calls fan out after the unlock to avoid holding mu across runSpawn
// cleanup which itself takes mu via releaseSlot.
func (r *SpawnRunner) sweepIdle(now time.Time) {
	threshold := r.cfg.IdleTimeout
	if threshold <= 0 {
		return
	}

	type victim struct {
		id     string
		cancel func()
		idle   time.Duration
		orphan bool
	}

	r.mu.Lock()
	lookup := r.idleLookup

	victims := make([]victim, 0, len(r.tasks))
	for id, t := range r.tasks {
		var (
			idle   time.Duration
			paired bool
		)

		if lookup != nil {
			idle, paired = lookup(id)
		}

		if !paired {
			// No shim registered for this spawn — measure from StartedAt so
			// claudes that crashed pre-hello (or never managed to load the
			// plugin) eventually free their parallel slot.
			idle = now.Sub(t.info.StartedAt)
		}

		if idle > threshold {
			victims = append(victims, victim{
				id: id, cancel: t.cancel, idle: idle, orphan: !paired,
			})
		}
	}
	r.mu.Unlock()

	for _, v := range victims {
		slog.Info("spawn idle-timeout exceeded; cancelling",
			"spawn_id", v.id, "idle", v.idle.Round(time.Second), "orphan", v.orphan)
		r.emit(Event{Type: "spawn_idle_killed", Severity: "info", Subject: v.id, Detail: fmt.Sprintf("idle=%s orphan=%v", v.idle.Round(time.Second), v.orphan)})
		v.cancel()
	}
}

// Stop cancels every live spawn. Daemon shutdown path uses this so spawned
// CC processes get a chance to clean up via PR_SET_PDEATHSIG on their shim.
func (r *SpawnRunner) Stop() {
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

func (r *SpawnRunner) Cancel(id string) error {
	r.mu.Lock()
	t, ok := r.tasks[id]

	var cancel func()
	if ok {
		cancel = t.cancel
	}

	r.mu.Unlock()

	if !ok {
		return ErrSpawnNotFound
	}

	cancel()

	return nil
}

// reserveSlot atomically picks a fresh spawn_id, enforces MaxParallel and
// the per-user hourly cap, and seeds tasks[id] with a starting-status entry.
// threadID is the forum topic the spawn is pinned to (0 for DM) — set at
// creation so TopicForSpawn is correct from the first call, before cmd.Start.
func (r *SpawnRunner) reserveSlot(userID string, threadID int) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.tasks) >= r.cfg.MaxParallel {
		return "", ErrTooManySpawnTasks
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
		return "", ErrSpawnRateLimited
	}

	r.perUser[userID] = append(keep, now)

	// Opportunistic GC: drop other users whose stamps have all aged out. See
	// peruser_gc.go for rationale.
	gcPerUserLocked(r.perUser, userID, cutoff)

	// 4 bytes → 32-bit space. 3 was the original budget but two spawns landing
	// on the same id within one daemon lifetime is a real risk at 24 bits
	// (~16k spawns has 50% birthday collision).
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}

	id := hex.EncodeToString(buf)
	r.tasks[id] = &spawnTask{
		info: bot.SpawnTaskInfo{
			ID:        id,
			StartedAt: now,
			UserID:    userID,
			Status:    string(SpawnStatusStarting),
		},
		cancel:        func() {},
		topicThreadID: threadID,
	}

	return id, nil
}

func (r *SpawnRunner) releaseSlot(id string, finalStatus SpawnStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if t, ok := r.tasks[id]; ok {
		t.info.Status = string(finalStatus)

		delete(r.tasks, id)
	}
}

// Spawn launches a new CC client wired up to talk to this daemon. Returns the
// short spawn_id; the spawned CC's shim will hello-handshake with that ID in
// its TELEGRAM_SPAWN_ID env so /spawn list can cross-reference.
func (r *SpawnRunner) Spawn(ctx context.Context, req bot.SpawnRequest) (string, error) {
	workdir := req.Workdir
	if workdir == "" {
		workdir = r.cfg.DefaultWorkdir
	}

	if workdir == "" {
		workdir, _ = os.UserHomeDir()
	}

	id, err := r.reserveSlot(req.UserID, req.ThreadID)
	if err != nil {
		return "", err
	}

	// Stamp the spawn ID into the child's env so its shim's hello carries it
	// back. Daemon then resolves /spawn list → alias mapping via Router lookup.
	// Filter any pre-existing TELEGRAM_SPAWN_ID so a nested /spawn (operator
	// daemon spawned by another daemon — extremely rare) can't carry a stale
	// id through the new child's env.
	env := filterEnv(os.Environ(), "TELEGRAM_SPAWN_ID=")
	env = append(env, "TELEGRAM_SPAWN_ID="+id)

	args := slices.Clone(r.cfg.ClaudeArgs)
	if req.Model != "" {
		args = append(args, "--model="+req.Model)
	}

	if req.ThinkingTokens > 0 {
		env = filterEnv(env, "MAX_THINKING_TOKENS=")
		env = append(env, "MAX_THINKING_TOKENS="+strconv.Itoa(req.ThinkingTokens))
	}

	// Detached from caller's ctx — /spawn outlives the bot's request scope.
	// HardTimeout is the only built-in cap; Cancel/Stop also flow here.
	taskCtx, cancel := context.WithTimeout(context.Background(), r.cfg.HardTimeout)

	proc, perr := r.cmd.Start(taskCtx, workdir, r.cfg.ClaudeBin, args, env)
	if perr != nil {
		cancel()
		r.releaseSlot(id, SpawnStatusFailed)
		_, _ = r.bot.SendMessage(ctx, req.ChatID,
			fmt.Sprintf("❌ Spawn %s failed to start: %v", id, perr),
			bot.SendOpts{MessageThreadID: req.ThreadID})

		return "", fmt.Errorf("start: %w", perr)
	}

	r.mu.Lock()
	t := r.tasks[id]
	t.cancel = cancel
	t.pid = proc.Pid()
	t.info.Pid = proc.Pid()
	t.info.Workdir = workdir
	t.info.ChatID = req.ChatID
	t.info.Status = string(SpawnStatusRunning)
	r.mu.Unlock()

	go r.runSpawn(taskCtx, cancel, id, proc)

	attrs := []any{"spawn_id", id, "chat_id", req.ChatID, "workdir", workdir, "pid", proc.Pid()}
	if req.Model != "" {
		attrs = append(attrs, "model", req.Model)
	}

	if req.ThinkingTokens > 0 {
		attrs = append(attrs, "thinking_tokens", req.ThinkingTokens)
	}

	slog.Info("spawn started", attrs...)

	_, _ = r.bot.SendMessage(ctx, req.ChatID,
		fmt.Sprintf("🚀 Spawn %s started · pid=%d · workdir=%s\nWait a moment for the shim to register — then use /sessions or @<alias> to talk to it.",
			id, proc.Pid(), workdir),
		bot.SendOpts{MessageThreadID: req.ThreadID})

	return id, nil
}

// TopicForSpawn reports the forum thread_id a spawn was pinned to — the topic
// the /spawn command was issued from. ok=false when the spawn is unknown or
// was launched outside a topic (DM). Wired into Forum so the spawned shim
// seats in that exact topic at hello instead of allocating one by workdir.
func (r *SpawnRunner) TopicForSpawn(id string) (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	t, ok := r.tasks[id]
	if !ok || t.topicThreadID == 0 {
		return 0, false
	}

	return t.topicThreadID, true
}

// filterEnv returns a copy of env with any entry whose key matches one of the
// prefixes removed. Used to drop a pre-existing TELEGRAM_SPAWN_ID /
// MAX_THINKING_TOKENS / TELEGRAM_ADMIN_TOKEN before stamping a fresh value, so a
// nested daemon can't leak a stale value to its child.
func filterEnv(env []string, prefixes ...string) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		if !hasAnyPrefix(e, prefixes) {
			out = append(out, e)
		}
	}

	return out
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if startsWith(s, p) {
			return true
		}
	}

	return false
}

func startsWith(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }

// runSpawn shuts the subprocess down on ctx cancel and reaps the exit code.
// Lifecycle is minimal because the shim+Router handle every TG ↔ CC message:
// runSpawn just makes sure SIGTERM lands, the pty is closed, and the slot is
// released. Closing the pty in the cancellation path also unblocks the
// io.Discard drain goroutine started in pty.Start.
func (r *SpawnRunner) runSpawn(ctx context.Context, cancel context.CancelFunc, id string, proc SpawnProcess) {
	defer cancel()

	waitDone := make(chan error, 1)

	go func() { waitDone <- proc.Wait() }()

	select {
	case <-ctx.Done():
		_ = proc.Signal(syscall.SIGTERM)

		select {
		case <-waitDone:
			// Process exited within the SIGTERM grace window.
			_ = proc.Close()
		case <-time.After(5 * time.Second):
			_ = proc.Signal(syscall.SIGKILL)
			// Close the pty so the drain goroutine in pty.Start sees EOF and
			// exits — if the child is stuck in an uninterruptible read, the
			// pty master close is what frees its slave file descriptors.
			_ = proc.Close()

			select {
			case <-waitDone:
			case <-time.After(3 * time.Second):
				slog.Warn("spawn process stuck after SIGKILL", "spawn_id", id, "pid", proc.Pid())
			}
		}

		r.releaseSlot(id, SpawnStatusCancelled)
		slog.Info("spawn cancelled", "spawn_id", id)
	case err := <-waitDone:
		_ = proc.Close()

		status := SpawnStatusDone
		if err != nil {
			status = SpawnStatusFailed

			r.emit(Event{Type: "spawn_crashed", Severity: "warning", Subject: id, Detail: fmt.Sprintf("pid=%d exited non-zero: %v", proc.Pid(), err)})
		}

		r.releaseSlot(id, status)
		slog.Info("spawn exited", "spawn_id", id, "status", status, "err", err)
	}
}

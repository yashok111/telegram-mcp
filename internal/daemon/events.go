package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/yakov/telegram-mcp/internal/ipc"
)

// Event is a durable anomaly record persisted to events.jsonl. JSON tags are
// stable wire format — do not rename without a migration story.
type Event struct {
	Type     string    `json:"type"`     // shim_disconnected|spawn_crashed|spawn_idle_killed|bg_failed|unauthorized_dm|error_burst
	Severity string    `json:"severity"` // info|warning|critical
	TS       time.Time `json:"ts"`
	Subject  string    `json:"subject"` // shim_id / chat_id / spawn_id / bg id — the thing the event is about
	Detail   string    `json:"detail"`
}

// EventSink is the interface emit-sites depend on. Callers nil-check before
// calling; a nil EventSink is valid and silently drops events.
type EventSink interface{ Emit(Event) }

const (
	defaultEventRetention = 30 * 24 * time.Hour
	defaultMaxEvents      = 5000
	maxEventLine          = 1 << 20
)

// EventLog is the durable JSONL store at <stateDir>/admin/events.jsonl.
// A single mutex serialises all reads and writes; events are one global
// stream, unlike per-chat history.
type EventLog struct {
	Dir       string        // <stateDir>/admin
	Retention time.Duration // default 30*24h; <=0 disables age axis
	MaxEvents int           // default 5000; <=0 disables count axis
	mu        sync.Mutex

	// appendsSinceCompact counts O(1) line-appends since the last full
	// read+retention+rewrite. Compaction runs every eventCompactEvery appends so
	// the per-event cost is amortized O(1) instead of O(n)-rewrite-per-event.
	appendsSinceCompact int
}

// eventCompactEvery bounds the file to MaxEvents + eventCompactEvery between
// compactions; small enough that the worst-case overshoot is trivial.
const eventCompactEvery = 64

func (l *EventLog) filePath() string {
	return filepath.Join(l.Dir, "events.jsonl")
}

// NewEventLog returns an EventLog whose file lives at
// filepath.Join(stateDir, "admin", "events.jsonl").
func NewEventLog(stateDir string) *EventLog {
	return &EventLog{
		Dir:       filepath.Join(stateDir, "admin"),
		Retention: defaultEventRetention,
		MaxEvents: defaultMaxEvents,
	}
}

// Append persists e. The common path is an O(1) O_APPEND of a single JSONL
// line; every eventCompactEvery appends it instead does a full
// read+retention+rewrite so the file stays bounded. e.TS is filled with
// time.Now().UTC() when zero.
func (l *EventLog) Append(e Event) error {
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.appendsSinceCompact++
	if l.appendsSinceCompact >= eventCompactEvery {
		l.appendsSinceCompact = 0

		existing := append(l.readFile(), e)

		return l.saveAtomic(l.applyRetention(existing))
	}

	return l.appendLine(e)
}

// appendLine appends one event as a JSONL line via O_APPEND — O(1), no rewrite.
// Retention is enforced by the periodic compaction in Append; the line is not
// fsync'd (an anomaly log losing its last few lines on a crash is acceptable,
// and compaction syncs).
func (l *EventLog) appendLine(e Event) error {
	if err := os.MkdirAll(l.Dir, 0o700); err != nil {
		return fmt.Errorf("mkdir events dir: %w", err)
	}

	f, err := os.OpenFile(l.filePath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open events file: %w", err)
	}
	defer func() { _ = f.Close() }()

	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}

	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("append event: %w", err)
	}

	return nil
}

// Recent returns the last n events, oldest-first. A missing file returns
// nil, nil. n<=0 returns all retained events.
func (l *EventLog) Recent(n int) ([]Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	events := l.applyRetention(l.readFile())

	if n > 0 && len(events) > n {
		events = events[len(events)-n:]
	}

	return events, nil
}

func (l *EventLog) readFile() []Event {
	f, err := os.Open(l.filePath())
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	var events []Event

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxEventLine)

	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}

		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			slog.Warn("events.jsonl skipping malformed entry", "path", l.filePath(), "err", err)
			continue
		}

		events = append(events, e)
	}

	if err := sc.Err(); err != nil {
		slog.Warn("events.jsonl read stopped early; later entries dropped", "path", l.filePath(), "err", err)
	}

	return events
}

func (l *EventLog) applyRetention(events []Event) []Event {
	if len(events) == 0 {
		return events
	}

	if l.Retention > 0 {
		cutoff := time.Now().Add(-l.Retention)

		i := 0
		for i < len(events) && events[i].TS.Before(cutoff) {
			i++
		}

		events = events[i:]
	}

	if l.MaxEvents > 0 && len(events) > l.MaxEvents {
		events = events[len(events)-l.MaxEvents:]
	}

	return events
}

func (l *EventLog) saveAtomic(events []Event) error {
	if err := os.MkdirAll(l.Dir, 0o700); err != nil {
		return fmt.Errorf("mkdir events dir: %w", err)
	}

	tmp, err := os.CreateTemp(l.Dir, ".events.tmp.*")
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}

	tmpName := tmp.Name()

	cleanup := tmpName
	defer func() {
		if cleanup != "" {
			_ = os.Remove(cleanup)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod tmp: %w", err)
	}

	enc := json.NewEncoder(tmp)
	for _, e := range events {
		if err := enc.Encode(e); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("encode entry: %w", err)
		}
	}

	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync tmp: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close tmp: %w", err)
	}

	if err := os.Rename(tmpName, l.filePath()); err != nil {
		return fmt.Errorf("rename tmp: %w", err)
	}

	cleanup = ""

	return nil
}

// AdminNotifyFunc pushes a notification to the connected admin-agent shim.
// Returns false when no admin is connected.
type AdminNotifyFunc func(method string, params any) bool

const eventBusCapacity = 128

// EventBus is a worker-backed async fan-out implementing EventSink. Emit is
// non-blocking so logging/handler goroutines never block on file IO.
type EventBus struct {
	log    *EventLog
	notify AdminNotifyFunc
	ch     chan Event
}

// NewEventBus creates an EventBus. notify may be nil (no admin push).
func NewEventBus(log *EventLog, notify AdminNotifyFunc) *EventBus {
	return &EventBus{
		log:    log,
		notify: notify,
		ch:     make(chan Event, eventBusCapacity),
	}
}

// Emit queues an event for async persistence and push. Non-blocking: drops
// the event with a warning when the channel is full. Nil-receiver-safe so a
// typed-nil *EventBus handed to an EventSink interface degrades to a no-op
// instead of panicking.
func (b *EventBus) Emit(e Event) {
	if b == nil {
		return
	}

	select {
	case b.ch <- e:
	default:
		slog.Warn("event bus full, dropping", "type", e.Type)
	}
}

// Run drains the event channel until ctx is cancelled. Call in a dedicated
// goroutine; returns when ctx.Done() fires, after draining whatever is still
// buffered so a clean shutdown doesn't lose already-queued anomalies.
func (b *EventBus) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			b.drainBuffered()
			return
		case e := <-b.ch:
			b.handle(e)
		}
	}
}

// drainBuffered persists every event still in the channel at shutdown.
// Best-effort: stops as soon as the buffer is empty (it does not wait for
// in-flight Emit callers — shutdown is racy by nature).
func (b *EventBus) drainBuffered() {
	for {
		select {
		case e := <-b.ch:
			b.handle(e)
		default:
			return
		}
	}
}

// handle persists then pushes one event. The push is always NotifyAdminEvent:
// EventBus is the anomaly-event channel only. The daily sitrep trigger is a
// distinct, non-persisted signal delivered via Router.AdminNotify
// (ipc.NotifyAdminSitrep) by the sitrep ticker — it must never be routed
// through Emit/this bus, or it would arrive mislabelled as an anomaly.
func (b *EventBus) handle(e Event) {
	if b.log != nil {
		if err := b.log.Append(e); err != nil {
			slog.Warn("event persist failed", "type", e.Type, "err", err)
		}
	}

	if b.notify != nil {
		b.notify(ipc.NotifyAdminEvent, e)
	}
}

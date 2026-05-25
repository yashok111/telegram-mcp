package daemon

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEventLogAppendAmortizedStaysBoundedAndReadable(t *testing.T) {
	l := NewEventLog(t.TempDir())
	l.MaxEvents = 50

	// Far more appends than MaxEvents, crossing several compaction boundaries via
	// the O(1) appendLine path in between.
	for i := range 200 {
		require.NoError(t, l.Append(Event{Type: "t", Subject: strconv.Itoa(i)}))
	}

	got, err := l.Recent(0)
	require.NoError(t, err)
	require.Len(t, got, 50, "retention caps the readable window at MaxEvents")
	assert.Equal(t, "199", got[len(got)-1].Subject, "most recent append is retained and last")
}

// makeEventLog returns an EventLog whose Dir is an isolated temp directory.
func makeEventLog(t *testing.T) *EventLog {
	t.Helper()

	return &EventLog{
		Dir:       t.TempDir(),
		Retention: 30 * 24 * time.Hour,
		MaxEvents: 5000,
	}
}

func TestEventLogAppendAndRecent_Roundtrip(t *testing.T) {
	l := makeEventLog(t)

	events := []Event{
		{Type: "shim_disconnected", Severity: "info", Subject: "shim-1", Detail: "gone"},
		{Type: "bg_failed", Severity: "warning", Subject: "bg-42", Detail: "exit 1"},
		{Type: "error_burst", Severity: "critical", Subject: "daemon", Detail: "too many"},
	}

	for _, e := range events {
		require.NoError(t, l.Append(e))
	}

	got, err := l.Recent(0)
	require.NoError(t, err)
	require.Len(t, got, 3)

	for i, e := range got {
		assert.Equal(t, events[i].Type, e.Type)
		assert.Equal(t, events[i].Severity, e.Severity)
		assert.Equal(t, events[i].Subject, e.Subject)
		assert.Equal(t, events[i].Detail, e.Detail)
		assert.False(t, e.TS.IsZero(), "TS must be auto-filled")
	}
}

func TestEventLogRecent_LimitsN(t *testing.T) {
	l := makeEventLog(t)

	for i := range 10 {
		require.NoError(t, l.Append(Event{Type: "shim_disconnected", Subject: string(rune('a' + i))}))
	}

	got, err := l.Recent(3)
	require.NoError(t, err)
	require.Len(t, got, 3, "should return last 3")

	// Oldest-first within the returned window.
	assert.Equal(t, string(rune('a'+7)), got[0].Subject)
	assert.Equal(t, string(rune('a'+8)), got[1].Subject)
	assert.Equal(t, string(rune('a'+9)), got[2].Subject)
}

func TestEventLogRetention_ByCount(t *testing.T) {
	l := &EventLog{
		Dir:       t.TempDir(),
		Retention: 0, // disable age axis so only count trims
		MaxEvents: 3,
	}

	for i := range 5 {
		require.NoError(t, l.Append(Event{Type: "shim_disconnected", Subject: string(rune('a' + i))}))
	}

	got, err := l.Recent(0)
	require.NoError(t, err)
	require.Len(t, got, 3, "count retention must trim to MaxEvents")

	// Should keep the 3 newest: c, d, e.
	assert.Equal(t, "c", got[0].Subject)
	assert.Equal(t, "d", got[1].Subject)
	assert.Equal(t, "e", got[2].Subject)
}

func TestEventLogRetention_ByAge(t *testing.T) {
	l := &EventLog{
		Dir:       t.TempDir(),
		Retention: time.Hour,
		MaxEvents: 0, // disable count axis
	}

	old := Event{
		Type:     "shim_disconnected",
		Subject:  "old",
		TS:       time.Now().Add(-2 * time.Hour),
		Severity: "info",
	}
	fresh := Event{
		Type:     "bg_failed",
		Subject:  "fresh",
		TS:       time.Now().Add(-5 * time.Minute),
		Severity: "warning",
	}

	// Append with explicit TS; Append only fills zero TS so these will be preserved.
	// Write directly via saveAtomic to bypass Append's zero-fill:
	l.mu.Lock()
	err := l.saveAtomic([]Event{old, fresh})
	l.mu.Unlock()
	require.NoError(t, err)

	got, err := l.Recent(0)
	require.NoError(t, err)
	require.Len(t, got, 1, "old event must be pruned by age retention")
	assert.Equal(t, "fresh", got[0].Subject)
}

func TestEventLogRetention_DisableAgeWhenZero(t *testing.T) {
	l := &EventLog{
		Dir:       t.TempDir(),
		Retention: 0, // age retention disabled
		MaxEvents: 0, // count retention disabled
	}

	old := Event{
		Type:    "shim_disconnected",
		Subject: "old",
		TS:      time.Now().Add(-365 * 24 * time.Hour),
	}

	l.mu.Lock()
	err := l.saveAtomic([]Event{old})
	l.mu.Unlock()
	require.NoError(t, err)

	got, err := l.Recent(0)
	require.NoError(t, err)
	require.Len(t, got, 1, "with Retention=0 and MaxEvents=0 nothing is trimmed")
}

func TestEventLogMalformedLineSkip(t *testing.T) {
	// Retention=0 so age axis doesn't drop events with zero/old timestamps.
	l := &EventLog{
		Dir:       t.TempDir(),
		Retention: 0,
		MaxEvents: 0,
	}

	good1 := Event{Type: "shim_disconnected", Severity: "info", Subject: "s1"}
	good2 := Event{Type: "bg_failed", Severity: "warning", Subject: "s2"}

	// Write good1, then a corrupt line, then good2 directly to the file.
	require.NoError(t, os.MkdirAll(l.Dir, 0o700))
	f, err := os.OpenFile(l.filePath(), os.O_CREATE|os.O_WRONLY, 0o600)
	require.NoError(t, err)

	enc := json.NewEncoder(f)
	require.NoError(t, enc.Encode(good1))

	_, err = f.WriteString("{this is not valid json}\n")
	require.NoError(t, err)
	require.NoError(t, enc.Encode(good2))
	require.NoError(t, f.Close())

	got, err := l.Recent(0)
	require.NoError(t, err)
	require.Len(t, got, 2, "malformed line must be skipped; good lines survive")
	assert.Equal(t, "s1", got[0].Subject)
	assert.Equal(t, "s2", got[1].Subject)
}

func TestEventLogSaveAtomic_FileMode(t *testing.T) {
	l := makeEventLog(t)

	require.NoError(t, l.Append(Event{Type: "shim_disconnected", Subject: "x"}))

	info, err := os.Stat(l.filePath())
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "events.jsonl must be 0600")
}

func TestEventLogMissingFile_ReturnsNilNil(t *testing.T) {
	l := makeEventLog(t)

	got, err := l.Recent(0)
	require.NoError(t, err)
	assert.Nil(t, got, "missing file must return nil, nil")
}

// -- EventBus tests --

func TestEventBus_EmitPersistsAndNotifies(t *testing.T) {
	l := makeEventLog(t)

	var mu sync.Mutex

	var notified []string

	notifyFn := func(method string, _ any) bool {
		mu.Lock()

		notified = append(notified, method)

		mu.Unlock()

		return true
	}

	bus := NewEventBus(l, notifyFn)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go bus.Run(ctx)

	bus.Emit(Event{Type: "shim_disconnected", Severity: "info", Subject: "x"})

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()

		return len(notified) == 1
	}, 2*time.Second, 10*time.Millisecond, "notify must be called")

	got, err := l.Recent(0)
	require.NoError(t, err)
	require.Len(t, got, 1, "event must be persisted to log")
	assert.Equal(t, "shim_disconnected", got[0].Type)

	cancel()
}

func TestEventBus_EmitDrop_WhenFull(t *testing.T) {
	l := makeEventLog(t)

	// A bus with no running worker so the channel never drains.
	bus := &EventBus{
		log:    l,
		notify: nil,
		ch:     make(chan Event, 2),
	}

	// Fill the channel.
	bus.Emit(Event{Type: "a"})
	bus.Emit(Event{Type: "b"})

	// This must not block or panic.
	done := make(chan struct{})

	go func() {
		bus.Emit(Event{Type: "c"})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Emit blocked when channel was full")
	}
}

func TestEventBus_RunExitsOnCtxCancel(t *testing.T) {
	// goleak in TestMain will catch any leak; this test just checks Run returns.
	l := makeEventLog(t)
	bus := NewEventBus(l, nil)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})

	go func() {
		bus.Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not exit after ctx cancel")
	}
}

func TestEventBus_RunDrainsBufferedOnCancel(t *testing.T) {
	l := makeEventLog(t)
	bus := NewEventBus(l, nil)

	// Buffer events with no running worker, then cancel before Run so the only
	// path that can persist them is the shutdown drain (or the racy first-select
	// — either way all buffered events must survive).
	bus.Emit(Event{Type: "spawn_crashed", Subject: "s1"})
	bus.Emit(Event{Type: "bg_failed", Subject: "b1"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	bus.Run(ctx) // ctx already cancelled: returns after draining the buffer

	got, err := l.Recent(0)
	require.NoError(t, err)
	require.Len(t, got, 2, "buffered events must be persisted on shutdown drain")
}

func TestEventBus_NilNotify_StillPersists(t *testing.T) {
	l := makeEventLog(t)
	bus := NewEventBus(l, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go bus.Run(ctx)

	bus.Emit(Event{Type: "unauthorized_dm", Subject: "chat-99"})

	require.Eventually(t, func() bool {
		got, _ := l.Recent(0)
		return len(got) == 1
	}, 2*time.Second, 10*time.Millisecond, "event must persist even with nil notify")

	cancel()
}

func TestEventLog_AppendFillsZeroTS(t *testing.T) {
	l := makeEventLog(t)
	before := time.Now()

	require.NoError(t, l.Append(Event{Type: "error_burst"}))

	got, err := l.Recent(0)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.True(t, got[0].TS.After(before) || got[0].TS.Equal(before),
		"auto-filled TS must be at or after time of Append call")
}

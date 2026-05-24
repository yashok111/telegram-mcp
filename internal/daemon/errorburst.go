package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// burstCounter is the shared sliding-window error counter behind one or more
// ErrorBurstHandler views (WithAttrs/WithGroup share the same counter).
type burstCounter struct {
	sink      EventSink
	threshold int
	window    time.Duration
	cooldown  time.Duration

	mu       sync.Mutex
	stamps   []time.Time
	lastEmit time.Time
}

func (c *burstCounter) record(ts time.Time) {
	if ts.IsZero() {
		ts = time.Now()
	}

	c.mu.Lock()

	cutoff := ts.Add(-c.window)
	kept := c.stamps[:0]

	for _, t := range c.stamps {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}

	kept = append(kept, ts)
	c.stamps = kept

	var (
		emit bool
		ev   Event
	)

	if len(c.stamps) >= c.threshold && ts.Sub(c.lastEmit) > c.cooldown {
		emit = true
		c.lastEmit = ts
		ev = Event{
			Type:     "error_burst",
			Severity: "critical",
			Subject:  "daemon",
			Detail:   fmt.Sprintf("%d errors within %s", len(c.stamps), c.window),
		}
		c.stamps = c.stamps[:0]
	}

	c.mu.Unlock()

	if emit && c.sink != nil {
		c.sink.Emit(ev)
	}
}

// ErrorBurstHandler wraps inner; it taps ERROR records into a shared burstCounter
// then delegates. nil sink => counting still happens but no Emit (cheap no-op).
type ErrorBurstHandler struct {
	inner slog.Handler
	c     *burstCounter
}

func NewErrorBurstHandler(inner slog.Handler, sink EventSink, threshold int, window, cooldown time.Duration) *ErrorBurstHandler {
	return &ErrorBurstHandler{
		inner: inner,
		c:     &burstCounter{sink: sink, threshold: threshold, window: window, cooldown: cooldown},
	}
}

func (h *ErrorBurstHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *ErrorBurstHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError {
		h.c.record(r.Time)
	}

	return h.inner.Handle(ctx, r)
}

func (h *ErrorBurstHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &ErrorBurstHandler{inner: h.inner.WithAttrs(as), c: h.c}
}

func (h *ErrorBurstHandler) WithGroup(name string) slog.Handler {
	return &ErrorBurstHandler{inner: h.inner.WithGroup(name), c: h.c}
}

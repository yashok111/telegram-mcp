package daemon

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// discardEnabled is a no-op slog.Handler whose Enabled always returns true so
// records reach ErrorBurstHandler's tap. slog.DiscardHandler can't be used here:
// its Enabled returns false, which would gate every record out before Handle.
type discardEnabled struct{}

func (discardEnabled) Enabled(context.Context, slog.Level) bool  { return true }
func (discardEnabled) Handle(context.Context, slog.Record) error { return nil }
func (d discardEnabled) WithAttrs([]slog.Attr) slog.Handler      { return d }
func (d discardEnabled) WithGroup(string) slog.Handler           { return d }

func TestErrorBurst_EmitsOnThreshold(t *testing.T) {
	sink := &recordingSink{}
	h := NewErrorBurstHandler(discardEnabled{}, sink, 3, time.Minute, time.Minute)
	lg := slog.New(h)

	lg.Error("one")
	lg.Error("two")
	lg.Error("three")

	assert.Equal(t, 1, sink.typeCount("error_burst"))

	lg.Error("four")
	lg.Error("five")

	// After the burst, the window resets to empty; two more errors (< threshold 3)
	// cannot re-trigger. (Cooldown specifically is covered below.)
	assert.Equal(t, 1, sink.typeCount("error_burst"), "post-reset count below threshold must not re-burst")
}

func TestErrorBurst_CooldownSuppressesSustainedStorm(t *testing.T) {
	sink := &recordingSink{}
	// window 1m, cooldown 1h: explicit timestamps drive the sliding window so the
	// cooldown guard (not the post-emit reset) is what suppresses the 2nd burst.
	h := NewErrorBurstHandler(discardEnabled{}, sink, 3, time.Minute, time.Hour)

	base := time.Now()
	h.c.record(base)
	h.c.record(base.Add(time.Second))
	h.c.record(base.Add(2 * time.Second))
	require.Equal(t, 1, sink.typeCount("error_burst"), "first storm bursts")

	// 3 more within the window AND within the cooldown — the cooldown must hold.
	h.c.record(base.Add(3 * time.Second))
	h.c.record(base.Add(4 * time.Second))
	h.c.record(base.Add(5 * time.Second))
	require.Equal(t, 1, sink.typeCount("error_burst"), "cooldown must suppress a 2nd burst within the cooldown window")

	// After the cooldown elapses, a fresh storm bursts again.
	later := base.Add(2 * time.Hour)
	h.c.record(later)
	h.c.record(later.Add(time.Second))
	h.c.record(later.Add(2 * time.Second))
	require.Equal(t, 2, sink.typeCount("error_burst"), "a storm past the cooldown bursts again")
}

func TestErrorBurst_IgnoresNonError(t *testing.T) {
	sink := &recordingSink{}
	h := NewErrorBurstHandler(discardEnabled{}, sink, 3, time.Minute, time.Minute)
	lg := slog.New(h)

	for range 10 {
		lg.Info("info msg")
		lg.Warn("warn msg")
	}

	assert.Equal(t, 0, sink.typeCount("error_burst"))
}

func TestErrorBurst_SharedCounterAcrossWithAttrs(t *testing.T) {
	sink := &recordingSink{}
	h := NewErrorBurstHandler(discardEnabled{}, sink, 3, time.Minute, time.Minute)

	h2, ok := h.WithAttrs([]slog.Attr{slog.String("component", "test")}).(*ErrorBurstHandler)
	require.True(t, ok)

	lg1 := slog.New(h)
	lg2 := slog.New(h2)

	lg1.Error("from h")
	lg2.Error("from h2")
	lg1.Error("from h again")

	require.Equal(t, 1, sink.typeCount("error_burst"), "shared counter must aggregate across derivations")
}

func TestErrorBurst_WindowEviction(t *testing.T) {
	sink := &recordingSink{}
	h := NewErrorBurstHandler(discardEnabled{}, sink, 3, time.Minute, time.Minute)

	now := time.Now()
	old := now.Add(-2 * time.Minute)

	h.c.record(old)
	h.c.record(old)

	assert.Equal(t, 0, sink.typeCount("error_burst"), "old stamps alone must not trigger burst")

	h.c.record(now)

	assert.Equal(t, 0, sink.typeCount("error_burst"), "evicted old stamps must not count toward threshold")
}

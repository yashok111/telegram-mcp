package daemon

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSitrepTicker_DisabledWhenIntervalZero(t *testing.T) {
	var count atomic.Int64

	fire := func() { count.Add(1) }

	s := NewSitrepTicker(0, fire)
	s.Run(context.Background())

	assert.Equal(t, int64(0), count.Load())
}

func TestSitrepTicker_FiresOnTick(t *testing.T) {
	var count atomic.Int64

	fire := func() { count.Add(1) }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	s := NewSitrepTicker(10*time.Millisecond, fire)

	go func() {
		defer close(done)

		s.Run(ctx)
	}()

	require.Eventually(t, func() bool {
		return count.Load() >= 1
	}, 2*time.Second, 5*time.Millisecond)

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestSitrepTicker_ExitsOnCtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	s := NewSitrepTicker(time.Hour, func() {})

	go func() {
		defer close(done)

		s.Run(ctx)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

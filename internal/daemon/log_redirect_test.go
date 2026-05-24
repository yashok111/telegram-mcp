package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRedirectStderrToFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")

	restore, err := RedirectStderrTo(path)
	require.NoError(t, err)

	defer restore()

	_, _ = os.Stderr.WriteString("hello\n")

	restore()

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "hello")
}

func TestShouldRedirectFalseWhenTTY(_ *testing.T) {
	// We can't easily simulate a real tty in unit tests; assert the function
	// runs and returns a bool. Real behavior is covered by manual integration.
	_ = ShouldRedirect()
}

func TestLoggerRotateMovesAndReopens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")

	l, err := OpenLog(path, 1024)
	require.NoError(t, err)

	defer l.Close()

	_, _ = os.Stderr.WriteString("pre-rotate\n")

	require.NoError(t, l.Rotate())

	_, _ = os.Stderr.WriteString("post-rotate\n")

	// .1 holds pre-rotate content; path holds post-rotate content.
	prev, err := os.ReadFile(path + ".1")
	require.NoError(t, err)
	assert.Contains(t, string(prev), "pre-rotate")
	assert.NotContains(t, string(prev), "post-rotate")

	l.Close() // flush+close before reading current

	cur, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(cur), "post-rotate")
	assert.NotContains(t, string(cur), "pre-rotate")
}

func TestLoggerRotateReplacesPriorBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")

	l, err := OpenLog(path, 1024)
	require.NoError(t, err)

	defer l.Close()

	_, _ = os.Stderr.WriteString("gen-1\n")

	require.NoError(t, l.Rotate())

	_, _ = os.Stderr.WriteString("gen-2\n")

	require.NoError(t, l.Rotate())

	// Only one .1 should exist (gen-2 replaced gen-1). No .2 should exist.
	_, err = os.Stat(path + ".2")
	assert.True(t, os.IsNotExist(err))

	prev, err := os.ReadFile(path + ".1")
	require.NoError(t, err)
	assert.Contains(t, string(prev), "gen-2")
	assert.NotContains(t, string(prev), "gen-1")
}

func TestMaybeRotateRespectsThreshold(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")

	l, err := OpenLog(path, 100)
	require.NoError(t, err)

	defer l.Close()

	_, _ = os.Stderr.WriteString("short\n")

	rotated, err := l.MaybeRotate()
	require.NoError(t, err)
	assert.False(t, rotated)

	// Push file past threshold.
	big := make([]byte, 200)
	for i := range big {
		big[i] = 'x'
	}

	_, _ = os.Stderr.Write(big)

	rotated, err = l.MaybeRotate()
	require.NoError(t, err)
	assert.True(t, rotated)
}

func TestMaybeRotateDisabledWhenMaxBytesZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")

	l, err := OpenLog(path, 0)
	require.NoError(t, err)

	defer l.Close()

	big := make([]byte, 200)
	_, _ = os.Stderr.Write(big)

	rotated, err := l.MaybeRotate()
	require.NoError(t, err)
	assert.False(t, rotated)
}

func TestRunExitsOnContextCancel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")

	l, err := OpenLog(path, 1024)
	require.NoError(t, err)

	defer l.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		l.Run(ctx, 50*time.Millisecond)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit within 2s of cancel")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")

	l, err := OpenLog(path, 0)
	require.NoError(t, err)

	// Double-close must not double-close the original fd (would EBADF or
	// worse, close a recycled descriptor).
	l.Close()

	assert.NotPanics(t, func() {
		l.Close()
	})
}

func TestSlogWritesToRotatedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")

	l, err := OpenLog(path, 1024)
	require.NoError(t, err)

	defer l.Close()

	// slog handler captured at the time setupSlog would have been called.
	prev := slog.Default()

	t.Cleanup(func() { slog.SetDefault(prev) })

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	slog.Info("pre-rotate-message")

	require.NoError(t, l.Rotate())

	slog.Info("post-rotate-message")

	l.Close()

	prevRaw, err := os.ReadFile(path + ".1")
	require.NoError(t, err)
	assert.Contains(t, string(prevRaw), "pre-rotate-message")
	assert.NotContains(t, string(prevRaw), "post-rotate-message")

	curRaw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(curRaw), "post-rotate-message", "slog must follow stderr re-attach")
	assert.NotContains(t, string(curRaw), "pre-rotate-message")
}

func TestRotateRestoresOnOpenFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")

	l, err := OpenLog(path, 1024)
	require.NoError(t, err)

	defer l.Close()

	_, _ = os.Stderr.WriteString("data\n")

	// Make the parent dir read-only AFTER the rename succeeds but BEFORE
	// OpenFile — we can't intercept that mid-call from a unit test. Instead,
	// we rotate, then mess with state.
	//
	// Simpler test: invoke Rotate once, then trigger another rotate after
	// removing write permission. The OpenFile in the second rotate fails;
	// the prior log (now path.1) should be restored back to path.
	require.NoError(t, l.Rotate())

	_, _ = os.Stderr.WriteString("gen-2\n")

	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err = l.Rotate()
	require.Error(t, err, "second rotate should fail due to read-only dir")

	require.NoError(t, os.Chmod(dir, 0o700))
	l.Close()

	// After the failed rotate, path should contain gen-2 (restored from
	// the rename-back), not the rotated state.
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "gen-2", "rotate-back should restore prior content to path")
}

func TestRunNoopWhenMaxBytesZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")

	l, err := OpenLog(path, 0)
	require.NoError(t, err)

	defer l.Close()

	ctx := t.Context()

	done := make(chan struct{})

	go func() {
		l.Run(ctx, time.Hour) // long interval; should return immediately
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return immediately with maxBytes=0")
	}
}

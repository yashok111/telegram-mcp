package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShimLogsOpenWriteClose(t *testing.T) {
	dir := t.TempDir()

	sink, err := NewShimLogs(dir, 0)
	require.NoError(t, err)

	require.NoError(t, sink.Open("abc"))
	sink.Write("abc", []byte(`{"msg":"hi"}`+"\n"))
	sink.Close("abc")

	raw, err := os.ReadFile(filepath.Join(dir, "abc.log"))
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"msg":"hi"`)
}

func TestShimLogsOpenIdempotent(t *testing.T) {
	dir := t.TempDir()

	sink, err := NewShimLogs(dir, 0)
	require.NoError(t, err)
	t.Cleanup(sink.CloseAll)

	require.NoError(t, sink.Open("abc"))
	require.NoError(t, sink.Open("abc"))
	assert.True(t, sink.IsOpen("abc"))
}

func TestShimLogsCloseUnknownNoop(t *testing.T) {
	dir := t.TempDir()

	sink, err := NewShimLogs(dir, 0)
	require.NoError(t, err)

	assert.NotPanics(t, func() {
		sink.Close("never-opened")
	})
}

func TestShimLogsWriteDropsWhenUnopened(t *testing.T) {
	dir := t.TempDir()

	sink, err := NewShimLogs(dir, 0)
	require.NoError(t, err)

	sink.Write("abc", []byte("ignored\n"))

	_, err = os.Stat(filepath.Join(dir, "abc.log"))
	assert.True(t, os.IsNotExist(err))
}

func TestShimLogsWriteIgnoresEmpty(t *testing.T) {
	dir := t.TempDir()

	sink, err := NewShimLogs(dir, 0)
	require.NoError(t, err)
	t.Cleanup(sink.CloseAll)

	require.NoError(t, sink.Open("abc"))
	sink.Write("", []byte("nope\n"))
	sink.Write("abc", nil)

	info, err := os.Stat(filepath.Join(dir, "abc.log"))
	require.NoError(t, err)
	assert.Equal(t, int64(0), info.Size())
}

func TestShimLogsRotatesOnMaxBytes(t *testing.T) {
	dir := t.TempDir()

	sink, err := NewShimLogs(dir, 64)
	require.NoError(t, err)
	t.Cleanup(sink.CloseAll)

	require.NoError(t, sink.Open("abc"))

	line := strings.Repeat("x", 32) + "\n"
	for range 5 {
		sink.Write("abc", []byte(line))
	}

	rotatedPath := filepath.Join(dir, "abc.log.1")

	_, err = os.Stat(rotatedPath)
	require.NoError(t, err, "rotation should have created .1 backup")
}

func TestShimLogsRotateNilsHandleOnRenameFailure(t *testing.T) {
	dir := t.TempDir()

	sink, err := NewShimLogs(dir, 64)
	require.NoError(t, err)
	t.Cleanup(sink.CloseAll)

	require.NoError(t, sink.Open("abc"))

	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	line := strings.Repeat("x", 32) + "\n"
	for range 5 {
		sink.Write("abc", []byte(line))
	}

	require.NoError(t, os.Chmod(dir, 0o700))

	sink.mu.Lock()
	file, ok := sink.files["abc"]
	sink.mu.Unlock()
	require.True(t, ok)

	file.mu.Lock()
	assert.Nil(t, file.f, "rotation failure must nil handle so subsequent Writes drop")
	file.mu.Unlock()

	sizeBefore, err := os.Stat(filepath.Join(dir, "abc.log"))
	require.NoError(t, err)

	for range 5 {
		sink.Write("abc", []byte(line))
	}

	sizeAfter, err := os.Stat(filepath.Join(dir, "abc.log"))
	require.NoError(t, err)
	assert.Equal(t, sizeBefore.Size(), sizeAfter.Size(),
		"post-failure writes must drop silently, not thrash the file")
}

func TestShimLogsRotateDisabledWhenMaxBytesZero(t *testing.T) {
	dir := t.TempDir()

	sink, err := NewShimLogs(dir, 0)
	require.NoError(t, err)
	t.Cleanup(sink.CloseAll)

	require.NoError(t, sink.Open("abc"))

	line := strings.Repeat("x", 128) + "\n"
	for range 10 {
		sink.Write("abc", []byte(line))
	}

	_, err = os.Stat(filepath.Join(dir, "abc.log.1"))
	assert.True(t, os.IsNotExist(err), "no rotation when maxBytes=0")
}

func TestShimLogsCloseAllReleasesEveryHandle(t *testing.T) {
	dir := t.TempDir()

	sink, err := NewShimLogs(dir, 0)
	require.NoError(t, err)

	require.NoError(t, sink.Open("a"))
	require.NoError(t, sink.Open("b"))

	sink.CloseAll()

	assert.False(t, sink.IsOpen("a"))
	assert.False(t, sink.IsOpen("b"))
}

func TestShimLogsConcurrentSafe(t *testing.T) {
	dir := t.TempDir()

	sink, err := NewShimLogs(dir, 0)
	require.NoError(t, err)
	t.Cleanup(sink.CloseAll)

	require.NoError(t, sink.Open("a"))
	require.NoError(t, sink.Open("b"))

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(2)
		go func() { defer wg.Done(); sink.Write("a", []byte("aa\n")) }()
		go func() { defer wg.Done(); sink.Write("b", []byte("bb\n")) }()
	}

	wg.Wait()

	aRaw, err := os.ReadFile(filepath.Join(dir, "a.log"))
	require.NoError(t, err)
	bRaw, err := os.ReadFile(filepath.Join(dir, "b.log"))
	require.NoError(t, err)

	assert.NotContains(t, string(aRaw), "bb")
	assert.NotContains(t, string(bRaw), "aa")
}

func TestShimLogHandlerFansShimIDRecord(t *testing.T) {
	dir := t.TempDir()

	sink, err := NewShimLogs(dir, 0)
	require.NoError(t, err)
	t.Cleanup(sink.CloseAll)

	require.NoError(t, sink.Open("abc"))

	stderrBuf := &syncBuffer{}
	inner := slog.NewJSONHandler(stderrBuf, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(NewShimLogHandler(inner, sink))

	logger.Info("hello", "shim_id", "abc", "extra", "value")

	rawA, err := os.ReadFile(filepath.Join(dir, "abc.log"))
	require.NoError(t, err)
	assert.Contains(t, string(rawA), `"msg":"hello"`)
	assert.Contains(t, string(rawA), `"shim_id":"abc"`)
	assert.Contains(t, string(rawA), `"extra":"value"`)
	assert.Contains(t, stderrBuf.String(), `"msg":"hello"`, "inner handler must still fire")
}

func TestShimLogHandlerSkipsRecordsWithoutShimID(t *testing.T) {
	dir := t.TempDir()

	sink, err := NewShimLogs(dir, 0)
	require.NoError(t, err)
	t.Cleanup(sink.CloseAll)

	require.NoError(t, sink.Open("abc"))

	stderrBuf := &syncBuffer{}
	inner := slog.NewJSONHandler(stderrBuf, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(NewShimLogHandler(inner, sink))

	logger.Info("daemon-level", "request_id", "r1")

	info, err := os.Stat(filepath.Join(dir, "abc.log"))
	require.NoError(t, err)
	assert.Equal(t, int64(0), info.Size())
}

func TestShimLogHandlerSkipsRecordsWithUnknownShim(t *testing.T) {
	dir := t.TempDir()

	sink, err := NewShimLogs(dir, 0)
	require.NoError(t, err)
	t.Cleanup(sink.CloseAll)

	stderrBuf := &syncBuffer{}
	inner := slog.NewJSONHandler(stderrBuf, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(NewShimLogHandler(inner, sink))

	logger.Info("orphan", "shim_id", "ghost")

	_, err = os.Stat(filepath.Join(dir, "ghost.log"))
	assert.True(t, os.IsNotExist(err), "Write must not auto-create file for unknown shim")
}

func TestShimLogHandlerWithAttrsCarriesShimID(t *testing.T) {
	dir := t.TempDir()

	sink, err := NewShimLogs(dir, 0)
	require.NoError(t, err)
	t.Cleanup(sink.CloseAll)

	require.NoError(t, sink.Open("abc"))

	stderrBuf := &syncBuffer{}
	inner := slog.NewJSONHandler(stderrBuf, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(NewShimLogHandler(inner, sink)).With("shim_id", "abc")

	logger.Info("bound")

	raw, err := os.ReadFile(filepath.Join(dir, "abc.log"))
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"msg":"bound"`)
	assert.Contains(t, string(raw), `"shim_id":"abc"`)
}

func TestShimLogHandlerNilSinkIsTransparent(t *testing.T) {
	stderrBuf := &syncBuffer{}
	inner := slog.NewJSONHandler(stderrBuf, &slog.HandlerOptions{Level: slog.LevelDebug})

	logger := slog.New(NewShimLogHandler(inner, nil))
	logger.Info("ok", "shim_id", "abc")

	assert.Contains(t, stderrBuf.String(), `"msg":"ok"`)
}

func TestShimLogHandlerEnabledMatchesInner(t *testing.T) {
	stderrBuf := &syncBuffer{}
	inner := slog.NewJSONHandler(stderrBuf, &slog.HandlerOptions{Level: slog.LevelWarn})

	h := NewShimLogHandler(inner, nil)
	ctx := context.Background()

	assert.False(t, h.Enabled(ctx, slog.LevelInfo))
	assert.True(t, h.Enabled(ctx, slog.LevelError))
}

// syncBuffer is a goroutine-safe bytes.Buffer wrapper. Inline rather than
// pulled from a fixture so the test file is self-contained.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

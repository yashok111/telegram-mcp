package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewShimsSweepNilWhenTTLZero(t *testing.T) {
	dir := t.TempDir()
	assert.Nil(t, NewShimsSweep(dir, nil, 0, time.Hour))
	assert.Nil(t, NewShimsSweep(dir, nil, -time.Hour, time.Hour))
}

func TestShimsSweepRemovesStaleClosedFiles(t *testing.T) {
	dir := t.TempDir()

	sink, err := NewShimLogs(dir, 0)
	require.NoError(t, err)
	t.Cleanup(sink.CloseAll)

	stale := filepath.Join(dir, "abc.log")
	require.NoError(t, os.WriteFile(stale, []byte("old\n"), 0o600))

	old := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(stale, old, old))

	sw := NewShimsSweep(dir, sink, time.Hour, time.Hour)
	require.NotNil(t, sw)

	sw.sweepOnce()

	_, err = os.Stat(stale)
	assert.True(t, os.IsNotExist(err), "stale file should be removed")
}

func TestShimsSweepKeepsFreshFiles(t *testing.T) {
	dir := t.TempDir()

	fresh := filepath.Join(dir, "abc.log")
	require.NoError(t, os.WriteFile(fresh, []byte("recent\n"), 0o600))

	sw := NewShimsSweep(dir, nil, time.Hour, time.Hour)
	sw.sweepOnce()

	_, err := os.Stat(fresh)
	require.NoError(t, err, "fresh file should be kept")
}

func TestShimsSweepSkipsCurrentlyOpenFile(t *testing.T) {
	dir := t.TempDir()

	sink, err := NewShimLogs(dir, 0)
	require.NoError(t, err)
	t.Cleanup(sink.CloseAll)

	require.NoError(t, sink.Open("abc"))

	target := filepath.Join(dir, "abc.log")
	old := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(target, old, old))

	sw := NewShimsSweep(dir, sink, time.Hour, time.Hour)
	sw.sweepOnce()

	_, err = os.Stat(target)
	require.NoError(t, err, "open file must survive sweep regardless of mtime")
}

func TestShimsSweepRemovesRotatedBackup(t *testing.T) {
	dir := t.TempDir()

	stale := filepath.Join(dir, "abc.log.1")
	require.NoError(t, os.WriteFile(stale, []byte("rotated\n"), 0o600))

	old := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(stale, old, old))

	sw := NewShimsSweep(dir, nil, time.Hour, time.Hour)
	sw.sweepOnce()

	_, err := os.Stat(stale)
	assert.True(t, os.IsNotExist(err), "rotated .log.1 should also be removable")
}

func TestShimsSweepIgnoresUnknownFiles(t *testing.T) {
	dir := t.TempDir()

	keepers := []string{"notes.txt", "other.json", "stash"}
	for _, name := range keepers {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))

		old := time.Now().Add(-2 * time.Hour)
		require.NoError(t, os.Chtimes(path, old, old))
	}

	sw := NewShimsSweep(dir, nil, time.Hour, time.Hour)
	sw.sweepOnce()

	for _, name := range keepers {
		_, err := os.Stat(filepath.Join(dir, name))
		require.NoError(t, err, "non-.log files must be left alone: %s", name)
	}
}

func TestShimsSweepReaddirMissingDirIsSilent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")

	sw := NewShimsSweep(dir, nil, time.Hour, time.Hour)
	require.NotNil(t, sw)

	assert.NotPanics(t, sw.sweepOnce)
}

func TestShimsSweepRunExitsOnContextCancel(t *testing.T) {
	dir := t.TempDir()

	sw := NewShimsSweep(dir, nil, time.Hour, 20*time.Millisecond)
	require.NotNil(t, sw)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		sw.Run(ctx)
		close(done)
	}()

	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit within 2s of cancel")
	}
}

func TestShimIDFromFilename(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"abc.log", "abc", true},
		{"abc.log.1", "abc", true},
		{"shim-id.log", "shim-id", true},
		{"shim-id.log.1", "shim-id", true},
		{".log", "", false},
		{".log.1", "", false},
		{"abc.txt", "", false},
		{"abc", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := shimIDFromFilename(tc.in)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

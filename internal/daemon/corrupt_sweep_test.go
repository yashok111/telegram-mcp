package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yakov/telegram-mcp/internal/access"
)

func TestNewCorruptSweepNilWhenTTLZero(t *testing.T) {
	store := access.NewStore(t.TempDir(), false)
	cs := NewCorruptSweep(store, 0, time.Hour)
	assert.Nil(t, cs)
}

func TestCorruptSweepRemovesOldFiles(t *testing.T) {
	dir := t.TempDir()
	store := access.NewStore(dir, false)

	old := filepath.Join(dir, "access.json.corrupt-100")
	require.NoError(t, os.WriteFile(old, []byte("garbage"), 0o600))

	twoDaysAgo := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(old, twoDaysAgo, twoDaysAgo))

	fresh := filepath.Join(dir, "access.json.corrupt-9999")
	require.NoError(t, os.WriteFile(fresh, []byte("recent"), 0o600))

	cs := NewCorruptSweep(store, 24*time.Hour, time.Hour)
	require.NotNil(t, cs)
	cs.sweepOnce()

	_, err := os.Stat(old)
	assert.True(t, os.IsNotExist(err), "old corrupt file should be removed")

	_, err = os.Stat(fresh)
	assert.NoError(t, err, "fresh corrupt file should be kept")
}

func TestCorruptSweepSkipsNonCorruptFiles(t *testing.T) {
	dir := t.TempDir()
	store := access.NewStore(dir, false)

	keep := filepath.Join(dir, "access.json")
	require.NoError(t, os.WriteFile(keep, []byte("{}"), 0o600))

	long := time.Now().Add(-365 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(keep, long, long))

	cs := NewCorruptSweep(store, 24*time.Hour, time.Hour)
	cs.sweepOnce()

	_, err := os.Stat(keep)
	assert.NoError(t, err, "access.json must never be deleted by corrupt sweep")
}

func TestCorruptSweepRunExitsOnContextCancel(t *testing.T) {
	dir := t.TempDir()
	store := access.NewStore(dir, false)
	cs := NewCorruptSweep(store, time.Hour, 50*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		cs.Run(ctx)
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

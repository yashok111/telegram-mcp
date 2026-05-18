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

func newInboxStore(t *testing.T) *access.Store {
	t.Helper()

	dir := t.TempDir()
	store := access.NewStore(dir, false)

	require.NoError(t, os.MkdirAll(store.InboxDir(), 0o700))

	return store
}

func TestInboxCleanup_disabledWhenTTLZero(t *testing.T) {
	store := newInboxStore(t)

	assert.Nil(t, NewInboxCleanup(store, 0, 0))
	assert.Nil(t, NewInboxCleanup(store, -time.Second, time.Hour))
}

func TestInboxCleanup_removesOldFiles(t *testing.T) {
	store := newInboxStore(t)

	path := filepath.Join(store.InboxDir(), "old.bin")
	require.NoError(t, os.WriteFile(path, []byte("stale"), 0o600))

	old := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(path, old, old))

	ic := NewInboxCleanup(store, time.Hour, time.Hour)
	require.NotNil(t, ic)
	ic.sweepOnce()

	_, err := os.Stat(path)
	assert.True(t, os.IsNotExist(err), "expected old file to be removed, got err=%v", err)
}

func TestInboxCleanup_keepsRecentFiles(t *testing.T) {
	store := newInboxStore(t)

	path := filepath.Join(store.InboxDir(), "fresh.bin")
	require.NoError(t, os.WriteFile(path, []byte("recent"), 0o600))

	ic := NewInboxCleanup(store, time.Hour, time.Hour)
	require.NotNil(t, ic)
	ic.sweepOnce()

	_, err := os.Stat(path)
	require.NoError(t, err, "fresh file must survive sweep")
}

func TestInboxCleanup_missingDir_silentNoop(t *testing.T) {
	dir := t.TempDir()
	store := access.NewStore(dir, false)

	// Intentionally do NOT create InboxDir.
	_, err := os.Stat(store.InboxDir())
	require.True(t, os.IsNotExist(err))

	ic := NewInboxCleanup(store, time.Hour, time.Hour)
	require.NotNil(t, ic)

	assert.NotPanics(t, ic.sweepOnce)
}

func TestInboxCleanup_ctxCancelReturns(t *testing.T) {
	store := newInboxStore(t)

	ic := NewInboxCleanup(store, time.Hour, 10*time.Millisecond)
	require.NotNil(t, ic)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() {
		ic.Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("InboxCleanup.Run did not return within 100ms of ctx cancel")
	}
}

func TestInboxCleanup_skipsSubdirectories(t *testing.T) {
	store := newInboxStore(t)

	sub := filepath.Join(store.InboxDir(), "subdir")
	require.NoError(t, os.MkdirAll(sub, 0o700))

	old := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(sub, old, old))

	ic := NewInboxCleanup(store, time.Hour, time.Hour)
	require.NotNil(t, ic)
	ic.sweepOnce()

	info, err := os.Stat(sub)
	require.NoError(t, err, "subdirectory must survive sweep")
	assert.True(t, info.IsDir())
}

package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPendingStorePutTakeRoundTrip(t *testing.T) {
	ps := NewPendingStore(t.TempDir())

	p := PendingMutation{
		ID:               "ab12cd34",
		Tool:             "evict_session",
		Args:             json.RawMessage(`{"target":"s2"}`),
		Summary:          "evict session @s2",
		ConfirmChatID:    330621952,
		ConfirmMessageID: 77,
		CreatedAt:        time.Now().UTC(),
	}
	require.NoError(t, ps.Put(p))

	got, ok := ps.Take("ab12cd34")
	require.True(t, ok)
	assert.Equal(t, "evict_session", got.Tool)
	assert.JSONEq(t, `{"target":"s2"}`, string(got.Args))
	assert.Equal(t, int64(330621952), got.ConfirmChatID)
	assert.Equal(t, 77, got.ConfirmMessageID)

	// Take consumes the record.
	_, ok = ps.Take("ab12cd34")
	assert.False(t, ok, "second Take must miss — Put record is consumed")
}

func TestPendingStoreTakeUnknown(t *testing.T) {
	ps := NewPendingStore(t.TempDir())
	_, ok := ps.Take("deadbeef")
	assert.False(t, ok)
}

func TestPendingStoreTakeRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	ps := NewPendingStore(dir)

	// A non-hex id (path traversal attempt) must miss without touching disk.
	_, ok := ps.Take("../../etc/passwd")
	assert.False(t, ok)
}

func TestPendingStorePutFileMode0600(t *testing.T) {
	dir := t.TempDir()
	ps := NewPendingStore(dir)
	require.NoError(t, ps.Put(PendingMutation{ID: "feed01", Tool: "add_allow", CreatedAt: time.Now()}))

	info, err := os.Stat(filepath.Join(dir, "admin", "pending", "feed01.json"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestPendingStoreSweepRemovesExpired(t *testing.T) {
	ps := NewPendingStore(t.TempDir())

	require.NoError(t, ps.Put(PendingMutation{ID: "a1d111", Tool: "add_allow", CreatedAt: time.Now().Add(-10 * time.Minute)}))
	require.NoError(t, ps.Put(PendingMutation{ID: "b2e222", Tool: "add_allow", CreatedAt: time.Now()}))

	expired := ps.Sweep(5 * time.Minute)
	require.Len(t, expired, 1)
	assert.Equal(t, "a1d111", expired[0].ID)

	// Fresh one survives and is still takeable; expired one is gone.
	_, ok := ps.Take("b2e222")
	assert.True(t, ok)
	_, ok = ps.Take("a1d111")
	assert.False(t, ok)
}

func TestPendingStoreSweepMissingDir(t *testing.T) {
	ps := NewPendingStore(t.TempDir()) // pending dir never created
	assert.Empty(t, ps.Sweep(time.Minute), "sweep of a missing dir is a no-op")
}

func TestPendingStoreSweepDropsCorruptRecord(t *testing.T) {
	dir := t.TempDir()
	ps := NewPendingStore(dir)

	// Seed a fresh valid record so the pending dir exists, then drop a corrupt
	// file beside it. Sweep must remove the corrupt one (it can never resolve)
	// without touching the valid, unexpired one.
	require.NoError(t, ps.Put(PendingMutation{ID: "facade", Tool: "add_allow", CreatedAt: time.Now()}))

	corrupt := filepath.Join(dir, "admin", "pending", "badbad.json")
	require.NoError(t, os.WriteFile(corrupt, []byte("{not json"), 0o600))

	expired := ps.Sweep(5 * time.Minute)
	assert.Empty(t, expired, "corrupt records are dropped, not returned as expired")

	_, err := os.Stat(corrupt)
	assert.True(t, os.IsNotExist(err), "corrupt record must be removed")

	_, ok := ps.Take("facade")
	assert.True(t, ok, "valid unexpired record survives the sweep")
}

func TestPendingStoreSetConfirmMessageIDNoopAfterTake(t *testing.T) {
	ps := NewPendingStore(t.TempDir())
	require.NoError(t, ps.Put(PendingMutation{ID: "abc12345", Tool: "evict_session", Summary: "x"}))

	// Patch before take: persists.
	require.NoError(t, ps.SetConfirmMessageID("abc12345", 7))
	got, ok := ps.Take("abc12345")
	require.True(t, ok)
	assert.Equal(t, 7, got.ConfirmMessageID)

	// After take the record is gone — a patch must NOT recreate a ghost that a
	// second confirm tap could double-apply.
	require.NoError(t, ps.SetConfirmMessageID("abc12345", 9))
	_, ok = ps.Take("abc12345")
	assert.False(t, ok, "SetConfirmMessageID must not resurrect a consumed pending")
}

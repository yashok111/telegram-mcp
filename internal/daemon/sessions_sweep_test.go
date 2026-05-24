package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yakov/telegram-mcp/internal/access"
)

func writeSessionFile(t *testing.T, dir, name string, shimPID int) string {
	t.Helper()

	full := filepath.Join(dir, name)
	payload := map[string]any{
		"alias":      "s1",
		"shim_pid":   shimPID,
		"cc_pid":     9999,
		"workdir":    "/tmp",
		"mode":       "shim",
		"shim_id":    "deadbeef",
		"started_at": time.Now(),
	}

	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(full, raw, 0o600))

	return full
}

func newTestSweep(t *testing.T, alive func(int) bool, ttl time.Duration) (*SessionsSweep, string) {
	t.Helper()

	stateDir := t.TempDir()
	sessionsDir := filepath.Join(stateDir, "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o700))

	store := access.NewStore(stateDir, false)
	sw := NewSessionsSweep(store, ttl, time.Hour)
	require.NotNil(t, sw)
	sw.procAlive = alive

	return sw, sessionsDir
}

func TestNewSessionsSweepNilWhenTTLZero(t *testing.T) {
	store := access.NewStore(t.TempDir(), false)
	assert.Nil(t, NewSessionsSweep(store, 0, time.Hour))
}

func TestSessionsSweepRemovesOrphan(t *testing.T) {
	sw, dir := newTestSweep(t, func(int) bool { return false }, 100*time.Millisecond)
	path := writeSessionFile(t, dir, "111.json", 111)

	old := time.Now().Add(-1 * time.Hour)
	require.NoError(t, os.Chtimes(path, old, old))

	sw.sweepOnce()

	_, err := os.Stat(path)
	assert.True(t, os.IsNotExist(err))
}

func TestSessionsSweepKeepsLive(t *testing.T) {
	sw, dir := newTestSweep(t, func(pid int) bool { return pid == 222 }, 100*time.Millisecond)
	path := writeSessionFile(t, dir, "222.json", 222)

	old := time.Now().Add(-1 * time.Hour)
	require.NoError(t, os.Chtimes(path, old, old))

	sw.sweepOnce()

	_, err := os.Stat(path)
	assert.NoError(t, err)
}

func TestSessionsSweepRaceProtection(t *testing.T) {
	// Fresh file (under TTL) must not be deleted even if procAlive says dead.
	sw, dir := newTestSweep(t, func(int) bool { return false }, time.Hour)
	path := writeSessionFile(t, dir, "333.json", 333)

	sw.sweepOnce()

	_, err := os.Stat(path)
	assert.NoError(t, err, "fresh session must survive ttl gate")
}

func TestSessionsSweepRemovesCorruptOldFile(t *testing.T) {
	sw, dir := newTestSweep(t, func(int) bool { return true }, 100*time.Millisecond)

	path := filepath.Join(dir, "corrupt.json")
	require.NoError(t, os.WriteFile(path, []byte("not-json"), 0o600))

	old := time.Now().Add(-1 * time.Hour)
	require.NoError(t, os.Chtimes(path, old, old))

	sw.sweepOnce()

	_, err := os.Stat(path)
	assert.True(t, os.IsNotExist(err), "old unreadable file should be removed")
}

func TestSessionsSweepSkipsNonJSON(t *testing.T) {
	sw, dir := newTestSweep(t, func(int) bool { return false }, 100*time.Millisecond)

	other := filepath.Join(dir, "stray.txt")
	require.NoError(t, os.WriteFile(other, []byte("hi"), 0o600))

	old := time.Now().Add(-1 * time.Hour)
	require.NoError(t, os.Chtimes(other, old, old))

	sw.sweepOnce()

	_, err := os.Stat(other)
	assert.NoError(t, err, "non-.json files outside our control must not be deleted")
}

func TestSessionsSweepRunExitsOnContextCancel(t *testing.T) {
	sw, _ := newTestSweep(t, func(int) bool { return true }, time.Hour)
	sw.interval = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		sw.Run(ctx)
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

package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/bot"
)

func TestServer_evictStalePending_removesOld(t *testing.T) {
	srv, _, _ := newServerWithAllowlist(t)

	now := time.Now()

	srv.permMu.Lock()
	srv.pending["old"] = pendingEntry{
		details:   bot.PermissionDetails{ToolName: "Bash"},
		createdAt: now.Add(-2 * pendingTTL),
	}
	srv.pending["fresh"] = pendingEntry{
		details:   bot.PermissionDetails{ToolName: "Read"},
		createdAt: now.Add(-time.Minute),
	}
	srv.permMu.Unlock()

	evicted := srv.evictStalePending(now)
	assert.Equal(t, 1, evicted)

	_, ok := srv.LookupPermission("old")
	assert.False(t, ok, "stale entry should be evicted")

	_, ok = srv.LookupPermission("fresh")
	assert.True(t, ok, "fresh entry should remain")
}

func TestServer_evictStalePending_keepsFresh(t *testing.T) {
	srv, _, _ := newServerWithAllowlist(t)

	now := time.Now()

	srv.permMu.Lock()
	srv.pending["a"] = pendingEntry{createdAt: now.Add(-time.Second)}
	srv.pending["b"] = pendingEntry{createdAt: now.Add(-5 * time.Minute)}
	srv.permMu.Unlock()

	evicted := srv.evictStalePending(now)
	assert.Equal(t, 0, evicted)

	_, ok := srv.LookupPermission("a")
	assert.True(t, ok)
	_, ok = srv.LookupPermission("b")
	assert.True(t, ok)
}

func TestServer_evictStalePending_emptyMap_zero(t *testing.T) {
	srv, _, _ := newServerWithAllowlist(t)
	assert.Equal(t, 0, srv.evictStalePending(time.Now()))
}

func TestServer_LookupPermission_unwrapsDetails(t *testing.T) {
	srv, _, _ := newServerWithAllowlist(t)

	want := bot.PermissionDetails{
		ToolName:     "Write",
		Description:  "write file",
		InputPreview: `{"file_path":"/tmp/x"}`,
	}

	srv.permMu.Lock()
	srv.pending["req"] = pendingEntry{details: want, createdAt: time.Now()}
	srv.permMu.Unlock()

	got, ok := srv.LookupPermission("req")
	require.True(t, ok)
	assert.Equal(t, want, got, "LookupPermission returns bare PermissionDetails")
}

func TestServer_pendingCleanup_ctxCancelReturns(t *testing.T) {
	srv, _, _ := newServerWithAllowlist(t)

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.pendingCleanup(ctx, time.Hour)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("pendingCleanup did not return within 100ms of ctx cancel")
	}
}

func TestServer_pendingCleanup_evictsViaTicker(t *testing.T) {
	srv, _, _ := newServerWithAllowlist(t)

	srv.permMu.Lock()
	srv.pending["old"] = pendingEntry{createdAt: time.Now().Add(-2 * pendingTTL)}
	srv.permMu.Unlock()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.pendingCleanup(ctx, 10*time.Millisecond)
	}()

	require.Eventually(t, func() bool {
		_, ok := srv.LookupPermission("old")
		return !ok
	}, time.Second, 10*time.Millisecond, "ticker-driven sweep never evicted the old entry")

	cancel()
	<-done
}

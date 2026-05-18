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

func newCleanupStore(t *testing.T) *access.Store {
	t.Helper()

	dir := t.TempDir()
	store := access.NewStore(dir, false)

	return store
}

func TestRulesCleanup_pruneOnce_dropsExpired(t *testing.T) {
	store := newCleanupStore(t)

	now := time.Now().UnixMilli()
	st := access.State{
		DMPolicy:  access.PolicyAllowlist,
		AllowFrom: []string{},
		Groups:    map[string]access.GroupPolicy{},
		Pending:   map[string]access.Pending{},
		Rules: []access.PermissionRule{
			{ID: "expired", Tool: "Read", Action: access.RuleApprove, ExpiresAt: now - 1000, CreatedAt: now - 5000},
			{ID: "fresh", Tool: "Bash", Action: access.RuleApprove, ExpiresAt: now + 60_000, CreatedAt: now},
		},
	}
	require.NoError(t, store.Save(st))

	rc := NewRulesCleanup(store, time.Minute)
	rc.pruneOnce()

	reloaded := store.Load()
	require.Len(t, reloaded.Rules, 1)
	assert.Equal(t, "fresh", reloaded.Rules[0].ID)
}

func TestRulesCleanup_pruneOnce_noopWhenNothingExpired(t *testing.T) {
	store := newCleanupStore(t)

	now := time.Now().UnixMilli()
	st := access.State{
		DMPolicy:  access.PolicyAllowlist,
		AllowFrom: []string{},
		Groups:    map[string]access.GroupPolicy{},
		Pending:   map[string]access.Pending{},
		Rules: []access.PermissionRule{
			{ID: "a", Tool: "Read", Action: access.RuleApprove, ExpiresAt: 0, CreatedAt: now},
			{ID: "b", Tool: "Bash", Action: access.RuleApprove, ExpiresAt: now + 60_000, CreatedAt: now},
		},
	}
	require.NoError(t, store.Save(st))

	dir := filepath.Dir(store.ApprovedDir())
	path := filepath.Join(dir, "access.json")
	before, err := os.Stat(path)
	require.NoError(t, err)

	// Ensure mtime resolution can distinguish a rewrite if it happens.
	time.Sleep(10 * time.Millisecond)

	rc := NewRulesCleanup(store, time.Minute)
	rc.pruneOnce()

	after, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, before.ModTime(), after.ModTime(), "pruneOnce must not rewrite when nothing expired")

	reloaded := store.Load()
	require.Len(t, reloaded.Rules, 2)
}

func TestRulesCleanup_ctxCancelReturns(t *testing.T) {
	store := newCleanupStore(t)

	rc := NewRulesCleanup(store, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() {
		rc.Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RulesCleanup.Run did not return after ctx cancel")
	}
}

func TestNewRulesCleanup_defaultsToMinute_whenZero(t *testing.T) {
	store := newCleanupStore(t)

	rc := NewRulesCleanup(store, 0)
	assert.Equal(t, time.Minute, rc.interval)

	rcNeg := NewRulesCleanup(store, -1)
	assert.Equal(t, time.Minute, rcNeg.interval)
}

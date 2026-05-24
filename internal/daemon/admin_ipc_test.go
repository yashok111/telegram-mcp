package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/access"
	"github.com/yakov/telegram-mcp/internal/bot"
	"github.com/yakov/telegram-mcp/internal/ipc"
)

type fakeSpawnLister struct{ items []bot.SpawnTaskInfo }

func (f fakeSpawnLister) List() []bot.SpawnTaskInfo { return f.items }

type fakeBgLister struct{ items []bot.BgTaskInfo }

func (f fakeBgLister) List() []bot.BgTaskInfo { return f.items }

func newAdminSnapHandlers(t *testing.T, token string) *Handlers {
	t.Helper()

	h := NewHandlers(access.NewStore(t.TempDir(), false), nil, NewRouter(), nil)
	h.SetAdminToken(token)

	return h
}

func snapParams(t *testing.T, token string) json.RawMessage {
	t.Helper()

	raw, err := json.Marshal(map[string]string{"token": token})
	require.NoError(t, err)

	return raw
}

func TestHandleAdminSnapshotRejectsBadOrMissingToken(t *testing.T) {
	h := newAdminSnapHandlers(t, "secret")

	for _, tok := range []string{"", "wrong"} {
		_, rpcErr := h.HandleAdminSnapshot(context.Background(), nil, snapParams(t, tok))
		require.NotNil(t, rpcErr, "token %q must be rejected", tok)
		assert.Equal(t, ipc.CodeUnauthorized, rpcErr.Code)
	}
}

func TestHandleAdminSnapshotRejectsWhenNoTokenConfigured(t *testing.T) {
	h := newAdminSnapHandlers(t, "") // admin disabled daemon-wide

	_, rpcErr := h.HandleAdminSnapshot(context.Background(), nil, snapParams(t, "anything"))
	require.NotNil(t, rpcErr)
	assert.Equal(t, ipc.CodeUnauthorized, rpcErr.Code)
}

func TestHandleAdminSnapshotReturnsLiveState(t *testing.T) {
	h := newAdminSnapHandlers(t, "secret")
	h.router.Register(&Shim{ID: "abc123def456", Label: "main", Workdir: "/w", Notify: func(string, any) error { return nil }})
	h.SetRunners(
		fakeSpawnLister{items: []bot.SpawnTaskInfo{{ID: "sp1", Pid: 42, Status: "running", StartedAt: time.Now()}}},
		fakeBgLister{items: []bot.BgTaskInfo{{ID: "bg1", Status: "running", PromptHead: "do x"}}},
	)

	res, rpcErr := h.HandleAdminSnapshot(context.Background(), nil, snapParams(t, "secret"))
	require.Nil(t, rpcErr)

	snap, ok := res.(AdminSnapshot)
	require.True(t, ok)

	require.Len(t, snap.Shims, 1)
	assert.Equal(t, "abc123def456", snap.Shims[0].ID)
	assert.Equal(t, "main", snap.Shims[0].Label)
	assert.NotEmpty(t, snap.Shims[0].Alias, "Register assigns an alias")

	require.Len(t, snap.Spawns, 1)
	assert.Equal(t, "sp1", snap.Spawns[0].ID)
	assert.Equal(t, 42, snap.Spawns[0].Pid)

	require.Len(t, snap.Bg, 1)
	assert.Equal(t, "bg1", snap.Bg[0].ID)
}

func TestHandleAdminSnapshotNilRunnersEmptySections(t *testing.T) {
	h := newAdminSnapHandlers(t, "secret") // SetRunners never called

	res, rpcErr := h.HandleAdminSnapshot(context.Background(), nil, snapParams(t, "secret"))
	require.Nil(t, rpcErr)

	snap, ok := res.(AdminSnapshot)
	require.True(t, ok)
	assert.Empty(t, snap.Shims)
	assert.Empty(t, snap.Spawns)
	assert.Empty(t, snap.Bg)
}

func TestHandleAdminSnapshotJSONRoundTrips(t *testing.T) {
	// The snapshot crosses IPC as JSON; the admin package decodes the tagged
	// fields without importing daemon, so the wire shape must be stable.
	h := newAdminSnapHandlers(t, "secret")
	h.router.Register(&Shim{ID: "xyz", Notify: func(string, any) error { return nil }})

	res, rpcErr := h.HandleAdminSnapshot(context.Background(), nil, snapParams(t, "secret"))
	require.Nil(t, rpcErr)

	raw, err := json.Marshal(res)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"shims"`)
	assert.Contains(t, string(raw), `"id":"xyz"`)
}

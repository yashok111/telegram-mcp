package main

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	adminpkg "github.com/yakov/telegram-mcp/internal/admin"
	daemonpkg "github.com/yakov/telegram-mcp/internal/daemon"
)

// assertNoZeroFields fails if any exported field of v is its zero value. The
// wire-compat test sets every producer field to a non-zero value, so a zero on
// the consumer side after the JSON round-trip means a tag mismatch dropped it.
func assertNoZeroFields(t *testing.T, label string, v any) {
	t.Helper()

	rv := reflect.ValueOf(v)
	for i := range rv.NumField() {
		assert.Falsef(t, rv.Field(i).IsZero(),
			"%s.%s decoded as zero — JSON tag drift between producer and consumer", label, rv.Type().Field(i).Name)
	}
}

// TestAdminSnapshotWireCompat guards the hand-mirrored structs on the two sides
// of the admin.snapshot IPC method: daemon.AdminSnapshot (producer) and
// admin.Snapshot (consumer) live in packages that cannot import each other, so
// only a marshal→unmarshal round-trip catches a field-tag drift between them.
func TestAdminSnapshotWireCompat(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)

	src := daemonpkg.AdminSnapshot{
		Shims: []daemonpkg.AdminShim{{
			ID: "abc123", Alias: "s1", Label: "main", Workdir: "/w",
			CCSessionID: "cc", SpawnID: "sp", TopicID: 7,
			ConnectedAt: now, LastOutbound: now, PinnedChats: []string{"42"}, Role: "admin",
		}},
		Spawns: []daemonpkg.AdminSpawn{{ID: "sp1", Pid: 99, Workdir: "/w", UserID: "u", ChatID: "42", Status: "running", StartedAt: now}},
		Bg:     []daemonpkg.AdminBg{{ID: "bg1", Workdir: "/w", PromptHead: "do x", UserID: "u", Status: "running", StartedAt: now}},
	}

	raw, err := json.Marshal(src)
	require.NoError(t, err)

	var dst adminpkg.Snapshot
	require.NoError(t, json.Unmarshal(raw, &dst))

	require.Len(t, dst.Shims, 1)
	assert.Equal(t, "abc123", dst.Shims[0].ID)
	assert.Equal(t, "s1", dst.Shims[0].Alias)
	assert.Equal(t, "main", dst.Shims[0].Label)
	assert.Equal(t, 7, dst.Shims[0].TopicID)
	assert.Equal(t, []string{"42"}, dst.Shims[0].PinnedChats)
	assert.True(t, dst.Shims[0].ConnectedAt.Equal(now))

	require.Len(t, dst.Spawns, 1)
	assert.Equal(t, "sp1", dst.Spawns[0].ID)
	assert.Equal(t, 99, dst.Spawns[0].Pid)
	assert.Equal(t, "42", dst.Spawns[0].ChatID)

	require.Len(t, dst.Bg, 1)
	assert.Equal(t, "bg1", dst.Bg[0].ID)
	assert.Equal(t, "do x", dst.Bg[0].PromptHead)

	// Every field was set non-zero on the producer; any zero here is a dropped
	// field (tag mismatch). Catches drift the explicit asserts above would miss.
	assertNoZeroFields(t, "ShimSnap", dst.Shims[0])
	assertNoZeroFields(t, "SpawnSnap", dst.Spawns[0])
	assertNoZeroFields(t, "BgSnap", dst.Bg[0])
}

// TestAdminEventWireCompat guards the hand-mirrored Event struct: daemon.Event
// (producer — written to events.jsonl and pushed over NotifyAdminEvent) and
// admin.Event (consumer — read by the agent + the list_recent_events tool) live
// in packages that can't import each other. A tag drift would silently drop a
// field; only a round-trip catches it.
func TestAdminEventWireCompat(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)

	src := daemonpkg.Event{
		Type:     "shim_disconnected",
		Severity: "warning",
		TS:       now,
		Subject:  "abc123",
		Detail:   "shim disconnected without goodbye",
	}

	raw, err := json.Marshal(src)
	require.NoError(t, err)

	var dst adminpkg.Event
	require.NoError(t, json.Unmarshal(raw, &dst))

	assert.Equal(t, "shim_disconnected", dst.Type)
	assert.Equal(t, "warning", dst.Severity)
	assert.Equal(t, "abc123", dst.Subject)
	assert.Equal(t, "shim disconnected without goodbye", dst.Detail)
	assert.True(t, dst.TS.Equal(now))

	assertNoZeroFields(t, "Event", dst)
}

// TestAdminMutateWireCompat guards the hand-mirrored admin.mutate reply:
// daemon.AdminMutateResult (producer) and admin.MutateResult (consumer) live in
// packages that can't import each other. Every field is set non-zero so a tag
// drift that drops one surfaces here.
func TestAdminMutateWireCompat(t *testing.T) {
	src := daemonpkg.AdminMutateResult{
		Tool:      "evict_session",
		Tier:      3,
		Applied:   true,
		Pending:   true,
		PendingID: "ab12cd34",
		Result:    "awaiting owner approval",
	}

	raw, err := json.Marshal(src)
	require.NoError(t, err)

	var dst adminpkg.MutateResult
	require.NoError(t, json.Unmarshal(raw, &dst))

	assert.Equal(t, "evict_session", dst.Tool)
	assert.Equal(t, 3, dst.Tier)
	assert.True(t, dst.Applied)
	assert.True(t, dst.Pending)
	assert.Equal(t, "ab12cd34", dst.PendingID)
	assert.Equal(t, "awaiting owner approval", dst.Result)

	assertNoZeroFields(t, "MutateResult", dst)
}

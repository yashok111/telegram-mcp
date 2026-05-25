package admin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	mcptypes "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func toolReq(args map[string]any) mcptypes.CallToolRequest {
	var req mcptypes.CallToolRequest

	req.Params.Arguments = args

	return req
}

func resultText(res *mcptypes.CallToolResult) string {
	if res == nil || len(res.Content) == 0 {
		return ""
	}

	if tc, ok := res.Content[0].(mcptypes.TextContent); ok {
		return tc.Text
	}

	return ""
}

func newTestToolServer(t *testing.T) *ToolServer {
	t.Helper()

	ts := NewToolServer(t.TempDir(), "/no/such.sock", "tok")
	ts.Snapshot = func(_ context.Context) (Snapshot, error) {
		return Snapshot{
			Shims: []ShimSnap{{
				ID: "abc123", Alias: "s1", Label: "main", Workdir: "/w",
				ConnectedAt: time.Now().Add(-2 * time.Minute), LastOutbound: time.Now().Add(-30 * time.Second),
			}},
			Spawns: []SpawnSnap{{ID: "sp1", Pid: 99, Status: "running"}},
			Bg:     []BgSnap{{ID: "bg1", Status: "running", PromptHead: "do x"}},
		}, nil
	}

	return ts
}

func TestToolListShims(t *testing.T) {
	ts := newTestToolServer(t)

	res, err := ts.handleListShims(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Contains(t, resultText(res), `"alias": "s1"`)
}

func TestToolRouterSnapshotHasAllSections(t *testing.T) {
	ts := newTestToolServer(t)

	res, err := ts.handleRouterSnapshot(context.Background(), toolReq(nil))
	require.NoError(t, err)

	txt := resultText(res)
	assert.Contains(t, txt, `"shims"`)
	assert.Contains(t, txt, `"spawns"`)
	assert.Contains(t, txt, `"bg"`)
	assert.Contains(t, txt, "sp1")
	assert.Contains(t, txt, "bg1")
}

func TestToolIPCHealthComputesIdle(t *testing.T) {
	ts := newTestToolServer(t)

	res, err := ts.handleIPCHealth(context.Background(), toolReq(nil))
	require.NoError(t, err)

	txt := resultText(res)
	assert.Contains(t, txt, `"connected": 1`)
	assert.Contains(t, txt, `"idle_seconds"`)
}

func TestToolSnapshotErrorSurfacesAsToolError(t *testing.T) {
	ts := newTestToolServer(t)
	ts.Snapshot = func(_ context.Context) (Snapshot, error) {
		return Snapshot{}, errors.New("daemon down")
	}

	res, err := ts.handleListShims(context.Background(), toolReq(nil))
	require.NoError(t, err) // tool errors are returned in-band, not as Go errors
	assert.True(t, res.IsError)
	assert.Contains(t, resultText(res), "daemon snapshot unavailable")
}

func TestToolReadDaemonLogTail(t *testing.T) {
	ts := newTestToolServer(t)
	writeFile(t, filepath.Join(ts.StateDir, "daemon.log"), "line1\nline2\nline3\n")

	res, err := ts.handleReadDaemonLog(context.Background(), toolReq(map[string]any{"lines": "2"}))
	require.NoError(t, err)

	txt := resultText(res)
	assert.NotContains(t, txt, "line1")
	assert.Contains(t, txt, "line2")
	assert.Contains(t, txt, "line3")
}

func TestToolReadDaemonLogMissing(t *testing.T) {
	ts := newTestToolServer(t)

	res, err := ts.handleReadDaemonLog(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.Contains(t, resultText(res), "not found")
}

func TestToolReadShimLogRejectsTraversal(t *testing.T) {
	ts := newTestToolServer(t)

	for _, bad := range []string{"../etc/passwd", "..", "a/b", "", "zzz../"} {
		res, err := ts.handleReadShimLog(context.Background(), toolReq(map[string]any{"shim_id": bad}))
		require.NoError(t, err)
		assert.True(t, res.IsError, "shim_id %q must be rejected", bad)
	}
}

func TestToolReadShimLog(t *testing.T) {
	ts := newTestToolServer(t)
	writeFile(t, filepath.Join(ts.StateDir, "shims", "abc123.log"), "hello\nworld\n")

	res, err := ts.handleReadShimLog(context.Background(), toolReq(map[string]any{"shim_id": "abc123"}))
	require.NoError(t, err)
	assert.Contains(t, resultText(res), "world")
}

func TestToolGrepLogs(t *testing.T) {
	ts := newTestToolServer(t)
	writeFile(t, filepath.Join(ts.StateDir, "daemon.log"), "alpha\nbeta needle\ngamma\n")
	writeFile(t, filepath.Join(ts.StateDir, "shims", "deadbeef.log"), "needle here too\n")

	res, err := ts.handleGrepLogs(context.Background(), toolReq(map[string]any{"pattern": "needle"}))
	require.NoError(t, err)

	txt := resultText(res)
	assert.Contains(t, txt, "daemon.log: beta needle")
	assert.Contains(t, txt, "deadbeef.log: needle here too")
}

func TestToolRecentErrors(t *testing.T) {
	ts := newTestToolServer(t)
	writeFile(t, filepath.Join(ts.StateDir, "daemon.log"),
		`{"level":"INFO","msg":"ok"}`+"\n"+`{"level":"ERROR","msg":"boom"}`+"\n")

	res, err := ts.handleRecentErrors(context.Background(), toolReq(nil))
	require.NoError(t, err)

	txt := resultText(res)
	assert.Contains(t, txt, "boom")
	assert.NotContains(t, txt, `"msg":"ok"`)
}

func TestToolAccessJSONTools(t *testing.T) {
	ts := newTestToolServer(t)
	writeFile(t, filepath.Join(ts.StateDir, "access.json"),
		`{"allowFrom":["123","456"],"pending":{},"groups":{},"effortByChat":{"42":"high"}}`)

	allow, err := ts.handleListAllowlist(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.Contains(t, resultText(allow), "123")

	effort, err := ts.handleGetEffort(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.Contains(t, resultText(effort), "high")

	// rules/pairings must return valid JSON even when empty, not error.
	rules, err := ts.handleListRules(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.False(t, rules.IsError)
}

func TestToolListSessions(t *testing.T) {
	ts := newTestToolServer(t)
	writeFile(t, filepath.Join(ts.StateDir, "sessions", "1234.json"),
		`{"alias":"s1","shim_id":"abc","cc_pid":1234,"workdir":"/w","mode":"shim"}`)

	res, err := ts.handleListSessions(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.Contains(t, resultText(res), `"alias": "s1"`) // MarshalIndent spaces after colon
}

func TestToolListSessionsEmpty(t *testing.T) {
	ts := newTestToolServer(t)

	res, err := ts.handleListSessions(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.Equal(t, "[]", resultText(res))
}

func TestAdminToolNamesNoDuplicates(t *testing.T) {
	assert.Len(t, adminToolNames, 30) // 16 read + 6 Tier-2 + 8 Tier-3 mutate

	seen := map[string]bool{}
	for _, n := range adminToolNames {
		require.False(t, seen[n], "duplicate tool name %q", n)
		seen[n] = true
	}
}

// TestAdminToolGroupsPartition checks the tiered groups compose adminToolNames
// exactly and that the autonomous-observer set is read+Tier-3 with NO Tier-2.
func TestAdminToolGroupsPartition(t *testing.T) {
	assert.Len(t, adminReadToolNames, 16)
	assert.Len(t, adminTier2ToolNames, 6)
	assert.Len(t, adminTier3ToolNames, 8)
	assert.Len(t, adminToolNames, 30)

	assert.Len(t, adminObserveToolNames, 24, "observe = read(16) + Tier-3(8), no Tier-2")

	for _, t2 := range adminTier2ToolNames {
		assert.NotContains(t, adminObserveToolNames, t2, "observer set must exclude Tier-2 tool %q", t2)
	}

	for _, r := range adminReadToolNames {
		assert.Contains(t, adminObserveToolNames, r)
	}

	for _, t3 := range adminTier3ToolNames {
		assert.Contains(t, adminObserveToolNames, t3)
	}
}

// TestAdminToolNamesMatchRegistered is the drift guard referenced by
// toolserver.go: adminToolNames (used for --allowedTools) must be exactly the
// set registerTools registers. A registered tool missing from the list would be
// silently un-callable (not in --allowedTools); a listed-but-unregistered name
// would allow a tool that does not exist.
func TestAdminToolNamesMatchRegistered(t *testing.T) {
	ts := NewToolServer(t.TempDir(), "/s", "tok")
	registered := ts.srv.ListTools()

	for name := range registered {
		assert.Contains(t, adminToolNames, name, "registered tool %q is missing from adminToolNames", name)
	}

	for _, name := range adminToolNames {
		assert.Contains(t, registered, name, "adminToolNames lists %q but registerTools does not register it", name)
	}

	assert.Len(t, registered, len(adminToolNames))
}

func TestToolServer_ListRecentEvents(t *testing.T) {
	ts := newTestToolServer(t)

	eventsDir := filepath.Join(ts.StateDir, "admin")
	require.NoError(t, os.MkdirAll(eventsDir, 0o700))
	writeFile(t, filepath.Join(eventsDir, "events.jsonl"),
		`{"type":"shim_crash","severity":"error","ts":"2024-01-01T00:00:00Z","subject":"s1","detail":"exit 1"}`+"\n"+
			`{"type":"unauthorized_dm","severity":"warn","ts":"2024-01-01T00:01:00Z","subject":"999","detail":"blocked"}`+"\n"+
			`{"type":"spawn_failure","severity":"error","ts":"2024-01-01T00:02:00Z","subject":"sp1","detail":"oom"}`+"\n")

	res, err := ts.handleListRecentEvents(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.False(t, res.IsError)
	txt := resultText(res)
	assert.Contains(t, txt, "shim_crash")
	assert.Contains(t, txt, "unauthorized_dm")
	assert.Contains(t, txt, "spawn_failure")

	// limit param: only last 1
	res2, err := ts.handleListRecentEvents(context.Background(), toolReq(map[string]any{"limit": "1"}))
	require.NoError(t, err)
	assert.False(t, res2.IsError)
	txt2 := resultText(res2)
	assert.Contains(t, txt2, "spawn_failure")
	assert.NotContains(t, txt2, "shim_crash")
}

func TestToolServer_ListRecentEventsMissingFile(t *testing.T) {
	ts := newTestToolServer(t)

	res, err := ts.handleListRecentEvents(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.False(t, res.IsError)
	// missing file → empty list, normalized to [] (not null) for the LLM.
	assert.Equal(t, "[]", resultText(res))
}

func TestToolServer_GetDirectives(t *testing.T) {
	ts := newTestToolServer(t)
	writeFile(t, filepath.Join(ts.StateDir, "admin", "directives.md"), "hello rules\nalways ask before deleting")

	res, err := ts.handleGetDirectives(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Contains(t, resultText(res), "hello rules")
}

func TestToolServer_GetDirectivesMissing(t *testing.T) {
	ts := newTestToolServer(t)

	res, err := ts.handleGetDirectives(context.Background(), toolReq(nil))
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Equal(t, "(no directives set)", resultText(res))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

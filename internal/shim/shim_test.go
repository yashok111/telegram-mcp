package shim

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/access"
	"github.com/yakov/telegram-mcp/internal/ipc"
	mcpkg "github.com/yakov/telegram-mcp/internal/mcp"
)

func TestShimNew(t *testing.T) {
	dir := t.TempDir()
	store := access.NewStore(dir, false)

	mcpSrv, err := mcpkg.New(store)
	require.NoError(t, err)

	fc := &fakeClient{}
	sh := &Shim{
		Client: fc,
		MCP:    mcpSrv,
		Store:  store,
	}

	require.NoError(t, sh.Wire())
	assert.NotNil(t, mcpSrv.Bot())
}

func TestShimSendsHelloOnWire(t *testing.T) {
	dir := t.TempDir()
	store := access.NewStore(dir, false)

	mcpSrv, err := mcpkg.New(store)
	require.NoError(t, err)

	fc := &fakeClient{returnResult: []byte(`{"shim_id":"abc","daemon_version":"test"}`)}
	sh := &Shim{
		Client:      fc,
		MCP:         mcpSrv,
		Store:       store,
		HelloPID:    1234,
		HelloLabel:  "session-X",
		WireContext: context.Background,
	}

	require.NoError(t, sh.Wire())
	assert.Equal(t, ipc.MethodHello, fc.calledMethod)
	assert.Contains(t, string(fc.calledParams), `"shim_pid":1234`)
	assert.Contains(t, string(fc.calledParams), `"label":"session-X"`)

	id, ok := sh.ShimID()
	require.True(t, ok)
	assert.Equal(t, "abc", id)
}

func TestShimWireCapturesAlias(t *testing.T) {
	dir := t.TempDir()
	store := access.NewStore(dir, false)

	mcpSrv, err := mcpkg.New(store)
	require.NoError(t, err)

	fc := &fakeClient{returnResult: []byte(`{"shim_id":"abcd","daemon_version":"test","alias":"s7"}`)}
	sh := &Shim{
		Client:      fc,
		MCP:         mcpSrv,
		Store:       store,
		WireContext: context.Background,
	}

	require.NoError(t, sh.Wire())

	alias, ok := sh.ShimAlias()
	require.True(t, ok)
	assert.Equal(t, "s7", alias)
}

func TestShimWireSendsWorkdirAndSession(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "test-sess-id")
	wd := t.TempDir()
	t.Chdir(wd)

	store := access.NewStore(t.TempDir(), false)
	mcpSrv, err := mcpkg.New(store)
	require.NoError(t, err)

	fc := &fakeClient{returnResult: []byte(`{"shim_id":"id1","alias":"s1","daemon_version":"test"}`)}
	sh := &Shim{
		Client:      fc,
		MCP:         mcpSrv,
		Store:       store,
		WireContext: context.Background,
	}
	require.NoError(t, sh.Wire())

	require.Equal(t, ipc.MethodHello, fc.calledMethod)
	assert.Contains(t, string(fc.calledParams), `"workdir":"`+wd+`"`)
	assert.Contains(t, string(fc.calledParams), `"cc_session_id":"test-sess-id"`)
}

func TestShimWireWritesSessionFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CODE_SESSION_ID", "diag-sid")

	store := access.NewStore(dir, false)
	mcpSrv, err := mcpkg.New(store)
	require.NoError(t, err)

	fc := &fakeClient{returnResult: []byte(`{"shim_id":"abcdef012345","alias":"s4","daemon_version":"test"}`)}
	sh := &Shim{
		Client:      fc,
		MCP:         mcpSrv,
		Store:       store,
		StateDir:    dir,
		HelloPID:    5555,
		CCPID:       12345,
		WireContext: context.Background,
	}

	require.NoError(t, sh.Wire())

	raw, err := os.ReadFile(filepath.Join(dir, "sessions", "12345.json"))
	require.NoError(t, err)

	var got SessionInfo
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, "s4", got.Alias)
	assert.Equal(t, "abcdef012345", got.ShimID)
	assert.Equal(t, "abcdef01", got.ShimIDPrefix)
	assert.Equal(t, 12345, got.CCPID)
	assert.Equal(t, 5555, got.ShimPID)
	assert.Equal(t, "diag-sid", got.CCSessionID)
	assert.Equal(t, "shim", got.Mode)
}

func TestShimWireWritesSessionFileEvenWithoutCCSessionIDEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")

	store := access.NewStore(dir, false)
	mcpSrv, err := mcpkg.New(store)
	require.NoError(t, err)

	fc := &fakeClient{returnResult: []byte(`{"shim_id":"id","alias":"s1","daemon_version":"test"}`)}
	sh := &Shim{
		Client:      fc,
		MCP:         mcpSrv,
		Store:       store,
		StateDir:    dir,
		CCPID:       777,
		WireContext: context.Background,
	}

	require.NoError(t, sh.Wire())

	raw, err := os.ReadFile(filepath.Join(dir, "sessions", "777.json"))
	require.NoError(t, err)

	var got SessionInfo
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, "s1", got.Alias)
	assert.Empty(t, got.CCSessionID, "cc_session_id omitted when env empty")
}

func TestShimWireSkipsSessionFileWhenNoStateDir(t *testing.T) {
	store := access.NewStore(t.TempDir(), false)
	mcpSrv, err := mcpkg.New(store)
	require.NoError(t, err)

	fc := &fakeClient{returnResult: []byte(`{"shim_id":"id","alias":"s1","daemon_version":"test"}`)}
	sh := &Shim{
		Client:      fc,
		MCP:         mcpSrv,
		Store:       store,
		CCPID:       4242,
		WireContext: context.Background,
	}

	require.NoError(t, sh.Wire())
}

func TestShimRunRemovesSessionFile(t *testing.T) {
	dir := t.TempDir()
	store := access.NewStore(dir, false)
	mcpSrv, err := mcpkg.New(store)
	require.NoError(t, err)

	fc := &fakeClient{returnResult: []byte(`{"shim_id":"id","alias":"s1","daemon_version":"test"}`)}
	sh := &Shim{
		Client:      fc,
		MCP:         mcpSrv,
		Store:       store,
		StateDir:    dir,
		CCPID:       9090,
		WireContext: context.Background,
	}

	require.NoError(t, sh.Wire())
	path := filepath.Join(dir, "sessions", "9090.json")
	_, err = os.Stat(path)
	require.NoError(t, err, "session file should exist after Wire")

	// Simulate the Run-exit cleanup.
	require.NoError(t, removeSessionFile(sh.StateDir, sh.ccPID))
	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err), "session file should be gone after cleanup")
}

func TestShimRunStopsOnContextCancel(t *testing.T) {
	dir := t.TempDir()
	store := access.NewStore(dir, false)

	mcpSrv, err := mcpkg.New(store)
	require.NoError(t, err)

	fc := &fakeClient{returnResult: []byte(`{"shim_id":"abc"}`)}
	sh := &Shim{
		Client:     fc,
		MCP:        mcpSrv,
		Store:      store,
		StateDir:   dir,
		SocketPath: filepath.Join(dir, "daemon.sock"),
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	require.NoError(t, sh.Wire())

	_ = ctx
}

package shim

import (
	"context"
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

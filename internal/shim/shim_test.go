package shim

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

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

	require.NoError(t, sh.Wire(context.Background()))
	assert.NotNil(t, mcpSrv.Bot())
}

func TestShimSendsHelloOnWire(t *testing.T) {
	dir := t.TempDir()
	store := access.NewStore(dir, false)

	mcpSrv, err := mcpkg.New(store)
	require.NoError(t, err)

	fc := &fakeClient{returnResult: []byte(`{"shim_id":"abc","daemon_version":"test"}`)}
	sh := &Shim{
		Client:     fc,
		MCP:        mcpSrv,
		Store:      store,
		HelloPID:   1234,
		HelloLabel: "session-X",
	}

	require.NoError(t, sh.Wire(context.Background()))
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
		Client: fc,
		MCP:    mcpSrv,
		Store:  store,
	}

	require.NoError(t, sh.Wire(context.Background()))

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
		Client: fc,
		MCP:    mcpSrv,
		Store:  store,
	}
	require.NoError(t, sh.Wire(context.Background()))

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
		Client:   fc,
		MCP:      mcpSrv,
		Store:    store,
		StateDir: dir,
		HelloPID: 5555,
		CCPID:    12345,
	}

	require.NoError(t, sh.Wire(context.Background()))

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
		Client:   fc,
		MCP:      mcpSrv,
		Store:    store,
		StateDir: dir,
		CCPID:    777,
	}

	require.NoError(t, sh.Wire(context.Background()))

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
		Client: fc,
		MCP:    mcpSrv,
		Store:  store,
		CCPID:  4242,
	}

	require.NoError(t, sh.Wire(context.Background()))
}

func TestShimRunRemovesSessionFile(t *testing.T) {
	dir := t.TempDir()
	store := access.NewStore(dir, false)
	mcpSrv, err := mcpkg.New(store)
	require.NoError(t, err)

	fc := &fakeClient{returnResult: []byte(`{"shim_id":"id","alias":"s1","daemon_version":"test"}`)}
	sh := &Shim{
		Client:   fc,
		MCP:      mcpSrv,
		Store:    store,
		StateDir: dir,
		CCPID:    9090,
	}

	require.NoError(t, sh.Wire(context.Background()))

	path := filepath.Join(dir, "sessions", "9090.json")
	_, err = os.Stat(path)
	require.NoError(t, err, "session file should exist after Wire")

	// Simulate the Run-exit cleanup.
	require.NoError(t, removeSessionFile(sh.StateDir, sh.ccPID))

	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err), "session file should be gone after cleanup")
}

func TestShimRunReconnectsAfterClientDone(t *testing.T) {
	dir := t.TempDir()
	store := access.NewStore(dir, false)
	mcpSrv, err := mcpkg.New(store)
	require.NoError(t, err)

	initClient := &fakeClient{
		returnResult: []byte(`{"shim_id":"abc","alias":"s1","daemon_version":"test"}`),
		doneCh:       make(chan struct{}),
	}

	reconnClient := &fakeClient{
		returnResult: []byte(`{"shim_id":"xyz","alias":"s7","daemon_version":"test"}`),
		doneCh:       make(chan struct{}),
	}

	var dialed atomic.Int32

	servedCtx := make(chan context.Context, 1)
	served := make(chan error, 1)

	sh := &Shim{
		Client:     initClient,
		MCP:        mcpSrv,
		Store:      store,
		StateDir:   dir,
		SocketPath: filepath.Join(dir, "daemon.sock"),
		CCPID:      4321,
		DialIPC: func(string) (IPCClient, error) {
			dialed.Add(1)
			return reconnClient, nil
		},
		ServeStdio: func(ctx context.Context) error {
			servedCtx <- ctx

			<-ctx.Done()

			return nil
		},
	}

	runCtx, cancel := context.WithCancel(t.Context())
	defer cancel()

	go func() { served <- sh.Run(runCtx) }()

	select {
	case <-servedCtx:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeStdio was never invoked")
	}

	require.Eventually(t, func() bool { return initClient.helloCount.Load() == 1 },
		2*time.Second, 10*time.Millisecond, "initial Hello never sent")

	initClient.closeDone()

	require.Eventually(t, func() bool {
		return dialed.Load() >= 1 && reconnClient.helloCount.Load() >= 1
	}, 2*time.Second, 10*time.Millisecond, "reconnect Hello never sent")

	alias, ok := sh.ShimAlias()
	require.True(t, ok)
	assert.Equal(t, "s7", alias, "alias updated after reconnect")

	cancel()

	select {
	case err := <-served:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after ctx cancel post-reconnect")
	}
}

func TestShimRunReconnectBackoffRespectsCtx(t *testing.T) {
	dir := t.TempDir()
	store := access.NewStore(dir, false)
	mcpSrv, err := mcpkg.New(store)
	require.NoError(t, err)

	initClient := &fakeClient{
		returnResult: []byte(`{"shim_id":"abc","alias":"s1","daemon_version":"test"}`),
		doneCh:       make(chan struct{}),
	}

	var dialed atomic.Int32

	sh := &Shim{
		Client:     initClient,
		MCP:        mcpSrv,
		Store:      store,
		StateDir:   dir,
		SocketPath: filepath.Join(dir, "daemon.sock"),
		CCPID:      9999,
		DialIPC: func(string) (IPCClient, error) {
			dialed.Add(1)
			return nil, errors.New("daemon unreachable")
		},
		ServeStdio: func(ctx context.Context) error {
			<-ctx.Done()
			return nil
		},
	}

	runCtx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)

	go func() { served <- sh.Run(runCtx) }()

	require.Eventually(t, func() bool { return initClient.helloCount.Load() == 1 },
		2*time.Second, 10*time.Millisecond, "initial Hello never sent")

	initClient.closeDone()

	require.Eventually(t, func() bool { return dialed.Load() >= 1 },
		2*time.Second, 10*time.Millisecond, "reconnect dial never attempted")

	cancel()

	select {
	case err := <-served:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit during backoff after ctx cancel")
	}
}

func TestBackoffDelayCapsAtMax(t *testing.T) {
	assert.Equal(t, 100*time.Millisecond, backoffDelay(0))
	assert.Equal(t, 200*time.Millisecond, backoffDelay(1))
	assert.Equal(t, 400*time.Millisecond, backoffDelay(2))
	assert.Equal(t, reconnectMaxBackoff, backoffDelay(20))
	assert.Equal(t, reconnectMaxBackoff, backoffDelay(100))
}

// TestShimRunReconnectsThroughRealIPC exercises the reconnect path against
// real ipc.Server and ipc.Dial — the closest unit-level analogue of a live
// `systemctl --user restart telegram-mcp` event short of spawning the daemon
// binary. Server 1 accepts initial Hello, exits; server 2 binds the same
// socket and accepts the reconnect Hello.
func TestShimRunReconnectsThroughRealIPC(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "daemon.sock")

	var helloCount atomic.Int32

	helloHandler := func(alias string) func(_ context.Context, _ *ipc.Conn, _ json.RawMessage) (any, *ipc.Error) {
		return func(_ context.Context, _ *ipc.Conn, _ json.RawMessage) (any, *ipc.Error) {
			helloCount.Add(1)

			return map[string]string{
				"shim_id":        "real-" + alias,
				"alias":          alias,
				"daemon_version": "test",
			}, nil
		}
	}

	startServer := func(alias string) (*ipc.Server, context.CancelFunc, chan struct{}) {
		srv := ipc.NewServer(sock)
		srv.Handle(ipc.MethodHello, helloHandler(alias))

		srvCtx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})

		go func() {
			_ = srv.Listen(srvCtx)

			close(done)
		}()

		require.Eventually(t, func() bool {
			_, err := os.Stat(sock)
			return err == nil
		}, 2*time.Second, 10*time.Millisecond, "server %s never bound socket", alias)

		return srv, cancel, done
	}

	_, cancel1, done1 := startServer("first")

	client, err := ipc.Dial(sock)
	require.NoError(t, err)

	store := access.NewStore(dir, false)
	mcpSrv, err := mcpkg.New(store)
	require.NoError(t, err)

	sh := &Shim{
		Client:     client,
		MCP:        mcpSrv,
		Store:      store,
		StateDir:   dir,
		SocketPath: sock,
		CCPID:      1234,
		DialIPC: func(p string) (IPCClient, error) {
			return ipc.Dial(p)
		},
		ServeStdio: func(ctx context.Context) error {
			<-ctx.Done()
			return nil
		},
	}

	runCtx, cancelRun := context.WithCancel(t.Context())
	served := make(chan error, 1)

	go func() { served <- sh.Run(runCtx) }()

	require.Eventually(t, func() bool { return helloCount.Load() == 1 },
		2*time.Second, 10*time.Millisecond, "initial Hello never observed by server")

	alias, ok := sh.ShimAlias()
	require.True(t, ok)
	assert.Equal(t, "first", alias)

	cancel1()
	<-done1

	_, cancel2, done2 := startServer("second")
	defer func() {
		cancel2()
		<-done2
	}()

	require.Eventually(t, func() bool { return helloCount.Load() >= 2 },
		5*time.Second, 50*time.Millisecond, "reconnect Hello never observed by second server")

	require.Eventually(t, func() bool {
		a, ok := sh.ShimAlias()
		return ok && a == "second"
	}, 2*time.Second, 10*time.Millisecond, "shim alias never updated after reconnect")

	cancelRun()

	select {
	case err := <-served:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after ctx cancel post-reconnect")
	}
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

	require.NoError(t, sh.Wire(context.Background()))

	_ = ctx
}

func TestShimUpdateLabelRewritesSessionfile(t *testing.T) {
	stateDir := t.TempDir()
	ccPID := 4242

	originalStart := time.Now().UTC().Add(-time.Hour)
	s := &Shim{
		StateDir:   stateDir,
		HelloPID:   1234,
		HelloLabel: "old",
	}
	s.id = "abcdef012345"
	s.alias = "s1"
	s.ccPID = ccPID
	s.startedAt = originalStart

	s.UpdateLabel("old")
	s.UpdateLabel("new-label")

	path := filepath.Join(stateDir, "sessions", strconv.Itoa(ccPID)+".json")
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"label":"new-label"`)

	var got SessionInfo
	require.NoError(t, json.Unmarshal(b, &got))
	assert.True(t, got.StartedAt.Equal(originalStart),
		"StartedAt must be preserved on label change: want %s got %s", originalStart, got.StartedAt)
}

func TestShimUpdateLabelEmptyClears(t *testing.T) {
	stateDir := t.TempDir()
	s := &Shim{StateDir: stateDir, HelloPID: 1234, HelloLabel: "old"}
	s.id = "abcdef012345"
	s.alias = "s1"
	s.ccPID = 1234

	s.UpdateLabel("")

	path := filepath.Join(stateDir, "sessions", "1234.json")
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(b), `"label"`)
}

func TestShimUpdateLabelNoStateDirNoop(_ *testing.T) {
	s := &Shim{HelloPID: 1234}
	s.id = "abcdef012345"
	s.ccPID = 1234

	s.UpdateLabel("x")
}

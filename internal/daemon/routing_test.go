package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreAnyFunction("github.com/valyala/fasthttp.(*HostClient).connsCleaner"),
		goleak.IgnoreAnyFunction("github.com/valyala/fasthttp.(*Client).mCleaner"),
		goleak.IgnoreAnyFunction("github.com/valyala/fasthttp.(*TCPDialer).tcpAddrsClean"),
		goleak.IgnoreAnyFunction("github.com/mymmrac/telego.(*Bot).doLongPolling"),
	)
}

func TestRouterRecordOutboundAndRouteInbound(t *testing.T) {
	r := NewRouter()
	a := &Shim{ID: "a"}
	b := &Shim{ID: "b"}

	r.Register(a)
	r.Register(b)

	r.RecordOutbound("a", "chat-1")

	got, ok := r.RouteInbound("chat-1")
	require.True(t, ok)
	assert.Equal(t, "a", got.ID)
}

func TestRouterLRUFallback(t *testing.T) {
	r := NewRouter()
	a := &Shim{ID: "a"}
	b := &Shim{ID: "b"}

	r.Register(a)
	r.Register(b) // b is most recent

	got, ok := r.RouteInbound("never-seen")
	require.True(t, ok)
	assert.Equal(t, "b", got.ID)
}

func TestRouterNoShims(t *testing.T) {
	r := NewRouter()
	_, ok := r.RouteInbound("any")
	assert.False(t, ok)
}

func TestRouterDropShimClearsOwnership(t *testing.T) {
	r := NewRouter()
	a := &Shim{ID: "a"}
	b := &Shim{ID: "b"}

	r.Register(a)
	r.Register(b)
	r.RecordOutbound("a", "chat-1")

	r.Drop("a")

	got, ok := r.RouteInbound("chat-1")
	require.True(t, ok)
	assert.Equal(t, "b", got.ID, "chat ownership transferred to LRU after drop")
}

func TestRouterPermissionRegisterRoutesByID(t *testing.T) {
	r := NewRouter()
	a := &Shim{ID: "a"}
	r.Register(a)

	require.NoError(t, r.RegisterPermission("ababc", "a", PermDetails{ToolName: "Bash"}))

	got, ok := r.RoutePermission("ababc")
	require.True(t, ok)
	assert.Equal(t, "a", got.ID)
}

func TestRouterPermissionCollisionReturnsError(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "a"})
	r.Register(&Shim{ID: "b"})

	require.NoError(t, r.RegisterPermission("dupe1", "a", PermDetails{}))
	err := r.RegisterPermission("dupe1", "b", PermDetails{})
	assert.ErrorIs(t, err, ErrPermissionIDInUse)
}

func TestRouterPermissionResolveRemoves(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "a"})

	require.NoError(t, r.RegisterPermission("k", "a", PermDetails{}))
	r.ResolvePermission("k")

	_, ok := r.RoutePermission("k")
	assert.False(t, ok)
}

func TestRouterDropShimReleasesPermissions(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "a"})

	require.NoError(t, r.RegisterPermission("q", "a", PermDetails{}))
	r.Drop("a")

	_, ok := r.RoutePermission("q")
	assert.False(t, ok)
}

func TestRouterPermissionDetailsLookup(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "a"})

	d := PermDetails{ToolName: "Bash", Description: "run", InputPreview: "ls -la"}
	require.NoError(t, r.RegisterPermission("k", "a", d))

	got, ok := r.LookupPermissionDetails("k")
	require.True(t, ok)
	assert.Equal(t, d, got)
}

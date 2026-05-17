package daemon

import (
	"testing"
	"time"

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

func TestRouteInboundMultiNoMentionFallsThroughToOwner(t *testing.T) {
	r := NewRouter()
	a := &Shim{ID: "a"}
	b := &Shim{ID: "b"}

	r.Register(a)
	r.Register(b)

	r.RecordOutbound("a", "chat-1")

	got := r.RouteInboundMulti("chat-1", "no mentions here")
	require.Len(t, got, 1)
	assert.Equal(t, "a", got[0].ID)
}

func TestRouteInboundMultiSingleMentionResolves(t *testing.T) {
	r := NewRouter()
	a := &Shim{ID: "a"}
	b := &Shim{ID: "b"}

	r.Register(a) // s1
	r.Register(b) // s2

	got := r.RouteInboundMulti("chat-1", "@s2 please")
	require.Len(t, got, 1)
	assert.Equal(t, "b", got[0].ID)
}

func TestRouteInboundMultiMultipleMentionsResolveEach(t *testing.T) {
	r := NewRouter()
	a := &Shim{ID: "a"}
	b := &Shim{ID: "b"}
	c := &Shim{ID: "c"}

	r.Register(a) // s1
	r.Register(b) // s2
	r.Register(c) // s3

	got := r.RouteInboundMulti("chat-1", "@s1 and @s3 do this")
	require.Len(t, got, 2)
	ids := []string{got[0].ID, got[1].ID}
	assert.ElementsMatch(t, []string{"a", "c"}, ids)
}

func TestRouteInboundMultiAllBroadcasts(t *testing.T) {
	r := NewRouter()
	a := &Shim{ID: "a"}
	b := &Shim{ID: "b"}
	c := &Shim{ID: "c"}

	r.Register(a)
	r.Register(b)
	r.Register(c)

	got := r.RouteInboundMulti("chat-1", "@all status")
	assert.Len(t, got, 3)
}

func TestRouteInboundMultiUnknownMentionFallsThrough(t *testing.T) {
	r := NewRouter()
	a := &Shim{ID: "a"}
	b := &Shim{ID: "b"}

	r.Register(a)
	r.Register(b)
	r.RecordOutbound("a", "chat-1")

	got := r.RouteInboundMulti("chat-1", "@s99 wrong")
	require.Len(t, got, 1, "unknown mention falls through to owner")
	assert.Equal(t, "a", got[0].ID)
}

func TestRouteInboundMultiMixOfKnownAndUnknownReturnsOnlyKnown(t *testing.T) {
	r := NewRouter()
	a := &Shim{ID: "a"}
	b := &Shim{ID: "b"}

	r.Register(a) // s1
	r.Register(b) // s2

	got := r.RouteInboundMulti("chat-1", "@s1 and @s99 mix")
	require.Len(t, got, 1, "known mention wins; unknown is silently dropped")
	assert.Equal(t, "a", got[0].ID)
}

func TestRouteInboundMultiNoShimsReturnsEmpty(t *testing.T) {
	r := NewRouter()
	got := r.RouteInboundMulti("chat-1", "@s1 hi")
	assert.Empty(t, got)
}

func TestRouteInboundMultiMentionDoesNotChangeOwner(t *testing.T) {
	r := NewRouter()
	a := &Shim{ID: "a"}
	b := &Shim{ID: "b"}

	r.Register(a) // s1
	r.Register(b) // s2
	r.RecordOutbound("a", "chat-1")

	_ = r.RouteInboundMulti("chat-1", "@s2 hello")

	owner, ok := r.RouteInbound("chat-1")
	require.True(t, ok)
	assert.Equal(t, "a", owner.ID, "mention dispatch must not rewrite chatOwners")
}

func TestRouterRegisterRecordsConnectedAt(t *testing.T) {
	r := NewRouter()
	before := time.Now()
	r.Register(&Shim{ID: "s1", Workdir: "/tmp/wd", CCSessionID: "cc-1"})
	after := time.Now()

	infos := r.Snapshot()
	require.Len(t, infos, 1)
	assert.Equal(t, "/tmp/wd", infos[0].Workdir)
	assert.Equal(t, "cc-1", infos[0].CCSessionID)
	assert.True(t, !infos[0].ConnectedAt.Before(before) && !infos[0].ConnectedAt.After(after))
	assert.True(t, infos[0].LastOutbound.IsZero())
}

func TestRouterRecordOutboundSetsLastOutbound(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "s1"})

	before := time.Now()
	r.RecordOutbound("s1", "chat-1")
	after := time.Now()

	infos := r.Snapshot()
	require.Len(t, infos, 1)
	assert.True(t, !infos[0].LastOutbound.Before(before) && !infos[0].LastOutbound.After(after))
}

func TestRouterPinOverridesOwner(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "s1"})
	r.Register(&Shim{ID: "s2"})
	r.RecordOutbound("s1", "chat-1") // s1 owns chat-1 via last-writer-wins

	require.NoError(t, r.Pin("chat-1", "s2", time.Minute))

	got, ok := r.RouteInbound("chat-1")
	require.True(t, ok)
	assert.Equal(t, "s2", got.ID)
}

func TestRouterPinUnknownShim(t *testing.T) {
	r := NewRouter()
	err := r.Pin("chat-1", "ghost", time.Minute)
	assert.Error(t, err)
}

func TestRouterPinExpires(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "s1"})
	r.Register(&Shim{ID: "s2"})
	r.RecordOutbound("s1", "chat-1")
	require.NoError(t, r.Pin("chat-1", "s2", -time.Second)) // already expired

	got, ok := r.RouteInbound("chat-1")
	require.True(t, ok)
	assert.Equal(t, "s1", got.ID)
}

func TestRouterOutboundFromOtherShimClearsPin(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "s1"})
	r.Register(&Shim{ID: "s2"})
	require.NoError(t, r.Pin("chat-1", "s2", time.Hour))
	r.RecordOutbound("s1", "chat-1") // different shim writes

	got, ok := r.RouteInbound("chat-1")
	require.True(t, ok)
	assert.Equal(t, "s1", got.ID, "pin should be cleared, last-writer-wins resumes")
}

func TestRouterOutboundFromPinnedShimKeepsPin(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "s1"})
	r.Register(&Shim{ID: "s2"})
	r.RecordOutbound("s1", "chat-1")
	require.NoError(t, r.Pin("chat-1", "s2", time.Hour))
	r.RecordOutbound("s2", "chat-1") // pinned shim writes — no-op for pin

	got, ok := r.RouteInbound("chat-1")
	require.True(t, ok)
	assert.Equal(t, "s2", got.ID)
}

func TestRouterUnpin(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "s1"})
	r.Register(&Shim{ID: "s2"})
	r.RecordOutbound("s1", "chat-1")
	require.NoError(t, r.Pin("chat-1", "s2", time.Hour))
	r.Unpin("chat-1")

	got, ok := r.RouteInbound("chat-1")
	require.True(t, ok)
	assert.Equal(t, "s1", got.ID)
}

func TestRouterResolveShimByPrefix(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "abcdef012345"})
	r.Register(&Shim{ID: "abcd99887766"})
	r.Register(&Shim{ID: "ffffffffffff"})

	s, err := r.ResolveShimByPrefix("abcdef")
	require.NoError(t, err)
	assert.Equal(t, "abcdef012345", s.ID)

	_, err = r.ResolveShimByPrefix("abcd")
	assert.ErrorIs(t, err, ErrAmbiguousShimPrefix)

	_, err = r.ResolveShimByPrefix("zz")
	assert.ErrorIs(t, err, ErrShimNotFound)

	_, err = r.ResolveShimByPrefix("")
	assert.ErrorIs(t, err, ErrShimNotFound)
}

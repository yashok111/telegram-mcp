package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/yakov/telegram-mcp/internal/ipc"
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

	r.RecordOutbound("a", "chat-1", 0)

	got, ok := r.RouteInbound("chat-1")
	require.True(t, ok)
	assert.Equal(t, "a", got.ID)
}

func TestRouterLRATieBreakLex(t *testing.T) {
	r := NewRouter()
	a := &Shim{ID: "a"}
	b := &Shim{ID: "b"}

	r.Register(a)
	r.Register(b) // b is most-recently-connected, but LRA tie-break is lex.

	// Both shims have zero lastOutbound/lastAssigned; explicit LRA tie-break
	// picks the lex-smallest shim ID.
	got, ok := r.RouteInbound("never-seen")
	require.True(t, ok)
	assert.Equal(t, "a", got.ID)
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
	r.RecordOutbound("a", "chat-1", 0)

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

	r.RecordOutbound("a", "chat-1", 0)

	got := r.RouteInboundMulti("chat-1", "no mentions here", 0)
	require.Len(t, got, 1)
	assert.Equal(t, "a", got[0].ID)
}

func TestRouteInboundMultiSingleMentionResolves(t *testing.T) {
	r := NewRouter()
	a := &Shim{ID: "a"}
	b := &Shim{ID: "b"}

	r.Register(a) // s1
	r.Register(b) // s2

	got := r.RouteInboundMulti("chat-1", "@s2 please", 0)
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

	got := r.RouteInboundMulti("chat-1", "@s1 and @s3 do this", 0)
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

	got := r.RouteInboundMulti("chat-1", "@all status", 0)
	assert.Len(t, got, 3)
}

func TestRouteInboundMultiUnknownMentionFallsThrough(t *testing.T) {
	r := NewRouter()
	a := &Shim{ID: "a"}
	b := &Shim{ID: "b"}

	r.Register(a)
	r.Register(b)
	r.RecordOutbound("a", "chat-1", 0)

	got := r.RouteInboundMulti("chat-1", "@s99 wrong", 0)
	require.Len(t, got, 1, "unknown mention falls through to owner")
	assert.Equal(t, "a", got[0].ID)
}

func TestRouteInboundMultiMixOfKnownAndUnknownReturnsOnlyKnown(t *testing.T) {
	r := NewRouter()
	a := &Shim{ID: "a"}
	b := &Shim{ID: "b"}

	r.Register(a) // s1
	r.Register(b) // s2

	got := r.RouteInboundMulti("chat-1", "@s1 and @s99 mix", 0)
	require.Len(t, got, 1, "known mention wins; unknown is silently dropped")
	assert.Equal(t, "a", got[0].ID)
}

func TestRouteInboundMultiNoShimsReturnsEmpty(t *testing.T) {
	r := NewRouter()
	got := r.RouteInboundMulti("chat-1", "@s1 hi", 0)
	assert.Empty(t, got)
}

func TestRouteInboundMultiMentionDoesNotChangeOwner(t *testing.T) {
	r := NewRouter()
	a := &Shim{ID: "a"}
	b := &Shim{ID: "b"}

	r.Register(a) // s1
	r.Register(b) // s2
	r.RecordOutbound("a", "chat-1", 0)

	_ = r.RouteInboundMulti("chat-1", "@s2 hello", 0)

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

	r.RecordOutbound("s1", "chat-1", 0)

	after := time.Now()

	infos := r.Snapshot()
	require.Len(t, infos, 1)
	assert.True(t, !infos[0].LastOutbound.Before(before) && !infos[0].LastOutbound.After(after))
}

func TestRouterPinOverridesOwner(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "s1"})
	r.Register(&Shim{ID: "s2"})
	r.RecordOutbound("s1", "chat-1", 0) // s1 owns chat-1 via last-writer-wins

	require.NoError(t, r.Pin("chat-1", "s2", time.Minute))

	got, ok := r.RouteInbound("chat-1")
	require.True(t, ok)
	assert.Equal(t, "s2", got.ID)
}

func TestRouterPinUnknownShim(t *testing.T) {
	r := NewRouter()
	err := r.Pin("chat-1", "ghost", time.Minute)
	require.ErrorIs(t, err, ErrShimNotFound)
}

func TestRouterPinExpires(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "s1"})
	r.Register(&Shim{ID: "s2"})
	r.RecordOutbound("s1", "chat-1", 0)
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
	r.RecordOutbound("s1", "chat-1", 0) // different shim writes

	got, ok := r.RouteInbound("chat-1")
	require.True(t, ok)
	assert.Equal(t, "s1", got.ID, "pin should be cleared, last-writer-wins resumes")
}

func TestRouterOutboundFromPinnedShimKeepsPin(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "s1"})
	r.Register(&Shim{ID: "s2"})
	r.RecordOutbound("s1", "chat-1", 0)
	require.NoError(t, r.Pin("chat-1", "s2", time.Hour))
	r.RecordOutbound("s2", "chat-1", 0) // pinned shim writes — no-op for pin

	got, ok := r.RouteInbound("chat-1")
	require.True(t, ok)
	assert.Equal(t, "s2", got.ID)
}

func TestRouterUnpin(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "s1"})
	r.Register(&Shim{ID: "s2"})
	r.RecordOutbound("s1", "chat-1", 0)
	require.NoError(t, r.Pin("chat-1", "s2", time.Hour))
	r.Unpin("chat-1")

	got, ok := r.RouteInbound("chat-1")
	require.True(t, ok)
	assert.Equal(t, "s1", got.ID)
}

func TestRouterSnapshotPinnedChatsActive(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "s1"})
	r.Register(&Shim{ID: "s2"})
	require.NoError(t, r.Pin("chatA", "s1", time.Hour))

	infos := r.Snapshot()
	require.Len(t, infos, 2)

	byID := map[string]ShimInfo{}
	for _, info := range infos {
		byID[info.ID] = info
	}

	require.Contains(t, byID, "s1")
	require.Contains(t, byID, "s2")
	assert.Equal(t, []string{"chatA"}, byID["s1"].PinnedChats)
	assert.Empty(t, byID["s2"].PinnedChats)
}

func TestRouterSnapshotPinnedChatsExpiredFiltered(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "s1"})
	r.Register(&Shim{ID: "s2"})
	require.NoError(t, r.Pin("chatA", "s1", -time.Second))

	infos := r.Snapshot()
	require.Len(t, infos, 2)

	for _, info := range infos {
		assert.Empty(t, info.PinnedChats, "expired pin must be filtered from snapshot for shim %s", info.ID)
	}
}

func TestRouterDropClearsPinsHeldByDroppedShim(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "s1"})
	r.Register(&Shim{ID: "s2"})
	require.NoError(t, r.Pin("chatA", "s1", time.Hour))
	require.NoError(t, r.Pin("chatB", "s2", time.Hour))

	r.Drop("s1")

	got, ok := r.RouteInbound("chatA")
	require.True(t, ok, "chatA should still resolve via fallback after s1 dropped")
	assert.Equal(t, "s2", got.ID, "chatA falls back to LRU (s2) since s1 gone and its pin cleared")

	infos := r.Snapshot()
	require.Len(t, infos, 1)
	assert.Equal(t, "s2", infos[0].ID)
	assert.Equal(t, []string{"chatB"}, infos[0].PinnedChats, "s2's pin must be untouched by s1 drop")
}

func TestRouteInboundMultiReplyBeatsMentionsOwnerAndLRU(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "a"}) // s1
	r.Register(&Shim{ID: "b"}) // s2
	r.Register(&Shim{ID: "c"}) // s3 — most recent (LRU head)

	r.RecordOutbound("b", "chat-1", 77) // s2 sent msg 77
	r.RecordOutbound("c", "chat-1", 99) // s3 owns chat-1 via last-writer-wins

	// Reply to s2's msg even with @s1 mention and s3 owning the chat.
	got := r.RouteInboundMulti("chat-1", "@s1 reply text", 77)
	require.Len(t, got, 1)
	assert.Equal(t, "b", got[0].ID, "reply must outrank @mention, owner, and LRU")
}

func TestRouteInboundMultiReplyMissFallsThroughToMention(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "a"}) // s1
	r.Register(&Shim{ID: "b"}) // s2

	r.RecordOutbound("a", "chat-1", 0) // a owns chat-1; no reply entry

	got := r.RouteInboundMulti("chat-1", "@s2 hi", 12345)
	require.Len(t, got, 1)
	assert.Equal(t, "b", got[0].ID, "unknown reply_to falls through to mention")
}

func TestRouteInboundMultiReplyMissFallsThroughToOwner(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "a"})
	r.Register(&Shim{ID: "b"})

	r.RecordOutbound("a", "chat-1", 0)

	got := r.RouteInboundMulti("chat-1", "plain text", 999)
	require.Len(t, got, 1)
	assert.Equal(t, "a", got[0].ID, "unknown reply_to with no mention falls to owner")
}

func TestRouteInboundMultiReplyToShimWhoseConnIsGone(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "a"})
	r.Register(&Shim{ID: "b"})
	r.RecordOutbound("a", "chat-1", 42)
	r.Drop("a")

	got := r.RouteInboundMulti("chat-1", "@b please", 42)
	require.Len(t, got, 1)
	assert.Equal(t, "b", got[0].ID, "reply target gone — fall through to mention @b")
}

func TestRouterRouteInboundByReplyHappyPath(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "s1"})
	r.Register(&Shim{ID: "s2"})

	r.RecordOutbound("s1", "chat-1", 42)

	got, ok := r.RouteInboundByReply("chat-1", 42)
	require.True(t, ok)
	assert.Equal(t, "s1", got.ID, "reply to s1's message_id routes back to s1 regardless of LRU")
}

func TestRouterRouteInboundByReplyZeroMessageIDReturnsFalse(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "s1"})
	r.RecordOutbound("s1", "chat-1", 42)

	_, ok := r.RouteInboundByReply("chat-1", 0)
	assert.False(t, ok, "msg_id=0 means no reply — must not match anything")
}

func TestRouterRouteInboundByReplyUnknownChatReturnsFalse(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "s1"})
	r.RecordOutbound("s1", "chat-1", 42)

	_, ok := r.RouteInboundByReply("other-chat", 42)
	assert.False(t, ok)
}

func TestRouterRouteInboundByReplyUnknownMessageReturnsFalse(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "s1"})
	r.RecordOutbound("s1", "chat-1", 42)

	_, ok := r.RouteInboundByReply("chat-1", 999)
	assert.False(t, ok)
}

func TestRouterRouteInboundByReplyShimGoneReturnsFalse(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "s1"})
	r.Register(&Shim{ID: "s2"})
	r.RecordOutbound("s1", "chat-1", 42)
	r.Drop("s1")

	_, ok := r.RouteInboundByReply("chat-1", 42)
	assert.False(t, ok, "owner gone — reply cannot route")
}

func TestRouterRecordOutboundZeroMessageIDDoesNotIndexReply(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "s1"})
	r.RecordOutbound("s1", "chat-1", 0)

	_, ok := r.RouteInboundByReply("chat-1", 0)
	assert.False(t, ok)

	_, ok = r.RouteInboundByReply("chat-1", 1)
	assert.False(t, ok, "msg_id=0 outbound must not write any reply entry")
}

func TestRouterRouteInboundByReplyEvictsOldestAtCap(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "s1"})

	for i := 1; i <= replyOwnerCapPerChat; i++ {
		r.RecordOutbound("s1", "chat-1", i)
	}

	_, ok := r.RouteInboundByReply("chat-1", 1)
	require.True(t, ok, "msg 1 should still resolve at exactly cap")

	r.RecordOutbound("s1", "chat-1", replyOwnerCapPerChat+1)

	_, ok = r.RouteInboundByReply("chat-1", 1)
	assert.False(t, ok, "oldest (1) must be evicted at cap+1")

	got, ok := r.RouteInboundByReply("chat-1", replyOwnerCapPerChat+1)
	require.True(t, ok)
	assert.Equal(t, "s1", got.ID)
}

func TestRouterRouteInboundByReplyPerChatIsolation(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "s1"})
	r.Register(&Shim{ID: "s2"})

	r.RecordOutbound("s1", "chatA", 42)
	r.RecordOutbound("s2", "chatB", 42)

	got, ok := r.RouteInboundByReply("chatA", 42)
	require.True(t, ok)
	assert.Equal(t, "s1", got.ID)

	got, ok = r.RouteInboundByReply("chatB", 42)
	require.True(t, ok)
	assert.Equal(t, "s2", got.ID, "same msg_id in another chat is a distinct entry")
}

func TestRouterRouteInboundByReplyReassignsOnSameMsgID(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "s1"})
	r.Register(&Shim{ID: "s2"})

	r.RecordOutbound("s1", "chat-1", 42)
	r.RecordOutbound("s2", "chat-1", 42) // unusual, but: same msg_id reassigned

	got, ok := r.RouteInboundByReply("chat-1", 42)
	require.True(t, ok)
	assert.Equal(t, "s2", got.ID)
}

func TestRouterDropCompactsReplyRingFifo(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "s1"})
	r.Register(&Shim{ID: "s2"})

	for i := 1; i <= replyOwnerCapPerChat; i++ {
		r.RecordOutbound("s1", "chat-1", i)
	}

	r.Drop("s1")
	r.RecordOutbound("s2", "chat-1", replyOwnerCapPerChat+1)

	ring, ok := r.replyOwners["chat-1"]
	require.True(t, ok, "ring should survive — s2 has a live entry")
	assert.Len(t, ring.fifo, 1, "dropShim must clear zombie IDs; only s2's single entry should remain")
	assert.Len(t, ring.owners, 1)
}

func TestRouterDropClearsReplyOwners(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "s1"})
	r.Register(&Shim{ID: "s2"})

	r.RecordOutbound("s1", "chatA", 10)
	r.RecordOutbound("s1", "chatA", 11)
	r.RecordOutbound("s2", "chatA", 20)
	r.RecordOutbound("s1", "chatB", 30)

	r.Drop("s1")

	_, ok := r.RouteInboundByReply("chatA", 10)
	assert.False(t, ok, "s1's chatA entry must be gone")

	_, ok = r.RouteInboundByReply("chatA", 11)
	assert.False(t, ok, "s1's chatA entry must be gone")

	got, ok := r.RouteInboundByReply("chatA", 20)
	require.True(t, ok)
	assert.Equal(t, "s2", got.ID, "s2's chatA entry survives s1 drop")

	_, ok = r.RouteInboundByReply("chatB", 30)
	assert.False(t, ok, "s1's chatB entry must be gone")
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
	require.ErrorIs(t, err, ErrAmbiguousShimPrefix)

	_, err = r.ResolveShimByPrefix("zz")
	require.ErrorIs(t, err, ErrShimNotFound)

	_, err = r.ResolveShimByPrefix("")
	require.ErrorIs(t, err, ErrShimNotFound)
}

func TestRouterLRARoundRobinBetweenTwoShims(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "a"}) // s1
	r.Register(&Shim{ID: "b"}) // s2

	// "a" just replied — bumps r.lastOutbound["a"]. Next unaddressed inbound
	// should go to "b" because it's the least-recently-assigned/touched.
	r.RecordOutbound("a", "chatA", 0)

	got, ok := r.RouteInbound("chatB") // fresh chat, no pin, no owner
	require.True(t, ok)
	assert.Equal(t, "b", got.ID, "after a's outbound, fresh inbound goes to least-recently-touched shim b")
}

func TestRouterLRARoundRobinThreeShims(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "a"})
	r.Register(&Shim{ID: "b"})
	r.Register(&Shim{ID: "c"})

	// First inbound: all at zero, lex tie-break picks "a".
	got, ok := r.RouteInbound("chat-1")
	require.True(t, ok)
	assert.Equal(t, "a", got.ID)

	// Second inbound on a different fresh chat: a was just assigned, b and c still zero → b.
	got, ok = r.RouteInbound("chat-2")
	require.True(t, ok)
	assert.Equal(t, "b", got.ID)

	// Third inbound: a and b assigned, c still zero → c.
	got, ok = r.RouteInbound("chat-3")
	require.True(t, ok)
	assert.Equal(t, "c", got.ID)

	// Fourth inbound: all assigned; oldest is a again → a.
	got, ok = r.RouteInbound("chat-4")
	require.True(t, ok)
	assert.Equal(t, "a", got.ID)
}

func TestRouterLRAMentionStillBeatsRoundRobin(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "a"}) // s1
	r.Register(&Shim{ID: "b"}) // s2
	r.Register(&Shim{ID: "c"}) // s3

	// LRA alone (no mention, no pin, no owner) would prefer a by lex.
	// A mention of @s3 must still win.
	got := r.RouteInboundMulti("chat-1", "@s3 do this", 0)
	require.Len(t, got, 1)
	assert.Equal(t, "c", got[0].ID)
}

func TestRouterLRAPinStillBeatsRoundRobin(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "a"})
	r.Register(&Shim{ID: "b"})

	// Pin chat to b. LRA alone would pick a (lex).
	require.NoError(t, r.Pin("chat-1", "b", time.Hour))

	got, ok := r.RouteInbound("chat-1")
	require.True(t, ok)
	assert.Equal(t, "b", got.ID)
}

func TestRouterLRASingleShimFallsBackToIt(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "only"})

	// With 1 shim, LRA branch is skipped (degenerate); lru[0] fallback returns it.
	got, ok := r.RouteInbound("chat-1")
	require.True(t, ok)
	assert.Equal(t, "only", got.ID)
}

func TestRouterLRAOwnerStillBeatsRoundRobin(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "a"})
	r.Register(&Shim{ID: "b"})

	r.RecordOutbound("a", "chat-1", 0) // a owns chat-1

	// LRA alone would prefer b (a was just touched). Owner check happens first.
	got, ok := r.RouteInbound("chat-1")
	require.True(t, ok)
	assert.Equal(t, "a", got.ID, "owner outranks LRA in precedence")
}

func TestRouterLRATieBreakDeterministicAtZero(t *testing.T) {
	// Two shims, both never outbound, never assigned: max(0,0)==0 for both.
	// Tie-break must be lex by shim_id, deterministic across runs.
	r := NewRouter()
	r.Register(&Shim{ID: "zzz"})
	r.Register(&Shim{ID: "aaa"})

	got, ok := r.RouteInbound("chat-1")
	require.True(t, ok)
	assert.Equal(t, "aaa", got.ID, "lex tie-break: aaa < zzz")
}

func TestRouterLRADropUpdatesLastAssigned(t *testing.T) {
	// Drop must purge lastAssigned so re-registering the same ID later doesn't
	// inherit a stale "recently touched" timestamp.
	r := NewRouter()
	r.Register(&Shim{ID: "a"})
	r.Register(&Shim{ID: "b"})

	_, _ = r.RouteInbound("chat-1") // assigns a
	r.Drop("a")

	r.Register(&Shim{ID: "a"}) // re-register

	// Now both have zero lastAssigned again; lex picks a.
	got, ok := r.RouteInbound("chat-2")
	require.True(t, ok)
	assert.Equal(t, "a", got.ID, "Drop must clear lastAssigned['a']")
}

func TestRouterSetLabelUpdatesShimAndSnapshot(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "s1"})

	info, err := r.SetLabel("s1", "main-bot")
	require.NoError(t, err)
	assert.Equal(t, "main-bot", info.Label)

	snap := r.Snapshot()
	require.Len(t, snap, 1)
	assert.Equal(t, "main-bot", snap[0].Label)
}

func TestRouterSetLabelEmptyClears(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "s1", Label: "old"})

	info, err := r.SetLabel("s1", "")
	require.NoError(t, err)
	assert.Empty(t, info.Label)
}

func TestRouterSetLabelUnknownShim(t *testing.T) {
	r := NewRouter()

	_, err := r.SetLabel("ghost", "x")
	require.ErrorIs(t, err, ErrShimNotFound)
}

func TestRouterSetLabelPushesNotify(t *testing.T) {
	r := NewRouter()

	var (
		gotMethod string
		gotParams any
	)

	notify := func(method string, params any) error {
		gotMethod = method
		gotParams = params

		return nil
	}

	r.Register(&Shim{ID: "s1", Notify: notify, ConnectedAt: time.Now()})

	_, err := r.SetLabel("s1", "x")
	require.NoError(t, err)
	assert.Equal(t, ipc.NotifyLabelChanged, gotMethod)

	m, ok := gotParams.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "x", m["label"])
}

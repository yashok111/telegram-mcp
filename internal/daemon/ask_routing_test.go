package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRouterAskRegisterRoutes(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "a"})

	require.NoError(t, r.RegisterAsk("qidaa", "a"))

	got, ok := r.RouteAndResolveAsk("qidaa")
	require.True(t, ok)
	assert.Equal(t, "a", got.ID)
}

func TestRouterAskCollisionReturnsError(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "a"})
	r.Register(&Shim{ID: "b"})

	require.NoError(t, r.RegisterAsk("dupeq", "a"))
	err := r.RegisterAsk("dupeq", "b")
	assert.ErrorIs(t, err, ErrAskIDInUse)
}

func TestRouterAskRegisterUnknownShim(t *testing.T) {
	r := NewRouter()
	err := r.RegisterAsk("qidxx", "ghost")
	assert.ErrorIs(t, err, ErrShimNotFound)
}

func TestRouterAskFirstTapWins(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "a"})

	require.NoError(t, r.RegisterAsk("qidaa", "a"))

	got, ok := r.RouteAndResolveAsk("qidaa")
	require.True(t, ok)
	assert.Equal(t, "a", got.ID)

	// Second tap finds nothing — the qid was atomically removed.
	_, ok = r.RouteAndResolveAsk("qidaa")
	assert.False(t, ok)
}

func TestRouterDropShimReleasesAsks(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "a"})

	require.NoError(t, r.RegisterAsk("qidaa", "a"))
	r.Drop("a")

	_, ok := r.RouteAndResolveAsk("qidaa")
	assert.False(t, ok)
}

func TestRouterDropAskIfOwnerForgetsEntry(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "a"})

	require.NoError(t, r.RegisterAsk("qidaa", "a"))

	assert.True(t, r.DropAskIfOwner("qidaa", "a"), "owner drop removes the entry")
	assert.False(t, r.DropAskIfOwner("qidaa", "a"), "second drop finds nothing")

	// A late tap after the drop finds no owner — no leak, no bogus resolve.
	_, ok := r.RouteAndResolveAsk("qidaa")
	assert.False(t, ok)
}

func TestRouterDropAskIfOwnerRejectsForeignShim(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "a"})
	r.Register(&Shim{ID: "b"})

	require.NoError(t, r.RegisterAsk("qidaa", "a"))

	assert.False(t, r.DropAskIfOwner("qidaa", "b"), "a foreign shim must not drop another's ask")

	// Owner's ask survives the foreign cancel attempt.
	got, ok := r.RouteAndResolveAsk("qidaa")
	require.True(t, ok)
	assert.Equal(t, "a", got.ID)
}

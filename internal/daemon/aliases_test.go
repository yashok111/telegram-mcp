package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRouterAllocAliasLowestFree(t *testing.T) {
	r := NewRouter()
	a := &Shim{ID: "a"}
	b := &Shim{ID: "b"}
	c := &Shim{ID: "c"}

	r.Register(a)
	r.Register(b)
	r.Register(c)

	assert.Equal(t, "s1", a.Alias)
	assert.Equal(t, "s2", b.Alias)
	assert.Equal(t, "s3", c.Alias)
}

func TestRouterAliasReleasedOnDrop(t *testing.T) {
	r := NewRouter()
	a := &Shim{ID: "a"}
	b := &Shim{ID: "b"}
	c := &Shim{ID: "c"}

	r.Register(a)
	r.Register(b)
	r.Register(c)
	r.Drop("b")

	d := &Shim{ID: "d"}
	r.Register(d)

	assert.Equal(t, "s2", d.Alias, "freed s2 must be reused before s4")
}

func TestRouterResolveAlias(t *testing.T) {
	r := NewRouter()
	a := &Shim{ID: "a"}
	r.Register(a)

	got, ok := r.ResolveAlias(a.Alias)
	require.True(t, ok)
	assert.Equal(t, "a", got.ID)

	_, ok = r.ResolveAlias("s99")
	assert.False(t, ok)
}

func TestRouter_AliasForShim(t *testing.T) {
	r := NewRouter()
	a := &Shim{ID: "shim-a", Notify: func(string, any) error { return nil }}
	b := &Shim{ID: "shim-b", Notify: func(string, any) error { return nil }}

	r.Register(a)
	r.Register(b)

	assert.Equal(t, "s1", r.AliasForShim("shim-a"))
	assert.Equal(t, "s2", r.AliasForShim("shim-b"))
	assert.Empty(t, r.AliasForShim("unknown"))

	r.Drop("shim-a")
	assert.Empty(t, r.AliasForShim("shim-a"), "alias released on Drop")
}

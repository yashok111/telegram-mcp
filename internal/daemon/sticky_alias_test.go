package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/access"
)

// Tracer: an offline project's alias number stays reserved, so a new project
// can't steal it and the original reattaches to the same @sN. This fails under
// the legacy lowest-free-integer scheme (the gap left by a disconnect is reused).
func TestRouter_StickyAlias_offlineProjectKeepsItsNumber(t *testing.T) {
	r := NewRouter()
	r.SetStickyAliasStore(access.NewStore(t.TempDir(), false))

	foo := &Shim{ID: "foo-1", Workdir: "/projects/foo"}
	r.Register(foo)
	fooAlias := foo.Alias
	require.NotEmpty(t, fooAlias)

	r.Drop("foo-1") // foo goes offline; its alias number must stay reserved

	baz := &Shim{ID: "baz-1", Workdir: "/projects/baz"}
	r.Register(baz)
	assert.NotEqual(t, fooAlias, baz.Alias,
		"a new project must not steal an offline project's reserved alias")

	foo2 := &Shim{ID: "foo-2", Workdir: "/projects/foo"}
	r.Register(foo2)
	assert.Equal(t, fooAlias, foo2.Alias,
		"offline project reattaches to its original sticky alias")
}

// A binding minted by one daemon is reloaded by the next (the access.json
// round-trip), so a restart does not reshuffle aliases — the live confusion the
// user hit after the cleanup restart.
func TestRouter_StickyAlias_survivesRestart(t *testing.T) {
	store := access.NewStore(t.TempDir(), false)

	r1 := NewRouter()
	r1.SetStickyAliasStore(store)

	foo := &Shim{ID: "foo-1", Workdir: "/projects/foo"}
	r1.Register(foo)
	want := foo.Alias

	// Simulate a daemon restart: fresh Router, same on-disk store.
	r2 := NewRouter()
	r2.SetStickyAliasStore(store)

	foo2 := &Shim{ID: "foo-2", Workdir: "/projects/foo"}
	r2.Register(foo2)

	assert.Equal(t, want, foo2.Alias, "alias binding persisted to access.json and reloaded on restart")
}

// Two live sessions in the same project can't share one alias — the second gets
// a transient fresh alias without disturbing the project's sticky binding.
func TestRouter_StickyAlias_concurrentSameProjectGetDistinct(t *testing.T) {
	r := NewRouter()
	r.SetStickyAliasStore(access.NewStore(t.TempDir(), false))

	a := &Shim{ID: "a", Workdir: "/projects/foo"}
	b := &Shim{ID: "b", Workdir: "/projects/foo"}

	r.Register(a)
	r.Register(b)

	assert.NotEqual(t, a.Alias, b.Alias, "two live sessions in one project must get distinct aliases")
}

// The admin-agent keeps its reserved AdminAlias regardless of sticky bindings.
func TestRouter_StickyAlias_adminUnaffected(t *testing.T) {
	r := NewRouter()
	r.SetStickyAliasStore(access.NewStore(t.TempDir(), false))

	adm := &Shim{ID: "adm", Role: "admin", Workdir: "/home/yakov"}
	r.Register(adm)

	assert.Equal(t, AdminAlias, adm.Alias, "admin role keeps its reserved alias, never a sticky sN")
}

// Without a sticky store wired (the test/default path) the legacy lowest-free
// allocation is preserved: a freed alias number is recycled by the next shim.
func TestRouter_StickyAlias_disabledByDefault_recyclesGap(t *testing.T) {
	r := NewRouter() // no SetStickyAliasStore → legacy allocation

	a := &Shim{ID: "a", Workdir: "/projects/foo"}
	r.Register(a)
	require.Equal(t, "s1", a.Alias)

	r.Drop("a")

	b := &Shim{ID: "b", Workdir: "/projects/bar"}
	r.Register(b)
	assert.Equal(t, "s1", b.Alias, "without sticky, the freed lowest alias is recycled (legacy behavior intact)")
}

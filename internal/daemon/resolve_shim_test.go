package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveShimByAlias(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "aaa111bbb222", Notify: func(string, any) error { return nil }})

	s, err := r.ResolveShim("s1")
	require.NoError(t, err)
	assert.Equal(t, "aaa111bbb222", s.ID)
}

func TestResolveShimByPrefixUnique(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "aaa111bbb222", Notify: func(string, any) error { return nil }})
	r.Register(&Shim{ID: "ccc333ddd444", Notify: func(string, any) error { return nil }})

	s, err := r.ResolveShim("ccc333")
	require.NoError(t, err)
	assert.Equal(t, "ccc333ddd444", s.ID)
}

func TestResolveShimAmbiguousPrefix(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "abc111", Notify: func(string, any) error { return nil }})
	r.Register(&Shim{ID: "abc222", Notify: func(string, any) error { return nil }})

	_, err := r.ResolveShim("abc")
	assert.ErrorIs(t, err, ErrAmbiguousShimPrefix)
}

func TestResolveShimNotFound(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "abc111", Notify: func(string, any) error { return nil }})

	_, err := r.ResolveShim("zzz")
	assert.ErrorIs(t, err, ErrShimNotFound)
}

func TestResolveShimEmptyTarget(t *testing.T) {
	r := NewRouter()
	_, err := r.ResolveShim("")
	assert.ErrorIs(t, err, ErrShimNotFound)
}

func TestResolveShimAdminAlias(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "9a9a9a9a", Role: "admin", Notify: func(string, any) error { return nil }})

	s, err := r.ResolveShim(AdminAlias)
	require.NoError(t, err)
	assert.Equal(t, "9a9a9a9a", s.ID)
	assert.Equal(t, "admin", s.Role)
}

// Alias wins over a shim-id that happens to share the alias's text as a prefix.
func TestResolveShimAliasWinsOverPrefix(t *testing.T) {
	r := NewRouter()
	// First shim gets alias s1. Second shim's id literally starts with "s1".
	r.Register(&Shim{ID: "aaa111", Notify: func(string, any) error { return nil }})
	r.Register(&Shim{ID: "s1ffff", Notify: func(string, any) error { return nil }})

	s, err := r.ResolveShim("s1")
	require.NoError(t, err)
	assert.Equal(t, "aaa111", s.ID, "alias s1 must win over the shim_id starting with s1")
}

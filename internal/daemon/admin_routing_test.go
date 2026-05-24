package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newAdminShim(id string) *Shim {
	return &Shim{ID: id, Role: "admin", Notify: func(string, any) error { return nil }}
}

func newUserShim(id string) *Shim {
	return &Shim{ID: id, Notify: func(string, any) error { return nil }}
}

func TestRegisterRoleAdminBindsAdminAlias(t *testing.T) {
	r := NewRouter()

	r.Register(newAdminShim("admin-shim"))

	s, ok := r.ResolveAlias(AdminAlias)
	require.True(t, ok)
	assert.Equal(t, "admin-shim", s.ID)
	assert.Equal(t, AdminAlias, s.Alias)
}

func TestRegisterUserNeverGetsAdminAlias(t *testing.T) {
	r := NewRouter()

	for i := range 5 {
		r.Register(newUserShim(string(rune('a' + i))))
	}

	_, ok := r.ResolveAlias(AdminAlias)
	assert.False(t, ok, "user shims must never bind AdminAlias")
}

func TestRegisterAdminEvictsPrior(t *testing.T) {
	r := NewRouter()

	r.Register(newAdminShim("admin-1"))
	r.Register(newAdminShim("admin-2"))

	s, ok := r.ResolveAlias(AdminAlias)
	require.True(t, ok)
	assert.Equal(t, "admin-2", s.ID, "last hello with role=admin wins")

	assert.Empty(t, r.AliasForShim("admin-1"),
		"prior admin shim must lose alias on takeover")
}

func TestDMFallbackRoutesToAdmin(t *testing.T) {
	r := NewRouter()

	r.Register(newAdminShim("admin-shim"))
	r.Register(newUserShim("user-shim"))

	s, ok := r.RouteInbound("330621952")
	require.True(t, ok)
	assert.Equal(t, "admin-shim", s.ID, "DM with no pin/owner must reach admin, not LRU")
}

func TestDMFallbackSkippedForGroupChat(t *testing.T) {
	r := NewRouter()

	r.Register(newAdminShim("admin-shim"))
	r.Register(newUserShim("user-shim"))

	s, ok := r.RouteInbound("-1001234567")
	require.True(t, ok)
	assert.NotEqual(t, "admin-shim", s.ID, "groups must not auto-route to admin")
}

func TestGroupWithOnlyAdminReturnsNoRoute(t *testing.T) {
	r := NewRouter()

	r.Register(newAdminShim("admin-shim"))

	_, ok := r.RouteInbound("-1001234567")
	assert.False(t, ok, "groups with only admin connected must return no route — admin only on @admin mention")
}

func TestConnectedCountExcludesAdmin(t *testing.T) {
	r := NewRouter()

	r.Register(newAdminShim("admin-shim"))
	assert.Equal(t, 0, r.ConnectedCount(), "admin alone must not keep idle-exit timer paused")

	r.Register(newUserShim("user-shim"))
	assert.Equal(t, 1, r.ConnectedCount(), "single user shim → count is 1")
}

func TestRegisterAdminFullyDropsPrior(t *testing.T) {
	r := NewRouter()

	r.Register(newAdminShim("admin-1"))

	r.RecordOutbound("admin-1", "330621952", 42)

	r.Register(newAdminShim("admin-2"))

	owner, ok := r.OwnerOfMessage("330621952", 42)
	assert.False(t, ok, "prior admin's reply ownership must be dropped on takeover")
	assert.Empty(t, owner)
}

func TestDMFallbackSkippedWhenAdminAbsent(t *testing.T) {
	r := NewRouter()

	r.Register(newUserShim("user-shim"))

	s, ok := r.RouteInbound("330621952")
	require.True(t, ok)
	assert.Equal(t, "user-shim", s.ID, "no admin → fall to LRU")
}

func TestDMFallbackSkippedWhenOwnerPresent(t *testing.T) {
	r := NewRouter()

	r.Register(newAdminShim("admin-shim"))
	r.Register(newUserShim("user-shim"))

	r.RecordOutbound("user-shim", "330621952", 0)

	s, ok := r.RouteInbound("330621952")
	require.True(t, ok)
	assert.Equal(t, "user-shim", s.ID, "chat owner outranks DM-admin fallback")
}

func TestDMFallbackSkippedWhenPinned(t *testing.T) {
	r := NewRouter()

	r.Register(newAdminShim("admin-shim"))
	r.Register(newUserShim("user-shim"))

	require.NoError(t, r.Pin("330621952", "user-shim", time.Hour))

	s, ok := r.RouteInbound("330621952")
	require.True(t, ok)
	assert.Equal(t, "user-shim", s.ID, "pin outranks DM-admin fallback")
}

func TestIsDMChatID(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"330621952", true},
		{"-1003914957143", false},
		{"-100200", false},
		{" 42 ", true},
		{"", false},
		{"abc", false},
		{"0", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, isDMChatID(tc.in))
		})
	}
}

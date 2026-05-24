package daemon

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/ipc"
)

func TestAdminNotify_NoAdmin_ReturnsFalse(t *testing.T) {
	r := NewRouter()

	r.Register(newUserShim("user-1"))

	ok := r.AdminNotify(ipc.NotifyAdminEvent, map[string]any{"type": "shim_disconnected"})
	assert.False(t, ok, "AdminNotify must return false when no admin shim is connected")
}

func TestAdminNotify_WithAdmin_InvokesNotify(t *testing.T) {
	r := NewRouter()

	var mu sync.Mutex

	var calls []string

	adminShim := &Shim{
		ID:   "admin-shim",
		Role: "admin",
		Notify: func(method string, _ any) error {
			mu.Lock()

			calls = append(calls, method)

			mu.Unlock()

			return nil
		},
	}

	r.Register(adminShim)

	ok := r.AdminNotify(ipc.NotifyAdminEvent, map[string]any{"type": "bg_failed"})
	require.True(t, ok, "AdminNotify must return true when admin shim is connected and Notify succeeds")

	mu.Lock()
	defer mu.Unlock()

	require.Len(t, calls, 1)
	assert.Equal(t, ipc.NotifyAdminEvent, calls[0])
}

func TestAdminNotify_AdminDropped_ReturnsFalse(t *testing.T) {
	r := NewRouter()

	admin := newAdminShim("admin-1")
	r.Register(admin)
	r.Drop("admin-1")

	ok := r.AdminNotify(ipc.NotifyAdminEvent, nil)
	assert.False(t, ok, "AdminNotify must return false after admin shim is dropped")
}

func TestAdminNotify_SitrepMethod_Routed(t *testing.T) {
	r := NewRouter()

	var mu sync.Mutex

	var methods []string

	adminShim := &Shim{
		ID:   "admin-shim",
		Role: "admin",
		Notify: func(method string, _ any) error {
			mu.Lock()

			methods = append(methods, method)

			mu.Unlock()

			return nil
		},
	}

	r.Register(adminShim)

	ok := r.AdminNotify(ipc.NotifyAdminSitrep, nil)
	require.True(t, ok)

	mu.Lock()
	defer mu.Unlock()

	require.Len(t, methods, 1)
	assert.Equal(t, ipc.NotifyAdminSitrep, methods[0])
}

func TestAdminNotify_NotifyError_ReturnsFalse(t *testing.T) {
	r := NewRouter()

	adminShim := &Shim{
		ID:   "admin-shim",
		Role: "admin",
		Notify: func(_ string, _ any) error {
			return assert.AnError
		},
	}

	r.Register(adminShim)

	ok := r.AdminNotify(ipc.NotifyAdminEvent, nil)
	assert.False(t, ok, "AdminNotify must return false when Notify returns an error")
}

// Package daemon hosts the long-running process that owns the Telegram poller
// and routes between it and N stdio shims (one per Claude Code session).
package daemon

import (
	"errors"
	"sync"
)

// ErrPermissionIDInUse is returned when a shim tries to register a permission
// request_id that another (still-connected) shim already holds.
var ErrPermissionIDInUse = errors.New("permission request_id already in use")

// Shim is the daemon-side handle for a connected shim. ID is stable for the
// connection's lifetime; the Notify function pushes daemon→shim notifications.
type Shim struct {
	ID     string
	Label  string
	Notify func(method string, params any) error // bound to the underlying ipc.Conn
}

// PermDetails caches the perm request fields embedded in
// bot.broadcastPermissionRequest so callback "See more" can render without
// an additional daemon→shim RPC.
type PermDetails struct {
	ToolName     string
	Description  string
	InputPreview string
}

// Router is mutex-guarded. All Route* methods are read-mostly; Register/Drop/Record* are writes.
type Router struct {
	mu          sync.RWMutex
	shims       map[string]*Shim // by ID
	lru         []string         // most-recent-first
	chatOwners  map[string]string
	permOwners  map[string]string
	permDetails map[string]PermDetails
}

func NewRouter() *Router {
	return &Router{
		shims:       map[string]*Shim{},
		chatOwners:  map[string]string{},
		permOwners:  map[string]string{},
		permDetails: map[string]PermDetails{},
	}
}

func (r *Router) Register(s *Shim) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.shims[s.ID] = s
	r.lru = prepend(r.lru, s.ID)
}

func (r *Router) Drop(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.shims, id)

	r.lru = removeStr(r.lru, id)

	for chat, owner := range r.chatOwners {
		if owner == id {
			delete(r.chatOwners, chat)
		}
	}

	for pid, owner := range r.permOwners {
		if owner == id {
			delete(r.permOwners, pid)
			delete(r.permDetails, pid)
		}
	}
}

func (r *Router) RecordOutbound(shimID, chatID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.shims[shimID]; !ok {
		return
	}

	r.chatOwners[chatID] = shimID
}

func (r *Router) RouteInbound(chatID string) (*Shim, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if owner, ok := r.chatOwners[chatID]; ok {
		if s, ok := r.shims[owner]; ok {
			return s, true
		}
	}

	if len(r.lru) == 0 {
		return nil, false
	}

	s, ok := r.shims[r.lru[0]]

	return s, ok
}

func (r *Router) RegisterPermission(reqID, shimID string, d PermDetails) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.permOwners[reqID]; exists {
		return ErrPermissionIDInUse
	}

	if _, ok := r.shims[shimID]; !ok {
		return errors.New("unknown shim")
	}

	r.permOwners[reqID] = shimID
	r.permDetails[reqID] = d

	return nil
}

func (r *Router) RoutePermission(reqID string) (*Shim, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	owner, ok := r.permOwners[reqID]
	if !ok {
		return nil, false
	}

	s, ok := r.shims[owner]

	return s, ok
}

func (r *Router) ResolvePermission(reqID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.permOwners, reqID)
	delete(r.permDetails, reqID)
}

func (r *Router) LookupPermissionDetails(reqID string) (PermDetails, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	d, ok := r.permDetails[reqID]

	return d, ok
}

// ConnectedCount is used by the idle-exit timer.
func (r *Router) ConnectedCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return len(r.shims)
}

func prepend(xs []string, x string) []string {
	xs = removeStr(xs, x)
	return append([]string{x}, xs...)
}

func removeStr(xs []string, x string) []string {
	out := xs[:0]
	for _, v := range xs {
		if v != x {
			out = append(out, v)
		}
	}

	return out
}

// Package daemon hosts the long-running process that owns the Telegram poller
// and routes between it and N stdio shims (one per Claude Code session).
package daemon

import (
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// ErrPermissionIDInUse is returned when a shim tries to register a permission
// request_id that another (still-connected) shim already holds.
var ErrPermissionIDInUse = errors.New("permission request_id already in use")

// Shim is the daemon-side handle for a connected shim. ID is stable for the
// connection's lifetime; the Notify function pushes daemon→shim notifications.
type Shim struct {
	ID          string
	Alias       string
	Label       string
	Workdir     string
	CCSessionID string
	ConnectedAt time.Time
	Notify      func(method string, params any) error // bound to the underlying ipc.Conn
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

	aliases   map[string]string // alias → shim_id
	shimAlias map[string]string // shim_id → alias  (for O(1) release at Drop)

	lastOutbound map[string]time.Time // shim_id → time
	pins         map[string]pin       // chat_id → pin
}

type pin struct {
	shimID    string
	expiresAt time.Time
}

func NewRouter() *Router {
	return &Router{
		shims:        map[string]*Shim{},
		chatOwners:   map[string]string{},
		permOwners:   map[string]string{},
		permDetails:  map[string]PermDetails{},
		aliases:      map[string]string{},
		shimAlias:    map[string]string{},
		lastOutbound: map[string]time.Time{},
		pins:         map[string]pin{},
	}
}

func (r *Router) Register(s *Shim) {
	r.mu.Lock()
	defer r.mu.Unlock()

	alias := r.allocAlias()
	s.Alias = alias
	r.aliases[alias] = s.ID
	r.shimAlias[s.ID] = alias

	if s.ConnectedAt.IsZero() {
		s.ConnectedAt = time.Now()
	}

	r.shims[s.ID] = s
	r.lru = prepend(r.lru, s.ID)
}

func (r *Router) Drop(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if alias, ok := r.shimAlias[id]; ok {
		delete(r.aliases, alias)
		delete(r.shimAlias, id)
	}

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

	delete(r.lastOutbound, id)

	for chat, p := range r.pins {
		if p.shimID == id {
			delete(r.pins, chat)
		}
	}
}

func (r *Router) RecordOutbound(shimID, chatID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.shims[shimID]; !ok {
		slog.Warn("RecordOutbound dropped: unknown shim", "shim_id", shimID, "chat_id", chatID)
		return
	}

	prev := r.chatOwners[chatID]
	r.chatOwners[chatID] = shimID
	r.lastOutbound[shimID] = time.Now()

	slog.Info("RecordOutbound", "shim_id", shimID, "chat_id", chatID, "prev_owner", prev)
}

// Snapshot returns a by-value list of connected shims, newest-first by ConnectedAt.
// Safe to hand to bot code without leaking router-internal state.
func (r *Router) Snapshot() []ShimInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]ShimInfo, 0, len(r.shims))
	pinsByShim := map[string][]string{}
	now := time.Now()
	for chat, p := range r.pins {
		if now.After(p.expiresAt) {
			continue
		}
		pinsByShim[p.shimID] = append(pinsByShim[p.shimID], chat)
	}

	for _, s := range r.shims {
		out = append(out, ShimInfo{
			ID:           s.ID,
			Alias:        s.Alias,
			Label:        s.Label,
			Workdir:      s.Workdir,
			CCSessionID:  s.CCSessionID,
			ConnectedAt:  s.ConnectedAt,
			LastOutbound: r.lastOutbound[s.ID],
			PinnedChats:  pinsByShim[s.ID],
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ConnectedAt.After(out[j].ConnectedAt)
	})
	return out
}

func (r *Router) RouteInbound(chatID string) (*Shim, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.routeInboundLocked(chatID)
}

// routeInboundLocked is the owner→LRU resolver. Caller holds r.mu.
func (r *Router) routeInboundLocked(chatID string) (*Shim, bool) {
	if owner, ok := r.chatOwners[chatID]; ok {
		if s, ok := r.shims[owner]; ok {
			slog.Info("RouteInbound owner", "chat_id", chatID, "shim_id", s.ID)
			return s, true
		}

		slog.Warn("RouteInbound owner gone", "chat_id", chatID, "stale_owner", owner)
	}

	if len(r.lru) == 0 {
		slog.Warn("RouteInbound no shims", "chat_id", chatID)
		return nil, false
	}

	s, ok := r.shims[r.lru[0]]
	if ok {
		slog.Info("RouteInbound LRU fallback", "chat_id", chatID, "shim_id", s.ID, "lru", r.lru)
	}

	return s, ok
}

// RouteInboundMulti resolves an inbound message to a set of target shims using
// the precedence chain: mentions (incl. @all broadcast) → chat owner → LRU.
// A mention dispatch never rewrites chatOwners — it's a one-shot address.
// Returns nil if no shims are connected.
func (r *Router) RouteInboundMulti(chatID, content string) []*Shim {
	mentions := parseMentions(content)

	r.mu.RLock()
	defer r.mu.RUnlock()

	if len(mentions) > 0 {
		targets := r.resolveMentionsLocked(chatID, mentions)
		if len(targets) > 0 {
			return targets
		}
		// All mentions were unknown; fall through to owner/LRU.
	}

	if single, ok := r.routeInboundLocked(chatID); ok {
		return []*Shim{single}
	}

	return nil
}

// resolveMentionsLocked translates alias tokens into Shim pointers. @all expands
// to every connected shim. Unknown aliases are silently dropped. Caller holds r.mu.
func (r *Router) resolveMentionsLocked(chatID string, mentions []string) []*Shim {
	seen := make(map[string]struct{}, len(mentions))
	out := make([]*Shim, 0, len(mentions))

	for _, m := range mentions {
		if m == "all" {
			for _, id := range r.lru {
				if _, dup := seen[id]; dup {
					continue
				}

				if s, ok := r.shims[id]; ok {
					seen[id] = struct{}{}

					out = append(out, s)
				}
			}

			continue
		}

		id, ok := r.aliases[m]
		if !ok {
			slog.Warn("mention resolved to unknown alias", "alias", m, "chat_id", chatID)
			continue
		}

		if _, dup := seen[id]; dup {
			continue
		}

		s, ok := r.shims[id]
		if !ok {
			continue
		}

		seen[id] = struct{}{}

		out = append(out, s)
	}

	return out
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

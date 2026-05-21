// Package daemon hosts the long-running process that owns the Telegram poller
// and routes between it and N stdio shims (one per Claude Code session).
package daemon

import (
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yakov/telegram-mcp/internal/ipc"
)

// ErrPermissionIDInUse is returned when a shim tries to register a permission
// request_id that another (still-connected) shim already holds.
var ErrPermissionIDInUse = errors.New("permission request_id already in use")

var (
	ErrShimNotFound        = errors.New("shim not found")
	ErrAmbiguousShimPrefix = errors.New("shim prefix is ambiguous")
)

// Shim is the daemon-side handle for a connected shim. ID is stable for the
// connection's lifetime; the Notify function pushes daemon→shim notifications.
type Shim struct {
	ID          string
	Alias       string
	Label       string
	Workdir     string
	CCSessionID string
	SpawnID     string
	ConnectedAt time.Time
	Notify      func(method string, params any) error // bound to the underlying ipc.Conn
}

// PermDetails caches the perm request fields carried by
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

	// labelIndex maps lowercase Shim.Label → shim_ids that currently carry it.
	// Lets matchByLabelLocked run in O(1) instead of scanning every shim.
	labelIndex map[string][]string

	lastOutbound map[string]time.Time  // shim_id → time
	lastAssigned map[string]time.Time  // shim_id → most recent inbound-route resolution
	pins         map[string]pin        // chat_id → pin
	replyOwners  map[string]*replyRing // chat_id → per-chat bounded message_id ownership
}

type pin struct {
	shimID    string
	expiresAt time.Time
}

// replyOwnerCapPerChat bounds the per-chat (message_id → shim_id) ring so
// replies to long-gone outbounds don't grow the daemon's memory unboundedly.
const replyOwnerCapPerChat = 1000

// replyRing is a bounded FIFO map: oldest message_id is evicted once cap is
// reached. Not safe for concurrent use — caller (Router) holds r.mu.
type replyRing struct {
	cap    int
	fifo   []int          // FIFO order, eldest at index 0
	owners map[int]string // message_id → shim_id
}

func newReplyRing(capacity int) *replyRing {
	return &replyRing{cap: capacity, owners: map[int]string{}}
}

func (rr *replyRing) add(msgID int, shimID string) {
	if _, exists := rr.owners[msgID]; exists {
		rr.owners[msgID] = shimID
		return
	}

	for len(rr.fifo) >= rr.cap {
		evicted := rr.fifo[0]
		rr.fifo = rr.fifo[1:]
		delete(rr.owners, evicted)
	}

	rr.fifo = append(rr.fifo, msgID)
	rr.owners[msgID] = shimID

	if cap(rr.fifo) > 4*rr.cap {
		compact := make([]int, len(rr.fifo))
		copy(compact, rr.fifo)
		rr.fifo = compact
	}
}

func (rr *replyRing) lookup(msgID int) (string, bool) {
	sid, ok := rr.owners[msgID]
	return sid, ok
}

func (rr *replyRing) dropShim(shimID string) {
	keep := rr.fifo[:0]

	for _, mid := range rr.fifo {
		sid, ok := rr.owners[mid]
		if !ok {
			continue
		}

		if sid == shimID {
			delete(rr.owners, mid)
			continue
		}

		keep = append(keep, mid)
	}

	rr.fifo = keep
}

func NewRouter() *Router {
	return &Router{
		shims:        map[string]*Shim{},
		chatOwners:   map[string]string{},
		permOwners:   map[string]string{},
		permDetails:  map[string]PermDetails{},
		aliases:      map[string]string{},
		shimAlias:    map[string]string{},
		labelIndex:   map[string][]string{},
		lastOutbound: map[string]time.Time{},
		lastAssigned: map[string]time.Time{},
		pins:         map[string]pin{},
		replyOwners:  map[string]*replyRing{},
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
	r.indexLabelLocked(s.ID, s.Label)
}

func (r *Router) Drop(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if alias, ok := r.shimAlias[id]; ok {
		delete(r.aliases, alias)
		delete(r.shimAlias, id)
	}

	if s, ok := r.shims[id]; ok {
		r.unindexLabelLocked(id, s.Label)
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
	delete(r.lastAssigned, id)

	for chat, p := range r.pins {
		if p.shimID == id {
			delete(r.pins, chat)
		}
	}

	for chat, ring := range r.replyOwners {
		ring.dropShim(id)

		if len(ring.owners) == 0 {
			delete(r.replyOwners, chat)
		}
	}
}

// RecordOutbound updates chat ownership (last-writer-wins) and, when messageID
// is non-zero, records (chat_id, message_id) → shim_id so a Telegram reply to
// that message can be routed back to the original sender.
func (r *Router) RecordOutbound(shimID, chatID string, messageID int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.shims[shimID]; !ok {
		slog.Warn("RecordOutbound dropped: unknown shim", "shim_id", shimID, "chat_id", chatID, "message_id", messageID)
		return
	}

	r.chatOwners[chatID] = shimID
	r.lastOutbound[shimID] = time.Now()

	if messageID > 0 {
		ring, ok := r.replyOwners[chatID]
		if !ok {
			ring = newReplyRing(replyOwnerCapPerChat)
			r.replyOwners[chatID] = ring
		}

		ring.add(messageID, shimID)
	}

	if p, ok := r.pins[chatID]; ok && p.shimID != shimID {
		delete(r.pins, chatID)
		slog.Info("router pin cleared by other shim outbound", "chat_id", chatID, "old_pin_shim", p.shimID, "new_owner", shimID)
	}

	slog.Debug("RecordOutbound", "shim_id", shimID, "chat_id", chatID, "message_id", messageID)
}

// RouteInboundByReply resolves an inbound Telegram reply to the shim that sent
// the replied-to message. Returns false if replyToMsgID is zero, the chat has
// no recorded outbounds, the message_id was evicted from the ring, or the
// owning shim has since disconnected.
func (r *Router) RouteInboundByReply(chatID string, replyToMsgID int) (*Shim, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.routeByReplyLocked(chatID, replyToMsgID)
}

// routeByReplyLocked is shared by RouteInboundByReply and RouteInboundMulti.
// Caller holds r.mu (read or write — function only reads).
func (r *Router) routeByReplyLocked(chatID string, replyToMsgID int) (*Shim, bool) {
	if replyToMsgID == 0 {
		return nil, false
	}

	ring, ok := r.replyOwners[chatID]
	if !ok {
		return nil, false
	}

	sid, ok := ring.lookup(replyToMsgID)
	if !ok {
		return nil, false
	}

	s, ok := r.shims[sid]
	if !ok {
		slog.Warn("RouteInbound reply owner gone", "chat_id", chatID, "reply_to_message_id", replyToMsgID, "stale_owner", sid)
		return nil, false
	}

	slog.Info("RouteInbound reply", "chat_id", chatID, "reply_to_message_id", replyToMsgID, "shim_id", s.ID)

	return s, true
}

// OwnerOfMessage returns the shim_id that originally sent (chatID, msgID), if
// the (chat, message) pair is still in the per-chat replyRing. Used by
// HandleEditMessage to prevent one shim editing another shim's messages: any
// connected shim has visibility into outbound message IDs via the routing
// path, so we must verify the caller actually authored the message.
func (r *Router) OwnerOfMessage(chatID string, msgID int) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ring, ok := r.replyOwners[chatID]
	if !ok {
		return "", false
	}

	return ring.lookup(msgID)
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
			SpawnID:      s.SpawnID,
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

// Pin overrides the chat→shim routing decision for ttl. A negative or zero ttl
// is treated as already-expired (test fixture). Outbound by a different shim or
// elapsed TTL clears the pin transparently in RouteInbound/RecordOutbound.
func (r *Router) Pin(chatID, shimID string, ttl time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.shims[shimID]; !ok {
		return ErrShimNotFound
	}

	r.pins[chatID] = pin{shimID: shimID, expiresAt: time.Now().Add(ttl)}
	slog.Info("router pin set", "chat_id", chatID, "shim_id", shimID, "ttl_sec", int(ttl.Seconds()))

	return nil
}

func (r *Router) Unpin(chatID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.pins[chatID]; ok {
		delete(r.pins, chatID)
		slog.Info("router pin cleared", "chat_id", chatID)
	}
}

// SetLabel updates the runtime label for shimID and fires a NotifyLabelChanged
// push to that shim so its sessionfile can be rewritten. Returns ErrShimNotFound
// if the shim has disconnected. Empty label is allowed (clears the label).
func (r *Router) SetLabel(shimID, label string) (ShimInfo, error) {
	r.mu.Lock()

	s, ok := r.shims[shimID]
	if !ok {
		r.mu.Unlock()
		return ShimInfo{}, ErrShimNotFound
	}

	r.unindexLabelLocked(shimID, s.Label)
	s.Label = label
	r.indexLabelLocked(shimID, label)

	notify := s.Notify

	r.mu.Unlock()

	if notify != nil {
		if err := notify(ipc.NotifyLabelChanged, map[string]any{"label": label}); err != nil {
			slog.Warn("label push failed", "shim_id", shimID, "label", label, "err", err)
		}
	}

	for _, info := range r.Snapshot() {
		if info.ID == shimID {
			return info, nil
		}
	}

	return ShimInfo{}, ErrShimNotFound
}

// ResolveShimByPrefix returns the unique shim whose ID starts with prefix.
// Empty prefix is rejected (would match every shim).
func (r *Router) ResolveShimByPrefix(prefix string) (*Shim, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if prefix == "" {
		return nil, ErrShimNotFound
	}

	var found *Shim

	for id, s := range r.shims {
		if strings.HasPrefix(id, prefix) {
			if found != nil {
				return nil, ErrAmbiguousShimPrefix
			}

			found = s
		}
	}

	if found == nil {
		return nil, ErrShimNotFound
	}

	return found, nil
}

func (r *Router) RouteInbound(chatID string) (*Shim, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.routeInboundLocked(chatID)
}

// routeInboundLocked is the pin→owner→LRA→LRU resolver. Caller holds r.mu
// (write lock — pin expiry may mutate r.pins).
func (r *Router) routeInboundLocked(chatID string) (*Shim, bool) {
	if p, ok := r.pins[chatID]; ok {
		if time.Now().After(p.expiresAt) {
			delete(r.pins, chatID)
			slog.Info("router pin expired", "chat_id", chatID)
		} else if s, ok := r.shims[p.shimID]; ok {
			slog.Info("RouteInbound pin", "chat_id", chatID, "shim_id", s.ID)
			r.recordAssignmentLocked(s.ID)

			return s, true
		}
	}

	if owner, ok := r.chatOwners[chatID]; ok {
		if s, ok := r.shims[owner]; ok {
			slog.Info("RouteInbound owner", "chat_id", chatID, "shim_id", s.ID)
			r.recordAssignmentLocked(s.ID)

			return s, true
		}

		slog.Warn("RouteInbound owner gone", "chat_id", chatID, "stale_owner", owner)
	}

	if len(r.shims) >= 2 {
		if s, ok := r.lraPickLocked(); ok {
			slog.Info("RouteInbound LRA", "chat_id", chatID, "shim_id", s.ID)
			r.recordAssignmentLocked(s.ID)

			return s, true
		}
	}

	if len(r.lru) == 0 {
		slog.Warn("RouteInbound no shims", "chat_id", chatID)
		return nil, false
	}

	s, ok := r.shims[r.lru[0]]
	if ok {
		slog.Info("RouteInbound LRU fallback", "chat_id", chatID, "shim_id", s.ID, "lru", r.lru)
		r.recordAssignmentLocked(s.ID)
	}

	return s, ok
}

// recordAssignmentLocked stamps lastAssigned for the picked shim so the LRA
// picker can rotate fairly across multiple connected shims. Caller holds r.mu.
func (r *Router) recordAssignmentLocked(shimID string) {
	r.lastAssigned[shimID] = time.Now()
}

// lraPickLocked picks the connected shim with the smallest
// max(lastOutbound, lastAssigned) timestamp. Ties broken lexicographically
// by shim ID. Returns (nil, false) when no shims are connected.
// Caller holds r.mu.
func (r *Router) lraPickLocked() (*Shim, bool) {
	if len(r.shims) == 0 {
		return nil, false
	}

	var (
		best  *Shim
		bestT time.Time
	)

	for id, s := range r.shims {
		t := r.lastOutbound[id]
		if a := r.lastAssigned[id]; a.After(t) {
			t = a
		}

		switch {
		case best == nil:
		case t.Before(bestT):
		case t.Equal(bestT) && id < best.ID:
		default:
			continue
		}

		best = s
		bestT = t
	}

	return best, true
}

// RouteInboundMulti resolves an inbound message to a set of target shims using
// the precedence chain: reply-to → mentions (incl. @all broadcast) → pin →
// chat owner → LRA (least-recently-assigned, when 2+ shims) → LRU. Reply wins
// because the user explicitly targeted the prior sender via Telegram's
// quote-reply UI. A mention dispatch never rewrites chatOwners — it's a
// one-shot address. Returns nil if no shims are connected.
func (r *Router) RouteInboundMulti(chatID, content string, replyToMsgID int) []*Shim {
	mentions := parseMentions(content)

	r.mu.Lock()
	defer r.mu.Unlock()

	if s, ok := r.routeByReplyLocked(chatID, replyToMsgID); ok {
		return []*Shim{s}
	}

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

// resolveMentionsLocked translates mention tokens into Shim pointers.
//
//	@all     → every connected shim (broadcast)
//	@<alias> → exact alias match; alias wins over a same-named label
//	@<label> → case-insensitive Shim.Label match; multiple matches fan out
//
// Unknown tokens are silently dropped (slog.Warn). Caller holds r.mu.
func (r *Router) resolveMentionsLocked(chatID string, mentions []string) []*Shim {
	seen := make(map[string]struct{}, len(mentions))
	out := make([]*Shim, 0, len(mentions))

	for _, m := range mentions {
		if m == "all" {
			for _, id := range r.lru {
				if s, ok := r.shims[id]; ok {
					out = addUnseenShim(seen, out, s)
				}
			}

			continue
		}

		if id, ok := r.aliases[m]; ok {
			if s, ok := r.shims[id]; ok {
				out = addUnseenShim(seen, out, s)
			}

			continue
		}

		labelMatches := r.matchByLabelLocked(m)
		if len(labelMatches) == 0 {
			slog.Warn("mention resolved to unknown alias or label", "token", m, "chat_id", chatID)
			continue
		}

		if len(labelMatches) > 1 {
			ids := make([]string, len(labelMatches))
			for i, s := range labelMatches {
				ids[i] = s.ID
			}

			slog.Warn("label collision — fanning out", "label", m, "chat_id", chatID, "shim_ids", ids)
		}

		for _, s := range labelMatches {
			out = addUnseenShim(seen, out, s)
		}
	}

	return out
}

// addUnseenShim appends s to out (and marks it in seen) only if s.ID isn't
// already present. Used by mention dispatch to dedupe across @all, alias, and
// label fan-outs that target overlapping shims.
func addUnseenShim(seen map[string]struct{}, out []*Shim, s *Shim) []*Shim {
	if _, dup := seen[s.ID]; dup {
		return out
	}

	seen[s.ID] = struct{}{}

	return append(out, s)
}

// matchByLabelLocked returns every shim whose non-empty Label equals token
// (case-insensitive). Uses labelIndex for O(1) lookup; preserves r.lru order
// for determinism on fan-out. Caller holds r.mu.
func (r *Router) matchByLabelLocked(token string) []*Shim {
	if token == "" {
		return nil
	}

	ids := r.labelIndex[strings.ToLower(token)]
	if len(ids) == 0 {
		return nil
	}

	if len(ids) == 1 {
		if s, ok := r.shims[ids[0]]; ok {
			return []*Shim{s}
		}

		return nil
	}

	want := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		want[id] = struct{}{}
	}

	out := make([]*Shim, 0, len(ids))

	for _, id := range r.lru {
		if _, ok := want[id]; !ok {
			continue
		}

		if s, ok := r.shims[id]; ok {
			out = append(out, s)
		}
	}

	return out
}

// indexLabelLocked records a shim under its label (case-insensitive). No-op for
// empty labels. Caller holds r.mu.
func (r *Router) indexLabelLocked(shimID, label string) {
	if label == "" {
		return
	}

	key := strings.ToLower(label)
	r.labelIndex[key] = append(r.labelIndex[key], shimID)
}

// unindexLabelLocked removes shimID from the label's bucket and drops empty
// buckets. Caller holds r.mu.
func (r *Router) unindexLabelLocked(shimID, label string) {
	if label == "" {
		return
	}

	key := strings.ToLower(label)

	ids := r.labelIndex[key]
	for i, id := range ids {
		if id == shimID {
			r.labelIndex[key] = append(ids[:i], ids[i+1:]...)
			break
		}
	}

	if len(r.labelIndex[key]) == 0 {
		delete(r.labelIndex, key)
	}
}

func (r *Router) RegisterPermission(reqID, shimID string, d PermDetails) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.permOwners[reqID]; exists {
		return ErrPermissionIDInUse
	}

	if _, ok := r.shims[shimID]; !ok {
		return ErrShimNotFound
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

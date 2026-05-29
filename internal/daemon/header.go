package daemon

import (
	"context"
	"hash/fnv"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yakov/telegram-mcp/internal/access"
	"github.com/yakov/telegram-mcp/internal/bot"
)

// Default cadences for the topic-header refresh loop. Telegram tolerates a
// handful of editMessageText calls per second per chat; 5s spacing keeps us far
// below that while still feeling live. The 60s tick re-renders uptime/idle-for
// so those age even when no state event fires.
const (
	defaultHeaderRefresh = 5 * time.Second
	defaultHeaderTick    = 60 * time.Second
)

// HeaderState is the lifecycle badge shown at the top of a forum topic header.
type HeaderState int

const (
	HeaderIdle         HeaderState = iota // 🟢 connected, no work in flight
	HeaderBusy                            // 🟡 inbound routed, awaiting outbound
	HeaderPermission                      // 🔵 a tool call is awaiting approval
	HeaderDisconnected                    // ⚪ owning shim left; topic kept for reuse
	HeaderClosed                          // 🔴 /topic close — queued for purge
)

// headerBot is the slice of *bot.Bot the header manager calls. Narrow so tests
// can substitute a recorder without a live Telegram API.
type headerBot interface {
	SendMessage(ctx context.Context, chatID, text string, opts bot.SendOpts) (int, error)
	EditMessage(ctx context.Context, chatID string, messageID int, text, parseMode string) (int, error)
	PinChatMessage(ctx context.Context, chatID int64, messageID int) error
}

// HeaderIdentity is the owning shim's identity snapshot the renderer needs.
// Pulled fresh on each flush so a /label change shows without a separate hook;
// cached on the entry so a disconnected topic still renders its last owner.
type HeaderIdentity struct {
	Alias        string
	Label        string
	Workdir      string
	ShimIDPrefix string
	ConnectedAt  time.Time
}

// headerIdentitySource resolves a forum thread to its owning shim's identity.
// Satisfied by *Router (HeaderIdentity); ok=false when no shim owns the thread
// (orphan or just-disconnected), in which case the manager falls back to the
// entry's cached identity.
type headerIdentitySource interface {
	HeaderIdentity(threadID int) (HeaderIdentity, bool)
}

// headerEntry is the per-topic runtime state. Identity fields are the last
// values pulled while the shim was connected, so a ⚪ disconnected header still
// names its owner. Guarded by HeaderManager.mu.
type headerEntry struct {
	state        HeaderState
	tool         string
	lastActivity time.Time

	alias        string
	label        string
	workdir      string
	shimIDPrefix string
	connectedAt  time.Time

	dirty      bool
	lastEditAt time.Time
	// flushing serializes IO per topic: a flush sets it under m.mu before
	// releasing the lock for Telegram IO, so a concurrent flush of the same
	// topic (tick vs. Ensure/Closed) bails instead of double-sending.
	flushing bool
}

// HeaderManager maintains one pinned header message per forum topic, editing it
// in place as the owning shim's state changes. State transitions are pushed in
// (Ensure/SetState/Disconnected/Closed); identity is pulled from the Router at
// render time. Edits are coalesced and rate-limited per topic by Run's ticker;
// a content hash skips redundant edits (and the "message is not modified" 400).
type HeaderManager struct {
	store       *access.Store
	bot         headerBot
	idents      headerIdentitySource
	forumChatID int64
	refresh     time.Duration
	tick        time.Duration
	now         func() time.Time // test seam
	// onPurge, when set, is called with a thread_id after purgeTopic drops a
	// permanently-gone topic's state. Wired to Router.DropTopic so the owning
	// shim's in-memory binding is cleared too — otherwise the Router keeps
	// routing inbound to (and the shim keeps sending into) a dead thread.
	// Set once at construction wiring; read without the lock. Nil-safe.
	onPurge func(threadID int)

	mu      sync.Mutex
	entries map[int]*headerEntry
}

// SetPurgeHook wires the callback invoked when a topic is purged for being
// permanently gone in Telegram. The daemon passes Router.DropTopic so the
// purge clears the in-memory topic binding in lockstep with the persisted
// state. Safe to leave unset (purge then only cleans header + access.json).
func (m *HeaderManager) SetPurgeHook(fn func(threadID int)) {
	if m == nil {
		return
	}

	m.onPurge = fn
}

// NewHeaderManager wires a manager for the configured forum supergroup. A
// non-positive refresh/tick falls back to the package defaults. forumChatID==0
// makes every method a no-op (forum mode off) — callers normally avoid
// constructing the manager at all in that case.
func NewHeaderManager(store *access.Store, b headerBot, idents headerIdentitySource, forumChatID int64, refresh, tick time.Duration) *HeaderManager {
	if refresh <= 0 {
		refresh = defaultHeaderRefresh
	}

	if tick <= 0 {
		tick = defaultHeaderTick
	}

	return &HeaderManager{
		store:       store,
		bot:         b,
		idents:      idents,
		forumChatID: forumChatID,
		refresh:     refresh,
		tick:        tick,
		now:         time.Now,
		entries:     map[int]*headerEntry{},
	}
}

// Ensure makes sure threadID has a header and paints it immediately. Called
// after Forum.AllocateOrReuse binds a topic: on a fresh topic it sends + pins;
// on reuse (header_message_id already persisted) it edits the existing message
// back to 🟢 idle rather than resending. Safe to call on a nil manager.
func (m *HeaderManager) Ensure(ctx context.Context, threadID int) {
	if m == nil || threadID <= 0 || m.forumChatID == 0 {
		return
	}

	m.mu.Lock()

	e, ok := m.entries[threadID]
	if !ok {
		e = &headerEntry{lastActivity: m.now()}
		m.entries[threadID] = e
	}

	e.state = HeaderIdle
	e.tool = ""
	m.mu.Unlock()

	m.flush(ctx, threadID)
}

// SetState records a state transition and marks the topic dirty; the next Run
// tick flushes it (coalescing bursts into one edit). lastActivity bumps so the
// header's "last activity" line tracks the most recent event.
func (m *HeaderManager) SetState(threadID int, st HeaderState, tool string) {
	if m == nil || threadID <= 0 {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.entries[threadID]
	if !ok {
		e = &headerEntry{}
		m.entries[threadID] = e
	}

	e.state = st
	e.tool = tool
	e.lastActivity = m.now()
	e.dirty = true
}

// Disconnected flips a topic to ⚪ on owner departure. No-op when the topic has
// no header (never seen a shim); lastActivity is left frozen so the header
// reads "disconnected — last activity Ns ago".
func (m *HeaderManager) Disconnected(threadID int) {
	if m == nil || threadID <= 0 {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.entries[threadID]
	if !ok {
		return
	}

	e.state = HeaderDisconnected
	e.tool = ""
	e.dirty = true
}

// Closed flips a topic to 🔴 and flushes immediately so /topic close shows at
// once. The header stays pinned (history-visible until the purge sweep deletes
// the topic).
func (m *HeaderManager) Closed(ctx context.Context, threadID int) {
	if m == nil || threadID <= 0 || m.forumChatID == 0 {
		return
	}

	// No header was ever pinned for this topic → don't send a fresh one just to
	// mark it closed; the purge sweep would orphan it. (create() persists the
	// message id synchronously, so a zero id reliably means "never sent".)
	if m.store.Load().TopicsByThread[strconv.Itoa(threadID)].HeaderMessageID == 0 {
		return
	}

	m.mu.Lock()

	e, ok := m.entries[threadID]
	if !ok {
		e = &headerEntry{}
		m.entries[threadID] = e
	}

	e.state = HeaderClosed
	e.tool = ""
	e.dirty = true
	m.mu.Unlock()

	m.flush(ctx, threadID)
}

// Refresh marks threadID dirty without changing its state so a later tick
// repaints it — used after a /label change, where the rendered identity moved
// but the lifecycle badge did not. No-op when the topic has no header.
func (m *HeaderManager) Refresh(threadID int) {
	if m == nil || threadID <= 0 {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if e, ok := m.entries[threadID]; ok {
		e.dirty = true
	}
}

// setShimHeaderState pushes a lifecycle-state change to the header of the topic
// owned by shimID, resolving the topic via r so the TopicID read stays behind
// r.mu (race-free with BindTopic). No-op when m is nil (headers off) or the
// shim has no forum topic. Shared by the IPC handlers and the notifier.
func setShimHeaderState(m *HeaderManager, r *Router, shimID string, st HeaderState, tool string) {
	if m == nil {
		return
	}

	if tid, ok := r.TopicForShim(shimID); ok {
		m.SetState(tid, st, tool)
	}
}

// Run drives the refresh + uptime tickers until ctx is done. refresh flushes
// dirty topics (rate-limited per topic); tick re-marks active topics dirty so
// uptime/idle-for age without an explicit state event.
func (m *HeaderManager) Run(ctx context.Context) {
	refresh := time.NewTicker(m.refresh)
	defer refresh.Stop()

	uptime := time.NewTicker(m.tick)
	defer uptime.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-refresh.C:
			m.flushDirty(ctx)
		case <-uptime.C:
			m.markActiveDirty()
		}
	}
}

// flushDirty flushes every dirty topic whose per-topic rate limit has elapsed.
// Snapshots the due set under the lock, then does IO outside it.
func (m *HeaderManager) flushDirty(ctx context.Context) {
	now := m.now()

	m.mu.Lock()

	var due []int

	for tid, e := range m.entries {
		if e.dirty && now.Sub(e.lastEditAt) >= m.refresh {
			due = append(due, tid)
		}
	}

	m.mu.Unlock()

	for _, tid := range due {
		m.flush(ctx, tid)
	}
}

// markActiveDirty re-marks every live-owned topic for a render so uptime and
// idle-for advance on the background tick. Closed (🔴) and disconnected (⚪)
// topics have no live owner: their relative-time lines would otherwise change
// every tick, defeating the content-hash dedup and churning an edit per topic
// per interval forever. They paint once on the state transition, then freeze.
func (m *HeaderManager) markActiveDirty() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, e := range m.entries {
		if e.state != HeaderClosed && e.state != HeaderDisconnected {
			e.dirty = true
		}
	}
}

// flush renders threadID's header and reconciles it with Telegram: send+pin
// when no header exists yet, skip when the content hash matches what's already
// shown, otherwise edit. Identity is pulled before taking m.mu to avoid a
// lock-order inversion with Router (SetLabel holds Router.mu then calls into
// the header hook, which takes m.mu).
func (m *HeaderManager) flush(ctx context.Context, threadID int) {
	if m.forumChatID == 0 {
		return
	}

	var (
		id     HeaderIdentity
		haveID bool
	)

	if m.idents != nil {
		id, haveID = m.idents.HeaderIdentity(threadID)
	}

	m.mu.Lock()

	e, ok := m.entries[threadID]
	if !ok {
		m.mu.Unlock()
		return
	}

	if e.flushing {
		// Another flush for this topic is mid-IO. Re-mark dirty so it (or the
		// next tick) repaints with the latest state, but never issue a second
		// concurrent send/edit against the same header message.
		e.dirty = true
		m.mu.Unlock()

		return
	}

	e.flushing = true

	if haveID {
		e.alias = id.Alias
		e.label = id.Label
		e.workdir = id.Workdir
		e.shimIDPrefix = id.ShimIDPrefix
		e.connectedAt = id.ConnectedAt
	}

	now := m.now()
	view := headerView{
		State:        e.state,
		Alias:        e.alias,
		Label:        e.label,
		Workdir:      e.workdir,
		ShimIDPrefix: e.shimIDPrefix,
		Tool:         e.tool,
		LastActivity: e.lastActivity,
		ConnectedAt:  e.connectedAt,
		Now:          now,
	}
	e.dirty = false
	e.lastEditAt = now
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		e.flushing = false
		m.mu.Unlock()
	}()

	text := renderHeader(view)
	hash := headerHash(text)
	chatStr := strconv.FormatInt(m.forumChatID, 10)

	tidStr := strconv.Itoa(threadID)
	meta := m.store.Load().TopicsByThread[tidStr]

	if meta.HeaderMessageID == 0 {
		m.create(ctx, threadID, chatStr, text, hash)
		return
	}

	m.edit(ctx, threadID, chatStr, text, hash, meta)
}

// create sends a fresh header into the topic and pins it. A pin failure is a
// graceful degrade (header still visible, just not pinned) recorded as
// header_pinned=false so a later flush retries the pin.
func (m *HeaderManager) create(ctx context.Context, threadID int, chatStr, text string, hash uint64) {
	msgID, err := m.bot.SendMessage(ctx, chatStr, text, bot.SendOpts{MessageThreadID: threadID})
	if err != nil {
		if bot.IsPermanentChatError(err) {
			slog.Info("topic permanently unreachable — purging header state", "thread_id", threadID, "err", err)
			m.purgeTopic(threadID)

			return
		}

		slog.Warn("topic header send failed", "thread_id", threadID, "err", err)
		m.redirty(threadID)

		return
	}

	pinned := m.tryPin(ctx, threadID, msgID)
	m.persist(threadID, msgID, pinned, hash)
	slog.Info("topic header created", "thread_id", threadID, "message_id", msgID, "pinned", pinned)
}

// edit reconciles an existing header. It skips the editMessageText call when the
// content hash is unchanged (dedup), recreates the header when Telegram reports
// the message was deleted, and swallows "message is not modified". A re-pin is
// attempted whenever the stored pin flag is false (pin rights gained later, or
// recovery after a recreate).
func (m *HeaderManager) edit(ctx context.Context, threadID int, chatStr, text string, hash uint64, meta access.TopicMeta) {
	if hash != meta.HeaderRenderHash {
		if _, err := m.bot.EditMessage(ctx, chatStr, meta.HeaderMessageID, text, ""); err != nil {
			switch {
			case isMessageToEditNotFound(err):
				slog.Info("topic header gone — recreating", "thread_id", threadID, "message_id", meta.HeaderMessageID)
				m.create(ctx, threadID, chatStr, text, hash)

				return
			case isMessageNotModified(err):
				// Content already current despite a hash miss; fall through to
				// persist the hash so we stop retrying.
			case bot.IsPermanentChatError(err):
				slog.Info("topic permanently unreachable — purging header state", "thread_id", threadID, "err", err)
				m.purgeTopic(threadID)

				return
			default:
				slog.Warn("topic header edit failed", "thread_id", threadID, "message_id", meta.HeaderMessageID, "err", err)
				m.redirty(threadID)

				return
			}
		}
	}

	pinned := meta.HeaderPinned
	if !pinned {
		pinned = m.tryPin(ctx, threadID, meta.HeaderMessageID)
	}

	// Nothing changed (hash matched, pin flag unchanged) — skip the store write
	// so the 60s uptime tick doesn't churn access.json for stable topics.
	if hash == meta.HeaderRenderHash && pinned == meta.HeaderPinned {
		return
	}

	m.persist(threadID, meta.HeaderMessageID, pinned, hash)
}

// tryPin pins messageID in the forum chat, reporting success. A failure logs a
// warning and returns false (caller persists header_pinned=false for retry).
func (m *HeaderManager) tryPin(ctx context.Context, threadID, messageID int) bool {
	if err := m.bot.PinChatMessage(ctx, m.forumChatID, messageID); err != nil {
		slog.Warn("topic header pin failed — header visible but unpinned",
			"thread_id", threadID, "message_id", messageID, "err", err)

		return false
	}

	return true
}

// persist writes the header bookkeeping (message id, pin flag, content hash)
// into the topic's TopicMeta, preserving the meta's other fields.
func (m *HeaderManager) persist(threadID, msgID int, pinned bool, hash uint64) {
	tidStr := strconv.Itoa(threadID)

	if err := m.store.Mutate(func(st *access.State) bool {
		if st.TopicsByThread == nil {
			st.TopicsByThread = map[string]access.TopicMeta{}
		}

		meta := st.TopicsByThread[tidStr]
		if meta.ThreadID == 0 {
			meta.ThreadID = threadID
		}

		meta.HeaderMessageID = msgID
		meta.HeaderPinned = pinned
		meta.HeaderRenderHash = hash
		st.TopicsByThread[tidStr] = meta

		return true
	}); err != nil {
		slog.Error("topic header meta persist failed", "thread_id", threadID, "err", err)
	}
}

// redirty re-flags a topic after a failed flush so a later tick retries.
func (m *HeaderManager) redirty(threadID int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if e, ok := m.entries[threadID]; ok {
		e.dirty = true
	}
}

// purgeTopic drops all runtime + persisted state for a forum topic that no
// longer exists in Telegram (deleted by a user/admin). Dropping the runtime
// entry stops markActiveDirty from re-dirtying it; deleting the meta, every
// reuse-key pointing at it, and any closed-queue entry stops a later hello from
// rebinding a dead thread. Without this, a recreate into the gone thread keeps
// failing and redirty re-queues it — an unbounded editMessageText error loop.
func (m *HeaderManager) purgeTopic(threadID int) {
	m.mu.Lock()
	delete(m.entries, threadID)
	m.mu.Unlock()

	tidStr := strconv.Itoa(threadID)

	if err := m.store.Mutate(func(st *access.State) bool {
		changed := false

		if _, ok := st.TopicsByThread[tidStr]; ok {
			delete(st.TopicsByThread, tidStr)

			changed = true
		}

		for key, tid := range st.TopicsByReuseKey {
			if tid == threadID {
				delete(st.TopicsByReuseKey, key)

				changed = true
			}
		}

		for i, ct := range st.ClosedTopics {
			if ct.ThreadID == threadID {
				st.ClosedTopics = append(st.ClosedTopics[:i], st.ClosedTopics[i+1:]...)
				changed = true

				break
			}
		}

		return changed
	}); err != nil {
		slog.Error("topic purge: state cleanup save failed", "thread_id", threadID, "err", err)
	}

	// Clear the in-memory Router binding too (the owning shim's TopicID +
	// topicOwners entry) so inbound stops routing to the dead thread and the
	// shim re-allocates a fresh topic on its next hello. Without this the
	// persisted state and the Router diverge.
	if m.onPurge != nil {
		m.onPurge(threadID)
	}
}

// headerView is the immutable input to renderHeader — a flattened snapshot so
// rendering is a pure function (testable without a manager or a clock).
type headerView struct {
	State        HeaderState
	Alias        string
	Label        string
	Workdir      string
	ShimIDPrefix string
	Tool         string
	LastActivity time.Time
	ConnectedAt  time.Time
	Now          time.Time
}

// renderHeader produces the plain-text header body. Plain text (not MarkdownV2)
// because workdir/label are full of `/._-` which would each need escaping for
// zero rendering benefit, and unescaped specials would also perturb the dedup
// hash. cc_pid is intentionally omitted: it is never transmitted daemon-side
// (the shim knows it via getppid but does not send it over IPC).
func renderHeader(v headerView) string {
	lines := []string{
		headerIcon(v.State) + " " + headerTitle(v),
		"workdir: " + orDash(v.Workdir),
		"label: " + orNone(v.Label),
		"status: " + headerStatusLine(v.State, v.Tool),
		"last activity: " + agoLine(v.Now, v.LastActivity),
		"uptime: " + uptimeLine(v.Now, v.ConnectedAt),
		"shim: " + orDash(v.ShimIDPrefix),
	}

	return strings.Join(lines, "\n")
}

func headerIcon(st HeaderState) string {
	switch st {
	case HeaderBusy:
		return "🟡"
	case HeaderPermission:
		return "🔵"
	case HeaderDisconnected:
		return "⚪"
	case HeaderClosed:
		return "🔴"
	case HeaderIdle:
		return "🟢"
	default:
		return "🟢"
	}
}

// headerTitle is the first-line subject: `@alias — <label or workdir basename>`,
// or just `@alias` when neither is available. Mirrors buildTopicName.
func headerTitle(v headerView) string {
	suffix := v.Label
	if suffix == "" {
		base := filepath.Base(v.Workdir)
		if base != "" && base != "." && base != "/" {
			suffix = base
		}
	}

	if suffix == "" {
		return "@" + v.Alias
	}

	return "@" + v.Alias + " — " + suffix
}

func headerStatusLine(st HeaderState, tool string) string {
	switch st {
	case HeaderBusy:
		if tool != "" {
			return "busy — " + tool
		}

		return "busy"
	case HeaderPermission:
		if tool != "" {
			return "awaiting permission — " + tool
		}

		return "awaiting permission"
	case HeaderDisconnected:
		return "disconnected — next session in same workdir reattaches"
	case HeaderClosed:
		return "closed — scheduled for purge"
	case HeaderIdle:
		return "idle"
	default:
		return "idle"
	}
}

// agoLine renders "<dur> ago", or "—" when the timestamp is zero (no activity
// recorded yet).
func agoLine(now, t time.Time) string {
	if t.IsZero() {
		return "—"
	}

	return humanizeDuration(now.Sub(t)) + " ago"
}

// uptimeLine renders the session lifetime, or "—" when connectedAt is unknown.
func uptimeLine(now, connectedAt time.Time) string {
	if connectedAt.IsZero() {
		return "—"
	}

	return humanizeDuration(now.Sub(connectedAt))
}

// humanizeDuration formats a duration coarsely with the largest two relevant
// units: "12s", "5m", "2h 15m", "3d 4h". Negative durations clamp to "0s".
func humanizeDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}

	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		h := int(d.Hours())
		mins := int(d.Minutes()) % 60

		return strconv.Itoa(h) + "h " + strconv.Itoa(mins) + "m"
	default:
		days := int(d.Hours()) / 24
		h := int(d.Hours()) % 24

		return strconv.Itoa(days) + "d " + strconv.Itoa(h) + "h"
	}
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}

	return s
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}

	return s
}

// headerHash is the dedup key for a rendered header. FNV-64a is fast and
// collision-resistant enough for "did the text change" — not a security hash.
func headerHash(text string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(text))

	return h.Sum64()
}

// isMessageToEditNotFound matches Telegram's 400 when the header message was
// deleted out from under us — triggers a recreate. No telego sentinel exists;
// the description text is Telegram's stable contract for the condition.
func isMessageToEditNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "message to edit not found")
}

// isMessageNotModified matches Telegram's 400 on a no-op edit. The hash check
// normally prevents reaching here; if a race slips through we swallow it.
func isMessageNotModified(err error) bool {
	return err != nil && strings.Contains(err.Error(), "message is not modified")
}

// HeaderEnabled reports whether the daemon should maintain pinned topic headers.
// Disabled by env TELEGRAM_TOPIC_HEADER in {"0","false","no","off"}
// (case-insensitive). Default: enabled (headers only appear in forum mode).
func HeaderEnabled() bool {
	v := strings.TrimSpace(os.Getenv("TELEGRAM_TOPIC_HEADER"))
	if v == "" {
		return true
	}

	switch strings.ToLower(v) {
	case "0", "false", "no", "off":
		return false
	}

	return true
}

package daemon

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/yakov/telegram-mcp/internal/access"
	"github.com/yakov/telegram-mcp/internal/bot"
	"github.com/yakov/telegram-mcp/internal/ipc"
)

// defaultAutoSpawnCooldown bounds how often a single forum topic re-triggers an
// auto-spawn. Covers the spawn-bootstrap window (fork → plugin load → shim
// hello → BindTopic); once the topic is owned, the empty-topic path stops
// firing. A failed spawn retries after the cooldown.
const defaultAutoSpawnCooldown = 90 * time.Second

// topicSpawner is the slice of SpawnRunner the Notifier needs to fork a CC
// session into an empty forum topic. *SpawnRunner satisfies it.
type topicSpawner interface {
	Spawn(ctx context.Context, req bot.SpawnRequest) (string, error)
}

// Notifier implements bot.Notifier by routing daemon-side bot callbacks to
// the right shim over IPC. The bot package doesn't import daemon — it sees
// only bot.Notifier.
type Notifier struct {
	router  *Router
	store   *access.Store
	typing  *TypingTracker
	mutator *AdminMutator
	header  *HeaderManager

	// Forum auto-spawn: an inbound landing in a forum topic that no shim owns
	// forks a CC session pinned to that topic instead of dropping the message.
	// nil spawner disables. The original message is NOT forwarded to the new
	// session (its MCP isn't ready that early) — the user re-asks once the
	// spawned shim registers and claims the topic via the normal hello path.
	spawner           topicSpawner
	autoSpawnCooldown time.Duration
	spawnMu           sync.Mutex
	lastTopicSpawn    map[int]time.Time
}

// SetAutoSpawn enables forum auto-spawn with the given per-topic cooldown
// (<=0 → default). A nil spawner leaves the feature off. Called once at daemon
// wiring, before Poll, so the field writes happen-before any DeliverInbound.
func (n *Notifier) SetAutoSpawn(spawner topicSpawner, cooldown time.Duration) {
	if cooldown <= 0 {
		cooldown = defaultAutoSpawnCooldown
	}

	n.spawner = spawner
	n.autoSpawnCooldown = cooldown
	n.lastTopicSpawn = map[int]time.Time{}
}

// NewNotifier wires the router used to fan out inbound messages plus, optionally,
// a TypingTracker (nil disables typing-refresh) and the access.Store used to
// decide whether the rotating-reaction half of the indicator should fire (only
// when access.State.AckReaction is non-empty). Passing nil for store is allowed
// and silently disables reaction rotation while typing-refresh still runs.
func NewNotifier(r *Router, store *access.Store, typing *TypingTracker) *Notifier {
	return &Notifier{router: r, store: store, typing: typing}
}

// DeliverInbound fans an inbound Telegram message out to every target shim
// resolved by the Router. RouteInboundMulti returns a snapshot of *Shim
// pointers and the Router's mu is released on its return — so the per-target
// Notify calls below run concurrently across DeliverInbound invocations for
// different chats, never serialized on r.mu.
func (n *Notifier) DeliverInbound(content string, meta map[string]string) {
	chatID := meta["chat_id"]
	replyToMsgID, _ := strconv.Atoi(meta["reply_to_message_id"])
	threadID, _ := strconv.Atoi(meta["message_thread_id"])

	targets := n.router.RouteInboundMulti(chatID, content, replyToMsgID, threadID)
	if len(targets) == 0 {
		if n.maybeAutoSpawn(chatID, threadID, meta) {
			return
		}

		slog.Warn("inbound dropped: no shim connected", "chat_id", chatID, "thread_id", threadID, "user", meta["user"])

		return
	}

	// Mark chat for typing-refresh BEFORE notifying shims. If the shim is fast
	// enough to send an outbound (and the IPC handler calls Clear) before this
	// goroutine reaches Mark, the order would invert and Mark would re-add a
	// just-cleared chat — leaving the typing indicator armed for one full TTL.
	msgID, _ := strconv.Atoi(meta["message_id"])
	n.typing.Mark(chatID, msgID, n.shouldRotateReaction())

	params := map[string]any{
		"content": content,
		"meta":    meta,
	}

	slog.Info("DeliverInbound dispatch",
		"chat_id", chatID,
		"fanout", len(targets),
		"targets", shimIDs(targets),
		"content_len", len(content),
	)

	for _, t := range targets {
		n.headerState(t.ID, HeaderBusy, "")

		if err := t.Notify(ipc.NotifyInbound, params); err != nil {
			slog.Error("inbound notify failed", "shim_id", t.ID, "chat_id", chatID, "err", err)
		}
	}
}

// maybeAutoSpawn forks a CC session into an unowned forum topic so a message in
// an empty topic boots a session instead of silently dropping. Returns true
// when it owns the inbound (spawn fired, or suppressed by the per-topic
// cooldown) so the caller skips the "dropped" warning. The spawn is pinned to
// the topic via SpawnRequest.ThreadID, so the new shim adopts it on hello and
// future messages route there normally. Best-effort: the original message is
// not forwarded — the user re-asks once the session is up.
func (n *Notifier) maybeAutoSpawn(chatID string, threadID int, meta map[string]string) bool {
	if n.spawner == nil || !n.inForumTopic(chatID, threadID) {
		return false
	}

	n.spawnMu.Lock()
	// Evict entries past the cooldown window: they no longer suppress anything,
	// so retaining them just leaks one map entry per topic ever auto-spawned.
	for tid, t := range n.lastTopicSpawn {
		if time.Since(t) >= n.autoSpawnCooldown {
			delete(n.lastTopicSpawn, tid)
		}
	}

	if last, seen := n.lastTopicSpawn[threadID]; seen && time.Since(last) < n.autoSpawnCooldown {
		n.spawnMu.Unlock()
		slog.Info("forum auto-spawn suppressed by cooldown", "thread_id", threadID, "chat_id", chatID)

		return true
	}

	n.lastTopicSpawn[threadID] = time.Now()
	n.spawnMu.Unlock()

	req := bot.SpawnRequest{
		Workdir:  n.topicWorkdir(threadID),
		ChatID:   chatID,
		UserID:   meta["user_id"],
		ThreadID: threadID,
	}
	n.applyEffort(chatID, &req)

	slog.Info("forum auto-spawn into empty topic",
		"thread_id", threadID, "chat_id", chatID, "workdir", req.Workdir, "user", meta["user"])

	// Spawn off the inbound goroutine: it forks a pty and posts a Telegram
	// status message, neither of which should block the bot's update loop.
	go func() {
		if _, err := n.spawner.Spawn(context.Background(), req); err != nil {
			slog.Warn("forum auto-spawn failed", "thread_id", threadID, "chat_id", chatID, "err", err)
		}
	}()

	return true
}

// inForumTopic reports whether (chatID, threadID) is a real topic in the
// configured forum supergroup — the only place auto-spawn fires.
func (n *Notifier) inForumTopic(chatID string, threadID int) bool {
	if threadID <= 0 || n.store == nil {
		return false
	}

	fc := n.store.Load().ForumChatID

	return fc != 0 && chatID == strconv.FormatInt(fc, 10)
}

// topicWorkdir returns the workdir recorded for threadID so the spawned session
// opens in the topic's repo. Empty when the topic is untracked — SpawnRunner
// then falls back to its configured default workdir.
func (n *Notifier) topicWorkdir(threadID int) string {
	if n.store == nil {
		return ""
	}

	if meta, ok := n.store.Load().TopicsByThread[strconv.Itoa(threadID)]; ok {
		return meta.Workdir
	}

	return ""
}

// applyEffort copies the chat's configured model/thinking budget onto req so an
// auto-spawn honors /effort exactly like a manual /spawn.
func (n *Notifier) applyEffort(chatID string, req *bot.SpawnRequest) {
	if n.store == nil {
		return
	}

	level, ok := n.store.Load().EffortByChat[chatID]
	if !ok {
		return
	}

	if cfg, found := bot.ResolveEffort(level); found {
		req.Model = cfg.Model
		req.ThinkingTokens = cfg.ThinkingTokens
	}
}

// shouldRotateReaction reports whether the user opted into ack reactions.
// Returns false when the store is unwired (tests) or AckReaction is unset,
// which keeps reaction rotation aligned with bot.handleMessage's initial
// reaction placement.
func (n *Notifier) shouldRotateReaction() bool {
	if n.store == nil {
		return false
	}

	return n.store.Load().AckReaction != ""
}

func shimIDs(targets []*Shim) []string {
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		out = append(out, t.ID)
	}

	return out
}

func (n *Notifier) LookupPermission(requestID string) (bot.PermissionDetails, bool) {
	d, ok := n.router.LookupPermissionDetails(requestID)
	if !ok {
		return bot.PermissionDetails{}, false
	}

	return bot.PermissionDetails{
		ToolName:     d.ToolName,
		Description:  d.Description,
		InputPreview: d.InputPreview,
	}, true
}

// SetMutator wires the admin mutation engine so owner ✅/❌ taps resolve over
// the bot → Notifier seam (bot must not import daemon). nil → ResolveMutation
// reports the feature is off. Set in cmd/server before Poll, same as the bot's
// notifier wiring.
func (n *Notifier) SetMutator(m *AdminMutator) { n.mutator = m }

// SetHeader wires the topic-header manager (nil disables header state updates).
// Called once at daemon wiring before Poll, so the write happens-before any
// DeliverInbound/ResolvePermission.
func (n *Notifier) SetHeader(m *HeaderManager) { n.header = m }

// headerState pushes a lifecycle-state change to the topic header owned by
// shimID. No-op when headers are off or the shim has no forum topic.
func (n *Notifier) headerState(shimID string, st HeaderState, tool string) {
	setShimHeaderState(n.header, n.router, shimID, st, tool)
}

// ResolveMutation implements bot.Notifier: routes an owner confirm tap to the
// AdminMutator. The bot already gate-authenticated the tapper.
func (n *Notifier) ResolveMutation(ctx context.Context, pendingID string, approve bool) (bool, string) {
	if n.mutator == nil {
		return false, "admin mutations are not enabled on this daemon"
	}

	return n.mutator.Resolve(ctx, pendingID, approve)
}

func (n *Notifier) ResolvePermission(requestID, behavior string) {
	target, ok := n.router.RoutePermission(requestID)
	n.router.ResolvePermission(requestID)

	if !ok {
		slog.Warn("permission resolution dropped: shim gone", "request_id", requestID, "behavior", behavior)
		return
	}

	// Permission resolved → the agent resumes work; flip 🔵 back to 🟡 busy
	// until its next outbound settles the header to 🟢 idle.
	n.headerState(target.ID, HeaderBusy, "")

	if err := target.Notify(ipc.NotifyPermissionResolved, map[string]any{
		"request_id": requestID,
		"behavior":   behavior,
	}); err != nil {
		slog.Error("permission notify failed", "shim_id", target.ID, "request_id", requestID, "err", err)
	}
}

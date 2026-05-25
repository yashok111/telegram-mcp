package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/yakov/telegram-mcp/internal/access"
	"github.com/yakov/telegram-mcp/internal/bot"
)

// forumBot is the subset of bot.Bot the Forum manager calls into. Lets tests
// drop in a fake without standing up a real Telegram API.
type forumBot interface {
	CreateForumTopic(ctx context.Context, chatID int64, name string, iconColor int) (int, error)
	EditForumTopic(ctx context.Context, chatID int64, threadID int, name string) error
	SendMessage(ctx context.Context, chatID, text string, opts bot.SendOpts) (int, error)
}

// Forum is the daemon-side topic allocator + reuse-key resolver. Stateless
// beyond access.Store; safe to call from concurrent hello handlers because
// access.Store.Mutate serializes the read-modify-write window.
type Forum struct {
	store *access.Store
	bot   forumBot
	// isLive reports whether a shim_id is currently connected. A topic lock
	// records the ephemeral shim_id of its holder; that id is minted fresh on
	// every hello (crypto/rand) and regenerated on every reconnect, so a lock
	// persisted to access.json goes stale the moment the daemon restarts —
	// the holder reconnects under a new id while the old id rots on disk.
	// Consulting live connections lets reuse seize a stale lock instead of
	// orphaning a duplicate topic. nil means "assume held" (no seizing).
	isLive func(shimID string) bool
	// home is the workdir treated as the "default" pwd. Sessions started
	// from $HOME or with empty workdir get a fresh topic rather than
	// schclassing into a shared bucket.
	home string
	// topicForSpawn resolves a spawn_id → the forum thread its /spawn was
	// issued from (ok=false for user-launched shims, DM spawns, or when no
	// lookup is wired). Wired to SpawnRunner.TopicForSpawn. When it resolves,
	// the shim is seated in that exact topic (priority over label/workdir).
	topicForSpawn func(spawnID string) (int, bool)
}

// NewForum constructs a Forum bound to a single Store + forumBot pair. isLive
// reports whether a shim_id is still connected; pass Router.IsConnected so
// reuse can distinguish a live lock holder from a stale one left by a crashed
// or restarted daemon. $HOME resolution is at constructor time — if HOME
// changes mid-process (not expected outside tests), restart the daemon.
func NewForum(store *access.Store, b forumBot, isLive func(shimID string) bool, topicForSpawn func(spawnID string) (int, bool)) *Forum {
	home, _ := os.UserHomeDir()
	return &Forum{store: store, bot: b, isLive: isLive, home: home, topicForSpawn: topicForSpawn}
}

// Enabled reports whether forum routing is active (ForumChatID configured).
// Cheap; reads the cached store snapshot.
func (f *Forum) Enabled() bool {
	return f.store.Load().ForumChatID != 0
}

// resolveReuseKey returns the lookup key for finding an existing topic to
// reuse. Composite priority: label (explicit) → workdir-if-not-home →
// fresh. Returns ok=false when no reusable key applies; the caller must
// create a fresh topic without registering any reuse-key mapping.
func (f *Forum) resolveReuseKey(shim *Shim) (string, bool) {
	if shim.Label != "" {
		return "label:" + shim.Label, true
	}

	if shim.Workdir != "" && shim.Workdir != f.home {
		return "workdir:" + shim.Workdir, true
	}

	return "", false
}

// AllocateOrReuse picks a forum topic for shim and returns its thread_id.
//
// A /spawn-pinned shim (its spawn_id resolves to a forum thread via
// topicForSpawn) is seated in that exact topic — adopted in place, no new
// topic created — which takes priority over label/workdir. If that topic is
// held by another live shim, it falls through to the normal allocation below
// rather than co-locating two sessions in one topic.
//
// Otherwise it reuses an existing topic keyed by label/workdir when free, or
// creates a fresh one. Returns (0, nil) when ForumChatID is unset (feature
// off) — caller falls back to DM-mode.
func (f *Forum) AllocateOrReuse(ctx context.Context, shim *Shim) (int, error) {
	if forced, ok := f.forcedTopic(shim); ok {
		tid, handled, err := f.adoptForcedTopic(ctx, shim, forced)
		if handled {
			return tid, err
		}
		// Pinned topic held by another live shim — fall through to normal
		// label/workdir/fresh allocation instead of co-locating.
	}

	return f.allocateByReuseKey(ctx, shim)
}

// forcedTopic reports the forum thread the shim's /spawn was issued from, via
// the wired spawn→thread lookup. ok=false for user-launched shims (no
// spawn_id), DM spawns (thread 0), or when no lookup is wired.
func (f *Forum) forcedTopic(shim *Shim) (int, bool) {
	if shim.SpawnID == "" || f.topicForSpawn == nil {
		return 0, false
	}

	tid, ok := f.topicForSpawn(shim.SpawnID)
	if !ok || tid <= 0 {
		// Guard the boundary: a 0/negative thread is never a real topic, so
		// never let it become a "topic:0" reuse-key.
		return 0, false
	}

	return tid, true
}

// adoptForcedTopic seats shim in the exact forum thread its /spawn was issued
// from. The thread already exists in Telegram (the user typed /spawn in it),
// so no CreateForumTopic call is made — meta plus a `topic:<tid>` reuse-key
// are registered and locked to shim. A free or stale-locked topic is
// (re)claimed; one held by another *live* shim is refused (handled=false) so
// the caller falls back to normal allocation. handled=true means the result
// is authoritative (adopted, or feature off); handled=false means "not mine,
// allocate normally".
func (f *Forum) adoptForcedTopic(ctx context.Context, shim *Shim, forced int) (int, bool, error) {
	reuseKey := "topic:" + strconv.Itoa(forced)
	tidStr := strconv.Itoa(forced)

	var (
		forumChat int64
		oldName   string
		conflict  bool
	)

	if err := f.store.Mutate(func(st *access.State) bool {
		forumChat = st.ForumChatID
		if forumChat == 0 {
			return false
		}

		meta, exists := st.TopicsByThread[tidStr]
		if exists && meta.LockedBy != "" && meta.LockedBy != shim.ID && f.connected(meta.LockedBy) {
			conflict = true
			return false
		}

		switch {
		case !exists:
			meta = access.TopicMeta{ThreadID: forced, Workdir: shim.Workdir, Label: shim.Label}
		default:
			oldName = meta.Name
			if meta.LockedBy != "" && meta.LockedBy != shim.ID {
				slog.Info("seizing stale topic lock for /spawn-pinned topic",
					"thread_id", forced, "stale_locked_by", meta.LockedBy, "shim_id", shim.ID)
			}
		}

		meta.LockedBy = shim.ID
		meta.LastShimID = shim.ID

		if st.TopicsByThread == nil {
			st.TopicsByThread = map[string]access.TopicMeta{}
		}

		st.TopicsByThread[tidStr] = meta

		if st.TopicsByReuseKey == nil {
			st.TopicsByReuseKey = map[string]int{}
		}

		st.TopicsByReuseKey[reuseKey] = forced

		return true
	}); err != nil {
		return 0, true, fmt.Errorf("forum: adopt pinned topic save: %w", err)
	}

	if conflict {
		slog.Warn("/spawn target topic held by another live shim — using default allocation",
			"thread_id", forced, "shim_id", shim.ID)

		return 0, false, nil
	}

	if forumChat == 0 {
		return 0, true, nil
	}

	// Label the adopted topic with the shim's alias, consistent with
	// daemon-created topics. oldName=="" (untracked, user-made topic) forces
	// the push; a reused topic only re-pushes on alias divergence.
	f.resyncName(ctx, forumChat, forced, oldName, shim)
	slog.Info("topic adopted for /spawn", "shim_id", shim.ID, "thread_id", forced, "reuse_key", reuseKey)

	return forced, true, nil
}

// allocateByReuseKey reuses an existing topic keyed by label/workdir when
// free, otherwise creates a fresh topic via the Telegram API. Returns the
// thread_id to use for outbound messages.
//
// On parallel collision (reuse_key matches an existing topic still
// LockedBy another shim), creates a fresh topic and logs a Warn. Concurrent
// callers race only inside access.Store.Mutate; the loser sees a non-empty
// LockedBy and falls through to fresh creation.
func (f *Forum) allocateByReuseKey(ctx context.Context, shim *Shim) (int, error) {
	var (
		threadID   int
		forumChat  int64
		reuseKey   string
		haveKey    bool
		oldName    string
		collision  bool
		holderName string
	)

	if err := f.store.Mutate(func(st *access.State) bool {
		forumChat = st.ForumChatID
		if forumChat == 0 {
			return false
		}

		reuseKey, haveKey = f.resolveReuseKey(shim)
		if !haveKey {
			return false
		}

		tid, ok := st.TopicsByReuseKey[reuseKey]
		if !ok {
			return false
		}

		tidStr := strconv.Itoa(tid)

		meta, exists := st.TopicsByThread[tidStr]
		if !exists {
			slog.Warn("topic reuse key references missing meta — dropping stale key",
				"reuse_key", reuseKey, "thread_id", tid)
			delete(st.TopicsByReuseKey, reuseKey)

			return true
		}

		if heldByOther := meta.LockedBy != "" && meta.LockedBy != shim.ID; heldByOther {
			if f.connected(meta.LockedBy) {
				slog.Warn("topic locked by another live shim — creating fresh",
					"reuse_key", reuseKey, "locked_by", meta.LockedBy, "shim_id", shim.ID, "thread_id", tid)

				collision = true
				holderName = meta.Name

				return false
			}

			// Holder is gone (clean disconnect missed, or a prior daemon died
			// before ReleaseLock). The lock is stale — seize it and reuse the
			// topic rather than orphaning a duplicate on every restart.
			slog.Info("seizing stale topic lock from disconnected shim",
				"reuse_key", reuseKey, "stale_locked_by", meta.LockedBy, "shim_id", shim.ID, "thread_id", tid)
		}

		meta.LockedBy = shim.ID
		meta.LastShimID = shim.ID
		st.TopicsByThread[tidStr] = meta
		threadID = tid
		oldName = meta.Name

		return true
	}); err != nil {
		return 0, fmt.Errorf("forum: lock claim save: %w", err)
	}

	if forumChat == 0 {
		return 0, nil
	}

	if threadID != 0 {
		f.resyncName(ctx, forumChat, threadID, oldName, shim)
		slog.Info("topic reused", "shim_id", shim.ID, "thread_id", threadID, "reuse_key", reuseKey)

		return threadID, nil
	}

	name := buildTopicName(shim)

	tid, err := f.bot.CreateForumTopic(ctx, forumChat, name, 0)
	if err != nil {
		return 0, err
	}

	tidStr := strconv.Itoa(tid)

	if err := f.store.Mutate(func(st *access.State) bool {
		if st.TopicsByThread == nil {
			st.TopicsByThread = map[string]access.TopicMeta{}
		}

		if st.TopicsByReuseKey == nil {
			st.TopicsByReuseKey = map[string]int{}
		}

		st.TopicsByThread[tidStr] = access.TopicMeta{
			ThreadID:   tid,
			Workdir:    shim.Workdir,
			Label:      shim.Label,
			Name:       name,
			LastShimID: shim.ID,
			LockedBy:   shim.ID,
		}
		if haveKey {
			// Race detection: if a concurrent AllocateOrReuse with the same
			// reuse_key reached the register step ahead of us, log the
			// collision so the operator notices the orphan topic. We still
			// overwrite (last writer wins) — the loser's freshly-created
			// topic is tracked under TopicsByThread and the sweep (Wave 5)
			// will eventually reap unreferenced threads. Single-user design
			// makes this race vanishingly rare in practice.
			if prev, exists := st.TopicsByReuseKey[reuseKey]; exists && prev != tid {
				slog.Warn("forum: reuse_key race — duplicate topic created",
					"reuse_key", reuseKey, "winner_thread_id", tid, "orphan_thread_id", prev)
			}

			st.TopicsByReuseKey[reuseKey] = tid
		}

		return true
	}); err != nil {
		// CreateForumTopic already succeeded — topic exists in Telegram but
		// is no longer tracked in access.json, so the sweep can't reach it.
		// Log thread_id at error level so the operator can manually delete
		// (or re-link by editing access.json) instead of orphaning silently.
		slog.Error("forum: orphan topic — created in Telegram but state save failed",
			"thread_id", tid, "forum_chat_id", forumChat, "shim_id", shim.ID, "name", name, "err", err)

		return 0, fmt.Errorf("forum: register save (orphan thread_id=%d): %w", tid, err)
	}

	slog.Info("topic created", "shim_id", shim.ID, "thread_id", tid, "name", name, "reuse_key", reuseKey)

	if collision {
		f.warnCollision(ctx, forumChat, tid, reuseKey, holderName, shim.Alias)
	}

	return tid, nil
}

// warnCollision posts a notice into a freshly-created topic explaining that it
// is a duplicate: another live session already holds a topic for the same
// reuse key, and the two can't share one Telegram topic (traffic would mix).
// The duplicate is intentional — the message just makes it explicit so the
// user understands where the second topic came from. Cosmetic: a send failure
// is logged, never propagated, so it can't drop the shim to DM-mode.
func (f *Forum) warnCollision(ctx context.Context, forumChat int64, threadID int, reuseKey, holderName, newAlias string) {
	keySubject := "workdir"
	if strings.HasPrefix(reuseKey, "label:") {
		keySubject = "label"
	}

	holderDesc := ""
	if holderName != "" {
		holderDesc = " (" + holderName + ")"
	}

	text := fmt.Sprintf(
		"⚠️ Another active session%s is already attached to this %s. "+
			"Two sessions can't share one Telegram topic, so this is a separate topic for @%s — "+
			"messages here won't reach the other session.",
		holderDesc, keySubject, newAlias)

	if _, err := f.bot.SendMessage(ctx, strconv.FormatInt(forumChat, 10), text, bot.SendOpts{MessageThreadID: threadID}); err != nil {
		slog.Warn("forum: collision warning send failed",
			"thread_id", threadID, "reuse_key", reuseKey, "err", err)
	}
}

// connected reports whether shimID is a currently-attached shim. A nil isLive
// (not wired) conservatively answers true so locks are never seized.
func (f *Forum) connected(shimID string) bool {
	return f.isLive != nil && f.isLive(shimID)
}

// resyncName re-pushes the topic title when a reused topic's stored name has
// diverged from the reattaching shim's — typically an alias migration after
// the original owner disconnected, which otherwise leaves two topics showing
// an identical frozen title. Cosmetic: a failed edit or save is logged but
// never propagated, so a stale title can't drop the shim to DM-mode. The
// stored name is updated only after EditForumTopic succeeds, so a failed push
// retries on the next reuse.
func (f *Forum) resyncName(ctx context.Context, forumChat int64, threadID int, oldName string, shim *Shim) {
	newName := buildTopicName(shim)
	if newName == oldName {
		return
	}

	if err := f.bot.EditForumTopic(ctx, forumChat, threadID, newName); err != nil {
		slog.Warn("topic name resync failed — keeping stale title",
			"shim_id", shim.ID, "thread_id", threadID, "name", newName, "err", err)

		return
	}

	slog.Info("topic name resynced",
		"shim_id", shim.ID, "thread_id", threadID, "old_name", oldName, "new_name", newName)

	if err := f.store.Mutate(func(st *access.State) bool {
		tidStr := strconv.Itoa(threadID)

		meta, ok := st.TopicsByThread[tidStr]
		if !ok {
			slog.Warn("topic name resync: thread entry gone before persist",
				"shim_id", shim.ID, "thread_id", threadID, "name", newName)

			return false
		}

		meta.Name = newName
		st.TopicsByThread[tidStr] = meta

		return true
	}); err != nil {
		slog.Error("topic name resync: state save failed",
			"shim_id", shim.ID, "thread_id", threadID, "name", newName, "err", err)
	}
}

// ReleaseLock clears LockedBy on every topic owned by shimID so the next
// matching hello can reattach. Mapping (reuse_key → thread_id, thread_id →
// meta) is preserved; only the lock flag drops. Called from the daemon's
// IPC OnDisconnect hook.
func (f *Forum) ReleaseLock(shimID string) {
	if err := f.store.Mutate(func(st *access.State) bool {
		if st.TopicsByThread == nil {
			return false
		}

		changed := false

		for tidStr, meta := range st.TopicsByThread {
			if meta.LockedBy != shimID {
				continue
			}

			meta.LockedBy = ""
			st.TopicsByThread[tidStr] = meta
			changed = true

			slog.Info("topic lock released", "shim_id", shimID, "thread_id", meta.ThreadID)
		}

		return changed
	}); err != nil {
		slog.Error("forum: release lock save failed", "shim_id", shimID, "err", err)
	}
}

// buildTopicName composes the human-visible topic name shown in Telegram's
// supergroup topic list. Format: `@<alias> — <label or workdir basename>`
// or just `@<alias>` when neither is available.
func buildTopicName(s *Shim) string {
	if s.Label != "" {
		return "@" + s.Alias + " — " + s.Label
	}

	base := filepath.Base(s.Workdir)
	if base == "" || base == "." || base == "/" {
		return "@" + s.Alias
	}

	return "@" + s.Alias + " — " + base
}

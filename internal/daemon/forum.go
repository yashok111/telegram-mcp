package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	"github.com/yakov/telegram-mcp/internal/access"
)

// forumBot is the subset of bot.Bot the Forum manager calls into. Lets tests
// drop in a fake without standing up a real Telegram API.
type forumBot interface {
	CreateForumTopic(ctx context.Context, chatID int64, name string, iconColor int) (int, error)
	EditForumTopic(ctx context.Context, chatID int64, threadID int, name string) error
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
}

// NewForum constructs a Forum bound to a single Store + forumBot pair. isLive
// reports whether a shim_id is still connected; pass Router.IsConnected so
// reuse can distinguish a live lock holder from a stale one left by a crashed
// or restarted daemon. $HOME resolution is at constructor time — if HOME
// changes mid-process (not expected outside tests), restart the daemon.
func NewForum(store *access.Store, b forumBot, isLive func(shimID string) bool) *Forum {
	home, _ := os.UserHomeDir()
	return &Forum{store: store, bot: b, isLive: isLive, home: home}
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

// AllocateOrReuse picks a forum topic for shim — reusing an existing one
// keyed by label/workdir when free, otherwise creating a fresh topic via
// the Telegram API. Returns the thread_id to use for outbound messages.
//
// Returns (0, nil) when ForumChatID is unset (feature off) — caller treats
// that as "no topic routing for this shim" and falls back to DM-mode.
//
// On parallel collision (reuse_key matches an existing topic still
// LockedBy another shim), creates a fresh topic and logs a Warn. Concurrent
// callers race only inside access.Store.Mutate; the loser sees a non-empty
// LockedBy and falls through to fresh creation.
func (f *Forum) AllocateOrReuse(ctx context.Context, shim *Shim) (int, error) {
	var (
		threadID  int
		forumChat int64
		reuseKey  string
		haveKey   bool
		oldName   string
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

	return tid, nil
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

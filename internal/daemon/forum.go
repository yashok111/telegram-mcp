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
	// home is the workdir treated as the "default" pwd. Sessions started
	// from $HOME or with empty workdir get a fresh topic rather than
	// schclassing into a shared bucket.
	home string
}

// NewForum constructs a Forum bound to a single Store + forumBot pair.
// $HOME resolution is at constructor time — if HOME changes mid-process
// (not expected outside tests), restart the daemon.
func NewForum(store *access.Store, b forumBot) *Forum {
	home, _ := os.UserHomeDir()
	return &Forum{store: store, bot: b, home: home}
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

		if meta.LockedBy != "" && meta.LockedBy != shim.ID {
			slog.Warn("topic locked by another shim — creating fresh",
				"reuse_key", reuseKey, "locked_by", meta.LockedBy, "shim_id", shim.ID, "thread_id", tid)

			return false
		}

		meta.LockedBy = shim.ID
		meta.LastShimID = shim.ID
		st.TopicsByThread[tidStr] = meta
		threadID = tid

		return true
	}); err != nil {
		return 0, fmt.Errorf("forum: lock claim save: %w", err)
	}

	if forumChat == 0 {
		return 0, nil
	}

	if threadID != 0 {
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

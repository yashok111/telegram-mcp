package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/yakov/telegram-mcp/internal/access"
	"github.com/yakov/telegram-mcp/internal/ipc"
)

// topicSpawnRunner is the subset of *SpawnRunner the closer needs. Defined
// here as an interface so tests can drop in a stub without standing up the
// real pty-spawning machinery.
type topicSpawnRunner interface {
	Cancel(id string) error
}

// topicCloseBot is the subset of *bot.Bot the closer needs. Same rationale.
type topicCloseBot interface {
	CloseForumTopic(ctx context.Context, chatID int64, threadID int) error
}

// TopicCloser implements bot.TopicCloser. Wired by main.go after the daemon
// + bot + spawn runner are constructed.
type TopicCloser struct {
	router      *Router
	store       *access.Store
	bot         topicCloseBot
	spawnRunner topicSpawnRunner
	header      *HeaderManager
}

// NewTopicCloser constructs a closer bound to the daemon's collaborators.
// spawnRunner may be nil if the daemon was started with /spawn disabled —
// non-spawned shutdown still works. header may be nil (topic headers disabled);
// CloseTopic's flip-to-🔴 is then a no-op.
func NewTopicCloser(r *Router, store *access.Store, b topicCloseBot, spawn topicSpawnRunner, header *HeaderManager) *TopicCloser {
	return &TopicCloser{
		router:      r,
		store:       store,
		bot:         b,
		spawnRunner: spawn,
		header:      header,
	}
}

// CloseTopic kills the shim that owns threadID (if any), closes the
// Telegram topic, and queues a ClosedTopics entry for the background
// sweep to delete after TELEGRAM_TOPIC_PURGE_AFTER.
//
// Errors from the cancel/notify path are non-fatal — the topic close +
// queue still proceeds. CloseForumTopic API failures surface as the
// return value because the operator likely wants to retry.
func (c *TopicCloser) CloseTopic(ctx context.Context, threadID int) error {
	if threadID <= 0 {
		return errors.New("topic close: invalid thread id")
	}

	st := c.store.Load()
	if st.ForumChatID == 0 {
		return errors.New("topic close: forum disabled (TELEGRAM_FORUM_CHAT_ID unset)")
	}

	shim, ok := c.router.ShimByTopic(threadID)
	switch {
	case !ok:
		slog.Info("topic close: orphan topic (no owning shim)", "thread_id", threadID)
	case shim.SpawnID != "" && c.spawnRunner != nil:
		if err := c.spawnRunner.Cancel(shim.SpawnID); err != nil {
			slog.Warn("topic close: spawn cancel failed (continuing close)",
				"thread_id", threadID, "spawn_id", shim.SpawnID, "err", err)
		}
	default:
		if shim.Notify != nil {
			if err := shim.Notify(ipc.NotifyShutdown, struct{}{}); err != nil {
				slog.Warn("topic close: shutdown notify failed (continuing close)",
					"thread_id", threadID, "shim_id", shim.ID, "err", err)
			}
		}
	}

	if err := c.bot.CloseForumTopic(ctx, st.ForumChatID, threadID); err != nil {
		return fmt.Errorf("close topic %d: %w", threadID, err)
	}

	// Flip the header to 🔴 only after the topic actually closed. The bot is a
	// topic admin, so it can still edit the pinned header in a closed topic.
	c.header.Closed(ctx, threadID)

	if err := c.store.Mutate(func(st *access.State) bool {
		st.ClosedTopics = append(st.ClosedTopics, access.ClosedTopic{
			ThreadID: threadID,
			ClosedAt: time.Now().Unix(),
		})

		return true
	}); err != nil {
		// Topic is closed in Telegram but the purge queue entry was lost.
		// Without queueing, the sweep never deletes it → permanent drift.
		// Surface the error so the operator sees the failure and can retry
		// (CloseForumTopic is idempotent on Telegram's side).
		slog.Error("topic close: ClosedTopics save failed — topic closed in Telegram but not queued for purge",
			"thread_id", threadID, "err", err)

		return fmt.Errorf("close topic %d: queue save (topic closed in Telegram): %w", threadID, err)
	}

	return nil
}

package daemon

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/yakov/telegram-mcp/internal/access"
)

// topicSweepBot is the slice of bot.Bot the sweep needs. Interface lets
// tests run sweep cycles against a stub without spinning up the real
// Telegram API mock.
type topicSweepBot interface {
	DeleteForumTopic(ctx context.Context, chatID int64, threadID int) error
}

// TopicSweep is the background goroutine that reaps closed forum topics
// past their purge TTL. It runs on a ticker; each tick scans
// access.State.ClosedTopics, calls bot.DeleteForumTopic for each entry
// whose ClosedAt is older than purgeAfter, and removes the entry plus
// any TopicsByThread / TopicsByReuseKey references on success.
type TopicSweep struct {
	store        *access.Store
	bot          topicSweepBot
	purgeAfter   time.Duration
	tickInterval time.Duration
}

// NewTopicSweep returns a sweep that ticks at tickInterval and deletes
// ClosedTopics older than purgeAfter. Both durations must be positive; the
// caller should default to a sensible production cadence (e.g., 1h tick,
// 14d TTL).
func NewTopicSweep(store *access.Store, b topicSweepBot, purgeAfter, tickInterval time.Duration) *TopicSweep {
	return &TopicSweep{
		store:        store,
		bot:          b,
		purgeAfter:   purgeAfter,
		tickInterval: tickInterval,
	}
}

// Run blocks until ctx is done, ticking the sweep on every interval. Safe
// to call once per daemon lifetime; ticker handles cancellation cleanly.
func (s *TopicSweep) Run(ctx context.Context) {
	if s.tickInterval <= 0 || s.purgeAfter <= 0 {
		slog.Info("topic sweep disabled (interval or purge_after <= 0)",
			"interval", s.tickInterval, "purge_after", s.purgeAfter)
		return
	}

	t := time.NewTicker(s.tickInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweep(ctx)
		}
	}
}

// sweep performs one purge pass. Exported on the receiver via SweepOnce
// for tests that want deterministic timing instead of the ticker.
func (s *TopicSweep) sweep(ctx context.Context) {
	st := s.store.Load()
	if st.ForumChatID == 0 || len(st.ClosedTopics) == 0 {
		return
	}

	cutoff := time.Now().Add(-s.purgeAfter).Unix()

	var expired []access.ClosedTopic
	for _, ct := range st.ClosedTopics {
		if ct.ClosedAt <= cutoff {
			expired = append(expired, ct)
		}
	}

	if len(expired) == 0 {
		return
	}

	slog.Info("topic sweep: deleting expired topics",
		"count", len(expired), "purge_after", s.purgeAfter)

	for _, ct := range expired {
		if err := s.bot.DeleteForumTopic(ctx, st.ForumChatID, ct.ThreadID); err != nil {
			slog.Warn("topic sweep: delete failed (retain in queue for next tick)",
				"thread_id", ct.ThreadID, "err", err)
			continue
		}

		s.removeFromState(ct.ThreadID)
	}
}

// SweepOnce drives a single purge pass synchronously. Tests use this
// instead of waiting on the ticker.
func (s *TopicSweep) SweepOnce(ctx context.Context) {
	s.sweep(ctx)
}

func (s *TopicSweep) removeFromState(threadID int) {
	if err := s.store.Mutate(func(st *access.State) bool {
		changed := false

		for i, e := range st.ClosedTopics {
			if e.ThreadID == threadID {
				st.ClosedTopics = append(st.ClosedTopics[:i], st.ClosedTopics[i+1:]...)
				changed = true

				break
			}
		}

		tidStr := strconv.Itoa(threadID)
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

		return changed
	}); err != nil {
		slog.Warn("topic sweep: state cleanup save failed", "thread_id", threadID, "err", err)
	}
}

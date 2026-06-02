package daemon

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yakov/telegram-mcp/internal/access"
	"github.com/yakov/telegram-mcp/internal/bot"
)

// orphanCloser closes a forum topic (and queues it for the purge sweep).
// Satisfied by *TopicCloser; an interface so tests need no Telegram API.
type orphanCloser interface {
	CloseTopic(ctx context.Context, threadID int) error
}

// OrphanSweep reaps forum topics whose owning shim disconnected and never came
// back. A released topic (LockedBy=="", ReleasedAt>0) that sits untouched past
// orphanAfter is handed to CloseTopic, which closes it in Telegram and queues it
// for the TopicSweep to delete after the purge TTL. Without this, topics
// accumulate forever: every collision (two live sessions, one workdir) and every
// out-of-band topic deletion mints a fresh topic while the corpse lingers.
type OrphanSweep struct {
	store       *access.Store
	closer      orphanCloser
	orphanAfter time.Duration
	tick        time.Duration
	now         func() time.Time // test seam
}

// NewOrphanSweep returns a sweep that ticks at tick and closes topics released
// longer than orphanAfter ago. Both durations must be positive; Run logs and
// exits otherwise.
func NewOrphanSweep(store *access.Store, closer orphanCloser, orphanAfter, tick time.Duration) *OrphanSweep {
	return &OrphanSweep{
		store:       store,
		closer:      closer,
		orphanAfter: orphanAfter,
		tick:        tick,
		now:         time.Now,
	}
}

// Run blocks until ctx is done, sweeping on every tick.
func (s *OrphanSweep) Run(ctx context.Context) {
	if s.tick <= 0 || s.orphanAfter <= 0 {
		slog.Info("orphan topic sweep disabled (tick or orphan_after <= 0)",
			"tick", s.tick, "orphan_after", s.orphanAfter)

		return
	}

	t := time.NewTicker(s.tick)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.SweepOnce(ctx)
		}
	}
}

// SweepOnce performs one pass: close every topic released past the TTL that
// isn't already locked or queued for purge.
func (s *OrphanSweep) SweepOnce(ctx context.Context) {
	st := s.store.Load()
	if st.ForumChatID == 0 || len(st.TopicsByThread) == 0 {
		return
	}

	cutoff := s.now().Add(-s.orphanAfter).Unix()

	queued := make(map[int]bool, len(st.ClosedTopics))
	for _, ct := range st.ClosedTopics {
		queued[ct.ThreadID] = true
	}

	var candidates []int

	for _, m := range st.TopicsByThread {
		if m.LockedBy == "" && m.ReleasedAt > 0 && m.ReleasedAt <= cutoff && !queued[m.ThreadID] {
			candidates = append(candidates, m.ThreadID)
		}
	}

	if len(candidates) == 0 {
		return
	}

	for _, tid := range candidates {
		// claim re-confirms orphan status atomically and removes the reuse key,
		// so a hello that reattaches to this project between the snapshot above
		// and the close below either wins the lock (claim bails) or gets a fresh
		// topic (its reuse key is gone). Without this a just-reconnected live
		// shim could have its topic closed out from under it.
		if !s.claim(tid, cutoff) {
			continue
		}

		slog.Info("orphan topic sweep: closing topic released past TTL",
			"thread_id", tid, "orphan_after", s.orphanAfter)

		if err := s.closer.CloseTopic(ctx, tid); err != nil {
			if bot.IsPermanentChatError(err) {
				// Already gone in Telegram (deleted out-of-band). CloseTopic can't
				// queue it for purge, so drop the dangling state here instead of
				// retrying the same doomed close every tick.
				slog.Info("orphan topic sweep: target already gone — dropping state",
					"thread_id", tid, "err", err)
				s.dropState(tid)

				continue
			}

			slog.Warn("orphan topic sweep: close failed (retry next tick)",
				"thread_id", tid, "err", err)
		}
	}
}

// CloseDuplicatesOnce closes forum topics that duplicate another topic's
// workdir. A project's canonical topic is the one bound to its workdir reuse
// key; spawn/collision topics adopt a topic:<id> key instead. After a crash or
// reboot the owners of those extra topics never reconnect, so they linger as
// corpses sharing the canonical topic's project-only title — the user can't
// tell live from dead, and a message typed into a corpse triggers a redundant
// auto-spawn. This keeps the canonical topic (a reconnecting shim seizes its
// stale lock) and closes the duplicates.
//
// SAFE ONLY AT STARTUP, before the IPC listener accepts any shim: it assumes no
// shim is connected, so a lock in the loaded state belongs to a previous daemon
// life, never a live owner. Do NOT call it on a running daemon — a legitimate
// second live session in the same workdir would be closed.
func (s *OrphanSweep) CloseDuplicatesOnce(ctx context.Context) {
	st := s.store.Load()
	if st.ForumChatID == 0 || len(st.TopicsByThread) == 0 {
		return
	}

	// canonical = topics bound to a workdir/label reuse key. topic:<id> keys are
	// deliberately excluded — they mark the spawn/collision duplicates we reap.
	canonical := make(map[int]bool)
	canonicalForWorkdir := make(map[string]int)

	for key, tid := range st.TopicsByReuseKey {
		switch {
		case strings.HasPrefix(key, "workdir:"):
			canonicalForWorkdir[strings.TrimPrefix(key, "workdir:")] = tid
			canonical[tid] = true
		case strings.HasPrefix(key, "label:"):
			canonical[tid] = true
		}
	}

	queued := make(map[int]bool, len(st.ClosedTopics))
	for _, ct := range st.ClosedTopics {
		queued[ct.ThreadID] = true
	}

	var dups []int

	for _, m := range st.TopicsByThread {
		if m.Workdir == "" || canonical[m.ThreadID] || queued[m.ThreadID] {
			continue
		}

		if canon, ok := canonicalForWorkdir[m.Workdir]; ok && canon != m.ThreadID {
			dups = append(dups, m.ThreadID)
		}
	}

	if len(dups) == 0 {
		return
	}

	sort.Ints(dups)

	for _, tid := range dups {
		slog.Info("startup dedup: closing duplicate forum topic (project already has a canonical topic)",
			"thread_id", tid)

		s.stripReuseKeys(tid)

		if err := s.closer.CloseTopic(ctx, tid); err != nil {
			if bot.IsPermanentChatError(err) {
				slog.Info("startup dedup: target already gone — dropping state", "thread_id", tid, "err", err)
				s.dropState(tid)

				continue
			}

			slog.Warn("startup dedup: close failed (periodic sweep retries)", "thread_id", tid, "err", err)
		}
	}
}

// stripReuseKeys removes every reuse key pointing at threadID so a later
// reconnect can't chase a topic that's being closed.
func (s *OrphanSweep) stripReuseKeys(threadID int) {
	if err := s.store.Mutate(func(st *access.State) bool {
		changed := false

		for key, tid := range st.TopicsByReuseKey {
			if tid == threadID {
				delete(st.TopicsByReuseKey, key)

				changed = true
			}
		}

		return changed
	}); err != nil {
		slog.Warn("startup dedup: reuse-key strip save failed", "thread_id", threadID, "err", err)
	}
}

// claim atomically re-confirms that threadID is still an orphan (released,
// past-TTL, unlocked) and removes its reuse keys so a concurrent hello can no
// longer reattach to it. Returns false when the topic was re-locked, vanished,
// or is no longer past the cutoff since the sweep's snapshot — in which case it
// must NOT be closed. The store mutex serializes this against the hello path's
// allocateByReuseKey, which sets LockedBy and clears ReleasedAt in one Mutate.
func (s *OrphanSweep) claim(threadID int, cutoff int64) bool {
	claimed := false

	if err := s.store.Mutate(func(st *access.State) bool {
		m, ok := st.TopicsByThread[strconv.Itoa(threadID)]
		if !ok || m.LockedBy != "" || m.ReleasedAt == 0 || m.ReleasedAt > cutoff {
			return false // a hello re-locked it (or it changed) — leave it live
		}

		changed := false

		for key, t := range st.TopicsByReuseKey {
			if t == threadID {
				delete(st.TopicsByReuseKey, key)

				changed = true
			}
		}

		claimed = true

		return changed // persist only if a reuse key was actually removed
	}); err != nil {
		slog.Warn("orphan topic sweep: claim save failed", "thread_id", threadID, "err", err)

		return false
	}

	return claimed
}

// dropState removes a permanently-gone topic from TopicsByThread and any
// reuse-key pointing at it. Sticky alias bindings (AliasByKey) are intentionally
// left: a project keeps its @sN even after its topic is gone, so a later
// reconnect reattaches to the same alias.
func (s *OrphanSweep) dropState(threadID int) {
	if err := s.store.Mutate(func(st *access.State) bool {
		tidStr := strconv.Itoa(threadID)

		_, ok := st.TopicsByThread[tidStr]
		if ok {
			delete(st.TopicsByThread, tidStr)
		}

		changed := ok

		for key, tid := range st.TopicsByReuseKey {
			if tid == threadID {
				delete(st.TopicsByReuseKey, key)

				changed = true
			}
		}

		return changed
	}); err != nil {
		slog.Warn("orphan topic sweep: state cleanup save failed", "thread_id", threadID, "err", err)
	}
}

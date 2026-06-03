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
	// isLive reports whether a shim_id is currently connected. Used by
	// SweepDuplicates to protect a duplicate topic held by a genuine live
	// concurrent session. nil (unwired — startup before the listener accepts,
	// or tests) treats every lock as dead, so dedup still reaps; the daemon
	// wires Router.IsConnected via SetIsLive.
	isLive func(shimID string) bool
}

// SetIsLive wires the liveness oracle SweepDuplicates uses to avoid reaping a
// duplicate topic still held by a connected shim. Pass Router.IsConnected.
func (s *OrphanSweep) SetIsLive(f func(shimID string) bool) {
	s.isLive = f
}

// liveLocked reports whether lockedBy is a currently-connected shim. An empty
// lock is never live; an unwired isLive treats the lock as dead (reapable).
func (s *OrphanSweep) liveLocked(lockedBy string) bool {
	return lockedBy != "" && s.isLive != nil && s.isLive(lockedBy)
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

// Run blocks until ctx is done, sweeping on every tick. Each tick reaps
// duplicate topics (SweepDuplicates) and, when the orphan TTL is enabled,
// long-released orphans (SweepOnce). Duplicate reaping is independent of the
// orphan TTL: disabling the TTL (orphan_after <= 0) keeps idle topics forever
// but still clears duplicates.
func (s *OrphanSweep) Run(ctx context.Context) {
	if s.tick <= 0 {
		slog.Info("topic sweep disabled (tick <= 0)", "tick", s.tick)

		return
	}

	if s.orphanAfter <= 0 {
		slog.Info("orphan-TTL reap disabled (orphan_after <= 0); duplicate reap still active",
			"orphan_after", s.orphanAfter)
	}

	t := time.NewTicker(s.tick)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if s.orphanAfter > 0 {
				s.SweepOnce(ctx)
			}

			s.SweepDuplicates(ctx)
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

// SweepDuplicates closes forum topics that duplicate another topic's workdir.
// A project's canonical topic is the one bound to its workdir reuse key;
// spawn/collision topics carry a topic:<id> key (or none). When a workdir
// briefly hosts two concurrent sessions the daemon correctly mints a second
// topic, but once that extra session disconnects its topic lingers as a corpse
// sharing the canonical topic's project-only title — the user can't tell live
// from dead, and a message typed into a corpse triggers a redundant auto-spawn.
// This keeps the canonical topic and closes the released/dead duplicates.
//
// Safe on a LIVE daemon: a duplicate still held by a connected shim (a genuine
// second concurrent session) is skipped via isLive, so only released or
// crash-orphaned duplicates are reaped. At startup the listener hasn't accepted
// any shim yet, so every lock reads dead and stale-locked corpses are reaped
// too. Called both at startup and on every periodic tick.
func (s *OrphanSweep) SweepDuplicates(ctx context.Context) {
	st := s.store.Load()
	if st.ForumChatID == 0 || len(st.TopicsByThread) == 0 {
		return
	}

	var dups []int

	for _, m := range st.TopicsByThread {
		if s.isReapableDuplicate(&st, m.ThreadID) {
			dups = append(dups, m.ThreadID)
		}
	}

	if len(dups) == 0 {
		return
	}

	sort.Ints(dups)

	for _, tid := range dups {
		// claimDuplicate re-confirms duplicate + not-live-locked status and
		// strips the reuse keys atomically, so a hello racing the close either
		// re-locks first (claim bails) or can no longer chase the closing topic.
		if !s.claimDuplicate(tid) {
			continue
		}

		slog.Info("dedup: closing duplicate forum topic (project already has a canonical topic)",
			"thread_id", tid)

		if err := s.closer.CloseTopic(ctx, tid); err != nil {
			if bot.IsPermanentChatError(err) {
				slog.Info("dedup: target already gone — dropping state", "thread_id", tid, "err", err)
				s.dropState(tid)

				continue
			}

			slog.Warn("dedup: close failed (retry next tick)", "thread_id", tid, "err", err)
		}
	}
}

// isReapableDuplicate reports whether threadID names a forum topic that shares
// another (canonical) topic's workdir and is safe to close now: it has a
// workdir, isn't itself canonical (no workdir:/label: reuse key of its own),
// isn't already queued for purge, and isn't currently held by a live shim. A
// different canonical topic must own its workdir — otherwise it's the sole
// survivor for that project and must be kept.
func (s *OrphanSweep) isReapableDuplicate(st *access.State, threadID int) bool {
	m, ok := st.TopicsByThread[strconv.Itoa(threadID)]
	if !ok || m.Workdir == "" || s.liveLocked(m.LockedBy) {
		return false
	}

	for _, ct := range st.ClosedTopics {
		if ct.ThreadID == threadID {
			return false // already queued for purge
		}
	}

	for key, tid := range st.TopicsByReuseKey {
		if tid == threadID && (strings.HasPrefix(key, "workdir:") || strings.HasPrefix(key, "label:")) {
			return false // canonical itself, never a duplicate
		}
	}

	canon, ok := st.TopicsByReuseKey["workdir:"+m.Workdir]

	return ok && canon != threadID
}

// claimDuplicate re-confirms under the store lock that threadID is still a
// reapable duplicate, then strips every reuse key pointing at it so a racing
// hello can't reattach to the topic about to close. Returns false when the
// topic was re-locked by a live shim, became canonical, or vanished since the
// snapshot — in which case it must NOT be closed. A duplicate with no reuse key
// of its own is still claimed (nothing to strip): the close + purge-queue runs.
func (s *OrphanSweep) claimDuplicate(threadID int) bool {
	claimed := false

	if err := s.store.Mutate(func(st *access.State) bool {
		if !s.isReapableDuplicate(st, threadID) {
			return false
		}

		changed := false

		for key, tid := range st.TopicsByReuseKey {
			if tid == threadID {
				delete(st.TopicsByReuseKey, key)

				changed = true
			}
		}

		claimed = true

		return changed // persist only if a reuse key was actually removed
	}); err != nil {
		slog.Warn("dedup: claim save failed", "thread_id", threadID, "err", err)

		return false
	}

	return claimed
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

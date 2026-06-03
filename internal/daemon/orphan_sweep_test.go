package daemon

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/access"
)

type fakeOrphanCloser struct {
	closed []int
	err    error
}

func (f *fakeOrphanCloser) CloseTopic(_ context.Context, threadID int) error {
	f.closed = append(f.closed, threadID)

	return f.err
}

func seedTopic(t *testing.T, store *access.Store, m access.TopicMeta) {
	t.Helper()
	require.NoError(t, store.Mutate(func(st *access.State) bool {
		st.ForumChatID = -100
		if st.TopicsByThread == nil {
			st.TopicsByThread = map[string]access.TopicMeta{}
		}

		st.TopicsByThread[strconv.Itoa(m.ThreadID)] = m

		return true
	}))
}

// Tracer: a topic whose lock was released longer ago than the orphan TTL is
// handed to CloseTopic so it leaves the forum instead of lingering forever.
func TestOrphanSweep_closesReleasedTopicPastTTL(t *testing.T) {
	store := access.NewStore(t.TempDir(), false)
	seedTopic(t, store, access.TopicMeta{
		ThreadID: 10, LockedBy: "", ReleasedAt: time.Now().Add(-48 * time.Hour).Unix(),
	})

	c := &fakeOrphanCloser{}
	s := NewOrphanSweep(store, c, 24*time.Hour, time.Hour)
	s.SweepOnce(context.Background())

	assert.Equal(t, []int{10}, c.closed, "topic released longer than the TTL is closed")
}

// Only a genuinely-orphaned topic is closed: a locked (live) one, one released
// too recently, one with no release stamp, and one already queued for purge are
// all left alone.
func TestOrphanSweep_skipsLiveRecentUnreleasedAndQueued(t *testing.T) {
	store := access.NewStore(t.TempDir(), false)
	old := time.Now().Add(-48 * time.Hour).Unix()
	recent := time.Now().Add(-1 * time.Hour).Unix()

	seedTopic(t, store, access.TopicMeta{ThreadID: 1, LockedBy: "live-shim"})            // live owner
	seedTopic(t, store, access.TopicMeta{ThreadID: 2, LockedBy: "", ReleasedAt: recent}) // released recently
	seedTopic(t, store, access.TopicMeta{ThreadID: 3, LockedBy: "", ReleasedAt: 0})      // no release stamp
	seedTopic(t, store, access.TopicMeta{ThreadID: 4, LockedBy: "", ReleasedAt: old})    // already queued (below)
	seedTopic(t, store, access.TopicMeta{ThreadID: 5, LockedBy: "", ReleasedAt: old})    // the real orphan
	require.NoError(t, store.Mutate(func(st *access.State) bool {
		st.ClosedTopics = append(st.ClosedTopics, access.ClosedTopic{ThreadID: 4, ClosedAt: old})

		return true
	}))

	c := &fakeOrphanCloser{}
	NewOrphanSweep(store, c, 24*time.Hour, time.Hour).SweepOnce(context.Background())

	assert.Equal(t, []int{5}, c.closed, "only the released, past-TTL, not-yet-queued topic is closed")
}

// The claim step removes a closing orphan's reuse key in the same store
// transaction that confirms it, so a hello racing the close can't reattach to
// the topic about to vanish — it falls through to a fresh topic instead.
func TestOrphanSweep_claimRemovesReuseKeyBeforeClose(t *testing.T) {
	store := access.NewStore(t.TempDir(), false)
	old := time.Now().Add(-48 * time.Hour).Unix()
	seedTopic(t, store, access.TopicMeta{ThreadID: 9, LockedBy: "", ReleasedAt: old, Workdir: "/p/foo"})
	require.NoError(t, store.Mutate(func(st *access.State) bool {
		st.TopicsByReuseKey = map[string]int{"workdir:/p/foo": 9}

		return true
	}))

	c := &fakeOrphanCloser{}
	NewOrphanSweep(store, c, 24*time.Hour, time.Hour).SweepOnce(context.Background())

	assert.Equal(t, []int{9}, c.closed, "orphan closed")
	assert.NotContains(t, store.Load().TopicsByReuseKey, "workdir:/p/foo",
		"reuse key removed under the claim so a racing hello can't reattach to the closing topic")
}

// A topic already deleted in Telegram surfaces TOPIC_ID_INVALID from CloseTopic;
// the sweep drops its state instead of retrying the doomed close forever.
func TestOrphanSweep_permanentErrorDropsState(t *testing.T) {
	store := access.NewStore(t.TempDir(), false)
	old := time.Now().Add(-48 * time.Hour).Unix()
	seedTopic(t, store, access.TopicMeta{ThreadID: 7, LockedBy: "", ReleasedAt: old, Workdir: "/p/foo"})
	require.NoError(t, store.Mutate(func(st *access.State) bool {
		st.TopicsByReuseKey = map[string]int{"workdir:/p/foo": 7}

		return true
	}))

	c := &fakeOrphanCloser{err: errors.New("telego: closeForumTopic: api: 400 \"Bad Request: TOPIC_ID_INVALID\"")}
	NewOrphanSweep(store, c, 24*time.Hour, time.Hour).SweepOnce(context.Background())

	st := store.Load()
	assert.NotContains(t, st.TopicsByThread, "7", "permanently-gone topic dropped from TopicsByThread")
	assert.NotContains(t, st.TopicsByReuseKey, "workdir:/p/foo", "its reuse key dropped too")
}

// A topic with no owner (LockedBy=="") that never got a ReleasedAt stamp —
// legacy state, or a shim that crashed before ReleaseLock ran — is enrolled in
// the orphan TTL clock so the sweep can eventually reap it. Without the stamp it
// is immortal (the sweep requires ReleasedAt>0).
func TestOrphanSweep_StampStuckReleasedOnce_stampsUnreleasedTopic(t *testing.T) {
	store := access.NewStore(t.TempDir(), false)
	seedTopic(t, store, access.TopicMeta{ThreadID: 441, Workdir: "/p/scip", LockedBy: "", ReleasedAt: 0})

	s := NewOrphanSweep(store, &fakeOrphanCloser{}, 24*time.Hour, time.Hour)
	s.now = func() time.Time { return time.Unix(1000, 0) }
	s.StampStuckReleasedOnce()

	assert.Equal(t, int64(1000), store.Load().TopicsByThread["441"].ReleasedAt,
		"stuck unreleased topic enrolled in the orphan TTL clock")
}

// The stamp touches only stuck topics: a live-locked topic and one already
// carrying a ReleasedAt are both left alone.
func TestOrphanSweep_StampStuckReleasedOnce_leavesLockedAndAlreadyStamped(t *testing.T) {
	store := access.NewStore(t.TempDir(), false)
	seedTopic(t, store, access.TopicMeta{ThreadID: 1, LockedBy: "live", ReleasedAt: 0}) // locked → skip
	seedTopic(t, store, access.TopicMeta{ThreadID: 2, LockedBy: "", ReleasedAt: 500})   // stamped → skip

	s := NewOrphanSweep(store, &fakeOrphanCloser{}, 24*time.Hour, time.Hour)
	s.now = func() time.Time { return time.Unix(1000, 0) }
	s.StampStuckReleasedOnce()

	st := store.Load()
	assert.Zero(t, st.TopicsByThread["1"].ReleasedAt, "locked topic not stamped")
	assert.Equal(t, int64(500), st.TopicsByThread["2"].ReleasedAt, "already-stamped topic untouched")
}

// End to end: once stamped, the formerly-immortal topic enters the TTL and the
// normal sweep closes it after orphanAfter elapses.
func TestOrphanSweep_StampStuckReleasedOnce_thenSweepCloses(t *testing.T) {
	store := access.NewStore(t.TempDir(), false)
	seedTopic(t, store, access.TopicMeta{ThreadID: 441, Workdir: "/p/scip", LockedBy: "", ReleasedAt: 0})

	c := &fakeOrphanCloser{}
	s := NewOrphanSweep(store, c, 24*time.Hour, time.Hour)
	stampAt := time.Unix(1000, 0)
	s.now = func() time.Time { return stampAt }
	s.StampStuckReleasedOnce()

	s.now = func() time.Time { return stampAt.Add(48 * time.Hour) } // past the 24h TTL
	s.SweepOnce(context.Background())

	assert.Equal(t, []int{441}, c.closed, "stamped topic closed once past the TTL")
}

// The reboot-corpse bug: one workdir ends up with its canonical (workdir-key)
// topic plus a stale spawn topic (topic:<id> key) sharing the same project
// title. At startup (no shim connected → every lock dead) dedup closes the
// duplicate and keeps the canonical one.
func TestOrphanSweep_SweepDuplicates_closesDuplicateKeepsCanonical(t *testing.T) {
	store := access.NewStore(t.TempDir(), false)
	seedTopic(t, store, access.TopicMeta{ThreadID: 759, Workdir: "/p/tg", LockedBy: "old-canon"})
	seedTopic(t, store, access.TopicMeta{ThreadID: 779, Workdir: "/p/tg", LockedBy: "old-spawn"})
	require.NoError(t, store.Mutate(func(st *access.State) bool {
		st.TopicsByReuseKey = map[string]int{"workdir:/p/tg": 759, "topic:779": 779}

		return true
	}))

	c := &fakeOrphanCloser{}
	NewOrphanSweep(store, c, 24*time.Hour, time.Hour).SweepDuplicates(context.Background())

	assert.Equal(t, []int{779}, c.closed, "only the non-canonical duplicate is closed")

	st := store.Load()
	assert.Equal(t, 759, st.TopicsByReuseKey["workdir:/p/tg"], "canonical reuse key untouched")
	assert.NotContains(t, st.TopicsByReuseKey, "topic:779", "duplicate's reuse key stripped")
}

// No duplicate to reap: each workdir owns exactly one canonical topic, and a
// label-keyed topic (admin) is never treated as a duplicate of its workdir.
func TestOrphanSweep_SweepDuplicates_noopWithoutDuplicate(t *testing.T) {
	store := access.NewStore(t.TempDir(), false)
	seedTopic(t, store, access.TopicMeta{ThreadID: 631, Workdir: "/p/vpn", LockedBy: "x"})
	seedTopic(t, store, access.TopicMeta{ThreadID: 427, Workdir: "/home/y", Label: "admin", LockedBy: "adm"})
	require.NoError(t, store.Mutate(func(st *access.State) bool {
		st.TopicsByReuseKey = map[string]int{"workdir:/p/vpn": 631, "label:admin": 427, "workdir:/home/y": 800}

		return true
	}))

	c := &fakeOrphanCloser{}
	NewOrphanSweep(store, c, 24*time.Hour, time.Hour).SweepDuplicates(context.Background())

	assert.Empty(t, c.closed, "no non-canonical duplicate exists")
}

func TestOrphanSweep_SweepDuplicates_skipsForumOff(t *testing.T) {
	store := access.NewStore(t.TempDir(), false)
	require.NoError(t, store.Mutate(func(st *access.State) bool {
		st.ForumChatID = 0
		st.TopicsByThread = map[string]access.TopicMeta{
			"779": {ThreadID: 779, Workdir: "/p/tg"},
		}
		st.TopicsByReuseKey = map[string]int{"workdir:/p/tg": 759, "topic:779": 779}

		return true
	}))

	c := &fakeOrphanCloser{}
	NewOrphanSweep(store, c, 24*time.Hour, time.Hour).SweepDuplicates(context.Background())

	assert.Empty(t, c.closed, "forum mode off → no dedup")
}

// A duplicate currently held by a live shim (a genuine second concurrent
// session in the same workdir) is NOT reaped — closing it would kill that
// session's topic mid-conversation. This is what makes SweepDuplicates safe to
// run on a live daemon, not just at startup.
func TestOrphanSweep_SweepDuplicates_keepsLiveLockedDuplicate(t *testing.T) {
	store := access.NewStore(t.TempDir(), false)
	seedTopic(t, store, access.TopicMeta{ThreadID: 100, Workdir: "/p/x", LockedBy: "canon-live"})
	seedTopic(t, store, access.TopicMeta{ThreadID: 101, Workdir: "/p/x", LockedBy: "dup-live"})
	require.NoError(t, store.Mutate(func(st *access.State) bool {
		st.TopicsByReuseKey = map[string]int{"workdir:/p/x": 100}

		return true
	}))

	c := &fakeOrphanCloser{}
	s := NewOrphanSweep(store, c, 24*time.Hour, time.Hour)
	s.SetIsLive(func(id string) bool { return id == "dup-live" })
	s.SweepDuplicates(context.Background())

	assert.Empty(t, c.closed, "a duplicate held by a live shim is left alone")
}

// Once the duplicate's owner disconnects (lock released), the next sweep closes
// it within the tick interval instead of waiting out the multi-day orphan TTL.
func TestOrphanSweep_SweepDuplicates_closesReleasedDuplicate(t *testing.T) {
	store := access.NewStore(t.TempDir(), false)
	seedTopic(t, store, access.TopicMeta{ThreadID: 100, Workdir: "/p/x", LockedBy: "canon-live"})
	seedTopic(t, store, access.TopicMeta{ThreadID: 101, Workdir: "/p/x", LockedBy: "", ReleasedAt: time.Now().Unix()})
	require.NoError(t, store.Mutate(func(st *access.State) bool {
		st.TopicsByReuseKey = map[string]int{"workdir:/p/x": 100}

		return true
	}))

	c := &fakeOrphanCloser{}
	s := NewOrphanSweep(store, c, 24*time.Hour, time.Hour)
	s.SetIsLive(func(string) bool { return false })
	s.SweepDuplicates(context.Background())

	assert.Equal(t, []int{101}, c.closed, "released duplicate closed promptly, not after the 7d TTL")
	assert.Equal(t, 100, store.Load().TopicsByReuseKey["workdir:/p/x"], "canonical reuse key untouched")
}

// A duplicate left locked by a shim that never reconnected (crash — ReleaseLock
// never ran) is still reaped: the lock holder isn't connected, so the topic is
// dead and unreachable.
func TestOrphanSweep_SweepDuplicates_closesDeadLockedDuplicate(t *testing.T) {
	store := access.NewStore(t.TempDir(), false)
	seedTopic(t, store, access.TopicMeta{ThreadID: 100, Workdir: "/p/x", LockedBy: "canon"})
	seedTopic(t, store, access.TopicMeta{ThreadID: 101, Workdir: "/p/x", LockedBy: "dead-shim"})
	require.NoError(t, store.Mutate(func(st *access.State) bool {
		st.TopicsByReuseKey = map[string]int{"workdir:/p/x": 100}

		return true
	}))

	c := &fakeOrphanCloser{}
	s := NewOrphanSweep(store, c, 24*time.Hour, time.Hour)
	s.SetIsLive(func(string) bool { return false }) // nobody connected
	s.SweepDuplicates(context.Background())

	assert.Equal(t, []int{101}, c.closed, "dead-locked duplicate reaped (lock holder gone)")
}

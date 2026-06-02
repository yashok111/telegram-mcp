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

// The reboot-corpse bug: one workdir ends up with its canonical (workdir-key)
// topic plus a stale spawn topic (topic:<id> key) sharing the same project
// title. Startup dedup closes the duplicate and keeps the canonical one.
func TestOrphanSweep_CloseDuplicatesOnce_closesDuplicateKeepsCanonical(t *testing.T) {
	store := access.NewStore(t.TempDir(), false)
	seedTopic(t, store, access.TopicMeta{ThreadID: 759, Workdir: "/p/tg", LockedBy: "old-canon"})
	seedTopic(t, store, access.TopicMeta{ThreadID: 779, Workdir: "/p/tg", LockedBy: "old-spawn"})
	require.NoError(t, store.Mutate(func(st *access.State) bool {
		st.TopicsByReuseKey = map[string]int{"workdir:/p/tg": 759, "topic:779": 779}

		return true
	}))

	c := &fakeOrphanCloser{}
	NewOrphanSweep(store, c, 24*time.Hour, time.Hour).CloseDuplicatesOnce(context.Background())

	assert.Equal(t, []int{779}, c.closed, "only the non-canonical duplicate is closed")

	st := store.Load()
	assert.Equal(t, 759, st.TopicsByReuseKey["workdir:/p/tg"], "canonical reuse key untouched")
	assert.NotContains(t, st.TopicsByReuseKey, "topic:779", "duplicate's reuse key stripped")
}

// No duplicate to reap: each workdir owns exactly one canonical topic, and a
// label-keyed topic (admin) is never treated as a duplicate of its workdir.
func TestOrphanSweep_CloseDuplicatesOnce_noopWithoutDuplicate(t *testing.T) {
	store := access.NewStore(t.TempDir(), false)
	seedTopic(t, store, access.TopicMeta{ThreadID: 631, Workdir: "/p/vpn", LockedBy: "x"})
	seedTopic(t, store, access.TopicMeta{ThreadID: 427, Workdir: "/home/y", Label: "admin", LockedBy: "adm"})
	require.NoError(t, store.Mutate(func(st *access.State) bool {
		st.TopicsByReuseKey = map[string]int{"workdir:/p/vpn": 631, "label:admin": 427, "workdir:/home/y": 800}

		return true
	}))

	c := &fakeOrphanCloser{}
	NewOrphanSweep(store, c, 24*time.Hour, time.Hour).CloseDuplicatesOnce(context.Background())

	assert.Empty(t, c.closed, "no non-canonical duplicate exists")
}

func TestOrphanSweep_CloseDuplicatesOnce_skipsForumOff(t *testing.T) {
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
	NewOrphanSweep(store, c, 24*time.Hour, time.Hour).CloseDuplicatesOnce(context.Background())

	assert.Empty(t, c.closed, "forum mode off → no dedup")
}

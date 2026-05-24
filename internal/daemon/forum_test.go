package daemon

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/access"
)

// fakeForumBot records createForumTopic calls and returns auto-incrementing
// thread IDs.
type fakeForumBot struct {
	mu sync.Mutex

	createName  []string
	createColor []int

	editThreadID []int
	editName     []string

	// nextID is the thread_id that the next CreateForumTopic call returns.
	nextID atomic.Int32

	// failCreate, when non-nil, makes CreateForumTopic return the error
	// without recording the call.
	failCreate error

	// failEdit, when non-nil, makes EditForumTopic return the error without
	// recording the call.
	failEdit error
}

func newFakeForumBot() *fakeForumBot {
	f := &fakeForumBot{}
	f.nextID.Store(100)

	return f
}

func (f *fakeForumBot) CreateForumTopic(_ context.Context, _ int64, name string, color int) (int, error) {
	if f.failCreate != nil {
		return 0, f.failCreate
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.createName = append(f.createName, name)
	f.createColor = append(f.createColor, color)

	return int(f.nextID.Add(1)), nil
}

func (f *fakeForumBot) EditForumTopic(_ context.Context, _ int64, threadID int, name string) error {
	if f.failEdit != nil {
		return f.failEdit
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.editThreadID = append(f.editThreadID, threadID)
	f.editName = append(f.editName, name)

	return nil
}

func newForumFixture(t *testing.T, forumChatID int64) (*Forum, *access.Store, *fakeForumBot) {
	t.Helper()

	dir := t.TempDir()
	store := access.NewStore(dir, false)
	st := access.State{
		DMPolicy:    access.PolicyAllowlist,
		AllowFrom:   []string{"123"},
		Groups:      map[string]access.GroupPolicy{},
		Pending:     map[string]access.Pending{},
		ForumChatID: forumChatID,
	}
	require.NoError(t, store.Save(st))

	fb := newFakeForumBot()
	// Default liveness: treat every lock holder as still connected, so reuse
	// keeps the historical "locked → fresh" behavior unless a test opts a
	// shim_id out of the live set to exercise stale-lock seizing.
	f := NewForum(store, fb, func(string) bool { return true })
	// Force a non-HOME workdir comparison by overriding home — tests pass
	// explicit workdirs and verify whether reuse triggers.
	f.home = "/test/home"

	return f, store, fb
}

func TestForum_disabled_whenForumChatIDZero(t *testing.T) {
	f, _, fb := newForumFixture(t, 0)

	threadID, err := f.AllocateOrReuse(context.Background(), &Shim{
		ID: "shim-a", Alias: "s1", Workdir: "/projects/foo",
	})
	require.NoError(t, err)
	assert.Zero(t, threadID, "ForumChatID=0 → 0 thread")
	assert.Empty(t, fb.createName, "no topic should have been created")
}

func TestForum_createsFreshTopic_withoutReuseKey(t *testing.T) {
	f, store, fb := newForumFixture(t, -100123)

	threadID, err := f.AllocateOrReuse(context.Background(), &Shim{
		ID: "shim-a", Alias: "s1", Workdir: "/test/home",
	})
	require.NoError(t, err)
	assert.Positive(t, threadID)
	require.Len(t, fb.createName, 1, "topic created")
	// $HOME workdir → no reuse key registered (label-less + workdir==home)
	assert.Empty(t, store.Load().TopicsByReuseKey, "no reuse key for $HOME workdir")
}

func TestForum_createsFreshTopic_registersLabelKey(t *testing.T) {
	f, store, fb := newForumFixture(t, -100123)

	shim := &Shim{ID: "shim-a", Alias: "s1", Label: "foo", Workdir: "/test/home"}

	threadID, err := f.AllocateOrReuse(context.Background(), shim)
	require.NoError(t, err)
	assert.Positive(t, threadID)
	require.Len(t, fb.createName, 1)
	assert.Equal(t, "@s1 — foo", fb.createName[0])

	st := store.Load()
	assert.Equal(t, threadID, st.TopicsByReuseKey["label:foo"])
	tidStr := strconv.Itoa(threadID)
	meta := st.TopicsByThread[tidStr]
	assert.Equal(t, "shim-a", meta.LockedBy)
	assert.Equal(t, "foo", meta.Label)
}

func TestForum_reusesByLabel_whenLockFree(t *testing.T) {
	f, _, fb := newForumFixture(t, -100123)

	firstShim := &Shim{ID: "shim-old", Alias: "s1", Label: "foo", Workdir: "/projects/foo"}
	first, err := f.AllocateOrReuse(context.Background(), firstShim)
	require.NoError(t, err)

	// Release the lock as if the first shim disconnected.
	f.ReleaseLock("shim-old")

	secondShim := &Shim{ID: "shim-new", Alias: "s2", Label: "foo", Workdir: "/projects/foo"}
	second, err := f.AllocateOrReuse(context.Background(), secondShim)
	require.NoError(t, err)
	assert.Equal(t, first, second, "same label → same thread_id")
	assert.Len(t, fb.createName, 1, "no fresh topic created on reuse")
	assert.Len(t, fb.editName, 1, "alias s1→s2 on reuse → name resynced")
}

// TestForum_reuseResyncsTopicName covers the alias-migration bug: a topic
// name is frozen at CreateForumTopic. When the creating shim disconnects and
// the topic is reused by a shim carrying a different alias, the name must be
// re-pushed via EditForumTopic so the topic list stays distinguishable.
func TestForum_reuseResyncsTopicName(t *testing.T) {
	f, store, fb := newForumFixture(t, -100123)

	first, err := f.AllocateOrReuse(context.Background(), &Shim{
		ID: "shim-old", Alias: "s1", Label: "foo", Workdir: "/projects/foo",
	})
	require.NoError(t, err)
	require.Len(t, fb.createName, 1, "topic created")
	assert.Equal(t, "@s1 — foo", fb.createName[0])

	f.ReleaseLock("shim-old")

	second, err := f.AllocateOrReuse(context.Background(), &Shim{
		ID: "shim-new", Alias: "s2", Label: "foo", Workdir: "/projects/foo",
	})
	require.NoError(t, err)
	assert.Equal(t, first, second, "reuse keeps the thread")
	assert.Len(t, fb.createName, 1, "no fresh topic on reuse")

	require.Len(t, fb.editName, 1, "diverged alias → one EditForumTopic")
	assert.Equal(t, "@s2 — foo", fb.editName[0], "name resynced to new alias")
	assert.Equal(t, second, fb.editThreadID[0], "edit targets the reused thread")
	assert.Equal(t, "@s2 — foo", store.Load().TopicsByThread[strconv.Itoa(second)].Name,
		"resynced name persisted")
}

// TestForum_reuseSameAlias_noResync guards against a spurious API call when
// the reattaching shim carries the same alias as the stored name.
func TestForum_reuseSameAlias_noResync(t *testing.T) {
	f, _, fb := newForumFixture(t, -100123)

	_, err := f.AllocateOrReuse(context.Background(), &Shim{
		ID: "shim-a", Alias: "s1", Label: "foo", Workdir: "/projects/foo",
	})
	require.NoError(t, err)

	f.ReleaseLock("shim-a")

	_, err = f.AllocateOrReuse(context.Background(), &Shim{
		ID: "shim-a2", Alias: "s1", Label: "foo", Workdir: "/projects/foo",
	})
	require.NoError(t, err)
	assert.Empty(t, fb.editName, "same alias → no EditForumTopic")
}

// TestForum_resyncFailure_stillReturnsThread asserts a cosmetic resync
// failure does not drop the shim to DM-mode: AllocateOrReuse returns the
// reused thread with no error even when EditForumTopic fails.
func TestForum_resyncFailure_stillReturnsThread(t *testing.T) {
	f, store, fb := newForumFixture(t, -100123)

	first, err := f.AllocateOrReuse(context.Background(), &Shim{
		ID: "shim-old", Alias: "s1", Label: "foo", Workdir: "/projects/foo",
	})
	require.NoError(t, err)

	f.ReleaseLock("shim-old")

	// A genuinely-propagating edit failure (TOPIC_NOT_MODIFIED is swallowed as
	// idempotent success in bot.EditForumTopic, so it never reaches here).
	fb.failEdit = errors.New("Bad Request: message thread not found")

	second, err := f.AllocateOrReuse(context.Background(), &Shim{
		ID: "shim-new", Alias: "s2", Label: "foo", Workdir: "/projects/foo",
	})
	require.NoError(t, err, "cosmetic resync failure must not abort the attach")
	assert.Equal(t, first, second, "reused thread still returned")
	assert.Equal(t, "@s1 — foo", store.Load().TopicsByThread[strconv.Itoa(second)].Name,
		"stored name unchanged when edit fails — next reuse retries")
}

func TestForum_reusesByWorkdir_whenNotHome(t *testing.T) {
	f, _, fb := newForumFixture(t, -100123)

	firstShim := &Shim{ID: "shim-old", Alias: "s1", Workdir: "/projects/foo"}
	first, err := f.AllocateOrReuse(context.Background(), firstShim)
	require.NoError(t, err)

	f.ReleaseLock("shim-old")

	secondShim := &Shim{ID: "shim-new", Alias: "s2", Workdir: "/projects/foo"}
	second, err := f.AllocateOrReuse(context.Background(), secondShim)
	require.NoError(t, err)
	assert.Equal(t, first, second, "same workdir → same thread_id")
	assert.Len(t, fb.createName, 1)
}

func TestForum_skipsReuse_forHomeWorkdir(t *testing.T) {
	f, _, fb := newForumFixture(t, -100123)

	for range 3 {
		_, err := f.AllocateOrReuse(context.Background(), &Shim{
			ID: "shim-" + strconv.Itoa(len(fb.createName)), Alias: "s1", Workdir: "/test/home",
		})
		require.NoError(t, err)
	}

	assert.Len(t, fb.createName, 3, "$HOME workdir → fresh topic every time")
}

func TestForum_collisionWhenLockHeldByLiveShim_createsFresh(t *testing.T) {
	f, _, fb := newForumFixture(t, -100123) // default fixture: every holder live

	first, err := f.AllocateOrReuse(context.Background(), &Shim{
		ID: "shim-a", Alias: "s1", Workdir: "/projects/foo",
	})
	require.NoError(t, err)

	// shim-a still locked AND still connected; concurrent shim with same key
	// arrives. The live lock is not seized → fresh topic.
	second, err := f.AllocateOrReuse(context.Background(), &Shim{
		ID: "shim-b", Alias: "s2", Workdir: "/projects/foo",
	})
	require.NoError(t, err)
	assert.NotEqual(t, first, second, "live lock collision → fresh topic")
	assert.Len(t, fb.createName, 2)
}

// TestForum_seizesStaleLock_fromDisconnectedShim is the core regression test
// for the duplicate-topic-on-restart bug. A shim locks a workdir topic, then
// its daemon dies without releasing the lock (SIGKILL/OOM → OnDisconnect never
// runs). On restart the shim reconnects under a fresh, random shim_id while
// the old id rots in access.json. The reattaching shim must detect the holder
// is no longer connected, seize the stale lock, and REUSE the existing topic
// instead of creating a duplicate.
func TestForum_seizesStaleLock_fromDisconnectedShim(t *testing.T) {
	f, store, fb := newForumFixture(t, -100123)

	first, err := f.AllocateOrReuse(context.Background(), &Shim{
		ID: "shim-dead", Alias: "s1", Workdir: "/projects/foo",
	})
	require.NoError(t, err)
	require.Len(t, fb.createName, 1)

	// Lock still held by shim-dead on disk, but it is not among the connected
	// shims (its daemon crashed before ReleaseLock).
	f.isLive = func(id string) bool { return id != "shim-dead" }

	second, err := f.AllocateOrReuse(context.Background(), &Shim{
		ID: "shim-new", Alias: "s2", Workdir: "/projects/foo",
	})
	require.NoError(t, err)

	assert.Equal(t, first, second, "stale lock seized → existing thread reused, not duplicated")
	assert.Len(t, fb.createName, 1, "no fresh topic created when seizing a stale lock")
	assert.Equal(t, "shim-new", store.Load().TopicsByThread[strconv.Itoa(second)].LockedBy,
		"lock transferred to the reattaching shim")
}

func TestForum_ReleaseLock_clearsLockedBy(t *testing.T) {
	f, store, _ := newForumFixture(t, -100123)

	threadID, err := f.AllocateOrReuse(context.Background(), &Shim{
		ID: "shim-a", Alias: "s1", Label: "foo",
	})
	require.NoError(t, err)

	tidStr := strconv.Itoa(threadID)
	assert.Equal(t, "shim-a", store.Load().TopicsByThread[tidStr].LockedBy)

	f.ReleaseLock("shim-a")
	assert.Empty(t, store.Load().TopicsByThread[tidStr].LockedBy, "lock cleared")
}

// TestForum_concurrentCreateRace_keepsLastWinner exercises the race window
// in AllocateOrReuse: two callers with the same reuse_key both find no
// existing mapping in the first Mutate, both call CreateForumTopic in
// parallel, both register in the second Mutate. The later writer wins
// TopicsByReuseKey; the earlier topic is left tracked in TopicsByThread
// as an orphan (sweep in Wave 5 reaps it). This documents the rare
// single-user race rather than fixing it.
func TestForum_concurrentCreateRace_keepsLastWinner(t *testing.T) {
	f, store, fb := newForumFixture(t, -100123)

	// Simulate a race by sequentially driving two AllocateOrReuse calls
	// against the same label, releasing the lock between them so neither
	// blocks on LockedBy. The second call's second-Mutate overwrites
	// TopicsByReuseKey with its fresh thread_id, leaving the first
	// thread_id as an orphan entry.
	first, err := f.AllocateOrReuse(context.Background(), &Shim{
		ID: "shim-a", Alias: "s1", Label: "race",
	})
	require.NoError(t, err)

	// Manually delete the reuse key (simulating the race window) before
	// the second call so it doesn't reuse — but leave TopicsByThread alone.
	require.NoError(t, store.Mutate(func(st *access.State) bool {
		delete(st.TopicsByReuseKey, "label:race")
		return true
	}))

	second, err := f.AllocateOrReuse(context.Background(), &Shim{
		ID: "shim-b", Alias: "s2", Label: "race",
	})
	require.NoError(t, err)

	assert.NotEqual(t, first, second, "two fresh topics created")
	assert.Len(t, fb.createName, 2)

	st := store.Load()
	assert.Equal(t, second, st.TopicsByReuseKey["label:race"], "last writer wins reuse_key")
	assert.Contains(t, st.TopicsByThread, strconv.Itoa(first), "loser thread remains tracked (sweep reaps)")
	assert.Contains(t, st.TopicsByThread, strconv.Itoa(second), "winner thread tracked")
}

func TestForum_ReleaseLock_unknownShimIsNoop(t *testing.T) {
	f, store, _ := newForumFixture(t, -100123)

	_, err := f.AllocateOrReuse(context.Background(), &Shim{
		ID: "shim-a", Alias: "s1", Label: "foo",
	})
	require.NoError(t, err)

	before := store.Load().TopicsByThread

	f.ReleaseLock("nonexistent")

	after := store.Load().TopicsByThread
	assert.Equal(t, before, after, "unknown shim release is no-op")
}

func TestForum_createFailure_propagates(t *testing.T) {
	f, _, fb := newForumFixture(t, -100123)
	fb.failCreate = errors.New("Forbidden: can_manage_topics")

	_, err := f.AllocateOrReuse(context.Background(), &Shim{
		ID: "shim-a", Alias: "s1", Label: "foo",
	})
	require.Error(t, err)
}

func TestBuildTopicName_labelWins(t *testing.T) {
	assert.Equal(t, "@s1 — foo", buildTopicName(&Shim{Alias: "s1", Label: "foo", Workdir: "/projects/bar"}))
}

func TestBuildTopicName_workdirBasename(t *testing.T) {
	assert.Equal(t, "@s1 — bar", buildTopicName(&Shim{Alias: "s1", Workdir: "/projects/bar"}))
}

func TestBuildTopicName_bareAlias_whenWorkdirIsRoot(t *testing.T) {
	assert.Equal(t, "@s1", buildTopicName(&Shim{Alias: "s1", Workdir: "/"}))
}

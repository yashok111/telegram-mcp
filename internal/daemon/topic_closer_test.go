package daemon

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/access"
	"github.com/yakov/telegram-mcp/internal/ipc"
)

type fakeCloseBot struct {
	mu       sync.Mutex
	closed   []int
	failNext error
}

func (f *fakeCloseBot) CloseForumTopic(_ context.Context, _ int64, threadID int) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil

		return err
	}

	f.closed = append(f.closed, threadID)

	return nil
}

type fakeSpawnRunner struct {
	mu        sync.Mutex
	cancelled []string
	failErr   error
}

func (f *fakeSpawnRunner) Cancel(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.failErr != nil {
		return f.failErr
	}

	f.cancelled = append(f.cancelled, id)

	return nil
}

func newCloserFixture(t *testing.T) (*TopicCloser, *Router, *access.Store, *fakeCloseBot, *fakeSpawnRunner) {
	t.Helper()

	dir := t.TempDir()
	store := access.NewStore(dir, false)
	require.NoError(t, store.Save(access.State{
		DMPolicy:    access.PolicyAllowlist,
		AllowFrom:   []string{"123"},
		Groups:      map[string]access.GroupPolicy{},
		Pending:     map[string]access.Pending{},
		ForumChatID: -100123,
	}))

	router := NewRouter()
	bot := &fakeCloseBot{}
	spawn := &fakeSpawnRunner{}
	closer := NewTopicCloser(router, store, bot, spawn)

	return closer, router, store, bot, spawn
}

func TestTopicCloser_invalidThreadID(t *testing.T) {
	c, _, _, _, _ := newCloserFixture(t)
	require.Error(t, c.CloseTopic(context.Background(), 0))
	require.Error(t, c.CloseTopic(context.Background(), -1))
}

func TestTopicCloser_forumDisabled(t *testing.T) {
	dir := t.TempDir()
	store := access.NewStore(dir, false)
	require.NoError(t, store.Save(access.State{ForumChatID: 0}))

	c := NewTopicCloser(NewRouter(), store, &fakeCloseBot{}, &fakeSpawnRunner{})
	require.Error(t, c.CloseTopic(context.Background(), 42))
}

func TestTopicCloser_orphanTopic_closesAndQueues(t *testing.T) {
	c, _, store, bot, spawn := newCloserFixture(t)

	require.NoError(t, c.CloseTopic(context.Background(), 42))

	assert.Equal(t, []int{42}, bot.closed)
	assert.Empty(t, spawn.cancelled, "no shim → no spawn cancel")

	st := store.Load()
	require.Len(t, st.ClosedTopics, 1)
	assert.Equal(t, 42, st.ClosedTopics[0].ThreadID)
	assert.Positive(t, st.ClosedTopics[0].ClosedAt)
}

func TestTopicCloser_spawnedShim_cancelsViaSpawnRunner(t *testing.T) {
	c, router, _, _, spawn := newCloserFixture(t)

	router.Register(&Shim{
		ID:      "shim-a",
		SpawnID: "spawn-xyz",
		Notify:  func(string, any) error { return nil },
	})
	router.BindTopic("shim-a", 42)

	require.NoError(t, c.CloseTopic(context.Background(), 42))

	assert.Equal(t, []string{"spawn-xyz"}, spawn.cancelled, "spawned shim → SpawnRunner.Cancel")
}

func TestTopicCloser_nonSpawnedShim_sendsShutdownNotify(t *testing.T) {
	c, router, _, _, spawn := newCloserFixture(t)

	var (
		notifyCount  atomic.Int32
		notifyMethod atomic.Value
	)

	router.Register(&Shim{
		ID: "shim-a",
		Notify: func(method string, _ any) error {
			notifyCount.Add(1)
			notifyMethod.Store(method)

			return nil
		},
	})
	router.BindTopic("shim-a", 42)

	require.NoError(t, c.CloseTopic(context.Background(), 42))

	assert.EqualValues(t, 1, notifyCount.Load(), "non-spawned shim → exactly one Notify")
	assert.Equal(t, ipc.NotifyShutdown, notifyMethod.Load())
	assert.Empty(t, spawn.cancelled)
}

func TestTopicCloser_closeForumTopicFailureBubblesUp(t *testing.T) {
	c, _, _, bot, _ := newCloserFixture(t)
	bot.failNext = errors.New("Forbidden: can_manage_topics revoked")

	err := c.CloseTopic(context.Background(), 42)
	require.Error(t, err)
}

func TestTopicCloser_storeMutateFailure_returnsError(t *testing.T) {
	// Setup a real store, then chmod its dir read-only so the post-close
	// Mutate's saveLocked fails on WriteFile. CloseForumTopic still
	// succeeds (fake bot) — exercising the "closed in TG but queue save
	// failed" branch.
	dir := t.TempDir()
	store := access.NewStore(dir, false)
	require.NoError(t, store.Save(access.State{
		DMPolicy:    access.PolicyAllowlist,
		AllowFrom:   []string{"123"},
		Groups:      map[string]access.GroupPolicy{},
		Pending:     map[string]access.Pending{},
		ForumChatID: -100123,
	}))

	bot := &fakeCloseBot{}
	c := NewTopicCloser(NewRouter(), store, bot, &fakeSpawnRunner{})

	require.NoError(t, os.Chmod(dir, 0o500), "drop write perm on store dir")
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := c.CloseTopic(context.Background(), 42)
	require.Error(t, err, "Mutate failure must surface to caller")
	assert.Contains(t, err.Error(), "topic closed in Telegram",
		"error message must indicate the partial-success state")
	assert.Equal(t, []int{42}, bot.closed, "Telegram-side close still happened")
}

func TestTopicCloser_spawnCancelFailure_continuesClose(t *testing.T) {
	c, router, _, bot, spawn := newCloserFixture(t)
	spawn.failErr = errors.New("spawn already gone")

	router.Register(&Shim{
		ID:      "shim-a",
		SpawnID: "spawn-xyz",
		Notify:  func(string, any) error { return nil },
	})
	router.BindTopic("shim-a", 42)

	require.NoError(t, c.CloseTopic(context.Background(), 42), "spawn cancel error is non-fatal")
	assert.Equal(t, []int{42}, bot.closed, "topic still closed")
}

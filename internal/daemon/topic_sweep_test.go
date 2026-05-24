package daemon

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/access"
)

type fakeSweepBot struct {
	mu        sync.Mutex
	deleted   []int
	failFirst bool
}

func (f *fakeSweepBot) DeleteForumTopic(_ context.Context, _ int64, threadID int) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.failFirst {
		f.failFirst = false
		return errors.New("Forbidden: not admin")
	}

	f.deleted = append(f.deleted, threadID)

	return nil
}

func newSweepFixture(t *testing.T, purgeAfter time.Duration) (*TopicSweep, *access.Store, *fakeSweepBot) {
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

	b := &fakeSweepBot{}
	s := NewTopicSweep(store, b, purgeAfter, time.Hour)

	return s, store, b
}

func TestTopicSweep_disabledWhenForumChatIDZero(t *testing.T) {
	dir := t.TempDir()
	store := access.NewStore(dir, false)
	require.NoError(t, store.Save(access.State{ForumChatID: 0}))

	b := &fakeSweepBot{}
	s := NewTopicSweep(store, b, 1*time.Second, time.Hour)
	s.SweepOnce(context.Background())

	assert.Empty(t, b.deleted, "ForumChatID=0 → sweep is a no-op")
}

func TestTopicSweep_emptyClosedTopics_noop(t *testing.T) {
	s, _, b := newSweepFixture(t, 1*time.Second)
	s.SweepOnce(context.Background())
	assert.Empty(t, b.deleted)
}

func TestTopicSweep_deletesExpiredAndCleansState(t *testing.T) {
	s, store, b := newSweepFixture(t, 1*time.Second)

	old := time.Now().Add(-10 * time.Second).Unix()
	fresh := time.Now().Unix()

	require.NoError(t, store.Mutate(func(st *access.State) bool {
		st.ClosedTopics = []access.ClosedTopic{
			{ThreadID: 42, ClosedAt: old},
			{ThreadID: 99, ClosedAt: fresh},
		}
		st.TopicsByThread = map[string]access.TopicMeta{
			"42": {ThreadID: 42, Label: "foo"},
			"99": {ThreadID: 99, Label: "bar"},
		}
		st.TopicsByReuseKey = map[string]int{
			"label:foo": 42,
			"label:bar": 99,
		}

		return true
	}))

	s.SweepOnce(context.Background())

	assert.Equal(t, []int{42}, b.deleted, "only expired thread deleted")

	st := store.Load()
	require.Len(t, st.ClosedTopics, 1)
	assert.Equal(t, 99, st.ClosedTopics[0].ThreadID, "fresh entry retained")

	assert.NotContains(t, st.TopicsByThread, "42")
	assert.Contains(t, st.TopicsByThread, "99")
	assert.NotContains(t, st.TopicsByReuseKey, "label:foo")
	assert.Contains(t, st.TopicsByReuseKey, "label:bar")
}

func TestTopicSweep_deleteFailureRetainsInQueue(t *testing.T) {
	s, store, b := newSweepFixture(t, 1*time.Second)
	b.failFirst = true

	old := time.Now().Add(-10 * time.Second).Unix()

	require.NoError(t, store.Mutate(func(st *access.State) bool {
		st.ClosedTopics = []access.ClosedTopic{
			{ThreadID: 42, ClosedAt: old},
		}
		st.TopicsByThread = map[string]access.TopicMeta{"42": {ThreadID: 42}}

		return true
	}))

	s.SweepOnce(context.Background())

	// Telegram returned Forbidden; entry must remain so the next tick retries.
	st := store.Load()
	require.Len(t, st.ClosedTopics, 1, "failed delete keeps entry queued")
	assert.Contains(t, st.TopicsByThread, "42", "TopicsByThread untouched on failure")

	// Second pass — fake clears failFirst after first call, so this should succeed.
	s.SweepOnce(context.Background())

	st = store.Load()
	assert.Empty(t, st.ClosedTopics)
	assert.NotContains(t, st.TopicsByThread, "42")
}

func TestTopicSweep_removeFromState_removesAllReuseKeyAliases(t *testing.T) {
	s, store, _ := newSweepFixture(t, 1*time.Second)

	require.NoError(t, store.Mutate(func(st *access.State) bool {
		st.TopicsByThread = map[string]access.TopicMeta{"42": {ThreadID: 42}}
		st.TopicsByReuseKey = map[string]int{
			"label:foo":     42,
			"workdir:/path": 42,
			"label:other":   99,
		}

		return true
	}))

	s.removeFromState(42)

	st := store.Load()
	assert.NotContains(t, st.TopicsByReuseKey, "label:foo")
	assert.NotContains(t, st.TopicsByReuseKey, "workdir:/path")
	assert.Contains(t, st.TopicsByReuseKey, "label:other", "keys pointing to other threads untouched")
	assert.NotContains(t, st.TopicsByThread, "42")
}

func TestTopicSweep_Run_respectsContextCancel(t *testing.T) {
	dir := t.TempDir()
	store := access.NewStore(dir, false)
	require.NoError(t, store.Save(access.State{ForumChatID: -100123}))

	s := NewTopicSweep(store, &fakeSweepBot{}, 1*time.Hour, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		s.Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sweep did not return within 1s of ctx cancel")
	}
}

func TestTopicSweep_Run_disabledOnNonPositiveDurations(t *testing.T) {
	dir := t.TempDir()
	store := access.NewStore(dir, false)
	s := NewTopicSweep(store, &fakeSweepBot{}, 0, 10*time.Millisecond)

	done := make(chan struct{})

	go func() {
		s.Run(context.Background())
		close(done)
	}()

	select {
	case <-done:
		// Expected: Run returns immediately when purge_after <= 0.
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Run with purge_after=0 should exit immediately")
	}
}

// TestTopicSweep_threadIDStringConsistency catches any divergence between
// the strconv.Itoa key writes (in forum.go) and the strconv.Itoa key
// deletes (in topic_sweep.go) — if they ever drift, sweep silently leaks.
func TestTopicSweep_threadIDStringConsistency(t *testing.T) {
	s, store, b := newSweepFixture(t, 1*time.Second)

	threadID := 12345
	tidStr := strconv.Itoa(threadID)

	require.NoError(t, store.Mutate(func(st *access.State) bool {
		st.ClosedTopics = []access.ClosedTopic{
			{ThreadID: threadID, ClosedAt: time.Now().Add(-1 * time.Hour).Unix()},
		}
		st.TopicsByThread = map[string]access.TopicMeta{tidStr: {ThreadID: threadID}}

		return true
	}))

	s.SweepOnce(context.Background())

	assert.Contains(t, b.deleted, threadID)
	assert.NotContains(t, store.Load().TopicsByThread, tidStr)
}

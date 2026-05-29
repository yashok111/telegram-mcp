package daemon

import (
	"context"
	"errors"
	"math"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/access"
	"github.com/yakov/telegram-mcp/internal/bot"
)

const testForumChat = int64(-100123)

type hdrSendRec struct {
	chatID   string
	text     string
	threadID int
}

type hdrEditRec struct {
	chatID string
	msgID  int
	text   string
}

type hdrPinRec struct {
	chatID int64
	msgID  int
}

// fakeHeaderBot records header Send/Edit/Pin calls and lets a test inject
// per-method errors.
type fakeHeaderBot struct {
	mu        sync.Mutex
	sends     []hdrSendRec
	edits     []hdrEditRec
	pins      []hdrPinRec
	nextMsgID int
	sendErr   error
	editErr   error
	pinErr    error
}

func (f *fakeHeaderBot) SendMessage(_ context.Context, chatID, text string, opts bot.SendOpts) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.sendErr != nil {
		return 0, f.sendErr
	}

	f.nextMsgID++
	f.sends = append(f.sends, hdrSendRec{chatID: chatID, text: text, threadID: opts.MessageThreadID})

	return f.nextMsgID, nil
}

func (f *fakeHeaderBot) EditMessage(_ context.Context, chatID string, msgID int, text, _ string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.editErr != nil {
		return 0, f.editErr
	}

	f.edits = append(f.edits, hdrEditRec{chatID: chatID, msgID: msgID, text: text})

	return msgID, nil
}

func (f *fakeHeaderBot) PinChatMessage(_ context.Context, chatID int64, msgID int) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.pinErr != nil {
		return f.pinErr
	}

	f.pins = append(f.pins, hdrPinRec{chatID: chatID, msgID: msgID})

	return nil
}

func (f *fakeHeaderBot) snapshot() ([]hdrSendRec, []hdrEditRec, []hdrPinRec) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]hdrSendRec(nil), f.sends...),
		append([]hdrEditRec(nil), f.edits...),
		append([]hdrPinRec(nil), f.pins...)
}

type fakeHeaderIdents struct {
	id HeaderIdentity
	ok bool
}

func (f *fakeHeaderIdents) HeaderIdentity(int) (HeaderIdentity, bool) {
	return f.id, f.ok
}

// newTestHeader builds a manager over a temp store with a refresh window the
// caller picks (throttle tests need it small/large) and a pinned clock that the
// returned setter advances.
func newTestHeader(t *testing.T, b headerBot, idents headerIdentitySource, refresh time.Duration) (*HeaderManager, *access.Store, func(time.Duration)) {
	t.Helper()

	store := access.NewStore(t.TempDir(), false)
	m := NewHeaderManager(store, b, idents, testForumChat, refresh, time.Minute)

	now := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }

	advance := func(d time.Duration) { now = now.Add(d) }

	return m, store, advance
}

func metaFor(t *testing.T, store *access.Store, threadID int) access.TopicMeta {
	t.Helper()

	return store.Load().TopicsByThread[strconv.Itoa(threadID)]
}

func TestRenderHeader_states(t *testing.T) {
	now := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	base := headerView{
		Alias:        "s3",
		Workdir:      "/home/yakov/projects/telegram-mcp",
		ShimIDPrefix: "d35ae17c",
		LastActivity: now.Add(-12 * time.Second),
		ConnectedAt:  now.Add(-(2*time.Hour + 15*time.Minute)),
		Now:          now,
	}

	t.Run("idle full layout", func(t *testing.T) {
		v := base
		v.State = HeaderIdle

		want := "🟢 @s3 — telegram-mcp\n" +
			"workdir: /home/yakov/projects/telegram-mcp\n" +
			"label: (none)\n" +
			"status: idle\n" +
			"last activity: 12s ago\n" +
			"uptime: 2h 15m\n" +
			"shim: d35ae17c"

		assert.Equal(t, want, renderHeader(v))
	})

	cases := []struct {
		name       string
		state      HeaderState
		tool       string
		wantIcon   string
		wantStatus string
	}{
		{"busy with tool", HeaderBusy, "Bash(go test)", "🟡", "status: busy — Bash(go test)"},
		{"busy no tool", HeaderBusy, "", "🟡", "status: busy"},
		{"permission with tool", HeaderPermission, "Bash(go test)", "🔵", "status: awaiting permission — Bash(go test)"},
		{"disconnected", HeaderDisconnected, "", "⚪", "status: disconnected — next session in same workdir reattaches"},
		{"closed", HeaderClosed, "", "🔴", "status: closed — scheduled for purge"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := base
			v.State = tc.state
			v.Tool = tc.tool

			got := renderHeader(v)
			assert.Contains(t, got, tc.wantIcon+" @s3")
			assert.Contains(t, got, tc.wantStatus)
		})
	}

	t.Run("label wins over workdir basename", func(t *testing.T) {
		v := base
		v.State = HeaderIdle
		v.Label = "main-bot"

		got := renderHeader(v)
		assert.Contains(t, got, "🟢 @s3 — main-bot")
		assert.Contains(t, got, "label: main-bot")
	})

	t.Run("zero timestamps render dash", func(t *testing.T) {
		got := renderHeader(headerView{State: HeaderIdle, Alias: "s1", Now: now})
		assert.Contains(t, got, "last activity: —")
		assert.Contains(t, got, "uptime: —")
		assert.Contains(t, got, "shim: —")
	})
}

func TestHumanizeDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{-5 * time.Second, "0s"},
		{12 * time.Second, "12s"},
		{59 * time.Second, "59s"},
		{60 * time.Second, "1m"},
		{5 * time.Minute, "5m"},
		{59 * time.Minute, "59m"},
		{60 * time.Minute, "1h 0m"},
		{2*time.Hour + 15*time.Minute, "2h 15m"},
		{23*time.Hour + 59*time.Minute, "23h 59m"},
		{24 * time.Hour, "1d 0h"},
		{3*24*time.Hour + 4*time.Hour, "3d 4h"},
	}

	for _, tc := range cases {
		assert.Equal(t, tc.want, humanizeDuration(tc.d), tc.d.String())
	}
}

func idleIdents() *fakeHeaderIdents {
	return &fakeHeaderIdents{
		ok: true,
		id: HeaderIdentity{
			Alias:        "s3",
			Workdir:      "/home/yakov/projects/telegram-mcp",
			ShimIDPrefix: "d35ae17c",
			ConnectedAt:  time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC),
		},
	}
}

func TestHeaderManager_EnsureCreatesAndPins(t *testing.T) {
	b := &fakeHeaderBot{}
	m, store, _ := newTestHeader(t, b, idleIdents(), time.Minute)

	m.Ensure(context.Background(), 119)

	sends, edits, pins := b.snapshot()
	require.Len(t, sends, 1)
	assert.Equal(t, 119, sends[0].threadID)
	assert.Contains(t, sends[0].text, "🟢 @s3 — telegram-mcp")
	assert.Empty(t, edits)
	require.Len(t, pins, 1)
	assert.Equal(t, testForumChat, pins[0].chatID)
	assert.Equal(t, 1, pins[0].msgID)

	meta := metaFor(t, store, 119)
	assert.Equal(t, 1, meta.HeaderMessageID)
	assert.True(t, meta.HeaderPinned)
	assert.NotZero(t, meta.HeaderRenderHash)
}

func TestHeaderManager_ReuseNoResend(t *testing.T) {
	b := &fakeHeaderBot{}
	m, store, _ := newTestHeader(t, b, idleIdents(), time.Minute)

	// Seed a persisted header (as if a prior daemon session pinned it).
	require.NoError(t, store.Mutate(func(st *access.State) bool {
		st.TopicsByThread = map[string]access.TopicMeta{
			"119": {ThreadID: 119, HeaderMessageID: 555, HeaderPinned: true, HeaderRenderHash: 42},
		}

		return true
	}))

	m.Ensure(context.Background(), 119)

	sends, edits, _ := b.snapshot()
	assert.Empty(t, sends, "reuse must not resend the header")
	require.Len(t, edits, 1, "reuse edits the existing message")
	assert.Equal(t, 555, edits[0].msgID)

	assert.Equal(t, 555, metaFor(t, store, 119).HeaderMessageID)
}

func TestHeaderManager_HashDedup(t *testing.T) {
	b := &fakeHeaderBot{}
	m, _, _ := newTestHeader(t, b, idleIdents(), time.Minute)

	m.Ensure(context.Background(), 119) // create (send #1)

	// Two identical busy transitions at the same pinned clock → one edit.
	m.SetState(119, HeaderBusy, "Bash(go test)")
	m.flush(context.Background(), 119)
	m.SetState(119, HeaderBusy, "Bash(go test)")
	m.flush(context.Background(), 119)

	sends, edits, _ := b.snapshot()
	assert.Len(t, sends, 1)
	assert.Len(t, edits, 1, "identical render must be deduped by hash")
}

func TestHeaderManager_Throttle(t *testing.T) {
	const refresh = 300 * time.Millisecond

	b := &fakeHeaderBot{}
	m, _, advance := newTestHeader(t, b, idleIdents(), refresh)

	m.Ensure(context.Background(), 119) // create; anchors lastEditAt

	// 10 state changes spread across ~1s, flushing the dirty set each step.
	const steps = 10

	const step = 100 * time.Millisecond

	for i := range steps {
		state := HeaderBusy
		if i%2 == 0 {
			state = HeaderIdle
		}

		m.SetState(119, state, "")
		advance(step)
		m.flushDirty(context.Background())
	}

	_, edits, _ := b.snapshot()

	window := steps * step
	maxEdits := int(math.Ceil(float64(window) / float64(refresh)))
	assert.LessOrEqual(t, len(edits), maxEdits, "throttle bounds edits to ceil(window/refresh)")
	assert.Less(t, len(edits), steps, "bursts must coalesce")
	assert.GreaterOrEqual(t, len(edits), 1, "at least one edit should land")
}

func TestHeaderManager_PinDegrade(t *testing.T) {
	b := &fakeHeaderBot{pinErr: errors.New("Bad Request: not enough rights to pin a message")}
	m, store, _ := newTestHeader(t, b, idleIdents(), time.Minute)

	m.Ensure(context.Background(), 119)

	sends, _, pins := b.snapshot()
	require.Len(t, sends, 1, "header still sent when pin fails")
	assert.Empty(t, pins, "failed pin records no success")

	meta := metaFor(t, store, 119)
	assert.Equal(t, 1, meta.HeaderMessageID)
	assert.False(t, meta.HeaderPinned, "graceful degrade: header_pinned=false")
}

func TestHeaderManager_RecreateOnNotFound(t *testing.T) {
	b := &fakeHeaderBot{}
	m, store, _ := newTestHeader(t, b, idleIdents(), time.Minute)

	m.Ensure(context.Background(), 119) // create send #1 (msgID 1)

	// User deletes the header; the next edit 400s and the manager recreates.
	b.editErr = errors.New("Bad Request: message to edit not found")

	m.SetState(119, HeaderBusy, "Bash")
	m.flush(context.Background(), 119)

	sends, _, pins := b.snapshot()
	require.Len(t, sends, 2, "not-found edit triggers a recreate send")
	assert.Equal(t, 119, sends[1].threadID)

	// New message id persisted + re-pinned.
	assert.Equal(t, 2, metaFor(t, store, 119).HeaderMessageID)
	require.Len(t, pins, 2)
	assert.Equal(t, 2, pins[1].msgID)
}

func TestHeaderManager_PurgeOnThreadGone(t *testing.T) {
	b := &fakeHeaderBot{}
	m, store, _ := newTestHeader(t, b, idleIdents(), time.Minute)

	const reuseKey = "workdir:/home/yakov/projects/telegram-mcp"

	// Seed a reuse-key pointing at the topic so we can assert it is purged too.
	require.NoError(t, store.Mutate(func(st *access.State) bool {
		st.TopicsByReuseKey = map[string]int{reuseKey: 283}

		return true
	}))

	m.Ensure(context.Background(), 283) // create send #1 (msgID 1), persists meta

	// Topic deleted in Telegram out from under us: the edit 400s "message to
	// edit not found", and the recreate send 400s "message thread not found".
	b.editErr = errors.New("Bad Request: message to edit not found")
	b.sendErr = errors.New("Bad Request: message thread not found")

	m.SetState(283, HeaderBusy, "Bash")
	m.flush(context.Background(), 283)

	// Runtime entry dropped so markActiveDirty stops re-dirtying it.
	m.mu.Lock()
	_, stillTracked := m.entries[283]
	m.mu.Unlock()
	assert.False(t, stillTracked, "purged topic must leave no runtime entry")

	// Persisted state purged: no meta, no reuse key.
	st := store.Load()

	_, hasMeta := st.TopicsByThread["283"]
	assert.False(t, hasMeta, "purged topic meta deleted")

	_, hasKey := st.TopicsByReuseKey[reuseKey]
	assert.False(t, hasKey, "purged topic reuse key deleted")

	// The error loop is broken: a background tick + flush issues no further IO.
	sendsBefore, editsBefore, _ := b.snapshot()

	m.markActiveDirty()
	m.flushDirty(context.Background())

	sendsAfter, editsAfter, _ := b.snapshot()
	assert.Len(t, sendsAfter, len(sendsBefore), "no further send after purge — loop broken")
	assert.Len(t, editsAfter, len(editsBefore), "no further edit after purge — loop broken")
}

func TestHeaderManager_PurgeOnPermanentError(t *testing.T) {
	// Any permanent Telegram failure (not just thread-deleted) must purge and
	// stop, not redirty forever — this is the generalization of the zombie fix.
	cases := []struct {
		name    string
		errText string
	}{
		{"thread deleted", "Bad Request: message thread not found"},
		{"bot kicked", "Forbidden: bot was kicked from the supergroup chat"},
		{"chat not found", "Bad Request: chat not found"},
		{"user deactivated", "Forbidden: user is deactivated"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &fakeHeaderBot{}
			m, store, _ := newTestHeader(t, b, idleIdents(), time.Minute)

			m.Ensure(context.Background(), 119) // create send #1

			b.editErr = errors.New(tc.errText)

			m.SetState(119, HeaderBusy, "Bash")
			m.flush(context.Background(), 119)

			m.mu.Lock()
			_, tracked := m.entries[119]
			m.mu.Unlock()
			assert.False(t, tracked, "permanent error purges the runtime entry")

			_, hasMeta := store.Load().TopicsByThread["119"]
			assert.False(t, hasMeta, "permanent error purges persisted meta")
		})
	}
}

func TestHeaderManager_PurgeFiresUnbindHook(t *testing.T) {
	b := &fakeHeaderBot{}
	m, _, _ := newTestHeader(t, b, idleIdents(), time.Minute)

	var unbound []int

	m.SetPurgeHook(func(tid int) { unbound = append(unbound, tid) })

	m.Ensure(context.Background(), 119)

	b.editErr = errors.New("Forbidden: bot was kicked from the supergroup chat")

	m.SetState(119, HeaderBusy, "x")
	m.flush(context.Background(), 119)

	assert.Equal(t, []int{119}, unbound, "purge fires the unbind hook so the Router binding is cleared in lockstep")
}

func TestHeaderManager_DisconnectedUsesCachedIdentity(t *testing.T) {
	idents := idleIdents()
	b := &fakeHeaderBot{}
	m, _, _ := newTestHeader(t, b, idents, time.Minute)

	m.Ensure(context.Background(), 119) // caches identity while connected

	idents.ok = false // shim dropped — Router no longer resolves the topic

	m.Disconnected(119)
	m.flush(context.Background(), 119)

	_, edits, _ := b.snapshot()
	require.NotEmpty(t, edits)
	last := edits[len(edits)-1].text
	assert.Contains(t, last, "⚪ @s3 — telegram-mcp", "cached identity still names the owner")
	assert.Contains(t, last, "status: disconnected")
}

func TestHeaderManager_Closed(t *testing.T) {
	b := &fakeHeaderBot{}
	m, _, _ := newTestHeader(t, b, idleIdents(), time.Minute)

	m.Ensure(context.Background(), 119)
	m.Closed(context.Background(), 119)

	_, edits, _ := b.snapshot()
	require.NotEmpty(t, edits)
	last := edits[len(edits)-1].text
	assert.Contains(t, last, "🔴 @s3")
	assert.Contains(t, last, "status: closed")
}

// blockingHeaderBot blocks the first SendMessage on a gate so a test can hold a
// flush mid-IO and prove a concurrent flush of the same topic is serialized.
type blockingHeaderBot struct {
	fakeHeaderBot
	entered chan struct{}
	gate    chan struct{}
}

func (b *blockingHeaderBot) SendMessage(ctx context.Context, chatID, text string, opts bot.SendOpts) (int, error) {
	close(b.entered) // panics on a 2nd send → exactly what we want the test to catch
	<-b.gate

	return b.fakeHeaderBot.SendMessage(ctx, chatID, text, opts)
}

func TestHeaderManager_FlushSerializedPerTopic(t *testing.T) {
	b := &blockingHeaderBot{entered: make(chan struct{}), gate: make(chan struct{})}
	m, _, _ := newTestHeader(t, b, idleIdents(), time.Minute)

	var wg sync.WaitGroup

	wg.Go(func() {
		m.Ensure(context.Background(), 119) // first flush blocks inside SendMessage
	})

	<-b.entered // first flush is mid-IO with flushing=true

	m.Ensure(context.Background(), 119) // second flush must bail, not double-send

	close(b.gate)
	wg.Wait()

	sends, _, _ := b.snapshot()
	assert.Len(t, sends, 1, "concurrent flush of the same topic must be serialized")
}

func TestRouterHeaderIdentity(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "abcdef012345", Workdir: "/home/yakov/projects/telegram-mcp", Label: "deploy"})
	r.BindTopic("abcdef012345", 119)

	id, ok := r.HeaderIdentity(119)
	require.True(t, ok)
	assert.Equal(t, "deploy", id.Label)
	assert.Equal(t, "/home/yakov/projects/telegram-mcp", id.Workdir)
	assert.Equal(t, "abcdef01", id.ShimIDPrefix)
	assert.NotEmpty(t, id.Alias)

	_, ok = r.HeaderIdentity(999)
	assert.False(t, ok, "unowned thread resolves to no identity")

	r.Drop("abcdef012345")

	_, ok = r.HeaderIdentity(119)
	assert.False(t, ok, "disconnected shim's topic resolves to no identity")
}

func TestRouterTopicForShim(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "s-with-topic"})
	r.Register(&Shim{ID: "s-no-topic"})
	r.BindTopic("s-with-topic", 119)

	tid, ok := r.TopicForShim("s-with-topic")
	assert.True(t, ok)
	assert.Equal(t, 119, tid)

	_, ok = r.TopicForShim("s-no-topic")
	assert.False(t, ok, "shim without a topic resolves to none")

	_, ok = r.TopicForShim("ghost")
	assert.False(t, ok, "unknown shim resolves to none")
}

func TestRouterDropTopic(t *testing.T) {
	r := NewRouter()
	r.Register(&Shim{ID: "s-owner"})
	r.BindTopic("s-owner", 119)

	tid, ok := r.TopicForShim("s-owner")
	require.True(t, ok)
	require.Equal(t, 119, tid)

	r.DropTopic(119)

	_, ok = r.TopicForShim("s-owner")
	assert.False(t, ok, "owning shim's TopicID cleared")

	_, ok = r.HeaderIdentity(119)
	assert.False(t, ok, "topicOwners entry removed")

	assert.NotPanics(t, func() {
		r.DropTopic(999) // unknown thread
		r.DropTopic(0)   // guard
	})
}

func TestRouterSetLabelFiresHeaderHook(t *testing.T) {
	r := NewRouter()

	var got []int

	r.SetHeaderHook(func(tid int) { got = append(got, tid) })

	r.Register(&Shim{ID: "topicful"})
	r.BindTopic("topicful", 119)
	_, err := r.SetLabel("topicful", "main-bot")
	require.NoError(t, err)
	assert.Equal(t, []int{119}, got, "label change on a topic-bound shim repaints its header")

	r.Register(&Shim{ID: "topicless"})
	_, err = r.SetLabel("topicless", "x")
	require.NoError(t, err)
	assert.Equal(t, []int{119}, got, "label change without a topic does not call the hook")

	// A nil hook (feature disabled) must not panic on a topic-bound shim.
	r.SetHeaderHook(nil)
	r.Register(&Shim{ID: "topicful2"})
	r.BindTopic("topicful2", 222)
	assert.NotPanics(t, func() {
		_, err := r.SetLabel("topicful2", "y")
		require.NoError(t, err)
	})
}

func TestHeaderEnabled(t *testing.T) {
	cases := []struct {
		name string
		val  string
		want bool
	}{
		{"empty defaults on", "", true},
		{"explicit 1", "1", true},
		{"zero", "0", false},
		{"false", "false", false},
		{"OFF case-insensitive", "OFF", false},
		{"no", "no", false},
		{"yes is on", "yes", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TELEGRAM_TOPIC_HEADER", tc.val)
			assert.Equal(t, tc.want, HeaderEnabled())
		})
	}
}

func TestHeaderManager_RefreshTriggersRepaint(t *testing.T) {
	idents := idleIdents()
	b := &fakeHeaderBot{}
	m, _, advance := newTestHeader(t, b, idents, 300*time.Millisecond)

	m.Ensure(context.Background(), 119) // create; clears dirty
	advance(time.Second)                // throttle window elapsed

	idents.id.Label = "deploy" // identity changed out-of-band (/label)

	m.flushDirty(context.Background())

	_, edits, _ := b.snapshot()
	require.Empty(t, edits, "no repaint until something marks the topic dirty")

	m.Refresh(119)
	m.flushDirty(context.Background())

	_, edits, _ = b.snapshot()
	require.Len(t, edits, 1, "Refresh marks dirty so the next tick repaints")
	assert.Contains(t, edits[0].text, "label: deploy")
}

func TestHeaderManager_MarkActiveDirtySkipsClosed(t *testing.T) {
	b := &fakeHeaderBot{}
	m, _, _ := newTestHeader(t, b, idleIdents(), time.Minute)

	m.mu.Lock()
	m.entries[1] = &headerEntry{state: HeaderBusy}
	m.entries[2] = &headerEntry{state: HeaderClosed}
	m.mu.Unlock()

	m.markActiveDirty() // the background uptime tick

	m.mu.Lock()
	defer m.mu.Unlock()

	assert.True(t, m.entries[1].dirty, "active topic re-marked dirty so uptime ages")
	assert.False(t, m.entries[2].dirty, "closed topic is terminal — not re-rendered")
}

func TestHeaderManager_MarkActiveDirtySkipsDisconnected(t *testing.T) {
	b := &fakeHeaderBot{}
	m, _, _ := newTestHeader(t, b, idleIdents(), time.Minute)

	m.mu.Lock()
	m.entries[1] = &headerEntry{state: HeaderBusy}
	m.entries[2] = &headerEntry{state: HeaderDisconnected}
	m.mu.Unlock()

	m.markActiveDirty() // the background uptime tick

	m.mu.Lock()
	defer m.mu.Unlock()

	assert.True(t, m.entries[1].dirty, "active topic re-marked dirty so uptime ages")
	assert.False(t, m.entries[2].dirty,
		"disconnected topic is frozen — no live owner, so the uptime tick must not churn an edit every interval")
}

func TestHeaderManager_ClosedNoHeaderNoSend(t *testing.T) {
	b := &fakeHeaderBot{}
	m, _, _ := newTestHeader(t, b, idleIdents(), time.Minute)

	m.Closed(context.Background(), 119) // never Ensure'd → no persisted header

	sends, edits, _ := b.snapshot()
	assert.Empty(t, sends, "Closed must not create a header for a topic that never had one")
	assert.Empty(t, edits)
}

func TestHeaderManager_NilSafe(t *testing.T) {
	var m *HeaderManager

	assert.NotPanics(t, func() {
		m.Ensure(context.Background(), 1)
		m.SetState(1, HeaderBusy, "")
		m.Disconnected(1)
		m.Closed(context.Background(), 1)
	})
}

package admin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/yakov/telegram-mcp/internal/access"
	"github.com/yakov/telegram-mcp/internal/ipc"
)

// stateWithOwner writes an access.json whose AllowFrom resolves owner == "123"
// so the agent's proactive paths have a DM target.
func stateWithOwner(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	st := access.NewStore(dir, false)
	require.NoError(t, st.Save(access.State{
		DMPolicy:  access.PolicyAllowlist,
		AllowFrom: []string{"123"},
		Groups:    map[string]access.GroupPolicy{},
		Pending:   map[string]access.Pending{},
	}))

	return dir
}

func resultStream(result string) string {
	return `{"type":"result","subtype":"success","num_turns":1,"result":"` + result + `","total_cost_usd":0.001}` + "\n"
}

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreAnyFunction("github.com/valyala/fasthttp.(*HostClient).connsCleaner"),
		goleak.IgnoreAnyFunction("github.com/valyala/fasthttp.(*Client).mCleaner"),
		goleak.IgnoreAnyFunction("github.com/valyala/fasthttp.(*TCPDialer).tcpAddrsClean"),
	)
}

type fakeClient struct {
	mu        sync.Mutex
	helloIn   map[string]any
	helloResp map[string]any
	helloErr  error
	subs      map[string]ipc.NotifyHandler
	doneCh    chan struct{}
	notifs    []notifRec
	calls     []callRec
	nextMsgID int
	closed    bool
}

type notifRec struct {
	method string
	params any
}

type callRec struct {
	method string
	params map[string]any
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		subs:   map[string]ipc.NotifyHandler{},
		doneCh: make(chan struct{}),
		helloResp: map[string]any{
			"shim_id":        "admin-shim-id",
			"daemon_version": "test",
			"alias":          "admin",
		},
	}
}

func (c *fakeClient) Call(_ context.Context, method string, params, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	pm, _ := params.(map[string]any)

	switch method {
	case ipc.MethodHello:
		c.helloIn = pm

		if c.helloErr != nil {
			return c.helloErr
		}

		raw, _ := json.Marshal(c.helloResp)

		return json.Unmarshal(raw, result)
	case ipc.MethodBotSendMessage:
		c.calls = append(c.calls, callRec{method: method, params: pm})
		c.nextMsgID++

		raw, _ := json.Marshal(map[string]any{"message_id": c.nextMsgID})

		return json.Unmarshal(raw, result)
	default:
		return errors.New("unexpected method " + method)
	}
}

func (c *fakeClient) sendCalls() []callRec {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make([]callRec, 0, len(c.calls))
	for _, r := range c.calls {
		if r.method == ipc.MethodBotSendMessage {
			out = append(out, r)
		}
	}

	return out
}

// waitForSendCalls polls until at least n bot.sendMessage calls are recorded.
// answerInbound dispatches to a worker goroutine, so replies arrive async.
func waitForSendCalls(t *testing.T, c *fakeClient, n int) []callRec {
	t.Helper()

	var calls []callRec

	require.Eventually(t, func() bool {
		calls = c.sendCalls()
		return len(calls) >= n
	}, 2*time.Second, 5*time.Millisecond, "expected %d send calls", n)

	return calls
}

func (c *fakeClient) Notify(method string, params any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.notifs = append(c.notifs, notifRec{method: method, params: params})

	return nil
}

func (c *fakeClient) OnNotify(method string, h ipc.NotifyHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.subs[method] = h
}

func (c *fakeClient) Done() <-chan struct{} { return c.doneCh }

func (c *fakeClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil
	}

	c.closed = true

	return nil
}

func (c *fakeClient) fire(method string, params json.RawMessage) {
	c.mu.Lock()
	h, ok := c.subs[method]
	c.mu.Unlock()

	if !ok {
		return
	}

	h(context.Background(), params)
}

func TestAgentRunHelloThenWaitsForCancel(t *testing.T) {
	fc := newFakeClient()

	a := &Agent{
		StateDir:   t.TempDir(),
		SocketPath: "/tmp/fake.sock",
		Workdir:    "/work",
		DialIPC:    func(string) (Client, error) { return fc, nil },
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	deadline := time.After(time.Second)

	for a.ShimID() == "" {
		select {
		case <-deadline:
			t.Fatal("agent did not complete hello in 1s")
		case <-time.After(10 * time.Millisecond):
		}
	}

	assert.Equal(t, "admin-shim-id", a.ShimID())
	assert.Equal(t, "admin", a.Alias())

	fc.mu.Lock()
	role, _ := fc.helloIn["role"].(string)
	label, _ := fc.helloIn["label"].(string)
	wd, _ := fc.helloIn["workdir"].(string)
	fc.mu.Unlock()

	assert.Equal(t, "admin", role)
	assert.Equal(t, "admin", label)
	assert.Equal(t, "/work", wd)

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit within 2s of cancel")
	}
}

func TestAgentRunReturnsErrorWhenDaemonDropsConnection(t *testing.T) {
	fc := newFakeClient()

	a := &Agent{
		SocketPath: "/tmp/fake.sock",
		DialIPC:    func(string) (Client, error) { return fc, nil },
	}

	done := make(chan error, 1)
	go func() { done <- a.Run(context.Background()) }()

	for a.ShimID() == "" {
		time.Sleep(5 * time.Millisecond)
	}

	close(fc.doneCh)

	select {
	case err := <-done:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "daemon ipc dropped")
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after Done() fired")
	}
}

func TestAgentRunHelloFailureSurfacesError(t *testing.T) {
	fc := newFakeClient()
	fc.helloErr = errors.New("denied")

	a := &Agent{
		SocketPath: "/tmp/fake.sock",
		DialIPC:    func(string) (Client, error) { return fc, nil },
	}

	err := a.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "hello:")
}

func TestAgentRunCustomInboundHandlerInvoked(t *testing.T) {
	fc := newFakeClient()

	invoked := make(chan json.RawMessage, 1)
	a := &Agent{
		SocketPath: "/tmp/fake.sock",
		DialIPC:    func(string) (Client, error) { return fc, nil },
		HandleInbound: func(_ context.Context, params json.RawMessage) {
			invoked <- params
		},
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	for a.ShimID() == "" {
		time.Sleep(5 * time.Millisecond)
	}

	fc.fire(ipc.NotifyInbound, json.RawMessage(`{"chat_id":"42","text":"hi"}`))

	select {
	case p := <-invoked:
		assert.Contains(t, string(p), `"chat_id":"42"`)
	case <-time.After(time.Second):
		t.Fatal("HandleInbound was not invoked")
	}

	cancel()
	<-done
}

func TestAgentAnswerInboundFullFlow(t *testing.T) {
	fc := newFakeClient()

	a := &Agent{
		SocketPath: "/tmp/fake.sock",
		DialIPC:    func(string) (Client, error) { return fc, nil },
		Invoker:    &Invoker{Exec: fixedExec(streamFixture, nil)},
		History:    NewHistory(t.TempDir()),
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	for a.ShimID() == "" {
		time.Sleep(5 * time.Millisecond)
	}

	// answerInbound runs on a worker goroutine (claude must not block the read
	// loop), so the reply + history land asynchronously — poll for them.
	fc.fire(ipc.NotifyInbound, json.RawMessage(
		`{"content":"hello admin","meta":{"chat_id":"42","message_id":"7","user":"@bob"}}`))

	calls := waitForSendCalls(t, fc, 1)
	assert.Equal(t, "42", calls[0].params["chat_id"])
	text, _ := calls[0].params["text"].(string)
	assert.Contains(t, text, "Hello there")
	assert.Equal(t, 7, calls[0].params["reply_to"])

	var msgs []Message

	require.Eventually(t, func() bool {
		msgs, _ = a.History.Load("42")
		return len(msgs) == 2
	}, time.Second, 5*time.Millisecond, "both turns recorded")

	assert.Equal(t, "user", msgs[0].Role)
	assert.Equal(t, "hello admin", msgs[0].Content)
	assert.Equal(t, "@bob", msgs[0].User)
	assert.Equal(t, "assistant", msgs[1].Role)
	assert.Equal(t, "Hello there", msgs[1].Content)

	cancel()
	<-done
}

// TestAnswerInboundOwnerGatesTier2 is the security regression guard for the DM
// path: the owner gets the full tool set (Tier-2 auto-apply), but a non-owner
// allowlisted user reaching the agent via the DM-admin fallback gets the
// observer set only (no Tier-2), so they can't drive an immediate mutation.
func TestAnswerInboundOwnerGatesTier2(t *testing.T) {
	fc := newFakeClient()

	var (
		mu       sync.Mutex
		lastArgs []string
	)

	exec := func(_ context.Context, _, _ string, args, _ []string) ([]byte, error) {
		mu.Lock()
		lastArgs = args
		mu.Unlock()

		return []byte(streamFixture), nil
	}

	a := &Agent{
		StateDir:   stateWithOwner(t), // owner == "123"
		SocketPath: "/tmp/fake.sock",
		DialIPC:    func(string) (Client, error) { return fc, nil },
		Invoker:    &Invoker{SelfBin: "/bin/tg", Exec: exec},
		History:    NewHistory(t.TempDir()),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	for a.ShimID() == "" {
		time.Sleep(5 * time.Millisecond)
	}

	getArgs := func() string {
		mu.Lock()
		defer mu.Unlock()

		return strings.Join(lastArgs, " ")
	}

	// Owner DM (chat 123): full toolset includes Tier-2 auto-apply.
	fc.fire(ipc.NotifyInbound, json.RawMessage(
		`{"content":"hi","meta":{"chat_id":"123","message_id":"1"}}`))
	waitForSendCalls(t, fc, 1)
	assert.Contains(t, getArgs(), "mcp__admin__pin_chat_to_shim", "owner DM gets Tier-2 tools")

	// Non-owner DM (chat 999): observer set only — read tools, no Tier-2.
	fc.fire(ipc.NotifyInbound, json.RawMessage(
		`{"content":"hi","meta":{"chat_id":"999","message_id":"2"}}`))
	waitForSendCalls(t, fc, 2)
	args := getArgs()
	assert.NotContains(t, args, "mcp__admin__pin_chat_to_shim", "non-owner DM must NOT get Tier-2")
	assert.Contains(t, args, "mcp__admin__list_shims", "non-owner still gets read tools")

	cancel()
	<-done
}

func TestAgentAnswerInboundChunksLongReply(t *testing.T) {
	fc := newFakeClient()

	long := strings.Repeat("x", 9000) // 3 chunks at 4096-16
	stream := `{"type":"result","subtype":"success","num_turns":1,"result":"` + long + `","total_cost_usd":0.001}` + "\n"

	a := &Agent{
		SocketPath: "/tmp/fake.sock",
		DialIPC:    func(string) (Client, error) { return fc, nil },
		Invoker:    &Invoker{Exec: fixedExec(stream, nil)},
		History:    NewHistory(t.TempDir()),
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	for a.ShimID() == "" {
		time.Sleep(5 * time.Millisecond)
	}

	fc.fire(ipc.NotifyInbound, json.RawMessage(
		`{"content":"long please","meta":{"chat_id":"42","message_id":"7"}}`))

	calls := waitForSendCalls(t, fc, 3)
	require.Len(t, calls, 3)

	for _, c := range calls {
		assert.Equal(t, "42", c.params["chat_id"])
	}

	// Only the first chunk quote-replies the inbound.
	assert.Equal(t, 7, calls[0].params["reply_to"])

	_, has1 := calls[1].params["reply_to"]
	assert.False(t, has1, "second chunk must not carry reply_to")

	_, has2 := calls[2].params["reply_to"]
	assert.False(t, has2, "third chunk must not carry reply_to")

	cancel()
	<-done
}

func TestAgentAnswerInboundInvokeErrorRepliesError(t *testing.T) {
	fc := newFakeClient()

	a := &Agent{
		SocketPath: "/tmp/fake.sock",
		DialIPC:    func(string) (Client, error) { return fc, nil },
		Invoker:    &Invoker{Exec: fixedExec("", errors.New("claude exploded"))},
		History:    NewHistory(t.TempDir()),
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	for a.ShimID() == "" {
		time.Sleep(5 * time.Millisecond)
	}

	fc.fire(ipc.NotifyInbound, json.RawMessage(
		`{"content":"do thing","meta":{"chat_id":"42","message_id":"7"}}`))

	calls := waitForSendCalls(t, fc, 1)
	text, _ := calls[0].params["text"].(string)
	assert.Contains(t, text, "error")
	assert.NotContains(t, text, "claude exploded", "raw error (with possible stderr secrets) must not reach the chat")

	// A failed invoke must not poison history with a half-exchange.
	msgs, err := a.History.Load("42")
	require.NoError(t, err)
	assert.Empty(t, msgs)

	cancel()
	<-done
}

func TestAgentReactToEventReportsToOwner(t *testing.T) {
	fc := newFakeClient()

	var (
		promptMu  sync.Mutex
		gotPrompt string
	)

	exec := func(_ context.Context, _, _ string, args, _ []string) ([]byte, error) {
		promptMu.Lock()
		gotPrompt = args[len(args)-1] // buildPrompt output is the final arg
		promptMu.Unlock()

		return []byte(resultStream("shim crashed — suggest restart")), nil
	}

	a := &Agent{
		StateDir:   stateWithOwner(t),
		SocketPath: "/tmp/fake.sock",
		DialIPC:    func(string) (Client, error) { return fc, nil },
		Invoker:    &Invoker{Exec: exec},
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	for a.ShimID() == "" {
		time.Sleep(5 * time.Millisecond)
	}

	fc.fire(ipc.NotifyAdminEvent, json.RawMessage(
		`{"type":"shim_disconnected","severity":"warning","subject":"abc123","detail":"no goodbye"}`))

	calls := waitForSendCalls(t, fc, 1)
	assert.Equal(t, "123", calls[0].params["chat_id"], "event report must DM the owner")
	text, _ := calls[0].params["text"].(string)
	assert.Contains(t, text, "shim crashed")

	// The invoker prompt must actually carry the triggering event.
	promptMu.Lock()
	p := gotPrompt
	promptMu.Unlock()
	assert.Contains(t, p, "shim_disconnected", "event prompt must carry the event type")
	assert.Contains(t, p, "abc123", "event prompt must carry the event subject")

	cancel()
	<-done
}

func TestAgentReactToEventNoReportSuppressesDM(t *testing.T) {
	fc := newFakeClient()

	invoked := make(chan struct{}, 1)
	exec := func(_ context.Context, _, _ string, _, _ []string) ([]byte, error) {
		select {
		case invoked <- struct{}{}:
		default:
		}

		return []byte(resultStream("NOREPORT")), nil
	}

	a := &Agent{
		StateDir:   stateWithOwner(t),
		SocketPath: "/tmp/fake.sock",
		DialIPC:    func(string) (Client, error) { return fc, nil },
		Invoker:    &Invoker{Exec: exec},
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	for a.ShimID() == "" {
		time.Sleep(5 * time.Millisecond)
	}

	fc.fire(ipc.NotifyAdminEvent, json.RawMessage(
		`{"type":"bg_failed","severity":"warning","subject":"deadbeef","detail":"exit 1"}`))

	select {
	case <-invoked:
	case <-time.After(time.Second):
		t.Fatal("invoker was not called for the event")
	}

	// The invoker ran and answered NOREPORT — no owner DM must follow.
	require.Never(t, func() bool {
		return len(fc.sendCalls()) > 0
	}, 200*time.Millisecond, 25*time.Millisecond, "NOREPORT must suppress the owner DM")

	cancel()
	<-done
}

func TestAgentProduceSitrepReportsToOwner(t *testing.T) {
	fc := newFakeClient()

	a := &Agent{
		StateDir:   stateWithOwner(t),
		SocketPath: "/tmp/fake.sock",
		DialIPC:    func(string) (Client, error) { return fc, nil },
		Invoker:    &Invoker{Exec: fixedExec(resultStream("2 sessions online, no errors"), nil)},
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	for a.ShimID() == "" {
		time.Sleep(5 * time.Millisecond)
	}

	fc.fire(ipc.NotifyAdminSitrep, json.RawMessage(`{"ts":"2026-05-24T00:00:00Z"}`))

	calls := waitForSendCalls(t, fc, 1)
	assert.Equal(t, "123", calls[0].params["chat_id"], "sitrep must DM the owner")
	text, _ := calls[0].params["text"].(string)
	assert.Contains(t, text, "2 sessions online")

	cancel()
	<-done
}

func TestAgentRunSendsGoodbyeOnShutdown(t *testing.T) {
	fc := newFakeClient()

	a := &Agent{
		SocketPath: "/tmp/fake.sock",
		DialIPC:    func(string) (Client, error) { return fc, nil },
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	for a.ShimID() == "" {
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	<-done

	fc.mu.Lock()
	defer fc.mu.Unlock()

	var sawGoodbye bool

	for _, n := range fc.notifs {
		if n.method == ipc.MethodGoodbye {
			sawGoodbye = true
		}
	}

	assert.True(t, sawGoodbye, "agent must say goodbye before disconnect")
}

func TestBuildEventPromptMentionsTiering(t *testing.T) {
	p := buildEventPrompt(Event{Type: "shim_disconnected", Subject: "s2"}, nil)

	assert.Contains(t, p, "Tier-2")
	assert.Contains(t, p, "Tier-3")
	assert.Contains(t, p, "proposed, awaiting your approval")
	assert.Contains(t, p, "UNTRUSTED", "must warn the model that observed content can be injected")
}

func TestBuildSitrepPromptMentionsTiering(t *testing.T) {
	p := buildSitrepPrompt(nil)

	assert.Contains(t, p, "Tier-3")
	assert.Contains(t, p, "UNTRUSTED")
}

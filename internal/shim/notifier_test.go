package shim

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/ipc"
)

type fakeMCP struct {
	inbound  []inboundCall
	resolved []resolvedCall
}

type inboundCall struct {
	content string
	meta    map[string]string
}

type resolvedCall struct {
	requestID, behavior string
}

func (f *fakeMCP) DeliverInbound(content string, meta map[string]string) {
	f.inbound = append(f.inbound, inboundCall{content, meta})
}

func (f *fakeMCP) ResolvePermission(requestID, behavior string) {
	f.resolved = append(f.resolved, resolvedCall{requestID, behavior})
}

type spyClient struct {
	handlers map[string]ipc.NotifyHandler
}

func newSpyClient() *spyClient { return &spyClient{handlers: map[string]ipc.NotifyHandler{}} }

func (s *spyClient) Call(context.Context, string, any, any) error { return nil }
func (s *spyClient) Notify(string, any) error                     { return nil }
func (s *spyClient) OnNotify(method string, h ipc.NotifyHandler)  { s.handlers[method] = h }
func (s *spyClient) Close() error                                 { return nil }
func (s *spyClient) Done() <-chan struct{}                        { return nil }

// newTestNotifier wires AttachNotifier with a worker for tests and registers
// the worker for cleanup. Returns the worker so tests that fire notifications
// can flush it.
func newTestNotifier(t *testing.T, c IPCClient, sink MCPSink) *notifierWorker {
	t.Helper()

	w := newNotifierWorker()
	t.Cleanup(w.Stop)
	AttachNotifier(c, sink, w)

	return w
}

func newTestLabelHandler(t *testing.T, c IPCClient, updater LabelUpdater) *notifierWorker {
	t.Helper()

	w := newNotifierWorker()
	t.Cleanup(w.Stop)
	AttachLabelHandler(c, updater, w)

	return w
}

// flush submits a sentinel and waits for it to run, which guarantees every
// previously-submitted task on the same worker has completed (the worker is
// strictly serial).
func flush(w *notifierWorker) {
	done := make(chan struct{})
	w.submit("flush", func() { close(done) })
	<-done
}

func TestNotifierRegistersHandlersOnAttach(t *testing.T) {
	c := newSpyClient()
	mcp := &fakeMCP{}
	newTestNotifier(t, c, mcp)

	_, hasInbound := c.handlers[ipc.NotifyInbound]
	_, hasResolved := c.handlers[ipc.NotifyPermissionResolved]

	assert.True(t, hasInbound)
	assert.True(t, hasResolved)
}

func TestNotifierDispatchesInbound(t *testing.T) {
	c := newSpyClient()
	mcp := &fakeMCP{}
	w := newTestNotifier(t, c, mcp)

	params := json.RawMessage(`{"content":"hi","meta":{"chat_id":"1","user":"@x"}}`)
	c.handlers[ipc.NotifyInbound](context.Background(), params)

	flush(w)

	require.Len(t, mcp.inbound, 1)
	assert.Equal(t, "hi", mcp.inbound[0].content)
	assert.Equal(t, "1", mcp.inbound[0].meta["chat_id"])
	assert.Equal(t, "@x", mcp.inbound[0].meta["user"])
}

func TestNotifierDispatchesPermissionResolved(t *testing.T) {
	c := newSpyClient()
	mcp := &fakeMCP{}
	w := newTestNotifier(t, c, mcp)

	c.handlers[ipc.NotifyPermissionResolved](context.Background(),
		json.RawMessage(`{"request_id":"ababc","behavior":"allow"}`))

	flush(w)

	require.Len(t, mcp.resolved, 1)
	assert.Equal(t, "ababc", mcp.resolved[0].requestID)
	assert.Equal(t, "allow", mcp.resolved[0].behavior)
}

type labelSpy struct {
	mu     sync.Mutex
	labels []string
}

func (l *labelSpy) UpdateLabel(label string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.labels = append(l.labels, label)
}

func (l *labelSpy) seen() []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]string(nil), l.labels...)
}

func TestAttachLabelHandlerInvokesUpdater(t *testing.T) {
	c := newSpyClient()
	spy := &labelSpy{}
	w := newTestLabelHandler(t, c, spy)

	h, ok := c.handlers[ipc.NotifyLabelChanged]
	require.True(t, ok, "AttachLabelHandler must register the notify handler")

	body, err := json.Marshal(map[string]string{"label": "main"})
	require.NoError(t, err)
	h(context.Background(), body)

	flush(w)

	assert.Equal(t, []string{"main"}, spy.seen())
}

func TestAttachLabelHandlerNilUpdaterNoop(t *testing.T) {
	c := newSpyClient()
	w := newNotifierWorker()
	t.Cleanup(w.Stop)
	AttachLabelHandler(c, nil, w)

	_, ok := c.handlers[ipc.NotifyLabelChanged]
	assert.False(t, ok, "nil updater must not register a handler")
}

func TestAttachLabelHandlerBadJSONIgnored(t *testing.T) {
	c := newSpyClient()
	spy := &labelSpy{}
	w := newTestLabelHandler(t, c, spy)

	h, ok := c.handlers[ipc.NotifyLabelChanged]
	require.True(t, ok)

	h(context.Background(), json.RawMessage(`{`))
	flush(w)

	assert.Empty(t, spy.seen())
}

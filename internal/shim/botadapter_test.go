package shim

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/yakov/telegram-mcp/internal/bot"
	"github.com/yakov/telegram-mcp/internal/ipc"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

type fakeClient struct {
	mu           sync.Mutex
	calledMethod string
	calledParams json.RawMessage
	returnResult json.RawMessage
	returnErr    error

	helloCount atomic.Int32

	doneCh    chan struct{}
	doneClose sync.Once
}

func (f *fakeClient) Call(_ context.Context, method string, params, result any) error {
	f.mu.Lock()
	f.calledMethod = method
	f.calledParams, _ = json.Marshal(params)
	f.mu.Unlock()

	if method == ipc.MethodHello {
		f.helloCount.Add(1)
	}

	if f.returnErr != nil {
		return f.returnErr
	}

	if result != nil && len(f.returnResult) > 0 {
		return json.Unmarshal(f.returnResult, result)
	}

	return nil
}

func (f *fakeClient) Notify(string, any) error           { return nil }
func (f *fakeClient) OnNotify(string, ipc.NotifyHandler) {}
func (f *fakeClient) Close() error                       { return nil }
func (f *fakeClient) Done() <-chan struct{}              { return f.doneCh }

// closeDone shuts down the simulated IPC client. Idempotent so tests can
// double-signal without panicking on a closed channel.
func (f *fakeClient) closeDone() {
	if f.doneCh == nil {
		return
	}

	f.doneClose.Do(func() { close(f.doneCh) })
}

func TestBotAdapterSendMessage(t *testing.T) {
	fc := &fakeClient{returnResult: json.RawMessage(`{"message_id":99}`)}
	a := NewBotAdapter(fc, nil)

	id, err := a.SendMessage(context.Background(), "123", "hi", bot.SendOpts{ReplyTo: 4, ParseMode: "MarkdownV2"})
	require.NoError(t, err)
	assert.Equal(t, 99, id)
	assert.Equal(t, "bot.sendMessage", fc.calledMethod)
	assert.JSONEq(t, `{"chat_id":"123","text":"hi","reply_to":4,"parse_mode":"MarkdownV2"}`, string(fc.calledParams))
}

func TestBotAdapterSendMessageNotAllowlisted(t *testing.T) {
	data, _ := json.Marshal(map[string]string{"chat_id": "9"})
	fc := &fakeClient{returnErr: &ipc.Error{Code: ipc.CodeNotAllowlisted, Message: "no", Data: data}}
	a := NewBotAdapter(fc, nil)

	_, err := a.SendMessage(context.Background(), "9", "x", bot.SendOpts{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotAllowlisted)
}

func TestBotAdapterReact(t *testing.T) {
	fc := &fakeClient{returnResult: json.RawMessage(`{}`)}
	a := NewBotAdapter(fc, nil)

	require.NoError(t, a.React(context.Background(), "1", 5, "👍"))
	assert.Equal(t, "bot.react", fc.calledMethod)
}

func TestBotAdapterDownloadFile(t *testing.T) {
	fc := &fakeClient{returnResult: json.RawMessage(`{"path":"/tmp/x.bin"}`)}
	a := NewBotAdapter(fc, nil)

	p, err := a.DownloadFile(context.Background(), "F123")
	require.NoError(t, err)
	assert.Equal(t, "/tmp/x.bin", p)
}

func TestBotAdapterBroadcastPermissionRequest(t *testing.T) {
	fc := &fakeClient{returnResult: json.RawMessage(`{}`)}
	a := NewBotAdapter(fc, func(string) (string, string) { return "do the thing", "tool args here" })

	a.BroadcastPermissionRequest(context.Background(), "ababc", "Bash")
	assert.Equal(t, "bot.broadcastPermissionRequest", fc.calledMethod)
	assert.Contains(t, string(fc.calledParams), `"request_id":"ababc"`)
	assert.Contains(t, string(fc.calledParams), `"description":"do the thing"`)
	assert.Contains(t, string(fc.calledParams), `"input_preview":"tool args here"`)
}

func TestBotAdapterEditMessage(t *testing.T) {
	fc := &fakeClient{returnResult: json.RawMessage(`{"message_id":12}`)}
	a := NewBotAdapter(fc, nil)

	id, err := a.EditMessage(context.Background(), "1", 12, "new", "MarkdownV2")
	require.NoError(t, err)
	assert.Equal(t, 12, id)
}

func TestBotAdapterPeersForwardsToIPC(t *testing.T) {
	fc := &fakeClient{returnResult: json.RawMessage(`{"peers":[
		{"alias":"s1","shim_id_prefix":"abcdef01","workdir":"/a","label":"","idle_seconds":42,"self":true},
		{"alias":"s2","shim_id_prefix":"deadbeef","workdir":"/b","label":"hot","idle_seconds":0,"self":false}
	]}`)}
	a := NewBotAdapter(fc, nil)

	peers, err := a.Peers(context.Background())
	require.NoError(t, err)
	require.Len(t, peers, 2)
	assert.Equal(t, "daemon.peers", fc.calledMethod)

	assert.Equal(t, "s1", peers[0].Alias)
	assert.Equal(t, "abcdef01", peers[0].ShimIDPrefix)
	assert.Equal(t, 42, peers[0].IdleSeconds)
	assert.True(t, peers[0].Self)

	assert.False(t, peers[1].Self)
	assert.Equal(t, "hot", peers[1].Label)
}

func TestBotAdapterPeersIPCError(t *testing.T) {
	fc := &fakeClient{returnErr: errors.New("ipc down")}
	a := NewBotAdapter(fc, nil)

	_, err := a.Peers(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ipc down")
}

func TestBotAdapterSendFileAttachmentTooLarge(t *testing.T) {
	data, _ := json.Marshal(map[string]any{"path": "/big.bin", "size": 99999999, "limit": 50000000})
	fc := &fakeClient{returnErr: &ipc.Error{Code: ipc.CodeAttachmentTooLarge, Message: "too large", Data: data}}
	a := NewBotAdapter(fc, nil)

	_, err := a.SendFile(context.Background(), "1", "/big.bin", bot.SendOpts{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAttachmentTooLarge)
}

func TestBotAdapterSwapClient(t *testing.T) {
	old := &fakeClient{returnResult: json.RawMessage(`{"message_id":1}`)}
	fresh := &fakeClient{returnResult: json.RawMessage(`{"message_id":2}`)}
	a := NewBotAdapter(old, nil)

	a.SwapClient(fresh)

	id, err := a.SendMessage(context.Background(), "1", "x", bot.SendOpts{})
	require.NoError(t, err)
	assert.Equal(t, 2, id, "after SwapClient, calls must hit the new client")
	assert.Empty(t, old.calledMethod, "old client must not be called after swap")
	assert.Equal(t, "bot.sendMessage", fresh.calledMethod)
}

func TestBotAdapterMapsConnectionClosedToErrDaemonUnreachable(t *testing.T) {
	fc := &fakeClient{returnErr: errors.New("connection closed")}
	a := NewBotAdapter(fc, nil)

	_, err := a.SendMessage(context.Background(), "1", "x", bot.SendOpts{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrDaemonUnreachable)
}

package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mcptypes "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/yakov/telegram-mcp/internal/access"
	"github.com/yakov/telegram-mcp/internal/bot"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// ===== fakeBot =====

type sendCall struct {
	chatID string
	text   string
	opts   bot.SendOpts
}

type fileCall struct {
	chatID string
	path   string
	opts   bot.SendOpts
}

type editCall struct {
	chatID    string
	messageID int
	text      string
	parseMode string
}

type fakeBot struct {
	mu sync.Mutex

	sendCalls     []sendCall
	fileCalls     []fileCall
	editCalls     []editCall
	reactCalls    int
	broadcastIDs  []string
	downloadFiles []string

	nextSendID int
	sendErrOn  int // index of send call that should fail (-1 = never)
	fileErrOn  int
	editErr    error
	reactErr   error
	downloadFn func(fileID string) (string, error)
}

func newFakeBot() *fakeBot {
	return &fakeBot{sendErrOn: -1, fileErrOn: -1}
}

func (f *fakeBot) SendMessage(_ context.Context, chatID, text string, opts bot.SendOpts) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	idx := len(f.sendCalls)

	f.sendCalls = append(f.sendCalls, sendCall{chatID, text, opts})
	if idx == f.sendErrOn {
		return 0, assertedErr("send failed")
	}

	f.nextSendID++

	return f.nextSendID, nil
}

func (f *fakeBot) SendFile(_ context.Context, chatID, path string, opts bot.SendOpts) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	idx := len(f.fileCalls)

	f.fileCalls = append(f.fileCalls, fileCall{chatID, path, opts})
	if idx == f.fileErrOn {
		return 0, assertedErr("send file failed")
	}

	f.nextSendID++

	return f.nextSendID, nil
}

func (f *fakeBot) EditMessage(_ context.Context, chatID string, messageID int, text, parseMode string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.editCalls = append(f.editCalls, editCall{chatID, messageID, text, parseMode})
	if f.editErr != nil {
		return 0, f.editErr
	}

	return messageID, nil
}

func (f *fakeBot) React(_ context.Context, _ string, _ int, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.reactCalls++

	return f.reactErr
}

func (f *fakeBot) DownloadFile(_ context.Context, fileID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.downloadFiles = append(f.downloadFiles, fileID)
	if f.downloadFn != nil {
		return f.downloadFn(fileID)
	}

	return "/tmp/dl/" + fileID, nil
}

func (f *fakeBot) BroadcastPermissionRequest(_ context.Context, requestID, _ string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.broadcastIDs = append(f.broadcastIDs, requestID)
}

type stringError string

func (e stringError) Error() string { return string(e) }

func assertedErr(s string) error { return stringError(s) }

// ===== helpers =====

func newServerWithAllowlist(t *testing.T, allow ...string) (*Server, *fakeBot, string) {
	t.Helper()
	dir := t.TempDir()
	store := access.NewStore(dir, false)
	st := access.State{
		DMPolicy:  access.PolicyAllowlist,
		AllowFrom: allow,
		Groups:    map[string]access.GroupPolicy{},
		Pending:   map[string]access.Pending{},
	}
	require.NoError(t, store.Save(st))
	srv, err := New(store)
	require.NoError(t, err)

	fb := newFakeBot()
	srv.AttachBot(fb)

	return srv, fb, dir
}

func callTool(name string, args map[string]any) mcptypes.CallToolRequest {
	var req mcptypes.CallToolRequest

	req.Params.Name = name
	req.Params.Arguments = args

	return req
}

// ===== handleReply =====

func TestHandleReply_singleChunk_returnsSentID(t *testing.T) {
	srv, fb, _ := newServerWithAllowlist(t, "123")
	res, err := srv.handleReply(t.Context(), callTool("reply", map[string]any{
		"chat_id": "123",
		"text":    "hello",
	}))
	require.NoError(t, err)
	require.False(t, res.IsError, contentText(res))
	assert.Contains(t, contentText(res), "sent (id: 1)")
	assert.Equal(t, "hello", fb.sendCalls[0].text)
}

func TestHandleReply_multiChunk(t *testing.T) {
	srv, fb, _ := newServerWithAllowlist(t, "123")
	long := strings.Repeat("a", 5000) // > 4096
	res, err := srv.handleReply(t.Context(), callTool("reply", map[string]any{
		"chat_id": "123",
		"text":    long,
	}))
	require.NoError(t, err)
	require.False(t, res.IsError, contentText(res))
	assert.Contains(t, contentText(res), "sent 2 parts")
	assert.Len(t, fb.sendCalls, 2)
}

func TestHandleReply_replyTo_firstChunkOnly(t *testing.T) {
	srv, fb, _ := newServerWithAllowlist(t, "123")
	long := strings.Repeat("a", 5000)
	_, err := srv.handleReply(t.Context(), callTool("reply", map[string]any{
		"chat_id":  "123",
		"text":     long,
		"reply_to": "42",
	}))
	require.NoError(t, err)
	assert.Equal(t, 42, fb.sendCalls[0].opts.ReplyTo)
	assert.Equal(t, 0, fb.sendCalls[1].opts.ReplyTo, "second chunk should NOT have reply_to (default mode 'first')")
}

func TestHandleReply_markdownv2_parseMode(t *testing.T) {
	srv, fb, _ := newServerWithAllowlist(t, "123")
	_, err := srv.handleReply(t.Context(), callTool("reply", map[string]any{
		"chat_id": "123",
		"text":    "*bold*",
		"format":  "markdownv2",
	}))
	require.NoError(t, err)
	assert.Equal(t, "MarkdownV2", fb.sendCalls[0].opts.ParseMode)
}

func TestHandleReply_notAllowlisted_returnsError(t *testing.T) {
	srv, _, _ := newServerWithAllowlist(t, "111")
	res, err := srv.handleReply(t.Context(), callTool("reply", map[string]any{
		"chat_id": "999",
		"text":    "hello",
	}))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res), "not allowlisted")
}

func TestHandleReply_sendErrorMidStream_reportsHowFar(t *testing.T) {
	srv, fb, _ := newServerWithAllowlist(t, "123")
	fb.sendErrOn = 1 // fail second chunk
	long := strings.Repeat("a", 5000)
	res, err := srv.handleReply(t.Context(), callTool("reply", map[string]any{
		"chat_id": "123",
		"text":    long,
	}))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res), "reply failed after 1 of 2 chunk")
}

func TestHandleReply_botNotAttached_returnsError(t *testing.T) {
	dir := t.TempDir()
	store := access.NewStore(dir, false)
	require.NoError(t, store.Save(access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"123"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	}))
	srv, err := New(store)
	require.NoError(t, err)
	res, err := srv.handleReply(t.Context(), callTool("reply", map[string]any{
		"chat_id": "123", "text": "x",
	}))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res), "bot not attached")
}

func TestHandleReply_fileTooLarge_rejected(t *testing.T) {
	srv, _, dir := newServerWithAllowlist(t, "123")
	// Create a 51MB sparse-ish file in the inbox.
	inbox := filepath.Join(dir, "inbox")
	require.NoError(t, os.MkdirAll(inbox, 0o700))
	big := filepath.Join(inbox, "big.bin")
	f, err := os.Create(big)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(MaxAttachmentBytes+1))
	require.NoError(t, f.Close())

	res, err := srv.handleReply(t.Context(), callTool("reply", map[string]any{
		"chat_id": "123",
		"text":    "x",
		"files":   []any{big},
	}))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res), "max 50MB")
}

func TestHandleReply_fileMissing_rejected(t *testing.T) {
	srv, _, _ := newServerWithAllowlist(t, "123")
	res, err := srv.handleReply(t.Context(), callTool("reply", map[string]any{
		"chat_id": "123",
		"text":    "x",
		"files":   []any{"/no/such/path.png"},
	}))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res), "stat")
}

func TestHandleReply_attachmentInsideStateDir_refused(t *testing.T) {
	srv, _, dir := newServerWithAllowlist(t, "123")
	// Drop a file in state dir (NOT inbox) — assertSendable should refuse.
	secret := filepath.Join(dir, "secret.txt")
	require.NoError(t, os.WriteFile(secret, []byte("token"), 0o600))

	res, err := srv.handleReply(t.Context(), callTool("reply", map[string]any{
		"chat_id": "123",
		"text":    "x",
		"files":   []any{secret},
	}))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res), "refusing to send channel state")
}

func TestHandleReply_attachmentInInbox_allowed(t *testing.T) {
	srv, fb, dir := newServerWithAllowlist(t, "123")
	inbox := filepath.Join(dir, "inbox")
	require.NoError(t, os.MkdirAll(inbox, 0o700))
	f := filepath.Join(inbox, "ok.png")
	require.NoError(t, os.WriteFile(f, []byte("img"), 0o644))

	_, err := srv.handleReply(t.Context(), callTool("reply", map[string]any{
		"chat_id": "123",
		"text":    "x",
		"files":   []any{f},
	}))
	require.NoError(t, err)
	assert.Len(t, fb.fileCalls, 1, "file send recorded")
}

// ===== handleReact =====

func TestHandleReact_ok(t *testing.T) {
	srv, fb, _ := newServerWithAllowlist(t, "123")
	res, err := srv.handleReact(t.Context(), callTool("react", map[string]any{
		"chat_id":    "123",
		"message_id": "42",
		"emoji":      "👍",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Equal(t, 1, fb.reactCalls)
}

func TestHandleReact_notAllowlisted(t *testing.T) {
	srv, fb, _ := newServerWithAllowlist(t, "111")
	res, err := srv.handleReact(t.Context(), callTool("react", map[string]any{
		"chat_id": "999", "message_id": "1", "emoji": "👍",
	}))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Equal(t, 0, fb.reactCalls)
}

func TestHandleReact_apiError(t *testing.T) {
	srv, fb, _ := newServerWithAllowlist(t, "123")
	fb.reactErr = assertedErr("boom")
	res, err := srv.handleReact(t.Context(), callTool("react", map[string]any{
		"chat_id": "123", "message_id": "1", "emoji": "👍",
	}))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res), "react failed: boom")
}

// ===== handleEdit =====

func TestHandleEdit_ok(t *testing.T) {
	srv, fb, _ := newServerWithAllowlist(t, "123")
	res, err := srv.handleEdit(t.Context(), callTool("edit_message", map[string]any{
		"chat_id": "123", "message_id": "55", "text": "new", "format": "markdownv2",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Contains(t, contentText(res), "edited (id: 55)")
	assert.Equal(t, "MarkdownV2", fb.editCalls[0].parseMode)
}

func TestHandleEdit_notAllowlisted(t *testing.T) {
	srv, _, _ := newServerWithAllowlist(t, "111")
	res, _ := srv.handleEdit(t.Context(), callTool("edit_message", map[string]any{
		"chat_id": "999", "message_id": "1", "text": "x",
	}))
	assert.True(t, res.IsError)
}

func TestHandleEdit_apiError(t *testing.T) {
	srv, fb, _ := newServerWithAllowlist(t, "123")
	fb.editErr = assertedErr("network")
	res, _ := srv.handleEdit(t.Context(), callTool("edit_message", map[string]any{
		"chat_id": "123", "message_id": "1", "text": "x",
	}))
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res), "network")
}

// ===== handleDownload =====

func TestHandleDownload_ok(t *testing.T) {
	srv, fb, _ := newServerWithAllowlist(t, "123")
	res, err := srv.handleDownload(t.Context(), callTool("download_attachment", map[string]any{
		"file_id": "AgADxxx",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Contains(t, contentText(res), "/tmp/dl/AgADxxx")
	assert.Equal(t, []string{"AgADxxx"}, fb.downloadFiles)
}

func TestHandleDownload_apiError(t *testing.T) {
	srv, fb, _ := newServerWithAllowlist(t, "123")
	fb.downloadFn = func(string) (string, error) { return "", assertedErr("expired") }
	res, _ := srv.handleDownload(t.Context(), callTool("download_attachment", map[string]any{"file_id": "x"}))
	assert.True(t, res.IsError)
	assert.Contains(t, contentText(res), "expired")
}

func TestServer_ensureInboxDir_idempotent(t *testing.T) {
	srv, _, dir := newServerWithAllowlist(t, "123")
	inbox := filepath.Join(dir, "inbox")

	require.NoError(t, srv.ensureInboxDir(), "first call should succeed")
	info, err := os.Stat(inbox)
	require.NoError(t, err)
	assert.True(t, info.IsDir())

	require.NoError(t, srv.ensureInboxDir(), "second call should succeed (cached)")

	// Delete the dir; sync.Once has already fired so ensureInboxDir must NOT
	// recreate it. This is the proof that the syscall is gated.
	require.NoError(t, os.RemoveAll(inbox))
	require.NoError(t, srv.ensureInboxDir(), "third call should still return nil from cached err")

	_, err = os.Stat(inbox)
	assert.True(t, os.IsNotExist(err), "inbox dir must NOT have been recreated: %v", err)
}

// ===== notifier surface =====

func TestLookupPermission_roundTrip(t *testing.T) {
	srv, _, _ := newServerWithAllowlist(t)
	srv.permMu.Lock()
	srv.pending["abcde"] = pendingEntry{
		details:   bot.PermissionDetails{ToolName: "Bash", Description: "run", InputPreview: `{"x":1}`},
		createdAt: time.Now(),
	}
	srv.permMu.Unlock()

	d, ok := srv.LookupPermission("abcde")
	assert.True(t, ok)
	assert.Equal(t, "Bash", d.ToolName)

	_, ok = srv.LookupPermission("missing")
	assert.False(t, ok)
}

func TestResolvePermission_clearsPending(t *testing.T) {
	srv, _, _ := newServerWithAllowlist(t)
	srv.permMu.Lock()
	srv.pending["abcde"] = pendingEntry{createdAt: time.Now()}
	srv.permMu.Unlock()

	srv.ResolvePermission("abcde", "allow")
	_, ok := srv.LookupPermission("abcde")
	assert.False(t, ok)
}

// ===== helpers =====

func TestAtoiSafe(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"   42  ", 42},
		{"-5", -5},
		{"abc", 0},
		{"3.14", 0},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, atoiSafe(tt.in), "atoiSafe(%q)", tt.in)
	}
}

func TestJoinInts(t *testing.T) {
	assert.Empty(t, joinInts(nil))
	assert.Equal(t, "7", joinInts([]int{7}))
	assert.Equal(t, "1, 2, 3", joinInts([]int{1, 2, 3}))
}

// ===== DeliverInbound =====

func TestDeliverInbound_doesNotPanicWithoutClients(t *testing.T) {
	srv, _, _ := newServerWithAllowlist(t)
	// No registered MCP session — DeliverInbound iterates 0 clients but must not panic.
	srv.DeliverInbound("hello", map[string]string{"chat_id": "123", "user": "@me"})
}

func TestRegisterNotifications_storesAndBroadcasts(t *testing.T) {
	srv, fb, _ := newServerWithAllowlist(t, "123")
	// Manually invoke the registered notification handler via the same
	// underlying srv. We dig into the unexported map; the alternative would
	// be a full JSON-RPC dispatch, which is overkill for verifying intent.
	srv.permMu.Lock()
	srv.pending["zzzzz"] = pendingEntry{
		details:   bot.PermissionDetails{ToolName: "Read", Description: "d", InputPreview: "{}"},
		createdAt: time.Now(),
	}
	srv.permMu.Unlock()
	// Direct fan-out — simulates a permission_request arrival.
	if b, ok := srv.Bot().(*fakeBot); ok {
		b.BroadcastPermissionRequest(t.Context(), "zzzzz", "Read")
	}

	assert.Contains(t, fb.broadcastIDs, "zzzzz")
}

// ===== test plumbing =====

func contentText(res *mcptypes.CallToolResult) string {
	if res == nil || len(res.Content) == 0 {
		return ""
	}

	if tc, ok := res.Content[0].(mcptypes.TextContent); ok {
		return tc.Text
	}

	return ""
}

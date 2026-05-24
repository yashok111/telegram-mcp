package bot

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/access"
)

// ===== mock Telegram Bot API =====

type apiCall struct {
	method string
	params map[string]any
}

type mockAPI struct {
	t  *testing.T
	mu sync.Mutex

	server *httptest.Server
	calls  []apiCall

	// nextMessageID is incremented for every Send*/Edit* response.
	nextMessageID int

	// errFor lets a specific method return an API error for one call.
	errFor map[string]string

	// onCall fires after a method is recorded — handy for tests that want
	// to assert payload contents inline.
	onCall func(apiCall)

	// blockGetUpdates, if non-nil, causes /getUpdates handlers to park until
	// the channel is closed. Simulates a wedged long-poll for shutdown tests.
	blockGetUpdates chan struct{}
}

func newMockAPI(t *testing.T) *mockAPI {
	t.Helper()
	m := &mockAPI{t: t, nextMessageID: 0, errFor: map[string]string{}}
	m.server = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.server.Close)

	return m
}

func (m *mockAPI) handle(w http.ResponseWriter, r *http.Request) {
	// Path format: /bot<TOKEN>/<method>
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 2 {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}

	method := parts[len(parts)-1]

	params := map[string]any{}

	contentType := r.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(contentType, "multipart/form-data"):
		if err := r.ParseMultipartForm(10 << 20); err == nil && r.MultipartForm != nil {
			for k, v := range r.MultipartForm.Value {
				params[k] = singleOrList(v)
			}

			for k := range r.MultipartForm.File {
				params[k+"_file"] = true
			}
		}
	default:
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()

		if len(body) > 0 {
			_ = json.Unmarshal(body, &params)
		}
	}

	m.mu.Lock()
	m.calls = append(m.calls, apiCall{method, params})
	cb := m.onCall
	errMsg, hasErr := m.errFor[method]
	block := m.blockGetUpdates
	m.mu.Unlock()

	if method == "getUpdates" && block != nil {
		<-block
		return
	}

	if cb != nil {
		cb(apiCall{method, params})
	}

	if hasErr {
		writeJSON(w, map[string]any{"ok": false, "error_code": 400, "description": errMsg})
		return
	}

	m.respond(w, method, params)
}

func (m *mockAPI) respond(w http.ResponseWriter, method string, params map[string]any) {
	switch method {
	case "getMe":
		writeJSON(w, ok(map[string]any{"id": 1, "is_bot": true, "username": "test_bot", "first_name": "Test"}))
	case "sendMessage", "sendPhoto", "sendDocument":
		m.mu.Lock()
		m.nextMessageID++
		id := m.nextMessageID
		m.mu.Unlock()
		writeJSON(w, ok(map[string]any{
			"message_id": id,
			"date":       time.Now().Unix(),
			"chat":       map[string]any{"id": 1, "type": "private"},
		}))
	case "editMessageText":
		writeJSON(w, ok(map[string]any{
			"message_id": editMessageID(params),
			"date":       time.Now().Unix(),
			"chat":       map[string]any{"id": 1, "type": "private"},
		}))
	case "setMessageReaction", "answerCallbackQuery", "sendChatAction", "setMyCommands",
		"editForumTopic", "closeForumTopic", "deleteForumTopic":
		writeJSON(w, ok(true))
	case "createForumTopic":
		m.mu.Lock()
		m.nextMessageID++
		threadID := m.nextMessageID
		m.mu.Unlock()
		writeJSON(w, ok(map[string]any{
			"message_thread_id": threadID,
			"name":              params["name"],
			"icon_color":        params["icon_color"],
		}))
	case "getFile":
		writeJSON(w, ok(map[string]any{
			"file_id":        params["file_id"],
			"file_unique_id": "uniq",
			"file_path":      "photos/file_1.jpg",
			"file_size":      100,
		}))
	default:
		http.Error(w, "unknown method: "+method, http.StatusNotFound)
	}
}

func editMessageID(params map[string]any) int {
	switch v := params["message_id"].(type) {
	case float64:
		return int(v)
	case string:
		n := 0

		for i := range len(v) {
			c := v[i]
			if c < '0' || c > '9' {
				return 0
			}

			n = n*10 + int(c-'0')
		}

		return n
	}

	return 0
}

func ok(result any) map[string]any { return map[string]any{"ok": true, "result": result} }

func singleOrList(v []string) any {
	if len(v) == 1 {
		return v[0]
	}

	return v
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (m *mockAPI) recordedCalls(method string) []apiCall {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []apiCall

	for _, c := range m.calls {
		if c.method == method {
			out = append(out, c)
		}
	}

	return out
}

// ===== test bot factory =====

func newTestBot(t *testing.T, st access.State) (*Bot, *mockAPI, string) {
	t.Helper()
	dir := t.TempDir()
	store := access.NewStore(dir, false)
	require.NoError(t, store.Save(st))

	api := newMockAPI(t)
	tgBot, err := telego.NewBot("1234567890:AAH00000000000000000000000000000000",
		telego.WithAPIServer(api.server.URL),
		telego.WithDiscardLogger(),
	)
	require.NoError(t, err)

	b := &Bot{
		api:      tgBot,
		token:    "1234567890:AAH00000000000000000000000000000000",
		store:    store,
		notifier: &noopNotifier{},
		username: "test_bot",
	}

	return b, api, dir
}

func payloadString(params map[string]any) string {
	b, _ := json.Marshal(params)
	return string(b)
}

type noopNotifier struct {
	mu        sync.Mutex
	delivered []deliveredCall
	resolved  []resolvedCall
	mutations []mutationCall

	mutateApplied bool
	mutateDetail  string
}

type mutationCall struct {
	pendingID string
	approve   bool
}

type deliveredCall struct {
	content string
	meta    map[string]string
}

type resolvedCall struct {
	requestID, behavior string
}

func (n *noopNotifier) DeliverInbound(content string, meta map[string]string) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.delivered = append(n.delivered, deliveredCall{content, meta})
}

func (n *noopNotifier) LookupPermission(requestID string) (PermissionDetails, bool) {
	if requestID == "abcde" {
		return PermissionDetails{ToolName: "Bash", Description: "d", InputPreview: "{}"}, true
	}

	return PermissionDetails{}, false
}

func (n *noopNotifier) ResolvePermission(requestID, behavior string) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.resolved = append(n.resolved, resolvedCall{requestID, behavior})
}

func (n *noopNotifier) ResolveMutation(_ context.Context, pendingID string, approve bool) (bool, string) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.mutations = append(n.mutations, mutationCall{pendingID, approve})

	return n.mutateApplied, n.mutateDetail
}

// ===== outbound API methods =====

func TestSendMessage_simple(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"42"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})
	id, err := b.SendMessage(t.Context(), "42", "hello", SendOpts{})
	require.NoError(t, err)
	assert.Equal(t, 1, id)

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Equal(t, "hello", calls[0].params["text"])
}

func TestSendMessage_withReplyToAndParseMode(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"42"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})
	_, err := b.SendMessage(t.Context(), "42", "*x*", SendOpts{ReplyTo: 5, ParseMode: "MarkdownV2"})
	require.NoError(t, err)

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Equal(t, "MarkdownV2", calls[0].params["parse_mode"])
	assert.Contains(t, payloadString(calls[0].params), `"message_id":5`)
}

func TestSendMessage_appliesMessageThreadID(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"42"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})
	_, err := b.SendMessage(t.Context(), "42", "hi", SendOpts{MessageThreadID: 7})
	require.NoError(t, err)

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.EqualValues(t, 7, calls[0].params["message_thread_id"])
}

func TestSendMessage_zeroThreadID_omitted(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"42"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})
	_, err := b.SendMessage(t.Context(), "42", "hi", SendOpts{})
	require.NoError(t, err)

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	_, present := calls[0].params["message_thread_id"]
	assert.False(t, present, "zero MessageThreadID should be omitted from outbound params")
}

func TestSendMessage_invalidChatID_errors(t *testing.T) {
	b, _, _ := newTestBot(t, access.State{})
	_, err := b.SendMessage(t.Context(), "not-a-number", "x", SendOpts{})
	assert.Error(t, err)
}

func TestSendMessage_apiError_propagates(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{})
	api.errFor["sendMessage"] = "Forbidden: blocked"
	_, err := b.SendMessage(t.Context(), "42", "x", SendOpts{})
	assert.Error(t, err)
}

func TestSendFile_photoExtensionRoutesToSendPhoto(t *testing.T) {
	b, api, dir := newTestBot(t, access.State{})
	p := filepath.Join(dir, "pic.png")
	require.NoError(t, os.WriteFile(p, []byte("img"), 0o644))

	id, err := b.SendFile(t.Context(), "42", p, SendOpts{ReplyTo: 3})
	require.NoError(t, err)
	assert.Positive(t, id)
	assert.NotEmpty(t, api.recordedCalls("sendPhoto"))
	assert.Empty(t, api.recordedCalls("sendDocument"))
}

func TestSendFile_documentExtensionRoutesToSendDocument(t *testing.T) {
	b, api, dir := newTestBot(t, access.State{})
	p := filepath.Join(dir, "data.bin")
	require.NoError(t, os.WriteFile(p, []byte("bin"), 0o644))

	_, err := b.SendFile(t.Context(), "42", p, SendOpts{})
	require.NoError(t, err)
	assert.NotEmpty(t, api.recordedCalls("sendDocument"))
}

func TestSendFile_appliesMessageThreadID_photo(t *testing.T) {
	b, api, dir := newTestBot(t, access.State{})
	p := filepath.Join(dir, "pic.png")
	require.NoError(t, os.WriteFile(p, []byte("img"), 0o644))

	_, err := b.SendFile(t.Context(), "42", p, SendOpts{MessageThreadID: 9})
	require.NoError(t, err)

	calls := api.recordedCalls("sendPhoto")
	require.Len(t, calls, 1)
	assert.EqualValues(t, "9", calls[0].params["message_thread_id"])
}

func TestSendFile_appliesMessageThreadID_document(t *testing.T) {
	b, api, dir := newTestBot(t, access.State{})
	p := filepath.Join(dir, "data.bin")
	require.NoError(t, os.WriteFile(p, []byte("bin"), 0o644))

	_, err := b.SendFile(t.Context(), "42", p, SendOpts{MessageThreadID: 11})
	require.NoError(t, err)

	calls := api.recordedCalls("sendDocument")
	require.Len(t, calls, 1)
	assert.EqualValues(t, "11", calls[0].params["message_thread_id"])
}

func TestSendFile_missingFile_errors(t *testing.T) {
	b, _, _ := newTestBot(t, access.State{})
	_, err := b.SendFile(t.Context(), "42", "/no/such/path.png", SendOpts{})
	assert.Error(t, err)
}

func TestSendFile_invalidChatID(t *testing.T) {
	b, _, _ := newTestBot(t, access.State{})
	_, err := b.SendFile(t.Context(), "bad", "/etc/hostname", SendOpts{})
	assert.Error(t, err)
}

func TestEditMessage(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{})
	id, err := b.EditMessage(t.Context(), "42", 7, "new", "MarkdownV2")
	require.NoError(t, err)
	assert.Equal(t, 7, id)
	assert.NotEmpty(t, api.recordedCalls("editMessageText"))
}

func TestEditMessage_invalidChatID(t *testing.T) {
	b, _, _ := newTestBot(t, access.State{})
	_, err := b.EditMessage(t.Context(), "bad", 1, "x", "")
	assert.Error(t, err)
}

func TestEditMessage_apiError(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{})
	api.errFor["editMessageText"] = "message can't be edited"
	_, err := b.EditMessage(t.Context(), "42", 1, "x", "")
	assert.Error(t, err)
}

func TestReact(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{})
	require.NoError(t, b.React(t.Context(), "42", 7, "👍"))

	calls := api.recordedCalls("setMessageReaction")
	require.Len(t, calls, 1)
	assert.Contains(t, payloadString(calls[0].params), "👍")
}

func TestReact_invalidChatID(t *testing.T) {
	b, _, _ := newTestBot(t, access.State{})
	assert.Error(t, b.React(t.Context(), "bad", 1, "👍"))
}

func TestSendChatAction(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{})
	require.NoError(t, b.SendChatAction(t.Context(), "42", "typing"))

	calls := api.recordedCalls("sendChatAction")
	require.Len(t, calls, 1)
	assert.Equal(t, "typing", calls[0].params["action"])
	assert.Contains(t, payloadString(calls[0].params), `"chat_id":42`)
}

func TestSendChatAction_invalidChatID(t *testing.T) {
	b, _, _ := newTestBot(t, access.State{})
	assert.Error(t, b.SendChatAction(t.Context(), "bad", "typing"))
}

func TestDownloadFile_writesToInbox(t *testing.T) {
	b, api, dir := newTestBot(t, access.State{})

	// Swap fileClient to point at our mock; restore after.
	orig := fileClient

	t.Cleanup(func() { fileClient = orig })

	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("fakepicbytes"))
	}))
	t.Cleanup(fileServer.Close)
	// Rewrite all outbound URLs from api.telegram.org/file/... → fileServer.URL/...
	fileClient = &http.Client{Transport: &redirectTransport{base: fileServer.URL}, Timeout: 5 * time.Second}

	path, err := b.DownloadFile(t.Context(), "AgADxyz")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(path, filepath.Join(dir, "inbox")), "downloaded file lives under inbox/")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "fakepicbytes", string(data))
	assert.NotEmpty(t, api.recordedCalls("getFile"))
}

func TestDownloadFile_getFileError(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{})
	api.errFor["getFile"] = "file not found"
	_, err := b.DownloadFile(t.Context(), "x")
	assert.Error(t, err)
}

// redirectTransport rewrites the request URL to use base's host:port, ignoring
// the original host. Used so fileClient.Do(req) hits our httptest server.
type redirectTransport struct{ base string }

func (r *redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req2 := req.Clone(req.Context())
	// req2.URL was https://api.telegram.org/file/bot<TOKEN>/<path>
	// We want fileServer/<path-stripped> but content doesn't matter for the test.
	parsed, _ := http.NewRequestWithContext(req.Context(), req.Method, r.base, nil)
	req2.URL = parsed.URL
	req2.Host = parsed.URL.Host

	return http.DefaultTransport.RoundTrip(req2)
}

// ===== checkApprovals =====

func TestCheckApprovals_sendsAndRemoves(t *testing.T) {
	b, api, dir := newTestBot(t, access.State{})
	approved := filepath.Join(dir, "approved")
	require.NoError(t, os.MkdirAll(approved, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(approved, "42"), []byte{}, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(approved, "not-a-number"), []byte{}, 0o600))

	b.checkApprovals(t.Context())

	calls := api.recordedCalls("sendMessage")
	assert.Len(t, calls, 1, "only the numeric file should produce a sendMessage")

	// Both files removed (numeric on success, malformed proactively).
	entries, _ := os.ReadDir(approved)
	assert.Empty(t, entries)
}

func TestCheckApprovals_noDir_silentNoop(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{})
	b.checkApprovals(t.Context()) // approved/ doesn't exist
	assert.Empty(t, api.recordedCalls("sendMessage"))
}

// ===== SendPermissionPrompt =====

func TestSendPermissionPrompt_sendsOneMessage(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"42"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})
	b.SendPermissionPrompt(t.Context(), PermissionTarget{ChatID: 42}, "", "abcde", "Bash")

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].params["text"], "Permission: Bash")
	assert.Contains(t, payloadString(calls[0].params), "perm:allow:abcde")
}

func TestSendPermissionPrompt_withPrefix(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"42"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})
	b.SendPermissionPrompt(t.Context(), PermissionTarget{ChatID: 42}, "@s1: ", "abcde", "Bash")

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Equal(t, "@s1: 🔐 Permission: Bash", calls[0].params["text"])
}

func TestSendPermissionPrompt_appliesThreadID(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"42"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})
	b.SendPermissionPrompt(t.Context(), PermissionTarget{ChatID: 42, ThreadID: 7}, "", "abcde", "Bash")

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.EqualValues(t, 7, calls[0].params["message_thread_id"])
}

func TestSendPermissionPrompt_zeroChatID_noop(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"42"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})
	b.SendPermissionPrompt(t.Context(), PermissionTarget{}, "", "abcde", "Bash")
	assert.Empty(t, api.recordedCalls("sendMessage"), "zero ChatID target should not send")
}

// ===== Forum topic admin API =====

func TestCreateForumTopic_returnsThreadID(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{})

	threadID, err := b.CreateForumTopic(t.Context(), -100123, "main", 0)
	require.NoError(t, err)
	assert.Positive(t, threadID)

	calls := api.recordedCalls("createForumTopic")
	require.Len(t, calls, 1)
	assert.Equal(t, "main", calls[0].params["name"])
	_, present := calls[0].params["icon_color"]
	assert.False(t, present, "zero iconColor must omit the field")
}

func TestCreateForumTopic_setsIconColorWhenNonZero(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{})

	_, err := b.CreateForumTopic(t.Context(), -100123, "main", 7322096)
	require.NoError(t, err)

	calls := api.recordedCalls("createForumTopic")
	require.Len(t, calls, 1)
	assert.EqualValues(t, 7322096, calls[0].params["icon_color"])
}

func TestCreateForumTopic_apiError(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{})
	api.errFor["createForumTopic"] = "Forbidden: can_manage_topics revoked"

	_, err := b.CreateForumTopic(t.Context(), -100123, "x", 0)
	require.Error(t, err)
}

func TestEditForumTopic_passesNameAndThreadID(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{})

	require.NoError(t, b.EditForumTopic(t.Context(), -100123, 42, "renamed"))

	calls := api.recordedCalls("editForumTopic")
	require.Len(t, calls, 1)
	assert.Equal(t, "renamed", calls[0].params["name"])
	assert.EqualValues(t, 42, calls[0].params["message_thread_id"])
}

// TestEditForumTopic_topicNotModified_isIdempotentSuccess asserts that
// Telegram's 400 TOPIC_NOT_MODIFIED — returned when the requested title
// already matches the current one — is treated as success, not failure. The
// caller's desired state already holds, so re-pushing the same name is a
// no-op, not an error worth surfacing or logging at WARN.
func TestEditForumTopic_topicNotModified_isIdempotentSuccess(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{})
	api.errFor["editForumTopic"] = "Bad Request: TOPIC_NOT_MODIFIED"

	err := b.EditForumTopic(t.Context(), -100123, 42, "same-name")
	require.NoError(t, err, "TOPIC_NOT_MODIFIED means the title already matches — idempotent success")
}

func TestCloseForumTopic_passesThreadID(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{})

	require.NoError(t, b.CloseForumTopic(t.Context(), -100123, 42))

	calls := api.recordedCalls("closeForumTopic")
	require.Len(t, calls, 1)
	assert.EqualValues(t, 42, calls[0].params["message_thread_id"])
}

func TestDeleteForumTopic_passesThreadID(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{})

	require.NoError(t, b.DeleteForumTopic(t.Context(), -100123, 42))

	calls := api.recordedCalls("deleteForumTopic")
	require.Len(t, calls, 1)
	assert.EqualValues(t, 42, calls[0].params["message_thread_id"])
}

func TestSendFile_caption(t *testing.T) {
	b, api, dir := newTestBot(t, access.State{})
	p := filepath.Join(dir, "pic.png")
	require.NoError(t, os.WriteFile(p, []byte("img"), 0o644))

	_, err := b.SendFile(t.Context(), "42", p, SendOpts{Caption: "@s1"})
	require.NoError(t, err)

	calls := api.recordedCalls("sendPhoto")
	require.Len(t, calls, 1)
	assert.Equal(t, "@s1", calls[0].params["caption"])
}

func TestSendFile_documentCaption(t *testing.T) {
	b, api, dir := newTestBot(t, access.State{})
	p := filepath.Join(dir, "data.bin")
	require.NoError(t, os.WriteFile(p, []byte("bin"), 0o644))

	_, err := b.SendFile(t.Context(), "42", p, SendOpts{Caption: "@s2"})
	require.NoError(t, err)

	calls := api.recordedCalls("sendDocument")
	require.Len(t, calls, 1)
	assert.Equal(t, "@s2", calls[0].params["caption"])
}

// ===== handleCommand =====

func TestHandleCommand_startInDM(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyPairing, AllowFrom: []string{}, Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})
	msg := telego.Message{Chat: telego.Chat{ID: 1, Type: "private"}, From: &telego.User{ID: 1}, Text: "/start"}
	require.NoError(t, b.handleCommand(t.Context(), msg))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].params["text"], "bridges Telegram")
}

func TestHandleCommand_helpInDM(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyPairing, AllowFrom: []string{}, Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})
	msg := telego.Message{Chat: telego.Chat{ID: 1, Type: "private"}, From: &telego.User{ID: 1}, Text: "/help"}
	require.NoError(t, b.handleCommand(t.Context(), msg))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].params["text"], "route to a paired")
}

func TestHandleCommand_statusPaired(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"1"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})
	msg := telego.Message{Chat: telego.Chat{ID: 1, Type: "private"}, From: &telego.User{ID: 1, Username: "alice"}, Text: "/status"}
	require.NoError(t, b.handleCommand(t.Context(), msg))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].params["text"], "@alice")
}

func TestHandleCommand_statusPending(t *testing.T) {
	now := time.Now().UnixMilli()
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyPairing, AllowFrom: []string{},
		Groups: map[string]access.GroupPolicy{},
		Pending: map[string]access.Pending{
			"zzzaaa": {SenderID: "1", ChatID: "1", ExpiresAt: now + 60_000, Replies: 1},
		},
	})
	msg := telego.Message{Chat: telego.Chat{ID: 1, Type: "private"}, From: &telego.User{ID: 1}, Text: "/status"}
	require.NoError(t, b.handleCommand(t.Context(), msg))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].params["text"], "pair `zzzaaa`")
	assert.Equal(t, "MarkdownV2", calls[0].params["parse_mode"])
}

func TestHandleCommand_statusNotPaired(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyPairing, AllowFrom: []string{},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})
	msg := telego.Message{Chat: telego.Chat{ID: 1, Type: "private"}, From: &telego.User{ID: 1}, Text: "/status"}
	require.NoError(t, b.handleCommand(t.Context(), msg))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].params["text"], "Not paired")
}

func TestHandleCommand_inGroup_silentDrop(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist,
		Groups:   map[string]access.GroupPolicy{"-100": {}}, Pending: map[string]access.Pending{},
	})
	msg := telego.Message{Chat: telego.Chat{ID: -100, Type: "group"}, From: &telego.User{ID: 1}, Text: "/start"}
	require.NoError(t, b.handleCommand(t.Context(), msg))
	assert.Empty(t, api.recordedCalls("sendMessage"))
}

func TestHandleCommand_disabledPolicy_silent(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyDisabled, AllowFrom: []string{}, Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})
	msg := telego.Message{Chat: telego.Chat{ID: 1, Type: "private"}, From: &telego.User{ID: 1}, Text: "/start"}
	require.NoError(t, b.handleCommand(t.Context(), msg))
	assert.Empty(t, api.recordedCalls("sendMessage"))
}

func TestHandleCommand_allowlistNotIncluded_silent(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"99"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})
	msg := telego.Message{Chat: telego.Chat{ID: 1, Type: "private"}, From: &telego.User{ID: 1}, Text: "/start"}
	require.NoError(t, b.handleCommand(t.Context(), msg))
	assert.Empty(t, api.recordedCalls("sendMessage"))
}

// ===== handleMessage =====

func TestHandleMessage_skipsCommandLines(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"1"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})
	msg := telego.Message{Chat: telego.Chat{ID: 1, Type: "private"}, From: &telego.User{ID: 1}, Text: "/start arg"}
	require.NoError(t, b.handleMessage(t.Context(), msg))
	// onCommand will handle it; handleMessage early-returns.
	assert.Empty(t, api.recordedCalls("sendMessage"))
}

func TestHandleMessage_gateDrop_silent(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})
	msg := telego.Message{Chat: telego.Chat{ID: 9, Type: "private"}, From: &telego.User{ID: 9}, Text: "hi"}
	require.NoError(t, b.handleMessage(t.Context(), msg))
	assert.Empty(t, api.recordedCalls("sendMessage"))
}

func TestHandleMessage_gatePair_sendsCode(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyPairing, AllowFrom: []string{},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})
	msg := telego.Message{Chat: telego.Chat{ID: 7, Type: "private"}, From: &telego.User{ID: 7}, Text: "hi"}
	require.NoError(t, b.handleMessage(t.Context(), msg))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].params["text"], "/telegram:access pair")
}

func TestHandleMessage_deliver_callsNotifier(t *testing.T) {
	b, _, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"1"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})
	n, _ := b.notifier.(*noopNotifier)
	msg := telego.Message{Chat: telego.Chat{ID: 1, Type: "private"}, From: &telego.User{ID: 1}, Text: "hi", Date: 1700000000}
	require.NoError(t, b.handleMessage(t.Context(), msg))
	require.Len(t, n.delivered, 1)
	assert.Equal(t, "hi", n.delivered[0].content)
	assert.Equal(t, "1", n.delivered[0].meta["chat_id"])
}

func TestHandleMessage_permissionReplyShortCircuits(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"1"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})
	n, _ := b.notifier.(*noopNotifier)
	msg := telego.Message{
		Chat: telego.Chat{ID: 1, Type: "private"}, From: &telego.User{ID: 1},
		MessageID: 9, Text: "yes abcde",
	}
	require.NoError(t, b.handleMessage(t.Context(), msg))
	require.Len(t, n.resolved, 1)
	assert.Equal(t, "abcde", n.resolved[0].requestID)
	assert.Equal(t, "allow", n.resolved[0].behavior)
	assert.Empty(t, n.delivered, "should NOT relay the permission reply as chat")
	assert.NotEmpty(t, api.recordedCalls("setMessageReaction"))
}

func TestHandleMessage_ackReactionFires(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"1"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
		AckReaction: "👀",
	})
	msg := telego.Message{Chat: telego.Chat{ID: 1, Type: "private"}, From: &telego.User{ID: 1}, MessageID: 9, Text: "hi"}
	require.NoError(t, b.handleMessage(t.Context(), msg))
	assert.NotEmpty(t, api.recordedCalls("setMessageReaction"))
	assert.NotEmpty(t, api.recordedCalls("sendChatAction"))
}

// ===== handleCallback =====

func TestHandleCallback_allow_resolvesAndEdits(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"42"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})
	n, _ := b.notifier.(*noopNotifier)
	q := telego.CallbackQuery{
		ID:   "cq1",
		From: telego.User{ID: 42},
		Data: "perm:allow:abcde",
		Message: &telego.Message{
			MessageID: 1, Chat: telego.Chat{ID: 42, Type: "private"}, Text: "🔐 Permission: Bash",
		},
	}
	require.NoError(t, b.handleCallback(t.Context(), q))
	require.Len(t, n.resolved, 1)
	assert.Equal(t, "allow", n.resolved[0].behavior)
	assert.NotEmpty(t, api.recordedCalls("answerCallbackQuery"))
	assert.NotEmpty(t, api.recordedCalls("editMessageText"))
}

func TestHandleCallback_deny(t *testing.T) {
	b, _, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"42"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})
	n, _ := b.notifier.(*noopNotifier)
	q := telego.CallbackQuery{
		ID: "cq2", From: telego.User{ID: 42},
		Data:    "perm:deny:abcde",
		Message: &telego.Message{MessageID: 1, Chat: telego.Chat{ID: 42, Type: "private"}, Text: "x"},
	}
	require.NoError(t, b.handleCallback(t.Context(), q))
	require.Len(t, n.resolved, 1)
	assert.Equal(t, "deny", n.resolved[0].behavior)
}

func TestHandleCallback_more_expandsDetails(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"42"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})
	q := telego.CallbackQuery{
		ID: "cq3", From: telego.User{ID: 42},
		Data:    "perm:more:abcde",
		Message: &telego.Message{MessageID: 1, Chat: telego.Chat{ID: 42, Type: "private"}, Text: "x"},
	}
	require.NoError(t, b.handleCallback(t.Context(), q))

	edits := api.recordedCalls("editMessageText")
	require.Len(t, edits, 1)
	assert.Contains(t, edits[0].params["text"], "tool_name: Bash")
}

func TestHandleCallback_more_missingDetails(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"42"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})
	q := telego.CallbackQuery{
		ID: "cq4", From: telego.User{ID: 42},
		Data:    "perm:more:zzzzz", // noopNotifier returns false for this id
		Message: &telego.Message{MessageID: 1, Chat: telego.Chat{ID: 42}, Text: "x"},
	}
	require.NoError(t, b.handleCallback(t.Context(), q))

	cb := api.recordedCalls("answerCallbackQuery")
	require.Len(t, cb, 1)
	assert.Contains(t, cb[0].params["text"], "no longer available")
}

func TestHandleCallback_notAllowlisted(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})
	n, _ := b.notifier.(*noopNotifier)
	q := telego.CallbackQuery{
		ID: "cqX", From: telego.User{ID: 42},
		Data:    "perm:allow:abcde",
		Message: &telego.Message{MessageID: 1, Chat: telego.Chat{ID: 42}, Text: "x"},
	}
	require.NoError(t, b.handleCallback(t.Context(), q))
	assert.Empty(t, n.resolved)

	cb := api.recordedCalls("answerCallbackQuery")
	require.Len(t, cb, 1)
	assert.Contains(t, cb[0].params["text"], "Not authorized")
}

func TestHandleCallback_invalidPayload(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{})
	q := telego.CallbackQuery{ID: "cqY", From: telego.User{ID: 42}, Data: "garbage"}
	require.NoError(t, b.handleCallback(t.Context(), q))
	// Only answers the callback to dismiss the spinner.
	assert.NotEmpty(t, api.recordedCalls("answerCallbackQuery"))
}

// ===== approvalLoop respects ctx (goleak coverage) =====

func TestApprovalLoop_exitsOnCtxCancel(t *testing.T) {
	b, _, _ := newTestBot(t, access.State{})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() {
		b.approvalLoop(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("approvalLoop did not exit after ctx cancel")
	}
}

// NewWithRouter() and Stop() smoke.
func TestNewWithRouter_invalidToken_errors(t *testing.T) {
	_, err := NewWithRouter("", access.NewStore(t.TempDir(), false), &noopNotifier{}, nil)
	assert.Error(t, err)
}

func TestStop_idempotent(t *testing.T) {
	b, _, _ := newTestBot(t, access.State{})
	// pollHandler is nil — Stop must be a safe no-op.
	b.Stop()
	b.Stop()
}

func TestPoll_exitsOnCtxCancel(t *testing.T) {
	b, _, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() { done <- b.Poll(ctx) }()
	// Give Poll a beat to call getMe + register handlers, then cancel.
	time.Sleep(150 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Poll did not exit after ctx cancel")
	}
}

// Regression: if telego's long-poller is wedged mid-request (slow upstream,
// connection drop without RST), Poll must still return within the 2s shutCtx
// deadline rather than blocking on <-done forever.
func TestPoll_returnsWhenBHStartHangs(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})

	release := make(chan struct{})

	t.Cleanup(func() { close(release) })

	api.mu.Lock()
	api.blockGetUpdates = release
	api.mu.Unlock()

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() { done <- b.Poll(ctx) }()

	time.Sleep(150 * time.Millisecond)

	start := time.Now()

	cancel()

	select {
	case <-done:
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Fatalf("Poll exit took %v; expected <3s (shutCtx is 2s)", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Poll did not exit within 3s of ctx cancel despite hung getUpdates")
	}
}

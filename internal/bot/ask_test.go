package bot

import (
	"testing"

	"github.com/mymmrac/telego"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/access"
)

func TestAskCallbackRE(t *testing.T) {
	cases := []struct {
		in       string
		match    bool
		qid, idx string
	}{
		{"ask:abcde:0", true, "abcde", "0"},
		{"ask:zzzzz:9", true, "zzzzz", "9"},
		{"ask:abcde:12", true, "abcde", "12"},
		{"ask:abcde:99", true, "abcde", "99"},
		{"ask:abcde:100", false, "", ""}, // 3 digits rejected
		{"ask:abcde:", false, "", ""},    // no index
		{"ask:abcd:0", false, "", ""},    // qid too short
		{"ask:abcdef:0", false, "", ""},  // qid too long
		{"ask:ablde:0", false, "", ""},   // 'l' excluded from alphabet
		{"ask:ABCDE:0", false, "", ""},   // uppercase rejected
		{"perm:allow:abcde", false, "", ""},
	}

	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			m := askCallbackRE.FindStringSubmatch(c.in)
			if !c.match {
				assert.Nil(t, m)
				return
			}

			require.NotNil(t, m)
			assert.Equal(t, c.qid, m[1])
			assert.Equal(t, c.idx, m[2])
		})
	}
}

// callbackData asserting the longest possible ask payload stays under
// Telegram's 64-byte callback_data limit (R6).
func TestAskCallbackData_underLimit(t *testing.T) {
	longest := "ask:zzzzz:99"
	assert.Less(t, len(longest), 64)
}

func TestSendAskPrompt_rendersButtonPerOption(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"42"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})
	b.SendAskPrompt(t.Context(), PermissionTarget{ChatID: 42}, "@s1: ", "abcde",
		"Deploy now?", []string{"Yes", "No", "Later"})

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Equal(t, "@s1: ❓ Deploy now?", calls[0].params["text"])

	payload := payloadString(calls[0].params)
	assert.Contains(t, payload, "ask:abcde:0")
	assert.Contains(t, payload, "ask:abcde:1")
	assert.Contains(t, payload, "ask:abcde:2")
	assert.Contains(t, payload, "Yes")
	assert.Contains(t, payload, "Later")
}

func TestSendAskPrompt_appliesThreadID(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"42"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})
	b.SendAskPrompt(t.Context(), PermissionTarget{ChatID: 42, ThreadID: 7}, "", "abcde",
		"Q?", []string{"A", "B"})

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.EqualValues(t, 7, calls[0].params["message_thread_id"])
}

func TestSendAskPrompt_zeroChatID_noop(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"42"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})
	b.SendAskPrompt(t.Context(), PermissionTarget{ChatID: 0}, "", "abcde", "Q?", []string{"A", "B"})
	assert.Empty(t, api.recordedCalls("sendMessage"))
}

func TestHandleCallback_ask_resolvesAndFreezes(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"42"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})
	n, _ := b.notifier.(*noopNotifier)
	q := telego.CallbackQuery{
		ID:   "cqA",
		From: telego.User{ID: 42, Username: "yak"},
		Data: "ask:abcde:1",
		Message: &telego.Message{
			MessageID: 5, Chat: telego.Chat{ID: 42, Type: "private"}, Text: "❓ Deploy now?",
			ReplyMarkup: &telego.InlineKeyboardMarkup{InlineKeyboard: [][]telego.InlineKeyboardButton{
				{{Text: "Yes", CallbackData: "ask:abcde:0"}},
				{{Text: "No", CallbackData: "ask:abcde:1"}},
			}},
		},
	}
	require.NoError(t, b.handleCallback(t.Context(), q))

	require.Len(t, n.asks, 1)
	assert.Equal(t, "abcde", n.asks[0].qid)
	assert.Equal(t, 1, n.asks[0].idx)
	assert.Equal(t, "@yak", n.asks[0].user)

	assert.NotEmpty(t, api.recordedCalls("answerCallbackQuery"))

	edits := api.recordedCalls("editMessageText")
	require.Len(t, edits, 1)
	assert.Contains(t, edits[0].params["text"], "chose: No")
}

func TestHandleCallback_ask_expiredShowsNoFalseSuccess(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"42"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})
	n, _ := b.notifier.(*noopNotifier)
	n.askNotDelivered = true // simulate a tap after the tool already timed out

	q := telego.CallbackQuery{
		ID:   "cqE",
		From: telego.User{ID: 42, Username: "yak"},
		Data: "ask:abcde:1",
		Message: &telego.Message{
			MessageID: 5, Chat: telego.Chat{ID: 42, Type: "private"}, Text: "❓ Deploy now?",
			ReplyMarkup: &telego.InlineKeyboardMarkup{InlineKeyboard: [][]telego.InlineKeyboardButton{
				{{Text: "Yes", CallbackData: "ask:abcde:0"}},
				{{Text: "No", CallbackData: "ask:abcde:1"}},
			}},
		},
	}
	require.NoError(t, b.handleCallback(t.Context(), q))

	cb := api.recordedCalls("answerCallbackQuery")
	require.Len(t, cb, 1)
	assert.Contains(t, cb[0].params["text"], "no longer active")

	edits := api.recordedCalls("editMessageText")
	require.Len(t, edits, 1)
	assert.Contains(t, edits[0].params["text"], "expired")
	assert.NotContains(t, edits[0].params["text"], "chose", "must not show a false success")
}

func TestHandleCallback_ask_notAllowlisted(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})
	n, _ := b.notifier.(*noopNotifier)
	q := telego.CallbackQuery{
		ID: "cqB", From: telego.User{ID: 99},
		Data:    "ask:abcde:0",
		Message: &telego.Message{MessageID: 1, Chat: telego.Chat{ID: 42}, Text: "❓ Q?"},
	}
	require.NoError(t, b.handleCallback(t.Context(), q))

	assert.Empty(t, n.asks)

	cb := api.recordedCalls("answerCallbackQuery")
	require.Len(t, cb, 1)
	assert.Contains(t, cb[0].params["text"], "Not authorized")
}

func TestAskOptionLabel_outOfRange(t *testing.T) {
	msg := &telego.Message{ReplyMarkup: &telego.InlineKeyboardMarkup{
		InlineKeyboard: [][]telego.InlineKeyboardButton{{{Text: "A"}}},
	}}
	assert.Equal(t, "A", askOptionLabel(msg, 0))
	assert.Empty(t, askOptionLabel(msg, 5))
	assert.Empty(t, askOptionLabel(msg, -1))
}
